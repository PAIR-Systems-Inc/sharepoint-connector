# SharePoint Connector for Goodmem

Synchronize files on a SharePoint site to a Goodmem memory space. 

There are two ways to keep your sharepoint drive and Goodmem in sync:
1. Event-triggered sync -- using a listener that updates Goodmem only after receiving notifications from Microsoft.
2. Manual/periodic sync -- checks for changes in your Sharepoint drive without knowing whether there are changes in your Sharepoint drive.

To use the event-triggered sync, the listener **must be deployed to a public internet and set to use HTTPS (SSL/TLS)**. This is a requirement of the Microsoft Graph webhook API.
To easily satisfy these requirements, we support depolying to: 
* Fly.io
* Railway (coming soon)


Files (repo layout):

```
sharepoint/
├── sharepoint_client.py   # Fetches files from SharePoint (Graph API).
├── goodmem_client.py     # Goodmem API client: spaces, ingest, list/delete memories.
├── sync_once.py          # One-time full sync: copies all files from SharePoint to Goodmem (uses .env or .env.example; --env-file to override).
├── listener.py           # Graph webhook server: receives change notifications, syncs add/update/delete to Goodmem.
├── watch_listener.py     # Polls listener /activity; run locally to monitor sync.
├── deploy_fly_io.sh      # Deploy Goodmem and/or listener to Fly.io (recommended).
├── requirements.txt      # Python dependencies. Needed for many deployment platforms despite uv.
├── Dockerfile            # Image for listener (used by both fly_io.listener.toml and fly_io.both.toml).
├── fly_io.listener.toml.template   # Template for listener-only Fly config (app/region substituted by deploy script).
├── fly_io.both.toml.template       # Template for listener Fly config when using --both.
├── .env.example
└── .env                  # Your credentials (do not commit). Create from .env.example; deploy script writes GOODMEM_* and GRAPH_NOTIFICATION_URL.
```



## Permissions and credentials

1. **Ask IT** to grant the Azure AD app permissions. Share [permission.md](permission.md) with them.

2. **Set credentials in `.env`.** Create it from the template:
   ```bash
   cp .env.example .env
   ```
   Edit `.env` and fill in the **Azure** section. 
   
   Leave other fields untouched. They will be populated by the deploy script or set by you manually below. 

## Manual/periodic sync

To sync your SharePoint drive to Goodmem once, run this command:

```bash
python sync_once.py
```

 Use `--env-file` to load a specific env file; otherwise `.env` is used if present, else `.env.example`. You need to set the following environment variables in your `.env` file:
* Azure crednetials: 
  * `AZURE_AD_CLIENT_ID` (ask your IT)
  * `AZURE_AD_TENANT_ID` (ask your IT)
  * `AZURE_AD_CLIENT_SECRET` (ask your IT)
  * `SHAREPOINT_SITE_URL` (ask your IT)
* Goodmem:
  * `GOODMEM_BASE_URL` (ask your IT)
  * `GOODMEM_API_KEY` (ask your IT)
  * `GOODMEM_SPACE_ID` OR `GOODMEM_EMBEDDER_ID`: If `GOODMEM_EMBEDDER_ID` is set, a new space of the name `SharePoint_{Org}_{Site}` (derived from `SHAREPOINT_SITE_URL`) will be created with that embedder. If `GOODMEM_SPACE_ID` is set, the listener will use that space.

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

This approach puts a listener between your SharePoint drive and Goodmem. The listener receives change notifications from Microsoft and syncs them to Goodmem. For Microsoft Graph API to talk to the listener, the listener **must be deployed to a public internet and set to use HTTPS (SSL/TLS)**.

Curretly, we only support Fly.io for deployment. We are adding other options. 

The `deploy_fly_io.sh` script helps you deploy the listener to Fly.io in two modes: 
1. Deploy listener only (Goodmem already setup)
2. Hands-free deployment of both Goodmem and listener to Fly.io; you just sit back and relax.

### Deploy Listener only (Goodmem already setup)

If Goodmem is already setup (e.g. elsewhere or you deployed it earlier), you only deploy the Listener.

