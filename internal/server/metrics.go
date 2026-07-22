package server

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/syncer"
)

// Metrics holds Prometheus-style counters and gauges for the listener, exposed
// at GET /metrics in the text exposition format. All fields are updated with
// atomics, so it is safe for concurrent use.
type Metrics struct {
	filesAdded         atomic.Int64
	filesUpdated       atomic.Int64
	filesDeleted       atomic.Int64
	filesSkipped       atomic.Int64
	syncErrors         atomic.Int64
	fullSyncs          atomic.Int64
	deltaSyncs         atomic.Int64
	throttleEvents     atomic.Int64
	subRenewals        atomic.Int64
	subRenewalFailures atomic.Int64
	lastSyncUnix       atomic.Int64

	// pendingFn provides the current pending-retry queue depths at scrape time.
	pendingFn atomic.Value // func() (add, update, remove int)
}

// NewMetrics returns a zeroed metrics registry.
func NewMetrics() *Metrics { return &Metrics{} }

// SetPendingFn registers a provider for the current pending-retry queue depths
// and dead-letter count (read at scrape time), e.g. syncer.Retrier.Counts.
func (m *Metrics) SetPendingFn(fn func() (add, update, remove, dead int)) {
	if m == nil || fn == nil {
		return
	}
	m.pendingFn.Store(fn)
}

// RecordSync records the result of a sync. kind is "full" or "delta"; res may be
// nil when a sync failed before producing a result.
func (m *Metrics) RecordSync(kind string, res *syncer.Result) {
	if m == nil {
		return
	}
	if kind == "full" {
		m.fullSyncs.Add(1)
	} else {
		m.deltaSyncs.Add(1)
	}
	m.lastSyncUnix.Store(time.Now().Unix())
	if res == nil {
		return
	}
	m.filesAdded.Add(int64(res.Added))
	m.filesUpdated.Add(int64(res.Updated))
	m.filesDeleted.Add(int64(res.Deleted))
	m.filesSkipped.Add(int64(res.Skipped))
	m.syncErrors.Add(int64(len(res.Errors)))
}

// RecordThrottle counts a Graph throttle/backoff event.
func (m *Metrics) RecordThrottle() {
	if m != nil {
		m.throttleEvents.Add(1)
	}
}

// RecordRenewal counts a subscription renew/ensure call (and its failures).
func (m *Metrics) RecordRenewal(ok bool) {
	if m == nil {
		return
	}
	m.subRenewals.Add(1)
	if !ok {
		m.subRenewalFailures.Add(1)
	}
}

// WritePrometheus writes all metrics in the Prometheus text exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	if m == nil {
		return
	}
	pAdd, pUpd, pRem, pDead := 0, 0, 0, 0
	if fn, ok := m.pendingFn.Load().(func() (int, int, int, int)); ok && fn != nil {
		pAdd, pUpd, pRem, pDead = fn()
	}
	metrics := []struct {
		name, typ, help string
		val             int64
	}{
		{"sharepoint_up", "gauge", "1 while the listener is running.", 1},
		{"sharepoint_files_added_total", "counter", "Files ingested (created) in Goodmem.", m.filesAdded.Load()},
		{"sharepoint_files_updated_total", "counter", "Files re-ingested (updated).", m.filesUpdated.Load()},
		{"sharepoint_files_deleted_total", "counter", "Memories deleted (orphaned or removed).", m.filesDeleted.Load()},
		{"sharepoint_files_skipped_total", "counter", "Files skipped (unsupported MIME / no download URL).", m.filesSkipped.Load()},
		{"sharepoint_sync_errors_total", "counter", "Per-item sync errors.", m.syncErrors.Load()},
		{"sharepoint_full_syncs_total", "counter", "Full syncs run.", m.fullSyncs.Load()},
		{"sharepoint_delta_syncs_total", "counter", "Delta syncs run.", m.deltaSyncs.Load()},
		{"sharepoint_graph_throttle_events_total", "counter", "Graph throttle/backoff events.", m.throttleEvents.Load()},
		{"sharepoint_subscription_renewals_total", "counter", "Subscription renew/ensure calls.", m.subRenewals.Load()},
		{"sharepoint_subscription_renewal_failures_total", "counter", "Subscription renew/ensure failures.", m.subRenewalFailures.Load()},
		{"sharepoint_last_sync_timestamp_seconds", "gauge", "Unix time of the last sync.", m.lastSyncUnix.Load()},
		{"sharepoint_pending_add", "gauge", "Files queued for retry as add.", int64(pAdd)},
		{"sharepoint_pending_update", "gauge", "Files queued for retry as update (delete-then-add).", int64(pUpd)},
		{"sharepoint_pending_remove", "gauge", "Files queued for retry as remove.", int64(pRem)},
		{"sharepoint_pending_dead", "gauge", "Files parked after exhausting retries (need operator attention).", int64(pDead)},
	}
	for _, mt := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", mt.name, mt.help, mt.name, mt.typ, mt.name, mt.val)
	}
}

// compile-time check that syncer.Retrier.Counts matches the pendingFn shape.
var _ = func(r *syncer.Retrier) { var _ func() (int, int, int, int) = r.Counts }
