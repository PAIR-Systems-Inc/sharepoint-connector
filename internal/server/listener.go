package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/graph"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/store"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/syncer"
)

// Listener runs the event-triggered sync: it stands up the webhook server, does
// a startup full sync, bootstraps and persists the Graph delta link, ensures the
// change subscription (with a renewal loop), and on each notification runs a
// delta sync. Syncs are serialized. Ported from listener.py's orchestration.
type Listener struct {
	GC                *graph.Client
	GM                *goodmem.Client
	SpaceID           string
	ClientState       string
	NotificationURL   string
	SubMinutes        int
	FullSyncMinutes   int // periodic safety full-sync interval; <= 0 disables it
	Port              string
	DeltaPath         string  // file holding the Graph delta link
	ExtractPageImages bool    // hint Goodmem to extract page images
	MaxItemAttempts   int     // transient failures before an item is dead-lettered
	MaxDeleteRatio    float64 // refuse a full sync deleting > this fraction of memories
	MaxFileBytes      int64   // skip files larger than this before downloading (0 = no cap)
	RetentionDays     int     // prune sync history older than this many days (<= 0 disables)
	IgnoredFolderPath string  // set when SHAREPOINT_FOLDER_PATH is configured but ignored (listener syncs whole drive)

	driveID   string
	delta     deltaStore
	retry     *syncer.Retrier
	history   *store.Store
	eventSink syncer.EventSink
	server    *Server
	baseCtx   context.Context
	syncMu    sync.Mutex    // serialize full/delta syncs
	notify    chan struct{} // 1-buffered: coalesces notification bursts into one delta run
	ready     atomic.Bool   // true once the startup full sync + subscription are done (GET /readyz)
}

// opts builds the sync Options for this listener (durable retry, page images,
// safety limits, and the sync-history sink).
func (l *Listener) opts() syncer.Options {
	return syncer.Options{
		ExtractPageImages: l.ExtractPageImages,
		MaxFileBytes:      l.MaxFileBytes,
		MaxDeleteRatio:    l.MaxDeleteRatio,
		Retry:             l.retry,
		Sink:              l.eventSink,
	}
}

