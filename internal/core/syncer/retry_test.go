package syncer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/gm"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// pending reports whether id is present in a pending set (attempt count ignored).
func pending(set map[string]int, id string) bool {
	_, ok := set[id]
	return ok
}

// TestRetrierPendingSets covers the outcome → pending-set state machine and
// persistence across Retrier instances.
func TestRetrierPendingSets(t *testing.T) {
	dir := t.TempDir()
	r := NewRetrier(dir, 0) // default max attempts

	// Transient add failure queues a retry-as-add; a later success clears it.
	r.recordAdd("f1", resTransient)
	if !pending(r.loadAdd(), "f1") {
		t.Fatal("f1 should be pending-add after transient failure")
	}
	r.recordAdd("f1", resOK)
	if pending(r.loadAdd(), "f1") {
		t.Fatal("f1 should be cleared after success")
	}

	// A create that came back FAILED re-queues as an UPDATE (delete-then-add),
	// not a plain add.
	r.recordAdd("f2", resFailedProcessing)
	if pending(r.loadAdd(), "f2") {
		t.Error("f2 should not be pending-add")
	}
	if !pending(r.loadUpdate(), "f2") {
		t.Error("f2 should be pending-update after FAILED processing")
	}

	// Update failure queues pending-update; success clears it.
	r.recordUpdate("f3", resTransient)
	if !pending(r.loadUpdate(), "f3") {
		t.Fatal("f3 should be pending-update")
	}
	r.recordUpdate("f3", resOK)
	if pending(r.loadUpdate(), "f3") {
		t.Fatal("f3 should be cleared")
	}

	// Remove failure/success.
	r.recordRemove("f4", false)
	if !pending(r.loadRemove(), "f4") {
		t.Fatal("f4 should be pending-remove after failure")
	}
	r.recordRemove("f4", true)
	if pending(r.loadRemove(), "f4") {
		t.Fatal("f4 should be cleared after success")
	}

	// Skipped clears any pending entry (permanent skip: unsupported/oversized).
	r.recordAdd("f6", resTransient)
	r.recordAdd("f6", resSkipped)
	if pending(r.loadAdd(), "f6") {
		t.Error("skipped item must be discarded from pending-add")
	}

	// Persistence: a fresh Retrier on the same dir sees the surviving entry.
	r.recordAdd("f5", resTransient)
	if !pending(NewRetrier(dir, 0).loadAdd(), "f5") {
		t.Fatal("pending-add did not persist across Retrier instances")
	}

	// nil-safe (one-shot CLI path).
	var rn *Retrier
	rn.recordAdd("x", resTransient) // must not panic
	rn.recordUpdate("x", resOK)
	rn.recordRemove("x", false)
	if rn.loadAdd() != nil {
		t.Error("nil Retrier loadAdd should be nil")
	}
}

// TestDeadLetter verifies an item is parked after maxAttempts transient failures
// (removed from the pending set, added to the dead set, one event emitted), and
// that a later success revives it out of the dead set.
func TestDeadLetter(t *testing.T) {
	dir := t.TempDir()
	var events []SyncEvent
	r := NewRetrier(dir, 3)
	r.Sink = func(e SyncEvent) { events = append(events, e) }

	// Three transient failures: attempts 1, 2, then park on 3.
	r.recordAdd("bad", resTransient)
	r.recordAdd("bad", resTransient)
	if !pending(r.loadAdd(), "bad") {
		t.Fatal("bad should still be pending after 2 attempts")
	}
	if got := r.loadAdd()["bad"]; got != 2 {
		t.Fatalf("attempt count = %d, want 2", got)
	}
	r.recordAdd("bad", resTransient) // 3rd attempt → dead-letter

	if pending(r.loadAdd(), "bad") {
		t.Error("bad should be removed from pending-add once parked")
	}
	_, _, _, dead := r.Counts()
	if dead != 1 {
		t.Errorf("dead count = %d, want 1", dead)
	}
	if len(events) != 1 || events[0].Status != "dead" || events[0].FileID != "bad" {
		t.Errorf("expected one dead-letter event for 'bad', got %+v", events)
	}

	// The file starts working again (e.g. after an edit): a success clears it from
	// the dead set too.
	r.recordAdd("bad", resOK)
	if _, _, _, dead := r.Counts(); dead != 0 {
		t.Errorf("dead count after revival = %d, want 0", dead)
	}
}

