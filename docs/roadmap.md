# Roadmap & history

What is **not** built yet, and how the connector got to its current shape.

This replaces the two planning documents that guided the earlier phases —
`PRODUCTIONIZATION.md` (the Python → Go rewrite) and `MULTI_SOURCE.md` (the
single-source → multi-source generalization). Both plans are complete; their
decisions moved to [tech_details.md § Design decisions](tech_details.md#design-decisions--why),
their operational rules to [usage.md](usage.md), and what remains is below.

For what has been *verified* rather than merely built, see
[testing.md](testing.md).

---

## Todo

Ordered roughly by value. Nothing here is a known defect — these are things not
yet built.

### Sources & connectivity

- **SMB Kerberos against Microsoft's KDC.** Verified against a Samba AD DC
  (keytab, credential cache and password); Microsoft's own KDC, GPO-enforced
  NTLM blocking, and live clock-skew behavior remain untested. See
  [testing.md](testing.md#windows-server-ad--the-gold-standard).
- **DFS namespace support.** Untested against any implementation; our SMB
  library's referral handling is unverified. A direct server path sidesteps it.
- **Google Drive push instead of polling.** Drive supports `changes.watch` and
  the adapter already implements it, but Google only delivers to a
  **domain-verified** HTTPS endpoint, so the default is a 2-minute poll. Behind a
  verified domain, `SYNC_POLL_MINUTES=0` switches to push. A wildcard-DNS host
  (e.g. `nip.io`) yields valid TLS but **cannot** be domain-verified.
- **My Drive support** via domain-wide delegation (needs per-user impersonation).
  Only Shared Drives are in scope today.

### Engine

- **Source-filtered orphan deletion.** Not required today — sync is one-way and
  each source owns its space — but stamping `source` on every provider's memories
  and filtering deletion by it would make a shared space safe and retire the
  one-space-per-source rule.
- **Streaming ingest.** Hand `Open`'s `io.ReadCloser` straight to
  `CreateFromReader` instead of buffering the file. Bounded today only by the
  size cap.
- **Full-sync memory.** A full sync loads the whole listing into maps; paginate
  and bound memory for very large tenants.
- **Worker queue + HA.** Webhook receipt is decoupled from work, but a durable
  queue with a bounded worker pool would allow more than one instance. Effectively
  YAGNI while it is one site per deployment.
- **Generalize the env knobs** that aren't provider-specific
  (`SHAREPOINT_MAX_FILE_MB`, the non-Graph `GRAPH_*` settings) to shared names.

### Operations

- **On-prem deployment recipe for SMB.** `deploy_gcp.sh` and `deploy_fly_io.sh`
  are the wrong shape: an internal file server generally isn't reachable from a
  cloud host on port 445, so SMB needs a documented `docker run` + systemd path
  for a host inside the customer's network.
- **Load/soak testing.** Notification bursts, large drives, throttling behavior.
  SMB is the pressing case — it walks the whole tree every poll, which is
  unmeasured beyond thousands of files.
- **Release hygiene.** Version stamping via `-ldflags`, signed images, SBOM.
- **Webhook hardening.** Request size limits and basic rate limiting on
  `/sync/webhook`.
- **Multi-tenant onboarding.** Automate onboarding/teardown; decide whether one
  deployment should ever serve multiple sites. Add runbooks and state backup.
- **Secret rotation.** Documented rotation for Azure/Goodmem/OpenAI keys, and a
  clearer failure when an SMB service-account password expires under an
  unattended listener.

### Smaller / deferred deliberately

- **`list` / `diff` subcommands** and the richer `watch` output (env-URL
  fallback, `?since=` paging) were not ported from the Python PoC.
- **`clientState` handling divergence.** Go rejects a whole webhook batch on any
  mismatch; the Python PoC skipped only the offending entries.
- **Base64 fallback** on Goodmem's multipart `400 Invalid JSON`, and cosmetic
  metadata differences (JSON `null` vs `""`, `size:0` presence).
- **Obfuscation level** — symbol stripping (`-s -w`) is in place; whether to add
  `garble` is unresolved.
- **Config format** — a TOML config file instead of `.env`.
- **Railway deployment support.**

---

## History

Compressed; the full record is in git.

### Phase 1 — Python proof-of-concept

`sync_once.py` + `listener.py` (~1,830 lines) on Fly.io. The design fundamentals
were sound — deterministic UUIDs for idempotency, delta-vs-full sync,
pending-retry sets, an auto-renewing Graph subscription, `clientState` validation
— but the sync engine was effectively untested. **No customer tenant ever ran the
Python stack**; it ran on test/dev clusters only.

### Phase 2 — Rewrite in Go (2026-07)

Rewritten as a single compiled binary, chiefly for **source protection**: Python
ships as readable source, a stripped Go binary does not. It also removed several
PoC→prod gaps at once — a production HTTP server in the stdlib, goroutines that
fit the webhook→queue→worker model, static typing, and a tiny static image with
no interpreter on disk.

*(The protection argument was recorded honestly: a binary is a deterrent, not a
vault. It still disassembles. The mitigations — never embedding secrets, keeping
sensitive IP server-side, backing it with licensing — are in
[tech_details.md](tech_details.md#design-decisions--why).)*

A module-by-module **port-fidelity audit** against the Python oracle found
several silent divergences, all fixed: the mass-delete guard, folder-path
scoping, space/embedder env aliases, page-image extraction, a self-inflicted
retry-safety regression on a non-idempotent POST, 404 tolerance on pre-update
delete, and the `GRAPH_PORT` default. A live deploy exposed two more that fakes
could not: **duplicate subscriptions on every restart** (Graph omits
`clientState` from `GET /subscriptions`, so the de-dupe match always failed), and
a **missing periodic full sync** — Python reconciled on every subscription
renewal and token refresh; the Go port did neither, so a dropped webhook could go
unrepaired until restart.

**Cutover model** (proposed 2026-07-22, ratified by @amin3141 in the PR #3 review
2026-07-25): Python was never a production fallback. It stayed in git as a
throwaway oracle, used one last time for an offline reference diff, then never
consulted again. Go was the only production system from the start.

Productionization landed alongside: unit + end-to-end integration tests against
in-process fakes, Graph throttling with `Retry-After`, durable state on a
persistent volume, dead-letter parking, a size cap, notification coalescing,
retention, `/healthz` + `/readyz`, structured logging via `slog`, Prometheus
`/metrics`, recommended alert rules, and a SQLite sync history behind
`GET /syncs`. CI became the merge gate.

### Phase 3 — Multi-source (2026-07)

Generalized from SharePoint-only to a provider-neutral engine: a shared
`internal/core` depending only on `source.Source`, plus one folder per provider.
The conceptual leap was small because both original sources expose the same three
primitives — a full listing, an incremental cursor, and a push mechanism with
expiry — so the trigger → pull-delta → coalesce → apply loop was reused unchanged,
including every hardening item above.

- **Google Drive** on the official SDK: Changes-API cursor, `changes.watch`
  channels, `files.export` for native docs. Poll mode was added so a
  domain-verified webhook stopped being a deploy blocker. Three auth paths were
  documented, two verified end-to-end.
- **Renamed** `gdrive` → `google-drive` across code, config, env vars and
  identity values — deliberately *before* any tenant went live, since the memory-id
  namespace and space name are frozen the moment one is.
- **Neutral metrics**: every series became `connector_*` with a `source` label,
  and the alert rules became source-agnostic.
- **Google Drive rate-limit backoff**: the Drive SDK never retries, so the
  throttle metric was pinned at 0 and its alert could never fire for Drive.
- **SMB / Windows network drives**: a third source reached as a standard
  `io/fs.FS`, poll-only because the protocol has no usable change feed, with NTLM
  and Kerberos. Required raising the module to Go 1.25.
