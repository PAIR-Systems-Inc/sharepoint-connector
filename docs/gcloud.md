# Knowledge about Google cloud

## Gcloud CLI authentication 

Gcloud CLI supports multiple profiles. Each profile has a default account (email) and a default project.
Operations on profiles are done via the `gcloud config` subcommand. 

A profile can be created using `gcloud config configurations create <profile_name>` command. Once this command is executed, subsequent gcloud commands, including configuration of this profile, will use the newly created profile until another profile is selected. To configure the profile, you can use `gcloud config set account <account_email>` and `gcloud config set project <project_id>` commands.
To switch between profiles, you can use `gcloud config configurations activate <profile_name>` command. To list all available profiles, you can use `gcloud config configurations list` command.
To know details of the current profile, you can use `gcloud config list` command.

If you do not want back and forth switching between profiles, you can attach the flag `--configuration <profile_name>` to any gcloud command to use a specific profile for that command.

## Application Default Credentials (ADC)

ADC is how Google's **client libraries** (the SDKs your *application code* uses)
find credentials automatically, so you never hardcode a key in code. When an app
authenticates "via ADC", the library searches, in order:

1. the **`GOOGLE_APPLICATION_CREDENTIALS`** environment variable — if set, it is a
   path to a credentials JSON file, and that file is used;
2. the **well-known gcloud ADC file**, `~/.config/gcloud/application_default_credentials.json`,
   created by `gcloud auth application-default login`;
3. on a GCP host (VM, Cloud Run, …), the **attached service account** via the
   metadata server.

The ADC JSON is one of three shapes: a **service-account key**
(`"type": "service_account"`), your **user login** (`"type": "authorized_user"` —
a refresh token from `application-default login`), or an
**impersonation config** (`"type": "impersonated_service_account"` — your login
plus a service account to impersonate).

**ADC is separate from the gcloud CLI account.** The gcloud *profile* (§ above)
decides who `gcloud` commands act as; ADC decides who your *application code* acts
as. Setting `GOOGLE_APPLICATION_CREDENTIALS` affects apps/SDKs, **not** `gcloud`
itself. They're independent — you can have `gcloud` on one identity and an app on
another at the same time.

## Enabling and authenticating Google Drive

The connector reads a Google **Shared Drive** through the Drive API using
**Application Default Credentials (ADC)** — see above. There are three ways to provide credentials; which one you can use
depends on your org's policy:

| Option | How the connector authenticates | When to use |
|---|---|---|
| **Service-account key** | `GDRIVE_SA_JSON_FILE` / `GDRIVE_SA_JSON` | Best for unattended deploys — **but** many orgs block downloadable keys (`iam.managed.disableServiceAccountKeyCreation`). |
| **Service-account impersonation (ADC)** | ADC file that impersonates a service account | Keyless; works when the org forbids keys. **What we use** for local runs. |
| **Own OAuth client (ADC)** | ADC created with your own OAuth client id | Alternative keyless route; more console setup. |

The gcloud *default* OAuth client can no longer grant the Drive scope, so a plain
`gcloud auth application-default login --scopes=…drive…` is rejected. That leaves
**impersonation** (below) or your own OAuth client.

### A. One-time GCP + Drive setup (per project / drive, not per machine)

Pick the GCP project + Google account (e.g. a `gcloud` profile — see above), then:

```bash
PROJECT=your-gcp-project
SA_EMAIL="goodmem-connector@${PROJECT}.iam.gserviceaccount.com"
YOU=you@your-domain.com                 # the account that will impersonate the SA

# 1. Enable the Drive API
gcloud services enable drive.googleapis.com --project="$PROJECT"

# 2. Create the service account (the Drive-reading identity)
gcloud iam service-accounts create goodmem-connector \
  --project="$PROJECT" --display-name="Goodmem Drive connector (read-only)"

# 3. Let your account impersonate it (an IAM binding — NOT a downloadable key,
#    so it's allowed even when key creation is blocked)
gcloud iam service-accounts add-iam-policy-binding "$SA_EMAIL" \
  --member="user:${YOU}" --role="roles/iam.serviceAccountTokenCreator" \
  --project="$PROJECT"
```

Then in the browser (Workspace required for Shared Drives):

