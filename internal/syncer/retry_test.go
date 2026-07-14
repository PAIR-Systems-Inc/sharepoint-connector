package syncer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/gm"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/graph"
)

// TestRetrierPendingSets covers the outcome → pending-set state machine and
// persistence across Retrier instances.
func TestRetrierPendingSets(t *testing.T) {
	dir := t.TempDir()
	r := NewRetrier(dir)

	// Transient add failure queues a retry-as-add; a later success clears it.
	r.recordAdd("f1", resTransient)
	if !r.loadAdd()["f1"] {
		t.Fatal("f1 should be pending-add after transient failure")
	}
	r.recordAdd("f1", resOK)
	if r.loadAdd()["f1"] {
		t.Fatal("f1 should be cleared after success")
	}

	// A create that came back FAILED re-queues as an UPDATE (delete-then-add),
	// not a plain add.
	r.recordAdd("f2", resFailedProcessing)
	if r.loadAdd()["f2"] {
		t.Error("f2 should not be pending-add")
	}
	if !r.loadUpdate()["f2"] {
		t.Error("f2 should be pending-update after FAILED processing")
	}

	// Update failure queues pending-update; success clears it.
	r.recordUpdate("f3", resTransient)
	if !r.loadUpdate()["f3"] {
		t.Fatal("f3 should be pending-update")
	}
	r.recordUpdate("f3", resOK)
	if r.loadUpdate()["f3"] {
		t.Fatal("f3 should be cleared")
	}

	// Remove failure/success.
	r.recordRemove("f4", false)
	if !r.loadRemove()["f4"] {
		t.Fatal("f4 should be pending-remove after failure")
	}
	r.recordRemove("f4", true)
	if r.loadRemove()["f4"] {
		t.Fatal("f4 should be cleared after success")
	}

	// Skipped is a no-op (never queued).
	r.recordAdd("f6", resSkipped)
	if r.loadAdd()["f6"] {
		t.Error("skipped item must not be queued")
	}

	// Persistence: a fresh Retrier on the same dir sees the surviving entry.
	r.recordAdd("f5", resTransient)
	if !NewRetrier(dir).loadAdd()["f5"] {
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
			r := NewRetrier(t.TempDir())
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
	supported := &graph.FileInfo{ID: "keep", Name: "keep.pdf", MimeType: "application/pdf", DownloadURL: "http://x/keep"}
	getter := &fakeGetter{
		files: map[string]*graph.FileInfo{
			"keep":     supported,
			"conflict": {ID: "conflict", Name: "c.pdf", MimeType: "application/pdf", DownloadURL: "http://x/c"},
		},
		errs: map[string]error{
			"gone": &graph.HTTPError{StatusCode: 404},
		},
	}
	dir := t.TempDir()
	r := NewRetrier(dir)
	// Seed pending sets: an add that still exists, an add that's 404-gone, and a
	// remove that conflicts with a live delta add of the same file.
	r.addAdd("keep")
	r.addAdd("gone")
	r.addRemove("conflict")

	deltaAdd := []graph.FileInfo{{ID: "conflict", Name: "c.pdf", MimeType: "application/pdf", DownloadURL: "http://x/c"}}
	adds, updates, removes := r.merge(getter, "drive1", deltaAdd, nil, nil)

	// "keep" merged into adds; "gone" dropped and discarded from pending-add.
	if !hasFileID(adds, "keep") {
		t.Error("expected 'keep' merged into adds")
	}
	if hasFileID(adds, "gone") {
		t.Error("'gone' (404) must not be added")
	}
	if r.loadAdd()["gone"] {
		t.Error("'gone' should be discarded from pending-add")
	}
	if !r.loadAdd()["keep"] {
		t.Error("'keep' should remain pending until its ingest succeeds")
	}
	// Conflict: 'conflict' is a live add AND a pending remove → file exists, so
	// the remove is dropped and pending-remove cleared.
	if containsStr(removes, "conflict") {
		t.Error("'conflict' should be removed from the remove list (file exists)")
	}
	if r.loadRemove()["conflict"] {
		t.Error("pending-remove for 'conflict' should be discarded")
	}
	if !hasFileID(adds, "conflict") {
		t.Error("'conflict' should remain in adds")
	}
	_ = updates
}

type fakeGetter struct {
	files map[string]*graph.FileInfo
	errs  map[string]error
}

func (f *fakeGetter) GetFileByID(driveID, id string) (*graph.FileInfo, error) {
	if e := f.errs[id]; e != nil {
		return nil, e
	}
	return f.files[id], nil
}
