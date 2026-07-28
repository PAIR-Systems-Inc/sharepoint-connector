# Multi-source: design decisions & history

Why this connector became a **multi-provider** one (module `goodmem-connectors`):
a shared sync/observability **core** plus swappable source **providers**
(SharePoint and Google Drive today), all shipping as **one binary**. Goodmem is
always the destination.

This is the *design record* — the rationale and how we got here. For how the
system works today see **[tech_details.md](tech_details.md)**; for running it see
**[usage.md](usage.md)**.

## Decisions (locked)

| Decision | Choice |
|---|---|
| Module name | `github.com/PAIR-Systems-Inc/goodmem-connectors` |
| Layout | one shared `core/`, one folder per provider under `providers/` |
| Binary | **one** — provider chosen by `SOURCE` / `--source` |
| Provider naming | spelled out in full: `google-drive`, not `gdrive` — no legacy aliases |
| Google Drive scope | a **Shared Drive**, read by a service account added to it as Viewer. My Drive (domain-wide delegation) is out of scope |
| Google Drive auth | **three** deploy-and-forget paths — service-account key, GCP-attached service account, workload identity federation — so the connector fits any customer IT policy. Paths 2 and 3 are verified end-to-end against real infrastructure (full sync, files reaching COMPLETED); path 1 is unverified only because our test org forbids key creation |
| Google Drive trigger | **poll by default.** Google's `changes.watch` needs a domain-verified HTTPS endpoint; polling the Changes API needs nothing public and is equally incremental |
| Memory-id namespace | per-source and permanent (`sharepoint.file.id`, `google-drive.file.id`) |

## Why this was a small conceptual leap

Both sources expose the three primitives the engine needs — a full listing, an
incremental cursor, and a push mechanism with expiry/renewal. The
trigger → pull-delta → coalesce → apply loop is *identical*, so everything in
`core/` is reused unchanged, including all of the hardening from the
productionization review (mass-delete guard, dead-letter, size cap, coalescing,
retention, `/readyz`, `slog`, alerts).

The per-provider differences that remain are catalogued in
[tech_details.md](tech_details.md#how-the-two-providers-differ).

## Phased plan

1. ✅ **Module rename** → `goodmem-connectors` (mechanical; tests green).
2. ✅ **Restructure** → `internal/core/*` + `internal/providers/sharepoint`
   (package `graph`→`sharepoint`); no behavior change.
3. ✅ **Extract `source.Source` + neutral types** (`internal/core/source`); retyped
   `syncer`/`server` off the provider. SharePoint became an adapter implementing
   the interface (data plane + webhook validation + subscription + throttle).
4. ✅ **Google Drive provider** (`internal/providers/googledrive`) via the official
   Drive SDK + a fake Drive server: export-vs-download policy, Changes-API cursor,
   `changes.watch`/`channels.stop`, header-token webhook validation.
5. ✅ **CLI/config** source selection + the Google Drive config group, source-aware
   validation, per-source space naming, `.env.example` + drift test.
6. ✅ **Productionize Google Drive.**
   - **Live pass:** a real Shared Drive synced end-to-end into Goodmem — `.pdf`,
     `.xlsx` and `.doc` all reached `COMPLETED` with correct content types and
     `source=google-drive`.
   - **Poll mode** added (`SYNC_POLL_MINUTES`, default 2 for Google Drive),
     removing the domain-verified-webhook requirement as a deploy blocker.
   - **Auth documented** as three deploy-and-forget paths, with the limits stated
     (impersonation is interactive so it's local-testing only; keys may be blocked
     by org policy; workload identity federation needs a platform that issues an
     OIDC token — Fly does not). See [usage.md](usage.md#google-drive-service-account)
     and [permissions-google-drive.md](permissions-google-drive.md).
   - **Review fixes:** per-source memory-id namespace; renewal cadence honors the
     lifetime Drive actually grants; folder-trash triggers a reconcile so orphaned
     descendants go promptly; push-channel id+resourceId persisted so a restart
     stops the old channel; `create-subscription` is source-aware; native-doc
     exports over Drive's 10 MB limit are a permanent skip, not dead-letter churn.
7. ✅ **Rename to `google-drive`** across code, config, env vars and identity values — done
   before any Google Drive tenant went live, since the namespace and space name
   are frozen once one is.
8. ✅ **Neutral metrics** — every series is now `connector_*` with a
   `source="<provider>"` label (was `sharepoint_*` regardless of source, including
   a Graph-specific throttle metric on Drive). `deploy/alerts.yml` is source-
   agnostic and names the firing connector via `{{ $labels.source }}`.

### Resolved along the way

- *"Does the Drive webhook address need a verified domain?"* — **Yes.** That is
  why poll mode exists and is the default.
- *"Is a service-account key required?"* — **No.** Two keyless paths exist
  (attached SA on GCP; workload identity federation off-GCP).

## Operational constraints

> ⚠️ **One Goodmem space per source.** Do **not** point a SharePoint listener and
> a Google Drive listener at the **same** `GOODMEM_SPACE_ID`. Each source's full
> sync reconciles the space against *its own* file set and deletes everything else
> as orphaned — so two sources sharing a space form a standing wipe loop: each
> full sync deletes the other's memories, which the other side re-adds, over and
> over (re-embedding every cycle). `GRAPH_MAX_DELETE_RATIO` only trips above its
> threshold (default 50%), so it won't reliably catch this.
>
> The defaults already prevent it: with `GOODMEM_SPACE_ID` unset each source
> creates its own space (`SharePoint_<org>_<site>` vs `GoogleDrive_<driveId>`). The
> trap only appears if you set an explicit shared space id. Per-source memory-id
> namespaces stop the two from ever minting colliding ids, but that does **not**
> rescue a shared space from the delete loop.

## Still deferred

- **Google Drive push instead of polling.** Drive *does* support event-based push
  (`changes.watch`) — the adapter already implements it — but Google only delivers
  to a **domain-verified** HTTPS endpoint, so the default is a 2-minute poll. Once
  the connector is deployed behind a domain we own and have verified (Search
  Console + registered for push notifications), setting `SYNC_POLL_MINUTES=0`
  switches it to push and removes the polling latency. Note a wildcard-DNS
  hostname (e.g. `nip.io`) yields a valid TLS certificate but **cannot** be
  domain-verified, so it is not sufficient for push.
- **Source-filtered orphan deletion** — the Google Drive adapter stamps
  `source: "google-drive"` into memory metadata; stamping SharePoint too and
  filtering orphan deletion by it would make a shared space safe and retire the
  one-space-per-source rule.
- **Generalize the env knobs** that aren't provider-specific (`SHAREPOINT_MAX_FILE_MB`,
  the non-Graph `GRAPH_*` settings) to shared names.
- **Streaming ingest** — hand `Open`'s `io.ReadCloser` straight to
  `CreateFromReader` instead of buffering.
- **My Drive support** via domain-wide delegation (needs per-user impersonation).

## Sources

- [Retrieve changes (Changes API)](https://developers.google.com/workspace/drive/api/guides/manage-changes)
- [changes.getStartPageToken](https://developers.google.com/workspace/drive/api/reference/rest/v3/changes/getStartPageToken)
- [Notifications for resource changes (push / watch)](https://developers.google.com/workspace/drive/api/guides/push)
- [Export MIME types for Google Workspace documents](https://developers.google.com/workspace/drive/api/guides/ref-export-formats)
- [files.export (10 MB limit)](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/export)
- [drive/v3 Go package](https://pkg.go.dev/google.golang.org/api/drive/v3)
