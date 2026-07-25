package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/fakes"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/gm"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/providers/sharepoint"
)

// TestSignalCoalesces: a burst of notifications collapses to a single queued
// delta run (1-buffered channel), and signal never blocks.
func TestSignalCoalesces(t *testing.T) {
	l := &Listener{notify: make(chan struct{}, 1)}
	for i := 0; i < 10; i++ {
		l.signal() // must never block, even when a run is already queued
	}
	if got := len(l.notify); got != 1 {
		t.Fatalf("after 10 signals, queued = %d, want 1 (coalesced)", got)
	}
	<-l.notify // the single worker drains one signal...
	select {
	case <-l.notify:
		t.Fatal("only one run should have been queued for the whole burst")
	default:
	}
	l.signal()
	if got := len(l.notify); got != 1 {
		t.Fatalf("signal after drain: queued = %d, want 1", got)
	}
}

// TestRenewAfter: the renewal cadence never exceeds half the lifetime the
// provider actually granted (Drive can grant less than requested), falls back to
// the configured half-life when the grant is unknown, and is floored so a tiny or
// past grant can't spin the loop.
func TestRenewAfter(t *testing.T) {
	normal := 36 * time.Hour
	cases := []struct {
		name    string
		granted time.Duration
		want    time.Duration
	}{
		{"unknown grant → normal", 0, normal},
		{"generous grant → normal", 10 * 24 * time.Hour, normal},
		{"short grant → half of it", 24 * time.Hour, 12 * time.Hour},
		{"tiny grant → floored", 30 * time.Second, time.Minute},
		{"already expired → floored", -time.Hour, normal}, // negative treated as unknown → normal
	}
	for _, c := range cases {
		if got := renewAfter(normal, c.granted); got != c.want {
			t.Errorf("%s: renewAfter(%s, %s) = %s, want %s", c.name, normal, c.granted, got, c.want)
		}
	}
}

// TestRunFullCursorAdvance: the delta cursor advances only when the full sync
// succeeds; a failed full sync must keep the old cursor so the missed window is
// retried rather than skipped.
func TestRunFullCursorAdvance(t *testing.T) {
	fg := fakes.NewGraph()
	gsrv := httptest.NewServer(fg.Handler())
	defer gsrv.Close()
	fg.SetBase(gsrv.URL)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})

	fm := fakes.NewGoodmem()
	msrv := httptest.NewServer(fm.Handler())
	defer msrv.Close()

	gc := sharepoint.NewClient("cid", "tid", "sec", "https://contoso.sharepoint.com/sites/Test",
		sharepoint.WithBaseURLs(gsrv.URL, gsrv.URL))
	gmc, err := gm.New(msrv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}

	s := sharepoint.NewAdapter(gc, "", "cs")
	l := &Listener{
		Src:     s,
		GM:      gmc,
		SpaceID: "space-1",
		baseCtx: context.Background(),
		delta:   deltaStore{path: filepath.Join(t.TempDir(), "delta")},
	}
	l.server = New(s, nil)

	// Successful full sync advances the cursor.
	if err := l.runFull("s1"); err != nil {
		t.Fatalf("first full sync should succeed: %v", err)
	}
	if l.delta.load() == "" {
		t.Fatal("delta cursor should be advanced after a successful full sync")
	}

	// Pre-seed a sentinel, then make SharePoint return zero files while Goodmem is
	// non-empty → the mass-delete guard fails the sync. The cursor must be kept.
	l.delta.save("SENTINEL-KEEP")
	fg.Del("a")
	if err := l.runFull("s2"); err == nil {
		t.Fatal("second full sync should fail via the mass-delete guard")
	}
	if got := l.delta.load(); got != "SENTINEL-KEEP" {
		t.Fatalf("delta cursor advanced on a FAILED full sync: got %q, want it kept", got)
	}
}

// TestPeriodicFullSyncDisabled: FullSyncMinutes <= 0 disables the loop, so it
// returns immediately instead of ticking.
func TestPeriodicFullSyncDisabled(t *testing.T) {
	l := &Listener{FullSyncMinutes: 0}
	done := make(chan struct{})
	go func() { l.periodicFullSyncLoop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("periodicFullSyncLoop should return immediately when disabled")
	}
}

func TestDeltaStore(t *testing.T) {
	d := deltaStore{path: filepath.Join(t.TempDir(), "delta")}

	if got := d.load(); got != "" {
		t.Errorf("load of missing store = %q, want empty", got)
	}
	if err := d.save("  https://graph.microsoft.com/v1.0/drives/x/root/delta?token=abc \n"); err != nil {
		t.Fatal(err)
	}
	if got := d.load(); got != "https://graph.microsoft.com/v1.0/drives/x/root/delta?token=abc" {
		t.Errorf("load = %q (want trimmed link)", got)
	}
}
