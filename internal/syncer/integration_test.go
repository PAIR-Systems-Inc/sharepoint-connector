package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/gm"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/graph"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/memid"
)

// This file is an end-to-end harness: it drives the REAL graph.Client and the
// REAL Goodmem SDK client (internal/gm) against in-process fake SharePoint and
// Goodmem HTTP servers, exercising RunFull/RunDelta the way the connector does.

// ---------- fake Goodmem ----------

type fakeGoodmem struct {
	mu            sync.Mutex
	mems          map[string]map[string]any // memoryId -> memory object
	createStatus  string                    // processingStatus returned by POST create
	failCreateIDs map[string]bool           // memoryIds whose create returns 500
	getStatus     map[string]string         // memoryId -> processingStatus override for GET
	created       int
	deleted       int
}

func newFakeGoodmem() *fakeGoodmem {
	return &fakeGoodmem{
		mems:          map[string]map[string]any{},
		createStatus:  "COMPLETED",
		failCreateIDs: map[string]bool{},
		getStatus:     map[string]string{},
	}
}

func (f *fakeGoodmem) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mems)
}

func (f *fakeGoodmem) has(memoryID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.mems[memoryID]
	return ok
}

func (f *fakeGoodmem) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && p == "/v1/memories":
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var req struct {
				SpaceID           string         `json:"spaceId"`
				MemoryID          *string        `json:"memoryId"`
				ContentType       string         `json:"contentType"`
				Metadata          map[string]any `json:"metadata"`
				ExtractPageImages *bool          `json:"extractPageImages"`
			}
			_ = json.Unmarshal([]byte(r.FormValue("request")), &req)
			id := ""
			if req.MemoryID != nil {
				id = *req.MemoryID
			}
			if f.failCreateIDs[id] {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message":"simulated create failure"}`)
				return
			}
			mem := map[string]any{
				"memoryId":         id,
				"spaceId":          req.SpaceID,
				"contentType":      req.ContentType,
				"processingStatus": f.createStatus,
				"metadata":         req.Metadata,
			}
			f.mems[id] = mem
			f.created++
			_ = json.NewEncoder(w).Encode(mem)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/v1/spaces/") && strings.HasSuffix(p, "/memories"):
			list := make([]map[string]any, 0, len(f.mems))
			for _, m := range f.mems {
				list = append(list, m)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"memories": list})

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/v1/memories/"):
			id := strings.TrimPrefix(p, "/v1/memories/")
			m, ok := f.mems[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"not found"}`)
				return
			}
			out := map[string]any{}
			for k, v := range m {
				out[k] = v
			}
			if s, ok := f.getStatus[id]; ok {
				out["processingStatus"] = s
			}
			_ = json.NewEncoder(w).Encode(out)

		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/v1/memories/"):
			id := strings.TrimPrefix(p, "/v1/memories/")
			if _, ok := f.mems[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"not found"}`)
				return
			}
			delete(f.mems, id)
			f.deleted++
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"message":"unhandled %s %s"}`, r.Method, p)
		}
	})
}

// ---------- fake SharePoint (Graph) ----------

type gfile struct {
	id, name, mime, modified, content, parent string // parent "" = drive root, else a folder name
}

type gdelta struct {
	id      string
	deleted bool
}

type fakeGraph struct {
	mu     sync.Mutex
	base   string
	files  map[string]*gfile
	deltas []gdelta
}

func newFakeGraph() *fakeGraph { return &fakeGraph{files: map[string]*gfile{}} }

func (g *fakeGraph) put(f *gfile) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[f.id] = f
}

func (g *fakeGraph) del(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.files, id)
}

func (g *fakeGraph) setDeltas(d ...gdelta) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deltas = d
}

func (g *fakeGraph) fileJSON(f *gfile) map[string]any {
	return map[string]any{
		"id":                           f.id,
		"name":                         f.name,
		"lastModifiedDateTime":         f.modified,
		"@microsoft.graph.downloadUrl": g.base + "/download/" + f.id,
		"file":                         map[string]any{"mimeType": f.mime},
	}
}

func (g *fakeGraph) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/oauth2/v2.0/token"):
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)

		case p == "/drives/drive1/root/delta":
			var items []map[string]any
			if r.URL.Query().Get("token") != "latest" {
				for _, d := range g.deltas {
					if d.deleted {
						items = append(items, map[string]any{"id": d.id, "deleted": map[string]any{"state": "deleted"}})
					} else if f := g.files[d.id]; f != nil {
						items = append(items, g.fileJSON(f))
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value":            items,
				"@odata.deltaLink": g.base + "/drives/drive1/root/delta?token=next",
			})

		case p == "/drives/drive1/root/children":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": g.childrenOf("")})

		case strings.HasPrefix(p, "/drives/drive1/root:/") && strings.HasSuffix(p, ":/children"):
			folder := strings.TrimSuffix(strings.TrimPrefix(p, "/drives/drive1/root:/"), ":/children")
			_ = json.NewEncoder(w).Encode(map[string]any{"value": g.childrenOf(folder)})

		case strings.HasPrefix(p, "/drives/drive1/items/") && strings.HasSuffix(p, "/children"):
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}}) // no nested folders

		case strings.HasPrefix(p, "/drives/drive1/items/"):
			id := strings.TrimPrefix(p, "/drives/drive1/items/")
			if f := g.files[id]; f != nil {
				_ = json.NewEncoder(w).Encode(g.fileJSON(f))
			} else {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
			}

		case p == "/sites/site1/drives":
			fmt.Fprint(w, `{"value":[{"id":"drive1","name":"Docs"}]}`)

		case strings.HasPrefix(p, "/download/"):
			id := strings.TrimPrefix(p, "/download/")
			if f := g.files[id]; f != nil {
				w.Header().Set("Content-Type", "application/octet-stream")
				fmt.Fprint(w, f.content)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}

		case strings.HasPrefix(p, "/sites/") && strings.Contains(p, ":"):
			fmt.Fprint(w, `{"id":"site1"}`) // GetSiteID (host:path form)

		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":{"code":"unhandled","message":%q}}`, r.Method+" "+p)
		}
	})
}

