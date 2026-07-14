# Production Roadmap

Plan to take the SharePoint → Goodmem connector from proof-of-concept (Python)
to a production-grade service, **rewritten entirely in Go** — a single compiled
binary — so the distributed code ships as a binary rather than readable source.

Current state: `sync_once.py`, `listener.py` (~1,830 lines), `watch_listener.py`,
`sharepoint_client.py`, `goodmem_client.py`, deployed to Fly.io via
`deploy_fly_io.sh`. Design fundamentals are sound (deterministic UUIDs for
idempotency, delta-vs-full sync, pending-retry sets, auto-renewing Graph
subscription, `clientState` validation). The gaps are the usual PoC→prod ones,
plus the source-protection requirement.

---

## 1. Language & architecture decision

### Rewrite the whole application in Go

**Why Go:**
- **Source protection.** Python is shipped as source / trivially-decompiled
  bytecode. A stripped Go binary is compiled machine code — a strong deterrent
  to reverse engineering. Ship it in a `scratch`/`distroless` image with no
  interpreter and no `.py` on disk.
- It also removes several PoC→prod gaps for free: a **production-grade HTTP
  server** in the stdlib (no dev-server swap needed), **goroutines/channels**
  that fit the webhook→queue→worker model, **static typing** that catches the
  class of bugs our untested Python can hide, and a **single static binary**
  (tiny image, fast cold start, small attack surface).

**Honest caveat on "protection":** a binary is a deterrent, not a vault. It can
still be disassembled and strings/logic recovered with effort. Pair it with:
- Build with `-ldflags "-s -w"` (strip symbols); consider `garble` for extra
  obfuscation.
- **Never embed secrets** in the binary — inject via env/secret store at runtime.
- Keep genuinely sensitive IP server-side where feasible; back it with
  licensing/legal terms. Treat Go as "raise the bar," not "make it impossible."

### Proposed shape: one Go binary, subcommands

Replace `listener.py` + `sync_once.py` with a single binary (e.g. `connector`):

| Subcommand | Replaces | Notes |
|---|---|---|
| `connector serve` | `listener.py` | The webhook server + sync engine (the distributed artifact) |
| `connector sync-once` | `sync_once.py` | One-time full sync (CLI) |
| `connector watch` | `watch_listener.py` | Local activity monitor |
| `connector create-subscription` | `listener.py create-subscription` | Manual subscription renew |

- **Shared Go packages:** `graph/` (Microsoft Graph client — use the official
  `msgraph-sdk-go`, or plain REST to keep the surface small), `goodmem/`
  (Goodmem REST client — port of `goodmem_client.py`), `sync/` (the diff /
  conflict-resolution / apply engine — the valuable IP), `config/`, `store/`.
- **Python retained only as a throwaway test oracle** during the port (§2) —
  differential-tested against the Go, then deleted at cutover. No Python ships
  in the product; nothing stays mixed long-term.
- **Stays as-is:** `deploy_fly_io.sh` (bash deploy glue calling the `fly` CLI —
  not application code; optionally folded into `connector deploy` later),
  templates, docs.

---

## 2. Port strategy (de-risk the rewrite)

The sync engine is intricate and currently **effectively untested** — porting it
blind would be dangerous. Sequence:

1. **Pin current behavior first.** Write characterization/golden tests against
   the *Python* engine (mocked SharePoint + Goodmem) covering the diff, conflict
   resolution, and apply paths — this becomes the executable spec the Go port
   must match.
2. **Port bottom-up:** `goodmem/` and `graph/` clients → `sync/` engine →
   `serve`/`sync-once` commands.
3. **Differential testing:** run Python and Go against identical fixtures and
   assert identical `to_add/to_update/to_delete` decisions. Keep the Python
   around until Go matches on a broad corpus.
4. **Cutover** one deployment (e.g. a test cluster) to the Go binary, run it in
   parallel/shadow, then promote. Retire the Python `listener.py`/`sync_once.py`.

---

## 3. Testing (Tier 1 — highest priority)

Today: only `test_graph_permissions.py` (87 lines); the 1,830-line sync logic is
untested. This is the biggest risk — a subtle bug silently drops or duplicates
memories.

- **Unit tests** for the sync engine: UUID set math, add/update/delete
  classification, conflict resolution (same file in multiple lists), pending
  merge/retry, timestamp comparisons, unsupported MIME handling.
- **Integration tests** with a fake SharePoint (Graph) + fake Goodmem: full
  sync, delta sync, deletes, renames/moves, pagination, partial-ingest failure,
  `processingStatus` PENDING/FAILED/COMPLETED, subscription create/renew.
- **Differential tests** (Go vs Python oracle) during the port.
- **Load/soak**: notification bursts, large drives, throttling behavior.
- Wire it all into CI (see §7).

---

## 4. Runtime, concurrency & durability (Tier 1–2)

- **Serving:** Go stdlib `net/http` (drops the `app.run()` dev-server problem);
  add graceful shutdown that drains in-flight syncs.
