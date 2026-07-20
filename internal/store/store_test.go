package store

import (
	"path/filepath"
	"testing"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/syncer"
)

// TestStore exercises the real SQLite store: record, query, filter, limit, and
// persistence across reopen.
func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync_history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	events := []syncer.SyncEvent{
		{FileID: "a", FileName: "a.pdf", MemoryID: "m-a", SpaceID: "sp", Op: "add", Status: "success"},
		{FileID: "b", FileName: "b.pdf", MemoryID: "m-b", SpaceID: "sp", Op: "add", Status: "failure", Message: "boom"},
		{FileID: "c", FileName: "c.pdf", MemoryID: "m-c", SpaceID: "sp", Op: "update", Status: "success"},
	}
	for _, e := range events {
		if err := s.Record(e); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.Recent(100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("Recent(all) = %d rows, want 3", len(all))
	}
	if all[0].FileID != "c" { // newest first
		t.Errorf("newest row = %q, want c", all[0].FileID)
	}
	if all[0].ID == 0 || all[0].TS == 0 {
		t.Errorf("id/ts should be populated: %+v", all[0])
	}
	if all[0].SpaceID != "sp" {
		t.Errorf("space id not stored: %+v", all[0])
	}

	failed, err := s.Recent(100, "failure")
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].FileID != "b" || failed[0].Message != "boom" {
		t.Fatalf("status filter = %+v, want the single failure b/boom", failed)
	}

	if lim, _ := s.Recent(1, ""); len(lim) != 1 {
		t.Errorf("limit 1 returned %d rows", len(lim))
	}

	// Durability: reopening the same file must still hold the records.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if again, _ := s2.Recent(100, ""); len(again) != 3 {
		t.Fatalf("after reopen = %d rows, want 3 (persistence)", len(again))
	}
}
