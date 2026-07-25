# Knowledge about Google cloud

## Gcloud CLI authentication 

Gcloud CLI supports multiple profiles. Each profile has a default account (email) and a default project.
Operations on profiles are done via the `gcloud config` subcommand. 

A profile can be created using `gcloud config configurations create <profile_name>` command. Once this command is executed, subsequent gcloud commands, including configuration of this profile, will use the newly created profile until another profile is selected. To configure the profile, you can use `gcloud config set account <account_email>` and `gcloud config set project <project_id>` commands.
To switch between profiles, you can use `gcloud config configurations activate <profile_name>` command. To list all available profiles, you can use `gcloud config configurations list` command.
To know details of the current profile, you can use `gcloud config list` command.

If you do not want back and forth switching between profiles, you can attach the flag `--configuration <profile_name>` to any gcloud command to use a specific profile for that command.

## Enabling and authenticating Google Drive

The connector reads a Google **Shared Drive** through the Drive API using
**Application Default Credentials (ADC)** — the standard way Google SDKs
authenticate. There are three ways to provide credentials; which one you can use
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

```bash
gcloud auth application-default login \
  --impersonate-service-account="$SA_EMAIL" \
  --scopes=https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/cloud-platform

# gcloud writes this to the default ADC path; copy it somewhere stable so it does
# not get overwritten by other `application-default login` runs:
cp ~/.config/gcloud/application_default_credentials.json ~/.config/gcloud/adc-gdrive.json
```

> Keep the URL on **one line** — terminals wrapping it break the scope list.
> Restore your normal default ADC afterwards with a plain
> `gcloud auth application-default login` (no `--impersonate…`); the connector uses
> the saved file via the env var below, so it doesn't need the default ADC.

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

