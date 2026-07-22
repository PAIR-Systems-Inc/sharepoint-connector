package syncer

import (
	"strings"
	"testing"
)

func TestRefuseMassDelete(t *testing.T) {
	cases := []struct {
		name                       string
		nFiles, nMemories, nDelete int
		ratio                      float64
		wantRefuse                 bool
	}{
		{"zero files, memories exist", 0, 5, 5, 0.5, true},       // likely transient failure
		{"both empty", 0, 0, 0, 0.5, false},                      // nothing to delete
		{"normal reconcile", 3, 5, 1, 0.5, false},                // small deletion, allow
		{"files present, empty space", 5, 0, 0, 0.5, false},      // add-only
		{"one file, many memories", 1, 100, 99, 0.5, true},       // 99% delete → partial listing
		{"large but under ratio", 50, 100, 40, 0.5, false},       // 40% ≤ 50%, allow
		{"large over ratio", 50, 100, 60, 0.5, true},             // 60% > 50%, refuse
		{"over ratio but under floor", 100, 100, 8, 0.01, false}, // 8 ≤ floor(10), always allowed
		{"ratio disabled", 50, 100, 100, 0, false},               // proportional check off
		{"ratio 1 allows full delete", 50, 100, 90, 1, false},    // override: 90% ≤ 100%
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := refuseMassDelete(tc.nFiles, tc.nMemories, tc.nDelete, tc.ratio)
			if refused := reason != ""; refused != tc.wantRefuse {
				t.Errorf("refuseMassDelete(%d,%d,%d,%v) refused=%v (%q), want %v",
					tc.nFiles, tc.nMemories, tc.nDelete, tc.ratio, refused, reason, tc.wantRefuse)
			}
		})
	}

	// The zero-files reason must be distinct from the proportional one.
	if r := refuseMassDelete(0, 5, 5, 0.5); !strings.Contains(r, "0 files") {
		t.Errorf("zero-files reason unexpected: %q", r)
	}
	if r := refuseMassDelete(50, 100, 60, 0.5); !strings.Contains(r, "GRAPH_MAX_DELETE_RATIO") {
		t.Errorf("proportional reason should mention the override knob: %q", r)
	}
}
