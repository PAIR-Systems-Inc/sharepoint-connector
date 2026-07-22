package graph

import (
	"os"
	"testing"
)

// TestLive_ListSharePoint exercises the hand-rolled client against the real
// Microsoft Graph API (auth → site → drives → recursive file list). It is
// skipped unless GRAPH_LIVE=1 and the Azure/SharePoint env vars are set:
//
//	GRAPH_LIVE=1 go test ./internal/graph -run TestLive -v
//
// Read-only; makes no changes to the site.
func TestLive_ListSharePoint(t *testing.T) {
	if os.Getenv("GRAPH_LIVE") != "1" {
		t.Skip("set GRAPH_LIVE=1 (and Azure/SharePoint env) to run the live Graph test")
	}
	cid := os.Getenv("AZURE_AD_CLIENT_ID")
	tid := os.Getenv("AZURE_AD_TENANT_ID")
	sec := os.Getenv("AZURE_AD_CLIENT_SECRET")
	site := os.Getenv("SHAREPOINT_SITE_URL")
	if cid == "" || tid == "" || sec == "" || site == "" {
		t.Skip("missing AZURE_AD_* / SHAREPOINT_SITE_URL env")
	}

	c := NewClient(cid, tid, sec, site)

	siteID, err := c.GetSiteID()
	if err != nil {
		t.Fatalf("GetSiteID: %v", err)
	}
	if siteID == "" {
		t.Fatal("GetSiteID returned empty id")
	}
	t.Logf("site id: %s", siteID)

	drives, err := c.GetDrives(siteID)
	if err != nil {
		t.Fatalf("GetDrives: %v", err)
	}
	t.Logf("drives: %d", len(drives))
	if len(drives) == 0 {
		t.Fatal("no drives found")
	}

	files, err := c.ListFiles("", "", true, siteID)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	t.Logf("files (recursive): %d", len(files))
	if len(files) > 0 {
		f := files[0]
		t.Logf("sample file: name=%q mime=%q size=%d relpath=%q modified=%q",
			f.Name, f.MimeType, f.Size, f.RelativePath, f.ModifiedDateTime)
		if f.ID == "" {
			t.Error("first file has empty ID")
		}
	}

	// Exercise the delta bootstrap (returns just the latest delta link, no items).
	_, deltaLink, err := c.DriveDelta(drives[0].ID, "", true)
	if err != nil {
		t.Fatalf("DriveDelta(token=latest): %v", err)
	}
	if deltaLink == "" {
		t.Error("DriveDelta bootstrap returned empty delta link")
	} else {
		t.Log("delta bootstrap: got a delta link ✓")
	}
}
