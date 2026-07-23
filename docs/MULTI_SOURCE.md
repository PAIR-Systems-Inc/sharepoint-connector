# Multi-source plan: SharePoint + Google Drive → Goodmem

Turn this single-source SharePoint connector into a **multi-provider** connector
(module `goodmem-connectors`): a shared sync/observability **core** plus swappable
source **providers** (SharePoint today, Google Drive next), all shipping as **one
binary**. Goodmem is always the destination.

## Decisions (locked)

| Decision | Choice |
|---|---|
| Module name | `github.com/PAIR-Systems-Inc/goodmem-connectors` |
| Layout | one shared `core/`, one folder per provider under `providers/` |
| Binary | **one** — provider chosen by config/flag (`--source`) |
| gdrive auth | **simplest: service account + Shared Drive** (share the drive with the SA's `client_email`); document the alternatives (domain-wide delegation / per-user OAuth) but don't build them yet |

## Why this is a small conceptual leap

Both sources expose the same three primitives the engine needs — full listing, an
incremental cursor, and push webhooks with expiry+renewal:

| Capability | SharePoint (MS Graph) | Google Drive |
|---|---|---|
| Full listing | `drives/{id}/root/children` (recursive) | `files.list` with parent `q` |
| Incremental cursor | delta token (`/delta`) | `changes.getStartPageToken` + `changes.list` → `newStartPageToken` |
| Push webhook | subscription (`/subscriptions`) | channel (`changes.watch`) |
| Webhook renewal | PATCH `expirationDateTime` | **re-`watch` (new id) + `channels.stop`** — no in-place renew |
| Webhook secret | `clientState` (echoed in body) | `token` (echoed as `X-Goog-Channel-Token` header) |
| Notification payload | list of changed resources | **header-only ping** (`X-Goog-Resource-State: sync\|change`) → then pull `changes.list` |
| Download bytes | `@microsoft.graph.downloadUrl` | `files.get?alt=media` (binary) / `files.export` (Google-native) |
| Stable file ID | item id | file id |
| Change timestamp / hash | `lastModifiedDateTime` / sha1 | `modifiedTime` / `md5Checksum` (binary only) |
| Deletion signal | delta item `deleted` facet | change `removed=true` or `file.trashed=true` |

The webhook → pull-delta → **coalesce** → apply loop is identical, so everything in
`core/` is reused unchanged, including all of amin3141's hardening (mass-delete
guard, dead-letter, size cap, coalescing, retention, `/readyz`, `slog`, alerts).

## The abstraction: a `Source` interface

New package `internal/core/source` holds **neutral types** (no Graph/Google leakage)
and the interface both providers implement:

```go
package source

// FileInfo is a provider-neutral file (replaces graph.FileInfo in the engine).
type FileInfo struct {
    ID, Name, MimeType   string
    ModifiedDateTime     string // ISO-8601, string-comparable
    Size                 int64
    Hash                 string // sha1 (SharePoint) / md5 (Drive); "" if native
    RelativePath         string
    DownloadHint         string // downloadURL (SharePoint) / "" (Drive resolves at Open)
    Native               bool   // Google-native doc needing export; false for SharePoint
}

// Change is one incremental change (replaces graph.Item in the delta path).
type Change struct {
    ID      string
    Deleted bool
    IsFile  bool
    File    FileInfo // valid when IsFile && !Deleted
}

type Subscription struct{ ID, Expiration string }

var ErrCursorExpired = errors.New("incremental cursor expired; full sync required")

// Source is one content source (SharePoint site, Google Shared Drive, …).
type Source interface {
    ListFiles(ctx context.Context) ([]FileInfo, error)                 // full sync
    LatestCursor(ctx context.Context) (string, error)                 // "now" bootstrap
    Delta(ctx context.Context, cursor string) ([]Change, string, error) // ErrCursorExpired on invalid
    GetFile(ctx context.Context, id string) (*FileInfo, error)        // pending re-fetch
    Open(ctx context.Context, f FileInfo) (io.ReadCloser, error)      // download OR export
    EnsureSubscription(ctx context.Context, notifyURL, secret string, ttl time.Duration) (Subscription, error)
    ValidateWebhook(r *http.Request, body []byte) (mine, changed bool) // provider-specific auth
    Label() string                                                    // "sharepoint" / "gdrive" (metrics/logs)
}
```

`internal/core/syncer` and `internal/core/server` depend only on `source`, never on
a provider package. `Open` returning an `io.ReadCloser` (not `[]byte`) also sets up
the streaming-ingest follow-up amin flagged, and lets Drive `Open` transparently
choose export-vs-download.

## Google Drive provider specifics

- **Auth (chosen):** a **service account** JSON key; add its `client_email` as a
  member of the target **Shared Drive**. No domain-wide delegation. All calls pass
  `supportsAllDrives=true` / `includeItemsFromAllDrives=true` and scope to `driveId`.
  Library: `google.golang.org/api/drive/v3` + `golang.org/x/oauth2/google`
  (`JWTConfigFromJSON`). *Alternatives (documented, not built):* domain-wide
  delegation to impersonate a user's My Drive, or per-user OAuth.