func (g *fakeGraph) childrenOf(parent string) []map[string]any {
	var out []map[string]any
	for _, f := range g.files {
		if f.parent == parent {
			out = append(out, g.fileJSON(f))
		}
	}
	return out
}

// ---------- harness wiring ----------

func newHarness(t *testing.T) (*fakeGraph, *fakeGoodmem, *graph.Client, *goodmem.Client, *Retrier) {
	t.Helper()
	fg := newFakeGraph()
	gsrv := httptest.NewServer(fg.handler())
	t.Cleanup(gsrv.Close)
	fg.base = gsrv.URL

	fm := newFakeGoodmem()
	msrv := httptest.NewServer(fm.handler())
	t.Cleanup(msrv.Close)

	gc := graph.NewClient("cid", "tid", "sec", "https://contoso.sharepoint.com/sites/Test",
		graph.WithBaseURLs(gsrv.URL, gsrv.URL))
	gmc, err := gm.New(msrv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	// A Retrier with no real waiting, so pending-retry / polling paths run fast.
	r := NewRetrier(t.TempDir())
	r.pollInterval = 0
	r.sleep = func(context.Context, time.Duration) {}
	return fg, fm, gc, gmc, r
}

const spaceID = "space-1"

// ---------- tests ----------

func TestIntegration_FullSyncLifecycle(t *testing.T) {
	fg, fm, gc, gmc, _ := newHarness(t)
	fg.put(&gfile{id: "a", name: "a.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "A"})
	fg.put(&gfile{id: "b", name: "b.txt", mime: "text/plain", modified: "2026-01-01T00:00:00Z", content: "B"})
	fg.put(&gfile{id: "c", name: "c.bin", mime: "application/x-thing", modified: "2026-01-01T00:00:00Z", content: "C"}) // unsupported

	ctx := context.Background()

	// Initial full sync: two supported files added, one skipped.
	res, err := RunFull(ctx, gc, gmc, spaceID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Skipped != 1 || res.Deleted != 0 {
		t.Fatalf("first sync: added=%d skipped=%d deleted=%d, want 2/1/0", res.Added, res.Skipped, res.Deleted)
	}
	if !fm.has(memid.FromFileID("a")) || !fm.has(memid.FromFileID("b")) {
		t.Fatal("expected memories for a and b")
	}
	if fm.has(memid.FromFileID("c")) {
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
	fg.put(&gfile{id: "b", name: "b.txt", mime: "text/plain", modified: "2026-02-01T00:00:00Z", content: "B2"})
	res, err = RunFull(ctx, gc, gmc, spaceID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 || res.Added != 0 {
		t.Fatalf("after touch: updated=%d added=%d, want 1/0", res.Updated, res.Added)
	}

	// Delete a from SharePoint → one orphan delete.
	fg.del("a")
	res, err = RunFull(ctx, gc, gmc, spaceID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("after delete: deleted=%d, want 1", res.Deleted)
	}
	if fm.has(memid.FromFileID("a")) {
		t.Fatal("memory a should be deleted")
	}
}

func TestIntegration_MassDeleteGuard(t *testing.T) {
	fg, fm, gc, gmc, _ := newHarness(t)
	fg.put(&gfile{id: "a", name: "a.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "A"})
	ctx := context.Background()
	if _, err := RunFull(ctx, gc, gmc, spaceID, Options{}); err != nil {
		t.Fatal(err)
	}
	if fm.count() != 1 {
		t.Fatalf("setup: want 1 memory, got %d", fm.count())
	}

	// SharePoint now returns zero files (simulated transient failure) while
	// Goodmem still has memories: the guard must refuse and delete nothing.
	fg.del("a")
	res, err := RunFull(ctx, gc, gmc, spaceID, Options{})
	if err == nil {
		t.Fatal("expected mass-delete guard to return an error")
	}
	if !strings.Contains(err.Error(), "refusing to apply") {
		t.Fatalf("unexpected error: %v", err)
	}
	if fm.count() != 1 {
		t.Fatalf("guard must not delete: memory count=%d, want 1", fm.count())
	}
	_ = res
}

func TestIntegration_FolderScope(t *testing.T) {
	fg, fm, gc, gmc, _ := newHarness(t)
	fg.put(&gfile{id: "root1", name: "r.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "R", parent: ""})
	fg.put(&gfile{id: "rep1", name: "x.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "X", parent: "Reports"})
	fg.put(&gfile{id: "rep2", name: "y.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "Y", parent: "Reports"})

	// Scope to "Reports": only the two folder files sync, not the root file.
	res, err := RunFull(context.Background(), gc, gmc, spaceID, Options{FolderPath: "Reports"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 {
		t.Fatalf("folder scope: added=%d, want 2", res.Added)
	}
	if fm.has(memid.FromFileID("root1")) {
		t.Fatal("root file must not be synced under folder scope")
	}
	if !fm.has(memid.FromFileID("rep1")) || !fm.has(memid.FromFileID("rep2")) {
		t.Fatal("both Reports files should be synced")
	}
}

func TestIntegration_Delta(t *testing.T) {
	fg, fm, gc, gmc, r := newHarness(t)
	fg.put(&gfile{id: "a", name: "a.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "A"})
	fg.put(&gfile{id: "b", name: "b.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "B"})
	ctx := context.Background()
	if _, err := RunFull(ctx, gc, gmc, spaceID, Options{Retry: r}); err != nil {
		t.Fatal(err)
	}

	// Delta: a changed, b deleted, c newly added.
	fg.put(&gfile{id: "a", name: "a.pdf", mime: "application/pdf", modified: "2026-03-01T00:00:00Z", content: "A2"})
	fg.put(&gfile{id: "c", name: "c.pdf", mime: "application/pdf", modified: "2026-03-01T00:00:00Z", content: "C"})
	fg.del("b")
	fg.setDeltas(gdelta{id: "a"}, gdelta{id: "c"}, gdelta{id: "b", deleted: true})

	_, res, err := RunDelta(ctx, gc, gmc, spaceID, "drive1", fg.base+"/drives/drive1/root/delta?token=1", Options{Retry: r})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Updated != 1 || res.Deleted != 1 {
		t.Fatalf("delta: added=%d updated=%d deleted=%d, want 1/1/1", res.Added, res.Updated, res.Deleted)
	}
	if !fm.has(memid.FromFileID("c")) || fm.has(memid.FromFileID("b")) {
		t.Fatal("c should exist, b should be gone")
	}
}

func TestIntegration_PendingRetry(t *testing.T) {
	fg, fm, gc, gmc, r := newHarness(t)
	fg.put(&gfile{id: "a", name: "a.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "A"})
	ctx := context.Background()

	// First delta: Goodmem create fails → the add is dropped from this run but
	// queued in the pending-add set.
	fm.failCreateIDs[memid.FromFileID("a")] = true
	fg.setDeltas(gdelta{id: "a"})
	if _, res, err := RunDelta(ctx, gc, gmc, spaceID, "drive1", fg.base+"/drives/drive1/root/delta?token=1", Options{Retry: r}); err != nil {
		t.Fatal(err)
	} else if res.Added != 0 {
		t.Fatalf("failed create: added=%d, want 0", res.Added)
	}
	// Pending sets are keyed by SharePoint file ID (memories are keyed by UUID).
	if !r.loadAdd()["a"] {
		t.Fatal("file a should be queued in pending-add after a failed create")
	}
	if fm.count() != 0 {
		t.Fatal("nothing should be stored yet")
	}

	// Second delta with an EMPTY batch: the pending set is merged and retried.
	// Goodmem now succeeds → a is ingested and cleared from pending.
	fm.failCreateIDs = map[string]bool{}
	fg.setDeltas() // empty delta batch
	if _, _, err := RunDelta(ctx, gc, gmc, spaceID, "drive1", fg.base+"/drives/drive1/root/delta?token=2", Options{Retry: r}); err != nil {
		t.Fatal(err)
	}
	if !fm.has(memid.FromFileID("a")) {
		t.Fatal("file a should be ingested on the pending retry")
	}
	if r.loadAdd()["a"] {
		t.Fatal("file a should be cleared from pending-add after a successful retry")
	}
}

func TestIntegration_ProcessingStatusFailed(t *testing.T) {
	fg, fm, gc, gmc, r := newHarness(t)
	fg.put(&gfile{id: "a", name: "a.pdf", mime: "application/pdf", modified: "2026-01-01T00:00:00Z", content: "A"})
	fm.createStatus = "FAILED" // Goodmem accepts (200) but processing fails
	fg.setDeltas(gdelta{id: "a"})

	_, res, err := RunDelta(context.Background(), gc, gmc, spaceID, "drive1", fg.base+"/drives/drive1/root/delta?token=1", Options{Retry: r})
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
	if !r.loadUpdate()["a"] {
		t.Fatal("file a should be queued in pending-update after FAILED processing")
	}
}
