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

### B. Per-machine: create the ADC credentials file

On each machine that runs the connector, mint an ADC file that impersonates the SA
with Drive scope (the login itself only uses the allowed `cloud-platform` scope;
the Drive scope is applied to the *impersonated* SA token):

`gcloud auth application-default login` has **no flag to choose the output file** —
it always writes to the default ADC path in the *active gcloud config directory*.
So there are three ways to end up with a dedicated gdrive credentials file:

**Option 1 — isolated config dir (cleanest; no copy, never touches your default ADC).**
Point `CLOUDSDK_CONFIG` at a separate directory just for the login:

```bash
CLOUDSDK_CONFIG=~/.config/gcloud-gdrive gcloud auth application-default login \
  --impersonate-service-account="$SA_EMAIL" \
  --scopes=https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/cloud-platform
# → writes ~/.config/gcloud-gdrive/application_default_credentials.json
# Point the connector there (see § C):
#   GOOGLE_APPLICATION_CREDENTIALS=~/.config/gcloud-gdrive/application_default_credentials.json
```

**Option 2 — login normally, then copy to a stable name.** Because the login
overwrites the *default* ADC (which other tools may rely on), copy it out:

```bash
gcloud auth application-default login \
  --impersonate-service-account="$SA_EMAIL" \
  --scopes=https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/cloud-platform
cp ~/.config/gcloud/application_default_credentials.json ~/.config/gcloud/adc-gdrive.json
# then restore your normal default ADC:  gcloud auth application-default login   (no --impersonate…)
```

**Option 3 — no separate file at all.** If this machine runs nothing else that
uses ADC, just do the impersonation login and let it be the *default* ADC — the
connector reads the default with no `GOOGLE_APPLICATION_CREDENTIALS` needed. Simplest,
but every ADC app on the box then impersonates the SA.

> Keep the `--scopes` URL on **one line** — terminals wrapping it break the list.
> Option 1 is recommended: the copy is only needed because the login can't target a
> file, and an isolated `CLOUDSDK_CONFIG` sidesteps both the copy and clobbering
> your default ADC.

### C. Point the connector at it (`.env`)

```dotenv
SOURCE=gdrive
GDRIVE_DRIVE_ID=<DRIVE_ID>
# Credentials — choose ONE:
#   (a) impersonation / user ADC:
GOOGLE_APPLICATION_CREDENTIALS=/home/you/.config/gcloud/adc-gdrive.json
#   (b) a service-account key file (only if your org allows keys):
# GDRIVE_SA_JSON_FILE=/home/you/keys/goodmem-connector.json

# Goodmem (as for any sync):
GOODMEM_BASE_URL=https://your-goodmem
GOODMEM_API_KEY=...
GOODMEM_SPACE_ID=...        # or leave unset to create GDrive_<DRIVE_ID>
```

`GOOGLE_APPLICATION_CREDENTIALS` is a standard Google SDK variable, not a connector
one — but the connector loads `.env` into the process environment before the Drive
client starts, so setting it in `.env` works. You can equally set it inline
(`GOOGLE_APPLICATION_CREDENTIALS=… ./connector sync-once --source gdrive`) or export
it in the shell. A real shell env var wins over `.env`.

Then run: `./connector sync-once --source gdrive` (or `serve` for the listener).

> **Deploys (Fly, etc.):** ADC impersonation needs an interactive login, so it is
> for local/GCP-hosted runs. An unattended off-GCP deploy needs a service-account
> **key** (option a → `GDRIVE_SA_JSON`), which requires an org that permits keys,
> or workload identity federation. The event-triggered listener also needs a
> **domain-verified** HTTPS webhook (Google requirement for `changes.watch`), which
> a throwaway `*.fly.dev` host cannot satisfy — use a domain you own.

