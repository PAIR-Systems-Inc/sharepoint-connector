package smb

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// TestLive_SMBShare exercises the real SMB2/3 wire protocol — session setup,
// tree connect, directory enumeration and file reads — against an actual server.
// The unit tests substitute an in-memory fs.FS, which proves the provider's
// logic but not that the library talks to a server correctly; this closes that
// gap. It is skipped unless SMB_LIVE=1 and the connection vars are set:
//
//	SMB_LIVE=1 SMB_HOST=127.0.0.1:1445 SMB_SHARE=data SMB_USER=connector \
//	  SMB_PASSWORD=… go test ./internal/providers/smb -run TestLive -v
//
// A disposable server is enough — the repo's docker-compose.smb-test.yml starts
// a Samba container preloaded with fixtures. Read-only; makes no changes.
func TestLive_SMBShare(t *testing.T) {
	if os.Getenv("SMB_LIVE") != "1" {
		t.Skip("set SMB_LIVE=1 (and SMB_HOST/SMB_SHARE/SMB_USER/SMB_PASSWORD) to run the live SMB test")
	}
	cfg := Config{
		Host:     os.Getenv("SMB_HOST"),
		Share:    os.Getenv("SMB_SHARE"),
		User:     os.Getenv("SMB_USER"),
		Password: os.Getenv("SMB_PASSWORD"),
		Domain:   os.Getenv("SMB_DOMAIN"),
		Root:     os.Getenv("SMB_ROOT"),
	}
	if err := cfg.Validate(); err != nil {
		t.Skipf("incomplete SMB config: %v", err)
	}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a := NewAdapter(c)

	files, err := a.ListFiles(ctx)
	if err != nil {
		// Distinguish the two failures that look alike from here. Connecting and
		// enumerating are separate steps, and saying "authenticated, but…" when
		// the logon itself was rejected sends you looking at share permissions
		// when the problem is the credentials.
		if strings.Contains(err.Error(), "smb connect") || strings.Contains(err.Error(), "smb mount") {
			t.Fatalf("could not connect: %v\n"+
				"  → the session never came up: check SMB_HOST reachability, and that "+
				"SMB_USER/SMB_PASSWORD/SMB_DOMAIN are right (a local Windows account's "+
				"domain is the computer name)", err)
		}
		t.Fatalf("ListFiles: %v\n"+
			"  → connected, but enumeration failed: check that the account has read "+
			"access at BOTH the share and NTFS layers (the stricter wins), and that "+
			"SMB_ROOT exists", err)
	}
	if len(files) == 0 {
		t.Fatal("listed 0 files; the test share should contain fixtures")
	}
	// Log shapes, not names: a real share's filenames are customer data and this
	// may run in CI. Set SMB_LIVE_VERBOSE=1 locally to see them.
	verbose := os.Getenv("SMB_LIVE_VERBOSE") == "1"
	t.Logf("listed %d file(s)", len(files))
	for i, f := range files {
		if verbose {
			t.Logf("  %-40s %-60s %8d bytes  %s", f.ID, f.MimeType, f.Size, f.ModifiedDateTime)
		} else {
			t.Logf("  file[%d] %-60s %8d bytes", i, f.MimeType, f.Size)
		}
	}

	// A cursor proves the incremental path can be bootstrapped.
	cur, err := a.LatestCursor(ctx)
	if err != nil {
		t.Fatalf("LatestCursor: %v", err)
	}
	if cur == "" {
		t.Fatal("LatestCursor returned an empty watermark for a non-empty share")
	}
	t.Logf("cursor OK (%s)", cur)

	// At the current watermark nothing is newer, so the delta must be empty. This
	// is the property that keeps a poll cycle cheap when nothing has changed.
	changes, next, err := a.Delta(ctx, cur)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("delta at the current watermark returned %d change(s), want 0", len(changes))
	}
	if next != cur {
		t.Errorf("watermark moved from %q to %q with no changes", cur, next)
	}

	// From a zero cursor every file is a change — the bootstrap path.
	changes, _, err = a.Delta(ctx, "")
	if err != nil {
		t.Fatalf("Delta(empty): %v", err)
	}
	if len(changes) != len(files) {
		t.Errorf("delta from empty cursor = %d change(s), want %d (all files)", len(changes), len(files))
	}

	// Actually pull bytes: enumeration and reads are different SMB operations, so
	// listing does not prove the account can fetch content.
	target := files[0]
	rc, err := a.Open(ctx, target)
	if err != nil {
		t.Fatalf("Open(%s): %v", target.ID, err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("reading %s: %v", target.ID, err)
	}
	if n != target.Size {
		t.Errorf("downloaded %d bytes, but the listing reported %d", n, target.Size)
	}
	t.Logf("downloaded one %s file: %d bytes", target.MimeType, n)

	// GetFile round-trips the identity the engine stores.
	got, err := a.GetFile(ctx, target.ID)
	if err != nil {
		t.Fatalf("GetFile(%s): %v", target.ID, err)
	}
	if got == nil || got.ID != target.ID {
		t.Fatalf("GetFile returned %+v, want id %q", got, target.ID)
	}

	// A path that cannot exist must be reported as absent, not as a hard failure —
	// the engine relies on this to retire memories for deleted files.
	if _, err := a.GetFile(ctx, "definitely/not/here-9f3a1c.bin"); err != source.ErrNotFound {
		t.Errorf("GetFile(missing) err = %v, want source.ErrNotFound", err)
	}
}
