# 21. There is a caller's side; an Activity is a third kind of deployed code

## Status

**Accepted.** Extends **ADR 0001** (one Nexus op, one way in) by giving that op a second caller.
Refines **ADR 0018** — the actor is still a Temporal activity worker and the handler still owns
the workflow; what changes is who is allowed to start one. Does not touch **ADR 0012/0013**: an
Actor still runs one Batch and the caller still shards.

## Context

The Nexus op has had exactly one caller since it existed:
`control/orchestrator/src/workflows/interpreter.ts:401`. Everything a **Run** could ever be had to be
expressible as a saved graph, because the graph interpreter was the only thing that knew how to
speak to a deployed actor. That is a real constraint on what work is possible, not a stylistic
one, and it shows up in three places.

**A graph's edges are fixed when you draw them.** The interpreter resolves each node's input
through `mapAndRoute` over the edges the graph declares (`interpreter.ts:306-350`). Mapping,
merge and a SQL filter are expressive, but they are all *transformations of a fixed topology*.
The shapes that are not: a loop that keeps dispatching until a frontier empties, a retry that
re-shards the drops at a smaller width, a fan-out whose width is a function of what the previous
actor returned. Every one of those has been wanted on a real campaign, and every one of them was
answered with a bespoke script — which the CLI-first rule of this repo treats as a defect report
against the product, not a solution.

**The obvious spelling is the opposite verb.** `actor.run()` reads like "call the actor". It is
the *serve* entrypoint: `sdk/python/actorkit/actor.py:395` resolves identity and calls `serve()`,
which is `asyncio.run(worker.run())` — it boots the callee and blocks forever. Inside a
`@workflow.defn` it is three separate sandbox violations. There was no verb for "make a deployed
actor do a batch" because there was no caller to need one.

**The seam was already named for a module that did not exist.**
`handler/internal/identity/identity.go` documents its queue derivations as having a Python peer
at `actorkit.workflows.shared_queue`. The comment was written against the shape the system was
going to have; nothing at that path had been built.

Separately, everything deployable had to be an **Actor**. An Actor is a session: a resource
loaded once, a commit map, three tiers of durable state, per-unit heartbeats and isolation. That
apparatus is what makes a 37,000-target crawl survivable and it is entirely wasted on a function
that takes an argument and returns a value — but a function still had to be dressed as an Actor,
with `arun` over a one-element Batch, to be deployed at all.

## Decision

### 1. `dispatch` is the caller's verb; the callee's is `serve`, not `run`

`actorkit.workflows` is the caller's side of the SDK. On the callee side, `actor.run()` becomes
**`actor.serve()`** (and `activity.serve()`), with `run()` kept one release as a deprecated alias
— the same policy ADR 0015 used for `checkpoint` → `arun_state`.

The old name was the confusion this ADR exists to end. `actor.run()` reads as the *caller's*
verb — "make this actor do the work" — while it is the *serve* entrypoint: it boots a worker and
blocks forever. That misreading has a concrete cost, since the natural next step is to put it
inside `@workflow.defn`, where it is a blocking `asyncio.run` in a sandbox. `serve()` is also
what the method has always called internally (`internals/temporal/host.py`), so the public name
was disagreeing with the private one it delegates to.

`register` was considered and rejected: this repo already registers — to the orchestrator
catalog, on boot (`publish_catalog`) — and overloading the word would collide with a live
concept. The precedent for `serve` is Prefect, which draws this exact line between calling a
flow and `flow.serve()` starting a process that waits to be given work.

### 2. A caller's dispatch IS the interpreter's dispatch — there is no second wire

`ActorHandle.dispatch` builds the same `EntryInput` and calls the same Nexus op (`kontra.actor` /
`run`) on the same derived endpoint, then dereferences the returned `BareRef` through
`kontra.fetch_blob` — an activity already registered on every deployed actor's shared queue
(`handler/main.go`). **Nothing new is deployed for a caller to exist**: the endpoint an actor
registers on boot is the endpoint you call, and an actor deployed a month ago is callable today.

The caller reaches it through a **shared service definition** (`sdk/python/actorkit/contract.py`:
`@nexusrpc.service class KontraActorService`), not through service/operation strings at the call
site. That is Temporal's own model for crossing a team boundary — the caller depends on the
contract and never on the callee's code — and it is what makes the operation name declared once
rather than typed everywhere. The payload types are `TypedDict`s, which is a wire decision and
not a style one: they are plain dicts at runtime, so the bytes stay byte-identical to what
`interpreter.ts` sends, where a dataclass would have emitted `params: null` for a key the
interpreter omits.

The same reasoning fixes the activity side, which does **not** get a bespoke calling convention:
`ActivityHandle.call` is `workflow.execute_activity` with the task queue filled in and
`**options` forwarded whole. A wrapper that enumerates the options it forwards can only fall
behind the SDK, and an option that silently fails to arrive is worse than one that is missing.
The activity may be named by string or by **function reference**, so a caller who can import the
bundle's module gets static checking for free. What the handle exists for is the one string a
user must never type: `{name}-{version}-activities` is derived in four places, and the caller is
the only writer with no congruence test behind them.

