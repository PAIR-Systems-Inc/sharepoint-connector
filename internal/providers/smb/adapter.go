package smb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// Adapter adapts an SMB share to the provider-neutral source.Source.
type Adapter struct {
	c *Client
}

// Compile-time interface checks.
var (
	_ source.Source           = (*Adapter)(nil)
	_ source.WebhookValidator = (*Adapter)(nil)
)

// NewAdapter wraps an SMB client as a source.Source.
func NewAdapter(c *Client) *Adapter { return &Adapter{c: c} }

func (a *Adapter) Label() string { return "smb" }

// MemNamespace is this provider's PERMANENT memory-id namespace. It says
// "path" rather than "id" deliberately: SMB has no stable per-file identifier,
// so a file's identity is its path relative to the sync root. The practical
// consequence is that renaming or moving a file reads as a delete plus an add
// (the content is re-embedded under the new path) — unavoidable without a
// server-side id.
//
// The namespace is per-*share*, not merely per-provider, because a relative path
// is not unique across servers — see Client.NamespaceKey. Never change it once a
// share is live: host, share name and SMB_ROOT are all part of it, so pin
// SMB_NAMESPACE if any of them might.
func (a *Adapter) MemNamespace() string { return "smb.file.path:" + a.c.NamespaceKey() }

func (a *Adapter) ListFiles(ctx context.Context) ([]source.FileInfo, error) {
	var out []source.FileInfo
	err := a.c.Walk(ctx, func(f File) error {
		out = append(out, a.toFileInfo(f))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LatestCursor returns a watermark positioned at "now" for the share.
//
// It is the newest modification time actually present, not the local clock:
// the watermark is compared against timestamps the *server* reports, and the
// two clocks are not the same. Taking it from the file set makes the comparison
// self-consistent, so a server running behind the connector's clock cannot
// cause changes to be skipped.
func (a *Adapter) LatestCursor(ctx context.Context) (string, error) {
	var newest time.Time
	err := a.c.Walk(ctx, func(f File) error {
		if f.ModTime.After(newest) {
			newest = f.ModTime
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return formatCursor(newest), nil
}

// Delta returns the files modified since the cursor, plus the new watermark.
//
// SMB has no change feed, so this walks the tree and compares modification
// times. Two consequences the engine relies on:
//
//   - Deletions are invisible. A deleted file is simply absent, and absence is
//     indistinguishable from "not modified". Deletions are therefore reconciled
//     by the periodic full sync, which diffs the whole share against the space.
//   - A file written in the same timestamp tick as the watermark, after the walk
//     passed it, is missed. The same periodic full sync repairs that.
//
// The walk is the same cost as a full sync's listing, but the work that follows
// is not: only genuinely-changed files are fetched, embedded and stored.
func (a *Adapter) Delta(ctx context.Context, cursor string) ([]source.Change, string, error) {
	since, err := parseCursor(cursor)
	if err != nil {
		// An unreadable cursor is not fatal: fall back to a full re-scan and
		// re-establish the watermark, exactly as a cursor expiry would.
		return nil, "", source.ErrCursorExpired
	}

	var (
		changes []source.Change
		newest  = since
	)
	err = a.c.Walk(ctx, func(f File) error {
		if f.ModTime.After(newest) {
			newest = f.ModTime
		}
		if f.ModTime.After(since) {
			changes = append(changes, source.Change{
				ID:     f.Path,
				IsFile: true,
				File:   a.toFileInfo(f),
			})
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return changes, formatCursor(newest), nil
}

func (a *Adapter) GetFile(ctx context.Context, id string) (*source.FileInfo, error) {
	f, err := a.c.Stat(ctx, id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, source.ErrNotFound
		}
		return nil, err
	}
	if f == nil {
		return nil, nil // a directory, not a file
	}
	fi := a.toFileInfo(*f)
	return &fi, nil
}

func (a *Adapter) Open(ctx context.Context, f source.FileInfo) (io.ReadCloser, error) {
	rc, err := a.c.Open(ctx, f.ID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, source.ErrNotFound
		}
		return nil, err
	}
	return rc, nil
}

// EnsureSubscription always fails: SMB has no push mechanism the connector can
// subscribe to from a client. The protocol's CHANGE_NOTIFY needs an open
// directory handle per watched tree, gives up under load (STATUS_NOTIFY_ENUM_DIR
// tells the client to re-enumerate), and is unimplemented by the Go SMB
// libraries; the NTFS change journal is a local-volume API that SMB never
// exposes. This provider therefore runs in poll mode only.
func (a *Adapter) EnsureSubscription(context.Context, string, time.Duration) (source.Subscription, error) {
	return source.Subscription{}, errors.New("smb has no push notifications; run in poll mode (SYNC_POLL_MINUTES > 0)")
}

// ValidateWebhook rejects everything — this provider has no webhook.
func (a *Adapter) ValidateWebhook(*http.Request, []byte) (source.WebhookResult, string) {
	return source.WebhookReject, ""
}

func (a *Adapter) toFileInfo(f File) source.FileInfo {
	return source.FileInfo{
		ID:               f.Path,
		Name:             f.Name,
		MimeType:         f.MimeType,
		ModifiedDateTime: f.ModTime.UTC().Format(time.RFC3339Nano),
		Size:             f.Size,
		RelativePath:     f.Path,
		DownloadRef:      f.Path,
		Metadata: map[string]any{
			"source":            "smb",
			"smb_host":          a.c.Host(),
			"smb_share":         a.c.Share(),
			"path":              f.Path,
			"modified_datetime": f.ModTime.UTC().Format(time.RFC3339Nano),
		},
	}
}

// --- cursor -----------------------------------------------------------------

// The cursor is an RFC-3339 timestamp: the newest modification time seen on the
// previous pass. Nanosecond precision is kept because SMB servers report it and
// truncating would re-emit files on every cycle.

func formatCursor(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseCursor(cursor string) (time.Time, error) {
	if cursor == "" {
		return time.Time{}, nil // no watermark yet: everything is "changed"
	}
	t, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid smb cursor %q: %w", cursor, err)
	}
	return t.UTC(), nil
}
