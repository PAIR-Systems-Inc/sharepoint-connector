# Technical details

Internals of the **`connector`** binary: the provider-neutral sync engine, the
three source providers (SharePoint, Google Drive and SMB/Windows network drives),
and how the file diff is computed and applied.

See also: [README.md](../README.md) (overview + quickstart) · [usage.md](usage.md)
(running, deploying, monitoring) · [testing.md](testing.md) (what is verified vs
merely supported) · [roadmap.md](roadmap.md) (what isn't built yet, and how we
got here).

## Architecture

One binary, one engine, one folder per source:

```
cmd/connector/            # subcommands: sync-once, serve, create-subscription, watch
internal/
├── core/                 # provider-agnostic — depends only on the Source interface
│   ├── source/           #   the Source interface + neutral types (the contract)
│   ├── syncer/           #   diff, apply, pending-retry, dead-letter, status polling
│   ├── server/           #   listener: webhook + poll loops, HTTP endpoints, metrics
│   ├── store/            #   SQLite sync history (behind GET /syncs)
│   ├── gm/               #   Goodmem SDK wrapper (the destination)
│   ├── config/           #   .env / environment loading
│   ├── memid/            #   deterministic memory IDs
│   └── fakes/            #   in-process fake servers for integration tests
└── providers/
    ├── sharepoint/       # Microsoft Graph client + Source adapter
    ├── googledrive/      # Google Drive v3 SDK client + Source adapter
    └── smb/              # SMB2/3 client (reached as an io/fs.FS) + Source adapter
```

The engine never imports a provider package. Adding a source means implementing
one interface — the sync logic, endpoints, retries, metrics and ops surface are
written once.

## Design decisions & why

Settled choices, kept so they are not silently re-litigated. Several are
**permanent** once a tenant is live.