Set the following environment variables in your `.env` file:
* Azure crednetials: 
  * `AZURE_AD_CLIENT_ID` (ask your IT)
  * `AZURE_AD_TENANT_ID` (ask your IT)
  * `AZURE_AD_CLIENT_SECRET` (ask your IT)
  * `SHAREPOINT_SITE_URL` (ask your IT)
* Goodmem:
  * `GOODMEM_BASE_URL` (ask your IT)
  * `GOODMEM_API_KEY` (ask your IT)
  * `GOODMEM_SPACE_ID` OR `GOODMEM_EMBEDDER_ID`: If `GOODMEM_EMBEDDER_ID` is set, a new space of the name `SharePoint_{Org}_{Site}` will be created with that embedder. If `GOODMEM_SPACE_ID` is set, the listener will use that space.
* Graph sync:
  * `GRAPH_CLIENT_STATE` (You pick this secret string; Graph subscription clientState)
* Fly.io:
  * `FLY_ORG` (ask your IT)
  * `FLY_REGION` (ask your IT)
  * `FLY_CLUSTER` (required. Ensure that app name `<FLY_CLUSTER>-listener` is not in use on Fly.io.)

Then run the following command to deploy the listener to Fly.io:

```bash
./deploy_fly_io.sh --listener-only
```

Run `./deploy_fly_io.sh -h` for more options. 

**Under the hood,** the `deploy_fly_io.sh` script does the following:
1. Generates `fly_io.listener.toml` from template (app name and region from `.env`).
2. Creates the Fly app of the name `<FLY_CLUSTER>-listener` on Fly.io if it doesn’t exist, or reuses the existing one. No image is deployed yet; this just ensures the app exists so we know its URL.
3. Based on the app name, determines and writes `GRAPH_NOTIFICATION_URL` into `.env`.
4. Imports secrets from `.env` and deploys the app with one machine (`fly_io.listener.toml` + `Dockerfile`).
5. Listener process (on Fly.io): on boot runs a full sync; a background loop creates the Graph subscription if none exists (using `GRAPH_NOTIFICATION_URL` from its env), then auto-renews it and handles webhooks.

**Example output:**
```
=== Deploying SharePoint listener ===
App name: sharepoint-joint-listener
Config: fly_io.listener.toml (listener only)
Using existing app: sharepoint-joint-listener
Set GRAPH_NOTIFICATION_URL=https://sharepoint-joint-listener.fly.dev/sync/webhook in .env
Importing secrets from .env...
Deploying (single machine: --ha=false, region: sjc)...
App is at: https://sharepoint-joint-listener.fly.dev
Listener creates the Graph subscription on startup if none exists. Watch: python watch_listener.py https://sharepoint-joint-listener.fly.dev
```

### Hands-free deployment of both Goodmem and Listener

You can deploy both Goodmem and Listener to Fly.io in one go. This approach creates a `text-embedding-3-small` embedder on the created Goodmem instance -- so it requires you to set `OPENAI_API_KEY` in your `.env` file. If you need a different embedder, contact us.

Set the following environment variables in your `.env` file -- leaving others untouched because they will not be used:

* Azure crednetials: 
  * `AZURE_AD_CLIENT_ID` (ask your IT)
  * `AZURE_AD_TENANT_ID` (ask your IT)
  * `AZURE_AD_CLIENT_SECRET` (ask your IT)
  * `SHAREPOINT_SITE_URL` (ask your IT)
* Graph sync:
  * `GRAPH_CLIENT_STATE` (You pick this secret string; Graph subscription clientState)
* Fly.io:
  * `FLY_ORG` (ask your IT)
  * `FLY_REGION` (ask your IT)
  * `FLY_CLUSTER` (required. Ensure that app names `<FLY_CLUSTER>-listener`, `<FLY_CLUSTER>-goodmem`, and `<FLY_CLUSTER>-goodmem-postgres` are not in use on Fly.io.)
  * `OPENAI_API_KEY` (required for hands-free; used to create a `text-embedding-3-small` embedder in Goodmem.)

**Under the hood,** the script runs two phases:

