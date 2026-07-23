package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// --- fake Google Drive server ---

type fakeDrive struct {
	files   map[string]*DriveFile
	content map[string]string
	changes []DriveChange
	gone    bool // next /changes returns 410
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{files: map[string]*DriveFile{}, content: map[string]string{}}
}

func (f *fakeDrive) put(df DriveFile, content string) {
	c := df
	f.files[df.ID] = &c
	f.content[df.ID] = content
}

func fileJSON(df *DriveFile) map[string]any {
	return map[string]any{
		"id": df.ID, "name": df.Name, "mimeType": df.MimeType,
		"modifiedTime": df.ModifiedTime, "size": strconv.FormatInt(df.Size, 10),
		"md5Checksum": df.MD5, "trashed": df.Trashed,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":{"code":%d,"message":%q}}`, code, msg)
}

func (f *fakeDrive) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/changes/startPageToken":
			writeJSON(w, map[string]any{"startPageToken": "start-1"})
		case p == "/changes/watch":
			var ch struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&ch)
			writeJSON(w, map[string]any{"id": ch.ID, "resourceId": "res-" + ch.ID, "expiration": "9999999999999"})
		case p == "/channels/stop":
			w.WriteHeader(http.StatusNoContent)
		case p == "/changes":
			if f.gone {
				writeErr(w, http.StatusGone, "expired change token")
				return
			}
			var chs []map[string]any
			for _, ch := range f.changes {
				m := map[string]any{"fileId": ch.FileID, "removed": ch.Removed}
				if ch.File != nil {
					m["file"] = fileJSON(ch.File)
				} else {
					m["file"] = nil
				}
				chs = append(chs, m)
			}
			writeJSON(w, map[string]any{"changes": chs, "newStartPageToken": "next-1"})
		case strings.HasSuffix(p, "/export"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/files/"), "/export")
			io.WriteString(w, "EXPORT:"+f.content[id]) // fake export marker
		case strings.HasPrefix(p, "/files/"):
			id := strings.TrimPrefix(p, "/files/")
			df := f.files[id]
			if df == nil {
				writeErr(w, http.StatusNotFound, "file not found")
				return
			}
			if r.URL.Query().Get("alt") == "media" {
				io.WriteString(w, f.content[id])
				return
			}
			writeJSON(w, fileJSON(df))
		case p == "/files":
			var list []map[string]any
			for _, df := range f.files {
				list = append(list, fileJSON(df))
			}
			writeJSON(w, map[string]any{"files": list})
		default:
			writeErr(w, http.StatusNotFound, "unhandled "+p)
		}
	})
}

