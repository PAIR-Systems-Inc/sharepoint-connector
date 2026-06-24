# Usage

How to sync SharePoint to Goodmem, deploy the event-triggered listener to Fly.io, and watch sync activity.

Before you start, set up credentials as described in [README.md](../README.md#permissions-and-credentials). 

## Manual/periodic sync

To sync your SharePoint drive to Goodmem once, run this command:

```bash
python sync_once.py
```

Use `--env-file` to load a specific env file; otherwise `.env` is used.

**Example output:**
```
Authenticating with Microsoft Graph API... ✓ Success.
Connecting to site... ✓ Connected.
Fetching files from SharePoint... ✓ Found 12 file(s).
Goodmem: Looking up space 'SharePoint_MyTenant_MySite'... Found. Using existing space.
Goodmem: Listing memories... Found 8.
Goodmem: Deleting memory for removed file: old_doc.pdf
Goodmem: Ingesting 4 file(s) (1.2 MB total)...
[====================>                    ] 512.0 KB / 1.2 MB
Sync complete.
```

## Event-triggered auto sync

This approach requires a listener between your SharePoint drive and Goodmem. The listener receives change notifications from Microsoft and syncs them to Goodmem. 

For Microsoft Graph API to talk to the listener, the listener **must be deployed to a public internet and set to use HTTPS (SSL/TLS)**. We provide a script `./deploy_fly_io.sh` to deploy the listener to Fly.io. It has many modes and options. Run `./deploy_fly_io.sh --help` to see them all. It has two major modes:
* **Deploy Listener only** — if Goodmem is already setup (e.g. elsewhere or you deployed it earlier) and configured in `.env`, you only deploy the Listener. See [Deploy Listener only](#deploy-listener-only-goodmem-already-setup).
* **Deploy both Goodmem and Listener** — if you want to deploy both the Listener and provision a new Goodmem instance to Fly.io in one go, see [Hands-free deployment of both Goodmem and Listener](#hands-free-deployment-of-both-goodmem-and-listener). This approach requires you to have a valid `OPENAI_API_KEY` in your `.env` file, as it creates a `text-embedding-3-small` embedder on the created Goodmem instance. 

### Deploy Listener only (Goodmem already setup)

If Goodmem is already setup (e.g. elsewhere or you deployed it earlier), you only deploy the Listener.

From the [Environment variables](../README.md#environment-variables) section, set the **Azure & SharePoint** and **Goodmem (A)** groups, plus `FLY_CLUSTER` (optionally `FLY_ORG` / `FLY_REGION`). The deploy script generates `GRAPH_CLIENT_STATE` and writes `GRAPH_NOTIFICATION_URL` for you. Then run:

```bash
./deploy_fly_io.sh
```

The step-by-step internals are in [tech_details.md](tech_details.md#deployment-deploy_fly_iosh).

**Example output** (trimmed):
```
=== Deploying SharePoint listener ===
App name: sharepoint-joint-listener
...
Set GRAPH_NOTIFICATION_URL=https://sharepoint-joint-listener.fly.dev/sync/webhook in .env
Generated a random GRAPH_CLIENT_STATE in .env
Deploying (single machine: --ha=false, region: sjc)...
...
App is at: https://sharepoint-joint-listener.fly.dev
Listener creates the Graph subscription on startup if none exists. Watch: python watch_listener.py https://sharepoint-joint-listener.fly.dev
```

### Hands-free deployment of both Goodmem and Listener

To stand up a new Goodmem **and** the listener in one go, use `--hands-free`. It provisions Goodmem on Fly.io and creates a `text-embedding-3-small` embedder (so it needs `OPENAI_API_KEY`; contact us if you need a different embedder).

From the [Environment variables](../README.md#environment-variables) section, set the **Azure & SharePoint** group, plus `FLY_CLUSTER` (optionally `FLY_ORG` / `FLY_REGION`) and `OPENAI_API_KEY`. Leave the **Goodmem (A)** group blank — the script provisions Goodmem and also generates `GRAPH_CLIENT_STATE` / writes `GRAPH_NOTIFICATION_URL`. Then run:

```bash
./deploy_fly_io.sh --hands-free
```

This runs a Goodmem phase, then the same listener deploy as above. Full step list: [tech_details.md](tech_details.md#deployment-deploy_fly_iosh).

**Example output** (trimmed):
```
=== Deploying Goodmem (get.goodmem.ai/flyio) ===
Goodmem app name: sharepoint-joint-goodmem
...
Set GOODMEM_BASE_URL=https://sharepoint-joint-goodmem.fly.dev in .env
Goodmem deploy finished.

=== Deploying SharePoint listener ===
...
App is at: https://sharepoint-joint-listener.fly.dev
```

### Watch Listener Activity (optional)

This step is **optional** — the listener syncs on its own whether or not you watch it. `watch_listener.py` just lets you observe the listener's activity (notifications received, sync plan, per-file syncing/synced/failed). Run it locally; use the same env file as your cluster so the watcher resolves the listener URL.

**Run:**
```bash
python watch_listener.py -n 0.5 # use the GRAPH_NOTIFICATION_URL from .env
# Or pass the listener URL directly:
python watch_listener.py -n 0.5 https://sharepoint-joint-listener.fly.dev
```

**Under the hood:** Polls `GET <listener_base>/activity?since=<id>` at the given interval. Prints new events as `[category] message` lines with a local timestamp: `[Graph Webhook] Received N change(s)`, the `[info] Full/Delta Sync (SharePoint → Goodmem): Started/Done` markers, the `To Add` / `To Update` / `To Remove` plan tree, per-file `[Add]/[Update]/[Remove] <path> (<file_id>) : Started` then `: Done`/`: Failed`, plus `[Graph Webhook] Subscribing/Subscribed/Renewing/Renewed` and `[oauth2]` token events. When idle, a single refreshing "no new activity (listener reachable)" line.

**Example output:**
```
Watching listener activity at https://sharepoint-joint-listener.fly.dev/activity (interval: 0.5s)
(Ctrl+C to stop)

Connected to listener. Waiting for activity...

  2026-02-02 01:19:33  [Graph Webhook] Received 1 change(s)
  2026-02-02 01:19:34  [info] Delta Sync (SharePoint → Goodmem): Started
  2026-02-02 01:19:34  To Add
  2026-02-02 01:19:34    (none)
  2026-02-02 01:19:34  To Update
  2026-02-02 01:19:34    Project/
  2026-02-02 01:19:34      docs/
  2026-02-02 01:19:34        └── Goodmem_just_works.docx
  2026-02-02 01:19:34  To Remove
  2026-02-02 01:19:34    (none)
  2026-02-02 01:19:45  [Update] Project/docs/Goodmem_just_works.docx (01DSLNGZ2OAHMTF4SKE5BYGBMAYG6X6HMV) : Started
  2026-02-02 01:19:46  [Update] Project/docs/Goodmem_just_works.docx (01DSLNGZ2OAHMTF4SKE5BYGBMAYG6X6HMV) : Done
  2026-02-02 01:19:46  [info] Delta Sync (SharePoint → Goodmem): Done
  2026-02-02 01:19:48  — no new activity (listener reachable)
```


**Renew subscription manually** (e.g. before expiry, or if it failed during deploy): `python listener.py create-subscription` (uses `.env`). The script will PATCH the existing subscription instead of creating a duplicate.

### Maintainance tasks

**Restarting a suspended listener:** If Fly.io suspends the app when idle, start the machine (not `fly apps resume`):
```bash
fly machine start $(fly machine list -a sharepoint-joint-listener 2>/dev/null | awk '/^[0-9a-f]{14}/ {print $1; exit}') -a sharepoint-joint-listener
```

**Manual deployment (alternative):** Generate the Fly config with `./deploy_fly_io.sh --generate-only [--org ORG] [--region R]`, then create the Fly app with `fly launch --no-deploy --name YOUR_LISTENER_APP --config fly_io.toml`, set `GRAPH_NOTIFICATION_URL=https://YOUR_LISTENER_APP.fly.dev/sync/webhook` in `.env`, run `fly secrets import < .env`, then `fly deploy`. The listener stays up for webhooks (`auto_stop_machines = 'off'`, `min_machines_running = 1`).
