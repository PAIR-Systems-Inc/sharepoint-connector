package syncer

import "testing"

func TestRefuseMassDelete(t *testing.T) {
	cases := []struct {
		nFiles, nMemories int
		want              bool
	}{
		{0, 5, true},    // 0 files but memories exist → refuse (likely transient failure)
		{0, 0, false},   // both empty → nothing to delete, allow
		{3, 5, false},   // files present → normal reconcile (may delete some)
		{5, 0, false},   // files present, empty space → normal add-only
		{1, 100, false}, // even one file means SharePoint responded → allow
	}
	for _, tc := range cases {
		if got := refuseMassDelete(tc.nFiles, tc.nMemories); got != tc.want {
			t.Errorf("refuseMassDelete(%d,%d) = %v, want %v", tc.nFiles, tc.nMemories, got, tc.want)
		}
	}
}
