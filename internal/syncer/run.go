package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"fury.io/pairsys/goodmem"
	gmodels "fury.io/pairsys/goodmem/models"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/graph"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/memid"
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

// Options carries cross-cutting sync settings shared by the full and delta paths.
type Options struct {
	FolderPath        string    // scope the full-sync listing to this folder; "" = whole drive
	ExtractPageImages bool      // hint Goodmem to extract page images (e.g. PDF page screenshots)
	DryRun            bool      // compute the plan without mutating Goodmem (full sync only)
	MaxFileBytes      int64     // skip files larger than this before downloading (0 = no cap)
	MaxDeleteRatio    float64   // refuse a full sync deleting > this fraction of memories (0 = disabled)
	Retry             *Retrier  // listener durable retry + status polling; nil = one-shot CLI (no pending sets, no polling)
	Sink              EventSink // per-item outcome sink for durable sync history; nil = discard
}

// emit sends a per-item sync outcome to the sink, if one is set.
func (o Options) emit(e SyncEvent) {
	if o.Sink != nil {
		o.Sink(e)
	}
}

// RunFull performs a one-shot full sync from a SharePoint site to a Goodmem
// space (the sync-once path). When opts.DryRun is true it computes and returns
// the plan without mutating Goodmem.
func RunFull(ctx context.Context, gc *graph.Client, gm *goodmem.Client, spaceID string, opts Options) (*Result, error) {
	siteID, err := gc.GetSiteID()
	if err != nil {
		return nil, fmt.Errorf("resolve site: %w", err)
	}
	files, err := gc.ListFiles("", opts.FolderPath, true, siteID)
	if err != nil {
		return nil, fmt.Errorf("list SharePoint files: %w", err)
	}

	memIDs, stored, err := listGoodmemMemories(ctx, gm, spaceID)
	if err != nil {
		return nil, fmt.Errorf("list Goodmem memories: %w", err)
	}

	plan := DiffFull(files, memIDs, stored)
	res := &Result{SharePointFiles: len(files), GoodmemMemories: len(memIDs), Plan: plan}
	if opts.DryRun {
		return res, nil
	}

	// Guard against a partial/failed listing wiping memories: refuse the apply if
	// SharePoint returned zero files while Goodmem is non-empty (a transient
	// Graph/auth failure), or if the plan would delete an implausibly large share
	// of the space (a listing that silently dropped a subtree). Either way the
	// next successful sync applies the real change.
	if reason := refuseMassDelete(len(files), len(memIDs), len(plan.Delete), opts.MaxDeleteRatio); reason != "" {
		return res, fmt.Errorf("refusing to apply full sync: %s", reason)
	}

	byID := make(map[string]graph.FileInfo, len(files))
	for _, f := range files {
		byID[f.ID] = f
	}

	// Delete orphaned memories (no longer in SharePoint).
	for _, uuid := range plan.Delete {
		if err := gm.Memories().Delete(ctx, uuid); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("delete %s: %v", uuid, err))
			opts.emit(SyncEvent{MemoryID: uuid, SpaceID: spaceID, Op: "delete", Status: "failure", Message: err.Error()})
			continue
		}
		res.Deleted++
		opts.emit(SyncEvent{MemoryID: uuid, SpaceID: spaceID, Op: "delete", Status: "success"})
	}

	// Ingest adds and updates (update = delete-then-create with the same memoryId).
	// In listener mode (opts.Retry != nil) the outcome updates the pending sets:
	// a success clears any stale pending entry, a failure queues a retry. The
	// full sync itself re-heals dropped adds/removes via the diff, so it does not
	// merge the pending sets (that happens in the delta path).
	for _, id := range plan.Add {
		opts.Retry.recordAdd(id, res.ingest(ctx, gc, gm, spaceID, byID[id], false, opts))
	}
	for _, id := range plan.Update {
		opts.Retry.recordUpdate(id, res.ingest(ctx, gc, gm, spaceID, byID[id], true, opts))
	}
	return res, nil
}

// massDeleteFloor is the number of deletes always allowed regardless of ratio,
// so small spaces (where a legitimate change can be a large fraction) are not
// blocked by the proportional guard.
const massDeleteFloor = 10

