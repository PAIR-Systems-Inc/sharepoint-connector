package graph

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSiteURL(t *testing.T) {
	cases := []struct{ in, host, path string }{
		{"https://contoso.sharepoint.com/sites/Test", "contoso.sharepoint.com", "/sites/Test"},
		{"https://contoso.sharepoint.com", "contoso.sharepoint.com", "/"},
		{"http://host/a/b/c", "host", "/a/b/c"},
	}
	for _, tc := range cases {
		h, p := parseSiteURL(tc.in)
		if h != tc.host || p != tc.path {
			t.Errorf("parseSiteURL(%q) = (%q, %q), want (%q, %q)", tc.in, h, p, tc.host, tc.path)
		}
	}
}

func TestFormatFileInfo(t *testing.T) {
	raw := `{
		"id":"01ABC","name":"report.pdf","webUrl":"https://x/report.pdf",
		"@microsoft.graph.downloadUrl":"https://dl/report.pdf","size":12345,
		"createdDateTime":"2026-01-01T00:00:00Z","lastModifiedDateTime":"2026-02-01T00:00:00Z",
		"createdBy":{"user":{"displayName":"Alice"}},
		"lastModifiedBy":{"user":{"displayName":"Bob"}},
		"file":{"mimeType":"application/pdf","hashes":{"sha1Hash":"DEADBEEF"}}
	}`
	var it graphItem
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		t.Fatal(err)
	}
	got := formatFileInfo(it)
	want := FileInfo{
		ID: "01ABC", Name: "report.pdf", WebURL: "https://x/report.pdf",
		DownloadURL: "https://dl/report.pdf", Size: 12345,
		CreatedDateTime: "2026-01-01T00:00:00Z", ModifiedDateTime: "2026-02-01T00:00:00Z",
		CreatedBy: "Alice", ModifiedBy: "Bob",
		MimeType: "application/pdf", FileHash: "DEADBEEF",
	}
	if got != want {
		t.Errorf("formatFileInfo:\n got  %+v\n want %+v", got, want)
	}
}

// TestGetSiteID_401Retry verifies the client-credentials flow and the
// re-authenticate-once-on-401 retry (ported from _request in the Python).
// TestListFilesFolderErrorPropagates: a failure listing a subfolder must abort
// the whole listing (returning an error), not silently drop that subtree — which
// would later be diffed as orphaned memories and mass-deleted.
func TestListFilesFolderErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/oauth2/v2.0/token"):
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
		case strings.HasSuffix(p, "/root/children"):
			// One file at the root plus one subfolder to descend into.
			fmt.Fprint(w, `{"value":[
				{"id":"f1","name":"top.pdf","file":{"mimeType":"application/pdf"},"@microsoft.graph.downloadUrl":"http://x/f1"},
				{"id":"sub","name":"Sub","folder":{"childCount":3}}
			]}`)
		case strings.HasSuffix(p, "/items/sub/children"):
			// The subfolder listing fails transiently (e.g. a 429/5xx storm).
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"code":"serverError"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient("cid", "tid", "secret", "https://contoso.sharepoint.com/sites/Test",
		WithBaseURLs(srv.URL, srv.URL))
	c.maxRetries = 0 // fail fast, no backoff sleeps

	files, err := c.ListFiles("drive1", "", true, "site1")
	if err == nil {
		t.Fatalf("expected an error when a subfolder listing fails; got %d files and nil error", len(files))
	}
	if !strings.Contains(err.Error(), "Sub") {
		t.Errorf("error should name the failing folder, got: %v", err)
	}
}

func TestGetSiteID_401Retry(t *testing.T) {
	var tokenCalls, siteCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			tokenCalls++
			tok := "tok1"
			if tokenCalls >= 2 {
				tok = "tok2"
			}
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, tok)
		case strings.HasPrefix(r.URL.Path, "/sites/"):
			siteCalls++
			if r.Header.Get("Authorization") != "Bearer tok2" {
				w.WriteHeader(http.StatusUnauthorized) // force one re-auth
				return
			}
			fmt.Fprint(w, `{"id":"site-123"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient("cid", "test-tenant", "secret", "https://contoso.sharepoint.com/sites/Test")
	c.loginBase = srv.URL
	c.graphBase = srv.URL

	id, err := c.GetSiteID()
	if err != nil {
		t.Fatalf("GetSiteID: %v", err)
	}
	if id != "site-123" {
		t.Errorf("site id = %q, want site-123", id)
	}
	if tokenCalls != 2 {
		t.Errorf("token endpoint hit %d times, want 2 (initial + 401 re-auth)", tokenCalls)
	}
	if siteCalls != 2 {
		t.Errorf("site endpoint hit %d times, want 2 (401 then retry)", siteCalls)
	}
}
