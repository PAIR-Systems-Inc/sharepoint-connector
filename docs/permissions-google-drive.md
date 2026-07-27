# Request for IT: Google Cloud + Drive access for the Goodmem connector

**Request:** Please provision a **read-only service account** that our connector
can use to read one Google **Shared Drive**, authorize it by **one** of the three
methods below (we'll tell you which fits how we deploy), and send us the values in
[What to send us](#what-to-send-us).

This spans **two permission systems** — often two different people:

- **Google Cloud (IAM) admin** — creates the service account and the chosen
  credential (a key, an attachment, or a federation binding).
- **Shared-Drive owner / Workspace admin** — adds the service account to the drive.

They may be the same person. The key handoff: the service-account **email** created
in part A is what the Drive owner needs in part B, and what we need in `.env`.
(Google Cloud roles do **not** grant Drive content access — that's why a separate
Drive share is always required.)

---

## Which method?

| Method | Choose when | What IT sets up | Keyless? |
|---|---|---|---|
| **1 — Service-account key** | We run **off GCP** and your policy allows downloadable keys | a JSON key | no |
| **2 — Attached service account** | We run **on your GCP** compute | attach the SA to the workload | yes |
| **3 — Workload Identity Federation** | We run **off GCP** and keys are **blocked** | a workload-identity pool + binding | yes |

We'll confirm which one before you start — all three are equivalent for us; pick
whichever suits your policy. Steps **A** and **B** are common to every method;
step **C** is the one chosen method.

---

## A. Common — Google Cloud admin (all methods)

```bash
PROJECT=your-gcp-project

# 1. Enable the Drive API
gcloud services enable drive.googleapis.com --project="$PROJECT"

# 2. Create the read-only service account
gcloud iam service-accounts create goodmem-connector \
  --project="$PROJECT" --display-name="Goodmem Drive connector (read-only)"
```

The service-account email is
`goodmem-connector@$PROJECT.iam.gserviceaccount.com`. **Send us this email and the
project id.**

## B. Common — Shared-Drive owner (all methods)

In [drive.google.com](https://drive.google.com): open (or create) the Shared Drive
→ **Manage members** → add **`goodmem-connector@$PROJECT.iam.gserviceaccount.com`**
as **Viewer**. **Send us the Drive ID** from the URL `…/drive/folders/<DRIVE_ID>`.

> This is a Drive-side share, not a Cloud IAM role — without it the service account
> can authenticate but sees no files.

## C. The chosen credential

### Method 1 — service-account key *(only if your policy permits keys)*

```bash
gcloud iam service-accounts keys create goodmem-connector.json \
  --iam-account="goodmem-connector@$PROJECT.iam.gserviceaccount.com"
```

**Send us `goodmem-connector.json` through a secure channel** (it is a secret —
use your secret-sharing tool, not email).

### Method 2 — attach the service account to our GCP workload

Attach `goodmem-connector@$PROJECT.iam.gserviceaccount.com` to the GCP compute we
run on — e.g. a GCE VM created with the Drive read-only access scope:

```bash
gcloud compute instances create goodmem-listener \
  --project="$PROJECT" \
  --service-account="goodmem-connector@$PROJECT.iam.gserviceaccount.com" \
  --scopes="https://www.googleapis.com/auth/drive.readonly"
```

Nothing to send us — just **confirm the SA is attached** (and that the workload can
obtain a Drive-scoped token; a `cloud-platform`-only token, as Cloud Run issues,
does not cover Drive).

**Permissions to create that VM:** **`roles/compute.instanceAdmin.v1`**
(`compute.instances.create`) plus **`roles/iam.serviceAccountUser`** on
`goodmem-connector@$PROJECT.iam.gserviceaccount.com` — attaching a service account
to an instance requires permission to act as it. If org policy blocks external IP
addresses (`constraints/compute.vmExternalIpAccess`), the VM must be created with
`--no-address`, which additionally needs Cloud NAT for egress and an IAP firewall
rule for SSH.

#### If the connector runs on an **existing** VM

Adding the Drive scope to a VM that is already running has three traps worth
knowing before you schedule it:

1. **Scopes can only be changed while the instance is stopped**, so this needs a
   maintenance window — you cannot add the scope to a running VM.
2. **`set-service-account` replaces the scope list, it does not append.** Read the
   current scopes first and re-list them all, or the VM silently loses logging,
   monitoring, and storage access:
   ```bash
   gcloud compute instances describe VM --zone=Z \
     --format='value(serviceAccounts[0].scopes)'      # capture these first
   ```
3. **Stopping a VM releases an ephemeral external IP.** If anything depends on that
   address — a DNS record, a TLS certificate, a hard-coded hostname — reserve it as
   static **before** stopping, which is non-disruptive and keeps the same address:
   ```bash
   gcloud compute addresses create NAME --addresses=CURRENT_IP --region=REGION
   ```

**Additional permissions** beyond the new-VM set above: `compute.instances.stop`,
`compute.instances.start` and `compute.instances.setServiceAccount` (all in
**`roles/compute.instanceAdmin.v1`**), plus **`compute.addresses.create`** (in
`roles/compute.networkAdmin`) for the IP reservation in step 3.

### Method 3 — workload identity federation *(keyless, off-GCP)*

Create a workload-identity **pool + provider** that trusts our runtime platform's
OIDC issuer, let its principal impersonate the SA (grant
`roles/iam.serviceAccountTokenCreator` on the SA, or a direct `principalSet`
binding), then generate the credential-configuration file:

```bash
gcloud iam workload-identity-pools create-cred-config \
  projects/$PROJECT/locations/global/workloadIdentityPools/<POOL>/providers/<PROVIDER> \
  --service-account="goodmem-connector@$PROJECT.iam.gserviceaccount.com" \
  --output-file=wif-credential-config.json
  # + the platform-specific --credential-source-… flag for our host
```

**Send us `wif-credential-config.json`.** It contains **no key** (not a secret) —
only the instructions to fetch and exchange the platform's OIDC token.

---

## What to send us

**Always:**

- **service-account email** (`goodmem-connector@…gserviceaccount.com`)
- **GCP project id**
- **Drive ID** of the Shared Drive

**Plus, for the chosen method:**

- Method 1 → the **JSON key file** (securely)
- Method 2 → confirmation the **SA is attached** to our workload
- Method 3 → the **credential-config JSON**

We place these in the connector's `.env` as `GOOGLE_DRIVE_SA_JSON_FILE` (method 1),
nothing (method 2), or `GOOGLE_APPLICATION_CREDENTIALS` (method 3).

## Org-policy notes

- If **key creation is blocked** (`iam.managed.disableServiceAccountKeyCreation`),
  Method 1 is unavailable — use **Method 2** (on GCP) or **Method 3** (off GCP);
  both are keyless.
- If **service-account creation** is restricted, an admin must perform step A.2.
- If Shared-Drive settings **restrict adding members** (e.g. block sharing with
  service accounts), an admin must allow the SA in step B.
- *(Push mode only.)* If we later run the listener with Google **push
  notifications** instead of polling, Google additionally requires a
  **domain-verified** HTTPS webhook. By **default we poll**, which needs none of
  that — so no domain verification is required for the standard setup.

## How we verify

After we receive the values, we run:

```bash
connector sync-once --source google-drive --dry-run
```

It authenticates as the service account, lists the Shared Drive's files, and prints
the sync plan **without changing anything**. No server or webhook is needed for this
check. (This is the Drive analog of the SharePoint `test_graph_permissions.py`
check in [permissions-sharepoint.md](permissions-sharepoint.md).)
