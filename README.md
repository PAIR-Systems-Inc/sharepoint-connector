# SharePoint Connector for Goodmem

Sync files on a SharePoint site to a Goodmem memory space. Supports a one-time full sync and ongoing sync (reflecting any changes -- add, update, or delete to the SharePoint files -- to the Goodmem memory space) via Microsoft Graph webhooks.

**Files (repo layout):**

```
sharepoint/
├── sharepoint_client.py   # Fetches files from SharePoint (Graph API).
├── goodmem_client.py     # Goodmem API client: spaces, ingest, list/delete memories.
├── sync_once.py          # One-time full sync: copies all files from SharePoint to Goodmem.
├── listener.py           # Graph webhook server: receives change notifications, syncs add/update/delete to Goodmem.
├── requirements.txt      # Python dependencies. Needed for many deployment platforms despite uv.
├── Dockerfile            # Used by Fly.io to run listener.py.
├── fly.toml
├── .env.example
└── .env                  # Your credentials (do not commit).
```

---

## Setup

**Full flow:** (1) Install GoodMem and write down credentials → (2) Obtain permissions and set `.env` (leave `SYNC_NOTIFICATION_URL` for later) → (3) Run initial sync once (`sync_once.py`) → (4) Deploy and run the listener for ongoing changes.

Follow these four steps in order.

### Step 1: Install GoodMem and write down credentials

