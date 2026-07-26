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

## Operational constraints

> ⚠️ **One Goodmem space per source.** Do **not** point a SharePoint listener and a
> Google Drive listener at the **same** `GOODMEM_SPACE_ID`. Each source's full sync
> reconciles the space against *its own* file set and deletes everything else as
> orphaned — so two sources sharing a space form a standing wipe loop: each full
> sync deletes the other source's memories, which the other side then re-adds, over
> and over (billing re-embeds each cycle). The `GRAPH_MAX_DELETE_RATIO` guard only
> trips above its threshold (default 50%), so it won't reliably catch this.
>
> The defaults already prevent it: with `GOODMEM_SPACE_ID` unset, each source
> creates its own space (`SharePoint_<org>_<site>` vs `GDrive_<driveId>`). The trap
> only appears if you set an explicit shared space id. Give each source its own.
> (Memory ids are also namespaced per source — `sharepoint.file.id` vs
> `gdrive.file.id` — so a SharePoint and a Drive file that happen to share an id
> never collide; but that does **not** rescue a shared space from the delete loop.)
>
> Longer-term (not this PR): the gdrive adapter already stamps `source: "gdrive"`
> into memory metadata; stamping the SharePoint side too and filtering orphan
> deletion to memories whose `source` matches would make a shared space safe. Until
> then, one space per source is a hard rule.

## Productionization parity

Inherited for free by gdrive: mass-delete guard, dead-letter, size cap, coalescing,
retention, `/metrics` (+ `sharepoint_*` renamed to a neutral `connector_*` prefix
with a `source` label), `/readyz`, `slog`, and `deploy/alerts.yml`. gdrive-specific
additions: the export-format policy, faster channel renewal (≤7d), service-account
key as a new secret class, `channels.stop` cleanup, and Shared-Drive scoping docs.

## Phased plan

1. ✅ **Module rename** → `goodmem-connectors` (mechanical; tests green).
2. ✅ **Restructure** → `internal/core/*` + `internal/providers/sharepoint` (package
   `graph`→`sharepoint`); no behavior change.
3. ✅ **Extract `source.Source` + neutral types** (`internal/core/source`); retyped
   `syncer`/`server` off the provider; SharePoint is now an `Adapter` implementing the
   interface (data plane + webhook validation + subscription + throttle). Green throughout.
4. ✅ **gdrive provider** (`internal/providers/gdrive`) implementing `source.Source`
   via the official Drive SDK (`google.golang.org/api/drive/v3`, service-account
   auth) + a fake Drive server; export-vs-download policy, Changes-API cursor,
   `changes.watch`/`channels.stop`, header-token webhook validation. Green under `-race`.
5. ✅ **CLI/config** source selection (`--source` flag / `SOURCE` env) + `GDRIVE_*`
   config, source-aware `ValidateSync`, per-source space naming; `.env.example` +
   drift test + a validation test. Green.
6. **Productionize gdrive**: docs (Shared-Drive setup, auth). ✅ **Live pass done** —
   a real Shared Drive synced end-to-end into Goodmem via impersonated ADC: `.pdf`,
   `.xlsx`, and `.doc` all reached `COMPLETED` with correct content types and
   `source=gdrive`, under the new `gdrive.file.id` namespace. **Poll mode is the
   gdrive default** (`SYNC_POLL_MINUTES`,
   default 2) — a timer-driven delta sync that needs **no public webhook**, so
   Google's domain-verified-endpoint requirement for `changes.watch` is no longer a
   deploy blocker; push mode stays available for anyone who owns a verifiable domain
   and wants lower latency. Unattended production auth options + limits (ADC
   impersonation is interactive; SA keys are org-blocked; workload identity
   federation off-GCP) are documented in [`docs/gcloud.md`](gcloud.md#d-unattended-production-auth-deploys).
   **Review fixes landed** (from the
   gdrive PR review): per-source memory-id namespace (`gdrive.file.id`, so Drive
   memories aren't minted in the SharePoint namespace); renewal cadence honors the
   lifetime Drive actually grants (not the requested TTL); folder-trash triggers a
   full reconcile so orphaned descendants are removed promptly; push-channel id +
   resourceId persisted next to the delta cursor so a restart stops the old channel
   instead of leaking it; `create-subscription` is source-aware; native-doc exports
   over Drive's 10 MB limit are a permanent skip, not dead-letter churn; and the
   "one space per source" constraint above is documented.

Deferred (do alongside gdrive, not blocking): rename the `sharepoint_*` metrics to a
neutral `connector_*` prefix with a `source` label; generalize the `GRAPH_*`/
`SHAREPOINT_*` env knobs that aren't provider-specific; stream `Open` straight into
`CreateFromReader` (the interface already returns an `io.ReadCloser`).

Steps 1–3 were pure refactors of the existing SharePoint connector — the engine is now
provider-neutral, so step 4 is additive (a new folder) rather than invasive.

## Sources

- [Retrieve changes (Changes API)](https://developers.google.com/workspace/drive/api/guides/manage-changes)
- [changes.getStartPageToken](https://developers.google.com/workspace/drive/api/reference/rest/v3/changes/getStartPageToken)
- [Notifications for resource changes (push / watch)](https://developers.google.com/workspace/drive/api/guides/push)
- [Export MIME types for Google Workspace documents](https://developers.google.com/workspace/drive/api/guides/ref-export-formats)
- [files.export (10 MB limit)](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/export)
- [drive/v3 Go package](https://pkg.go.dev/google.golang.org/api/drive/v3)
