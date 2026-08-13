package syncer

import (
	"reflect"
	"testing"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/memid"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// nsTest is the namespace these engine tests run under: the integration tests
// drive the real SharePoint adapter, so the engine derives memory ids in the
// SharePoint namespace. mid computes the memory id the engine would for a file id.
const nsTest = "sharepoint.file.id"

func mid(id string) string { return memid.FromFileID(nsTest, id) }

// TestDiffFull is a characterization test of the full-sync set math + timestamp
// rules.
func TestDiffFull(t *testing.T) {
	sp := []source.FileInfo{
		{ID: "A", ModifiedDateTime: "2026-01-02T00:00:00Z"}, // in both, source newer -> update
		{ID: "B", ModifiedDateTime: "2026-01-01T00:00:00Z"}, // only at source      -> add
		{ID: "C", ModifiedDateTime: "2026-01-01T00:00:00Z"}, // in both, equal       -> skip
		{ID: "D", ModifiedDateTime: "2026-01-01T00:00:00Z"}, // in both, Goodmem newer-> anomaly
	}
	gm := []string{
		mid("A"),
		mid("C"),
		mid("D"),
		mid("X"), // only in Goodmem -> delete
	}
	stored := map[string]StoredMeta{
		mid("A"): {Modified: "2026-01-01T00:00:00Z"}, // older than SP -> update
		mid("C"): {Modified: "2026-01-01T00:00:00Z"}, // equal -> skip
		mid("D"): {Modified: "2026-01-02T00:00:00Z"}, // newer than SP -> anomaly
	}

	got := DiffFull(sp, gm, stored, nsTest, "")
	want := Plan{
		Add:             []string{"B"},
		Update:          []string{"A"},
		Delete:          []string{mid("X")},
		UnexpectedNewer: []string{"D"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiffFull:\n got  %+v\n want %+v", got, want)
	}
}

func TestDiffFull_MissingStoredTimestampForcesUpdate(t *testing.T) {
	sp := []source.FileInfo{{ID: "E", ModifiedDateTime: "2026-01-01T00:00:00Z"}}
	gm := []string{mid("E")}
	got := DiffFull(sp, gm, map[string]StoredMeta{}, nsTest, "") // no stored ts
	if len(got.Update) != 1 || got.Update[0] != "E" || len(got.Add) != 0 || len(got.Delete) != 0 {
		t.Errorf("missing stored timestamp should force update; got %+v", got)
	}
}

// TestDiffFull_EnrichVersion pins the rule that makes an extractor change
// re-ingest anything at all: the files are UNCHANGED, so the timestamp diff
// alone says "skip" and the corpus would keep the old extractor's metadata
// forever.
func TestDiffFull_EnrichVersion(t *testing.T) {
	ts := "2026-01-01T00:00:00Z"
	sp := []source.FileInfo{
		{ID: "A", ModifiedDateTime: ts},
		{ID: "B", ModifiedDateTime: ts},
	}
	gm := []string{mid("A"), mid("B")}
	stored := map[string]StoredMeta{
		mid("A"): {Modified: ts, EnrichVersion: "v1"}, // stale extractor -> update
		mid("B"): {Modified: ts, EnrichVersion: "v2"}, // current        -> skip
	}

	got := DiffFull(sp, gm, stored, nsTest, "v2")
	if len(got.Update) != 1 || got.Update[0] != "A" {
		t.Errorf("stale enrich_version should force update of A only; got %+v", got)
	}

	// With no version configured the rule is inert — enrichment is opt-in, and a
	// space full of memories with no enrich_version must not re-ingest itself.
	if got := DiffFull(sp, gm, stored, nsTest, ""); len(got.Update) != 0 {
		t.Errorf("no wanted version should leave the plan empty; got %+v", got)
	}

	// A memory that has never been enriched is as stale as a wrongly-enriched
	// one: turning enrichment ON must re-ingest the corpus.
	bare := map[string]StoredMeta{mid("A"): {Modified: ts}, mid("B"): {Modified: ts}}
	if got := DiffFull(sp, gm, bare, nsTest, "v1"); len(got.Update) != 2 {
		t.Errorf("enabling enrichment should update every memory; got %+v", got)
	}
}

// TestDiffDelta pins the delta classification to the listener's behavior.
func TestDiffDelta(t *testing.T) {
	changes := []source.Change{
		{ID: "D", Deleted: true}, // deleted -> Delete uuid(D)
		{ID: "F"},                // non-file (folder) -> ignored
		{ID: "A", IsFile: true, File: source.FileInfo{ID: "A", ModifiedDateTime: "2026-01-01T00:00:00Z"}}, // new     -> Add
		{ID: "B", IsFile: true, File: source.FileInfo{ID: "B", ModifiedDateTime: "2026-01-02T00:00:00Z"}}, // present, stored older -> Update
		{ID: "C", IsFile: true, File: source.FileInfo{ID: "C", ModifiedDateTime: "2026-01-01T00:00:00Z"}}, // present, stored newer -> Update + anomaly
	}
	stored := map[string]string{
		mid("B"): "2026-01-01T00:00:00Z", // older -> update
		mid("C"): "2026-01-02T00:00:00Z", // newer -> update + anomaly
	}
	got := DiffDelta(changes, stored, nsTest)
	want := Plan{
		Add:             []string{"A"},
		Update:          []string{"B", "C"},
		Delete:          []string{mid("D")},
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
