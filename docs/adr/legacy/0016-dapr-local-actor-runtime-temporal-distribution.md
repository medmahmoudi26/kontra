# 16. Dapr is the local actor runtime; Temporal owns distribution

## Status

**Accepted — Dapr is the per-worker LOCAL actor runtime; Temporal owns distribution. Per-worker
placement is RETAINED as the actor runtime's mandatory local dependency.**

This ADR settles a deliberate call raised by roadmap platform-x100 #08: every worker runs its OWN
Dapr placement (`infra/worker-entrypoint.sh`, `--placement-host-address 127.0.0.1:50005`), so each
worker is a Dapr actor cluster-of-one while Temporal does the real cross-worker distribution — which
made per-worker placement *look* vestigial. The decision is **option A′**: keep the bundled
placement and fix the narrative, **not** the issue's original option A (delete it). No functional
code shipped this pass — the placement process, its ports, and the `KONTRA_PLACEMENT` override are
unchanged; only entrypoint comments/logs were relabeled to describe placement as *local actor
membership*, and this ADR records the stance. It pairs with **#02** (shared state store, the
cross-worker exactly-once dependency) and shares a seam with **#08** (secrets): the worker's LOCAL
daprd is the single integration point for both actor invocation and secret resolution.

## Context

Kontra runs Dapr actors and Temporal side by side, and the boundary between them was never written
down. The self-contained worker (`infra/worker-entrypoint.sh`) bundles, in one container: redis (the
actor state store), **its own placement** (`/kontra/placement --port 50005`), daprd (pointed at
`127.0.0.1:50005`), the actor host, and the Go handler (the Temporal poller). Because every worker
points daprd at its **own** placement, each worker is a Dapr actor cluster **of one**.

That layout invites two wrong conclusions — that placement is vestigial (a cluster of one needs no
membership service, so why run it?), and that the system does "distributed actors" (implying Dapr
placement distributes work across workers). Both are wrong, and the code says why:

- **Actor invocation is ALWAYS local.** The handler drives the actor by PUTting to its **local**
  sidecar — `KONTRA_DAPR_SIDECAR`, default `http://127.0.0.1:3500` — at
  `/v1.0/actors/<type>/<id>/method/RunBatch` and `/method/Close`
  (`runtime/handler/activity.go:86,119,167`); the worker sets `KONTRA_DAPR_SIDECAR=http://127.0.0.1:3500`.
  The actor call never leaves the container. daprd resolves the actor id against its LOCAL placement
  — whose only member is this very worker — and invokes the co-located host.
- **Distribution is Temporal, not Dapr.** Every worker of an actor binds the **shared** Temporal
  task queue `SharedQueue = "{actor}-{version}"` (plus a `-sessions` queue)
  (`runtime/handler/internal/identity/identity.go:19-24`); Temporal fans units/chunks across whichever
  workers poll it (the per-step-queue pattern of ADR 0013; actor-as-Nexus-operation of ADR 0001).
  Dapr placement never routes cross-worker — it cannot, because no worker's daprd knows about any
  other worker's placement.

So per-worker placement genuinely IS a cluster-of-one. The trap is concluding it is therefore
removable. It is not: **Dapr actors hard-require a reachable placement service.** Running daprd
1.18.1 with no placement logs, verbatim, `Actor runtime disabled: ... placement service is not
configured. Actors and Workflow APIs will be unavailable` (directly observed), and its
`--actors-disseminate-timeout` help states it *halts hosted actors* if dissemination fails. There is
no embedded-placement or actors-without-placement mode. Because `RunBatch`/`Close` are Dapr **actor**
methods, deleting placement breaks execution outright. Placement is not vestigial — it is the
load-bearing local dependency that lets daprd host actors at all, and what makes Dapr's per-actor
single-activation and the per-(step,unit) commit/TTL **state manager (ADR 0015)** work.

## Decision

### 1. Name the seam: Dapr = local actor runtime, Temporal = distribution

Dapr's job is the **single-host actor runtime**: per-actor single-activation and the per-(step,unit)
commit/TTL state manager (ADR 0015) — the correctness primitives that hold **within** one worker.
Temporal's job is **distribution**: cross-worker fan-out, retries, and durable orchestration, via
the shared task queues (ADR 0013, ADR 0001). They are complementary layers, not competitors. This
line is now the stance the whole stack speaks; the "distributed actors" phrasing is retired.

