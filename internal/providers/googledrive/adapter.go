package googledrive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// Adapter adapts the Google Drive Client to the provider-neutral source.Source.
type Adapter struct {
	c            *Client
	channelToken string       // webhook secret, echoed as X-Goog-Channel-Token
	store        ChannelStore // optional: persists the channel pair across restarts

	mu      sync.Mutex
	chID    string // last push channel, so a renewal can stop it (Drive has no in-place renew)
	chResID string
}

// Compile-time interface checks.
var (
	_ source.Source           = (*Adapter)(nil)
	_ source.WebhookValidator = (*Adapter)(nil)
)

// NewAdapter wraps a Drive client as a source.Source. channelToken is the secret
// echoed back in each push notification for validation.
func NewAdapter(c *Client, channelToken string) *Adapter {
	return &Adapter{c: c, channelToken: channelToken}
}

// WithChannelStore attaches durable channel state so a restart stops the
// previously-created push channel instead of leaking it. Returns the adapter for
// chaining. Used by the listener (serve); one-shot commands don't need it.
func (a *Adapter) WithChannelStore(s ChannelStore) *Adapter {
	a.store = s
	return a
}

func (a *Adapter) Label() string { return "google-drive" }

// MemNamespace is Google Drive's own permanent memory-id namespace — distinct
// from SharePoint's so the two providers never mint colliding ids. Never change
// it once a Drive tenant is live.
func (a *Adapter) MemNamespace() string { return "google-drive.file.id" }

func (a *Adapter) ListFiles(ctx context.Context) ([]source.FileInfo, error) {
	files, err := a.c.ListFiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]source.FileInfo, 0, len(files))
	for _, f := range files {
		out = append(out, toSourceFile(f))
	}
	return out, nil
}

func (a *Adapter) LatestCursor(ctx context.Context) (string, error) {
	return a.c.StartPageToken(ctx)
}

func (a *Adapter) Delta(ctx context.Context, cursor string) ([]source.Change, string, error) {
	changes, next, err := a.c.Changes(ctx, cursor)
	if err != nil {
		if IsCursorExpired(err) {
			return nil, "", source.ErrCursorExpired
		}
		return nil, "", err
	}
	out := make([]source.Change, 0, len(changes))
	for _, ch := range changes {
		sc := source.Change{ID: ch.FileID}
		switch {
		case ch.Removed || ch.File == nil || ch.File.Trashed:
			sc.Deleted = true
			// A trashed folder implicitly trashes its descendants, but Drive emits
			// no per-descendant change. Flag a reconcile so the engine's full sync
			// removes the orphaned descendant memories promptly. (A hard "removed"
			// with no file resource can't be identified as a folder — the periodic
			// full sync stays the backstop there.)
			if ch.File != nil && ch.File.MimeType == folderMime {
				sc.ReconcileHint = true
			}
		case ch.File.MimeType != folderMime:
			sc.IsFile = true
			sc.File = toSourceFile(*ch.File)
		}
		out = append(out, sc)
	}
	return out, next, nil
}

func (a *Adapter) GetFile(ctx context.Context, id string) (*source.FileInfo, error) {
	f, err := a.c.GetFile(ctx, id)
	if err != nil {
		if IsNotFound(err) {
			return nil, source.ErrNotFound
		}
		return nil, err
	}
	if f == nil { // a folder, not a file
		return nil, nil
	}
	sf := toSourceFile(*f)
	return &sf, nil
}

func (a *Adapter) Open(ctx context.Context, f source.FileInfo) (io.ReadCloser, error) {
	// DownloadRef carries the file's original Google mimeType so the client can
	// choose export (native docs) vs. alt=media (binary).
	rc, err := a.c.Open(ctx, f.ID, f.DownloadRef)
	if err != nil && IsExportTooLarge(err) {
		// A native doc whose export exceeds Drive's ~10 MB cap can never succeed
		// (and Size=0 means the byte-size guard never fired). Report a permanent
		// skip so the engine records it and moves on instead of dead-lettering it.
		return nil, fmt.Errorf("google export exceeds Drive's export size limit: %w", source.ErrSkip)
	}
	return rc, err
}

func (a *Adapter) EnsureSubscription(ctx context.Context, notifyURL string, ttl time.Duration) (source.Subscription, error) {
	// The channel to stop is the one from this process if we've created one, else
	// the one persisted by a previous process (so a restart doesn't leak it).
	a.mu.Lock()
	oldID, oldRes := a.chID, a.chResID
	a.mu.Unlock()
	if oldID == "" && a.store != nil {
		if id, res, err := a.store.Load(); err == nil {
			oldID, oldRes = id, res
		}
	}

	// Google Drive push channels can't be renewed in place — create a fresh
	// channel, then best-effort stop the previous one to avoid duplicate delivery.
	ch, err := a.c.Watch(ctx, randomChannelID(), notifyURL, a.channelToken, ttl)
	if err != nil {
		return source.Subscription{}, err
	}
	a.mu.Lock()
	a.chID, a.chResID = ch.ID, ch.ResourceID
	a.mu.Unlock()
	// Persist the new pair before stopping the old, so a crash in between still
	// leaves the current channel recorded for the next restart to manage.
	if a.store != nil {
		_ = a.store.Save(ch.ID, ch.ResourceID)
	}
	if oldID != "" && oldRes != "" {
		_ = a.c.StopChannel(ctx, oldID, oldRes)
	}
	return source.Subscription{ID: ch.ID, Expiration: ch.Expiration, ExpiresAt: ch.ExpiresAt}, nil
}

// ValidateWebhook implements the Drive push contract: the X-Goog-Channel-Token
// header must match our secret, an X-Goog-Resource-State of "sync" is the
// channel-established handshake, and anything else is a change ping.
func (a *Adapter) ValidateWebhook(r *http.Request, body []byte) (source.WebhookResult, string) {
	if r.Header.Get("X-Goog-Channel-Token") != a.channelToken {
		return source.WebhookReject, ""
	}
	switch r.Header.Get("X-Goog-Resource-State") {
	case "sync":
		return source.WebhookHandshake, "" // channel established; no changes yet
	case "":
		return source.WebhookReject, ""
	default: // "change" / "update" / "add" / "remove" / …
		return source.WebhookChange, ""
	}
}

// toSourceFile converts a Drive file to the neutral source.FileInfo. Google-native
// docs report their export target as the effective MimeType (so IsMimeSupported
// passes and the memory's content type is the exported format), while DownloadRef
// carries the original Google mimeType for Open to branch on.
func toSourceFile(f DriveFile) source.FileInfo {
	effective := f.MimeType
	if IsNativeDoc(f.MimeType) {
		effective = ExportTarget(f.MimeType) // "" if unsupported → engine skips it
	}
	md := map[string]any{
		"name":              f.Name,
		"mime_type":         effective,
		"google_mime":       f.MimeType,
		"modified_datetime": f.ModifiedTime,
		"md5":               f.MD5,
		"source":            "google-drive",
	}
	for k, v := range md {
		if s, ok := v.(string); ok && s == "" {
			delete(md, k)
		}
	}
	return source.FileInfo{
		ID:               f.ID,
		Name:             f.Name,
		MimeType:         effective,
		ModifiedDateTime: f.ModifiedTime,
		Size:             f.Size,
		RelativePath:     f.Name,
		DownloadRef:      f.MimeType,
		Metadata:         md,
	}
}

func randomChannelID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
