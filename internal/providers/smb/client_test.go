package smb

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{Host: "fs1", Share: "data", User: "u", Password: "p"}, false},
		{"no host", Config{Share: "data", User: "u"}, true},
		{"no share", Config{Host: "fs1", User: "u"}, true},
		{"no user", Config{Host: "fs1", Share: "data"}, true},
		{"blank host", Config{Host: "   ", Share: "data", User: "u"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestConfigAddress(t *testing.T) {
	cases := map[string]string{
		"fs1":            "fs1:445",
		"fs1:445":        "fs1:445",
		"fs1:1445":       "fs1:1445",
		"192.168.1.10":   "192.168.1.10:445",
		"127.0.0.1:1445": "127.0.0.1:1445",
	}
	for in, want := range cases {
		if got := (Config{Host: in}).address(); got != want {
			t.Errorf("address(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMimeTypeForName(t *testing.T) {
	cases := map[string]string{
		"a.pdf":       "application/pdf",
		"a.PDF":       "application/pdf", // extension match is case-insensitive
		"a.docx":      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"a.xlsx":      "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"a.txt":       "text/plain",
		"a.md":        "text/markdown",
		"a.csv":       "text/csv",
		"a.json":      "application/json",
		"deck.pptx":   "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"archive.zip": "application/zip",
		"noext":       "application/octet-stream",
		"a.unknownxx": "application/octet-stream",
	}
	for name, want := range cases {
		got := MimeTypeForName(name)
		if name == "archive.zip" {
			// Provided by Go's built-in table on some systems and /etc/mime.types
			// on others; only assert it is not silently empty.
			if got == "" {
				t.Errorf("MimeTypeForName(%q) is empty", name)
			}
			continue
		}
		if got != want {
			t.Errorf("MimeTypeForName(%q) = %q, want %q", name, got, want)
		}
	}
}

// The container the connector ships in has no /etc/mime.types, so the office
// and text extensions must resolve from the package's own table rather than
// mime.TypeByExtension — otherwise every document is filtered out as
// unsupported at runtime while tests on a dev box pass.
func TestMimeTypeForName_DoesNotDependOnSystemMimeTypes(t *testing.T) {
	for _, ext := range []string{".pdf", ".docx", ".xlsx", ".pptx", ".doc", ".txt", ".md", ".csv", ".rtf"} {
		if _, ok := extMimeTypes[ext]; !ok {
			t.Errorf("extension %q is not in the built-in table; it would resolve to \"\" in the scratch container", ext)
		}
	}
}

func TestSkipFile(t *testing.T) {
	skip := []string{"~$report.docx", "Thumbs.db", "thumbs.db", "desktop.ini", ".DS_Store", ".hidden", ".~lock.doc.odt#"}
	keep := []string{"report.pdf", "notes.txt", "a~b.txt", "budget v2.xlsx", "Report~$.pdf"}
	for _, n := range skip {
		if !skipFile(n) {
			t.Errorf("skipFile(%q) = false, want true", n)
		}
	}
	for _, n := range keep {
		if skipFile(n) {
			t.Errorf("skipFile(%q) = true, want false", n)
		}
	}
}

func TestSkipDir(t *testing.T) {
	skip := []string{"$RECYCLE.BIN", "$Recycle.Bin", "System Volume Information", ".snapshot", ".git"}
	keep := []string{"Documents", "sub", "Reports 2026"}
	for _, n := range skip {
		if !skipDir(n) {
			t.Errorf("skipDir(%q) = false, want true", n)
		}
	}
	for _, n := range keep {
		if skipDir(n) {
			t.Errorf("skipDir(%q) = true, want false", n)
		}
	}
}

func TestPathHelpers(t *testing.T) {
	t.Run("walkRoot", func(t *testing.T) {
		cases := map[string]string{
			"":            ".",
			"/":           ".",
			"sub":         "sub",
			"/sub/":       "sub",
			`sub\nested`:  "sub/nested", // operators write Windows separators
			"sub//nested": "sub/nested",
		}
		for in, want := range cases {
			if got := walkRoot(in); got != want {
				t.Errorf("walkRoot(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("joinRoot", func(t *testing.T) {
		cases := []struct{ root, rel, want string }{
			{"", "a.txt", "a.txt"},
			{"", "sub/a.txt", "sub/a.txt"},
			{"", "", "."},
			{"sub", "a.txt", "sub/a.txt"},
			{"sub", "nested/a.txt", "sub/nested/a.txt"},
			{"sub", "", "sub"},
			{`sub\nested`, "a.txt", "sub/nested/a.txt"},
		}
		for _, c := range cases {
			if got := joinRoot(c.root, c.rel); got != c.want {
				t.Errorf("joinRoot(%q, %q) = %q, want %q", c.root, c.rel, got, c.want)
			}
		}
	})

	t.Run("relativeTo", func(t *testing.T) {
		cases := []struct{ root, p, want string }{
			{".", "a.txt", "a.txt"},
			{".", ".", ""},
			{"sub", "sub", ""},
			{"sub", "sub/a.txt", "a.txt"},
			{"sub", "sub/nested/a.txt", "nested/a.txt"},
		}
		for _, c := range cases {
			if got := relativeTo(c.root, c.p); got != c.want {
				t.Errorf("relativeTo(%q, %q) = %q, want %q", c.root, c.p, got, c.want)
			}
		}
	})
}

// An unreadable subtree must not fail the whole walk: real shares contain
// directories the connector's account cannot enter, and one of them must not
// stop everything else from syncing.
func TestWalk_SkipsUnreadableSubtreeButKeepsGoing(t *testing.T) {
	base := fstest.MapFS{
		"ok/a.txt":     {Data: []byte("a"), ModTime: t1},
		"denied/b.txt": {Data: []byte("b"), ModTime: t1},
		"ok2/c.txt":    {Data: []byte("c"), ModTime: t1},
	}
	c := newWithFS(Config{Host: "h", Share: "s", User: "u"}, denyFS{FS: base, deny: "denied"})

	var got []string
	if err := c.Walk(context.Background(), func(f File) error {
		got = append(got, f.Path)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("walked %v, want the two readable files", got)
	}
}

// Failing to read the sync root itself is different: continuing would report an
// empty share, and an empty listing is what the mass-delete guard exists to
// catch. It must surface as an error instead.
func TestWalk_RootFailureIsFatal(t *testing.T) {
	c := newWithFS(Config{Host: "h", Share: "s", User: "u", Root: "missing"}, fstest.MapFS{
		"other/a.txt": {Data: []byte("a"), ModTime: t1},
	})
	err := c.Walk(context.Background(), func(File) error { return nil })
	if err == nil {
		t.Fatal("want an error when the sync root cannot be read")
	}
}

func TestWalk_ContextCancellation(t *testing.T) {
	c := newWithFS(Config{Host: "h", Share: "s", User: "u"}, testShare())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Walk(ctx, func(File) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestIsConnectionError(t *testing.T) {
	if isConnectionError(fs.ErrNotExist) {
		t.Error("a missing file must not invalidate the session")
	}
	if isConnectionError(fs.ErrPermission) {
		t.Error("a permission denial must not invalidate the session")
	}
	if isConnectionError(nil) {
		t.Error("nil is not a connection error")
	}
	if !isConnectionError(errTimeout{}) {
		t.Error("a net.Error should invalidate the session")
	}
}

type errTimeout struct{}

func (errTimeout) Error() string   { return "i/o timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

// denyFS returns a permission error for one subtree.
type denyFS struct {
	fs.FS
	deny string
}

func (d denyFS) Open(name string) (fs.File, error) {
	if len(name) >= len(d.deny) && name[:len(d.deny)] == d.deny {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return d.FS.Open(name)
}
