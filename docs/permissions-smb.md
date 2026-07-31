# Request for IT: read-only access to a Windows network drive

**Request:** Please create a **read-only service account** that our connector can
use to read one **SMB share** (a "Windows network drive"), grant it read access
to that share, and send us the values in [What to send us](#what-to-send-us).

This works against any SMB2/3 server — **Windows Server, Samba, or a NAS
appliance** — so the steps below are given for Windows with notes for the others.

The connector **only reads**. It never writes, renames, moves, or deletes
anything on the share.

---

## A. Create the service account

A normal domain (or local) user account, used only by this connector:

- **Username:** e.g. `svc-goodmem`
- **Password:** non-expiring, or tell us the rotation schedule so we can plan for
  it — the connector runs unattended and a silently expired password stops syncing
- **Interactive logon:** not required; deny it if your policy prefers
- **Groups:** no privileged groups. This account needs nothing beyond read access
  to the one share

> On a standalone server or NAS, a local user is fine — there is no domain
> requirement.

## B. Grant read-only access to the share

The account needs read access at **both** layers Windows applies, because the
effective permission is the *more restrictive* of the two — a common cause of
"it authenticates but sees nothing":

1. **Share permissions** — Share tab → *Advanced Sharing* → *Permissions* → add
   `svc-goodmem` with **Read**.
2. **NTFS permissions** — Security tab → add `svc-goodmem` with **Read & execute**,
   **List folder contents**, **Read**, applied to *This folder, subfolders and files*.

On Samba, the equivalent is a read-only share (`read only = yes`) with the account
in `valid users`, plus POSIX read/execute on the directory tree.

If parts of the tree must stay private, simply do not grant access to them: the
connector **skips unreadable subdirectories and syncs the rest**. Restricting the
account is a supported way to scope what gets indexed.

## C. Network access

The connector connects over **TCP port 445** from wherever it runs to the file
server. Please confirm that path is open. Port 139 (legacy NetBIOS) is not used.

Because a network drive normally is not reachable from the public internet, the
connector usually runs **inside your network** — on a small Linux VM or container
with a route to the file server. It needs **no inbound** connectivity and **no
public URL**: it polls the share and pushes to Goodmem outbound only.

## D. Authentication method

**NTLM** (username + password) is what the connector uses today, and it is what
the values in the next section describe.

*If your policy requires **Kerberos**,* tell us — the underlying library supports
it via a credentials cache, and we will confirm the setup with you before you
provision anything.

---

## What to send us

- **Server hostname** (e.g. `fileserver.corp.example.com`) — and the port if not 445
- **Share name** (the `Shared` in `\\fileserver\Shared`)
- **Account username** and **password** (through your secret-sharing tool, not email)
- **Domain / workgroup** name, if the server is domain-joined
- *(Optional)* a **subdirectory** to limit the sync to, e.g. `Reports/2026`

> ⚠️ Please treat the subdirectory as **permanent**. Each file's identity is its
> path relative to that directory, so changing it later re-keys every document we
> have indexed and forces a full re-ingest.

## How we verify

Once we have the values:

```bash
connector sync-once --source smb --dry-run
```

It authenticates as the service account, lists the share, and prints the sync plan
**without changing anything** — on the share or in Goodmem. If it authenticates
but lists nothing, that is nearly always the share-vs-NTFS permission mismatch in
step B.

## Notes on what the connector does *not* need

- **No write access** of any kind.
- **No administrator rights** on the file server. (Windows' change journal would
  need them, but the connector deliberately does not use it — see
  [tech_details.md](tech_details.md#why-smb-polls).)
- **No inbound firewall rule, public DNS name, or TLS certificate** — unlike the
  SharePoint connector's webhook, this source polls and needs no callback URL.
- **No changes to the share's configuration** — no auditing, no notifications,
  no agent installed on the file server.