Follow instructions [here](https://goodmem.ai/quick-start), to install GoodMem and save the REST URL (`GOODMEM_BASE_URL`) and root API key (`GOODMEM_API_KEY`). You will need them for `.env` in Step 2. 

When it finishes, the script prints:

- **REST ENDPOINT URL** — e.g. `https://zesty-sprout.goodmem.ai` → use as `GOODMEM_BASE_URL`
- **Root API key** - e.g. `gm_wwzolnjitflg6iz55vuvkma2ea` → use as `GOODMEM_API_KEY`

Save both; you need them for Step 2.

---

### Step 2: Obtain permissions and set .env (leave SYNC_NOTIFICATION_URL for later)

1. **Ask IT** to grant the Azure AD app permissions. Share [permission.md](permission.md) with them.

2. **Copy and edit `.env`:**
   ```bash
   cp .env.example .env
   ```
   Fill in everything **except** `SYNC_NOTIFICATION_URL` (you set that in Step 4 when the listener app exists):

3. **Install dependencies** (optional):
   Only needed if you do not use [uv](https://docs.astral.sh/uv/) — uv installs on the fly when you run the scripts.

   ```bash
   pip install -r requirements.txt
   ```
---

### Step 3: Initial sync (one-time, or whenever needed, manually or periodically)

Backfill existing SharePoint files into Goodmem. Run once when Goodmem is fresh and SharePoint already has files; after that, the listener (Step 4) handles ongoing changes. If you want to backfill again, run the script again.

```bash
python sync_once.py
```

Or with uv: `uv run sync_once.py` (or `chmod +x sync_once.py` and `./sync_once.py`).

---

### Step 4: Deploying the listener (ongoing sync via Graph webhooks)

The listener app will keep the Sharepoint and Goodmem in sync whenever there are changes (add, modify, or delete) to the SharePoint files.

The instruction below is for deploying the listener app to Fly.io. Support for other platforms will come soon. 

#### Using Fly.io 

The **listener** is a web server that receives Microsoft Graph change notifications (HTTPS). Graph will only call **HTTPS** and **publicly reachable** URLs; subscription creation fails without HTTPS.

**4.0 — First time only: create the listener app and set SYNC_NOTIFICATION_URL**

If the listener Fly app does not exist yet, 
pick a name for it (e.g., `your-listener-app` in the placeholder below). 
Then run the commands below to create the app and set the `SYNC_NOTIFICATION_URL` in `.env`. 
If someone else has already picked that name, it will fail so you know to pick another name. 
After picking a new name, run the commands again.

```bash
APP_NAME=your-listener-app   
fly launch --no-deploy --name "$APP_NAME" 
sed -i "s|^SYNC_NOTIFICATION_URL=.*|SYNC_NOTIFICATION_URL=https://$APP_NAME.fly.dev/sync/webhook|" .env
```

**4.1 — Import secrets from `.env`**

```bash
fly secrets import < .env
```

**4.2 — Deploy**

```bash
fly deploy
```

The project’s `fly.toml` sets `auto_stop_machines = 'off'` and `min_machines_running = 1` so the listener stays running and can receive Graph webhooks even when there is no traffic. If you change these, the app may be stopped or suspended when idle and will miss notifications until you restart it.

**4.3 — Create/Renew the Graph subscription** (from repo root; `.env` must be set, including `SYNC_NOTIFICATION_URL`)

Create the Graph subscription if it does not exist, or renew it (extend expiration) if one already exists for the same drive and `SYNC_CLIENT_STATE`. 
Keep the subscription renewed by running this script periodically (e.g. every 2 days). When the subscription eventually expires without renewal, syncing between SharePoint and Goodmem stops.

**You can run this before the current subscription expires**; the script will find the existing subscription and PATCH its expiration instead of creating a duplicate. 

```bash
python listener.py create-subscription
```

**4.4 — Restarting a suspended listener app**

Fly.io may suspend the listener app when it is idle (no traffic). In `fly apps list` the app shows **suspended** and the webhook stops receiving Graph notifications until the app is running again.

To bring the app back up, **start the suspended machine** (do not use `fly apps resume` or `fly scale count`; those do not start suspended machines). From the repo root, using your listener app name (e.g. `sharepoint`):

```bash
fly machine start $(fly machine list -a sharepoint 2>/dev/null | awk '/^[0-9a-f]{14}/ {print $1; exit}') -a sharepoint
```

Replace `sharepoint` with your Fly app name if different. Then run `fly apps list` or `fly status -a sharepoint` to confirm the app is **deployed** and the machine is **started**.

**Note:** The script creates a Graph subscription with `changeType: updated` (the only allowed value for drive root), which effectively covers add/modify/delete. When Graph sends a notification, the listener inspects the payload and performs the corresponding Goodmem action (create, delete, or delete-then-recreate for updates).

**References (Microsoft):**

- [subscription resource type](https://learn.microsoft.com/en-us/graph/api/resources/subscription) — `changeType` and drive root/list support only `updated`
- [Set up change notifications (webhooks)](https://learn.microsoft.com/en-us/graph/change-notifications-overview)

---

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

Full sync from SharePoint to Goodmem: fetches all files from the default drive (root, recursive) and syncs them. Creates a space from the site URL if needed (`SharePoint_{Org}_{Site}`). Rules:

1. File not in Goodmem → ingest.
2. File in Goodmem with newer `modified_datetime` → delete old memory, ingest again.
3. File in Goodmem but no longer in SharePoint → delete memory.

**Space name:** Derived from `SHAREPOINT_SITE_URL`: org = host before `.sharepoint.com`, site = first segment after `sites/`; space name = `SharePoint_{Org}_{Site}`.

**Supported MIME types:** text/*, PDF, RTF, Word (.doc/.docx), types containing "+xml" or "json". Others are skipped.

### `listener.py`

Microsoft Graph webhook server. Run `listener.py server` (e.g. on Fly.io via Dockerfile); run `listener.py create-subscription` to create or renew the subscription (if one already exists for the same resource and client state, it is renewed via PATCH instead of creating a duplicate). Requires same SharePoint/Goodmem env as `sync_once.py` plus `SYNC_NOTIFICATION_URL` and `SYNC_CLIENT_STATE`.

The server exposes **GET /activity** with an in-memory log of recent events (notifications received, files synced/deleted/skipped, errors). Use **`watch_listener.py`** on your local machine to poll and print this log:

```bash
python watch_listener.py https://your-listener-app.fly.dev
# or set SYNC_NOTIFICATION_URL (or LISTENER_ACTIVITY_URL) in .env and run:
python watch_listener.py
```