// TestPollStatus verifies polling returns COMPLETED/FAILED/PENDING correctly.
func TestPollStatus(t *testing.T) {
	cases := []struct {
		name          string
		sequence      []string // processingStatus returned on successive Get calls
		maxAttempts   int
		want          string
		wantMinChecks int32
	}{
		{"immediate completed", []string{"COMPLETED"}, 30, "COMPLETED", 1},
		{"pending then completed", []string{"PENDING", "PROCESSING", "COMPLETED"}, 30, "COMPLETED", 3},
		{"failed", []string{"FAILED"}, 30, "FAILED", 1},
		{"timeout still pending", []string{"PENDING"}, 3, "PENDING", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				i := int(atomic.AddInt32(&n, 1)) - 1
				status := tc.sequence[len(tc.sequence)-1]
				if i < len(tc.sequence) {
					status = tc.sequence[i]
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"processingStatus":%q}`, status)
			}))
			defer srv.Close()

			gmc, err := gm.New(srv.URL, "key")
			if err != nil {
				t.Fatal(err)
			}
			r := NewRetrier(t.TempDir(), 0)
			r.pollMaxAttempts = tc.maxAttempts
			r.sleep = func(context.Context, time.Duration) {} // no real waiting
			if got := r.pollStatus(context.Background(), gmc, "mem-1"); got != tc.want {
				t.Errorf("pollStatus = %q, want %q (after %d checks)", got, tc.want, atomic.LoadInt32(&n))
			}
			if got := atomic.LoadInt32(&n); got < tc.wantMinChecks {
				t.Errorf("Get calls = %d, want >= %d", got, tc.wantMinChecks)
			}
		})
	}
}

// TestMergeAndConflicts covers pending-merge (re-fetch, 404 discard) and the
// intra-sync conflict resolution.
func TestMergeAndConflicts(t *testing.T) {
	supported := &source.FileInfo{ID: "keep", Name: "keep.pdf", MimeType: "application/pdf", DownloadRef: "http://x/keep"}
	getter := &fakeGetter{
		files: map[string]*source.FileInfo{
			"keep":     supported,
			"conflict": {ID: "conflict", Name: "c.pdf", MimeType: "application/pdf", DownloadRef: "http://x/c"},
		},
		errs: map[string]error{
			"gone": source.ErrNotFound,
		},
	}
	dir := t.TempDir()
	r := NewRetrier(dir, 0)
	// Seed pending sets: an add that still exists, an add that's gone (ErrNotFound),
	// and a remove that conflicts with a live delta add of the same file.
	r.recordAdd("keep", resTransient)
	r.recordAdd("gone", resTransient)
	r.recordRemove("conflict", false)

	deltaAdd := []source.FileInfo{{ID: "conflict", Name: "c.pdf", MimeType: "application/pdf", DownloadRef: "http://x/c"}}
	adds, updates, removes := r.merge(context.Background(), getter, deltaAdd, nil, nil)

	// "keep" merged into adds; "gone" dropped and discarded from pending-add.
	if !hasFileID(adds, "keep") {
		t.Error("expected 'keep' merged into adds")
	}
	if hasFileID(adds, "gone") {
		t.Error("'gone' (404) must not be added")
	}
	if pending(r.loadAdd(), "gone") {
		t.Error("'gone' should be discarded from pending-add")
	}
	if !pending(r.loadAdd(), "keep") {
		t.Error("'keep' should remain pending until its ingest succeeds")
	}
	// Conflict: 'conflict' is a live add AND a pending remove → file exists, so
	// the remove is dropped and pending-remove cleared.
	if containsStr(removes, "conflict") {
		t.Error("'conflict' should be removed from the remove list (file exists)")
	}
	if pending(r.loadRemove(), "conflict") {
		t.Error("pending-remove for 'conflict' should be discarded")
	}
	if !hasFileID(adds, "conflict") {
		t.Error("'conflict' should remain in adds")
	}
	_ = updates
}

type fakeGetter struct {
	files map[string]*source.FileInfo
	errs  map[string]error
}

func (f *fakeGetter) GetFile(ctx context.Context, id string) (*source.FileInfo, error) {
	if e := f.errs[id]; e != nil {
		return nil, e
	}
	return f.files[id], nil
}
