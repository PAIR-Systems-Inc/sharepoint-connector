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
	"time"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/graph"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/syncer"
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
	DeltaPath         string // file holding the Graph delta link
	ExtractPageImages bool   // hint Goodmem to extract page images

	driveID string
	delta   deltaStore
	retry   *syncer.Retrier
	server  *Server
	baseCtx context.Context
	syncMu  sync.Mutex // serialize full/delta syncs
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
	l.retry = syncer.NewRetrier(stateDir)
	l.server = New(l.ClientState, func(int) { l.onNotification() })
	l.server.Metrics.SetPendingFn(l.retry.Counts)
	l.server.Log("info", "durable state dir: "+stateDir+" (delta cursor + pending-retry sets)")

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
	go l.subscriptionLoop(ctx)     // periodic renewal
	go l.periodicFullSyncLoop(ctx) // periodic safety full-sync (repairs missed deltas)

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
	}
}

// fullSyncLocked runs a full sync and re-bootstraps the delta link, serialized
// against other syncs. tag labels the activity log ("startup" / "periodic").
func (l *Listener) fullSyncLocked(tag string) {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	l.server.Log("info", "["+tag+"] full sync starting")
	res, err := syncer.RunFull(l.baseCtx, l.GC, l.GM, l.SpaceID, syncer.Options{ExtractPageImages: l.ExtractPageImages, Retry: l.retry})
	if err != nil {
		l.server.Log("error", "["+tag+"] full sync: "+err.Error())
	} else {
		l.server.Log("info", fmt.Sprintf("[%s] full sync done: +%d ~%d -%d (skipped %d)", tag, res.Added, res.Updated, res.Deleted, res.Skipped))
	}
	l.server.Metrics.RecordSync("full", res)
	if _, link, err := l.GC.DriveDelta(l.driveID, "", true); err == nil && link != "" {
		if err := l.delta.save(link); err == nil {
			l.server.Log("info", "["+tag+"] delta link saved")
		}
	}
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

// subscriptionLoop renews the subscription roughly every half-lifetime.
func (l *Listener) subscriptionLoop(ctx context.Context) {
	interval := time.Duration(max(l.SubMinutes/2, 20)) * time.Minute
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sub, err := l.GC.EnsureSubscription(l.NotificationURL, l.ClientState, l.SubMinutes)
			l.server.Metrics.RecordRenewal(err == nil)
			if err != nil {
				l.server.Log("error", "subscription renew: "+err.Error())
			} else {
				l.server.Log("info", "subscription renewed (expires "+sub.ExpirationDateTime+")")
			}
		}
	}
}

// onNotification runs a delta sync (serialized). On an expired delta token it
// falls back to a full sync and re-bootstraps the delta link.
func (l *Listener) onNotification() {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	l.server.Log("info", "[delta] sync starting")
	newLink, res, err := syncer.RunDelta(l.baseCtx, l.GC, l.GM, l.SpaceID, l.driveID, l.delta.load(), syncer.Options{ExtractPageImages: l.ExtractPageImages, Retry: l.retry})
	if err == syncer.ErrDeltaExpired {
		l.server.Log("info", "[delta] token expired; running full sync")
		fres, ferr := syncer.RunFull(l.baseCtx, l.GC, l.GM, l.SpaceID, syncer.Options{ExtractPageImages: l.ExtractPageImages, Retry: l.retry})
		l.server.Metrics.RecordSync("full", fres)
		if ferr != nil {
			l.server.Log("error", "[delta] fallback full sync: "+ferr.Error())
		}
		if _, link, e := l.GC.DriveDelta(l.driveID, "", true); e == nil && link != "" {
			_ = l.delta.save(link)
		}
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
