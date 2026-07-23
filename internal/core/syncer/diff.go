// Package syncer computes and applies the source → Goodmem sync plan.
// (Named "syncer" rather than "sync" to avoid shadowing the stdlib sync package.)
package syncer

import (
	"sort"
	"strings"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/memid"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// Plan is the outcome of a diff: what to add, update, and delete.
type Plan struct {
	Add             []string // source file IDs to ingest
	Update          []string // file IDs to re-ingest (delete-then-add)
	Delete          []string // Goodmem memory UUIDs to delete
	UnexpectedNewer []string // file IDs whose stored Goodmem timestamp is newer than the source (skipped; should not happen)
}

// DiffFull computes the full-sync plan:
//
//   - Add    = source UUIDs not present in Goodmem.
//   - Delete = Goodmem UUIDs not present at the source.
//   - Update = UUIDs in both whose stored Goodmem modified_datetime is older
//     than the current source modified_datetime. A missing timestamp on either
//     side forces an update; a stored timestamp that is *newer* than the source
//     is an anomaly — skipped and reported in UnexpectedNewer.
//
// gmStoredModified maps a memory UUID (present in both sets) to the source
// modified_datetime stored in that memory's metadata at ingest. Timestamps are
// compared as strings (ISO-8601 sorts chronologically).
//
// MIME filtering is intentionally separate (see IsMimeSupported): the returned
// Add/Update may include unsupported types for the caller to drop at ingest.
func DiffFull(srcFiles []source.FileInfo, gmMemoryIDs []string, gmStoredModified map[string]string) Plan {
	srcByUUID := make(map[string]source.FileInfo, len(srcFiles))
	srcUUIDs := make(map[string]struct{}, len(srcFiles))
	for _, f := range srcFiles {
		u := memid.FromFileID(f.ID)
		srcByUUID[u] = f
		srcUUIDs[u] = struct{}{}
	}
	gmUUIDs := make(map[string]struct{}, len(gmMemoryIDs))
	for _, id := range gmMemoryIDs {
		gmUUIDs[id] = struct{}{}
	}

	var p Plan
	for u, f := range srcByUUID {
		if _, inGoodmem := gmUUIDs[u]; inGoodmem {
			update, newer := classify(gmStoredModified[u], f.ModifiedDateTime)
			switch {
			case update:
				p.Update = append(p.Update, f.ID)
			case newer:
				p.UnexpectedNewer = append(p.UnexpectedNewer, f.ID)
			}
		} else {
			p.Add = append(p.Add, f.ID)
		}
	}
	for id := range gmUUIDs {
		if _, inSource := srcUUIDs[id]; !inSource {
			p.Delete = append(p.Delete, id)
		}
	}

	// Deterministic output (apply order within a list does not affect correctness).
	sort.Strings(p.Add)
	sort.Strings(p.Update)
	sort.Strings(p.Delete)
	sort.Strings(p.UnexpectedNewer)
	return p
}

// classify decides, for a file present in both sets, whether it needs an update
// (stored older, or either timestamp missing), is an anomaly (stored newer), or
// is unchanged. Comparison is lexicographic on the ISO-8601 strings.
func classify(gmModified, srcModified string) (update, newer bool) {
	if gmModified == "" || srcModified == "" {
		return true, false
	}
	switch {
	case gmModified < srcModified:
		return true, false
	case gmModified > srcModified:
		return false, true
	default:
		return false, false
	}
}

// DiffDelta classifies incremental changes into a Plan:
//
//   - Deleted items     → Delete (the item's memory UUID; deletion is idempotent).
//   - Non-deleted files → Add when the memory is absent from Goodmem, else Update.
//
// gmStoredModified maps a candidate memory UUID to the source modified_datetime
// stored in its metadata (from a batch-get); a present key means the memory
// exists. Folders and non-file items are ignored. Callers should pre-filter
// unsupported MIME types (see IsMimeSupported). A present file whose stored
// timestamp is not older than the change timestamp is still updated but flagged
// in UnexpectedNewer.
func DiffDelta(changes []source.Change, gmStoredModified map[string]string) Plan {
	var p Plan
	for _, it := range changes {
		if it.ID == "" {
			continue
		}
		if it.Deleted {
			p.Delete = append(p.Delete, memid.FromFileID(it.ID))
			continue
		}
		if !it.IsFile {
			continue
		}
		u := memid.FromFileID(it.ID)
		if stored, exists := gmStoredModified[u]; exists {
			p.Update = append(p.Update, it.ID)
			if s := it.File.ModifiedDateTime; stored != "" && s != "" && stored >= s {
				p.UnexpectedNewer = append(p.UnexpectedNewer, it.ID)
			}
		} else {
			p.Add = append(p.Add, it.ID)
		}
	}
	sort.Strings(p.Add)
	sort.Strings(p.Update)
	sort.Strings(p.Delete)
	sort.Strings(p.UnexpectedNewer)
	return p
}

// IsMimeSupported reports whether Goodmem's content extractor supports mimeType.
func IsMimeSupported(mimeType string) bool {
	if mimeType == "" {
		return false
	}
	m := strings.ToLower(mimeType)
	if strings.HasPrefix(m, "text/") {
		return true
	}
	switch m {
	case "application/pdf",
		"application/rtf",
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return true
	}
	if strings.Contains(m, "+xml") {
		return true
	}
	if strings.Contains(m, "json") {
		return true
	}
	return false
}
