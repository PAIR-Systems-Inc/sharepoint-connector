package sharepoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// Adapter adapts the Microsoft Graph *Client to the provider-neutral
// source.Source used by the shared engine.
type Adapter struct {
	c           *Client
	folderPath  string // scope for a one-time full sync; "" = whole drive (the listener always syncs the whole drive)
	clientState string // webhook secret Graph echoes back in each notification

	mu      sync.Mutex
	siteID  string
	driveID string
}

// Compile-time interface checks.
var (
	_ source.Source           = (*Adapter)(nil)
	_ source.ThrottleReporter = (*Adapter)(nil)
	_ source.WebhookValidator = (*Adapter)(nil)
)

// NewAdapter wraps a Graph client as a source.Source. folderPath scopes the
// full-sync listing (used by sync-once); clientState is the webhook secret.
func NewAdapter(c *Client, folderPath, clientState string) *Adapter {
	return &Adapter{c: c, folderPath: folderPath, clientState: clientState}
}

func (a *Adapter) Label() string { return "sharepoint" }

// resolve looks up the site and its first document library once.
func (a *Adapter) resolve() (siteID, driveID string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.driveID != "" {
		return a.siteID, a.driveID, nil
	}
	sid, err := a.c.GetSiteID()
	if err != nil {
		return "", "", err
	}
	drives, err := a.c.GetDrives(sid)
	if err != nil {
		return "", "", err
	}
	if len(drives) == 0 {
		return "", "", errors.New("no drives found for site")
	}
	a.siteID, a.driveID = sid, drives[0].ID
	return a.siteID, a.driveID, nil
}

func (a *Adapter) ListFiles(ctx context.Context) ([]source.FileInfo, error) {
	siteID, driveID, err := a.resolve()
	if err != nil {
		return nil, err
	}
	files, err := a.c.ListFiles(driveID, a.folderPath, true, siteID)
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
	_, driveID, err := a.resolve()
	if err != nil {
		return "", err
	}
	_, link, err := a.c.DriveDelta(driveID, "", true)
	if err != nil {
		return "", err
	}
	return link, nil
}

func (a *Adapter) Delta(ctx context.Context, cursor string) ([]source.Change, string, error) {
	_, driveID, err := a.resolve()
	if err != nil {
		return nil, "", err
	}
	items, next, err := a.c.DriveDelta(driveID, cursor, false)
	if err != nil {
		return nil, "", err
	}
	if items == nil && next == "" {
		return nil, "", source.ErrCursorExpired // 410 Gone
	}
	changes := make([]source.Change, 0, len(items))
	for _, it := range items {
		ch := source.Change{ID: it.ID, Deleted: it.Deleted, IsFile: it.IsFile}
		if it.IsFile && !it.Deleted {
			f := it.File
			// Delta stubs often lack the download URL / full metadata — re-fetch.
			if f.DownloadURL == "" {
				if full, gerr := a.c.GetFileByID(driveID, it.ID); gerr == nil && full != nil {
					rel := f.RelativePath
					if rel == "" {
						rel = f.Name
					}
					f = *full
					f.RelativePath = rel
				}
			}
			ch.File = toSourceFile(f)
		}
		changes = append(changes, ch)
	}
	return changes, next, nil
}

func (a *Adapter) GetFile(ctx context.Context, id string) (*source.FileInfo, error) {
	_, driveID, err := a.resolve()
	if err != nil {
		return nil, err
	}
	f, err := a.c.GetFileByID(driveID, id)
	if err != nil {
		if isHTTPStatus(err, http.StatusNotFound) {
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
	ref := f.DownloadRef
	if ref == "" {
		// A stub without a download URL (e.g. a pending re-fetch): resolve it now.
		_, driveID, err := a.resolve()
		if err != nil {
			return nil, err
		}
		full, err := a.c.GetFileByID(driveID, f.ID)
		if err != nil {
			return nil, err
		}
		if full == nil || full.DownloadURL == "" {
			return nil, errors.New("no download URL for " + f.Name)
		}
		ref = full.DownloadURL
	}
	b, err := a.c.Download(ref)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (a *Adapter) EnsureSubscription(ctx context.Context, notifyURL string, ttl time.Duration) (source.Subscription, error) {
	minutes := int(ttl / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	sub, err := a.c.EnsureSubscription(notifyURL, a.clientState, minutes)
	if err != nil {
		return source.Subscription{}, err
	}
	return source.Subscription{ID: sub.ID, Expiration: sub.ExpirationDateTime}, nil
}

func (a *Adapter) SetThrottleHook(fn func(status, attempt int, retryAfter time.Duration)) {
	a.c.OnThrottle = fn
}

// graphNotification is the subset of a Graph change-notification payload we read.
type graphNotification struct {
	Value []struct {
		ClientState string `json:"clientState"`
	} `json:"value"`
}

// ValidateWebhook implements the Graph webhook contract: a ?validationToken=…
// query is echoed back (subscription-validation handshake); otherwise the body's
// clientState must match ours on every entry (spoof protection).
func (a *Adapter) ValidateWebhook(r *http.Request, body []byte) (source.WebhookResult, string) {
	if token := r.URL.Query().Get("validationToken"); token != "" {
		return source.WebhookHandshake, token
	}
	if r.Method != http.MethodPost {
		return source.WebhookReject, ""
	}
	var n graphNotification
	if err := json.Unmarshal(body, &n); err != nil {
		return source.WebhookReject, ""
	}
	for _, v := range n.Value {
		if v.ClientState != a.clientState {
			return source.WebhookReject, ""
		}
	}
	return source.WebhookChange, ""
}

// toSourceFile converts a Graph FileInfo to the neutral source.FileInfo, carrying
// the rich Graph metadata (matching what was stored before) as Metadata.
func toSourceFile(f FileInfo) source.FileInfo {
	return source.FileInfo{
		ID:               f.ID,
		Name:             f.Name,
		MimeType:         f.MimeType,
		ModifiedDateTime: f.ModifiedDateTime,
		Size:             f.Size,
		RelativePath:     f.RelativePath,
		DownloadRef:      f.DownloadURL,
		Metadata:         fileMetadata(f),
	}
}

// fileMetadata converts a FileInfo to the metadata map stored on the memory,
// dropping empty fields (mirroring the Python `{k: v for ... if v is not None}`).
func fileMetadata(f FileInfo) map[string]any {
	b, _ := json.Marshal(f)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		} else if v == nil {
			delete(m, k)
		}
	}
	return m
}

// isHTTPStatus reports whether err is a *HTTPError with the given status.
func isHTTPStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.StatusCode == status
}
