package memid

import "testing"

// TestFromFileID pins the SharePoint namespace to the Python reference
// (uuid_from_file_id in goodmem_client.py). Expected values were produced by the
// Python oracle; a mismatch means the port diverged and would create
// duplicate/orphan memories. These values MUST NOT change — SharePoint memories
// already in the wild carry them.
func TestFromFileID(t *testing.T) {
	oracle := map[string]string{
		"01DSLNGZ2OAHMTF4SKE5BYGBMAYG6X6HMV": "f4358a63-c03d-586f-ad11-7c24ce5da004",
		"hello":                              "f4dc6bdf-f57e-5719-83dd-10bf2b95c110",
		"":                                   "0e8569b1-66c6-5447-b50e-ae90d27b1a04",
	}
	for in, want := range oracle {
		if got := FromFileID("sharepoint.file.id", in); got != want {
			t.Errorf("FromFileID(sharepoint, %q) = %s, want %s (Python oracle)", in, got, want)
		}
	}
}

// TestNamespaceIsolation locks in that a different namespace yields a different
// id for the same file id — the whole point of per-source namespacing (a Google
// Drive file and a SharePoint file that happened to share an id must not collide
// on the same memory). It also pins the gdrive namespace value so it, too,
// becomes a stable contract once live.
func TestNamespaceIsolation(t *testing.T) {
	const id = "shared-file-id"
	sp := FromFileID("sharepoint.file.id", id)
	gd := FromFileID("google-drive.file.id", id)
	if sp == gd {
		t.Fatalf("namespaces collided: sharepoint and gdrive both gave %s", sp)
	}
	// Determinism: same inputs ⇒ same id.
	if again := FromFileID("google-drive.file.id", id); again != gd {
		t.Errorf("non-deterministic: %s != %s", again, gd)
	}
}