// refuseMassDelete returns a non-empty reason when a full-sync apply should be
// refused as a likely-transient anomaly rather than a genuine change:
//
//   - zero files listed while Goodmem is non-empty — a transient Graph/auth
//     failure that would otherwise delete every memory; or
//   - the plan would delete more than maxDeleteRatio of all memories (and more
//     than massDeleteFloor) — a listing that silently dropped a subtree.
//
// maxDeleteRatio <= 0 disables the proportional check. Set it to 1 (or higher)
// to allow a genuinely large deletion through.
func refuseMassDelete(nFiles, nMemories, nDelete int, maxDeleteRatio float64) string {
	if nFiles == 0 && nMemories > 0 {
		return fmt.Sprintf("SharePoint returned 0 files but Goodmem has %d memories (likely a transient Graph/auth failure); skipping to avoid deleting everything", nMemories)
	}
	if maxDeleteRatio > 0 && nDelete > massDeleteFloor && float64(nDelete) > maxDeleteRatio*float64(nMemories) {
		return fmt.Sprintf("plan would delete %d of %d memories (>%.0f%%); refusing as a likely partial listing — raise GRAPH_MAX_DELETE_RATIO (e.g. 1) to override", nDelete, nMemories, maxDeleteRatio*100)
	}
	return ""
}

// ingest downloads a file and (re-)creates its memory. Unsupported types and
// files without a download URL are skipped (mirroring sync_once.py).
func (res *Result) ingest(ctx context.Context, gc *graph.Client, gm *goodmem.Client, spaceID string, f graph.FileInfo, isUpdate bool, opts Options) ingestResult {
	op := "add"
	if isUpdate {
		op = "update"
	}
	uuid := memid.FromFileID(f.ID)
	emit := func(status, msg string) {
		opts.emit(SyncEvent{FileID: f.ID, FileName: f.Name, MemoryID: uuid, SpaceID: spaceID, Op: op, Status: status, Message: msg})
	}

	if !IsMimeSupported(f.MimeType) || f.DownloadURL == "" {
		res.Skipped++
		emit("skipped", "unsupported MIME or no download URL")
		return resSkipped
	}
	// Skip oversized files before downloading — Download buffers the whole file in
	// memory, so a multi-hundred-MB file would OOM a small VM. Recorded as skipped
	// (not queued for retry) so it does not loop forever in the pending set.
	if opts.MaxFileBytes > 0 && f.Size > opts.MaxFileBytes {
		res.Skipped++
		emit("skipped", fmt.Sprintf("file size %.1f MB exceeds cap %.1f MB", float64(f.Size)/1e6, float64(opts.MaxFileBytes)/1e6))
		return resSkipped
	}
	if isUpdate {
		// A 404 means the memory is already gone; treat it as an add and fall
		// through rather than aborting the update (matching listener.py).
		if err := gm.Memories().Delete(ctx, uuid); err != nil && !isNotFound(err) {
			res.Errors = append(res.Errors, fmt.Sprintf("pre-update delete %s: %v", f.Name, err))
			emit("failure", "pre-update delete: "+err.Error())
			return resTransient
		}
	}
	content, err := gc.Download(f.DownloadURL)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("download %s: %v", f.Name, err))
		emit("failure", "download: "+err.Error())
		return resTransient
	}
	mem, err := createMemory(ctx, gm, spaceID, f, content, opts.ExtractPageImages)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("ingest %s: %v", f.Name, err))
		emit("failure", "create: "+err.Error())
		return resTransient
	}

	// In listener mode, confirm Goodmem finished processing: a 200 create can
	// still end in FAILED. The one-shot CLI (opts.Retry == nil) treats a 200 as
	// done, matching sync_once.py.
	if opts.Retry != nil {
		status := strings.ToUpper(mem.ProcessingStatus)
		if status != "COMPLETED" && status != "FAILED" {
			memID := mem.MemoryID
			if memID == "" {
				memID = uuid
			}
			status = opts.Retry.pollStatus(ctx, gm, memID)
		}
		switch status {
		case "FAILED":
			res.Errors = append(res.Errors, fmt.Sprintf("ingest %s: Goodmem processing FAILED", f.Name))
			emit("failure", "Goodmem processing FAILED")
			return resFailedProcessing
		case "COMPLETED":
			// confirmed
		default: // still PENDING after the poll timeout
			res.Errors = append(res.Errors, fmt.Sprintf("ingest %s: Goodmem processing still pending (timeout)", f.Name))
			emit("failure", "Goodmem processing pending (timeout)")
			return resTransient
		}
	}

	if isUpdate {
		res.Updated++
	} else {
		res.Added++
	}
	emit("success", "")
	return resOK
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

