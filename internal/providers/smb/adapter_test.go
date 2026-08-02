package smb

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sort"
	"testing"
	"testing/fstest"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

var (
	t1 = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	t3 = time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
)

// testShare is a share containing content plus the machine noise a real Windows
// share is full of.
func testShare() fstest.MapFS {
	return fstest.MapFS{
		"report.pdf":                      {Data: []byte("pdf bytes"), ModTime: t1},
		"notes.txt":                       {Data: []byte("some notes"), ModTime: t2},
		"sub/budget.xlsx":                 {Data: []byte("xlsx bytes"), ModTime: t3},
		"sub/nested/memo.docx":            {Data: []byte("docx bytes"), ModTime: t1},
		"~$report.pdf":                    {Data: []byte("office lock"), ModTime: t3},
		"Thumbs.db":                       {Data: []byte("thumbs"), ModTime: t3},
		".hidden.txt":                     {Data: []byte("dotfile"), ModTime: t3},
		"$RECYCLE.BIN/deleted.pdf":        {Data: []byte("trash"), ModTime: t3},
		"System Volume Information/x.txt": {Data: []byte("sys"), ModTime: t3},
		"image.png":                       {Data: []byte("png bytes"), ModTime: t1},
	}
}

func newTestAdapter(t *testing.T, fsys fs.FS, root string) *Adapter {
	t.Helper()
	return NewAdapter(newWithFS(Config{Host: "fs1", Share: "data", User: "u", Root: root}, fsys))
}

func paths(files []source.FileInfo) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.ID)
	}
	sort.Strings(out)
	return out
}

func TestListFiles_SkipsMachineNoise(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	files, err := a.ListFiles(context.Background())
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	got := paths(files)
	want := []string{"image.png", "notes.txt", "report.pdf", "sub/budget.xlsx", "sub/nested/memo.docx"}
	if len(got) != len(want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listed %v, want %v", got, want)
		}
	}
}

// The identity is the path relative to the sync root — not the share root — or
// changing SMB_ROOT would silently re-key every memory.
func TestListFiles_RootScoping(t *testing.T) {
	a := newTestAdapter(t, testShare(), "sub")
	files, err := a.ListFiles(context.Background())
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	got := paths(files)
	want := []string{"budget.xlsx", "nested/memo.docx"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("listed %v, want %v", got, want)
	}
}

func TestListFiles_MetadataAndMime(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	files, err := a.ListFiles(context.Background())
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	var pdf source.FileInfo
	for _, f := range files {
		if f.ID == "report.pdf" {
			pdf = f
		}
	}
	if pdf.ID == "" {
		t.Fatal("report.pdf missing from listing")
	}
	if pdf.MimeType != "application/pdf" {
		t.Errorf("MimeType = %q, want application/pdf", pdf.MimeType)
	}
	if pdf.Size != int64(len("pdf bytes")) {
		t.Errorf("Size = %d, want %d", pdf.Size, len("pdf bytes"))
	}
	if pdf.ModifiedDateTime != t1.Format(time.RFC3339Nano) {
		t.Errorf("ModifiedDateTime = %q, want %q", pdf.ModifiedDateTime, t1.Format(time.RFC3339Nano))
	}
	if pdf.Metadata["source"] != "smb" {
		t.Errorf("metadata source = %v, want smb", pdf.Metadata["source"])
	}
	if pdf.Metadata["smb_share"] != "data" {
		t.Errorf("metadata smb_share = %v, want data", pdf.Metadata["smb_share"])
	}
}

func TestLatestCursor_IsNewestModTimeNotWallClock(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	cur, err := a.LatestCursor(context.Background())
	if err != nil {
		t.Fatalf("LatestCursor: %v", err)
	}
	// The watermark comes from the newest *syncable* file (sub/budget.xlsx at t3).
	// The skipped noise files also carry t3, so this additionally pins that the
	// walk callback — not the raw directory listing — is what feeds the cursor.
	if cur != t3.Format(time.RFC3339Nano) {
		t.Errorf("cursor = %q, want %q", cur, t3.Format(time.RFC3339Nano))
	}
}

func TestLatestCursor_EmptyShare(t *testing.T) {
	a := newTestAdapter(t, fstest.MapFS{}, "")
	cur, err := a.LatestCursor(context.Background())
	if err != nil {
		t.Fatalf("LatestCursor: %v", err)
	}
	if cur != "" {
		t.Errorf("cursor = %q, want empty for an empty share", cur)
	}
}

