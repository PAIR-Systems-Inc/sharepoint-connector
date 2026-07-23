package syncer

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/fakes"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/gm"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/memid"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/providers/sharepoint"
)

// End-to-end harness: drives the REAL sharepoint.Client and the REAL Goodmem SDK
// client against in-process fake SharePoint + Goodmem HTTP servers (internal/fakes),
// exercising RunFull/RunDelta the way the connector does.

const spaceID = "space-1"

func newHarness(t *testing.T) (*fakes.Graph, *fakes.Goodmem, *sharepoint.Client, *goodmem.Client, *Retrier) {
	t.Helper()
	fg := fakes.NewGraph()
	gsrv := httptest.NewServer(fg.Handler())
	t.Cleanup(gsrv.Close)
	fg.SetBase(gsrv.URL)

	fm := fakes.NewGoodmem()
	msrv := httptest.NewServer(fm.Handler())
	t.Cleanup(msrv.Close)

	gc := sharepoint.NewClient("cid", "tid", "sec", "https://contoso.sharepoint.com/sites/Test",
		sharepoint.WithBaseURLs(gsrv.URL, gsrv.URL))
	gmc, err := gm.New(msrv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	// A Retrier with no real waiting, so pending-retry / polling paths run fast.
	r := NewRetrier(t.TempDir(), 0)
	r.pollInterval = 0
	r.sleep = func(context.Context, time.Duration) {}
	return fg, fm, gc, gmc, r
}

func TestIntegration_FullSyncLifecycle(t *testing.T) {
	fg, fm, gc, gmc, _ := newHarness(t)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})
	fg.Put(fakes.File{ID: "b", Name: "b.txt", Mime: "text/plain", Modified: "2026-01-01T00:00:00Z", Content: "B"})
	fg.Put(fakes.File{ID: "c", Name: "c.bin", Mime: "application/x-thing", Modified: "2026-01-01T00:00:00Z", Content: "C"}) // unsupported

	ctx := context.Background()

	// Initial full sync: two supported files added, one skipped.
	res, err := RunFull(ctx, gc, gmc, spaceID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Skipped != 1 || res.Deleted != 0 {
		t.Fatalf("first sync: added=%d skipped=%d deleted=%d, want 2/1/0", res.Added, res.Skipped, res.Deleted)
	}
	if !fm.Has(memid.FromFileID("a")) || !fm.Has(memid.FromFileID("b")) {
		t.Fatal("expected memories for a and b")
	}
	if fm.Has(memid.FromFileID("c")) {
		t.Fatal("unsupported file c must not be ingested")
	}

	// Re-run: idempotent (nothing changed).
	res, err = RunFull(ctx, gc, gmc, spaceID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 || res.Updated != 0 || res.Deleted != 0 {
		t.Fatalf("idempotent re-run: added=%d updated=%d deleted=%d, want 0/0/0", res.Added, res.Updated, res.Deleted)
	}

	// Touch b (newer timestamp) → one update.
	fg.Put(fakes.File{ID: "b", Name: "b.txt", Mime: "text/plain", Modified: "2026-02-01T00:00:00Z", Content: "B2"})
	res, err = RunFull(ctx, gc, gmc, spaceID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 || res.Added != 0 {
		t.Fatalf("after touch: updated=%d added=%d, want 1/0", res.Updated, res.Added)
	}

	// Delete a from SharePoint → one orphan delete.
	fg.Del("a")
	res, err = RunFull(ctx, gc, gmc, spaceID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("after delete: deleted=%d, want 1", res.Deleted)
	}
	if fm.Has(memid.FromFileID("a")) {
		t.Fatal("memory a should be deleted")
	}
}

func TestIntegration_SizeCap(t *testing.T) {
	fg, fm, gc, gmc, _ := newHarness(t)
	fg.Put(fakes.File{ID: "small", Name: "s.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "S", Size: 1_000})
	fg.Put(fakes.File{ID: "big", Name: "b.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "B", Size: 500_000_000}) // 500 MB

	res, err := RunFull(context.Background(), gc, gmc, spaceID, Options{MaxFileBytes: 100 * 1024 * 1024}) // 100 MB cap
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Skipped != 1 {
		t.Fatalf("size cap: added=%d skipped=%d, want 1/1", res.Added, res.Skipped)
	}
	if !fm.Has(memid.FromFileID("small")) {
		t.Error("small file should be ingested")
	}
	if fm.Has(memid.FromFileID("big")) {
		t.Error("oversized file must be skipped, not ingested")
	}
}

func TestIntegration_MassDeleteGuard(t *testing.T) {
	fg, fm, gc, gmc, _ := newHarness(t)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})
	ctx := context.Background()
	if _, err := RunFull(ctx, gc, gmc, spaceID, Options{}); err != nil {
		t.Fatal(err)
	}
	if fm.Count() != 1 {
		t.Fatalf("setup: want 1 memory, got %d", fm.Count())
	}

	// SharePoint now returns zero files (simulated transient failure) while
	// Goodmem still has memories: the guard must refuse and delete nothing.
	fg.Del("a")
	_, err := RunFull(ctx, gc, gmc, spaceID, Options{})
	if err == nil || !strings.Contains(err.Error(), "refusing to apply") {
		t.Fatalf("expected mass-delete guard error, got: %v", err)
	}
	if fm.Count() != 1 {
		t.Fatalf("guard must not delete: memory count=%d, want 1", fm.Count())
	}
}

func TestIntegration_FolderScope(t *testing.T) {
	fg, fm, gc, gmc, _ := newHarness(t)
	fg.Put(fakes.File{ID: "root1", Name: "r.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "R"})
	fg.Put(fakes.File{ID: "rep1", Name: "x.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "X", Parent: "Reports"})
	fg.Put(fakes.File{ID: "rep2", Name: "y.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "Y", Parent: "Reports"})

	res, err := RunFull(context.Background(), gc, gmc, spaceID, Options{FolderPath: "Reports"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 {
		t.Fatalf("folder scope: added=%d, want 2", res.Added)
	}
	if fm.Has(memid.FromFileID("root1")) {
		t.Fatal("root file must not be synced under folder scope")
	}
	if !fm.Has(memid.FromFileID("rep1")) || !fm.Has(memid.FromFileID("rep2")) {
		t.Fatal("both Reports files should be synced")
	}
}

func TestIntegration_Delta(t *testing.T) {
	fg, fm, gc, gmc, r := newHarness(t)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})
	fg.Put(fakes.File{ID: "b", Name: "b.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "B"})
	ctx := context.Background()
	if _, err := RunFull(ctx, gc, gmc, spaceID, Options{Retry: r}); err != nil {
		t.Fatal(err)
	}

	// Delta: a changed, b deleted, c newly added.
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-03-01T00:00:00Z", Content: "A2"})
	fg.Put(fakes.File{ID: "c", Name: "c.pdf", Mime: "application/pdf", Modified: "2026-03-01T00:00:00Z", Content: "C"})
	fg.Del("b")
	fg.SetDeltas(fakes.Delta{ID: "a"}, fakes.Delta{ID: "c"}, fakes.Delta{ID: "b", Deleted: true})

	_, res, err := RunDelta(ctx, gc, gmc, spaceID, "drive1", fg.DeltaLink(), Options{Retry: r})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Updated != 1 || res.Deleted != 1 {
		t.Fatalf("delta: added=%d updated=%d deleted=%d, want 1/1/1", res.Added, res.Updated, res.Deleted)
	}
	if !fm.Has(memid.FromFileID("c")) || fm.Has(memid.FromFileID("b")) {
		t.Fatal("c should exist, b should be gone")
	}
}

func TestIntegration_PendingRetry(t *testing.T) {
	fg, fm, gc, gmc, r := newHarness(t)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})
	ctx := context.Background()

	// First delta: Goodmem create fails → the add is dropped from this run but
	// queued in the pending-add set.
	fm.FailCreateIDs[memid.FromFileID("a")] = true
	fg.SetDeltas(fakes.Delta{ID: "a"})
	if _, res, err := RunDelta(ctx, gc, gmc, spaceID, "drive1", fg.DeltaLink(), Options{Retry: r}); err != nil {
		t.Fatal(err)
	} else if res.Added != 0 {
		t.Fatalf("failed create: added=%d, want 0", res.Added)
	}
	// Pending sets are keyed by SharePoint file ID (memories are keyed by UUID).
	if !pending(r.loadAdd(), "a") {
		t.Fatal("file a should be queued in pending-add after a failed create")
	}
	if fm.Count() != 0 {
		t.Fatal("nothing should be stored yet")
	}

	// Second delta with an EMPTY batch: the pending set is merged and retried.
	// Goodmem now succeeds → a is ingested and cleared from pending.
	fm.FailCreateIDs = map[string]bool{}
	fg.SetDeltas() // empty delta batch
	if _, _, err := RunDelta(ctx, gc, gmc, spaceID, "drive1", fg.DeltaLink(), Options{Retry: r}); err != nil {
		t.Fatal(err)
	}
	if !fm.Has(memid.FromFileID("a")) {
		t.Fatal("file a should be ingested on the pending retry")
	}
	if pending(r.loadAdd(), "a") {
		t.Fatal("file a should be cleared from pending-add after a successful retry")
	}
}

func TestIntegration_ProcessingStatusFailed(t *testing.T) {
	fg, fm, gc, gmc, r := newHarness(t)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})
	fm.CreateStatus = "FAILED" // Goodmem accepts (200) but processing fails
	fg.SetDeltas(fakes.Delta{ID: "a"})

	_, res, err := RunDelta(context.Background(), gc, gmc, spaceID, "drive1", fg.DeltaLink(), Options{Retry: r})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 {
		t.Fatalf("FAILED processing must not count as added: added=%d, want 0", res.Added)
	}
	if len(res.Errors) == 0 {
		t.Fatal("a FAILED create should be recorded as an error")
	}
	// FAILED re-queues as an update (delete-then-add) for the next sync.
	if !pending(r.loadUpdate(), "a") {
		t.Fatal("file a should be queued in pending-update after FAILED processing")
	}
}
