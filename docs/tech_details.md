# Technical details

Internals of the SharePoint → Goodmem connector: the clients, the sync scripts, and how the file diff is computed and applied.

See also: [README.md](../README.md) (overview, permissions) · [usage.md](usage.md) (running sync, deploying, watching activity).

## `sharepoint_client.py`

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
  "created_by": "ABC XYZ",
  "modified_by": "PQR LMN",
  "mime_type": "application/pdf",
  "file_hash": null
}
```

## `goodmem_client.py`

Goodmem API client. Do not load credentials from `.env` except under `__main__`.

- Create space, find space by name, list/delete memories, ingest files.
- `find_space_by_name(space_name)` returns the space ID or `None`; `sync_once.py` creates a new space when `None`.

## `sync_once.py`

One-time full sync from SharePoint to Goodmem. Env file: `--env-file` to specify; otherwise uses `.env`. (`.env.example` is only a template and is never loaded.) Fetches all files from the default drive (root, recursive) and syncs them. Creates a space from the site URL if needed (`SharePoint_{Org}_{Site}`). Rules:

1. File not in Goodmem → ingest.
2. File in Goodmem with newer `modified_datetime` → delete old memory, ingest again.
3. File in Goodmem but no longer in SharePoint → delete memory.

**Space name:** Derived from `SHAREPOINT_SITE_URL`: org = host before `.sharepoint.com`, site = first segment after `sites/`; space name = `SharePoint_{Org}_{Site}`.

**Supported MIME types:** text/*, PDF, RTF, Word (.doc/.docx), types containing "+xml" or "json". Others are skipped.

## `listener.py`

Microsoft Graph webhook server. Requires same SharePoint/Goodmem env as `sync_once.py` plus `GRAPH_NOTIFICATION_URL` and `GRAPH_CLIENT_STATE`. Optional: `GRAPH_DELTA_TOKEN_FILE` — path to file where the Graph delta link is persisted (default: `.graph_delta_link` in the script directory).

**Sync strategy:** On each **root** change notification the server uses the **Graph driveItem delta API** to fetch only changes since the last sync and applies them to Goodmem (no full tree list). A **full list vs Goodmem** sync runs (1) **on startup**, (2) **on OAuth token refresh**, and (3) **on subscription renew** to repair drift. After each full sync the current delta link is saved for the next delta sync.

**On startup:** (1) The server runs a **one-time full sync** (same logic as `sync_once.py`) in a background thread, then persists the Graph delta link for future incremental syncs. (2) A background **subscription renewal loop** creates a Graph subscription if none exists (using `GRAPH_NOTIFICATION_URL`), or renews the existing one via PATCH **before it expires** using the `expirationDateTime` returned by Graph (sleeps until near expiry, then renews and reschedules from the new expiration). After each renewal it queues a full list vs Goodmem sync.

**Commands:** `listener.py create-subscription` creates or renews the subscription manually (same PATCH-if-exists behavior). Useful if renewal failed or you want to renew ahead of the loop.

The server exposes **GET /activity** with an in-memory log of recent events. Use **`watch_listener.py`** locally to poll and print this log; use `.env` (default) or `--env-file` for `GRAPH_NOTIFICATION_URL`, or pass the listener base URL as an argument.

**References (Microsoft):** [subscription resource type](https://learn.microsoft.com/en-us/graph/api/resources/subscription) · [Change notifications (webhooks)](https://learn.microsoft.com/en-us/graph/change-notifications-overview)

## Deployment (`deploy_fly_io.sh`)

Deploys the listener to Fly.io, optionally provisioning Goodmem too. With no flag it deploys only the listener; `--hands-free` runs a Goodmem phase first. (`--goodmem-only` runs just the Goodmem phase; `--generate-only` renders the Fly config templates without deploying.) Run `./deploy_fly_io.sh --help` for all modes.

**Listener deploy (every run):**
1. Generates `fly_io.toml` from `fly_io.toml.template` (app name and region from the env file).
2. Creates the Fly app `<FLY_CLUSTER>-listener` if it doesn't exist, or reuses it. No image is deployed yet; this just establishes the app URL.
3. Writes `GRAPH_NOTIFICATION_URL=https://<app>.fly.dev/sync/webhook` into the env file, and generates a random `GRAPH_CLIENT_STATE` if one isn't already set (reused on re-deploy so it stays stable — changing it would break validation of an existing subscription).
4. Imports the env file as Fly secrets, deploys one machine (`--ha=false`), then scales to 1 with auto-stop disabled so the listener stays up for webhooks.
5. On boot the listener runs a full sync, then a background loop creates the Graph subscription if none exists (using `GRAPH_NOTIFICATION_URL`) and auto-renews it.

**Goodmem phase (`--hands-free` only) — runs before the listener deploy:**
1. Runs the [get.goodmem.ai/flyio](https://get.goodmem.ai/flyio) installer (app `<FLY_CLUSTER>-goodmem`, tier small, region from the env file) and scales it to one machine.
2. Writes `GOODMEM_BASE_URL` and `GOODMEM_API_KEY` into the env file.
3. Creates a `text-embedding-3-small` embedder via the Goodmem API (requires `OPENAI_API_KEY`).

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

**Add/ingest and Goodmem processingStatus.** A successful add is only when Goodmem returns **200** and **processingStatus: COMPLETED**. Non-200 (request never accepted) → `file_id` stays in **pending_add** (retry as add). 200 with **processingStatus: FAILED** (request accepted but ingest failed, e.g. file corruption or backend error) → `file_id` goes to **pending_update** so we retry as delete-then-add. The insert response usually returns **PENDING** because ingestion is async; we then poll [Get memory by ID](https://docs.goodmem.ai/docs/reference/api-reference/rest/memories/getMemory/) until processingStatus is COMPLETED or FAILED (or timeout). While PENDING we log "Received by Goodmem. Pending processing." Only COMPLETED is treated as success; timeout still PENDING → pending_add for next sync.

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
