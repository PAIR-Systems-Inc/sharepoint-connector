package syncer

// SyncEvent is a single per-item sync outcome, emitted to Options.Sink for
// durable sync-history recording (e.g. the SQLite store behind GET /syncs).
type SyncEvent struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MemoryID string `json:"memory_id"`
	SpaceID  string `json:"space_id"`
	Op       string `json:"op"`     // "add", "update", "delete"
	Status   string `json:"status"` // "success", "failure", "skipped"
	Message  string `json:"message,omitempty"`
}

// SyncRecord is a stored SyncEvent with its row id and timestamp (unix seconds).
type SyncRecord struct {
	ID int64 `json:"id"`
	TS int64 `json:"ts"`
	SyncEvent
}

// EventSink receives per-item sync outcomes. Nil = discard.
type EventSink func(SyncEvent)