// Run binds the HTTP server and blocks until ctx is cancelled.
func (l *Listener) Run(ctx context.Context) error {
	l.baseCtx = ctx
	l.delta = deltaStore{path: l.DeltaPath}
	// Durable state (delta cursor + pending-retry sets) lives in this directory —
	// on a mounted volume in production, so it survives restarts. Ensure it exists.
	stateDir := filepath.Dir(l.DeltaPath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	l.retry = syncer.NewRetrier(stateDir, l.MaxItemAttempts)
	l.notify = make(chan struct{}, 1)
	l.server = New(l.ClientState, func(int) { l.signal() })
	l.server.Metrics.SetPendingFn(l.retry.Counts)
	l.server.SetReadyFn(l.ready.Load)
	l.server.Log("info", "durable state dir: "+stateDir+" (delta cursor + pending-retry sets)")
	if l.IgnoredFolderPath != "" {
		l.server.Log("warn", "SHAREPOINT_FOLDER_PATH="+l.IgnoredFolderPath+" is ignored by the listener; it syncs the whole drive (folder scoping applies only to sync-once)")
	}

	// Durable, queryable sync history (SQLite on the same volume) → GET /syncs.
	// Non-fatal: on failure the listener runs without history.
	if st, err := store.Open(filepath.Join(stateDir, "sync_history.db")); err != nil {
		l.server.Log("error", "sync-history disabled: "+err.Error())
	} else {
		l.history = st
		l.server.History = st
		l.eventSink = func(e syncer.SyncEvent) {
			if err := st.Record(e); err != nil {
				l.server.Log("error", "sync-history record: "+err.Error())
			}
		}
		defer func() {
			if l.history != nil {
				_ = l.history.Close()
			}
		}()
	}
	// Route dead-letter events to the same history sink so parked items show up in
	// GET /syncs (set after eventSink is resolved; nil is fine — events discarded).
	l.retry.Sink = l.eventSink

	// Surface Graph throttling/backoff in the activity log + metrics.
	l.GC.OnThrottle = func(status, attempt int, retryAfter time.Duration) {
		l.server.Metrics.RecordThrottle()
		msg := fmt.Sprintf("[throttle] Graph status=%d; backing off before retry %d", status, attempt)
		if retryAfter > 0 {
			msg += fmt.Sprintf(" (Retry-After %s)", retryAfter)
		}
		l.server.Log("warn", msg)
	}

	siteID, err := l.GC.GetSiteID()
	if err != nil {
		return fmt.Errorf("resolve site: %w", err)
	}
	drives, err := l.GC.GetDrives(siteID)
	if err != nil {
		return fmt.Errorf("list drives: %w", err)
	}
	if len(drives) == 0 {
		return fmt.Errorf("no drives found for site")
	}
	l.driveID = drives[0].ID

	// Bind the port first so Graph's subscription-validation POST can reach us.
	ln, err := net.Listen("tcp", ":"+l.Port)
	if err != nil {
		return err
	}
	go l.startup()                 // full sync + delta bootstrap + create subscription
	go l.deltaWorker(ctx)          // single worker draining coalesced notifications
	go l.subscriptionLoop(ctx)     // periodic renewal (short-backoff retry on failure)
	go l.periodicFullSyncLoop(ctx) // periodic safety full-sync (repairs missed deltas)
	go l.retentionLoop(ctx)        // prune old sync-history rows

	srv := &http.Server{Handler: l.server.Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	l.server.Log("info", "listener started on :"+l.Port)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// startup runs the boot-time full sync, persists a fresh delta link, then
// creates/renews the subscription (after the server is listening).
func (l *Listener) startup() {
	l.fullSyncLocked("startup")
	sub, err := l.GC.EnsureSubscription(l.NotificationURL, l.ClientState, l.SubMinutes)
	l.server.Metrics.RecordRenewal(err == nil)
	if err != nil {
		l.server.Log("error", "subscription: "+err.Error())
	} else {
		l.server.Log("info", "subscription ready (expires "+sub.ExpirationDateTime+")")
		l.ready.Store(true) // startup full sync done + subscription ensured → serve /readyz 200
	}
}

// fullSyncLocked acquires the sync lock and runs a full sync + delta
// re-bootstrap. tag labels the activity log ("startup" / "periodic").
func (l *Listener) fullSyncLocked(tag string) {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	l.runFull(tag)
}

// runFull performs a full sync and advances the delta cursor. Caller must hold
// syncMu. Two correctness rules:
//
//   - The cursor is captured BEFORE listing, so a change that lands mid-sync
//     (after the listing, before the token) is caught by the next delta rather
//     than falling between the full sync and all future deltas.
//   - The cursor is advanced only when the full sync SUCCEEDED. On failure the
//     old link is kept so its window is retried; resetting to "now" on a failed
//     sync would silently skip every change in between until the next reconcile.
func (l *Listener) runFull(tag string) error {
	l.server.Log("info", "["+tag+"] full sync starting")
	_, preLink, derr := l.GC.DriveDelta(l.driveID, "", true)
	res, err := syncer.RunFull(l.baseCtx, l.GC, l.GM, l.SpaceID, l.opts())
	if err != nil {
		l.server.Log("error", "["+tag+"] full sync: "+err.Error())
	} else {
		l.server.Log("info", fmt.Sprintf("[%s] full sync done: +%d ~%d -%d (skipped %d)", tag, res.Added, res.Updated, res.Deleted, res.Skipped))
	}
	l.server.Metrics.RecordSync("full", res)
	if err == nil && derr == nil && preLink != "" {
		if serr := l.delta.save(preLink); serr == nil {
			l.server.Log("info", "["+tag+"] delta link saved")
		}
	}
	return err
}

// periodicFullSyncLoop runs a safety full sync every FullSyncMinutes to reconcile
// anything the event-triggered deltas missed (dropped/undelivered notifications,
// or a FAILED-status memory whose timestamp still matches). Python ran this on
// each subscription renewal; the Go port previously had no periodic reconcile —
// a Graph delta 410 is opportunistic, not a schedule, so it is not a substitute.
// FullSyncMinutes <= 0 disables the loop.
func (l *Listener) periodicFullSyncLoop(ctx context.Context) {
	if l.FullSyncMinutes <= 0 {
		return
	}
	t := time.NewTicker(time.Duration(l.FullSyncMinutes) * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.fullSyncLocked("periodic")
		}
	}
}

// subscriptionLoop renews the subscription roughly every half-lifetime, but on a
// failed renewal retries on a short exponential backoff instead of waiting the
// full half-lifetime — otherwise a single failed renewal isn't retried until
// almost exactly expiry, and two in a row let the subscription lapse.
func (l *Listener) subscriptionLoop(ctx context.Context) {
	normal := time.Duration(max(l.SubMinutes/2, 20)) * time.Minute
	const retryMin, retryMax = 2 * time.Minute, 15 * time.Minute
	retry := retryMin
	t := time.NewTimer(normal)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sub, err := l.GC.EnsureSubscription(l.NotificationURL, l.ClientState, l.SubMinutes)
			l.server.Metrics.RecordRenewal(err == nil)
			if err != nil {
				l.server.Log("error", fmt.Sprintf("subscription renew failed: %v; retrying in %s", err, retry))
				t.Reset(retry)
				if retry *= 2; retry > retryMax {
					retry = retryMax
				}
			} else {
				l.server.Log("info", "subscription renewed (expires "+sub.ExpirationDateTime+")")
				l.ready.Store(true) // recovered if the startup subscription had failed
				retry = retryMin
				t.Reset(normal)
			}
		}
	}
}

