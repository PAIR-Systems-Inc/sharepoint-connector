// Package syncer computes and applies the SharePoint → Goodmem sync plan.
// (Named "syncer" rather than "sync" to avoid shadowing the stdlib sync package.)
package syncer

import (
	"sort"
	"strings"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/graph"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/memid"
)

// Plan is the outcome of a diff: what to add, update, and delete.
type Plan struct {
	Add             []string // SharePoint file IDs to ingest
	Update          []string // file IDs to re-ingest (delete-then-add)
	Delete          []string // Goodmem memory UUIDs to delete
	UnexpectedNewer []string // file IDs whose stored Goodmem timestamp is newer than SharePoint (skipped; should not happen)
}

// DiffFull computes the full-sync plan, mirroring sync_once.py:
//
//   - Add    = SharePoint UUIDs not present in Goodmem.
//   - Delete = Goodmem UUIDs not present in SharePoint.
//   - Update = UUIDs in both whose stored Goodmem modified_datetime is older
//     than the current SharePoint modified_datetime. A missing timestamp on
//     either side forces an update; a stored timestamp that is *newer* than
//     SharePoint is an anomaly — skipped and reported in UnexpectedNewer.
//
// gmStoredModified maps a memory UUID (present in both sets) to the SharePoint
// modified_datetime stored in that memory's metadata at ingest. Timestamps are
// compared as strings (ISO-8601 sorts chronologically), matching the reference.
//
// MIME filtering is intentionally separate (see IsMimeSupported): the returned
// Add/Update may include unsupported types for the caller to drop at ingest.
func DiffFull(spFiles []graph.FileInfo, gmMemoryIDs []string, gmStoredModified map[string]string) Plan {
	spByUUID := make(map[string]graph.FileInfo, len(spFiles))
	spUUIDs := make(map[string]struct{}, len(spFiles))
	for _, f := range spFiles {
		u := memid.FromFileID(f.ID)
		spByUUID[u] = f
		spUUIDs[u] = struct{}{}
	}
	gmUUIDs := make(map[string]struct{}, len(gmMemoryIDs))
	for _, id := range gmMemoryIDs {
		gmUUIDs[id] = struct{}{}
	}

	var p Plan
	for u, f := range spByUUID {
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
		if _, inSharePoint := spUUIDs[id]; !inSharePoint {
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
// is unchanged. Comparison is lexicographic on the ISO-8601 strings, matching
// sync_once.py's `gm_modified < sp_modified`.
func classify(gmModified, spModified string) (update, newer bool) {
	if gmModified == "" || spModified == "" {
		return true, false
	}
	switch {
	case gmModified < spModified:
		return true, false
	case gmModified > spModified:
		return false, true
	default:
		return false, false
	}
}

// DiffDelta classifies Microsoft Graph delta items into a Plan, mirroring the
// listener's delta handling:
//
//   - Deleted items     → Delete (the item's memory UUID; deletion is idempotent).
//   - Non-deleted files → Add when the memory is absent from Goodmem, else Update.
//
// gmStoredModified maps a candidate memory UUID to the SharePoint
// modified_datetime stored in its metadata (from a batch-get); a present key
// means the memory exists. Folders and non-file items are ignored. Callers
// should pre-filter unsupported MIME types and items lacking a download URL
// (see IsMimeSupported). A present file whose stored timestamp is not older than
// the delta timestamp is still updated but flagged in UnexpectedNewer.
func DiffDelta(items []graph.Item, gmStoredModified map[string]string) Plan {
	var p Plan
	for _, it := range items {
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
			if sp := it.File.ModifiedDateTime; stored != "" && sp != "" && stored >= sp {
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
// Ported from _is_mime_type_supported in sync_once.py.
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