1. [drive.google.com](https://drive.google.com) → **Shared drives → New**.
2. **Manage members** → add `SA_EMAIL` as **Viewer** (the SA is the reader).
3. Add your files.
4. Copy the **Drive ID** from the URL `…/drive/folders/<DRIVE_ID>`.

### B. Per-machine: create the ADC credentials

On each machine that runs the connector, do an impersonation login — it mints an
ADC that reads the Drive as the SA (the login itself uses only the allowed
`cloud-platform` scope; the Drive scope is applied to the *impersonated* SA token):

```bash
gcloud auth application-default login \
  --impersonate-service-account="$SA_EMAIL" \
  --scopes=https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/cloud-platform
```

> Keep the `--scopes` value on **one line** — terminals wrapping the URLs break it.

This writes the **default** ADC file (`~/.config/gcloud/application_default_credentials.json`);
`application-default login` has no flag to target a different path. What you do next
depends on whether anything else on the machine also uses ADC:

**Default case — the connector is the only ADC app (e.g. a standalone listener
instance): do nothing.** The connector reads the default ADC automatically — no
`GOOGLE_APPLICATION_CREDENTIALS`, no separate file. Simplest, and what we use.
Caveat: the impersonation creds now live *only* in the default ADC file, so if you
later run a plain `gcloud auth application-default login` (no `--impersonate…`) it
overwrites them — just re-run the impersonation login to restore.

**Other apps also use ADC:** keep the connector's creds in a *separate* file so the
default ADC stays free for them, and point the connector at it (`GOOGLE_APPLICATION_CREDENTIALS`,
see § C). Two ways to get that file:

```bash
# (a) copy the default out after logging in:
cp ~/.config/gcloud/application_default_credentials.json ~/.config/gcloud/adc-gdrive.json
gcloud auth application-default login          # then restore your normal default ADC (no --impersonate…)

# (b) or write it straight to an isolated dir at login time (no copy, never touches the default):
CLOUDSDK_CONFIG=~/.config/gcloud-gdrive gcloud auth application-default login \
  --impersonate-service-account="$SA_EMAIL" \
  --scopes=https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/cloud-platform
#   → ~/.config/gcloud-gdrive/application_default_credentials.json
```

### C. Point the connector at it (`.env`)

```dotenv
SOURCE=gdrive
GDRIVE_DRIVE_ID=<DRIVE_ID>

# Credentials:
#   • Default case (§ B) — the connector uses the DEFAULT ADC automatically, so
#     set NOTHING here.
#   • Separate-file case — point at it (absolute path recommended):
# GOOGLE_APPLICATION_CREDENTIALS=/home/you/.config/gcloud/adc-gdrive.json
#   • Service-account key (only if your org allows keys):
# GDRIVE_SA_JSON_FILE=/home/you/keys/goodmem-connector.json

# Goodmem (as for any sync):
GOODMEM_BASE_URL=https://your-goodmem
GOODMEM_API_KEY=...
GOODMEM_SPACE_ID=...        # or leave unset to create GDrive_<DRIVE_ID>
```

In the default case the connector falls back to ADC on its own — no credential
variable needed. If you do use a separate file, `GOOGLE_APPLICATION_CREDENTIALS` is
a standard Google SDK variable, not a connector one, but the connector loads `.env`
into the process environment before the Drive client starts, so setting it in `.env`
works (equally, set it inline or export it in the shell; a real shell env var wins
over `.env`). Keep any credentials file out of git — the repo's `.gitignore` covers
`*adc*.json`, `*-sa.json`, and `secrets/`.

Then run: `./connector sync-once --source gdrive` (or `serve` for the listener).

### D. Unattended production auth (deploys)

The three methods above behave very differently for an **unattended** deploy — a
long-running listener with no human present to log in:

| Method | Unattended? | Limitation |
|---|---|---|
| **ADC impersonation** (§ B) | ❌ No | `gcloud auth application-default login --impersonate-service-account` is **interactive** — it opens a browser and mints a *user* refresh token. Great for a laptop or a hands-on GCP VM; a headless container can't perform it, and the credential is tied to your user session. Use it for local runs and one-off syncs, not a deployed listener. |
| **Service-account key** (`GDRIVE_SA_JSON` / `_FILE`) | ✅ Yes | The straightforward unattended path — **but** many orgs (including ours) block key creation via `iam.managed.disableServiceAccountKeyCreation`, and a downloadable key is a long-lived secret you must store, rotate, and guard. If your org permits keys, ship it as a secret (e.g. a Fly secret) and you're done. |
| **GCP-attached service account** (metadata server) | ✅ Yes | Zero secrets on disk — but only when the listener **runs on GCP** (Cloud Run / GKE / GCE) with the SA attached to the workload. Not available off-GCP. |
| **Workload Identity Federation (WIF)** | ✅ Yes | Keyless *even off-GCP*: the host platform's own OIDC token is exchanged for short-lived SA credentials, so there's no downloadable key — it satisfies the key-block policy. Costs more setup (an identity pool + provider + a credential-config JSON) and requires the platform to issue an OIDC token to the workload. |

**Pick by where the listener runs:**

- **On GCP** (Cloud Run / GKE / GCE): attach the Drive service account to the
  workload — nothing to store or rotate. Simplest.
- **Off GCP, keys allowed**: a service-account key in `GDRIVE_SA_JSON` (as a
  platform secret).
- **Off GCP, keys blocked** (our situation): **workload identity federation** is
  the only keyless, unattended option.

**Workload identity federation, in brief.** Create a workload identity pool +
provider that trusts your platform's OIDC issuer; let the pool's principal
impersonate the Drive SA (grant it `roles/iam.serviceAccountTokenCreator`, or bind
it directly on the SA); then generate an ADC *credential-configuration* file
(`gcloud iam workload-identity-pools create-cred-config …`) and point
`GOOGLE_APPLICATION_CREDENTIALS` at it. At runtime the Google SDK reads that config,
exchanges the platform's OIDC token for a short-lived Drive-scoped SA token, and no
key ever touches disk. Caveat: the platform must actually issue a workload OIDC
token — GCP, GitHub Actions, AWS, and Azure do; a plain **Fly.io** app does not
today, so on Fly the realistic choices remain a **key** (if the org allowed one) or
running the listener on a **GCP host**. Track this as the blocker for a fully
keyless off-GCP gdrive deploy.

### E. Push vs poll for the listener

The gdrive listener defaults to **poll mode** (`SYNC_POLL_MINUTES`, default 2), which
runs a delta sync on a timer and needs **no public webhook**. Google's push channels
(`changes.watch`) require a **domain-verified** HTTPS endpoint — a throwaway
`*.fly.dev` host can't satisfy it — so push mode is only worth it when you own a
verifiable domain and want sub-minute latency. Poll mode removes that blocker
entirely; it is the recommended default for gdrive.