### 2. Retain per-worker placement as the runtime's mandatory local dependency

Keep the bundled placement (`infra/worker-entrypoint.sh` step 2) exactly as is. Each worker stays a
Dapr actor cluster-of-one **by design** — that is the shape a local actor runtime takes when
distribution is delegated to Temporal. Placement's ports and the `KONTRA_PLACEMENT` override are
unchanged.

### 3. Do not centralize placement

A shared placement across workers would create genuine cross-worker Dapr actor routing. That routing
would (a) compete with Temporal's task-queue fan-out (two systems deciding where work runs), (b) add
a control-plane single point of failure, and (c) be incoherent without a shared **actor** state
store (roadmap #02): a central placement could route an actor id to a worker whose local redis holds
none of that id's committed state, silently defeating the committed-unit skip.

### 4. No functional change this pass

The placement process, ports, and `KONTRA_PLACEMENT` override are untouched. The entrypoint comments
and startup log were relabeled to describe placement as **local actor membership (single-host), not
distribution**, pointing here. Removing placement is infeasible (§Alternatives A); centralizing it
is rejected (§3). Any future move belongs to the lead's runtime integration, gated on #02.

## Alternatives considered

- **A — Delete the per-worker placement process (the issue's original recommendation).** Rejected:
  **infeasible.** Dapr actors hard-require a reachable placement; daprd disables the actor runtime
  without one (observed: `placement service is not configured. Actors and Workflow APIs will be
  unavailable`) and *halts hosted actors* on dissemination failure. Since `RunBatch`/`Close` are
  invoked over the Dapr actor API (`/v1.0/actors/...`), deleting placement breaks execution. The
  issue's implied check — "a worker runs without a local placement process" — is expected to FAIL;
  that failure IS the proof that delete-placement is not on the table while Dapr actors are used.

- **A″ — Drop the Dapr actor model; invoke the host over plain HTTP / service invocation.** This
  would truly remove placement, but it forces re-implementing single-activation **and** the
  per-(step,unit) commit/TTL state manager that Dapr's actor state manager gives us today (ADR
  0015). That contradicts the "use the platform, don't rebuild it" principle and trades a benign
  cluster-of-one for a large correctness surface. Rejected.

- **B — Centralize placement (one shared placement for the whole fleet).** Rejected: it manufactures
  cross-worker Dapr routing that competes with Temporal's fan-out, is a control-plane SPOF, and is
  pointless/incoherent without the shared actor state store of roadmap #02. Centralizing placement
  without #02 would route ids to workers that cannot see their state.

## Consequences

- **Honest narrative.** "Distributed actors" is corrected to "Dapr = local actor runtime, Temporal =
  distribution." README/wiki wording should follow this stance (owned by the docs seam, not changed
  here; the wiki ADR index adds the row for this ADR separately).
- **Placement stays; the cluster-of-one is intentional.** No operator action, no behavior change;
  the relabeled comments make the design legible so the next reader doesn't file the same "delete
  vestigial placement" issue.
- **Couples to roadmap #02 (shared state store).** Single-activation and the committed-unit skip
  hold only **within** a worker, because placement AND redis are both per-worker. A Temporal retry
  on the shared queue can land on **another** worker whose local redis is empty and re-run the unit
  from scratch. Cross-worker exactly-once therefore requires the shared state store (#02); until it
  lands, exactly-once is per-worker, with the blast radius bounded by content-addressed idempotent
  sub-unit emits (sha-keyed, ADR 0015) that make a re-run an idempotent overwrite.
- **Shares the "local daprd" seam with #08 (secrets).** The handler/host talk only to
  `127.0.0.1:3500` for BOTH actor invocation (this ADR) and secret resolution (the Dapr Secrets API,
  #08) — one local integration point, no cross-worker Dapr traffic in either.
- **A future central-routing model, if ever wanted, is a re-evaluation, not a tweak.** It requires
  #02 first and a fresh weigh-in against Temporal's role (option B revisited).

Refines **ADR 0015** (the per-actor Dapr state manager is exactly what makes placement load-bearing —
the crux of why "local actor runtime" is a real decision and not a shrug). Relates to **ADR 0013**
(the per-step shared task queue is the Temporal distribution mechanism named here) and **ADR 0001**
(actor-as-Nexus-operation = the orchestrator dispatches through Temporal, so Temporal owns where work
runs). Supersedes nothing.
