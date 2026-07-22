// Package fakes provides in-process fake Microsoft Graph (SharePoint) and
// Goodmem HTTP servers for end-to-end tests: the real graph.Client and the real
// Goodmem SDK talk to these over real HTTP, exercising the connector's actual
// wire behavior. Test-only — nothing in the shipped binary imports it.
package fakes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// ---------- fake Goodmem ----------

// Goodmem is a stateful fake of the Goodmem memories API.
type Goodmem struct {
	mu   sync.Mutex
	mems map[string]map[string]any // memoryId -> memory object

	// Knobs (guard with the mutex only if mutated during a running sync).
	CreateStatus  string          // processingStatus returned by POST create (default COMPLETED)
	FailCreateIDs map[string]bool // memoryIds whose create returns 500
	GetStatus     map[string]string
}

// NewGoodmem returns an empty fake Goodmem whose creates report COMPLETED.
func NewGoodmem() *Goodmem {
	return &Goodmem{
		mems:          map[string]map[string]any{},
		CreateStatus:  "COMPLETED",
		FailCreateIDs: map[string]bool{},
		GetStatus:     map[string]string{},
	}
}

// Count returns the number of stored memories.
func (f *Goodmem) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mems)
}

// Has reports whether a memory with the given id exists.
func (f *Goodmem) Has(memoryID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.mems[memoryID]
	return ok
}

// Handler returns the HTTP handler implementing the memories endpoints.
func (f *Goodmem) Handler() http.Handler {
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
				SpaceID     string         `json:"spaceId"`
				MemoryID    *string        `json:"memoryId"`
				ContentType string         `json:"contentType"`
				Metadata    map[string]any `json:"metadata"`
			}
			_ = json.Unmarshal([]byte(r.FormValue("request")), &req)
			id := ""
			if req.MemoryID != nil {
				id = *req.MemoryID
			}
			if f.FailCreateIDs[id] {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message":"simulated create failure"}`)
				return
			}
			mem := map[string]any{
				"memoryId":         id,
				"spaceId":          req.SpaceID,
				"contentType":      req.ContentType,
				"processingStatus": f.CreateStatus,
				"metadata":         req.Metadata,
			}
			f.mems[id] = mem
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
			if s, ok := f.GetStatus[id]; ok {
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
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"message":"unhandled %s %s"}`, r.Method, p)
		}
	})
}

// ---------- fake SharePoint (Graph) ----------

// File is a fake SharePoint drive file. Parent "" = drive root, else a folder name.
type File struct {
	ID, Name, Mime, Modified, Content, Parent string
	Size                                      int64 // reported drive size; 0 = len(Content)
}

// Delta is one scripted delta item (a change, or a deletion).
type Delta struct {
	ID      string
	Deleted bool
}

// Graph is a stateful fake of the subset of Microsoft Graph the client uses.
type Graph struct {
	mu     sync.Mutex
	base   string
	files  map[string]*File
	deltas []Delta
}

// NewGraph returns an empty fake Graph.
func NewGraph() *Graph { return &Graph{files: map[string]*File{}} }

// SetBase records the server's own base URL (used to build download/delta links).
func (g *Graph) SetBase(url string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.base = url
}

// Put adds or replaces a file.
func (g *Graph) Put(f File) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := f
	g.files[f.ID] = &c
}

// Del removes a file.
func (g *Graph) Del(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.files, id)
}

// SetDeltas sets the items the next delta call returns.
func (g *Graph) SetDeltas(d ...Delta) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deltas = d
}

// DeltaLink returns a delta-link URL the fake serves the scripted deltas for
// (pass to RunDelta as the current cursor).
func (g *Graph) DeltaLink() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.base + "/drives/drive1/root/delta?token=x"
}

func (g *Graph) fileJSON(f *File) map[string]any {
	size := f.Size
	if size == 0 {
		size = int64(len(f.Content))
	}
	return map[string]any{
		"id":                           f.ID,
		"name":                         f.Name,
		"lastModifiedDateTime":         f.Modified,
		"size":                         size,
		"@microsoft.graph.downloadUrl": g.base + "/download/" + f.ID,
		"file":                         map[string]any{"mimeType": f.Mime},
	}
}

func (g *Graph) childrenOf(parent string) []map[string]any {
	var out []map[string]any
	for _, f := range g.files {
		if f.Parent == parent {
			out = append(out, g.fileJSON(f))
		}
	}
	return out
}

// Handler returns the HTTP handler implementing the Graph endpoints.
func (g *Graph) Handler() http.Handler {
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
					if d.Deleted {
						items = append(items, map[string]any{"id": d.ID, "deleted": map[string]any{"state": "deleted"}})
					} else if f := g.files[d.ID]; f != nil {
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

		case p == "/subscriptions" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"value":[]}`) // no existing subscriptions → EnsureSubscription creates one

		case p == "/subscriptions" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"sub-1","resource":"sites/site1/drives/drive1/root","expirationDateTime":"2099-01-01T00:00:00.000Z"}`)

		case strings.HasPrefix(p, "/download/"):
			id := strings.TrimPrefix(p, "/download/")
			if f := g.files[id]; f != nil {
				w.Header().Set("Content-Type", "application/octet-stream")
				fmt.Fprint(w, f.Content)
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
