package syncer

import (
	"context"
	"errors"
	"fmt"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/graph"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/memid"
)

// ErrDeltaExpired means the Graph delta token was invalid (410 Gone). The
// caller should run a full sync (RunFull) and re-bootstrap the delta link.
var ErrDeltaExpired = errors.New("delta token expired (410); full sync required")

// RunDelta applies one Graph delta batch to Goodmem and returns the next delta
// link. Deleted items are removed; changed files are added or updated (via the
// diff's existence check). On a 410 it returns ErrDeltaExpired.
func RunDelta(ctx context.Context, gc *graph.Client, gm *goodmem.Client, spaceID, driveID, deltaLink string, opts Options) (newLink string, res *Result, err error) {
	items, newLink, err := gc.DriveDelta(driveID, deltaLink, false)
	if err != nil {
		return "", nil, err
	}
	if items == nil && newLink == "" {
		return "", nil, ErrDeltaExpired
	}

	// Prepare file infos for the changed (non-deleted) file items, recovering a
	// missing download URL via GetFileByID (delta stubs often lack it).
	fileByID := make(map[string]graph.FileInfo)
	var candidateUUIDs []string
	for _, it := range items {
		if it.Deleted || !it.IsFile {
			continue
		}
		f := it.File
		if f.DownloadURL == "" {
			if full, gerr := gc.GetFileByID(driveID, it.ID); gerr == nil && full != nil {
				rel := f.RelativePath
				if rel == "" {
					rel = f.Name
				}
				f = *full
				f.RelativePath = rel
			}
		}
		if f.RelativePath == "" {
			f.RelativePath = f.Name
		}
		fileByID[it.ID] = f
		candidateUUIDs = append(candidateUUIDs, memid.FromFileID(it.ID))
	}

	stored, err := goodmemStoredModified(ctx, gm, candidateUUIDs)
	if err != nil {
		return "", nil, err
	}

	plan := DiffDelta(items, stored)
	res = &Result{Plan: plan}

	// Assemble the work as file infos / IDs so the durable pending sets (and
	// their re-fetched SharePoint state) can be merged in before applying.
	var addFiles, updateFiles []graph.FileInfo
	for _, id := range plan.Add {
		addFiles = append(addFiles, fileByID[id])
	}
	for _, id := range plan.Update {
		updateFiles = append(updateFiles, fileByID[id])
	}
	var removeIDs []string // SharePoint file IDs of deleted items
	for _, it := range items {
		if it.Deleted && it.ID != "" {
			removeIDs = append(removeIDs, it.ID)
		}
	}

	// Listener mode: fold in previously-failed items and resolve any file that
	// now lands in more than one action list.
	if opts.Retry != nil {
		addFiles, updateFiles, removeIDs = opts.Retry.merge(gc, driveID, addFiles, updateFiles, removeIDs)
	}

	for _, fid := range removeIDs {
		uuid := memid.FromFileID(fid)
		err := gm.Memories().Delete(ctx, uuid)
		ok := err == nil || isNotFound(err)
		if ok {
			res.Deleted++
			opts.emit(SyncEvent{FileID: fid, MemoryID: uuid, SpaceID: spaceID, Op: "delete", Status: "success"})
		} else {
			res.Errors = append(res.Errors, fmt.Sprintf("delete %s: %v", fid, err))
			opts.emit(SyncEvent{FileID: fid, MemoryID: uuid, SpaceID: spaceID, Op: "delete", Status: "failure", Message: err.Error()})
		}
		opts.Retry.recordRemove(fid, ok)
	}
	for _, f := range addFiles {
		opts.Retry.recordAdd(f.ID, res.ingest(ctx, gc, gm, spaceID, f, false, opts))
	}
	for _, f := range updateFiles {
		opts.Retry.recordUpdate(f.ID, res.ingest(ctx, gc, gm, spaceID, f, true, opts))
	}
	return newLink, res, nil
}

// goodmemStoredModified returns, for each memory UUID that exists in Goodmem,
// the SharePoint modified_datetime stored in its metadata (present key = exists;
// value may be "" if the memory lacks that metadata).
func goodmemStoredModified(ctx context.Context, gm *goodmem.Client, uuids []string) (map[string]string, error) {
	stored := make(map[string]string, len(uuids))
	for _, u := range uuids {
		m, err := gm.Memories().Get(ctx, u, nil)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		v := ""
		if m.Metadata != nil {
			if s, ok := m.Metadata["modified_datetime"].(string); ok {
				v = s
			}
		}
		stored[u] = v
	}
	return stored, nil
}

func isNotFound(err error) bool {
	var nf *goodmem.NotFoundError
	return errors.As(err, &nf)
}
