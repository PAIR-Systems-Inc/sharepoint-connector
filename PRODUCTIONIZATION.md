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

## 0. Port-fidelity audit (2026-07-14)

A module-by-module cross-check of the Go port against the Python oracle found
several **silent divergences** — behaviors present in Python but missing or
different in Go. The high-impact ones are fixed; the larger features are tracked
below. (The Go port is otherwise faithful: deterministic UUIDs, diff/classify,
MIME set, space/chunking config, webhook handshake, 410→full-sync fallback,
delta persistence, subscription renew-vs-create, and token/refresh all verified
equivalent.)

**Fixed (2026-07-14):**
- **Mass-delete guard** — `RunFull` now refuses to apply when SharePoint returns
  0 files while Goodmem has memories (a likely transient Graph/auth failure that
  would otherwise wipe the whole space). Mirrors `listener.py`'s guard.
- **`SHAREPOINT_FOLDER_PATH` scoping** — was loaded but ignored; now wired into
  `sync-once` (the listener syncs the whole drive, matching `listener.py`).
- **Space/embedder env aliases** — `SPACE_ID`/`DEFAULT_SPACE_ID` and
  `EMBEDDER_ID`/`DEFAULT_EMBEDDER_ID` are honored again (GOODMEM_-prefixed wins).
- **`GOODMEM_EXTRACT_PAGE_IMAGES`** — now sent through to Goodmem on ingest.
- **Retry-safety regression (self-inflicted)** — the new throttling layer had
  been retrying the non-idempotent subscription-create `POST` on 5xx/network,
  risking a duplicate subscription. `POST` is now retried only on `429`.
- **Pre-update delete 404 tolerance** — an update whose memory is already gone
  now falls through to re-add instead of erroring (matches `listener.py`).
- **`GRAPH_PORT` default** — 8080 → 5000, matching Python and `.env.example`.

