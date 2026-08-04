package server

import (
	"context"
	"errors"
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

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/store"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/syncer"
)

// Listener runs the event-triggered sync: it stands up the webhook server, does
// a startup full sync, bootstraps and persists the incremental cursor, ensures
// the change subscription (with a renewal loop), and on each notification runs a
// delta sync. Syncs are serialized. Provider-agnostic — it drives a source.Source.
type Listener struct {
	Src               source.Source
	GM                *goodmem.Client
	SpaceID           string
	NotificationURL   string
	SubMinutes        int
	FullSyncMinutes   int // periodic safety full-sync interval; <= 0 disables it
	PollMinutes       int // >0 → poll the delta feed on this interval instead of using a push subscription (no webhook needed); 0 → push/webhook mode
	Port              string
	DeltaPath         string  // file holding the incremental cursor
	ExtractPageImages bool    // hint Goodmem to extract page images
	MaxItemAttempts   int     // transient failures before an item is dead-lettered
	MaxDeleteRatio    float64 // refuse a full sync deleting > this fraction of memories
	MaxFileBytes      int64   // skip files larger than this before downloading (0 = no cap)
	RetentionDays     int     // prune sync history older than this many days (<= 0 disables)
	IgnoredFolderPath string  // set when a folder scope is configured but ignored (listener syncs whole drive)

	delta      deltaStore
	retry      *syncer.Retrier
	history    *store.Store
	eventSink  syncer.EventSink
	server     *Server
	baseCtx    context.Context
	syncMu     sync.Mutex    // serialize full/delta syncs
	notify     chan struct{} // 1-buffered: coalesces notification bursts into one delta run
	ready      atomic.Bool   // true once the startup full sync + subscription are done (GET /readyz)
	subExpNano atomic.Int64  // provider-granted subscription expiration (UnixNano); 0 = unknown
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
	l.server = New(l.Src, func(int) { l.signal() })
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

	// Surface provider throttling/backoff in the activity log + metrics (providers
	// that back off implement source.ThrottleReporter).
	if tr, ok := l.Src.(source.ThrottleReporter); ok {
		tr.SetThrottleHook(func(status, attempt int, retryAfter time.Duration) {
			l.server.Metrics.RecordThrottle()
			msg := fmt.Sprintf("[throttle] source status=%d; backing off before retry %d", status, attempt)
			if retryAfter > 0 {
				msg += fmt.Sprintf(" (Retry-After %s)", retryAfter)
			}
			l.server.Log("warn", msg)
		})
	}

	// Bind the port first so the provider's subscription-validation POST can reach us.
	ln, err := net.Listen("tcp", ":"+l.Port)
	if err != nil {
		return err
	}
	go l.startup(ctx)              // full sync + delta bootstrap, then push subscription (or nothing in poll mode)
	go l.deltaWorker(ctx)          // single worker draining coalesced delta triggers (webhook or poll)
	go l.periodicFullSyncLoop(ctx) // periodic safety full-sync (repairs anything missed)
	go l.retentionLoop(ctx)        // prune old sync-history rows
	go l.watchLoop(ctx)            // event-driven syncs where the source supports them (no-op otherwise)
	if l.PollMinutes > 0 {
		go l.periodicDeltaLoop(ctx) // poll mode: drive delta syncs on a timer instead of push notifications
	}

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

// startup runs the boot-time full sync and persists a fresh delta link. In push
// mode it then creates the subscription and runs the renewal loop (blocking until
// ctx is cancelled); in poll mode there is no subscription — the periodicDeltaLoop
// drives sync — so it just marks the listener ready.
func (l *Listener) startup(ctx context.Context) {
	l.fullSyncLocked("startup")
	if l.PollMinutes > 0 {
		// Poll mode: no push subscription (e.g. Google Drive's changes.watch needs a
		// domain-verified webhook). Ready once the startup full sync has been attempted.
		l.ready.Store(true)
		l.server.Log("info", fmt.Sprintf("poll mode: delta sync every %d min (no push subscription)", l.PollMinutes))
		return
	}
	l.renewSubscription(ctx, "subscription ready")
	l.subscriptionLoop(ctx)
}

// periodicDeltaLoop drives a delta sync every PollMinutes (poll mode). It signals
// the same coalescing worker the webhook path uses, so a slow sync can't pile up
// overlapping runs. Used when there is no push subscription (e.g. Google Drive
// without a domain-verified webhook). PollMinutes <= 0 disables it (push mode).
func (l *Listener) periodicDeltaLoop(ctx context.Context) {
	if l.PollMinutes <= 0 {
		return
	}
	t := time.NewTicker(time.Duration(l.PollMinutes) * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.signal()
		}
	}
}

// renewSubscription (re)creates the change subscription, records the outcome, and
// stores the provider-granted expiration so the renewal loop can schedule against
// it. Reports whether it succeeded. On success it marks the listener ready — the
// startup full sync has already been attempted by the time this runs (a failed
// startup sync is left to the periodic reconcile, not gated here → /readyz 200).
func (l *Listener) renewSubscription(ctx context.Context, okMsg string) bool {
	sub, err := l.Src.EnsureSubscription(ctx, l.NotificationURL, time.Duration(l.SubMinutes)*time.Minute)
	l.server.Metrics.RecordRenewal(err == nil)
	if err != nil {
		l.server.Log("error", "subscription: "+err.Error())
		return false
	}
	l.setSubExp(sub.ExpiresAt)
	l.server.Log("info", okMsg+" (expires "+sub.Expiration+")")
	l.ready.Store(true)
	return true
}