func TestDelta_OnlyFilesNewerThanCursor(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")

	// Watermark at t1: only the t2 and t3 files are changes.
	changes, next, err := a.Delta(context.Background(), t1.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	got := make([]string, 0, len(changes))
	for _, c := range changes {
		if !c.IsFile || c.Deleted {
			t.Errorf("change %+v: want a non-deleted file change", c)
		}
		got = append(got, c.ID)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "notes.txt" || got[1] != "sub/budget.xlsx" {
		t.Errorf("changes = %v, want [notes.txt sub/budget.xlsx]", got)
	}
	if next != t3.Format(time.RFC3339Nano) {
		t.Errorf("next cursor = %q, want %q", next, t3.Format(time.RFC3339Nano))
	}
}

func TestDelta_EmptyCursorReturnsEverything(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	changes, _, err := a.Delta(context.Background(), "")
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if len(changes) != 5 {
		t.Errorf("changes = %d, want all 5 syncable files", len(changes))
	}
}

func TestDelta_NoChangesKeepsCursorStable(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	cur := t3.Format(time.RFC3339Nano)
	changes, next, err := a.Delta(context.Background(), cur)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %v, want none", changes)
	}
	if next != cur {
		t.Errorf("next cursor = %q, want it unchanged at %q", next, cur)
	}
}

// Deleting the newest file must not rewind the watermark — that would re-emit
// every file newer than the new maximum on the following cycle, forever.
func TestDelta_WatermarkNeverGoesBackwards(t *testing.T) {
	share := testShare()
	delete(share, "sub/budget.xlsx") // the only syncable t3 file
	a := newTestAdapter(t, share, "")

	cur := t3.Format(time.RFC3339Nano)
	_, next, err := a.Delta(context.Background(), cur)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if next != cur {
		t.Errorf("next cursor = %q, want it held at %q despite the newest file being deleted", next, cur)
	}
}

func TestDelta_InvalidCursorAsksForFullResync(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	_, _, err := a.Delta(context.Background(), "not-a-timestamp")
	if !errors.Is(err, source.ErrCursorExpired) {
		t.Errorf("err = %v, want source.ErrCursorExpired", err)
	}
}

func TestGetFile(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")

	fi, err := a.GetFile(context.Background(), "sub/budget.xlsx")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if fi == nil || fi.ID != "sub/budget.xlsx" {
		t.Fatalf("GetFile = %+v, want sub/budget.xlsx", fi)
	}
	if fi.MimeType != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Errorf("MimeType = %q", fi.MimeType)
	}

	if _, err := a.GetFile(context.Background(), "nope/missing.pdf"); !errors.Is(err, source.ErrNotFound) {
		t.Errorf("missing file err = %v, want source.ErrNotFound", err)
	}

	// A directory is "not a file", signalled as (nil, nil) like the other providers.
	fi, err = a.GetFile(context.Background(), "sub")
	if err != nil || fi != nil {
		t.Errorf("GetFile(dir) = (%+v, %v), want (nil, nil)", fi, err)
	}
}

func TestOpen(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	rc, err := a.Open(context.Background(), source.FileInfo{ID: "notes.txt"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "some notes" {
		t.Errorf("content = %q, want %q", b, "some notes")
	}

	if _, err := a.Open(context.Background(), source.FileInfo{ID: "gone.txt"}); !errors.Is(err, source.ErrNotFound) {
		t.Errorf("missing file err = %v, want source.ErrNotFound", err)
	}
}

func TestOpen_RespectsRoot(t *testing.T) {
	a := newTestAdapter(t, testShare(), "sub")
	rc, err := a.Open(context.Background(), source.FileInfo{ID: "budget.xlsx"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "xlsx bytes" {
		t.Errorf("content = %q, want %q", b, "xlsx bytes")
	}
}

// Push is structurally unavailable for SMB; the failure must be explicit and
// tell the operator what to do instead.
func TestEnsureSubscription_NotSupported(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	_, err := a.EnsureSubscription(context.Background(), "https://example.com/hook", time.Hour)
	if err == nil {
		t.Fatal("want an error; SMB has no push mechanism")
	}
	if got := err.Error(); !contains(got, "poll") {
		t.Errorf("error %q should point the operator at poll mode", got)
	}
}

func TestValidateWebhook_AlwaysRejects(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	res, echo := a.ValidateWebhook(nil, nil)
	if res != source.WebhookReject || echo != "" {
		t.Errorf("ValidateWebhook = (%v, %q), want (reject, \"\")", res, echo)
	}
}

func TestMemNamespaceIsStable(t *testing.T) {
	a := newTestAdapter(t, testShare(), "")
	// This value is permanent: changing it re-keys every memory ever written.
	if got := a.MemNamespace(); got != "smb.file.path:fs1/data/" {
		t.Errorf("MemNamespace = %q, want smb.file.path:fs1/data/", got)
	}
	if got := a.Label(); got != "smb" {
		t.Errorf("Label = %q, want smb", got)
	}
}

// Memory ids are global in Goodmem, and an SMB identity is only a path relative
// to the sync root — which is not unique across servers. Without the share in
// the namespace, two shares that each contain notes.txt mint the same id and the
// second is rejected with a 409. Found the hard way against a real share.
func TestMemNamespace_DistinguishesShares(t *testing.T) {
	ns := func(host, share, root string) string {
		return NewAdapter(newWithFS(Config{Host: host, Share: share, User: "u", Root: root}, testShare())).MemNamespace()
	}

	distinct := map[string]string{
		"different host":  ns("fs2", "data", ""),
		"different share": ns("fs1", "other", ""),
		"different root":  ns("fs1", "data", "sub"),
	}
	base := ns("fs1", "data", "")
	for name, got := range distinct {
		if got == base {
			t.Errorf("%s: namespace %q collides with the base share", name, got)
		}
	}

	// Addressing the same share differently must NOT re-key it.
	for _, equivalent := range []string{
		ns("FS1", "data", ""),     // host case
		ns("fs1:445", "data", ""), // explicit default port
		ns("fs1", "DATA", ""),     // share case
		ns("fs1", "data", "/"),    // empty root spelled differently
	} {
		if equivalent != base {
			t.Errorf("namespace %q should equal the base %q — the same share addressed differently", equivalent, base)
		}
	}
}

// SMB_NAMESPACE exists so an operator can pin identity when the way they address
// the server may change (an IP today, an FQDN later).
func TestMemNamespace_ExplicitOverride(t *testing.T) {
	a := NewAdapter(newWithFS(Config{
		Host: "10.0.0.5", Share: "data", User: "u", Namespace: "fileserver/Shared",
	}, testShare()))
	if got := a.MemNamespace(); got != "smb.file.path:fileserver/Shared" {
		t.Errorf("MemNamespace = %q, want the pinned value", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
