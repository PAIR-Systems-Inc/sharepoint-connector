package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"fury.io/pairsys/goodmem"
	gmodels "fury.io/pairsys/goodmem/models"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/graph"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/memid"
)

// Result summarizes a full sync run.
type Result struct {
	SharePointFiles int
	GoodmemMemories int
	Plan            Plan
	Added           int
	Updated         int
	Deleted         int
	Skipped         int      // unsupported MIME / no download URL
	Errors          []string // non-fatal per-item failures
}

// RunFull performs a one-shot full sync from a SharePoint site to a Goodmem
// space (the sync-once path). When dryRun is true it computes and returns the
// plan without mutating Goodmem.
func RunFull(ctx context.Context, gc *graph.Client, gm *goodmem.Client, spaceID string, dryRun bool) (*Result, error) {
	siteID, err := gc.GetSiteID()
	if err != nil {
		return nil, fmt.Errorf("resolve site: %w", err)
	}
	files, err := gc.ListFiles("", "", true, siteID)
	if err != nil {
		return nil, fmt.Errorf("list SharePoint files: %w", err)
	}

	memIDs, stored, err := listGoodmemMemories(ctx, gm, spaceID)
	if err != nil {
		return nil, fmt.Errorf("list Goodmem memories: %w", err)
	}

	plan := DiffFull(files, memIDs, stored)
	res := &Result{SharePointFiles: len(files), GoodmemMemories: len(memIDs), Plan: plan}
	if dryRun {
		return res, nil
	}

	byID := make(map[string]graph.FileInfo, len(files))
	for _, f := range files {
		byID[f.ID] = f
	}

	// Delete orphaned memories (no longer in SharePoint).
	for _, uuid := range plan.Delete {
		if err := gm.Memories().Delete(ctx, uuid); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("delete %s: %v", uuid, err))
			continue
		}
		res.Deleted++
	}

	// Ingest adds and updates (update = delete-then-create with the same memoryId).
	for _, id := range plan.Add {
		res.ingest(ctx, gc, gm, spaceID, byID[id], false)
	}
	for _, id := range plan.Update {
		res.ingest(ctx, gc, gm, spaceID, byID[id], true)
	}
	return res, nil
}

// ingest downloads a file and (re-)creates its memory. Unsupported types and
// files without a download URL are skipped (mirroring sync_once.py).
func (res *Result) ingest(ctx context.Context, gc *graph.Client, gm *goodmem.Client, spaceID string, f graph.FileInfo, isUpdate bool) {
	if !IsMimeSupported(f.MimeType) || f.DownloadURL == "" {
		res.Skipped++
		return
	}
	uuid := memid.FromFileID(f.ID)
	if isUpdate {
		if err := gm.Memories().Delete(ctx, uuid); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("pre-update delete %s: %v", f.Name, err))
			return
		}
	}
	content, err := gc.Download(f.DownloadURL)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("download %s: %v", f.Name, err))
		return
	}
	if err := ingestFile(ctx, gm, spaceID, f, content); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("ingest %s: %v", f.Name, err))
		return
	}
	if isUpdate {
		res.Updated++
	} else {
		res.Added++
	}
}

// listGoodmemMemories returns the memory ids in a space and a map of memory id →
// the SharePoint modified_datetime stored in its metadata (for the diff).
func listGoodmemMemories(ctx context.Context, gm *goodmem.Client, spaceID string) (ids []string, stored map[string]string, err error) {
	stored = make(map[string]string)
	page, err := gm.Memories().List(ctx, spaceID, nil)
	if err != nil {
		return nil, nil, err
	}
	for m, err := range page.All(ctx) {
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, m.MemoryID)
		if m.Metadata != nil {
			if v, ok := m.Metadata["modified_datetime"].(string); ok {
				stored[m.MemoryID] = v
			}
		}
	}
	return ids, stored, nil
}

// ingestFile creates a Goodmem memory for f's content, using the deterministic
// memoryId and storing the file metadata (including modified_datetime).
func ingestFile(ctx context.Context, gm *goodmem.Client, spaceID string, f graph.FileInfo, content []byte) error {
	mime := f.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}
	uuid := memid.FromFileID(f.ID)
	filename := f.Name
	if filename == "" {
		filename = "upload"
	}
	req := &gmodels.JSONMemoryCreationRequest{
		SpaceID:     spaceID,
		MemoryID:    &uuid,
		ContentType: mime,
		Metadata:    fileMetadata(f),
	}
	_, err := gm.Memories().CreateFromReader(ctx, bytes.NewReader(content), filename, req)
	return err
}

// fileMetadata converts a FileInfo to the metadata map stored on the memory,
// dropping empty fields (mirroring the Python `{k: v for ... if v is not None}`).
func fileMetadata(f graph.FileInfo) map[string]any {
	b, _ := json.Marshal(f)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		} else if v == nil {
			delete(m, k)
		}
	}
	return m
}

// ResolveSpaceID returns the target Goodmem space id. Preference order:
// explicit spaceID; else a space named SharePoint_{Org}_{Site}. Creating a new
// space from an embedder is not yet ported.
func ResolveSpaceID(ctx context.Context, gm *goodmem.Client, spaceID, siteURL, embedderID string) (string, error) {
	if strings.TrimSpace(spaceID) != "" {
		return spaceID, nil
	}
	name := SpaceNameFromSiteURL(siteURL)
	page, err := gm.Spaces().List(ctx, &goodmem.SpacesListParams{NameFilter: &name})
	if err != nil {
		return "", err
	}
	for sp, err := range page.All(ctx) {
		if err != nil {
			return "", err
		}
		if sp.Name == name {
			return sp.SpaceID, nil
		}
	}
	if strings.TrimSpace(embedderID) != "" {
		return "", fmt.Errorf("space %q not found; embedder-based creation is not yet ported — create the space or set GOODMEM_SPACE_ID", name)
	}
	return "", fmt.Errorf("space %q not found; set GOODMEM_SPACE_ID", name)
}

// SpaceNameFromSiteURL derives the Goodmem space name from the SharePoint site
// URL: SharePoint_{org}_{site}, where org is the host before ".sharepoint.com"
// and site is the first path segment after "sites/". Ported from
// _space_name_from_site_url (case preserved, not title-cased).
func SpaceNameFromSiteURL(siteURL string) string {
	s := strings.TrimRight(siteURL, "/")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	host, path := s, ""
	if i := strings.Index(s, "/"); i >= 0 {
		host = s[:i]
		path = strings.Trim(s[i:], "/")
	}
	org := host
	if i := strings.Index(host, ".sharepoint.com"); i >= 0 {
		org = host[:i]
	}
	site := ""
	if strings.HasPrefix(strings.ToLower(path), "sites/") {
		rest := path[len("sites/"):]
		if j := strings.Index(rest, "/"); j >= 0 {
			site = rest[:j]
		} else {
			site = rest
		}
	}
	return fmt.Sprintf("SharePoint_%s_%s", org, site)
}
