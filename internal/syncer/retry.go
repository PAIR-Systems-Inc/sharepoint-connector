package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/graph"
)

// Retrier carries the durable failure-retry state and Goodmem processing-status
// poll config used by the event-triggered listener. It ports listener.py's three
// persisted pending sets plus the post-create status polling:
//
//   - pending-add    → file IDs whose Goodmem add failed; retried as an add.
//   - pending-update → file IDs to retry as delete-then-add (a failed update, or
//     a create whose processingStatus came back FAILED).
//   - pending-remove → file IDs whose Goodmem delete failed; retried as a delete.
//
// A create is polled until Goodmem reports COMPLETED/FAILED (or a timeout), so a
// 200-but-later-FAILED ingest is not silently counted as success.
//
// The one-shot CLI passes a nil *Retrier (Options.Retry == nil): no pending sets
// and no polling, matching sync_once.py. All methods are nil-safe.
type Retrier struct {
	addPath    string
	updatePath string
	removePath string

	pollInterval    time.Duration
	pollMaxAttempts int
	sleep           func(context.Context, time.Duration) // injectable for tests

	mu sync.Mutex // guards the pending files (load-modify-save)
}

const (
	defaultPollInterval    = 10 * time.Second
	defaultPollMaxAttempts = 30 // ~5 min, matching listener.py
)

// NewRetrier stores the three pending-set files alongside dir (the same
// directory as the delta-link file).
func NewRetrier(dir string) *Retrier {
	return &Retrier{
		addPath:         filepath.Join(dir, ".graph_pending_add"),
		updatePath:      filepath.Join(dir, ".graph_pending_update"),
		removePath:      filepath.Join(dir, ".graph_pending_removes"),
		pollInterval:    defaultPollInterval,
		pollMaxAttempts: defaultPollMaxAttempts,
		sleep:           ctxSleep,
	}
}

// ctxSleep waits d, returning early if ctx is cancelled.
func ctxSleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// ingestResult is the outcome of a single add/update attempt.
type ingestResult int

const (
	resSkipped          ingestResult = iota // unsupported MIME / no download URL
	resOK                                   // ingested and (in listener mode) confirmed COMPLETED
	resFailedProcessing                     // Goodmem processingStatus FAILED → retry as delete-then-add
	resTransient                            // download/network/5xx/timeout → retry the same op
)

// --- pending-set persistence (newline-delimited file IDs) ---

func readIDSet(path string) map[string]bool {
	set := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			set[s] = true
		}
	}
	return set
}

func writeIDSet(path string, set map[string]bool) error {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return os.WriteFile(path, []byte(strings.Join(ids, "\n")), 0o600)
}

// mutate loads the set, adds or removes id, and saves — mirroring listener.py's
// load-before-mutate so a concurrent set is never clobbered with a stale one.
func (r *Retrier) mutate(path, id string, add bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := readIDSet(path)
	if add {
		if set[id] {
			return
		}
		set[id] = true
	} else {
		if !set[id] {
			return
		}
		delete(set, id)
	}
	_ = writeIDSet(path, set)
}

func (r *Retrier) load(path string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return readIDSet(path)
}

func (r *Retrier) addAdd(id string)     { r.mutate(r.addPath, id, true) }
func (r *Retrier) discardAdd(id string) { r.mutate(r.addPath, id, false) }
func (r *Retrier) loadAdd() map[string]bool {
	if r == nil {
		return nil
	}
	return r.load(r.addPath)
}

func (r *Retrier) addUpdate(id string)     { r.mutate(r.updatePath, id, true) }
func (r *Retrier) discardUpdate(id string) { r.mutate(r.updatePath, id, false) }
func (r *Retrier) loadUpdate() map[string]bool {
	if r == nil {
		return nil
	}
	return r.load(r.updatePath)
}

func (r *Retrier) addRemove(id string)     { r.mutate(r.removePath, id, true) }
func (r *Retrier) discardRemove(id string) { r.mutate(r.removePath, id, false) }
func (r *Retrier) loadRemove() map[string]bool {
	if r == nil {
		return nil
	}
	return r.load(r.removePath)
}

// Counts returns the number of file IDs currently queued in the pending add,
// update, and remove sets (queue depth, for metrics). Nil-safe.
func (r *Retrier) Counts() (add, update, remove int) {
	if r == nil {
		return 0, 0, 0
	}
	return len(r.loadAdd()), len(r.loadUpdate()), len(r.loadRemove())
}

// --- outcome → pending-set bookkeeping (all nil-safe) ---

func (r *Retrier) recordAdd(fileID string, out ingestResult) {
	if r == nil {
		return
	}
	switch out {
	case resOK:
		r.discardAdd(fileID)
	case resFailedProcessing:
		r.addUpdate(fileID) // memory may exist in a bad state → delete-then-add
	case resTransient:
		r.addAdd(fileID)
	}
}

func (r *Retrier) recordUpdate(fileID string, out ingestResult) {
	if r == nil {
		return
	}
	switch out {
	case resOK:
		r.discardUpdate(fileID)
	case resFailedProcessing, resTransient:
		r.addUpdate(fileID)
	}
}

func (r *Retrier) recordRemove(fileID string, ok bool) {
	if r == nil {
		return
	}
	if ok {
		r.discardRemove(fileID)
	} else {
		r.addRemove(fileID)
	}
}

// --- processing-status polling ---

