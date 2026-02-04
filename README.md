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

**Under the hood:** Polls `GET <listener_base>/activity?since=<id>` at the given interval. Prints new events: notification received, Full/Delta sync started/done, `[Sync] Add-ing`/`Update-ing`/`Delete-ing` per file and `Add-ed`/`Update-ed`/`Delete-ed` when each finishes. When idle, at most one "no new activity (listener reachable)" line.

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

Microsoft Graph webhook server. Requires same SharePoint/Goodmem env as `sync_once.py` plus `GRAPH_NOTIFICATION_URL` and `GRAPH_CLIENT_STATE`. Optional: `GRAPH_DELTA_TOKEN_FILE` — path to file where the Graph delta link is persisted (default: `.graph_delta_link` in the script directory).

**Sync strategy:** On each **root** change notification the server uses the **Graph driveItem delta API** to fetch only changes since the last sync and applies them to Goodmem (no full tree list). A **full list vs Goodmem** sync runs (1) **on startup**, (2) **on OAuth token refresh**, and (3) **on subscription renew** to repair drift. After each full sync the current delta link is saved for the next delta sync.

**On startup:** (1) The server runs a **one-time full sync** (same logic as `sync_once.py`) in a background thread, then persists the Graph delta link for future incremental syncs. (2) A background **subscription renewal loop** creates a Graph subscription if none exists (using `GRAPH_NOTIFICATION_URL`), or renews the existing one via PATCH **before it expires** using the `expirationDateTime` returned by Graph (sleeps until near expiry, then renews and reschedules from the new expiration). After each renewal it queues a full list vs Goodmem sync.

**Commands:** `listener.py create-subscription` creates or renews the subscription manually (same PATCH-if-exists behavior). Useful if renewal failed or you want to renew ahead of the loop.

The server exposes **GET /activity** with an in-memory log of recent events. Use **`watch_listener.py`** locally to poll and print this log; use `.env` (default) or `--env-file` for `GRAPH_NOTIFICATION_URL`, or pass the listener base URL as an argument.

## How file diff is computed

We keep SharePoint and Goodmem in sync by computing three **UUID-level diff sets**—`to_add_uuids`, `to_update_uuids`, and `to_delete_uuids`—using **set operations on UUIDs** and Goodmem’s **batch-get** API. For update decisions, timestamps are compared only using the SharePoint value we store in Goodmem metadata (not Goodmem’s `updatedAt`), so both sides use the same clock. The three diff sets, `to_{add,update,delete}_uuids`, are UUIDs of files that need to be (potentially) added to Goodmem, updated in Goodmem, or deleted from Goodmem respectively.

Each SharePoint file is mapped to a **deterministic UUID** (i.e. UUID v5 from the file id) via the function `uuid_from_file_id`. That UUID is used as Goodmem’s `memoryId` when creating a memory. Same file ⇒ same UUID ⇒ no duplicate memories for the same file, and we never need to search Goodmem by `metadata.id` to find a memory. Thanks to the deterministic UUID, we can always locate the memory for a given SharePoint file.

### Full sync (e.g. startup)

1. **SharePoint:** Fetch all files → build `sharepoint_by_id: Dict` (file_id → file_info) and `sharepoint_uuids` = set of UUIDs computed for all `file_id`  in `sharepoint_by_id`.
2. **Goodmem:** List the space once → `goodmem_uuids` = set of memory IDs in that space.
3. **Set operations:**
   - `to_add_uuids` = `sharepoint_uuids` − `goodmem_uuids` (files in SharePoint, not in Goodmem).
   - `to_delete_uuids` = `goodmem_uuids` − `sharepoint_uuids` (files in Goodmem, not in SharePoint).
   - `both_uuids` = `sharepoint_uuids` ∩ `goodmem_uuids` (files in both; need timestamp to decide update).
4. Find UUIDs of memories that need to be updated (i.e. `to_update_uuids`) from `both_uuids`:
   - Call Goodmem memory batch-get API for all UUIDs in `both_uuids` to get metadata for each memory.
   - For each memory, read `modified_datetime` in metadata (the SharePoint timestamp stored at ingestion) and compare it with the current timestamp of the same file in `sharepoint_by_id`. If the stored value is **older** than the current SharePoint timestamp, add the memory UUID to `to_update_uuids`. If **equal**, skip (file unchanged). If the stored value is **newer** than the current SharePoint timestamp, report an error — it cannot happen.

### Delta sync (after a Graph notification)

The Graph delta API indicates whether a file change is a deletion or a non-deletion. The non-deletion case covers both addition and update, but the Graph API does not distinguish between them. We must distinguish them because update requires deletion before addition.

1. **Graph delta API:** Page through all changes (delta API has pagination) → build `delta_by_id: Dict` (file_id → file_info) for changed files.
2. For all files labeled as deleted by the Graph delta API, compute their UUIDs and add them to the set `to_delete_uuids`.
3. For all remaining (non-deleted but changed) files, compute their UUIDs and add them to the set `to_add_or_update_uuids`. We still need to classify each as add or update.
4. Call Goodmem memory batch-get API for all UUIDs in `to_add_or_update_uuids`.
5. For each batch-get result: if a memory is **found**, add its UUID to `to_update_uuids` and bufffer its metadata; otherwise (not found), add it to `to_add_uuids`.
6. **Checkpoint:** For each file whose UUID is in `to_update_uuids`, the timestamp in Goodmem metadata (from batch-get) must be **older** than the timestamp of that file in `delta_by_id`. If any file has Goodmem timestamp ≥ SharePoint timestamp, raise an error.
7. (The syncing section below describes how UUID sets are turned into the action lists and applied.)