func newTestClient(t *testing.T, fd *fakeDrive) *Client {
	t.Helper()
	srv := httptest.NewServer(fd.handler())
	t.Cleanup(srv.Close)
	c, err := New(context.Background(), "drive-1",
		option.WithEndpoint(srv.URL+"/"),
		option.WithHTTPClient(srv.Client()),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// --- tests ---

func TestAdapterSourceMethods(t *testing.T) {
	fd := newFakeDrive()
	fd.put(DriveFile{ID: "bin", Name: "a.pdf", MimeType: "application/pdf", ModifiedTime: "2026-05-01T00:00:00.000Z", Size: 5, MD5: "abc"}, "HELLO")
	fd.put(DriveFile{ID: "doc", Name: "Notes", MimeType: "application/vnd.google-apps.document", ModifiedTime: "2026-06-01T00:00:00.000Z"}, "DOCBODY")

	a := NewAdapter(newTestClient(t, fd), "chan-secret")
	ctx := context.Background()

	if a.Label() != "gdrive" {
		t.Errorf("Label = %q, want gdrive", a.Label())
	}

	// ListFiles → neutral files; native Google Doc reports its export target as the
	// effective MimeType, with the original Google mime kept in DownloadRef.
	files, err := a.ListFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]source.FileInfo{}
	for _, f := range files {
		byID[f.ID] = f
	}
	if len(files) != 2 {
		t.Fatalf("ListFiles = %d, want 2", len(files))
	}
	bin := byID["bin"]
	if bin.MimeType != "application/pdf" || bin.DownloadRef != "application/pdf" || bin.Metadata["name"] != "a.pdf" {
		t.Errorf("binary file wrong: %+v", bin)
	}
	doc := byID["doc"]
	if doc.MimeType != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Errorf("native doc effective mime = %q, want docx export target", doc.MimeType)
	}
	if doc.DownloadRef != "application/vnd.google-apps.document" {
		t.Errorf("native doc DownloadRef = %q, want the original google mime", doc.DownloadRef)
	}
	if doc.Metadata["google_mime"] != "application/vnd.google-apps.document" {
		t.Errorf("native doc metadata missing google_mime: %+v", doc.Metadata)
	}

	// Open: binary → alt=media bytes; native doc → export bytes.
	if b := mustRead(t, a, ctx, bin); b != "HELLO" {
		t.Errorf("Open(binary) = %q, want HELLO", b)
	}
	if b := mustRead(t, a, ctx, doc); b != "EXPORT:DOCBODY" {
		t.Errorf("Open(native) = %q, want exported bytes", b)
	}

	// LatestCursor / GetFile.
	if cur, err := a.LatestCursor(ctx); err != nil || cur != "start-1" {
		t.Errorf("LatestCursor = %q, %v", cur, err)
	}
	if got, err := a.GetFile(ctx, "bin"); err != nil || got == nil || got.ID != "bin" {
		t.Fatalf("GetFile(bin) = %+v, %v", got, err)
	}
	if _, err := a.GetFile(ctx, "missing"); !errors.Is(err, source.ErrNotFound) {
		t.Errorf("GetFile(missing) = %v, want source.ErrNotFound", err)
	}

	// Delta → one add change (bin) and one delete change (removed).
	fd.changes = []DriveChange{
		{FileID: "bin", File: fd.files["bin"]},
		{FileID: "ghost", Removed: true},
	}
	changes, next, err := a.Delta(ctx, "start-1")
	if err != nil {
		t.Fatal(err)
	}
	if next != "next-1" {
		t.Errorf("Delta next = %q, want next-1", next)
	}
	var adds, dels int
	for _, ch := range changes {
		switch {
		case ch.Deleted:
			dels++
		case ch.IsFile:
			adds++
		}
	}
	if adds != 1 || dels != 1 {
		t.Errorf("delta: adds=%d dels=%d, want 1/1", adds, dels)
	}

	// EnsureSubscription creates a channel; a second call stops the old one (the
	// fake /channels/stop returns 204) without error.
	if sub, err := a.EnsureSubscription(ctx, "https://example.test/webhook", time.Hour); err != nil || sub.ID == "" {
		t.Fatalf("EnsureSubscription = %+v, %v", sub, err)
	}
	if _, err := a.EnsureSubscription(ctx, "https://example.test/webhook", time.Hour); err != nil {
		t.Fatalf("renewal EnsureSubscription: %v", err)
	}
}

func mustRead(t *testing.T, a *Adapter, ctx context.Context, f source.FileInfo) string {
	t.Helper()
	rc, err := a.Open(ctx, f)
	if err != nil {
		t.Fatalf("Open(%s): %v", f.ID, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestAdapterDeltaCursorExpired(t *testing.T) {
	fd := newFakeDrive()
	fd.gone = true
	a := NewAdapter(newTestClient(t, fd), "s")
	if _, _, err := a.Delta(context.Background(), "old"); !errors.Is(err, source.ErrCursorExpired) {
		t.Errorf("Delta on 410 = %v, want source.ErrCursorExpired", err)
	}
}

func TestAdapterValidateWebhook(t *testing.T) {
	a := NewAdapter(nil, "secret")

	newReq := func(token, state string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/webhook", nil)
		r.Header.Set("X-Goog-Channel-Token", token)
		r.Header.Set("X-Goog-Resource-State", state)
		return r
	}

	if res, _ := a.ValidateWebhook(newReq("nope", "change"), nil); res != source.WebhookReject {
		t.Errorf("bad token: got %v, want reject", res)
	}
	if res, _ := a.ValidateWebhook(newReq("secret", "sync"), nil); res != source.WebhookHandshake {
		t.Errorf("sync: got %v, want handshake", res)
	}
	if res, _ := a.ValidateWebhook(newReq("secret", "change"), nil); res != source.WebhookChange {
		t.Errorf("change: got %v, want change", res)
	}
}

func TestExportPolicy(t *testing.T) {
	cases := map[string]string{
		"application/vnd.google-apps.document":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.google-apps.spreadsheet":  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.google-apps.presentation": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.google-apps.form":         "", // no supported export
	}
	for mime, want := range cases {
		if !IsNativeDoc(mime) {
			t.Errorf("IsNativeDoc(%q) = false, want true", mime)
		}
		if got := ExportTarget(mime); got != want {
			t.Errorf("ExportTarget(%q) = %q, want %q", mime, got, want)
		}
	}
	if IsNativeDoc("application/pdf") {
		t.Error("IsNativeDoc(application/pdf) = true, want false")
	}
}
