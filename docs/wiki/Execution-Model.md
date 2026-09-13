# Execution Model

> **Emit is decoupled from the Unit — [ADR 0028](../adr/0028-emit-decoupled-from-unit-caller-redirects.md) (2026-08-18), shipped.** A **Method**'s third parameter is the caller's output **Dataset**, and the actor pushes into it directly rather than round-tripping records back through the caller's workflow to publish. `unit.emit` is gone. The `await dns.addrs(batch)` caller shape survives, and a call now returns `(results, dropped)`.

What happens between `await dns.addrs(batch)` in your workflow and the Batch that comes back.

## The path

```
YOUR workflow (Python or Go, anywhere): actorkit `catalog`, ADR 0021 / ADR 0023
  ─── the ONLY caller: ADR 0023 §12 deleted the orchestrator's graph interpreter ───
   │
   ├─ OpenSession  ──▶ {name}-{version}-sessions ──▶ ONE actor worker answers, and that
   │                                                 answer IS the placement decision
   │
   └─ Nexus op `run` (kontra.actor) ──▶ handler workflow      one per call, type
                                        (id actor-{name}-{base})  kontra.v1.ActorService.Run,
                                        on queue {name}-{version}   in /handler (Go)
        └─ RunBatch activity ──▶ the SESSION's queue {name}-{version}-s-{sessionId}
             (unscoped: the shared -sessions queue)
                └─ the ACTOR's worker (Python or Go)
                     load → YOUR @actor.method(batch, dataset) → per-unit commits, one heartbeat each
                        └─ store_blob(pushed) ──▶ a ~110-byte BareRef back to you
```

The workflow side is ONE Go **handler** (the service shape comes from
`shared/contracts/kontra/v1/actor_service.proto` via `protoc-gen-go-temporal`), run per-actor. The actor
process is itself a **Temporal activity worker**: it registers `RunBatch` and `Close` and polls
its queues directly (`runtime/python/internals/temporal/host.py`,
`runtime/go/temporalhost`). Temporal splits workflow and activity across languages by
design, which is what lets the two halves stay decoupled — they meet only at the queue name and
the JSON wire (`EntryInput`/`BareRef`, `RunBatchInput`).

**Single activation is a per-process concern.** What serializes work for one actor id is Temporal:
one Session pinned to one worker, one backing workflow per call, blocking on its activity. So a
dict plus a lock inside the actor worker is the whole of it — a cluster-wide registry would
neither be needed nor help, since the fleet shares one Redis and a global lookup would still race.
[Legacy ADR 0018](../adr/legacy/0018-temporal-native-actor-runtime.md) has the full argument.

## A Batch in, a Batch out

The model is one sentence: **a Dataset yields Batches, and a Method takes a Batch and returns a
Batch** ([ADR 0023](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) §1). Both ends of that
sentence are **refs**, which is what makes chaining free:

```python
async for batch in catalog.dataset("targets").batches(200, order_by="host"):
    resolved, _ = await dns.addrs(batch)      # ref in, ref out — no row enters your history
    async for chunk in resolved.batches(200):
        checked, _ = await probe.head(chunk)  # one Actor's output IS the next one's input
```

`EntryInput` carries **no sharding knobs**: a call processes the ONE Batch it is handed, and the
paging is the caller's loop (ADR 0023 §3, superseding the dispatcher-splits model of
[legacy ADR 0012](../adr/legacy/0012-split-entryinput-actor-runs-one-batch-dispatcher-shards.md)). `page size` guards of **200**
(soft) and **1000** (hard) were measured on the retired `kontra dispatch` and are now enforced on
`batches()`.

Nothing extra is deployed for any of this — the endpoint an actor registers on boot is the
endpoint you call. `catalog.serve([YourWorkflow], task_queue=…)` runs your side with the
claim-check codec already wired, and `kontra workflow serve|start` drives it from a terminal. The
API is `sdk/python/kontra/catalog.py`; the Go peer is `sdk/go/catalog`. See
[[Writing-Actors-Python]] for the callee half.

## The handler workflow (thin)

`kontra.v1.ActorService.Run` (hand-written in `/handler`, exposed as the Nexus op `run`):

