package memid

import "testing"

// TestFromFileID pins the Go implementation to the Python reference
// (uuid_from_file_id in goodmem_client.py). Expected values were produced by the
// Python oracle; a mismatch means the port diverged and would create
// duplicate/orphan memories.
func TestFromFileID(t *testing.T) {
	oracle := map[string]string{
		"01DSLNGZ2OAHMTF4SKE5BYGBMAYG6X6HMV": "f4358a63-c03d-586f-ad11-7c24ce5da004",
		"hello":                              "f4dc6bdf-f57e-5719-83dd-10bf2b95c110",
		"":                                   "0e8569b1-66c6-5447-b50e-ae90d27b1a04",
	}
	for in, want := range oracle {
		if got := FromFileID(in); got != want {
			t.Errorf("FromFileID(%q) = %s, want %s (Python oracle)", in, got, want)
		}
	}
}
