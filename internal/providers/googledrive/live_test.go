package googledrive

import (
	"context"
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
	t.Logf("listed %d file(s) in Shared Drive %s", len(files), driveID)
	for _, f := range files {
		t.Logf("  %-55s %-70s %d bytes", f.Name, f.MimeType, f.Size)
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
}