- **Listing / changes:** `files.list` (scoped to `driveId`) for full sync;
  `changes.getStartPageToken` → `changes.list` for the cursor. A change carries
  `fileId`, `removed`, and `file.trashed` → map both to a `Change{Deleted:true}`.
- **Webhook:** `changes.watch` with `{id: uuid, type: "web_hook", address: <https>,
  token: <secret>, expiration: <=7d}`. Notifications are **header-only**; validate
  by comparing `X-Goog-Channel-Token` to our secret (the `clientState` analog) and
  treat `X-Goog-Resource-State: sync` as the startup handshake (ack, no pull).
  Renewal = a fresh `changes.watch` before expiry, then `channels.stop` on the old
  channel (store channel `id`+`resourceId` in durable state to stop it).
- **Download vs export:** binary → `files.get?alt=media`; Google-native
  (`application/vnd.google-apps.{document,spreadsheet,presentation}`) → `files.export`
  to `{docx, xlsx, pptx}` (fall back to `pdf`). **Export is capped at 10 MB** — over
  that, skip-with-event (the existing `skipped`/size-cap path).
- **Ops wrinkle to confirm at build time:** whether the webhook `address` domain must
  be registered/verified (Search Console / GCP) for Drive push, or just valid HTTPS.

Export MIME map (Google-native → export target):

| Native source | Export to |
|---|---|
| `…google-apps.document` | `…wordprocessingml.document` (.docx), else `application/pdf` |
| `…google-apps.spreadsheet` | `…spreadsheetml.sheet` (.xlsx), else `application/pdf` |
| `…google-apps.presentation` | `…presentationml.presentation` (.pptx), else `application/pdf` |

## Target repo layout

```
goodmem-connectors/                 # module renamed
├── cmd/connector/                  # one binary; --source selects the provider
├── internal/
│   ├── core/                       # SHARED — provider-agnostic
│   │   ├── source/                 # NEW: Source interface + neutral types
│   │   ├── syncer/  server/  store/  gm/  memid/  config/  fakes/
│   └── providers/
│       ├── sharepoint/             # was internal/graph (package graph → sharepoint)
│       └── gdrive/                 # NEW
├── deploy/  docs/  Dockerfile  …
```

## Config & provider selection

- `SOURCE=sharepoint|gdrive` (or `--source`), defaulting by which credentials are set.
- Keep the existing `AZURE_*` / `SHAREPOINT_*` group; add a `GDRIVE_*` group
  (`GDRIVE_SA_JSON` or path, `GDRIVE_DRIVE_ID`). All the cross-cutting knobs
  (`SHAREPOINT_MAX_FILE_MB` → rename generically to a shared `MAX_FILE_MB`, plus
  `GRAPH_MAX_*` → shared `SYNC_*` where they aren't Graph-specific) move to core.
- The `.env.example` grows a "Google Drive" section; the docs↔code drift test keeps
  it honest.

## Productionization parity

Inherited for free by gdrive: mass-delete guard, dead-letter, size cap, coalescing,
retention, `/metrics` (+ `sharepoint_*` renamed to a neutral `connector_*` prefix
with a `source` label), `/readyz`, `slog`, and `deploy/alerts.yml`. gdrive-specific
additions: the export-format policy, faster channel renewal (≤7d), service-account
key as a new secret class, `channels.stop` cleanup, and Shared-Drive scoping docs.

## Phased plan

1. **Module rename** → `goodmem-connectors` (mechanical; tests stay green). ← *this PR*
2. **Restructure** → `internal/core/*` + `internal/providers/sharepoint` (package
   `graph`→`sharepoint`); no behavior change. ← *this PR*
3. **Extract `source.Source` + neutral types**; retype `syncer`/`server` off the
   provider; SharePoint becomes adapter #1. Rename metrics to `connector_*` + `source`
   label. Green throughout.
4. **gdrive provider** against the interface + in-process fakes (mirror `core/fakes`).
5. **CLI/config** source selection + per-provider validation; `.env.example` + drift test.
6. **Productionize gdrive**: export policy, channel renewal/stop, docs, one live pass.

Steps 1–2 are pure refactors worth doing regardless; 3 is the real design work; 4–6
deliver Google Drive.

## Sources

- [Retrieve changes (Changes API)](https://developers.google.com/workspace/drive/api/guides/manage-changes)
- [changes.getStartPageToken](https://developers.google.com/workspace/drive/api/reference/rest/v3/changes/getStartPageToken)
- [Notifications for resource changes (push / watch)](https://developers.google.com/workspace/drive/api/guides/push)
- [Export MIME types for Google Workspace documents](https://developers.google.com/workspace/drive/api/guides/ref-export-formats)
- [files.export (10 MB limit)](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/export)
- [drive/v3 Go package](https://pkg.go.dev/google.golang.org/api/drive/v3)
