# SharePoint Connector for Goodmem

Keep a Goodmem space in sync with a SharePoint site. This connector (an Azure AD app) offers two ways to sync — **manual/periodic** and **event-triggered** — shown below.

## How it works

![SharePoint connector sync modes — manual/periodic via sync_once.py, and event-triggered via a public listener with watch_listener.py monitoring and deploy_fly_io.sh provisioning Fly.io](docs/sync_architecture.svg)

**Manual / periodic sync** — `sync_once.py` runs between SharePoint and Goodmem: it pulls the current files and ingests them into the Goodmem space. Run it on demand or on a schedule (cron), from anywhere.

**Event-triggered sync** — a long-running **listener** sits between SharePoint and Goodmem. Microsoft Graph sends it a webhook on each change; the listener pulls the delta and syncs it to Goodmem. Graph requires the listener to be **publicly reachable over HTTPS/TLS** — any host works. `watch_listener.py` is an optional local tool that polls the listener's `/activity` log to monitor sync state (the listener syncs with or without it). `./deploy_fly_io.sh` is the supported way to stand this up on Fly.io: with no flag it deploys the listener (Goodmem already runs elsewhere); `--hands-free` deploys the listener and a Goodmem server together. Run `./deploy_fly_io.sh --help` to see all modes and options. (Railway support is coming.)

## Getting started

1. **Ask IT** to grant the Azure AD app permissions. Share [permission.md](docs/permission.md) with them.

2. **Set credentials in `.env`.** Create it from the template:
   ```bash
   cp .env.example .env
   ```
   Then fill in the variables for the mode you intend to run. 
   [`.env.example`](.env.example) is very self-documenting, and [usage.md](docs/usage.md) explains which variables are needed for each mode. In general, there are four groups of variables:  
   * **Azure AD & SharePoint** — always required (ask your IT for the values).
   * **Goodmem** — required for manual sync and the listener when Goodmem already exists. If you let `./deploy_fly_io.sh --hands-free` provision Goodmem for you, these get filled in automatically.
   * **Graph webhook** — event-triggered sync only. The deploy script generates `GRAPH_CLIENT_STATE` and writes `GRAPH_NOTIFICATION_URL` for you; set them by hand only for a manual (non-Fly) deployment.
   * **Fly.io** — event-triggered sync deployed via `deploy_fly_io.sh`; skip for a manual sync.
3. **Sync**. You have two options:  
   * **Manual / periodic sync** — run `python sync_once.py` on demand or on a schedule (cron). See [usage.md](docs/usage.md#manualperiodic-sync).  
   * **Event-triggered sync** — deploy the listener with `./deploy_fly_io.sh` (see [usage.md](docs/usage.md#event-triggered-auto-sync)). Optionally watch it locally with `python watch_listener.py -n 0.5`.


## Documentation

* **[usage.md](docs/usage.md)** — run a manual sync, deploy the event-triggered listener to Fly.io, and watch sync activity.
* **[tech_details.md](docs/tech_details.md)** — internals: the clients, the sync scripts, and how the file diff is computed and applied.

## Repo layout

```
sharepoint/
├── sharepoint_client.py   # Fetches files from SharePoint (Graph API).
├── goodmem_client.py     # Goodmem API client: spaces, ingest, list/delete memories.
├── sync_once.py          # One-time full sync: copies all files from SharePoint to Goodmem (uses .env; --env-file to override).
├── listener.py           # Graph webhook server: receives change notifications, syncs add/update/delete to Goodmem.
├── watch_listener.py     # Polls listener /activity; run locally to monitor sync.
├── deploy_fly_io.sh      # Deploy Goodmem and/or listener to Fly.io (recommended).
├── requirements.txt      # Python dependencies. Needed for many deployment platforms despite uv.
├── Dockerfile            # Image for the listener (referenced by fly_io.toml).
├── fly_io.toml.template            # Template for the listener's Fly config (app/region substituted by deploy script).
├── .env.example
├── .env                  # Your credentials (do not commit). Create from .env.example; deploy script writes GOODMEM_* and GRAPH_NOTIFICATION_URL.
└── docs/                 # Documentation: usage.md, tech_details.md, permission.md, and the architecture diagram (sync_architecture.svg).
```

## Roadmap

* Use TOML-based environment file than .env.
* Railway deployment support.