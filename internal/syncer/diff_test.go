package syncer

import (
	"reflect"
	"testing"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/graph"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/memid"
)

// TestDiffFull is a characterization test of the full-sync set math + timestamp
// rules, pinning the Go engine to sync_once.py's behavior.
func TestDiffFull(t *testing.T) {
	sp := []graph.FileInfo{
		{ID: "A", ModifiedDateTime: "2026-01-02T00:00:00Z"}, // in both, SharePoint newer -> update
		{ID: "B", ModifiedDateTime: "2026-01-01T00:00:00Z"}, // only in SharePoint  -> add
		{ID: "C", ModifiedDateTime: "2026-01-01T00:00:00Z"}, // in both, equal       -> skip
		{ID: "D", ModifiedDateTime: "2026-01-01T00:00:00Z"}, // in both, Goodmem newer-> anomaly
	}
	gm := []string{
		memid.FromFileID("A"),
		memid.FromFileID("C"),
		memid.FromFileID("D"),
		memid.FromFileID("X"), // only in Goodmem -> delete
	}
	stored := map[string]string{
		memid.FromFileID("A"): "2026-01-01T00:00:00Z", // older than SP -> update
		memid.FromFileID("C"): "2026-01-01T00:00:00Z", // equal -> skip
		memid.FromFileID("D"): "2026-01-02T00:00:00Z", // newer than SP -> anomaly
	}

	got := DiffFull(sp, gm, stored)
	want := Plan{
		Add:             []string{"B"},
		Update:          []string{"A"},
		Delete:          []string{memid.FromFileID("X")},
		UnexpectedNewer: []string{"D"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiffFull:\n got  %+v\n want %+v", got, want)
	}
}

func TestDiffFull_MissingStoredTimestampForcesUpdate(t *testing.T) {
	sp := []graph.FileInfo{{ID: "E", ModifiedDateTime: "2026-01-01T00:00:00Z"}}
	gm := []string{memid.FromFileID("E")}
	got := DiffFull(sp, gm, map[string]string{}) // no stored ts
	if len(got.Update) != 1 || got.Update[0] != "E" || len(got.Add) != 0 || len(got.Delete) != 0 {
		t.Errorf("missing stored timestamp should force update; got %+v", got)
	}
}

// TestDiffDelta pins the delta classification to the listener's behavior.
func TestDiffDelta(t *testing.T) {
	items := []graph.Item{
		{ID: "D", Deleted: true},   // deleted -> Delete uuid(D)
		{ID: "F", IsFolder: true},  // folder  -> ignored
		{ID: "A", IsFile: true, File: graph.FileInfo{ID: "A", ModifiedDateTime: "2026-01-01T00:00:00Z"}}, // new     -> Add
		{ID: "B", IsFile: true, File: graph.FileInfo{ID: "B", ModifiedDateTime: "2026-01-02T00:00:00Z"}}, // present, stored older -> Update
		{ID: "C", IsFile: true, File: graph.FileInfo{ID: "C", ModifiedDateTime: "2026-01-01T00:00:00Z"}}, // present, stored newer -> Update + anomaly
	}
	stored := map[string]string{
		memid.FromFileID("B"): "2026-01-01T00:00:00Z", // older -> update
		memid.FromFileID("C"): "2026-01-02T00:00:00Z", // newer -> update + anomaly
	}
	got := DiffDelta(items, stored)
	want := Plan{
		Add:             []string{"A"},
		Update:          []string{"B", "C"},
		Delete:          []string{memid.FromFileID("D")},
		UnexpectedNewer: []string{"C"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiffDelta:\n got  %+v\n want %+v", got, want)
	}
}

func TestIsMimeSupported(t *testing.T) {
	supported := []string{
		"text/plain", "text/html", "TEXT/CSV",
		"application/pdf", "APPLICATION/PDF", "application/rtf", "application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/xhtml+xml", "application/json", "application/ld+json",
	}
	unsupported := []string{"", "image/png", "application/octet-stream", "video/mp4", "application/epub+zip"}
	for _, m := range supported {
		if !IsMimeSupported(m) {
			t.Errorf("IsMimeSupported(%q) = false, want true", m)
		}
	}
	for _, m := range unsupported {
		if IsMimeSupported(m) {
			t.Errorf("IsMimeSupported(%q) = true, want false", m)
		}
	}
}
