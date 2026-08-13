package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/fakes"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// enrichServer stands in for a customer's extraction service: it records what it
// was sent and replies with whatever metadata the test wants.
func enrichServer(t *testing.T, reply map[string]any, status int) (*httptest.Server, *EnrichContext, *[]byte) {
	t.Helper()
	var gotCtx EnrichContext
	var gotBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.Unmarshal([]byte(r.FormValue("context")), &gotCtx)
		if fh, _, err := r.FormFile("file"); err == nil {
			gotBytes, _ = io.ReadAll(fh)
			fh.Close()
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":"boom"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": reply})
	}))
	t.Cleanup(srv.Close)
	return srv, &gotCtx, &gotBytes
}

func TestHTTPEnricher_RequestAndResponse(t *testing.T) {
	want := map[string]any{"header": map[string]any{"customer_name": "Innotron"}}
	srv, gotCtx, gotBytes := enrichServer(t, want, http.StatusOK)

	content := []byte("the form bytes")
	f := source.FileInfo{
		ID: "f1", Name: "OSF.xlsx", RelativePath: "Orders/OSF.xlsx",
		MimeType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		Size:     int64(len(content)), ModifiedDateTime: "2026-01-01T00:00:00Z",
		Metadata: map[string]any{"source": "smb"},
	}
	md, err := HTTPEnricher(srv.URL, 10*time.Second)(context.Background(), f, content)
	if err != nil {
		t.Fatal(err)
	}

	if string(*gotBytes) != string(content) {
		t.Errorf("service received %q, want %q", *gotBytes, content)
	}
	sum := sha256.Sum256(content)
	if gotCtx.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want the digest of the posted bytes", gotCtx.SHA256)
	}
	if gotCtx.FileID != "f1" || gotCtx.Path != "Orders/OSF.xlsx" || gotCtx.Name != "OSF.xlsx" {
		t.Errorf("context identity wrong: %+v", *gotCtx)
	}
	if gotCtx.Metadata["source"] != "smb" {
		t.Errorf("provider metadata not forwarded: %+v", gotCtx.Metadata)
	}
	if h, ok := md["header"].(map[string]any); !ok || h["customer_name"] != "Innotron" {
		t.Errorf("nested metadata did not survive the round trip: %+v", md)
	}
}

func TestHTTPEnricher_NonOKIsAnError(t *testing.T) {
	srv, _, _ := enrichServer(t, nil, http.StatusInternalServerError)
	_, err := HTTPEnricher(srv.URL, 10*time.Second)(context.Background(),
		source.FileInfo{Name: "x.txt", MimeType: "text/plain"}, []byte("x"))
	if err == nil {
		t.Fatal("a 500 from the enrichment service must be an error, not empty metadata")
	}
}

// TestIntegration_EnrichmentMergedIntoMemory drives the real engine end to end
// and asserts the memory is BORN with the extra fields — the whole point of the
// seam, since Goodmem has no UpdateMemory to add them later.
func TestIntegration_EnrichmentMergedIntoMemory(t *testing.T) {
	fg, fm, gc, gmc, _ := newHarness(t)
	s := src(gc)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})

	enrich := func(_ context.Context, f source.FileInfo, content []byte) (map[string]any, error) {
		return map[string]any{
			"country": "Japan",
			"bytes":   len(content),
			// Engine-owned: must be dropped, not merged. An extractor that could
			// set this would corrupt the diff (a "newer" stored timestamp makes
			// the engine skip real updates forever).
			"modified_datetime": "1999-01-01T00:00:00Z",
		}, nil
	}

	res, err := RunFull(context.Background(), s, gmc, spaceID, Options{
		Enrich: enrich, EnrichRequired: true, EnrichVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 {
		t.Fatalf("added=%d, want 1 (errors: %v)", res.Added, res.Errors)
	}

	md := fm.Metadata(mid("a"))
	if md["country"] != "Japan" {
		t.Errorf("enrichment field missing: %+v", md)
	}
	if md["enrich_version"] != "v1" {
		t.Errorf("enrich_version = %v, want v1", md["enrich_version"])
	}
	if md["modified_datetime"] != "2026-01-01T00:00:00Z" {
		t.Errorf("enricher overwrote a reserved key: modified_datetime = %v", md["modified_datetime"])
	}
	if md["name"] == nil {
		t.Errorf("provider metadata lost: %+v", md)
	}
}

// TestIntegration_EnrichFailure covers both halves of the ENRICH_REQUIRED
// choice, which is the difference between a loud failure and a silent hole: an
// un-enriched memory still retrieves semantically, so nothing looks broken while
// every metadata filter over it misses.
func TestIntegration_EnrichFailure(t *testing.T) {
	boom := func(context.Context, source.FileInfo, []byte) (map[string]any, error) {
		return nil, fmt.Errorf("extractor unavailable")
	}

	t.Run("required: not ingested", func(t *testing.T) {
		fg, fm, gc, gmc, _ := newHarness(t)
		fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})
		res, err := RunFull(context.Background(), src(gc), gmc, spaceID, Options{
			Enrich: boom, EnrichRequired: true, EnrichVersion: "v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Added != 0 || len(res.Errors) == 0 {
			t.Fatalf("added=%d errors=%v, want 0 added and a reported error", res.Added, res.Errors)
		}
		if fm.Has(mid("a")) {
			t.Error("a file whose required enrichment failed must not be ingested")
		}
	})

	t.Run("best effort: ingested bare, and retried next sync", func(t *testing.T) {
		fg, fm, gc, gmc, _ := newHarness(t)
		fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})
		opts := Options{Enrich: boom, EnrichRequired: false, EnrichVersion: "v1"}
		res, err := RunFull(context.Background(), src(gc), gmc, spaceID, opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Added != 1 || len(res.Errors) == 0 {
			t.Fatalf("added=%d errors=%v, want 1 added and a reported error", res.Added, res.Errors)
		}
		if v := fm.Metadata(mid("a"))["enrich_version"]; v != nil {
			t.Errorf("enrich_version = %v on a FAILED enrichment; stamping it would mark a bare memory as done", v)
		}
		// Because the version was not stamped, the next full sync sees the
		// mismatch and tries again — self-healing rather than silently stuck.
		res, err = RunFull(context.Background(), src(gc), gmc, spaceID, opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.Updated != 1 {
			t.Errorf("updated=%d, want 1: an un-enriched memory must be retried", res.Updated)
		}
	})
}
