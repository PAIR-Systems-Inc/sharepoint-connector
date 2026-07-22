// Package store is a durable, queryable sync-history log backed by SQLite
// (pure-Go driver, so it works in the CGO_ENABLED=0 distroless build). It
// records every per-item sync outcome and serves them to GET /syncs. The DB
// lives on the persistent /data volume alongside the delta cursor.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/syncer"
)

// Store is a SQLite-backed sync-history log. Safe for concurrent use.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS sync_events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        INTEGER NOT NULL,
    file_id   TEXT,
    file_name TEXT,
    memory_id TEXT,
    space_id  TEXT,
    op        TEXT,
    status    TEXT,
    message   TEXT
);
CREATE INDEX IF NOT EXISTS idx_sync_events_ts ON sync_events(ts);
CREATE INDEX IF NOT EXISTS idx_sync_events_status ON sync_events(status);`

// Open opens (creating if needed) the SQLite DB at path and ensures the schema.
// WAL + a busy timeout let the /syncs reader run alongside sync writes.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init sync-history schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Record stores one sync event, stamped with the current time.
func (s *Store) Record(e syncer.SyncEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO sync_events(ts,file_id,file_name,memory_id,space_id,op,status,message)
		 VALUES(?,?,?,?,?,?,?,?)`,
		time.Now().Unix(), e.FileID, e.FileName, e.MemoryID, e.SpaceID, e.Op, e.Status, e.Message)
	return err
}

// Recent returns up to limit most-recent records (newest first), optionally
// filtered by status ("success"/"failure"/"skipped"; "" = any).
func (s *Store) Recent(limit int, status string) ([]syncer.SyncRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id,ts,file_id,file_name,memory_id,space_id,op,status,message FROM sync_events`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []syncer.SyncRecord
	for rows.Next() {
		var r syncer.SyncRecord
		if err := rows.Scan(&r.ID, &r.TS, &r.FileID, &r.FileName, &r.MemoryID, &r.SpaceID, &r.Op, &r.Status, &r.Message); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Prune deletes sync events older than cutoff and reclaims WAL space, so the
// history (which shares the /data volume with the delta cursor and pending sets)
// does not grow without bound and eventually degrade sync itself. Returns the
// number of rows removed.
func (s *Store) Prune(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sync_events WHERE ts < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// Checkpoint-truncate so the deleted rows' pages are actually returned to the
	// filesystem rather than lingering in the WAL.
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return n, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }
