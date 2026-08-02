# Testing & verification

What is proven, how it was proven, and what is not proven yet.

The distinction that matters throughout: a test against a **fake** proves our
logic; a test against a **real server** proves we speak the protocol; a test
**end-to-end into Goodmem** proves the whole pipeline. Passing the first says
nothing about the third, so this document tracks them separately.

For internals see [tech_details.md](tech_details.md); for running the connector
see [usage.md](usage.md); for what isn't built yet see [roadmap.md](roadmap.md).

---

## Support & verification matrix

**Supported** means implemented and unit-tested. **Verified** means exercised
against real infrastructure.

### Content sources

| Source | Trigger | Verified against a real server | Verified end-to-end into Goodmem |
|---|---|---|---|
| SharePoint | push (webhook) + poll | ✅ live Graph test | ✅ |
| Google Drive | poll (push needs a domain-verified endpoint) | ✅ live Drive test | ✅ `.pdf`, `.xlsx`, `.doc` → `COMPLETED` |
| SMB / Windows network drive | poll only | ✅ Samba **and** real Windows | ✅ all files `COMPLETED`, content retrievable |

### Authentication paths

| Source | Method | Status | How it was proven |
|---|---|---|---|
| SharePoint | Azure AD client credentials | ✅ verified | live Graph test against a real tenant |
| Google Drive | 1 — service-account key | ⚠️ **supported, unverified** | our test org forbids key creation; the code path is the same one ADC uses |
| Google Drive | 2 — GCP-attached service account | ✅ verified | GCE VM, keyless, full sync into Goodmem |
| Google Drive | 3 — workload identity federation | ✅ verified | GitHub Actions off-GCP **and** a VM with a 403 negative control proving the attached SA could not have done the work |
| SMB | NTLM | ✅ verified | Samba container and a real Windows share, over Tailscale |
| SMB | Kerberos (keytab / ccache / password) | ⚠️ **supported, unverified** | config, SPN derivation, credential precedence, cache resolution and error mapping are unit-tested; **no ticket has been exchanged with a live KDC** |

Two gaps are deliberate and both are recorded above rather than glossed: Drive
path 1 and SMB Kerberos. Neither is believed broken; neither has been run.

---

## Tiers, and what each proves

### 1. Unit tests — always run, no network

```bash
go test ./...
```

Providers are tested against in-process fakes: a fake Graph server, a fake Drive
server, and `fstest.MapFS` for SMB. This is why the SMB library is confined to
`client.go` behind an `io/fs` seam — everything above it is testable with no
server at all.

Proves: diff logic, cursor handling, skip policy, retry/backoff decisions, config
validation. Does **not** prove we speak any real protocol.

### 2. Live provider tests — real server, no Goodmem

Env-gated so CI stays hermetic. Each proves the wire protocol and credentials in
isolation, which is what you want when diagnosing an access problem.

```bash
# SharePoint
GRAPH_LIVE=1 go test ./internal/providers/sharepoint -run TestLive -v

# Google Drive
GDRIVE_LIVE=1 GOOGLE_DRIVE_ID=<id> go test ./internal/providers/googledrive -run TestLive -v

# SMB — against the bundled Samba container
docker compose -f docker-compose.smb-test.yml up -d
SMB_LIVE=1 SMB_HOST=127.0.0.1:1445 SMB_SHARE=data \
  SMB_USER=connector SMB_PASSWORD=Passw0rd123 \
  go test ./internal/providers/smb -run TestLive -v
docker compose -f docker-compose.smb-test.yml down
```

These log **shapes, not filenames** by default, because a real share's file names
are customer data and the job log may be world-readable. Set
`*_LIVE_VERBOSE=1` locally when you need the names.

### 3. End-to-end into Goodmem — the whole pipeline

