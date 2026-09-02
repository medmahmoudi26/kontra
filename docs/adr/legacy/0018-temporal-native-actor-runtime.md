# 18. Temporal is the actor runtime; Dapr is removed

## Status

**Accepted.** Supersedes **ADR 0016** (Dapr as the local actor runtime). Refines **ADR 0015**
(three-tier state) — the tier semantics survive; the mechanism clauses naming Dapr do not.

## Context

ADR 0016 kept Dapr on two claims. Both were re-audited against the code and measured on the
live stack, and neither survives.

**"Removing Dapr forces re-implementing single-activation."** It does not, because Dapr is not
providing it. Every worker runs its own placement (`infra/worker-entrypoint.sh:153`,
`127.0.0.1:50005`) — a cluster of one — while `KONTRA_REDIS_HOST` points the whole fleet at one
shared Redis (`:67`). Two workers can therefore host the same actor id simultaneously, each the
sole member of its own cluster, both writing to the same state. What actually serializes turns
is Temporal: one backing workflow per actor id (`handler/nexus.go:44-53` derived from the same
base as `runtime/handler/workflow.go:33-36`), and a turn loop that blocks on
`ExecuteActivity(...).Get()` (`workflow.go:137`). ADR 0016 concedes this in its own
Consequences; it just did not follow the concession to its conclusion.

**"A large correctness surface."** The Dapr state-manager seam is four methods, and the test
double standing in for the whole of it is **14 lines** (`tests/test_dapr_host.py:20-34`).
`control/orchestrator/src/stateStore.ts:95-123` already bypasses Dapr and reads the same Redis keys
directly with ioredis — it even encodes that Dapr writes a HASH rather than a string. The
substantial correctness code (turn loop, windowing, commit-by-identity, per-unit isolation,
streaming sub-units) is ours already and is untouched by removal.

Measured cost of keeping it: the sidecar hop is **2–4 ms p50**, 0.2% of a 1,422 ms RunBatch —
so the case for removal is *not* latency. It is **270 MB of every worker image (29.7%)**, about
541 MB once a `chmod` layer duplicates it; two of five processes; **five** ports (3001, 3500,
9090, 9091 and placement's 50005 — an earlier draft of this ADR omitted the last); and 1,530 code
and 945 test LOC. Dapr's pub/sub, bindings, reminders, timers
and workflow APIs have **zero** call sites, and the Secrets API is staged but never read —
`infra/dapr/secrets.example.json:3` says so itself.

Separately, daprd logs on every boot: *"Redis does not support transaction rollbacks and should
not be used in production as an actor state store."*

## Decision

### 1. An Actor consumes a Batch and returns; a Watcher is a different thing

An **Actor** is versioned code that consumes a Batch of Units and returns. It has no lifetime of
its own and no addressable identity beyond the run/node that keys it. Long-running observation
is a **Watcher**: a durable loop, modelled as a Temporal workflow owned by the platform, that
emits Units and never consumes a Batch.

This resolves a real incident. `registry-watch` was built as an Actor whose `arun` runs for 24
hours. The turn time-box cannot preempt it — `runtime/python/internals/dapr/window.py:30`
declines to start *new* units and lets in-flight ones drain, and a single long-lived unit has no
successor to withhold — so `StartToClose` executed it at 10 minutes and the workflow failed after
ten attempts. That was a category error, not a bug.

### 2. The Actor is a Temporal activity worker, not a sidecar-hosted entity

The actor process registers a `RunBatch` activity and polls the `{actor}-{version}-sessions`
queue directly. The Go handler keeps owning the workflow; Temporal splits workflow and activity
across languages by design. Actor identity becomes the workflow id it already is, and the
per-process `{actorID → instance, lock}` registry replaces daprd's activation.

Deleted outright: daprd, placement, three component manifests, ports 3001/3500/9090/9091, the
~110 LOC of sidecar PUT in `runtime/handler/activity.go`, the envelope validation, and the `actorType`
derivation triplicated across `runtime/handler/main.go:157`, `actorkit/go/internal/dapr/host.go:50` and
`runtime/python/internals/dapr/__init__.py:8` with three hand-synced test tables.

### 3. One activity per Batch — the multi-turn protocol is dropped

The turn protocol existed only because the handler could not observe the actor.
`runtime/handler/workflow.go:82-84` is explicit: *"HeartbeatTimeout can't do this — the keepalive ticker
beats even while the PUT hangs."* Once the actor heartbeats for itself, a wedged unit simply
stops heartbeating and `HeartbeatTimeout` becomes a true liveness check. `StartToClose` stops
being a guillotine.

Gone with it: `done:false`, the resume loop, `maxTurns = 1000`, the `Done` count,
`_TURN_DEADLINE_S`, and `window.py`'s `deadline_s`. A resource loads once per Batch by
construction, which is also why retry affinity stops mattering — relevant because Temporal
worker Sessions are **Go-only** and two of our three languages could not have used them.

> **Correction (Phase 2 scout).** This section is **Python-only work**. The Go host never
> implemented turns: `RunBatchResp` has no `Done` field
> (`actorkit/go/internal/dapr/dapr_actor.go:134-138`), there is no `KONTRA_TURN_SECONDS` in Go,
> and `handler/turns_test.go:106-117` says so outright — *"a host without turn support (e.g. the
> Go host, pre-parity) omits 'done'"*.
>
> Which means **Go is exposed to the guillotine today.** A Go `RunBatch` exceeding
> `StartToCloseTimeout` (10 min, `runtime/handler/workflow.go:97`) is killed and retried up to ten times
> (`:105`) with no cooperative escape — structurally the same failure that killed
> `registry-watch`, live in Go right now, for any actor whose batch takes over ten minutes. The
> Go half of this migration is therefore **additive** (heartbeating, which it has never had),
> not subtractive.

### 4. State splits by lifetime

~~Per-Unit state becomes Temporal's: the commit set rides heartbeat details as an exact set rather
than the approximate count of `workflow.go:172`, and `arun_state` rides the same channel.~~

> **SUPERSEDED (Phase 2 scout). Tier 1 does not migrate at all.** The sentence above is struck,
> not amended: all five scouts independently reached the stronger conclusion that **neither the
> commit set nor `arun_state` should move as the AUTHORITY.** Heartbeat details become a
> *within-execution replica* only — useful for progress, never the source of truth. Both tiers
> stay in Redis, keyed by actor id with the existing 24 h TTL.
>
> The decisive mechanism is the third one below: **a lost commit does not degrade, it
> DUPLICATES.** `runtime/python/internals/dapr/unitstore.py:49` ends the blob key with
> `sha256(data)`, and `control/orchestrator/src/activities/mapAndRoute.ts:250-266` hands the downstream
> node every key it has not seen. So a re-run whose records are not byte-identical writes a
> SECOND blob under the same `unit=` prefix and the child processes both. The docstring at
> `unitstore.py:6-8` — *"Deterministic keys (not content-addressed) on purpose: a re-run
> overwrites idempotently"* — is contradicted by line 49 of its own file. That is a live defect
> independent of this migration, and it makes any weakening of commit durability unsafe.
>
> Also: the set is not small. `dapr_actor.py:295` stores the **whole unit** in each failure
> record (`{"unit": unit, "error": …}`), matched at `dapr_actor.go:788-790`, so the
> catastrophic-isolation path (`runtime/handler/workflow.go:177-179`, a 15,814-target run) would carry
> the entire batch through a heartbeat.
>
> **Consequence: Dapr removal becomes a runtime swap plus a client swap with ZERO state-lifetime
> change** — which deletes the hardest and least-verifiable work from the critical path.
>
> The three mechanisms, verified against Temporal Go SDK v1.46.0:
>
> 1. **`RecordHeartbeat` `panic`s on encode failure** (`internal/internal_activity.go:407`) — it
>    does not degrade. `arun_state` is *author-controlled opaque JSON* (`core.go:75-88`), held per
>    in-flight unit at `ConcurrentAruns(4)` and up, and a crawl frontier is exactly the shape that
>    outgrows a payload limit. One oversized scratch blob would kill the activity. It stays in
>    Redis, where it already has the `-ckpt` key scheme and a 24 h TTL.
> 2. **Heartbeats are throttled and intermediate details are dropped.** The batching window is
>    `0.8 × HeartbeatTimeout`, capped at 60 s (`internal_task_handlers.go:2583-2606`); only the
>    last detail in a window reaches the server, and there is no flush on SIGKILL. At today's
>    `HeartbeatTimeout = 3m` (`workflow.go:98`) that is **up to 60 s of commits lost** on a hard
>    kill. Carrying the commit set therefore requires *lowering* `HeartbeatTimeout` (the throttle
>    tracks it) — 30 s gives a 24 s window.
> 3. **Details survive attempts, not executions.** `GetHeartbeatDetails` reads "the last failed
>    attempt" (`internal/activity.go:250-254`) — one scheduled activity. Today's Redis commit map
>    is keyed by actor id with a 24 h TTL, so it survives *any* re-execution, including a
>    re-dispatch on the same idempotency key and `recover_run`. **This is a real narrowing the
>    original decision did not price**: "24 h, any execution" becomes "the attempts of one
>    execution". With the turn loop gone there is one activity per workflow, so the common path is
>    unaffected — but the recovery path is strictly weaker, and that is a deliberate trade rather
>    than a free win.
Per-instance `session_state` and fleet-wide `global_state` stay in Redis, reached by a plain
client instead of Dapr — `global_state` keeps its ETag CAS behind the existing `EtagKV` seam,
re-implemented over Redis `WATCH`/`MULTI` or Lua, with first-write-wins-on-create preserved.

