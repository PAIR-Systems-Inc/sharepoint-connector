package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
)

// Enricher computes extra metadata for a file from its content, so a memory can
// be born with the fields a caller wants to filter on rather than only the ones
// the provider knows (path, size, modified time).
//
// WHY THIS IS A SEAM AND NOT A FEATURE
// ====================================
// The connector copies BYTES; applications query FIELDS. Nothing in a generic
// connector can know that a form's "customer" lives in cell B7 — but without
// those fields a synced space supports semantic search only, and every metadata
// filter over it silently returns nothing.
//
// It has to happen HERE, before CreateMemory, because Goodmem has no
// UpdateMemory RPC: memories are immutable, so "attach the fields afterwards"
// means delete-and-recreate — a second embedding of every file and a window in
// which the memory exists un-enriched and filters quietly under-return.
//
// Enrich is nil for every caller that has not asked for it (cmd/connector leaves
// it unset unless ENRICH_URL is configured), which is the point: this is a func
// field like Sink, not a behavior. Nil means the file streams straight to
// Goodmem exactly as before, never buffered.
//
// The connector stays the only writer to the space. An Enricher is a pure
// function — it is handed bytes and returns metadata; it must not talk to
// Goodmem itself. That is what keeps the two-writer failure modes (no
// compare-and-swap, re-ingest loops, the un-enriched window) from existing at
// all.
//
// content is the whole file. Callers with an Enricher configured therefore
// buffer it in memory: set MaxFileBytes (SHAREPOINT_MAX_FILE_MB) to bound that.
type Enricher func(ctx context.Context, f source.FileInfo, content []byte) (map[string]any, error)

// reservedMetadataKeys are engine-owned: an Enricher's values for these are
// dropped rather than merged.
//
// modified_datetime is not cosmetic — DiffFull reads it back out of Goodmem to
// decide what changed, so an extractor that overwrote it would corrupt sync
// state (a "newer" stored timestamp makes the engine skip real updates).
// enrich_version is stamped by the engine only on a SUCCESSFUL enrichment,
// which is what lets a failed one be retried by the next full sync.
var reservedMetadataKeys = map[string]bool{
	"modified_datetime": true,
	"enrich_version":    true,
}

// EnrichContext is the JSON "context" part of an enrichment request: everything
// the connector knows about the file, so the service does not have to re-derive
// it from the bytes.
type EnrichContext struct {
	FileID           string `json:"file_id"`
	Name             string `json:"name"`
	Path             string `json:"path"`
	Mime             string `json:"mime"`
	Size             int64  `json:"size"`
	ModifiedDateTime string `json:"modified_datetime"`
	// SHA256 of the exact bytes in the "file" part — a cache key, so a service
	// doing expensive extraction (an LLM call) can skip work it has already done.
	SHA256   string         `json:"sha256"`
	Metadata map[string]any `json:"metadata"`
}

// enrichResponse is the reply envelope. The envelope (rather than a bare
// metadata object) exists so the contract can grow — a future "skip" or
// "content_type" field would not change the shape of what is already deployed.
type enrichResponse struct {
	Metadata map[string]any `json:"metadata"`
}

// HTTPEnricher returns an Enricher that POSTs each file to url as
// multipart/form-data and reads {"metadata": {...}} back:
//
//	POST url                       multipart/form-data
//	  part "context"  application/json   -> EnrichContext
//	  part "file"     <mime>             -> the raw bytes, filename = f.Name
//	  200 {"metadata": {...}}            -> merged into the memory's metadata
//
// Any non-2xx, an unreadable body, or a non-object "metadata" is an error; what
// the engine does with that error is the caller's choice (Options.EnrichRequired).
//
// The service on the other end is NOT built from this repo: it needs no Goodmem
// client, no source client and no sync logic. It is an extractor behind an HTTP
// handler. Run it on the same host and point ENRICH_URL at 127.0.0.1 so document
// bytes never leave the network they are already on.
func HTTPEnricher(url string, timeout time.Duration) Enricher {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context, f source.FileInfo, content []byte) (map[string]any, error) {
		body, contentType, err := enrichRequestBody(f, content)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("enrich %s: %w", f.Name, err)
		}
		defer resp.Body.Close()
		// Cap the reply: a misconfigured URL pointing at something that is not an
		// enrichment service should fail, not stream into memory.
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("enrich %s: read response: %w", f.Name, err)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("enrich %s: HTTP %d: %s", f.Name, resp.StatusCode, snippet(raw))
		}
		var out enrichResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("enrich %s: decode response: %w (%s)", f.Name, err, snippet(raw))
		}
		return out.Metadata, nil
	}
}

// enrichRequestBody builds the multipart body and its Content-Type header.
func enrichRequestBody(f source.FileInfo, content []byte) (io.Reader, string, error) {
	sum := sha256.Sum256(content)
	ctxPart := EnrichContext{
		FileID:           f.ID,
		Name:             f.Name,
		Path:             f.RelativePath,
		Mime:             f.MimeType,
		Size:             f.Size,
		ModifiedDateTime: f.ModifiedDateTime,
		SHA256:           hex.EncodeToString(sum[:]),
		Metadata:         f.Metadata,
	}
	ctxJSON, err := json.Marshal(ctxPart)
	if err != nil {
		return nil, "", fmt.Errorf("enrich %s: encode context: %w", f.Name, err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="context"`)
	h.Set("Content-Type", "application/json")
	w, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := w.Write(ctxJSON); err != nil {
		return nil, "", err
	}

	name := f.Name
	if name == "" {
		name = "upload"
	}
	mime := f.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}
	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, name))
	fh.Set("Content-Type", mime)
	fw, err := mw.CreatePart(fh)
	if err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(content); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

// snippet trims a response body down to something safe to put in an error.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