The alternative — a caller-side client that reaches into the actor's sessions queue directly —
was rejected. It would bypass the backing workflow, and with it the actor-id derivation, the
close-on-every-exit-path, and the retry that makes reload exactly-once. The handler is not
overhead on this path; it is the path.

### 3. A dispatch returns committed results AND drops

`dispatch()` returns a `BatchResult` (`.results`, `.failures`, `.done`, `.ref`), not a list.
Isolation is not an error and will not fail the caller's workflow — that is ADR 0009 and it is
correct — so the only remaining question is whether the caller can *see* it. If drops are not in
the value the caller is already holding, they are invisible, and a node that discarded every unit
reads exactly like a node that legitimately found nothing. That conflation is how a
15,814-target run once reported `completed` in about seven minutes having scanned almost nothing.
`BatchResult.__bool__` is therefore explicit rather than derived from `__len__`: an all-isolated
batch is truthy, so `if not result:` cannot be read as "nothing to do".

### 4. An **Activity** is a third kind of deployed code, and it ships no handler

A directory whose `actor.json` says `"kind": "activity"` deploys as a bundle of plain functions.
It polls `{name}-{version}-activities`; a call is one activity task, direct to that queue.

It gets **no handler binary in its image** (`cli/deploy.go`, `workerDockerfile`). A bundle has no
backing workflow and no Nexus endpoint, so a handler would be a second process polling a queue
nothing schedules onto — a container that reports healthy and does half of nothing. One process,
one queue, and `infra/worker-entrypoint.sh` `exec`s it so the container's PID 1 is the work.

The queue is a **third** name, not the sessions queue. Sharing would put a bundle beside an
actor's own `RunBatch` worker, where it could take a Batch it has no session machinery to run.

The choice between the two kinds is the retry grain, and it is the author's to make: an Activity's
unit of retry is the whole call, so a death halfway starts it over. Choose an Actor when losing
half the work matters; choose an Activity when it does not. **Activity** enters the Execution
glossary (`CONTEXT.md`) on those terms: it neither consumes a **Batch** nor emits **Units**, which
is precisely why it has no **Node**, no **Manifest**, and no place in a graph.

### 5. What a caller's run does not get, decided rather than deferred

A caller's workflow is not a **Run**. It gets no orchestrator run row, no **Manifest**, no
published dataset, and no failure policy. These are the interpreter's jobs and they stay there;
duplicating them behind a client library would give the repo two implementations of run identity,
which ADR 0006 exists to prevent.

The per-unit blobs are unaffected — the actor still writes `units/run={run_id}/…`, keyed by the
`run_id` the caller passes (defaulting to its workflow id) — so the *data* remains queryable by
every existing tool while the *run status* does not exist. That divergence is the deliberate
price of code-first dispatch, and it is documented at the call site rather than discovered.

## Consequences

- **The Nexus op is now a public API surface, not an internal seam.** `EntryInput`'s field set is
  pinned from three sides against `entry.proto` — Go (`wire_congruence_test.go`), TS
  (`interpreter.ts`) and now Python (`tests/test_workflows_client.py`). The queue and endpoint
  derivations gain a fourth independent writer, guarded by the same congruence tables; a drift
  there has no loud failure mode, it simply routes to a queue nobody polls.
- **A bundle is invisible to `kontra workers list`.** That command joins the orchestrator catalog
  with live pollers, and a bundle publishes no catalog entry because it is not a graph node.
  `kontra workflow bundle <name> [version]` describes its queue directly and is currently the
  only liveness answer for one. A bundle that is deployed but unserved therefore looks like
  nothing at all until a call sits in its queue — the same "registered but no worker" trap
  `kontra workers list` was built to surface, reopened for the new kind.
- **The claim-check codec is now required in a third process.** The caller's worker builds it
  from the same `KONTRA_S3_*` env (`internals/temporal/wfhost.py`), because a client without it
  fails every over-128-KiB result with `Unknown payload encoding binary/claim-check-v1` — which
  passes on a demo and dies on a real batch. The CLI's `workflow start --wait` carries a
  **decode-only** half of the same codec for the same reason.
- **Ref-to-ref chaining is not possible yet, and the asymmetry is why.** A result ref addresses an
  *envelope* (`{done, results, failures, opens}`); an input ref must address a *bare list*, since
  the handler decodes it into `[]any`. So `dispatch_ref()` output cannot be fed back as
  `input_ref` — the caller must fetch, take `.results`, and pass those, materializing through its
  own workflow. The clean fix is a `chain_units` activity on the handler (fetch envelope → store
  the results list → return the new ref), which would keep the payload entirely handler-side.
  It is not in this decision because it changes the handler, and the handler is baked into every
  deployed worker image: shipping it means redeploying the fleet, which must be its own change.
- **The caller's side is Python-only.** A Go-authored workflow cannot dispatch actors today —
  `sdk/go` has no peer for `workflows.py` and no `@activity.defn`. `kontra deploy`
  refuses `-engine go` on a bundle explicitly rather than building an image whose entrypoint has
  nothing to launch.
- **`kontra deploy` now emits two image shapes** and must keep both honest. The difference is one
  `COPY` and pinned by test in both directions (`cli/workflow_test.go`), because a bundle that
  silently shipped a handler, or an actor that silently lost one, would both boot and both look
  fine.