The author-facing API (`self.arun_state`, `self.session_state`, `self.global_state`) does not
change. `sdk/python/actorkit/actor.py` and `sdk/go/kontra.go` already import no Dapr.

## Alternatives considered

- **Keep Dapr (ADR 0016 restated).** Rejected: the two claims it rests on do not survive audit,
  and the measured cost is a third of every worker image for a runtime whose headline guarantee
  Temporal already provides and whose remaining APIs we do not call.
- **Drop Dapr but keep an HTTP boundary** (0016's own alternative A″). Rejected, though it is the
  lower-risk path and preserves "any language with an HTTP server." It leaves us hand-writing an
  HTTP server, the envelope, the actor registry and the per-id lock in every language — all of
  which Temporal supplies. Polyglot narrowing to Temporal-SDK languages was accepted explicitly:
  kontra uses Python, Go and TypeScript only.
- **Move every state tier to Temporal.** Rejected: `global_state` is cross-run and fleet-wide,
  which needs an entity workflow — the DIY actor pattern, with signal ordering and unbounded
  history growth we would own, underneath the dedup set `nuclei`, `cachebuster` and `subfinder`
  depend on.

## Consequences

- **ADR 0015's mechanism clauses are superseded, its semantics are not.** The three tiers, the
  key schemes, TTL renewal and streaming all stand; "via a `DaprClient`", "the `statestore`
  component" and "Dapr has no TTL-touch primitive" now read as direct Redis, and `ActorStateTTL`
  becomes Redis `EXPIRE`. ADR 0017 §6 cites 0016 for a property that is Temporal's, not Dapr's —
  the cross-reference moves here; the claim is unaffected.
- **`control/orchestrator/src/state.ts` and `stateStore.ts` must change.** They parse Dapr's composite
  key and HASH encoding to power the raw-state endpoints; the per-Unit tier leaves Redis
  entirely and becomes visible in Temporal instead.
- **BOTH actor hosts become Temporal clients and therefore need the claim-check codec.** The
  original wording named only Python; that was wrong. The activity *argument* is encoded by the
  handler's claim-check converter (`runtime/handler/main.go:48-49`), so a Go actor worker without the
  codec receives an unreadable ref payload — and `actorkit/go/go.mod` has **no** `go.temporal.io`
  dependency at all today. There is no Python codec module either, despite
  `runtime/handler/internal/codec/codec.go:3` calling itself "the Go peer of python actorkit.codec".
  `shared/conformance/codec/README.md:8` ("the Python actor host does NOT codec") stops being true for
  both. This forces a deliberate choice the ADR did not anticipate: **duplicate the Go codec into
  actorkit** — a third byte-exact implementation, and `fixtures.json` exists precisely because
  that drift has already happened once (`README.md:42-45`) — **or extract `codec`+`cas`+
  `objectstore` into a shared Go module** and accept coupling that `registrar.go:23-25`
  deliberately avoids. Whichever, both new implementations must pass `fixtures.json` before the
  first migrated actor ships.

- **New conformance corpora are required, and the heartbeat one is urgent.** The heartbeat detail
  becomes a three-party contract (Go writes, Python writes, TS orchestrator reads) and a mismatch
  is **silent** — the monitor simply shows no progress, the exact ambiguity that
  `actorkit/go/internal/dapr/metrics.go:15-25` records as having cost hours of triage and two
  retracted numbers. Also unpinned today and needing fixtures: the `{results, failures, opens}`
  envelope, the `terminal`/`exhausted` isolation categories, `maxUnitReloads = 2` (hand-synced in
  both SDKs), and `core.ErrorTypeName` (which feeds Temporal's non-retryable matching).

- **Go has no behavioural oracle.** `scan_offline.py` is named above as the correctness oracle for
  the first migrated slice, and it is Python-only. One of `examples/go/{nuclei,subfinder}` needs a
  frozen input/output pair before the Go migration ships, or that half lands with nothing to check
  it against.
- **Migration is per-actor, not a flag day.** Task queues are already `{actor}-{version}`, so a
  migrated `layer-scan@0.3.0` runs beside `0.2.0` on Dapr with no overlap. `scan_offline.py`
  reproduces the fleet run exactly — 87 layers, 14 oversized skips, 73 clean — which is the
  correctness oracle for the first slice.
- **Loss ledger.** daprd's `:9090` Prometheus metrics (actorkit serves its own `/metrics`); the
  daprd span in the OTel trace (removed, not lost — one fewer hop); and any future Dapr building
  block, none of which we call.
- **Two venvs collapse into one — but NOT for the reason this ADR originally gave.** The stated
  justification, taken from `install.sh:22-24` ("dapr pins protobuf 7, temporalio pins <7 — they
  cannot share an env"), is **stale and was never re-verified**. Measured this session:

  ```
  .venv          protobuf 7.35.1 | temporalio 1.30.0 | dapr -
  .venv-actor    protobuf 7.35.1 | temporalio -      | dapr 1.18.3
  ```

  Both venvs already run the SAME protobuf. There is no conflict to resolve, so the split is
  unnecessary *today* rather than forced by a dependency — meaning it could be collapsed without
  removing Dapr at all, and removing Dapr cannot be credited with fixing it. The real
  consequence is narrower and still worth having: with the `dapr` package gone, actorkit becomes
  pip-installable instead of riding `PYTHONPATH`, and the repo-root `actorkit/__init__.py` shim
  can go. The `install.sh` comment should be corrected regardless of this ADR's fate.

## Implementation status

Shipped. Dapr is gone from the repository — no `daprd`, no placement service, no actor type
name, no component manifests, no bundled per-worker Redis, and no `dapr` dependency in either
SDK's manifest.

What landed, and where it lives now:

| Slice | Before | After |
| --- | --- | --- |
| Python actor | `internals/dapr/dapr_actor.py` + a FastAPI app under a sidecar | `internals/engine.py` (transport-free) + `internals/temporal/host.py` (activity worker) |
| Go actor | `internal/dapr/{dapr_actor,host}.go` | `internal/engine` + `internal/temporalhost` |
| Tiers 1+2 | Dapr state manager, key per field, no TTL touch | `statekv` (Py + Go): one hash `kontra-actor:{id}`, one `EXPIRE` slides all of it |
| Tier 3 | Dapr ETag state API, conflict detected by matching error strings | `rediskv` (Py + Go): Lua CAS returning 0/1, so a conflict is a value and an error is an error |
| Handler | `RunBatchOnDapr`/`CloseOnDapr` + a turn loop | owns the workflow only; schedules `RunBatch`/`Close` by name onto `{queue}-sessions` |
| Machine | 6 systemd units | 4: `kontra-actor`, `kontra-handler`, `kontra-vmagent`, `kontra-watchdog` |

Three things are worth recording because they were NOT foreseen above:

- **The operator state projection broke silently and was not caught by its own tests.**
  `control/orchestrator/src/state.ts` kept scanning Dapr's `kontra-<app>||<type>||<id>||<key>` composite
  after the layout moved, so every state tier returned an empty result — indistinguishable from
  an actor with no state, which is the failure mode that file's own comments warn about. Its
  tests passed throughout, because they pinned the glob rather than the writer. They now assert
  against `statekv.py`/`redis_kv.py` by name.

- **Live progress broke the same way, in two places.** The actor emitted `{committed, total}`
  while `control/orchestrator/src/heartbeat.ts` decodes `{node, done, total, isolated}` (every field
  optional, defaulting to 0), and `temporalClient.ts` still filtered pending activities on the
  old activity name `RunBatchOnDapr`. Either alone yields an empty progress map. Both are fixed
  and pinned on the emitting side.

  The pattern in all three: a cross-process contract expressed as a string, with a default that
  turns a mismatch into "nothing here" rather than an error.

- **The two-venv justification really was stale.** As the Consequences section suspected, the
  protobuf conflict no longer existed. `.venv-actor` is deleted and there is one interpreter.

### Known gap

The Go migration shipped **without** the frozen input/output oracle this ADR asked for. The Go
half is covered by unit tests (engine, statekv, rediskv against a real Redis, host activation and
queue congruence) and was verified live — the binary registers as an activity poller on
`subfinder-0.1.0-sessions` — but no Go actor has been run end-to-end over a known corpus and
diffed against a pre-migration result. Until that exists, Python is the migrated path with a
behavioural oracle behind it and Go is the one without.