func (l *Listener) setSubExp(t time.Time) {
	if t.IsZero() {
		l.subExpNano.Store(0)
		return
	}
	l.subExpNano.Store(t.UnixNano())
}

// subExpRemaining is the time until the granted subscription expiry, or 0 if the
// provider didn't report one (fall back to the configured cadence).
func (l *Listener) subExpRemaining() time.Duration {
	n := l.subExpNano.Load()
	if n == 0 {
		return 0
	}
	return time.Until(time.Unix(0, n))
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
	preLink, derr := l.Src.LatestCursor(l.baseCtx)
	res, err := syncer.RunFull(l.baseCtx, l.Src, l.GM, l.SpaceID, l.opts())
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

// watchReconnectDelay bounds how fast a lost watch is re-established, so a
// server that keeps refusing the handle cannot become a spin loop.
const watchReconnectDelay = 5 * time.Second

// watchLoop drives syncs from provider change notifications instead of waiting
// for the poll timer, when the source supports it (source.ChangeWatcher).
//
// It does not replace the periodic loops, and must not: every notification
// mechanism can drop records — a server-side buffer overflow, a dropped
// connection — so the poll and the periodic full sync remain the guarantee while
// this only lowers latency.
//
// A notification that may be a deletion asks for a full reconcile rather than a
// delta, because a timestamp-based delta cannot see a deleted file: it is simply
// absent, which is indistinguishable from unchanged.
func (l *Listener) watchLoop(ctx context.Context) {
	w, ok := l.Src.(source.ChangeWatcher)
	if !ok {
		return
	}
	l.server.Log("info", "change notifications enabled: syncing on source events (periodic loops remain as a safety net)")

	for {
		if ctx.Err() != nil {
			return
		}
		res, err := w.WatchChanges(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// The watch is gone, so changes during the gap were missed. Reconcile
			// once the watch is back rather than trusting the delta cursor.
			l.server.Log("warn", "[watch] lost: "+err.Error()+"; re-establishing")
			select {
			case <-ctx.Done():
				return
			case <-time.After(watchReconnectDelay):
			}
			l.requestReconcile("watch-reestablished")
			continue
		}
		if !res.Changed {
			continue
		}
		if res.ReconcileRecommended {
			// A deletion, or a notification stream that lost records.
			l.requestReconcile("watch-deletion")
			continue
		}
		l.signal()
	}
}

// requestReconcile runs a full sync, taking the sync lock so it cannot overlap a
// delta already in flight.
func (l *Listener) requestReconcile(reason string) {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	l.runFull(reason)
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

// subscriptionLoop renews the subscription before it expires. The cadence is the
// smaller of the configured half-lifetime and half the lifetime the provider
// actually granted — Google Drive clamps changes.watch server-side, so renewing
// on the *requested* half-life alone could leave a dead window (channel expired,
// next renewal not yet due) where notifications are silently dropped. On a failed
// renewal it retries on a short exponential backoff instead of waiting a full
// cycle, so one failure isn't retried near expiry and two in a row don't lapse.
func (l *Listener) subscriptionLoop(ctx context.Context) {
	normal := time.Duration(max(l.SubMinutes/2, 20)) * time.Minute
	const retryMin, retryMax = 2 * time.Minute, 15 * time.Minute
	retry := retryMin
	// First interval already honors the startup grant (renewSubscription ran before us).
	t := time.NewTimer(renewAfter(normal, l.subExpRemaining()))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if l.renewSubscription(ctx, "subscription renewed") {
				retry = retryMin
				t.Reset(renewAfter(normal, l.subExpRemaining()))
			} else {
				l.server.Log("warn", fmt.Sprintf("subscription renew failed; retrying in %s", retry))
				t.Reset(retry)
				if retry *= 2; retry > retryMax {
					retry = retryMax
				}
			}
		}
	}
}

// renewAfter picks how long to wait before the next renewal: the smaller of the
// configured half-life (normal) and half the lifetime the provider actually
// granted (granted). A zero/negative grant means "unknown" → fall back to normal.
// Floored at one minute so a tiny or already-past grant can't spin the loop.
func renewAfter(normal, granted time.Duration) time.Duration {
	next := normal
	if granted > 0 && granted/2 < next {
		next = granted / 2
	}
	if next < time.Minute {
		next = time.Minute
	}
	return next
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
	newLink, res, err := syncer.RunDelta(l.baseCtx, l.Src, l.GM, l.SpaceID, l.delta.load(), l.opts())
	if errors.Is(err, source.ErrCursorExpired) {
		l.server.Log("info", "[delta] cursor expired; running full sync")
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

	// A deletion hinted that descendants may have been orphaned without their own
	// change entries (e.g. a trashed Drive folder). Reconcile now so those memories
	// are removed promptly rather than lingering until the periodic full sync. We
	// still hold syncMu, so call runFull directly.
	if res.ReconcileRecommended {
		l.server.Log("info", "[delta] deletion hint → running full reconcile for orphaned descendants")
		l.runFull("folder-delete-reconcile")
	}
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