// pollStatus polls Goodmem's get-memory-by-id until it reports COMPLETED or
// FAILED, or attempts are exhausted (returns "PENDING" on timeout). Mirrors
// _poll_memory_processing_status.
func (r *Retrier) pollStatus(ctx context.Context, gm *goodmem.Client, memID string) string {
	for i := 0; i < r.pollMaxAttempts; i++ {
		if m, err := gm.Memories().Get(ctx, memID, nil); err == nil {
			switch strings.ToUpper(m.ProcessingStatus) {
			case "COMPLETED":
				return "COMPLETED"
			case "FAILED":
				return "FAILED"
			}
		}
		r.sleep(ctx, r.pollInterval)
		if ctx.Err() != nil {
			return "PENDING"
		}
	}
	return "PENDING"
}

// --- pending merge + conflict resolution (delta path) ---

// fileGetter fetches a SharePoint file's current state by ID (satisfied by
// *graph.Client; narrowed to an interface so the merge logic is unit-testable).
type fileGetter interface {
	GetFileByID(driveID, itemID string) (*graph.FileInfo, error)
}

// merge augments the delta work with previously-failed items (re-fetching their
// current SharePoint state) and resolves any file that lands in more than one
// list. Ports the pending-merge + _resolve_sync_conflicts logic. Returns the
// merged add/update file infos and remove file IDs.
func (r *Retrier) merge(gc fileGetter, driveID string, addFiles, updateFiles []graph.FileInfo, removeIDs []string) ([]graph.FileInfo, []graph.FileInfo, []string) {
	addFiles = r.mergeInto(gc, driveID, addFiles, r.loadAdd(), r.discardAdd)
	updateFiles = r.mergeInto(gc, driveID, updateFiles, r.loadUpdate(), r.discardUpdate)
	for id := range r.loadRemove() {
		if !containsStr(removeIDs, id) {
			removeIDs = append(removeIDs, id)
		}
	}
	return r.resolveConflicts(gc, driveID, addFiles, updateFiles, removeIDs)
}

// mergeInto appends pending file IDs (not already present) to files, re-fetching
// each from SharePoint. A file that is gone (404) or unsupported is discarded
// from the pending set; a transient fetch failure leaves it pending.
func (r *Retrier) mergeInto(gc fileGetter, driveID string, files []graph.FileInfo, pending map[string]bool, discard func(string)) []graph.FileInfo {
	for id := range pending {
		if hasFileID(files, id) {
			continue
		}
		f, err := gc.GetFileByID(driveID, id)
		if err != nil {
			if isHTTPStatus(err, 404) {
				discard(id) // gone from SharePoint
			}
			continue // transient: keep pending, retry next sync
		}
		if f == nil { // a folder, not a file
			continue
		}
		if !IsMimeSupported(f.MimeType) || f.DownloadURL == "" {
			continue
		}
		if f.RelativePath == "" {
			f.RelativePath = f.Name
		}
		files = append(files, *f)
	}
	return files
}

// resolveConflicts ensures each file ID has at most one action. For any ID in
// more than one list it re-checks SharePoint: absent/unsupported → keep only the
// remove; present/supported → drop the remove, and if in both add and update
// keep only the update (update = delete-then-add). Ports _resolve_sync_conflicts.
func (r *Retrier) resolveConflicts(gc fileGetter, driveID string, addFiles, updateFiles []graph.FileInfo, removeIDs []string) ([]graph.FileInfo, []graph.FileInfo, []string) {
	addIDs := idSet(addFiles)
	updateIDs := idSet(updateFiles)
	removeSet := map[string]bool{}
	for _, id := range removeIDs {
		removeSet[id] = true
	}
	conflicts := map[string]bool{}
	for id := range addIDs {
		if updateIDs[id] || removeSet[id] {
			conflicts[id] = true
		}
	}
	for id := range updateIDs {
		if removeSet[id] {
			conflicts[id] = true
		}
	}
	for id := range conflicts {
		f, err := gc.GetFileByID(driveID, id)
		supported := err == nil && f != nil && IsMimeSupported(f.MimeType) && f.DownloadURL != ""
		if !supported {
			addFiles = removeFileByID(addFiles, id)
			updateFiles = removeFileByID(updateFiles, id)
			r.discardAdd(id)
			r.discardUpdate(id)
		} else {
			removeSet[id] = false
			r.discardRemove(id)
			if addIDs[id] && updateIDs[id] {
				addFiles = removeFileByID(addFiles, id) // keep only the update
			}
		}
	}
	var mergedRemoves []string
	for _, id := range removeIDs {
		if removeSet[id] {
			mergedRemoves = append(mergedRemoves, id)
		}
	}
	return addFiles, updateFiles, mergedRemoves
}

// --- small helpers ---

func hasFileID(files []graph.FileInfo, id string) bool {
	for _, f := range files {
		if f.ID == id {
			return true
		}
	}
	return false
}

func idSet(files []graph.FileInfo) map[string]bool {
	s := make(map[string]bool, len(files))
	for _, f := range files {
		s[f.ID] = true
	}
	return s
}

func removeFileByID(files []graph.FileInfo, id string) []graph.FileInfo {
	out := files[:0]
	for _, f := range files {
		if f.ID != id {
			out = append(out, f)
		}
	}
	return out
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// isHTTPStatus reports whether err is a *graph.HTTPError with the given status.
func isHTTPStatus(err error, status int) bool {
	var he *graph.HTTPError
	return errors.As(err, &he) && he.StatusCode == status
}
