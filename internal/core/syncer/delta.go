package syncer

import (
	"context"
	"errors"
	"fmt"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/memid"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// RunDelta applies one incremental batch from the source to Goodmem and returns
// the next cursor. Deleted items are removed; changed files are added or updated
// (via the diff's existence check). On an expired cursor it returns
// source.ErrCursorExpired, so the caller runs a full sync and re-bootstraps.
func RunDelta(ctx context.Context, src source.Source, gm *goodmem.Client, spaceID, cursor string, opts Options) (next string, res *Result, err error) {
	changes, next, err := src.Delta(ctx, cursor)
	if err != nil {
		return "", nil, err // includes source.ErrCursorExpired
	}

	// Index the changed (non-deleted) files; the provider returns them ready to
	// ingest (download ref + metadata already resolved).
	fileByID := make(map[string]source.FileInfo)
	var candidateUUIDs []string
	for _, it := range changes {
		if it.Deleted || !it.IsFile {
			continue
		}
		f := it.File
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

	plan := DiffDelta(changes, stored)
	res = &Result{Plan: plan}

	// Assemble the work as file infos / IDs so the durable pending sets (and
	// their re-fetched source state) can be merged in before applying.
	var addFiles, updateFiles []source.FileInfo
	for _, id := range plan.Add {
		addFiles = append(addFiles, fileByID[id])
	}
	for _, id := range plan.Update {
		updateFiles = append(updateFiles, fileByID[id])
	}
	var removeIDs []string // source file IDs of deleted items
	for _, it := range changes {
		if it.Deleted && it.ID != "" {
			removeIDs = append(removeIDs, it.ID)
		}
	}

	// Listener mode: fold in previously-failed items and resolve any file that
	// now lands in more than one action list.
	if opts.Retry != nil {
		addFiles, updateFiles, removeIDs = opts.Retry.merge(ctx, src, addFiles, updateFiles, removeIDs)
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
		opts.Retry.recordAdd(f.ID, res.ingest(ctx, src, gm, spaceID, f, false, opts))
	}
	for _, f := range updateFiles {
		opts.Retry.recordUpdate(f.ID, res.ingest(ctx, src, gm, spaceID, f, true, opts))
	}
	return next, res, nil
}

// goodmemStoredModified returns, for each memory UUID that exists in Goodmem,
// the source modified_datetime stored in its metadata (present key = exists;
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
