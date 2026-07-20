# Usage

How to sync SharePoint to Goodmem with the **`connector`** binary, deploy the
event-triggered listener to Fly.io, and monitor sync activity.

Before you start, set up credentials as described in
[README.md](../README.md#getting-started). Config lives in `.env` — copy it from
[`.env.example`](../.env.example), which documents every variable.

## The `connector` binary

The connector is a single compiled Go binary with subcommands (it replaces the
Python proof-of-concept). Build it from source:

```bash
go build -o connector ./cmd/connector
./connector help
```

| Subcommand | What it does |
|---|---|
| `connector sync-once` | One-time full sync SharePoint → Goodmem. Flags: `--env-file PATH`, `--dry-run` (plan only, no changes). |
| `connector serve` | Run the webhook listener + sync engine (this is what gets deployed). Flag: `--env-file PATH`. |
| `connector create-subscription` | Create or renew the Graph change subscription. Flag: `--env-file PATH`. |
| `connector watch [-n SECS] <url>` | Tail a running listener's activity log locally. |

By default each command loads `.env` if present; `--env-file` overrides.

## Manual / periodic sync

Sync the whole SharePoint drive to Goodmem once:

```bash
./connector sync-once            # uses .env
./connector sync-once --dry-run  # show the add/update/delete plan without applying
```

Scope a one-time sync to a single folder with `SHAREPOINT_FOLDER_PATH` (see
`.env.example`). Run it on demand or on a schedule (cron).

## Event-triggered auto sync (the listener)

A long-running listener (`connector serve`) receives Microsoft Graph change
notifications and syncs the delta to Goodmem. Graph requires it to be
**publicly reachable over HTTPS/TLS**, so it must be deployed. `./deploy_fly_io.sh`
is the supported way to stand it up on Fly.io — it builds the Go binary into a
distroless image (via `Dockerfile`) and ships it. Run `./deploy_fly_io.sh --help`
for all modes; the two main ones:

### Deploy the listener only (Goodmem already set up)

Set the **Azure & SharePoint** and **Goodmem (A)** groups in `.env`, plus
`FLY_CLUSTER` (optionally `FLY_ORG` / `FLY_REGION`). The deploy script generates
`GRAPH_CLIENT_STATE` and writes `GRAPH_NOTIFICATION_URL` for you. Then:

```bash
./deploy_fly_io.sh
```

The container runs `connector serve`. On startup the listener does a full sync,
bootstraps the delta cursor, and creates the Graph subscription. Step-by-step
internals: [tech_details.md](tech_details.md#deployment-deploy_fly_iosh).

### Hands-free: deploy Goodmem + listener together

`--hands-free` also provisions a fresh Goodmem server on Fly.io and creates a
`text-embedding-3-small` embedder (so it needs `OPENAI_API_KEY`). Leave the
**Goodmem (A)** group blank; set **Azure & SharePoint**, `FLY_CLUSTER`, and
`OPENAI_API_KEY`. Then:

```bash
./deploy_fly_io.sh --hands-free
```

## HTTP endpoints

The listener (`connector serve`) exposes:

| Endpoint | Purpose |
|---|---|
| `POST /sync/webhook` | Microsoft Graph change notifications (validation handshake + `clientState` check). |
| `GET /healthz` | Liveness probe (always `200`). |
| `GET /metrics` | **Prometheus** metrics — files added/updated/deleted/skipped, sync errors, full/delta sync counts, Graph throttle events, subscription-renewal health, last-sync time, and pending-retry queue depth. Point Prometheus/Grafana here. |
| `GET /syncs` | **Durable sync history** (SQLite): one JSON record per item — `file_id`, `file_name`, `memory_id`, `space_id`, `op`, `status`, `message`, `ts`. Query params: `?limit=100&status=failure`. Great for "did file X sync, and why did it fail?". |
| `GET /activity` | In-memory recent-events log (what `connector watch` polls). |

## Monitoring

- **Metrics / dashboards / alerts:** scrape `GET /metrics` with Prometheus. This
  supersedes the old manual watch loop.
- **Debugging a specific file:** `curl "https://<listener>/syncs?status=failure"`.
- **Live tail (optional):** `./connector watch https://<listener>` prints new
  activity events as they happen. The listener syncs with or without it.

## Operations

- **Durable state.** The delta cursor, pending-retry sets, and the sync-history
  SQLite DB live under `GRAPH_DELTA_TOKEN_FILE`'s directory — on Fly that's the
  persistent `/data` volume (created by the deploy script), so they survive
  restarts. Locally they default to the working directory.
- **Periodic safety full-sync.** Beyond deltas, the listener runs a full
  reconcile every `GRAPH_FULL_SYNC_MINUTES` (default = half the subscription
  lifetime; `0` disables) to repair anything a missed notification dropped.
- **Renew the subscription manually** (e.g. after a failed deploy):
  `./connector create-subscription` — it renews the existing subscription
  instead of creating a duplicate.
- **Restart a suspended listener.** If Fly suspends the app when idle, start the
  machine (not `fly apps resume`):
  ```bash
  fly machine start $(fly machine list -a <FLY_CLUSTER>-listener 2>/dev/null | awk '/^[0-9a-f]{14}/ {print $1; exit}') -a <FLY_CLUSTER>-listener
  ```
- **Manual deployment (alternative to the script).** Generate the Fly config
  with `./deploy_fly_io.sh --generate-only [--org ORG] [--region R]`, then
  `fly launch --no-deploy --name YOUR_LISTENER_APP --config fly_io.toml`, set
  `GRAPH_NOTIFICATION_URL=https://YOUR_LISTENER_APP.fly.dev/sync/webhook` in
  `.env`, `fly secrets import < .env`, and `fly deploy`. The listener stays up
  for webhooks (`auto_stop_machines = 'off'`, `min_machines_running = 1`) and
  mounts the `/data` volume for durable state.