| Decision | Choice and reasoning |
|---|---|
| Language | **Go**, one compiled binary — chiefly source protection: Python ships as readable source, a stripped binary does not. It also brought a production HTTP server, goroutines matching the webhook→worker model, static typing and a tiny interpreter-free image. *Honest caveat: a binary is a deterrent, not a vault — it still disassembles. Pair it with never embedding secrets, keeping sensitive IP server-side, and licensing terms.* |
| Module & layout | `github.com/PAIR-Systems-Inc/goodmem-connectors`; one shared `core/`, one folder per provider under `providers/` |
| Binary | **one**, provider chosen by `SOURCE` / `--source` |
| Graph client | hand-rolled REST rather than the official SDK — smaller and easier to audit |
| Provider naming | spelled out in full: `google-drive`, not `gdrive` — no legacy aliases |
| `.env` precedence | the real process environment wins over `.env` (the Python PoC was the reverse). Deliberate: it matches how Fly and container secrets work |
| Sync direction | **one-way**, source → Goodmem. The drive is the single source of truth and the space is a *mirror*, so a full sync deleting anything the source lacks is the reconcile working, not data loss. Nothing is ever written back |
| **Memory-id namespace** | per-source and **permanent**: `sharepoint.file.id`, `google-drive.file.id`, `smb.file.path`. Changing one re-keys every memory |
| Google Drive scope | a **Shared Drive**, read by a service account added as Viewer. My Drive (domain-wide delegation) is out of scope |
| Google Drive auth | **three** deploy-and-forget paths — service-account key, GCP-attached service account, workload identity federation — so the connector fits any customer IT policy |
| Google Drive trigger | **poll by default** — `changes.watch` needs a domain-verified HTTPS endpoint; polling the Changes API needs nothing public and is equally incremental |
| SMB naming | source token **`smb`**, not `windows-network-drive` — the same share may be served by Windows Server, Samba or a NAS, so naming it after Windows would be wrong more often than right |
| **SMB identity** | the **path** relative to `SMB_ROOT` — SMB has no stable file id. Not case-normalized (see [above](#smb-identity-and-its-consequences)). `SMB_ROOT` is therefore permanent too |
| SMB auth | **NTLM and Kerberos.** NTLM needs no infrastructure and covers standalone servers, workgroups and NAS; Kerberos covers domains that have disabled NTLM, which Microsoft is progressively making the default |
| SMB trigger | **poll only** — no change feed exists (see [above](#why-smb-polls)) |
| SMB library | `cloudsoda/go-smb2` — maintained, NTLM + Kerberos, and the fork rclone depends on. Hand-rolling SMB2 was rejected: unlike the Graph client (750 lines of HTTPS + JSON) it would mean owning NTLMv2, SMB3 signing and encryption, and credit-based flow control |
| State store | plain state files on a persistent volume, plus SQLite for sync history — no external datastore at single-tenant scale. Revisit only if HA / >1 machine becomes a goal |

## The `Source` interface

`internal/core/source` defines the contract and the neutral types (no Graph or
Google types leak into the engine):

| Method | Purpose |
|---|---|
| `Label()` | provider name for logs/metrics (`sharepoint`, `google-drive`, `smb`) |
| `MemNamespace()` | **permanent** namespace for deterministic memory ids |
| `ListFiles(ctx)` | every in-scope file — the full sync |
| `LatestCursor(ctx)` | a cursor positioned at "now" (bootstrap) |
| `Delta(ctx, cursor)` | changes since the cursor + the next cursor |
| `GetFile(ctx, id)` | one file's current state (pending-retry re-fetch) |
| `Open(ctx, f)` | the bytes — download **or** export |
| `EnsureSubscription(ctx, url, ttl)` | create/renew a push subscription |
| `ValidateWebhook(r, body)` | classify an incoming webhook |

Neutral types: `FileInfo` (id, name, effective MIME, modified time, size,
`DownloadRef`, provider `Metadata`), `Change` (id, deleted, is-file, file,
`ReconcileHint`), and `Subscription` (id, native expiration string, parsed
`ExpiresAt`).

Sentinel errors carry provider-independent meaning:

- **`ErrCursorExpired`** — the incremental cursor is stale (Graph 410 / Drive 410);
  the caller runs a full sync and re-bootstraps.
- **`ErrNotFound`** — the file is gone at the source (404).
- **`ErrSkip`** — the file can *never* be ingested (e.g. a Google-native doc whose
  export exceeds Drive's 10 MB cap), so it is recorded as a permanent skip instead
  of being retried forever.

Two optional capabilities are detected by type assertion: `ThrottleReporter`
(surface provider back-off in logs/metrics) and `WebhookValidator`.

## How the providers differ

Everything below the adapter is identical; these are the only real differences.

| Capability | SharePoint (MS Graph) | Google Drive | SMB (Windows network drive) |
|---|---|---|---|
| Full listing | `drives/{id}/root/children`, recursive | `files.list` scoped to `driveId` | recursive directory walk (`fs.WalkDir`) |
| Incremental cursor | delta token (`/delta`) | `changes.getStartPageToken` → `changes.list` → `newStartPageToken` | newest modification time seen (RFC-3339 watermark) |
| Default trigger | **push** (webhook subscription) | **poll** (`SYNC_POLL_MINUTES`, default 2) | **poll only** — no push exists |
| Push mechanism | subscription; PATCH to renew | channel (`changes.watch`); **no in-place renew** — re-watch + `channels.stop` | none |
| Push prerequisite | any public HTTPS URL | a **domain-verified** HTTPS endpoint | n/a |
| Webhook secret | `clientState` (in the body) | `token` (header `X-Goog-Channel-Token`) | n/a |
| Notification payload | list of changed resources | header-only ping → then pull `changes.list` | n/a |
| Download bytes | `@microsoft.graph.downloadUrl` | `files.get?alt=media`, or `files.export` for Google-native docs | open the file over SMB |
| Content type | supplied by Graph | supplied by Drive | **inferred from the extension** (SMB carries none) |
| Deletion signal | delta item `deleted` facet | `removed=true` or `file.trashed=true` | **none** — found by the periodic full sync |
| Rate-limit backoff | in the hand-rolled Graph client | in a retry transport under the SDK (the SDK itself never retries) | n/a (no server-side rate limit) |
| Authentication | Azure AD client credentials | service-account / ADC / workload identity | **NTLM or Kerberos** (SPNEGO-negotiated) |
| Memory-id namespace | `sharepoint.file.id` | `google-drive.file.id` | `smb.file.path` |
| Default space name | `SharePoint_<org>_<site>` | `GoogleDrive_<driveId>` | `SMB_<host>_<share>` |

### Why SMB polls

SharePoint and Drive both expose a change feed. SMB has neither a feed nor a
usable push mechanism, and the two candidates fail for different reasons:

- **SMB2 CHANGE_NOTIFY** is in the protocol and does work remotely, but it cannot
  stand alone. The server buffers change records against an open directory handle
  and, when they overflow the client's buffer, returns `STATUS_NOTIFY_ENUM_DIR` —
  "too many changes, re-enumerate yourself". So the protocol *requires* a rescan
  fallback, and it gives up precisely when the share is busiest. The watch also
  dies silently with its handle on any reconnect, support varies across Samba and
  NAS implementations, and no Go SMB library implements it (verified against both
  `cloudsoda/go-smb2` and `hirochachacha/go-smb2`).
- **The NTFS change journal (USN)** is not merely unreliable over the network — it
  is *unreachable*. It is read with `DeviceIoControl(FSCTL_READ_USN_JOURNAL)`
  against a **volume** handle, needing local access and administrator rights. SMB
  never exposes it. It would only be an option if we shipped an agent onto the
  file server itself, which is a different product shape.

Since a full walk is needed as the fallback in every design, it is the
foundation rather than the compromise: the walk finds modified files by
timestamp, and the periodic full sync reconciles deletions. Notification could
later reduce latency, but it could never replace the walk.

### SMB identity and its consequences

SMB gives a file no stable identifier, so the **path relative to `SMB_ROOT` is the
identity** (hence the namespace `smb.file.path`). Two consequences fall out of
that and are properties of the protocol, not bugs:

- **A rename or move is a delete plus an add.** The content is re-embedded under
  the new path.
- **`SMB_ROOT` is permanent.** Paths are stored relative to it, so changing it
  re-keys every memory.

Deliberately *not* case-normalized. Verified against a real Windows share:
`notes.txt`, `NOTES.TXT` and `Notes.Txt` all resolve to the same file, and the
directory listing returns the canonical on-disk name — which is what feeds the
identity, so stored paths stay stable. Two files differing only in case therefore
cannot coexist on Windows, and the case-collision risk exists only on a
case-sensitive server (Samba on Linux), where lowercasing the identity would make
one file silently overwrite the other's memory. The cost of not normalizing is
that a case-only rename churns one memory. Churn is recoverable; collision is
data loss.

### Google Drive specifics

- **Auth** — a service-account key, a GCP-attached service account, or workload
  identity federation. All three end with the connector acting as a service
  account that has been added to the Shared Drive as a Viewer. See
  [usage.md](usage.md) for setup and [permissions-google-drive.md](permissions-google-drive.md)
  for the IT request.
- **Export vs download** — binary files use `alt=media`; Google-native types are
  exported. The adapter reports the *export* MIME as the file's effective type
  (so MIME filtering and the stored content-type are right) and keeps the original
  Google MIME in `DownloadRef` for `Open` to branch on.

  | Native type | Exported as |
  |---|---|
  | `…google-apps.document` | `.docx` |
  | `…google-apps.spreadsheet` | `.xlsx` |
  | `…google-apps.presentation` | `.pptx` |

  Drive reports `size=0` for native docs, so the byte-size cap can't catch an
  oversized export; a `403 exportSizeLimitExceeded` is therefore mapped to
  `ErrSkip` (permanent skip, no dead-letter churn).
- **Folder deletion** — trashing a folder implicitly trashes its descendants, but
  the Changes feed emits **only** the folder's change. The adapter flags those
  deletions (`Change.ReconcileHint`), and the listener answers with a full
  reconcile so orphaned descendants are removed promptly.
- **Channel lifetime** — `changes.watch` is capped at 7 days and Drive may grant
  *less* than requested, so the renewal loop schedules from the **granted**
  expiry, not the requested TTL. Channel `id`+`resourceId` are persisted next to
  the delta cursor so a restart stops the old channel instead of leaking it.

## Deterministic memory IDs

Every source file maps to a **deterministic UUID** used as Goodmem's `memoryId`:

```
memoryId = uuid5(uuid5(NAMESPACE_DNS, <namespace>), <file id>)
```

Same file ⇒ same memory id ⇒ idempotent inserts: no duplicates, and we never have
to search Goodmem by metadata to find a file's memory. The namespace comes from
`Source.MemNamespace()` and is **per-source and permanent** — it is the
idempotency key, so changing it re-keys (deletes and re-embeds) every memory in
that space. SharePoint's value is inherited from the Python proof-of-concept and
is pinned by an oracle test.

## How the file diff is computed

The engine compares three **UUID-level sets** and uses the source timestamp it
previously stored in Goodmem metadata (never Goodmem's own `updatedAt`) so both
sides of a comparison use the same clock.

### Full sync

1. **Source:** list all files → their UUIDs.
2. **Goodmem:** list the space → the memory UUIDs it holds, plus each memory's
   stored `modified_datetime`.
3. **Set math:**
   - `Add` = source − Goodmem
   - `Delete` = Goodmem − source
   - in both → compare timestamps: stored **older** ⇒ `Update`; **equal** ⇒ skip;
     stored **newer** ⇒ an anomaly, reported in `UnexpectedNewer` and skipped.

### Delta sync

The incremental feed says whether a change is a deletion, but not whether a
non-deletion is an add or an update — and updates must delete before adding.

1. Pull changes since the cursor.
2. Deletions → `Delete`.
3. For the rest, look each candidate memory up in Goodmem: **found** ⇒ `Update`,
   **missing** ⇒ `Add`.
4. An expired cursor (`ErrCursorExpired`) falls back to a full sync, which
   re-bootstraps the cursor.

## How the diff is applied

Apply order is fixed: **delete → add → update** (an update is delete-then-create
with the same memory id, so it is safe even if the memory was never there).

**Ingest rules.** Unsupported MIME types and files over `SHAREPOINT_MAX_FILE_MB`
are skipped before download (never buffered). A create is only a success when
Goodmem returns 200 **and** processing reaches `COMPLETED`; because ingestion is
async the listener polls the memory until `COMPLETED`/`FAILED` or a timeout. A
200-then-`FAILED` is re-queued as delete-then-add, not counted as success.

**Pending-retry sets (listener only).** A failed add/update/delete parks the file
id in a durable set beside the delta cursor. The next delta sync re-fetches those
files' current source state and merges them into the action lists before applying.
The one-shot CLI keeps none of this — it matches the simpler `sync-once` model.

**Conflict resolution.** After merging pending work, a file id can land in more
than one list. Applying both would leave Goodmem inconsistent, so each conflicting
id is re-checked against the source once: if the file **exists**, keep only
`Add`/`Update` (preferring `Update`); if it is **gone**, keep only `Delete` and
drop it from the pending add/update sets. If the re-check itself fails, the fixed
apply order decides. The source is the single source of truth — never history or
ordering, which is why unordered sets are sufficient.

**Dead-lettering.** An item that keeps failing is parked after
`GRAPH_MAX_ITEM_ATTEMPTS` attempts instead of being retried forever; parked items
appear in `GET /syncs?status=dead` and the `connector_pending_dead` gauge.

## Safety guards

- **Mass-delete guard** — a full sync is refused if the source returns zero files
  while Goodmem is non-empty, or if the plan would delete more than
  `GRAPH_MAX_DELETE_RATIO` of the space (above a small floor). Protects against a
  partial listing wiping a subtree.
- **Cursor discipline** — the cursor is captured *before* listing and saved *only*
  when the full sync succeeded, so a change landing mid-sync is caught by the next
  delta and a failed sync never skips its window.
- **Coalescing** — notifications collapse into a 1-buffered channel drained by a
  single worker, so a bulk upload becomes one follow-up sync rather than thousands.
- **History retention** — sync-history rows older than
  `SYNC_HISTORY_RETENTION_DAYS` are pruned so the state volume can't fill.

## Deployment (`deploy_fly_io.sh`)

Deploys the listener to Fly.io, optionally provisioning Goodmem too. No flag =
listener only; `--hands-free` runs a Goodmem phase first; `--goodmem-only` and
`--generate-only` do just those parts. `./deploy_fly_io.sh --help` lists all modes.

**Listener deploy (every run):**
1. Generates `fly_io.toml` from the template (app name and region from the env file).
2. Creates or reuses the Fly app `<FLY_CLUSTER>-listener`, establishing its URL.
3. Writes `GRAPH_NOTIFICATION_URL=https://<app>.fly.dev/sync/webhook` and generates
   a random `GRAPH_CLIENT_STATE` if unset (stable across re-deploys — changing it
   would invalidate an existing subscription).
4. Imports the env file as Fly secrets, deploys one machine (`--ha=false`), and
   scales to 1 with auto-stop disabled so the listener stays up.
5. On boot the listener runs a full sync, bootstraps the cursor, then either
   creates the push subscription or starts the poll loop.

**Goodmem phase (`--hands-free` only), before the listener deploy:** runs the
[get.goodmem.ai/flyio](https://get.goodmem.ai/flyio) installer (app
`<FLY_CLUSTER>-goodmem`), writes `GOODMEM_BASE_URL`/`GOODMEM_API_KEY` into the env
file, and creates a `text-embedding-3-small` embedder (needs `OPENAI_API_KEY`).

> Fly is a fit for the SharePoint listener. For Google Drive, note that Fly issues
> no workload OIDC token (so no workload identity federation) and `*.fly.dev`
> can't be domain-verified (so no push) — a Drive deployment on Fly means a
> service-account key plus poll mode. See [usage.md](usage.md).

## Reference: gcloud profiles & ADC

Background for the Google Drive provider. (SharePoint has no equivalent — its
credentials are plain env vars.)

### gcloud CLI profiles

The gcloud CLI supports multiple profiles, each with a default account and
project, managed with `gcloud config`:

- Create: `gcloud config configurations create <name>`; set values with
  `gcloud config set account <email>` / `gcloud config set project <id>`.
- Switch: `gcloud config configurations activate <name>`; list with
  `gcloud config configurations list`; inspect with `gcloud config list`.
- Or scope a single command with `--configuration <name>`.

### Application Default Credentials (ADC)

ADC is how Google's client libraries find credentials without hardcoding them.
The SDK searches, in order:

1. **`GOOGLE_APPLICATION_CREDENTIALS`** — a path to a credentials JSON (a
   service-account key **or** a workload-identity credential config);
2. the **well-known gcloud file**,
   `~/.config/gcloud/application_default_credentials.json`, written by
   `gcloud auth application-default login`;
3. on a GCP host, the **attached service account** via the metadata server.

The connector uses ADC for the keyless paths (attached SA → step 3; workload
identity federation → step 1). The service-account-key path does **not** use ADC:
the connector reads `GOOGLE_DRIVE_SA_JSON`/`_FILE` and passes the key explicitly.

**ADC is independent of the gcloud CLI account.** The gcloud profile decides who
`gcloud` commands act as; ADC decides who the *application* acts as. The two can
be different identities at the same time.

## Known limitations

- SharePoint change notifications can take ~30 seconds to reach the listener (a
  Microsoft Graph characteristic).
- Only the SharePoint site's **first** document library is synced, and the
  listener always syncs the whole drive (`SHAREPOINT_FOLDER_PATH` scopes only
  `sync-once`). See [usage.md → Scope & limits](usage.md#scope--limits).
- Google Drive is scoped to a **Shared Drive**; personal My Drive content needs
  domain-wide delegation, which is not implemented.
- Metrics are named `connector_*` and carry a `source="<provider>"` label, so one
  dashboard/alert covers both providers; see [`deploy/alerts.yml`](../deploy/alerts.yml).

## References

Google Drive API surfaces the provider depends on:

- [Retrieve changes (Changes API)](https://developers.google.com/workspace/drive/api/guides/manage-changes)
- [changes.getStartPageToken](https://developers.google.com/workspace/drive/api/reference/rest/v3/changes/getStartPageToken)
- [Notifications for resource changes (push / watch)](https://developers.google.com/workspace/drive/api/guides/push)
- [Export MIME types for Google Workspace documents](https://developers.google.com/workspace/drive/api/guides/ref-export-formats)
- [files.export (10 MB limit)](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/export)
- [drive/v3 Go package](https://pkg.go.dev/google.golang.org/api/drive/v3)
