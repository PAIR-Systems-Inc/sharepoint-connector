package sharepoint

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/fakes"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// TestAdapterSourceMethods drives the SharePoint Adapter's source.Source methods
// directly against the in-process fake Graph server and asserts each one produces
// the correct neutral values — in particular that the rich Graph metadata (name,
// modified_datetime) is carried through unchanged into what gets stored on the
// Goodmem memory, and that download/cursor/get/delta all round-trip.
func TestAdapterSourceMethods(t *testing.T) {
	fg := fakes.NewGraph()
	srv := httptest.NewServer(fg.Handler())
	defer srv.Close()
	fg.SetBase(srv.URL)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-05-01T00:00:00Z", Content: "HELLO"})

	c := NewClient("cid", "tid", "sec", "https://contoso.sharepoint.com/sites/Test", WithBaseURLs(srv.URL, srv.URL))
	a := NewAdapter(c, "", "secret")
	ctx := context.Background()

	// Label.
	if a.Label() != "sharepoint" {
		t.Errorf("Label = %q, want sharepoint", a.Label())
	}

	// ListFiles → neutral FileInfo with metadata carried through.
	files, err := a.ListFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("ListFiles = %d files, want 1", len(files))
	}
	f := files[0]
	if f.ID != "a" || f.Name != "a.pdf" || f.MimeType != "application/pdf" || f.ModifiedDateTime != "2026-05-01T00:00:00Z" {
		t.Errorf("neutral file wrong: %+v", f)
	}
	if f.DownloadRef == "" {
		t.Error("DownloadRef should be set from the Graph download URL")
	}
	if f.Size != int64(len("HELLO")) {
		t.Errorf("Size = %d, want %d", f.Size, len("HELLO"))
	}
	// The metadata stored on the memory must preserve the Graph fields the diff and
	// operators rely on — especially modified_datetime (the diff reads it back).
	if f.Metadata["name"] != "a.pdf" || f.Metadata["modified_datetime"] != "2026-05-01T00:00:00Z" {
		t.Errorf("metadata not carried through: %+v", f.Metadata)
	}

	// Open → the file's bytes (download resolution).
	rc, err := a.Open(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "HELLO" {
		t.Errorf("Open content = %q, want HELLO", b)
	}

	// LatestCursor → a non-empty bootstrap cursor.
	if cur, err := a.LatestCursor(ctx); err != nil || cur == "" {
		t.Errorf("LatestCursor = %q, %v; want non-empty", cur, err)
	}

	// GetFile(existing) and GetFile(missing → ErrNotFound).
	if got, err := a.GetFile(ctx, "a"); err != nil || got == nil || got.ID != "a" {
		t.Fatalf("GetFile(a) = %+v, %v", got, err)
	}
	if _, err := a.GetFile(ctx, "does-not-exist"); !errors.Is(err, source.ErrNotFound) {
		t.Errorf("GetFile(missing) err = %v, want source.ErrNotFound", err)
	}

	// Delta → one add Change (new file) and one delete Change.
	fg.Put(fakes.File{ID: "b", Name: "b.pdf", Mime: "application/pdf", Modified: "2026-06-01T00:00:00Z", Content: "B"})
	fg.SetDeltas(fakes.Delta{ID: "b"}, fakes.Delta{ID: "a", Deleted: true})
	changes, next, err := a.Delta(ctx, fg.DeltaLink())
	if err != nil {
		t.Fatal(err)
	}
	if next == "" {
		t.Error("Delta next cursor is empty")
	}
	var adds, dels int
	for _, ch := range changes {
		switch {
		case ch.Deleted:
			dels++
			if ch.ID != "a" {
				t.Errorf("unexpected delete id %q", ch.ID)
			}
		case ch.IsFile:
			adds++
			if ch.File.ID != "b" || ch.File.DownloadRef == "" {
				t.Errorf("add change wrong: %+v", ch.File)
			}
		}
	}
	if adds != 1 || dels != 1 {
		t.Errorf("delta changes: adds=%d dels=%d, want 1/1", adds, dels)
	}
}

// TestAdapterDeltaCursorExpired: a 410 Gone from the Graph delta surfaces as the
// neutral source.ErrCursorExpired so the engine falls back to a full sync.
func TestAdapterDeltaCursorExpired(t *testing.T) {
	fg := fakes.NewGraph()
	srv := httptest.NewServer(fg.Handler())
	defer srv.Close()
	fg.SetBase(srv.URL)
	fg.ExpireDelta() // next delta call returns 410 Gone

	c := NewClient("cid", "tid", "sec", "https://contoso.sharepoint.com/sites/Test", WithBaseURLs(srv.URL, srv.URL))
	a := NewAdapter(c, "", "secret")

	_, _, err := a.Delta(context.Background(), fg.DeltaLink()) // the fake serves this URL a 410
	if !errors.Is(err, source.ErrCursorExpired) {
		t.Errorf("Delta on 410 = %v, want source.ErrCursorExpired", err)
	}
}
