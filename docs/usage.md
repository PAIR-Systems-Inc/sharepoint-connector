# Usage

How to sync a content source — **SharePoint** or **Google Drive** — into Goodmem
with the **`connector`** binary, deploy the event-triggered listener, and monitor
it. For a five-line quickstart, see [README.md](../README.md); this is the
detailed reference.

Config lives in `.env` — copy it from [`.env.example`](../.env.example), which
documents every variable.

## The `connector` binary

A single compiled Go binary with subcommands. Build it from source:

```bash
go build -o connector ./cmd/connector
./connector help
```

| Subcommand | What it does |
|---|---|
| `connector sync-once` | One-time full sync into Goodmem. Flags: `--env-file PATH`, `--source NAME`, `--dry-run` (plan only, no changes). |
| `connector serve` | Run the listener + sync engine (this is what gets deployed). Flags: `--env-file PATH`, `--source NAME`. |
| `connector create-subscription` | Create or renew the push subscription. Flags: `--env-file PATH`, `--source NAME`. |
| `connector watch [-n SECS] <url>` | Tail a running listener's activity log locally. |

By default each command loads `.env` if present; `--env-file` overrides.

## Choosing the source

Set **`SOURCE=sharepoint`** (default) or **`SOURCE=google-drive`** in `.env`, or
pass `--source` per run. It selects which credential group below is required;
everything else — the sync engine, endpoints, retries, metrics — is identical.

```bash
./connector sync-once --source google-drive
```

## Authentication

### SharePoint (Azure AD)

The connector authenticates as an **Azure AD application** with Microsoft Graph
*application* permissions (`Files.Read.All`, `Sites.Read.All`).

1. **Ask IT** to register the app and grant those permissions — hand them
   [permissions-sharepoint.md](permissions-sharepoint.md). They return a client
   id, client secret, and tenant id.
2. Put them in `.env`:

```dotenv
SOURCE=sharepoint
AZURE_AD_CLIENT_ID=...
AZURE_AD_TENANT_ID=...
AZURE_AD_CLIENT_SECRET=...
SHAREPOINT_SITE_URL=https://your-tenant.sharepoint.com/sites/YourSite
```

That's the whole setup — the credential is a static secret the app uses directly.

### Google Drive (service account)

The connector reads a Google **Shared Drive** as a **service account** (a
read-only robot identity). There are **three deploy-and-forget options** — set up
once, runs unattended, no human ever logs in again. Pick by one question: *where
does the connector run, and does your org allow downloadable service-account keys?*

| Path | Use when | You manage | Secret? |
|---|---|---|---|
| **1 — Service-account key** | Runs **anywhere** (Fly, on-prem, a VM, another cloud) and your org allows keys | one JSON file/secret | yes (1 static key) |
| **2 — Attached service account** | Runs **on GCP compute** that can mint a Drive-scoped token (e.g. a GCE VM) | nothing | no |
| **3 — Workload Identity Federation** | Runs **off GCP** *and* your org forbids keys | one non-secret config file | no |

> **In one line:** keys allowed → **Path 1** (simplest, works everywhere). On GCP
> → **Path 2** (no secret). Neither → **Path 3**.

Ask IT for the pieces with
[permissions-google-drive.md](permissions-google-drive.md) — it covers all three.

**Common to every path: the Shared Drive must be shared with the service
account.** Google Cloud roles do not grant Drive content access, so until the
service-account email is added as a **Viewer** on the drive, it authenticates
successfully but sees zero files. Copy the **Drive ID** from the drive's URL
(`…/drive/folders/<DRIVE_ID>`).

Shared `.env` base for all three paths:

```dotenv
SOURCE=google-drive
GOOGLE_DRIVE_ID=<DRIVE_ID>
```

**Path 1 — service-account key.** IT provides a JSON key; add one of:

```dotenv
GOOGLE_DRIVE_SA_JSON_FILE=/secure/path/goodmem-connector.json   # a file, OR
# GOOGLE_DRIVE_SA_JSON={"type":"service_account",...}            # inline (e.g. a Fly secret)
```

Keep it out of git (`.gitignore` covers `*-sa.json`, `secrets/`). Rotation is
optional hygiene.

**Path 2 — attached service account.** Set **no** credential variable; the GCP
host supplies the identity. The workload must be able to obtain a *Drive-scoped*
token — e.g. a GCE VM created with the `drive.readonly` access scope:

```bash
gcloud compute instances create goodmem-listener \
  --service-account="goodmem-connector@<project>.iam.gserviceaccount.com" \
  --scopes="https://www.googleapis.com/auth/drive.readonly"
```

> **Scope gotcha:** a `cloud-platform`-only token does **not** cover the Drive
> API. **Cloud Run** issues exactly that and can't downscope it, so Cloud Run
> needs Path 1 or 3.

**Path 3 — workload identity federation.** IT provides a credential-configuration
JSON (no key inside — not a secret). Point the Google SDK at it:

```dotenv
GOOGLE_APPLICATION_CREDENTIALS=/path/to/wif-credential-config.json
```