- **Concurrency model:** decouple webhook receipt from work — webhook handler
  enqueues; a bounded pool of workers (goroutines) drains a **durable queue**.
  Replaces the current in-process `threading` + locks and enables >1 instance.
- **Durable state (kills 3 PoC-isms at once):** the delta link is a **local
  file** (`.graph_delta_link`) and the activity log is **in-memory**
  (`_activity_log`) — both lost on restart and broken with >1 machine. Move
  delta cursors, subscription state, pending-retry sets, and an audit log to a
  **datastore** (Postgres, or SQLite on a Fly volume for single-tenant). This
  also unblocks **HA** (currently `min_machines_running=1` = SPOF).
- **Full-sync memory:** startup/refresh loads the whole drive into maps —
  stream/paginate and bound memory for large tenants.

---

## 5. Resilience

- **Microsoft Graph throttling:** there is currently **no `429` / `Retry-After`
  handling** — Graph *will* throttle at scale. Add retry-with-backoff honoring
  `Retry-After` on every Graph call; jittered backoff for transient 5xx/network.
- **Idempotency & crash safety:** deterministic UUIDs already help; ensure sync
  is resumable/re-runnable after a crash mid-apply with no data loss
  (checkpoint before/after apply).
- **Poison handling:** cap retries per file; dead-letter persistently failing
  items with visibility, rather than looping forever.
- **Subscription lifecycle:** harden the renew loop (renew-before-expiry exists);
  alert on renewal failure; recreate on 404.

---

## 6. Observability

- Replace the in-memory `/activity` log + `watch_listener.py` polling with
  **structured logging** (JSON) and **metrics** (sync latency, files
  added/updated/deleted, failures, throttle events, subscription-renewal
  health, queue depth) — Prometheus/OpenTelemetry.
- **Alerting** on: subscription renewal failure, sustained sync failures,
  throttle storms, queue backlog, auth/token failures.
- Keep a lightweight `/activity` (now backed by the datastore) for humans; add
  `/healthz` (liveness) and `/readyz` (readiness).

---

## 7. Build, CI/CD & supply chain

- **CI today only deploys** (`.github/workflows/fly-deploy.yml`) — no test/lint
  gate. Add a pipeline: `go test ./...`, `go vet`, `golangci-lint`, build,
  vulnerability scan (`govulncheck`), then deploy on green.
- **Reproducible builds:** Go modules with a checked-in `go.sum` (replaces the
  unpinned `requests>=…` / `flask>=…` in `requirements.txt`, no lockfile today).
- **Minimal image:** multi-stage Dockerfile → static binary in
  `distroless`/`scratch` (no interpreter, no source, small CVE surface).
- **Release hygiene:** version stamping via `-ldflags`, signed images, SBOM.

---

## 8. Security & config

- **Secrets:** `.env` → Fly secrets is okay; enforce no secrets on disk in the
  image, validate required config at startup with clear errors, and rotate
  Azure/Goodmem/OpenAI keys.
- **Least privilege:** review the Graph application permissions in
  `docs/permission.md` — request the minimum scopes needed.
- **Webhook hardening:** `clientState` validation exists (now auto-generated);
  add request size limits and basic rate limiting on `/sync/webhook`.

---

## 9. Multi-tenancy & operations

- **Deployment model:** today it's one Fly cluster per customer
  (`<FLY_CLUSTER>-*`). Fine as a model — document/automate onboarding and
  teardown; decide whether one deployment should ever serve multiple sites.
- **Ops:** liveness/readiness probes, graceful shutdown mid-sync, backup/restore
  of the datastore, and runbooks (the "restart a suspended listener" note in
  `docs/usage.md` is a start).

---

## Phased roadmap

| Phase | Focus | Key deliverables |
|---|---|---|
| **0. Pin behavior** | De-risk the port | Characterization tests against the Python engine (the oracle) |
| **1. Go port** | Source protection + typing | `connector` binary (`serve`/`sync-once`), Go `graph`/`goodmem`/`sync` packages, differential tests vs Python, shadow-run then cutover |
| **2. Durability & resilience** | Kill SPOF / data-loss risk | Datastore-backed state + queue/workers, Graph throttling/backoff, HA (>1 instance) |
| **3. Observability & CI/CD** | Operable & safe to change | Structured logs + metrics + alerts, health probes, full CI (test/lint/scan), minimal signed image |
| **4. Hardening & ops** | Productization | Secret/scope tightening, binary hardening (`-s -w`/garble), multi-tenant onboarding automation, runbooks, backups |

**Top 3 if nothing else:** (1) tests around the sync engine, (2) the Go
rewrite with durable state + production serving (protects the source *and*
removes the dev-server/local-file/single-machine risks together), (3) Graph
throttling/retry.

---

## Open decisions

- Datastore: Postgres (HA, multi-tenant) vs SQLite-on-volume (simplest,
  single-tenant)?
- Graph access in Go: official `msgraph-sdk-go` vs hand-rolled REST (smaller,
  fewer deps, easier to audit/obfuscate)?
- Obfuscation level: symbol-stripping only, or `garble`?