## How file diff is synced 

We take the UUID-level diff (`to_add_uuids`, `to_update_uuids`, `to_delete_uuids`), map it into action lists, merge pending syncs, resolve conflicts, then apply in a fixed order. SharePoint is the source of truth; we do not use order or timestamps—only current membership and a single re-check per conflicting file_id.

### Collecting info for sync actions

Each UUID set is projected into one of three action lists the sync loop iterates over. 

- **`to_{add,update}_uuids` → `to_{add,update}`**: `list[dict[str, Any]]` where each dict is a SharePoint `file_info` with **string keys** like `id`, `name`, `relative_path`, `mime_type`, `download_url`, `modified_datetime` (values are typically strings/URLs/timestamps from Graph).
- **`to_delete_uuids` → `to_remove`**: `list[dict[str, Any]]` where each dit is a SharePoint `file_info` shaped like `id`, `name`, `relative_path` (metadata from Goodmem when available).

**Both the UUID-level diff sets (`to_{add,update,delete}_uuids`) and action lists (`to_{add,update,remove}`) only reflect the current state.** 

When a sync operation fails, its `file_id` is stored in one of three pending sets: `_pending_add_file_ids`, `_pending_update_file_ids`, or `_pending_remove_file_ids`. On the next sync we build **two action lists**: (1) the current diff (`to_add` / `to_update` / `to_remove`) and (2) a pending-retry list rebuilt by re-fetching SharePoint `file_info` by `file_id`. We then merge the pending-retry items into the current diff lists before conflict resolution and apply.

### Conflict resolution (one action per file_id)

After merging, the same file_id can appear in more than one list (e.g. in `to_add` and `to_remove`). Applying both would leave Goodmem out of sync (e.g. add then remove for a file deleted on SharePoint). Before applying, a **conflict-resolution** step runs:

1. **Find conflicts:** Collect every file_id that appears in at least two of `to_add`, `to_update`, `to_remove`.
2. **Re-validate with SharePoint:** For each such file_id, call `get_file_by_id(drive_id, file_id)` once.
3. **One action per file:**
   - If the file **exists** on SharePoint (and is supported): keep it only in `to_add` or `to_update` (remove it from `to_remove`). If in both add and update, keep only in `to_update` (update is delete-then-add, so it works even when the memory was never added).
   - If the file **does not exist** on SharePoint: keep it only in `to_remove` (remove it from `to_add` and `to_update`). Discard that file_id from pending add/update so we do not retry adding a deleted file.
   - If re-validation fails (e.g. network): leave the lists unchanged for that file_id; apply order **remove → add → update** then determines the outcome.

**Source of truth:** SharePoint. Conflict resolution enforces a single action per file_id from current SharePoint state, so we never apply contradictory add and remove in the same run. Together with the fixed apply order and re-validation at merge time (for pending add/update), this keeps the six collections safe and Goodmem consistent with SharePoint.


### Apply the sync

Apply order: **remove**, then **add**, then **update**.


**Full sync apply (pseudocode):**

```python
for uuid in to_delete_uuids:
    delete_memory(uuid)

for file_id, file_info in sharepoint_by_id.items():
   if uuid_from_file_id(file_id) in to_add_uuids:
       create_memory(..., memoryId=uuid_from_file_id(file_id))

   elif uuid_from_file_id(file_id) in to_update_uuids:
       delete_memory(uuid_from_file_id(file_id))
       create_memory(..., memoryId=uuid_from_file_id(file_id))
```

**Delta sync apply (pseudocode):**

```python
for uuid in to_delete_uuids:
    delete_memory(uuid)

for file_id, file_info in delta_by_id.items():
   if uuid_from_file_id(file_id) in to_add_uuids:
       create_memory(..., memoryId=uuid_from_file_id(file_id))

   elif uuid_from_file_id(file_id) in to_update_uuids:
       delete_memory(uuid_from_file_id(file_id))
       create_memory(..., memoryId=uuid_from_file_id(file_id))
```

### Why sets are safe (no order or timestamps)

- **Diff lists:** A point-in-time snapshot of “what to do.” We only need *which* file_ids need add, update, or remove—not the order they were discovered. Applying order inside each list does not affect correctness.
- **Pending sets:** “These file_ids failed before; retry next sync.” We do not use “which pending list was written first” or “when it failed.” Conflicts (same file_id in more than one list) are resolved by re-checking **current SharePoint state**, not by history. Sets (unordered, no timestamps) are sufficient.


## Known limitations

- It takes about 30 seconds for the update to reach our listener from Azure. This is a limitation of the Microsoft Graph API.
- The listener uses the Graph driveItem delta API on root notifications so only changes are fetched; full list vs Goodmem runs at startup, OAuth refresh, and subscription renew. 

##  Roadmap

* Use TOML-based environment file than .env.