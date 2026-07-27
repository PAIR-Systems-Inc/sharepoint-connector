package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/syncer"
)

func TestMetricsExposition(t *testing.T) {
	m := NewMetrics("google-drive")
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

	// Every series is connector_* and carries the source label.
	const lbl = `{source="google-drive"}`
	want := map[string]string{
		"connector_up":                                  "1",
		"connector_files_added_total":                   "4", // 3 + 1
		"connector_files_updated_total":                 "1",
		"connector_files_deleted_total":                 "2",
		"connector_files_skipped_total":                 "4",
		"connector_sync_errors_total":                   "2",
		"connector_full_syncs_total":                    "2", // full + nil-full
		"connector_delta_syncs_total":                   "1",
		"connector_throttle_events_total":               "2",
		"connector_subscription_renewals_total":         "2",
		"connector_subscription_renewal_failures_total": "1",
		"connector_pending_add":                         "5",
		"connector_pending_update":                      "6",
		"connector_pending_remove":                      "7",
		"connector_pending_dead":                        "8",
	}
	for name, val := range want {
		if !strings.Contains(out, "\n"+name+lbl+" "+val+"\n") {
			t.Errorf("expected metric line %q%s = %q in output:\n%s", name, lbl, val, out)
		}
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("missing # TYPE line for %s", name)
		}
	}
	if !strings.Contains(out, "connector_last_sync_timestamp_seconds"+lbl+" ") {
		t.Error("missing last_sync timestamp gauge")
	}
	// The old sharepoint_* names are gone (no back-compat aliasing).
	if strings.Contains(out, "sharepoint_") {
		t.Errorf("legacy sharepoint_* metric names still emitted:\n%s", out)
	}

	// A different provider labels its series accordingly.
	var spBuf bytes.Buffer
	NewMetrics("sharepoint").WritePrometheus(&spBuf)
	if !strings.Contains(spBuf.String(), `connector_up{source="sharepoint"} 1`) {
		t.Errorf("sharepoint source label missing:\n%s", spBuf.String())
	}

	// nil-safety (must not panic).
	var mn *Metrics
	mn.RecordSync("full", nil)
	mn.RecordThrottle()
	mn.RecordRenewal(true)
	mn.SetPendingFn(func() (int, int, int, int) { return 0, 0, 0, 0 })
	mn.WritePrometheus(&buf)
}
