// Package source defines the provider-neutral interface and types the shared
// sync engine uses. Each content source (a SharePoint site, a Google Shared
// Drive, …) implements Source; the engine (internal/core/syncer and
// internal/core/server) depends only on this package, never on a concrete
// provider.
package source

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// FileInfo is a provider-neutral description of a source file.
type FileInfo struct {
	ID               string
	Name             string
	MimeType         string // effective content type stored on the memory (Google-native docs report their export type)
	ModifiedDateTime string // ISO-8601, lexically comparable
	Size             int64
	RelativePath     string
	DownloadRef      string         // opaque to the engine; the provider's Open() uses it (e.g. a download URL)
	Metadata         map[string]any // provider-supplied; stored on the Goodmem memory
}

// Change is one item from a provider's incremental (delta/changes) feed.
type Change struct {
	ID      string
	Deleted bool
	IsFile  bool
	File    FileInfo // valid when IsFile && !Deleted
	// ReconcileHint marks a deletion that may have implicitly orphaned other
	// memories the delta feed won't report individually — e.g. a Google Drive
	// folder trash, which implicitly trashes every descendant but emits a change
	// only for the folder. The engine responds by running a full reconcile after
	// the delta, so the orphaned descendants are deleted promptly instead of
	// lingering until the next periodic full sync.
	ReconcileHint bool
}

// Subscription is a provider push-notification registration.
type Subscription struct {
	ID         string
	Expiration string    // provider-native expiration string, for display/logs
	ExpiresAt  time.Time // parsed expiration; zero if unknown. The renewal loop schedules the next renewal from this — providers (e.g. Google Drive) may grant a shorter lifetime than requested, so the requested TTL alone is not safe to renew against.
}

// WebhookResult classifies an incoming webhook request.
type WebhookResult int

const (
	WebhookReject    WebhookResult = iota // not ours / bad secret → 401
	WebhookHandshake                      // validation/sync ping → ack (echo the returned string if non-empty)
	WebhookChange                         // a genuine change → trigger a sync
)

// WebhookValidator classifies an incoming webhook request (body already read).
type WebhookValidator interface {
	ValidateWebhook(r *http.Request, body []byte) (WebhookResult, string)
}

// ErrCursorExpired means the incremental cursor is no longer valid; the caller
// should run a full sync and re-bootstrap the cursor.
var ErrCursorExpired = errors.New("incremental cursor expired; full sync required")

// ErrNotFound means a file no longer exists at the source (analogous to 404).
var ErrNotFound = errors.New("file not found at source")

// ErrSkip, returned by Open, means the file can never be ingested (e.g. a
// Google-native doc whose export exceeds Drive's 10 MB export limit). The engine
// records it as a permanent skip rather than a transient failure, so it is not
// retried or dead-lettered forever. Wrap it to add detail: fmt.Errorf("...: %w", source.ErrSkip).
var ErrSkip = errors.New("file cannot be ingested; skipping permanently")

// Source is one content source the shared engine syncs into Goodmem.
type Source interface {
	// Label identifies the provider (e.g. "sharepoint", "gdrive") for logs/metrics.
	Label() string
	// MemNamespace is the PERMANENT namespace this source uses to derive
	// deterministic memory ids (memid.FromFileID). It is the idempotency key and
	// must never change once a tenant is live, or every memory in the space is
	// re-keyed. Distinct per provider so two sources can't collide.
	MemNamespace() string
	// ListFiles returns every file currently in scope (for a full sync).
	ListFiles(ctx context.Context) ([]FileInfo, error)
	// LatestCursor returns a cursor positioned at "now" (bootstrap).
	LatestCursor(ctx context.Context) (string, error)
	// Delta returns the changes since cursor and the next cursor. It returns
	// ErrCursorExpired when the cursor is no longer valid.
	Delta(ctx context.Context, cursor string) (changes []Change, next string, err error)
	// GetFile returns a file's current state, ErrNotFound if it is gone, or
	// (nil, nil) if the id refers to a non-file (e.g. a folder).
	GetFile(ctx context.Context, id string) (*FileInfo, error)
	// Open returns the file's bytes (downloading or exporting as needed).
	Open(ctx context.Context, f FileInfo) (io.ReadCloser, error)
	// EnsureSubscription creates or renews the push subscription for this source.
	EnsureSubscription(ctx context.Context, notifyURL string, ttl time.Duration) (Subscription, error)
	// ValidateWebhook classifies an incoming webhook request.
	ValidateWebhook(r *http.Request, body []byte) (WebhookResult, string)
}

// ThrottleReporter is an optional Source capability: providers that back off on
// rate limits call the hook before each backoff so the listener can surface it.
type ThrottleReporter interface {
	SetThrottleHook(fn func(status, attempt int, retryAfter time.Duration))
}
