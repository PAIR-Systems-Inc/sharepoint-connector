# Graph listener setup (step-by-step)

Get the Graph listener running on a web server (Ubuntu or Railway/Fly.io). The endpoint must be **HTTPS** and **publicly reachable**.

**Before you start:** Ask IT to grant the app permissions in [permission.md](permission.md).

---

## What to copy

Copy these into one directory on the server:

- `graph_listener.py`
- `sharepoint_client.py`
- `goodmem_client.py`
- `requirements.txt`
- `.env` (create from `.env.example` and fill in)

---

## Setup on Ubuntu (or any Linux server)

**Step 1.** Copy the files to the server (e.g. `scp`, `rsync`, or git clone).

**Step 2.** Go to the project directory and create a virtualenv:

```bash
cd /path/to/copied/files
python3 -m venv .venv
source .venv/bin/activate
```

**Step 3.** Install dependencies:

```bash
pip install -r requirements.txt
```

**Step 4.** Create `.env` from the example and set all required variables:

```bash
cp .env.example .env
nano .env
```

Set:

- `SHAREPOINT_CLIENT_ID`, `SHAREPOINT_TENANT_ID`, `SHAREPOINT_CLIENT_SECRET`, `SHAREPOINT_SITE_URL`
- `GOODMEM_BASE_URL`, `GOODMEM_API_KEY`
- `SYNC_NOTIFICATION_URL` = `https://YOUR-PUBLIC-DOMAIN/sync/webhook` (your server’s HTTPS URL + `/sync/webhook`)
- `SYNC_CLIENT_STATE` = any secret string
- `SYNC_PORT` = port the app will listen on (e.g. `5000`), or leave unset if your reverse proxy sets `PORT`

**Step 5.** Put the app behind HTTPS. The URL in `SYNC_NOTIFICATION_URL` must be HTTPS.

- **Option A:** Nginx (or Caddy) + Let’s Encrypt. Configure the proxy to forward to `http://127.0.0.1:5000` (or your `SYNC_PORT`).
- **Option B:** Run `ngrok http 5000` and set `SYNC_NOTIFICATION_URL` to the ngrok HTTPS URL + `/sync/webhook`.

**Step 6.** Start the listener. From the project directory with the venv activated:

```bash
source .venv/bin/activate
python graph_listener.py server
```

Keep it running (e.g. in a terminal, or run it under systemd so it restarts on reboot).

**Step 7.** Create the Graph subscription (run once after the server is up and reachable at `SYNC_NOTIFICATION_URL`):

```bash
source .venv/bin/activate
python graph_listener.py create-subscription
```

You can run Step 7 on the server or from your laptop if `.env` is the same.

**Step 8.** Renew before expiry: subscriptions expire in a few days. Run `python graph_listener.py create-subscription` again (e.g. cron every 2 days or manually).

---

## Setup on Railway or Fly.io

**Step 1.** Copy the files into the app (e.g. push the repo or upload the same files listed above).

**Step 2.** Set the start command so the app runs the listener:

- **Railway:** Service → Settings → set start command to: `python graph_listener.py server`
- **Fly.io:** Set the process command to: `python graph_listener.py server`

**Step 3.** In the Railway/Fly.io dashboard, add these environment variables:

- `SHAREPOINT_CLIENT_ID`, `SHAREPOINT_TENANT_ID`, `SHAREPOINT_CLIENT_SECRET`, `SHAREPOINT_SITE_URL`
- `GOODMEM_BASE_URL`, `GOODMEM_API_KEY`
- `SYNC_NOTIFICATION_URL` = `https://YOUR-APP-URL/sync/webhook` (the app’s public HTTPS URL + `/sync/webhook`)
- `SYNC_CLIENT_STATE` = any secret string

Do not set `SYNC_PORT`; the platform sets `PORT` and the script uses it.

**Step 4.** Deploy. Wait until the app is live and the URL in `SYNC_NOTIFICATION_URL` is reachable.

**Step 5.** Create the Graph subscription. From your machine (same project and `.env`, or same env values):

```bash
python graph_listener.py create-subscription
```

**Step 6.** Renew before expiry: run `python graph_listener.py create-subscription` again (e.g. every 2 days or manually).