// createMemory creates a Goodmem memory for f's content, using the deterministic
// memoryId and storing the file metadata (including modified_datetime). It
// returns the created memory so the caller can inspect its processingStatus.
func createMemory(ctx context.Context, gm *goodmem.Client, spaceID string, f graph.FileInfo, content []byte, extractPageImages bool) (*gmodels.Memory, error) {
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
	if extractPageImages {
		t := true
		req.ExtractPageImages = &t
	}
	return gm.Memories().CreateFromReader(ctx, bytes.NewReader(content), filename, req)
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

// ResolveSpaceID returns the target Goodmem space id: an explicit spaceID, else
// a space named SharePoint_{Org}_{Site}, creating it if absent — with an
// embedder that is the given one, else the first available, else a
// text-embedding-3-small embedder created from openaiKey.
func ResolveSpaceID(ctx context.Context, gm *goodmem.Client, spaceID, siteURL, embedderID, openaiKey string) (string, error) {
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

	// Not found — create it with a resolved embedder.
	embID, err := ensureEmbedder(ctx, gm, embedderID, openaiKey)
	if err != nil {
		return "", err
	}
	weight := 1.0
	sp, err := gm.Spaces().Create(ctx, &gmodels.SpaceCreationRequest{
		Name:           name,
		SpaceEmbedders: []gmodels.SpaceEmbedderConfig{{EmbedderID: embID, DefaultRetrievalWeight: &weight}},
		DefaultChunkingConfig: gmodels.ChunkingConfiguration{
			Recursive: &gmodels.RecursiveChunkingConfiguration{
				ChunkSize:         512,
				ChunkOverlap:      64,
				KeepStrategy:      gmodels.SeparatorKeepStrategy("KEEP_END"),
				LengthMeasurement: gmodels.LengthMeasurement("CHARACTER_COUNT"),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create space %q: %w", name, err)
	}
	return sp.SpaceID, nil
}

// ensureEmbedder returns an embedder id: the given one, else the first existing
// embedder, else a text-embedding-3-small OpenAI embedder created from openaiKey.
func ensureEmbedder(ctx context.Context, gm *goodmem.Client, embedderID, openaiKey string) (string, error) {
	if strings.TrimSpace(embedderID) != "" {
		return embedderID, nil
	}
	resp, err := gm.Embedders().List(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list embedders: %w", err)
	}
	if len(resp.Embedders) > 0 {
		return resp.Embedders[0].EmbedderID, nil
	}
	if strings.TrimSpace(openaiKey) == "" {
		return "", errors.New("no embedders available and no OPENAI_API_KEY to create one; set GOODMEM_EMBEDDER_ID or GOODMEM_SPACE_ID")
	}
	apiPath := "/embeddings"
	maxSeq := int32(8192)
	er, err := gm.Embedders().Create(ctx, &gmodels.EmbedderCreationRequest{
		DisplayName:         "text-embedding-3-small",
		ProviderType:        gmodels.ProviderType("OPENAI"),
		EndpointURL:         "https://api.openai.com/v1/",
		APIPath:             &apiPath,
		ModelIdentifier:     "text-embedding-3-small",
		Dimensionality:      1536,
		DistributionType:    gmodels.DistributionType("DENSE"),
		MaxSequenceLength:   &maxSeq,
		SupportedModalities: []gmodels.Modality{gmodels.Modality("TEXT")},
	}, openaiKey)
	if err != nil {
		return "", fmt.Errorf("create embedder: %w", err)
	}
	return er.EmbedderID, nil
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
