package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/syncer"
)

func TestMetricsExposition(t *testing.T) {
	m := NewMetrics()
	m.RecordSync("full", &syncer.Result{Added: 3, Updated: 1, Deleted: 2, Skipped: 4, Errors: []string{"e1", "e2"}})
	m.RecordSync("delta", &syncer.Result{Added: 1})
	m.RecordSync("full", nil) // hard failure: counts the attempt, no file deltas
	m.RecordThrottle()
	m.RecordThrottle()
	m.RecordRenewal(true)
	m.RecordRenewal(false)
	m.SetPendingFn(func() (int, int, int, int) { return 5, 6, 7, 8 })

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	out := buf.String()

	want := map[string]string{
		"sharepoint_up":                                  "1",
		"sharepoint_files_added_total":                   "4", // 3 + 1
		"sharepoint_files_updated_total":                 "1",
		"sharepoint_files_deleted_total":                 "2",
		"sharepoint_files_skipped_total":                 "4",
		"sharepoint_sync_errors_total":                   "2",
		"sharepoint_full_syncs_total":                    "2", // full + nil-full
		"sharepoint_delta_syncs_total":                   "1",
		"sharepoint_graph_throttle_events_total":         "2",
		"sharepoint_subscription_renewals_total":         "2",
		"sharepoint_subscription_renewal_failures_total": "1",
		"sharepoint_pending_add":                         "5",
		"sharepoint_pending_update":                      "6",
		"sharepoint_pending_remove":                      "7",
		"sharepoint_pending_dead":                        "8",
	}
	for name, val := range want {
		if !strings.Contains(out, "\n"+name+" "+val+"\n") {
			t.Errorf("expected metric line %q = %q in output:\n%s", name, val, out)
		}
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("missing # TYPE line for %s", name)
		}
	}
	if !strings.Contains(out, "sharepoint_last_sync_timestamp_seconds ") {
		t.Error("missing last_sync timestamp gauge")
	}

	// nil-safety (must not panic).
	var mn *Metrics
	mn.RecordSync("full", nil)
	mn.RecordThrottle()
	mn.RecordRenewal(true)
	mn.SetPendingFn(func() (int, int, int, int) { return 0, 0, 0, 0 })
	mn.WritePrometheus(&buf)
}