> Requires the host platform to issue a workload OIDC token — GCP, GitHub
> Actions, AWS, Azure and Kubernetes do; a plain **Fly.io** app does **not**
> today. On Fly, use Path 1 or run on a GCP host.

**Local testing only — impersonation.** For hands-on runs you can impersonate the
service account with your own Google login instead:

```bash
gcloud auth application-default login \
  --impersonate-service-account="goodmem-connector@<project>.iam.gserviceaccount.com" \
  --scopes=https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/cloud-platform
```

(Keep `--scopes` on **one line**.) **Not for deployment:** it stores a *user*
refresh token that Google's reauth policy expires on a schedule, so a deployed
listener would silently stop syncing until a human re-ran the login.

*(Background on gcloud profiles and how ADC resolves credentials:
[tech_details.md → Reference](tech_details.md#reference-gcloud-profiles--adc).)*

### Goodmem (always required)

```dotenv
GOODMEM_BASE_URL=https://your-goodmem
GOODMEM_API_KEY=...
GOODMEM_SPACE_ID=...     # or leave unset to auto-create a per-source space
```

> ⚠️ **One space per source.** Never point a SharePoint listener and a Google
> Drive listener at the **same** `GOODMEM_SPACE_ID`: each full sync reconciles the
> space against *its own* files and deletes the rest as orphans, so the two would
> delete and re-add each other's memories forever (re-embedding every cycle).
> Leave `GOODMEM_SPACE_ID` unset and each source creates its own space
> (`SharePoint_<org>_<site>` / `GoogleDrive_<driveId>`).

## Manual / periodic sync

Sync everything once:

```bash
./connector sync-once                        # uses .env
./connector sync-once --dry-run              # show the plan without applying
./connector sync-once --source google-drive  # override the configured source
```

The dry-run lists the files it can see and the add/update/delete plan — it is also
the quickest way to verify credentials and (for Google Drive) the Drive share.
Run it on demand or on a schedule (cron).

SharePoint only: scope a one-time sync to a folder with `SHAREPOINT_FOLDER_PATH`.

## Event-triggered auto sync (the listener)

`connector serve` runs continuously and keeps Goodmem up to date. It gets its
changes one of two ways:

| Mode | How it triggers | Default for | Needs |
|---|---|---|---|
| **Push** | the provider POSTs a webhook on each change | SharePoint | a public HTTPS URL (`GRAPH_NOTIFICATION_URL`) + `GRAPH_CLIENT_STATE` |
| **Poll** | the listener pulls the delta on a timer | Google Drive | nothing public |

Set **`SYNC_POLL_MINUTES`** to choose: `>0` polls on that interval, `0` uses push.
Google Drive defaults to poll (2 min) because Google's push channels require a
**domain-verified** HTTPS endpoint — a throwaway `*.fly.dev` host cannot satisfy
that. Push is worth it only if you own a verifiable domain and want sub-minute
latency.

Either way the listener also runs a periodic full reconcile as a safety net.

### Deploy the listener to Fly.io

`./deploy_fly_io.sh` is the supported path; `--help` lists all modes.

**Listener only (Goodmem already exists):** set your source's credentials plus
the **Goodmem** group and `FLY_CLUSTER` (optionally `FLY_ORG` / `FLY_REGION`).
For push mode the script generates `GRAPH_CLIENT_STATE` and writes
`GRAPH_NOTIFICATION_URL` for you. Then:

```bash
./deploy_fly_io.sh
```

**Hands-free (Goodmem + listener):** `--hands-free` also provisions a Goodmem
server and a `text-embedding-3-small` embedder (needs `OPENAI_API_KEY`); leave the
Goodmem group blank.

```bash
./deploy_fly_io.sh --hands-free
```

On startup the listener runs a full sync, bootstraps the delta cursor, then either
creates the push subscription or starts polling. Internals:
[tech_details.md](tech_details.md#deployment-deploy_fly_iosh).

> **Google Drive on Fly:** Fly issues no workload OIDC token and `*.fly.dev` can't
> be domain-verified — so a Drive listener on Fly means **Path 1 (a key) + poll
> mode**. Alternatively run it on a GCP host and use Path 2.

## HTTP endpoints

| Endpoint | Purpose |
|---|---|
| `POST /sync/webhook` | Provider change notifications (push mode): validation handshake + secret check. |
| `GET /healthz` | Liveness probe (always `200` once the server is up). |
| `GET /readyz` | Readiness probe — `200` once the subscription is ensured (push mode) and the startup full sync has been **attempted**; `503` until then. A failed startup sync does **not** hold readiness: the periodic reconcile retries it, and the cursor isn't advanced on failure so nothing is silently skipped. Point your load balancer here. |
| `GET /metrics` | **Prometheus** metrics — files added/updated/deleted/skipped, sync errors, full/delta counts, throttle events, subscription-renewal health, last-sync time, pending-retry depth, and `sharepoint_pending_dead` (parked items — alert on this). |
| `GET /syncs` | **Durable sync history** (SQLite): one JSON record per item — `file_id`, `file_name`, `memory_id`, `space_id`, `op`, `status`, `message`, `ts`. `status` is `success`, `failure`, `skipped`, or `dead`. Query: `?limit=100&status=failure`. Answers "did file X sync, and why did it fail?". |
| `GET /activity` | In-memory recent-events log (what `connector watch` polls). |

## Monitoring

- **Metrics / dashboards:** scrape `GET /metrics` with Prometheus.
- **Alerting:** a recommended rules file ships at
  [`deploy/alerts.yml`](../deploy/alerts.yml) — load it into Prometheus
  (`rule_files:`) and point it at Alertmanager. It covers the otherwise-silent
  failure modes: listener down, parked files, subscription-renewal failures,
  retry backlog, sync errors, throttle storms, and stale sync (tune its threshold
  above `GRAPH_FULL_SYNC_MINUTES`).
- **Structured logs:** JSON to stderr. `LOG_LEVEL` (debug|info|warn|error, default
  info) and `LOG_FORMAT` (json|text, default json).
- **Debugging one file:** `curl "https://<listener>/syncs?status=failure"` (or
  `?status=dead` for parked files).
- **Live tail (optional):** `./connector watch https://<listener>`.

## Scope & limits

- **SharePoint: first document library only.** The listener syncs and subscribes
  to the site's **first** drive. A site with several libraries only has that one
  covered.
- **SharePoint: the listener always syncs the whole drive.**
  `SHAREPOINT_FOLDER_PATH` scopes a one-time `sync-once` only; the listener
  ignores it and logs a warning at startup if it is set.
  ⚠️ **Trap:** running a folder-scoped `sync-once` into a space and then pointing
  the listener at that same space makes the startup full sync ingest the *entire*
  drive. Use a dedicated space for the listener.
- **Google Drive: Shared Drives only.** Personal *My Drive* content would need
  domain-wide delegation, which is not implemented.
- **Google Drive: native docs are exported.** Docs/Sheets/Slides are converted to
  `.docx`/`.xlsx`/`.pptx`. Google caps exports at 10 MB; a larger one is recorded
  as a permanent skip (it can never succeed).
- **Safety knobs** (all in [`.env.example`](../.env.example)):
  `SHAREPOINT_MAX_FILE_MB` skips oversized files (default 100 MB);
  `GRAPH_MAX_DELETE_RATIO` refuses a full sync that would delete an implausible
  share of memories (default 0.5, guarding against a partial listing);
  `GRAPH_MAX_ITEM_ATTEMPTS` parks a permanently-failing file after N tries
  (default 10); `SYNC_HISTORY_RETENTION_DAYS` prunes old `/syncs` rows (default 90).

## Operations

- **Durable state.** The delta cursor, pending-retry sets, the Google Drive push
  channel record, and the sync-history SQLite DB live under
  `GRAPH_DELTA_TOKEN_FILE`'s directory — on Fly the persistent `/data` volume, so
  they survive restarts. Locally they default to the working directory.
- **Periodic safety full-sync.** Beyond deltas, the listener runs a full reconcile
  every `GRAPH_FULL_SYNC_MINUTES` (default = half the subscription lifetime; `0`
  disables) to repair anything a missed notification or poll dropped.
- **Parked (dead-lettered) files.** A file that keeps failing (corrupt, oversized,
  or one Goodmem always marks FAILED) is parked after `GRAPH_MAX_ITEM_ATTEMPTS`
  tries instead of being re-downloaded every sync. It appears in
  `GET /syncs?status=dead` and the `sharepoint_pending_dead` gauge — alert on it,
  investigate, and re-uploading or editing the file queues a fresh attempt.
- **Shutdown.** On SIGTERM the listener stops accepting work and exits; an
  in-flight provider call may still be sleeping between retries (bounded to a
  couple of minutes), so shutdown can briefly wait — process exit is the backstop.
  This is safe: the cursor is saved only *after* a sync's changes are applied and
  re-ingestion is idempotent, so a mid-sync kill is recoverable.
- **Renew the subscription manually** (push mode; e.g. after a failed deploy):
  `./connector create-subscription` — it renews rather than duplicating. Works for
  either source (pass `--source`).
- **Restart a suspended listener.** If Fly suspends the app when idle, start the
  machine (not `fly apps resume`):
  ```bash
  fly machine start $(fly machine list -a <FLY_CLUSTER>-listener 2>/dev/null | awk '/^[0-9a-f]{14}/ {print $1; exit}') -a <FLY_CLUSTER>-listener
  ```
- **Manual deployment (alternative to the script).** Generate the Fly config with
  `./deploy_fly_io.sh --generate-only [--org ORG] [--region R]`, then
  `fly launch --no-deploy --name YOUR_LISTENER_APP --config fly_io.toml`, set
  `GRAPH_NOTIFICATION_URL=https://YOUR_LISTENER_APP.fly.dev/sync/webhook` in
  `.env`, `fly secrets import < .env`, and `fly deploy`. The listener stays up for
  webhooks (`auto_stop_machines = 'off'`, `min_machines_running = 1`) and mounts
  the `/data` volume for durable state.
