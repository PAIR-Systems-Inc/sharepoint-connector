# Google Drive — connector setup & authentication

How a **deploying engineer** stands up the connector against a Google **Shared
Drive**. The connector reads the drive as a **service account** (a read-only robot
identity) and finds its credentials through **Application Default Credentials
(ADC)** — see [Reference](#reference-gcloud-profiles--adc).

Anything that needs admin authority (create the service account, a key, IAM
bindings, share the drive) is collected in the companion **[Request for
IT](gcloud-auth.md)** — hand that to your GCP / Workspace admins.

## Choose your auth path

There are **three deploy-and-forget options** — set up once, runs unattended, no
human ever logs in again, no periodic re-auth. Pick by one question: *where does
the connector run, and does your org allow downloadable service-account keys?*

| Path | Use when | Engineer manages | Secret? |
|---|---|---|---|
| **1 — Service-account key** | Runs **anywhere** (Fly, on-prem, a VM, another cloud) and your org allows keys | one JSON file/secret | yes (1 static key) |
| **2 — Attached service account** | Runs **on GCP compute** that can mint a Drive-scoped token (e.g. a GCE VM) | nothing | no |
| **3 — Workload Identity Federation** | Runs **off GCP** *and* your org forbids keys | one non-secret config file | no |

> **In one line:** keys allowed → **Path 1** (simplest, works everywhere). On GCP
> → **Path 2** (no secret). Neither → **Path 3**.

Each customer uses exactly **one** path. A fourth flow — interactive
**impersonation** — is for [local testing](#local-testing-impersonation--not-for-deployment)
only; it needs a human to re-login periodically, so it is **not** deployable.

## Common step (every path): share the Shared Drive

The service account sees **nothing** until it's added to the drive — this is a
**Drive** permission, not a Google Cloud one, so it is required on all three paths.

1. [drive.google.com](https://drive.google.com) → **Shared drives → New** (a
   Workspace is required for Shared Drives).
2. **Manage members** → add the service-account email
   `goodmem-connector@<project>.iam.gserviceaccount.com` as **Viewer**.
3. Add your files.
4. Copy the **Drive ID** from the URL `…/drive/folders/<DRIVE_ID>` — this is
   `GDRIVE_DRIVE_ID`.

(Your IT / Drive owner usually does steps 1–2 — see [Request for IT](gcloud-auth.md).)

Every path shares this `.env` base; only the **credential** differs (per path below):

```dotenv
SOURCE=gdrive
GDRIVE_DRIVE_ID=<DRIVE_ID>
GOODMEM_BASE_URL=https://your-goodmem
GOODMEM_API_KEY=...
GOODMEM_SPACE_ID=...        # or leave unset to auto-create GDrive_<DRIVE_ID>
```

---

## Path 1 — Service-account key

Simplest and most portable — the Google analog of an Azure app client secret.
Requires your org to permit downloadable keys.

**IT provides** a service-account **JSON key** (see [Request for IT](gcloud-auth.md)).
Add **one** of these to `.env`:

```dotenv
GDRIVE_SA_JSON_FILE=/secure/path/goodmem-connector.json   # a file on disk, OR
# GDRIVE_SA_JSON={"type":"service_account",...}            # the JSON inline (e.g. a Fly secret)
```

Done — the connector authenticates as the SA from the key, mints its own
short-lived tokens, and never needs a human. Keep the key out of git (`.gitignore`
covers `*-sa.json`, `secrets/`); rotating it periodically is optional hygiene.

## Path 2 — Attached service account (on GCP)

Zero secrets: the GCP host supplies the identity automatically through ADC (the
metadata server).

**Requires** the workload to run on GCP compute whose **attached SA can obtain a
Drive-scoped token**, with that SA added to the drive (common step). The clean
case is a **GCE VM created with the `drive.readonly` access scope**:

```bash
gcloud compute instances create goodmem-listener \
  --service-account="goodmem-connector@<project>.iam.gserviceaccount.com" \
  --scopes="https://www.googleapis.com/auth/drive.readonly"
```

**`.env`:** set **nothing** credential-wise — ADC uses the attached SA.

> **Scope gotcha:** a token scoped only to `cloud-platform` does **not** cover the
> Drive API. **Cloud Run** issues a `cloud-platform` token you can't downscope, so
> Cloud Run needs Path 1 or 3 instead. Always confirm with the dry-run below.

## Path 3 — Workload Identity Federation (WIF)

Keyless **off** GCP: the host platform's own machine identity (an OIDC token) is
exchanged for short-lived SA credentials — no key, nothing to rotate, no human.

**IT provides** a WIF **credential-configuration JSON** (see [Request for
IT](gcloud-auth.md)). It is **not a secret** — it holds no key, only instructions
to fetch the platform's token and exchange it.

**`.env`:**

```dotenv
GOOGLE_APPLICATION_CREDENTIALS=/path/to/wif-credential-config.json
```

The connector's ADC reads that config, exchanges the platform OIDC token for a
Drive-scoped SA token at runtime, and runs unattended.

> **Requires the platform to issue a workload OIDC token** — GCP, GitHub Actions,
> AWS, Azure, and Kubernetes (OIDC) do; a plain **Fly.io** app does **not** today.
> On Fly, use Path 1 (a key) or run on a GCP host (Path 2).

---

## Run & verify

```bash
./connector sync-once --source gdrive --dry-run   # lists files + plan, no changes
./connector sync-once --source gdrive             # apply once
./connector serve --source gdrive                 # run the listener (poll mode default)
```

If the dry-run lists your Drive's files, both the credential **and** the Drive
share are correct. This is the whole acceptance check.

## Local testing (impersonation) — not for deployment

For hands-on local runs you can skip keys / attached-SA and impersonate the SA with
your own Google login:

```bash
gcloud auth application-default login \
  --impersonate-service-account="goodmem-connector@<project>.iam.gserviceaccount.com" \
  --scopes=https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/cloud-platform
```

(Keep `--scopes` on **one line** — a wrapped terminal splits it.) The connector
then uses the default ADC, so set no credential var.

**Do not use this for a deployed listener.** It stores a *user* refresh token that
Google's reauth policy expires on a schedule (hours to days), so the listener will
**silently stop syncing** until a human re-runs the login. It's a developer
convenience, not one of the three deploy-and-forget paths above.

## Listener: push vs poll (not an auth setting)

The gdrive listener defaults to **poll mode** (`SYNC_POLL_MINUTES`, default 2): it
runs a delta sync on a timer and needs **no public webhook**. Google's push
channels (`changes.watch`) require a **domain-verified** HTTPS endpoint — a
throwaway `*.fly.dev` host can't satisfy it — so push mode is worth it only when
you own a verifiable domain and want sub-minute latency. Poll mode removes that
requirement entirely and is the recommended default.

---

## Reference: gcloud profiles & ADC

### Gcloud CLI authentication (profiles)

Gcloud CLI supports multiple profiles; each has a default account (email) and
project, managed via `gcloud config`.

- Create: `gcloud config configurations create <name>` (subsequent `gcloud`
  commands use it until you switch). Configure with `gcloud config set account
  <email>` and `gcloud config set project <project_id>`.
- Switch: `gcloud config configurations activate <name>`; list with
  `… configurations list`; inspect the current one with `gcloud config list`.
- Or attach `--configuration <name>` to a single command instead of switching.

### Application Default Credentials (ADC)

ADC is how Google's **client libraries** (the SDK the connector uses) find
credentials automatically. The library searches, in order:

1. the **`GOOGLE_APPLICATION_CREDENTIALS`** env var — a path to a credentials JSON
   (a service-account key **or** a WIF credential-config);
2. the **well-known gcloud ADC file**,
   `~/.config/gcloud/application_default_credentials.json`, written by
   `gcloud auth application-default login`;
3. on a GCP host, the **attached service account** via the metadata server.

That ordering is exactly why each path above "just works": Path 1/3 set (1), the
local-testing login writes (2), and Path 2 relies on (3). The ADC JSON is one of a
few shapes — service-account key (`"type": "service_account"`), user login
(`"type": "authorized_user"`), impersonation (`"type":
"impersonated_service_account"`), or external account / WIF (`"type":
"external_account"`).

**ADC is separate from the gcloud CLI account.** The gcloud *profile* decides who
`gcloud` commands act as; ADC decides who the *application* acts as. You can have
`gcloud` on one identity and the connector on another at the same time.
