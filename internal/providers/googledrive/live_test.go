package googledrive

import (
	"context"
	"io"
	"os"
	"testing"
)

// TestLive_ListGoogleDrive exercises the real Drive API through whatever
// credentials Application Default Credentials resolves — the code path used by
// the two keyless auth options (a GCP host's attached service account, and
// workload identity federation off GCP). It is skipped unless GDRIVE_LIVE=1 and
// GOOGLE_DRIVE_ID is set:
//
//	GDRIVE_LIVE=1 GOOGLE_DRIVE_ID=<id> go test ./internal/providers/googledrive -run TestLive -v
//
// Read-only; it lists metadata and makes no changes to the Drive. Deliberately
// needs no Goodmem configuration, so CI can verify credentials in isolation.
func TestLive_ListGoogleDrive(t *testing.T) {
	if os.Getenv("GDRIVE_LIVE") != "1" {
		t.Skip("set GDRIVE_LIVE=1 (and GOOGLE_DRIVE_ID) to run the live Drive test")
	}
	driveID := os.Getenv("GOOGLE_DRIVE_ID")
	if driveID == "" {
		t.Skip("missing GOOGLE_DRIVE_ID")
	}

	ctx := context.Background()
	c, err := NewWithADC(ctx, driveID)
	if err != nil {
		t.Fatalf("NewWithADC: %v", err)
	}

	files, err := c.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v (credentials resolved, but the Drive read failed — "+
			"check that the identity is a Viewer on the Shared Drive and that its "+
			"token carries the drive.readonly scope)", err)
	}
	// Log shapes, not names: this test runs in CI, and on a public repository the
	// job log is world-readable — customer document titles do not belong there.
	// Set GDRIVE_LIVE_VERBOSE=1 locally if you want the actual names.
	verbose := os.Getenv("GDRIVE_LIVE_VERBOSE") == "1"
	t.Logf("listed %d file(s) in Shared Drive", len(files))
	for i, f := range files {
		if verbose {
			t.Logf("  %-55s %-70s %d bytes", f.Name, f.MimeType, f.Size)
		} else {
			t.Logf("  file[%d] %-70s %d bytes", i, f.MimeType, f.Size)
		}
	}

	// A cursor proves the Changes API (the delta path) is reachable too.
	tok, err := c.StartPageToken(ctx)
	if err != nil {
		t.Fatalf("StartPageToken: %v", err)
	}
	if tok == "" {
		t.Fatal("StartPageToken returned an empty cursor")
	}
	t.Logf("changes cursor OK (%d chars)", len(tok))

	// Actually pull bytes. Listing only proves metadata access; downloading uses a
	// different endpoint (files.get?alt=media, or files.export for native docs), so
	// credentials that can list are not automatically proven able to fetch content.
	var picked *DriveFile
	for i := range files {
		if IsNativeDoc(files[i].MimeType) && ExportTarget(files[i].MimeType) == "" {
			continue // no export format; the engine skips these
		}
		picked = &files[i]
		break
	}
	if picked == nil {
		t.Skip("no downloadable file in the Drive to exercise content fetch")
	}
	rc, err := c.Open(ctx, picked.ID, picked.MimeType)
	if err != nil {
		t.Fatalf("Open(%s): %v", picked.MimeType, err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("reading %s: %v", picked.MimeType, err)
	}
	if n == 0 {
		t.Fatalf("downloaded a %s file but got 0 bytes", picked.MimeType)
	}
	t.Logf("downloaded one %s file: %d bytes", picked.MimeType, n)
}
