package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

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