The only tier that proves content is extracted, embedded and retrievable. See
[usage.md → Verifying a deployment](usage.md#verifying-a-deployment) for the
cost-ordered checks and the negative-control technique.

Minimum worth running for a new source:

1. `sync-once --dry-run` — plan only, changes nothing
2. `sync-once` — memories reach `COMPLETED` with the right content types
3. **retrieval** — query for text that only exists inside a document. Status
   `COMPLETED` says the job finished, not that anything was extracted.
4. **re-run** — must be `+0 ~0 -0`, proving deterministic ids
5. **mutate** — modify, add and delete a file, then reconcile. For SMB this is
   the only way to exercise deletion, which the delta cannot see.

---

## Test environments

### Samba container — the default SMB fixture

`docker-compose.smb-test.yml` starts a read-only share preloaded from
`testdata/smbshare/`. Samba is **not an emulator**: it is an independent
implementation of SMB2/3, and a large share of real "Windows network drives" are
NAS boxes running exactly this. It covers NTLM, enumeration, downloads, and
SMB3 signing/encryption when configured to require them.

It does **not** cover Kerberos, DFS namespaces, NT ACL denial semantics, or
Windows case-insensitivity.

### Real Windows share — covered

Verified over Tailscale against a Windows laptop in a workgroup: enumeration,
Office lock-file skipping, an 888 KB PDF downloaded and its text retrievable, and
**case-insensitivity** (`notes.txt` / `NOTES.TXT` / `Notes.Txt` all resolve to the
same file, with the directory listing returning the canonical name).

A workgroup laptop has no Domain Controller, so this is NTLM only. A client
edition of Windows **cannot** be promoted to a DC — that needs Windows Server.

### Samba AD DC — the Kerberos test (next)

Samba can run as a full Active Directory Domain Controller, which is a genuine
Kerberos KDC plus a file server joined to the realm. Self-contained in Docker,
no Windows required.

Covers: krb5.conf loading, keytab parsing, the `cifs/<fqdn>` SPN, the ticket
exchange, SPNEGO session setup, and — by deliberately skewing the clock — that
the skew hint fires.

Does not cover Microsoft-specific behavior: encryption-type negotiation and PAC
handling are the plausible divergences.

### Windows Server AD — the gold standard

The faithful test, because it is Microsoft's own KDC and the same GPO machinery a
customer will have. Worth doing once against a configuration resembling a real
customer's; not worth doing before Kerberos passes against Samba AD DC.

**What it takes**

*Roughly 2–3 hours, most of it unattended installation.*

1. **A VM.** Windows Server evaluation media is free for 180 days from the
   Microsoft Evaluation Center. Budget 2 vCPU, 4 GB RAM, 60 GB disk on
   Hyper-V/KVM/VirtualBox. It must be reachable from the connector host — a host-only
   or bridged network, or Tailscale as we used for the laptop.

2. **Promote it to a Domain Controller.** Use a `.test` or otherwise
   non-routable domain so nothing collides with real DNS:

   ```powershell
   Install-WindowsFeature AD-Domain-Services -IncludeManagementTools
   Install-ADDSForest -DomainName corp.example.test -InstallDNS
   ```

3. **DNS is not optional.** Kerberos discovers the KDC through `_kerberos._tcp`
   SRV records, so the connector host must resolve using the DC as its DNS
   server, or have the realm pinned in `krb5.conf` and the host in `/etc/hosts`.
   This is the single most common reason a Linux client fails against AD.

4. **Service account, SPN and keytab.**

   ```powershell
   New-ADUser -Name svc-goodmem -AccountPassword (Read-Host -AsSecureString) `
     -Enabled $true -PasswordNeverExpires $true
   setspn -S cifs/fileserver.corp.example.test svc-goodmem
   ktpass -princ svc-goodmem@CORP.EXAMPLE.TEST -mapuser CORP\svc-goodmem `
     -crypto AES256-SHA1 -ptype KRB5_NT_PRINCIPAL -pass * -out svc-goodmem.keytab
   ```

   `ktpass -pass *` **resets the account password**, and any later password change
   invalidates the keytab.

5. **A share**, with read granted at *both* the share and NTFS layers (see
   [permissions-smb.md](permissions-smb.md)).

6. **Clock sync.** Kerberos rejects tickets when client and KDC differ by more
   than ~5 minutes. Point the connector host at the DC for NTP, or at the same
   upstream source.

**What to actually test, in order of value**

| Test | Why it matters |
|---|---|
| **Disable NTLM by GPO**, then connect with Kerberos | The real customer scenario. Proves Kerberos works *and* that nothing silently fell back to NTLM — the failure a passing test would otherwise hide. |
| Keytab authentication | The unattended production path |
| Credential-cache authentication | The alternative credential source |
| Deliberate clock skew | Confirms the skew hint fires with actionable text |
| Wrong/absent SPN | Confirms the failure is legible rather than opaque |
| Encryption types (AES vs RC4) | Microsoft is retiring RC4; confirms we negotiate AES |
| A DFS namespace path | Referral handling — still entirely unverified |

The NTLM-disabled test is the one that justifies the whole exercise. Everything
else can be approximated on Samba; *"the domain forbids NTLM and we still
connect"* cannot.

---

## Known gaps

- **SMB Kerberos** — unit-tested, never exchanged a ticket with a live KDC.
- **DFS namespaces** — untested against any implementation; our library's referral
  handling is unverified.
- **NT ACL denial semantics** — Samba maps NT ACLs onto POSIX, so *which*
  operations fail and with what status differ from Windows.
- **Google Drive path 1** (service-account key) — unverified; our test org forbids
  key creation.
- **Scale** — SMB walks the whole tree every poll. Fine for thousands of files;
  unmeasured at hundreds of thousands.
- **Windows Server AD** — the gold-standard pass above has not been run.