**Fixed (2026-07-14, part 2):**
- **Pending-retry sets** — the listener now keeps three durable sets
  (`.graph_pending_add` / `_update` / `_removes`, alongside the delta link) via
  `syncer.Retrier`: a failed Goodmem add/update/delete is queued and retried on
  the next delta sync (re-fetching the file's current SharePoint state), with
  intra-sync conflict resolution when a file lands in more than one action list.
  Gated to the listener — the one-shot CLI (`Options.Retry == nil`) stays simple,
  matching `sync_once.py`. **Note:** these files live under
  `GRAPH_DELTA_TOKEN_FILE`'s dir (ephemeral `/tmp` on Fly) — real durability is
  still §5.
- **Goodmem processing-status polling** — a create is now polled to
  COMPLETED/FAILED (or a ~5-min timeout); a 200-but-FAILED ingest is reported as
  a failure and re-queued as delete-then-add instead of counted as success.

**Deferred / tracked (larger work):**
- **Notification coalescing** (`_root_sync_pending`): Go runs one delta sync per
  notification; Python debounces bursts.
- **`clientState` handling**: Go rejects the whole webhook batch on any mismatch
  (and enforces even when unset); Python skips only the offending entries.
- **`serve` without `GRAPH_NOTIFICATION_URL`**: Python runs (skips auto-subscribe);
  Go hard-requires it.
- **`.env` precedence**: Python `load_dotenv(override=True)` lets `.env` win;
  Go lets real process env win. Go's behavior is arguably better for Fly secrets —
  **kept intentionally**, noted here so the divergence is on record.
- **Missing CLI**: `list` / `diff` subcommands and the richer `watch` output
  (env-URL fallback, `?since=` paging) are not ported.
- **Base64 fallback** on Goodmem's multipart `400 Invalid JSON`; **cosmetic**
  metadata differences (JSON `null` vs `""`, `size:0` presence).

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

- **Unit tests** for the sync engine: ✅ **done** — UUID set math, add/update/delete
  classification, intra-sync conflict resolution (a file landing in more than one
  action list after the pending merge), pending merge/retry, timestamp
  comparisons, and unsupported MIME handling all have tests.
- **Integration tests** with a fake SharePoint (Graph) + fake Goodmem: ⏳ **partial** —
  component-level `httptest` coverage exists (Graph client flows incl.
  retry/backoff, webhook handshake, `processingStatus` polling, pending-merge +
  conflict resolution); a single end-to-end full-sync/delta harness is not built.
- **Differential tests** (Go vs Python oracle): ⏳ **partial** — a manual
  module-by-module audit was done (see §0); not yet codified as an automated suite.
- **Load/soak**: notification bursts, large drives, throttling behavior. ❌ not started.
- Wire it all into CI (see §7). ✅ **done.**

---

## 4. Runtime, concurrency & durability (Tier 1–2)

- **Serving:** ✅ **done** — Go stdlib `net/http`, with graceful shutdown on
  SIGINT/SIGTERM (10s HTTP drain; in-flight syncs cancel via context).
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

- **Microsoft Graph throttling:** ✅ **done.** Every Graph call (auth, JSON
  requests, delta, download) now retries through a single executor
  (`graph.Client.httpDoRetry`): it honors `Retry-After` on `429`/`503`, applies
  full-jitter exponential backoff to transient `5xx` and network errors, caps
  attempts at `GRAPH_MAX_RETRIES` (default 5, clamp `[0,10]`) and `Retry-After`
  at 120s, and fires an `OnThrottle` hook that the listener logs to `/activity`.
- **Idempotency & crash safety:** ⏳ **partial** — deterministic UUIDs +
  durable pending-retry sets (§0) mean failed items are re-attempted; still
  missing an explicit checkpoint before/after apply, and the state files are
  ephemeral (see §4 durable state).
- **Poison handling:** ⏳ **partial** — persistently failing items are retried
  via the pending sets, but with **no cap / dead-letter** yet (they loop
  indefinitely, matching Python).
- **Subscription lifecycle:** ⏳ **partial** — renew-before-expiry loop and
  recreate-on-404 (`EnsureSubscription`) exist; alerting on renewal failure does not.

---

## 6. Observability

> **Deferred (2026-07-14).** Not planned for the current push. `/activity` +
> `/healthz` are sufficient for now; revisit when scale/operational needs grow.
> The `OnThrottle` hook (§5) is already in place to feed metrics when we return.

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

- **CI gate:** ✅ **done** (`.github/workflows/go-ci.yml`). The `build-test` job
  is the merge gate — `go build`, `go vet`, a `gofmt`-clean check, and
  `go test -race ./...`; `golangci-lint` and `govulncheck` run as advisory
  (`continue-on-error`) jobs, ready to promote to gates once confirmed clean.
  `fly-deploy.yml` still handles deploy-on-push-to-main.
- **Reproducible builds:** ✅ **done** — Go modules with a checked-in `go.sum`
  (replaces the unpinned `requests>=…` / `flask>=…` in `requirements.txt`).
- **Minimal image:** ✅ **done** — multi-stage Dockerfile → static binary in
  `gcr.io/distroless/static-debian12:nonroot` (no interpreter, no source).
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

Phases here are the "tiers" — **Phase/Tier 1 is the Go port + engine tests** (§3),
**Tier 1–2 is runtime/durability** (§4). Status of each:

| Phase / Tier | Focus | Key deliverables | Status |
|---|---|---|---|
| **0. Pin behavior** | De-risk the port | Characterization tests against the Python engine (the oracle) | ⏳ **Partial** — manual module-by-module audit done (§0); not a codified oracle suite |
| **1. Go port** | Source protection + typing | `connector` binary (`serve`/`sync-once`), Go `graph`/`goodmem`/`syncer` packages, differential tests vs Python, shadow-run then cutover | ✅ **Mostly done** — binary + packages complete, port-fidelity gaps fixed (§0), unit tests green; integration/differential tests partial (§3); shadow-run → cutover still an ops step |
| **2. Durability & resilience** | Kill SPOF / data-loss risk | Datastore-backed state + queue/workers, Graph throttling/backoff, HA (>1 instance) | ⏳ **In progress** — throttling/backoff ✅ and pending-retry ✅; durable state, worker queue, and HA (>1 instance) pending |
| **3. Observability & CI/CD** | Operable & safe to change | Structured logs + metrics + alerts, health probes, full CI (test/lint/scan), minimal signed image | ⏳ **Partial** — CI gate ✅ + distroless image ✅; observability **deferred** (§6); signed image/SBOM pending |
| **4. Hardening & ops** | Productization | Secret/scope tightening, binary hardening (`-s -w`/garble), multi-tenant onboarding automation, runbooks, backups | ❌ **Not started** |

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