1. **Materialize the Batch** — inline `units`, or `kontra.fetch_blob` from a CAS ref the caller
   passed (so the caller's workflow never held the units). The input side tolerates both the bare
   list a v2 Batch ref addresses and the legacy `{results, failures}` envelope.
2. **Run the Batch** — ONE `RunBatch` activity, scheduled **by name** onto the Session's queue
   (derived from this workflow's own task queue, because workflow code must stay deterministic and
   cannot read env). A generous `StartToCloseTimeout` of an hour, a tight `HeartbeatTimeout` of
   two minutes, and a patient `RetryPolicy` (10 attempts).
3. **Close** — a bounded-retry `Close` on the same queue, on EVERY exit path of an **unscoped**
   dispatch, success or failure. A failed run is exactly when a loaded resource must not leak, and
   the teardown is workflow-driven rather than runtime-driven so both SDKs behave identically.
   Inside a caller's `async with`, the close belongs to the scope instead — the handler does not
   tear the resource down between two Method calls of one Session.
4. **Return** — `kontra.store_blob` the pushed records and return a small `BareRef`.

The workflow and the two blob activities are all the handler registers; `RunBatch` and `Close`
live in the actor's process (`runtime/handler/main.go`).

### What the returned ref addresses

The ref addresses a **bare list of pushed records** — not an envelope — and its `Meta` carries
everything a caller reads without fetching anything:

| `Meta` key | Means |
|---|---|
| `n` | how many Units the Batch holds — this is what `len(batch)` reads |
| `done` | whether the Method drained its input |
| `isolated` | how many Units were dropped (absent when none were) |
| `failures` | the sha of a **second** blob holding the drop records, fetched by `await batch.failures()` |

That split is the point: a caller can branch on drops, count them, and re-page the survivors
without ever downloading a row. A Method call returns `(results, dropped)` (ADR 0028 §4) — you
cannot reach `results` without binding `dropped`, so a caller who does not care writes
`results, _ = ...` on purpose rather than never learning there were drops.

## The actor runs the Batch — one activity, start to finish

There is exactly **one** `RunBatch` call per Batch. It runs to completion:

- **Lazy load** — `RunBatch` opens the author's resource (`@actor.load`) on the instance pinned to
  this actor id; a later call in the same Session reuses it.
- **YOUR loop** — the Method receives the whole Batch and the concurrency is the author's
  (`async for unit in batch`, or `asyncio.gather` over it if that is what the work wants). The v1
  sliding window and its `concurrent_aruns` knob are gone: the framework does not schedule inside
  your Method (ADR 0023 §3).
- **Per-unit commits** — every Unit's outcome commits under its own field in the actor's **Redis
  hash** `kontra-actor:{actor_id}`, keyed by the **Batch's content hash plus the Unit's index**
  (§17) — its position in the INPUT Batch, identity, never completion order. A committed field is
  skipped on a re-run, so a retry can never swap one Unit's result for another's, and a hash does
  not know its scope died — which is what lets a reopened scope resume.
- **Push-time blobs** — with `KONTRA_S3_ENDPOINT` set, each pushed record is written at push time
  under the hive key
  `units/run={run}/dt={date}/actor={actor}/shard={n}/unit={i}/{sha}.json`, and the commit holds
  only `{"$ref": {key, size, sha256}}`. Blob write **first**, then the ref commit, so a committed
  ref always points at written bytes. `run=` leads for a measured reason (8.4s → 0.17s to locate
  one run on a 188k-object bucket) and both SDKs are pinned to one golden fixture,
  `shared/conformance/blobkey.json`. Store unset ⇒ inline commits (dev/test). See [[Data-Plane]].
- **1 → N is just pushing twice** — a Unit that pushes six records commits six sub-unit blobs; the
  Unit's done-marker is written only after the Method finishes with it, so a death mid-Unit
  re-runs it with already-pushed records overwritten idempotently by content sha. Which is why
  records must be **content-deterministic** — no timestamps, no random ids.
- **Heartbeat per unit** — the actor calls `RecordHeartbeat` with `{node, done, total, isolated}`
  as each Unit's outcome lands (`_beat` in `internals/engine.py`, `SetHeartbeat` in the Go host),
  decoded by `control/orchestrator/src/heartbeat.ts`. Every field there is optional and defaults to `0`,
  so a renamed field does not error — it reports `0/0` forever.

### Why one activity, not a loop of turns

`RunBatch` used to be a loop of TIME-BOXED turns: the host returned `done:false` when its turn
budget expired and the handler re-invoked until `done:true`. That existed for exactly one reason —
the handler could not observe the actor. It drove the actor over an HTTP `PUT` to the sidecar and
beat the heartbeat on the actor's behalf with a keepalive ticker, which beat *even while the PUT
hung*, so `HeartbeatTimeout` could not tell a working actor from a wedged one and
`StartToCloseTimeout` had to act as a guillotine instead. Time-boxing was the cooperative escape
from that guillotine.

Now the actor beats for itself, per committed Unit. A wedged Unit simply stops beating, so
`HeartbeatTimeout` is a true liveness check and `StartToClose` is a generous ceiling rather than a
deadline — a legitimately slow Batch is no longer killed for taking its time. Gone with the loop:
`done:false`, `maxTurns`, the per-turn `Done` count and the turn deadline. A resource therefore
loads exactly **once per Batch, by construction** (ADR 0018 §3). The Go host never implemented
turns at all, which meant it had no escape from the old ten-minute `StartToClose`.

## A dead resource ENDS the Session

This is the v2 reversal and the line most likely to surprise a v1 reader. `@actor.healthcheck`
used to mean *"reload me"*, and the framework rebuilt the resource in place — which silently reset
`self.*` underneath a Method that kept running. §20 gave it the same answer §7 gives host loss:
**the Session ends, and the caller reopens.** Barely more expensive (a reload re-runs
`@actor.load` anyway), and the gain is that a Session's promise has no third case — while it
lives, `self.*` is coherent.

What survives the reopen is what was **committed**: the caller resumes from the cursor it holds,
the Batch's content hash is unchanged, so the commit map skips every finished Unit. A **poison**
Unit that has killed N scopes is recorded as a failure and skipped (§21) — without that, §20 plus
§17 is a tight loop. Execution is at-least-once; result recording is exactly-once. See
[[Durability-and-Failures]].

## Session admission (`max_parallel_sessions`)

`KONTRA_MAX_PARALLEL_SESSIONS` (default **4**) is how many live **Sessions** one actor process
holds at once. A host at its cap REFUSES an open, and because the open is an ordinary activity on
the shared `{name}-{version}-sessions` queue, the refusal is back-pressure rather than a failure:
the task returns to that queue and any host with a slot takes it.

It used to be the worker's activity-slot count — `max_concurrent_activities` in Python,
`MaxConcurrentActivityExecutionSize` in Go — and that stopped being the same number in v2
(ADR 0023 §6). A Session now outlives the activity that opened it: the open returns as soon as
the Session's worker is up and frees its slot, so a 2-slot worker would happily hold six live
Sessions. The cap is a count of live Sessions, kept by the actor process
(`internals/temporal/sessions.py`).

The dedicated `{name}-{version}-sessions` queue predates that: it was the slot count of a second
Go worker inside the handler, which had to be a separate queue so a capped `RunBatch` could not
starve the `Close` that frees a slot. Today the actor polls it directly, an OPEN is what lands on
it, and a scoped call goes to that Session's own `{name}-{version}-s-{sessionId}` queue instead.
The backing workflow and the two blob activities stay on the **shared** `{name}-{version}` queue.
See [legacy ADR 0015](../adr/legacy/0015-three-tier-state-and-naming.md) and
[legacy ADR 0018](../adr/legacy/0018-temporal-native-actor-runtime.md).

## Queues / ids at a glance

| Name | Kind | What runs there |
|---|---|---|
| `{name}-{version}` | Temporal task queue | the handler: the backing workflow + `kontra.store_blob` / `kontra.fetch_blob` |
| `{name}-{version}-sessions` | Temporal task queue | the ACTOR's own worker: `OpenSession`, plus `RunBatch` + `Close` for an UNSCOPED dispatch. Every worker of the version polls it, so Temporal's dispatch of an open is the placement decision |
| `{name}-{version}-s-{sessionId}` | Temporal task queue | ONE live Session (ADR 0023 §6): `RunBatch` for every call in that scope, and the `CloseSession` that ends it. Polled by the single worker that answered the open — which is what pins the scope to one process, and why an orphaned one is a `ScheduleToStart` timeout (bounded at one minute) rather than a silent re-activation |
| `kontra-datasets` | Temporal task queue | the orchestrator's dataset activities — paging, splitting, publishing and closing a Dataset for a caller's loop |
| **actor id** (`idempotency_key`, else `session_id`, else `run_id-node_id`) | the key of the live instance | the resource pin + the per-Unit committed state. A key claims a shared identity and carries its `object_state`; an unkeyed scope takes its Session id, so every call in it meets one instance. It MUST be present — a fixed fallback would make id-less runs share one instance and read each other's state, so the workflow fails loud instead |

## Reading the output

Per-Unit failures are **intra-call**: a dropped Unit is simply absent from the returned Batch, so
the next stage operates on survivors and one failure never poisons a chain (§14). Nothing is
retried automatically — re-dispatching a drop is a decision, and
`workflows/sweep.py` shows one way to make it.

A Batch's records may **mix** inline units and `{"$ref": {key, size, sha256}}` entries. The actor
side resolves them on ingest (bounded at 16 concurrent fetches, `_resolve_refs` in
`internals/engine.py`) so a Method's author never sees a ref; the orchestrator resolves them in
`control/orchestrator/src/data/parquet.ts` — sha-verified, spliced back in order — so materialization and
datasets see plain units. See [[Query-Surface]].

## The durable unit

The durable unit is the per-Unit commit keyed by Batch-hash-plus-index. Every committed key is
skipped on a re-run; a dead resource ends the Session and the caller reopens from its cursor.
See [[Durability-and-Failures]].
