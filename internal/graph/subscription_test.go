package graph

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEnsureSubscription covers both the create path (no existing subscription)
// and the renew path (a matching resource+clientState already exists).
func TestEnsureSubscription(t *testing.T) {
	var created, renewed bool
	var existing []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
		case strings.HasPrefix(r.URL.Path, "/sites/") && strings.Contains(r.URL.Path, "/drives"):
			fmt.Fprint(w, `{"value":[{"id":"drive1","name":"Docs"}]}`)
		case strings.HasPrefix(r.URL.Path, "/sites/"):
			fmt.Fprint(w, `{"id":"site1"}`)
		case r.URL.Path == "/subscriptions" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"value": existing})
		case r.URL.Path == "/subscriptions" && r.Method == http.MethodPost:
			created = true
			fmt.Fprint(w, `{"id":"sub1","resource":"sites/site1/drives/drive1/root","clientState":"cs"}`)
		case strings.HasPrefix(r.URL.Path, "/subscriptions/") && r.Method == http.MethodPatch:
			renewed = true
			fmt.Fprintf(w, `{"id":%q}`, strings.TrimPrefix(r.URL.Path, "/subscriptions/"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient("cid", "tid", "sec", "https://contoso.sharepoint.com/sites/Test")
	c.loginBase = srv.URL
	c.graphBase = srv.URL

	// No existing subscription -> create.
	sub, err := c.EnsureSubscription("https://x/sync/webhook", "cs", SubMinutesDefault)
	if err != nil {
		t.Fatalf("EnsureSubscription (create): %v", err)
	}
	if !created || sub.ID != "sub1" {
		t.Errorf("expected create; created=%v sub=%+v", created, sub)
	}

	// A matching subscription exists -> renew, not create.
	existing = []map[string]any{{"id": "sub1", "resource": "sites/site1/drives/drive1/root", "clientState": "cs"}}
	created = false
	if _, err := c.EnsureSubscription("https://x/sync/webhook", "cs", SubMinutesDefault); err != nil {
		t.Fatalf("EnsureSubscription (renew): %v", err)
	}
	if created {
		t.Error("created a new subscription when a matching one already existed")
	}
	if !renewed {
		t.Error("expected a renew (PATCH)")
	}
}
