package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
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
// Each pending item carries an attempt count; after maxAttempts transient
// failures it is moved to a durable dead-letter set (and, if a Sink is set,
// surfaced to the sync history) instead of being re-downloaded and re-ingested
// forever — the unbounded retry loop the Python reference had.
//
// The one-shot CLI passes a nil *Retrier (Options.Retry == nil): no pending sets
// and no polling, matching sync_once.py. All methods are nil-safe.
type Retrier struct {
	addPath    string
	updatePath string
	removePath string
	deadPath   string

	maxAttempts int       // transient failures before an item is dead-lettered
	Sink        EventSink // optional: dead-letter events → durable sync history

	pollInterval    time.Duration
	pollMaxAttempts int
	sleep           func(context.Context, time.Duration) // injectable for tests

	mu sync.Mutex // guards the pending files (load-modify-save)
}

const (
	defaultPollInterval    = 10 * time.Second
	defaultPollMaxAttempts = 30 // ~5 min, matching listener.py
	defaultMaxAttempts     = 10 // transient retries before dead-lettering an item
)

// NewRetrier stores the pending-set files alongside dir (the same directory as
// the delta-link file). maxAttempts <= 0 uses defaultMaxAttempts.
func NewRetrier(dir string, maxAttempts int) *Retrier {
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	return &Retrier{
		addPath:         filepath.Join(dir, ".graph_pending_add"),
		updatePath:      filepath.Join(dir, ".graph_pending_update"),
		removePath:      filepath.Join(dir, ".graph_pending_removes"),
		deadPath:        filepath.Join(dir, ".graph_dead"),
		maxAttempts:     maxAttempts,
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

// --- pending-set persistence (one "id<TAB>attempts" line per item) ---

// readIDSet parses a pending file into id → attempt count. Lines may be a bare
// "id" (count 0, the pre-dead-letter format) or "id<TAB>count".
func readIDSet(path string) map[string]int {
	set := map[string]int{}
	b, err := os.ReadFile(path)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, cnt, hasCnt := strings.Cut(line, "\t")
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		n := 0
		if hasCnt {
			n, _ = strconv.Atoi(strings.TrimSpace(cnt))
		}
		set[id] = n
	}
	return set
}

func writeIDSet(path string, set map[string]int) error {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sb strings.Builder
	for _, id := range ids {
		sb.WriteString(id)
		sb.WriteByte('\t')
		sb.WriteString(strconv.Itoa(set[id]))
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

func (r *Retrier) load(path string) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return readIDSet(path)
}

// discard removes id from the set at path (a no-op if absent). Used on success
// and on a permanent skip (oversized / unsupported) so the item stops retrying.
func (r *Retrier) discard(path, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := readIDSet(path)
	if _, ok := set[id]; !ok {
		return
	}
	delete(set, id)
	_ = writeIDSet(path, set)
}

// bump increments id's attempt count at path; once it reaches maxAttempts the id
// is moved to the dead-letter set and (if a Sink is set) surfaced to the sync
// history, so a permanently-failing item stops being re-downloaded every sync.
// op labels the dead-letter event ("add"/"update"/"remove").
func (r *Retrier) bump(path, id, op string) {
	r.mu.Lock()
	set := readIDSet(path)
	n := set[id] + 1
	dead := n >= r.maxAttempts
	if dead {
		delete(set, id)
	} else {
		set[id] = n
	}
	_ = writeIDSet(path, set)
	if dead {
		deadSet := readIDSet(r.deadPath)
		deadSet[id] = n
		_ = writeIDSet(r.deadPath, deadSet)
	}
	r.mu.Unlock()
	if dead && r.Sink != nil {
		r.Sink(SyncEvent{FileID: id, Op: op, Status: "dead",
			Message: fmt.Sprintf("parked after %d failed attempts; needs operator attention", n)})
	}
}

// undead removes id from the dead-letter set — called on a later success so a
// file that starts working again (e.g. after an edit) leaves the parked set.
func (r *Retrier) undead(id string) { r.discard(r.deadPath, id) }

func (r *Retrier) discardAdd(id string) { r.discard(r.addPath, id); r.undead(id) }
func (r *Retrier) loadAdd() map[string]int {
	if r == nil {
		return nil
	}
	return r.load(r.addPath)
}

func (r *Retrier) discardUpdate(id string) { r.discard(r.updatePath, id); r.undead(id) }
func (r *Retrier) loadUpdate() map[string]int {
	if r == nil {
		return nil
	}
	return r.load(r.updatePath)
}

func (r *Retrier) discardRemove(id string) { r.discard(r.removePath, id); r.undead(id) }
func (r *Retrier) loadRemove() map[string]int {
	if r == nil {
		return nil
	}
	return r.load(r.removePath)
}

// Counts returns the current depth of the pending add/update/remove queues and
// the dead-letter set (for metrics). Nil-safe.
func (r *Retrier) Counts() (add, update, remove, dead int) {
	if r == nil {
		return 0, 0, 0, 0
	}
	return len(r.loadAdd()), len(r.loadUpdate()), len(r.loadRemove()), len(r.load(r.deadPath))
}

// --- outcome → pending-set bookkeeping (all nil-safe) ---

func (r *Retrier) recordAdd(fileID string, out ingestResult) {
	if r == nil {
		return
	}
	switch out {
	case resOK, resSkipped:
		r.discardAdd(fileID) // done (or a permanent skip) → stop retrying
	case resFailedProcessing:
		r.discard(r.addPath, fileID)
		r.bump(r.updatePath, fileID, "update") // memory may exist in a bad state → delete-then-add
	case resTransient:
		r.bump(r.addPath, fileID, "add")
	}
}

func (r *Retrier) recordUpdate(fileID string, out ingestResult) {
	if r == nil {
		return
	}
	switch out {
	case resOK, resSkipped:
		r.discardUpdate(fileID)
	case resFailedProcessing, resTransient:
		r.bump(r.updatePath, fileID, "update")
	}
}

func (r *Retrier) recordRemove(fileID string, ok bool) {
	if r == nil {
		return
	}
	if ok {
		r.discardRemove(fileID)
	} else {
		r.bump(r.removePath, fileID, "remove")
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

// fileFetcher fetches a file's current source state by ID (satisfied by any
// source.Source; narrowed to an interface so the merge logic is unit-testable).
type fileFetcher interface {
	GetFile(ctx context.Context, id string) (*source.FileInfo, error)
}

// merge augments the delta work with previously-failed items (re-fetching their
// current source state) and resolves any file that lands in more than one list.
// Returns the merged add/update file infos and remove file IDs.
func (r *Retrier) merge(ctx context.Context, gc fileFetcher, addFiles, updateFiles []source.FileInfo, removeIDs []string) ([]source.FileInfo, []source.FileInfo, []string) {
	addFiles = r.mergeInto(ctx, gc, addFiles, r.loadAdd(), r.discardAdd)
	updateFiles = r.mergeInto(ctx, gc, updateFiles, r.loadUpdate(), r.discardUpdate)
	for id := range r.loadRemove() {
		if !containsStr(removeIDs, id) {
			removeIDs = append(removeIDs, id)
		}
	}
	return r.resolveConflicts(ctx, gc, addFiles, updateFiles, removeIDs)
}

// mergeInto appends pending file IDs (not already present) to files, re-fetching
// each from the source. A file that is gone (ErrNotFound) or unsupported is
// discarded from the pending set; a transient fetch failure leaves it pending.
func (r *Retrier) mergeInto(ctx context.Context, gc fileFetcher, files []source.FileInfo, pending map[string]int, discard func(string)) []source.FileInfo {
	for id := range pending {
		if hasFileID(files, id) {
			continue
		}
		f, err := gc.GetFile(ctx, id)
		if err != nil {
			if errors.Is(err, source.ErrNotFound) {
				discard(id) // gone from the source
			}
			continue // transient: keep pending, retry next sync
		}
		if f == nil { // a folder, not a file
			continue
		}
		if !IsMimeSupported(f.MimeType) {
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
// more than one list it re-checks the source: absent/unsupported → keep only the
// remove; present/supported → drop the remove, and if in both add and update
// keep only the update (update = delete-then-add).
func (r *Retrier) resolveConflicts(ctx context.Context, gc fileFetcher, addFiles, updateFiles []source.FileInfo, removeIDs []string) ([]source.FileInfo, []source.FileInfo, []string) {
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
		f, err := gc.GetFile(ctx, id)
		supported := err == nil && f != nil && IsMimeSupported(f.MimeType)
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

func hasFileID(files []source.FileInfo, id string) bool {
	for _, f := range files {
		if f.ID == id {
			return true
		}
	}
	return false
}

func idSet(files []source.FileInfo) map[string]bool {
	s := make(map[string]bool, len(files))
	for _, f := range files {
		s[f.ID] = true
	}
	return s
}

func removeFileByID(files []source.FileInfo, id string) []source.FileInfo {
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