// signal requests a delta sync. The 1-buffered channel coalesces a burst of
// notifications (e.g. a bulk upload) into at most one queued run: the in-flight
// sync plus one follow-up that picks up everything via the delta cursor, instead
// of thousands of serialized no-op delta calls.
func (l *Listener) signal() {
	select {
	case l.notify <- struct{}{}:
	default: // a run is already pending; this notification folds into it
	}
}

// deltaWorker is the single consumer draining coalesced notification signals.
func (l *Listener) deltaWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.notify:
			l.runDelta()
		}
	}
}

// runDelta runs one delta sync (serialized). On an expired delta token it falls
// back to a full sync, which re-bootstraps the delta link.
func (l *Listener) runDelta() {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	l.server.Log("info", "[delta] sync starting")
	newLink, res, err := syncer.RunDelta(l.baseCtx, l.GC, l.GM, l.SpaceID, l.driveID, l.delta.load(), l.opts())
	if err == syncer.ErrDeltaExpired {
		l.server.Log("info", "[delta] token expired; running full sync")
		l.runFull("delta-fallback")
		return
	}
	if err != nil {
		l.server.Log("error", "[delta] sync: "+err.Error())
		return
	}
	l.server.Metrics.RecordSync("delta", res)
	if newLink != "" {
		_ = l.delta.save(newLink)
	}
	l.server.Log("info", fmt.Sprintf("[delta] done: +%d ~%d -%d (skipped %d)", res.Added, res.Updated, res.Deleted, res.Skipped))
}

// retentionLoop prunes sync-history rows older than RetentionDays: once at
// startup, then daily. Disabled when RetentionDays <= 0 or history is off.
func (l *Listener) retentionLoop(ctx context.Context) {
	if l.history == nil || l.RetentionDays <= 0 {
		return
	}
	l.pruneHistory()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.pruneHistory()
		}
	}
}

func (l *Listener) pruneHistory() {
	cutoff := time.Now().Add(-time.Duration(l.RetentionDays) * 24 * time.Hour)
	if n, err := l.history.Prune(cutoff); err != nil {
		l.server.Log("error", "history prune: "+err.Error())
	} else if n > 0 {
		l.server.Log("info", fmt.Sprintf("history prune: removed %d record(s) older than %d days", n, l.RetentionDays))
	}
}

// deltaStore persists the Graph delta link to a file (single-machine state;
// durable/shared state is a follow-up).
type deltaStore struct{ path string }

func (d deltaStore) load() string {
	b, _ := os.ReadFile(d.path)
	return strings.TrimSpace(string(b))
}

func (d deltaStore) save(link string) error {
	return os.WriteFile(d.path, []byte(link), 0o600)
}