1. **Goodmem phase**
   - Runs [get.goodmem.ai/flyio](https://get.goodmem.ai/flyio) installer (app `<FLY_CLUSTER>-goodmem`, tier small, region from `.env`).
   - Scales Goodmem to one machine.
   - Writes `GOODMEM_BASE_URL` and `GOODMEM_API_KEY` into `.env`.
   - Creates a `text-embedding-3-small` embedder via Goodmem API (requires `OPENAI_API_KEY` in `.env`).
2. **Listener phase** (same steps as “Deploy listener only”)
   - Generates `fly_io.both.toml` from template (app name and region from .env).
   - Creates the Fly app on Fly.io if it doesn’t exist, or reuses it (name `<FLY_CLUSTER>-listener`). No image deployed yet.
   - Writes `GRAPH_NOTIFICATION_URL` into `.env`.
   - Imports secrets from `.env`, deploys with one machine (`fly_io.both.toml` + `Dockerfile`).
   - Listener process (on Fly.io): on boot runs a full sync; a background loop creates the Graph subscription if none exists, then auto-renews it and handles webhooks.

**Example output:**
```
=== Deploying Goodmem (get.goodmem.ai/flyio) ===
Goodmem app name: sharepoint-joint-goodmem
Env file: .env
Region: sjc
...
Set GOODMEM_BASE_URL=https://sharepoint-joint-goodmem.fly.dev in .env
Set GOODMEM_API_KEY in .env (from installer output)
Scaling Goodmem to one machine...
Goodmem deploy finished. Listener will use .env when you run --both or deploy listener next.

=== Deploying SharePoint listener ===
App name: sharepoint-joint-listener
Config: fly_io.both.toml (both on one app)
Using existing app: sharepoint-joint-listener
Set GRAPH_NOTIFICATION_URL=https://sharepoint-joint-listener.fly.dev/sync/webhook in .env
Importing secrets from .env...
Deploying (single machine: --ha=false, region: sjc)...
App is at: https://sharepoint-joint-listener.fly.dev
Listener creates the Graph subscription on startup if none exists. Watch: python watch_listener.py https://sharepoint-joint-listener.fly.dev
```

### Watch Listener Activity

Monitor the listener's activity (notifications received, sync plan, per-file syncing/synced/failed). Run locally; use the same env file as your cluster so the watcher resolves the listener URL.

**Run:**
```bash
python watch_listener.py -n 0.5 # use the GRAPH_NOTIFICATION_URL from .env
# Or pass the listener URL directly:
python watch_listener.py -n 0.5 https://sharepoint-joint-listener.fly.dev
```

**Under the hood:** Polls `GET <listener_base>/activity?since=<id>` at the given interval. Prints new events: notification received, delta tree (To Add / To Update / To Remove), `[Syncing]` per file, `[Synced]` or `[Failed]` when each finishes. When idle, at most one "no new activity (listener reachable)" line.

**Example output:**
```
Watching listener activity at https://sharepoint-joint-listener.fly.dev/activity (interval: 0.5s)
Connected to listener. Waiting for activity...

  2026-02-02 01:19:33  [notification_received]  Received 1 change(s) from Graph
  2026-02-02 01:19:44  To Add
  2026-02-02 01:19:44    (none)
  2026-02-02 01:19:44  To Update
  2026-02-02 01:19:44    Project/
  2026-02-02 01:19:44      docs/
  2026-02-02 01:19:44        └── Goodmem_just_works.docx
  2026-02-02 01:19:44  To Remove
  2026-02-02 01:19:44    (none)
  2026-02-02 01:19:45  [Syncing] Update: Project/docs/Goodmem_just_works.docx
  2026-02-02 01:19:46  [Synced] Update: Project/docs/Goodmem_just_works.docx
```


**Renew subscription manually** (e.g. before expiry, or if it failed during deploy): `python listener.py create-subscription` (uses `.env`). The script will PATCH the existing subscription instead of creating a duplicate.

### Maintainance tasks

**Restarting a suspended listener:** If Fly.io suspends the app when idle, start the machine (not `fly apps resume`):
```bash
fly machine start $(fly machine list -a sharepoint-joint-listener 2>/dev/null | awk '/^[0-9a-f]{14}/ {print $1; exit}') -a sharepoint-joint-listener
```

**Manual deployment (alternative):** Generate Fly configs with `./deploy_fly_io.sh --generate-only [--org ORG] [--region R]`, then create the Fly app with `fly launch --no-deploy --name YOUR_LISTENER_APP --config fly_io.listener.toml`, set `GRAPH_NOTIFICATION_URL=https://YOUR_LISTENER_APP.fly.dev/sync/webhook` in `.env`, run `fly secrets import < .env`, then `fly deploy`. The listener stays up for webhooks (`auto_stop_machines = 'off'`, `min_machines_running = 1`).

**References (Microsoft):** [subscription resource type](https://learn.microsoft.com/en-us/graph/api/resources/subscription) · [Change notifications (webhooks)](https://learn.microsoft.com/en-us/graph/change-notifications-overview)

## Technical details

### `sharepoint_client.py`

Fetches files from SharePoint. Do not load credentials from `.env` except under `__main__`.

Example file JSON:

```json
{
  "id": "01DSLNGZ2OAHMTF4SKE5BYGBMAYG6X6HMV",
  "name": "claude_usage.pdf",
  "web_url": "https://incorta.sharepoint.com/sites/Pair/Shared%20Documents/claude_usage.pdf",
  "download_url": "...",
  "size": 153749,
  "created_datetime": "2026-01-28T08:27:35Z",
  "modified_datetime": "2026-01-28T08:27:35Z",
  "created_by": "Mohamed Helmy",
  "modified_by": "Mohamed Helmy",
  "mime_type": "application/pdf",
  "file_hash": null
}
```

### `goodmem_client.py`

Goodmem API client. Do not load credentials from `.env` except under `__main__`.

- Create space, find space by name, list/delete memories, ingest files.
- `find_space_by_name(space_name)` returns the space ID or `None`; `sync_once.py` creates a new space when `None`.

### `sync_once.py`

One-time full sync from SharePoint to Goodmem. Env file: `--env-file` to specify; otherwise uses `.env` if present, else `.env.example`. Fetches all files from the default drive (root, recursive) and syncs them. Creates a space from the site URL if needed (`SharePoint_{Org}_{Site}`). Rules:

1. File not in Goodmem → ingest.
2. File in Goodmem with newer `modified_datetime` → delete old memory, ingest again.
3. File in Goodmem but no longer in SharePoint → delete memory.

**Space name:** Derived from `SHAREPOINT_SITE_URL`: org = host before `.sharepoint.com`, site = first segment after `sites/`; space name = `SharePoint_{Org}_{Site}`.

**Supported MIME types:** text/*, PDF, RTF, Word (.doc/.docx), types containing "+xml" or "json". Others are skipped.

### `listener.py`

Microsoft Graph webhook server. Requires same SharePoint/Goodmem env as `sync_once.py` plus `GRAPH_NOTIFICATION_URL` and `GRAPH_CLIENT_STATE`.

**On startup:** (1) The server runs a **one-time full sync** (same logic as `sync_once.py`) in a background thread so Goodmem is up to date before handling webhooks. (2) A background **subscription renewal loop** creates a Graph subscription if none exists (using `GRAPH_NOTIFICATION_URL`), or renews the existing one via PATCH **before it expires** using the `expirationDateTime` returned by Graph (sleeps until near expiry, then renews and reschedules from the new expiration).

**Commands:** `listener.py create-subscription` creates or renews the subscription manually (same PATCH-if-exists behavior). Useful if renewal failed or you want to renew ahead of the loop.

The server exposes **GET /activity** with an in-memory log of recent events. Use **`watch_listener.py`** locally to poll and print this log; use `.env` (default) or `--env-file` for `GRAPH_NOTIFICATION_URL`, or pass the listener base URL as an argument.

## Known limitations

- It takes about 30 seconds for the update to reach our listener from Azure. This is a limitation of the Microsoft Graph API.
- Current implementation requires the listener to do delta every time a push is received. 

##  Roadmap

* Use TOML-based environment file than .env.