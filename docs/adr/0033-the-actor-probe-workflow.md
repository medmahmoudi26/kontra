# 33. The Actor probe: kontra owns a workflow that calls exactly one Method

## Status

**Accepted.** Reverses the *generate-and-hand-it-over* half of the call flow shipped under **0023**
§12 (`control/orchestrator/src/actorControl.ts:callerFor`, `frontend/src/panels/MethodCall.tsx`) and
keeps the half that teaches — the generated caller survives as a read-only artefact, in the same
spirit **0030** kept a code editor's legibility while dropping its write.

**It does not reopen 0023 §12.** §12 removed a general graph interpreter; this admits a probe with
one Method in it, and §1 of the Decision states the line and the test that keeps it there.

Leans on **0028** (§2 the Dataset parameter, §4 the `(results, dropped)` tuple) for the call's shape,
**0029** §3 for its output's lifetime — which needs no extension — **0022** and **0023** §10 for why
the probe refuses a key, and **0007** for why it does not schedule `RunBatch` itself. Supersedes no
ADR.

## Context

Filling in a form, being handed a file, saving that file into a folder, serving it and starting it is
five steps to answer "does this Method work". The artefact is genuinely useful; the errand is not.
The page should call the Method.

Server-side dispatch is also, at a glance, the shape §12 deleted — so the distinction has to be
recorded rather than assumed, and the mechanism has to be recorded too, because the obvious
alternative is wrong in a way no test would catch. Five things were established against the real code
before deciding anything.

1. **A Nexus operation is scheduled by a WORKFLOW command, in every SDK we have.** Go takes
   `workflow.Context` (`workflow.NewNexusClient` → `ExecuteOperation`,
   `sdk/go/catalog/workflows.go:514`); Python takes the workflow module
   (`workflow.create_nexus_client`, `sdk/python/actorkit/catalog.py:797`); TypeScript exports
   `createNexusServiceClient` from `@temporalio/workflow` and only from there
   (`@temporalio/workflow@1.18.1`, `lib/nexus.d.ts:82`). There is no activity-context spelling in any
   of the three. **So "a dispatch activity" is not a cheaper design — it is not a design.** A workflow
   must exist. The only open question is whose it is, and today the answer is *the operator's*,
   written out as source they have to host themselves.

2. **`POST /api/runs` does not 404, and has not since the day after the interpreter was removed.**
   This matters because five comments in this repo say it does — `control/orchestrator/src/server.ts:782`,
   `control/orchestrator/src/actorControl.ts:147`, `frontend/src/panels/ActorsPage.tsx:51`,
   `panels/methodCall.ts:7`, `panels/methodCall.render.test.ts:13` — and that sentence is the load-
   bearing premise of the flow this ADR reverses. What `4a69b27` (2026-08-14) removed was the route
   that **took a graph** and started the interpreter, along with the server-minted run id, the durable
   pre-start record and the idempotency replay that existed *because the server owned execution* —
   which is exactly what `server.ts:14–17` still says, correctly, two lines under a headline
   ("IT DOES NOT START RUNS", `:12`; "runs (READ ONLY)", `:7`) that is no longer true of the file it
   introduces. `e8d9b28` (2026-08-15) reintroduced the verb in its post-§12 meaning: `server.ts:851`
   → `workflowControl.startRun` (`workflowControl.ts:599`) starts **one execution of a caller's
   workflow**, on a queue derived from that caller's folder content, refusing outright when no worker
   polls it. **0029** §2 depends on that route by name.

   The true invariant is therefore narrower and survives this ADR intact: **the orchestrator starts
   workflows; it does not execute Batches.** "Starting a run is a 404" was never the rule — it is a
   sentence that outlived the thing it described.

3. **The backing workflow is not a formality, and its id deliberately does not name the Method.**
   `handler/nexus.go:26` wires the op with `temporalnexus.NewWorkflowRunOperation`, whose backing
   workflow is `runtime/handler/workflow.go:19`. That workflow rehydrates a Batch that arrived as a ref
   (ADR 0007), derives the actor id from key → Session → run/node and **fails loudly rather than
   defaulting**, upserts `KontraRunId`/`KontraActor` so the run is discoverable at all, schedules
   `RunBatch` onto the *Session-aware* queue with the retry/heartbeat pairing that makes
   resume-from-committed real (`runActivityOptions`, `runtime/handler/workflow.go:239` —
   `MaximumAttempts: 10`, `HeartbeatTimeout: 2m`, against a commit map keyed on Batch hash + index
   that skips committed Units, `sdk/python/actorkit/actor.py:81`), drives `Close` on **every** exit
   path unless the call was scoped, and stores results *and* dropped Units as CAS objects with
   `n` / `done` / `isolated` / `machine` on the ref's meta.

   `backingWorkflowID` (`handler/nexus.go:48`) is `actor-<name>-<idempotencyKey | runID-nodeID |
   uuid>`. **The Method is not in it**, and the comment is explicit that nothing parses the id — it is
   a label for the Temporal UI. Combined with the documented attach semantics of a keyed dispatch
   (`ActorHandle.__getitem__`, `sdk/python/actorkit/catalog.py:610`, the bullet at `:623`: "a
   concurrent second dispatch ATTACHES rather
   than queues… it returns THAT batch's results — it does not run your units"), two keyed probes of
   *different* Methods on one key collide onto one backing workflow, and the second is handed the
   first's rows. Nothing anywhere downstream could tell those apart. See Decision §2.

4. **There is no orchestrator execution queue any more, and that was §12's doing.**
   `control/orchestrator/src/queues.ts` opens by saying so: the queue "carried the graph interpreter and its
   data-routing activities", and what survives is placement-driven work only (`kontra-materializer`,
   `kontra-datasets`). kontra does still own and serve workflows — `kontra-infra` runs the Pulumi
   stack, the tmux session and **0029** §3's own retention sweep (`control/orchestrator/src/infra.ts:123`,
   `workflows/infra.ts`) — so a kontra-owned workflow is not a new category. A kontra-owned *general*
   execution queue would be.

5. **The TypeScript Nexus caller was deleted with the interpreter, and its own docstring says why.**
   `control/orchestrator/src/workflows/nexusService.ts` went in `4a69b27`; only a stale build artefact is
   left, and it reads: *"The Nexus service **the interpreter** calls to dispatch an actor node."*
   TypeScript has no caller half at all — **0023** §22 gave the caller's side to Python and (on
   reversal) to Go, and left it there. Restoring that file to host the probe would put a third,
   untested caller-side wire encoder in the one language with no peer to pin it against, next to a
   name derivation that already exists in five independent copies with, in `nexusRegistry.ts`'s own
   words, "no loud failure mode for a drift — a caller dispatches to a name nobody created and waits".

## Decision

### 1. The probe accepts exactly one call, and that is the whole of it

The probe takes **one Actor, one version, one Method, one Batch**. Concretely, its parameters are
`(actor, version, method, units, dataset?)` and nothing else. It refuses, by having nowhere to put
them: a second Method, a second Actor, an output wired to another input, a branch, a condition, a
loop, a retry policy, a schedule, a fan-out width. It runs one Method call, publishes what came back,
and returns.

**The test a reviewer applies is a count.** *How many Methods can one request name?* One is a probe.
Two — in any spelling, however it is dressed — is a **topology**, and a server that executes a
topology is the interpreter, whatever the route is called. **The moment the probe accepts a topology
it is the interpreter again**, and at that moment this ADR is reopened rather than extended.

This is the whole distinction from **0023** §12, stated plainly: §12 removed a **general interpreter
executing user-composed topologies** — it owned sharding, failure policy, materialization and the
streaming cursor, and it was removed because it owned too much and because a caller who wanted to
drive dispatch themselves got none of it. The probe composes nothing, decides no failure policy, owns
no cursor, and shards nothing. It is one dispatch that a human asked for by hand, of the same kind
the operator would have written into a file — and §16's rule is untouched, because composition
remains the author's, inside their Method, as ordinary function calls.

Two corollaries follow from the same rule and are listed so they are not re-litigated per feature: a
**"call this again with the failures"** button is a second call and belongs to the operator, not to a
probe that loops; and **chaining two probes** is two Runs the operator started, never one request.

### 2. The probe does not take a key

`crawler["acme.com"]` is a claim on a shared virtual object (**0023** §10, **0022**), and a
concurrent dispatch on a held key *attaches* to the running execution and returns its results
(finding 3). A probe is the one caller most likely to be fired twice in ten seconds by an impatient
human, and the backing workflow id carries no Method to keep two of them apart. So the probe dispatches
**unkeyed** — a private, anonymous Session — and its `run_id` is fresh per probe, which is what makes
`backingWorkflowID` fall through to a distinct id. Probing a keyed object's durable state is a real
need and it is *not* satisfied by quietly adding a key field: it needs the attach semantics on screen,
and that is a separate decision with its own ADR.

### 3. kontra owns the workflow, because something has to and the operator should not

The probe is a **one-shot workflow owned by kontra**: it starts, makes one Method call, publishes, and
returns. Finding 1 is the whole argument for it being a workflow — a Nexus operation cannot be
scheduled from anywhere else — and everything after that is only about ownership. Making the operator
own it means making them host a process to answer a question about someone else's Actor.

**It is served on a kontra-owned queue that serves exactly one workflow type.** That constraint is
part of the line, not an implementation note: a kontra queue serving *arbitrary* caller workflows is
the execution queue finding 4 says was deleted, rebuilt with a different name. One type, and a second
type on that queue is a reviewable event.

**It runs actorkit's caller half — `catalog.actor(name, version).<method>(batch, out)` — not a
reimplementation of it.** That path is the one the SDK documents, every example uses, and
`actorControl.test.ts` already *executes* against the real handle; it returns `(results, dropped)`
(**0028** §4) and takes the output Dataset as its second positional argument (**0028** §2). Reusing it
is what makes Decision §6's artefact honest rather than merely illustrative: the code shown beside the
button is the code the probe runs. The cost is stated rather than discovered — kontra's shipped
containers are Node-only (`docker-compose.yml`: `dist/src/{server,infra,materializer}.js`), so hosting
a caller-half worker is a real deployment addition. It is the right cost to pay: finding 5 prices the
alternative in a third wire encoder and a sixth endpoint-name derivation, in the language whose only
Nexus artefact was deleted for being the interpreter's.

### 4. The probe goes through the production Nexus operation

The probe dispatches through `kontra.actor:run` on the Actor's registered endpoint
(`kontra-<name>-<version>`, owned by registration — `control/orchestrator/src/nexusRegistry.ts`), exactly as a
caller's workflow does. It does **not** schedule `RunBatch` onto the Actor's queue directly.

The direct route is genuinely simpler and it is genuinely wrong. It skips the backing workflow, which
means the probe would have to reproduce, correctly and forever: the ref rehydration for a Batch that
arrived as a ref; the actor-id derivation and its deliberate loud failure; the `KontraRunId` /
`KontraActor` upsert without which the run is invisible to the surfaces that read it; the
retry-plus-heartbeat pairing that turns the commit map into resume-from-committed; the `Close` on
every exit path; and the CAS store that keeps results and dropped rows out of workflow history
(**0007**). Two of those fail *silently* when reproduced wrong. The queue is the sharpest: RunBatch's
destination is derived inside the backing workflow from **its own** task queue name
(`workflow.GetInfo(ctx).TaskQueueName`, because workflow code must stay deterministic), so a probe on
a kontra-owned queue that copied that line would compute `kontra-probe-sessions` — a queue nobody
polls, hanging until `ScheduleToStart` with no error. Deriving the Actor's session queue in the probe
instead adds a seventh independent copy of a name **0023** §22 already flagged as having no loud
failure mode.

And the reason that outlives all of those: **what you debug should be what runs.** A debug path with
its own dispatch mechanism has its own timeouts, its own retry count, its own isolation reporting and
its own bugs — so a Method that passes the probe and fails in production, or the reverse, tells the
operator nothing. This is the same dev-equals-prod argument that settled the artefact question, and it
is worth more than the code it costs.

The one thing the probe must handle that a hand-written caller need not: an Actor whose registration
has **no endpoint** — a real, recorded state (`nexusRegistry.ts` header: "`endpoint` is absent, which
is a state the Actors page can show and a later register can repair"). The probe refuses with that
sentence, before dispatching, rather than timing out on a name nobody created.

### 5. Probe output is an ordinary untagged Dataset, and gets no new lifecycle

A probe writes through the same Dataset writer any caller uses, so its output is an ordinary Dataset
with an ordinary name and no tags. **0029** §3 already disposes of it: untagged, it is swept after the
repo's own TTL (default 24h, deliberately not the namespace's retention), with the clock running from
last write and a grace window so a retroactive tag does not race the sweep. An operator who finds a
probe's output worth keeping tags it, and "tagged: kept" applies unchanged.

Nothing new is built here, and nothing new is *allowed* here. A probe-specific expiry, a "probe"
flag on a Dataset, or a special-cased sweep class would be a second lifetime for the same object,
which is the shape **0029**'s temp-versus-sweep clarification exists to prevent.

Stated because it is not derivable: the probe workflow is not a registered folder and has no
`workflow.json`, so `stampRunWorkflow` (`workflowControl.ts`) has no manifest identity to snapshot
unless the probe stamps its **own** name and version through the existing
`PUT /api/runs/:runId/workflow`. It should — a probe's output should read as a probe's. If it does
not, **0029**'s documented Actor-grain fallback renders the name, which is a supported path and not a
defect. Either way this is an existing mechanism, not a new one.

### 6. The generated caller survives as a read-only artefact; the errand does not

`POST /api/sources/actor/:id/caller` keeps generating the caller, and the page shows it **beside the
run button** — read-only, copyable, regenerating as the form changes (which is already
`MethodCall.tsx`'s rule: a source block that outlives its form is a lie). It is the same artefact
`callerFor` emits today, teaching the same shape: `catalog.actor(...)`, a callable handle, a
destructured `(results, dropped)`, an optional Dataset writer.

**What is dropped is the write into a folder.** The save-target picker, the folder shelf, and the
`PUT /api/sources/workflow/:id/file` call that put a generated file on the operator's disk all go from
this flow. Copying beats writing here for the reason **0030** gave for the viewers: an artefact that
lands on disk is code that can diverge from what actually ran, and this one now *always* has something
to diverge from. The teaching survives; the detour does not.

**0030** explicitly warned that a future simplification "must not fold that write away with it" —
this ADR is not that simplification. It removes the write because the feature that needed it is gone,
and it says so here so the removal reads as a decision rather than as the drift **0030** feared. Whether
`writeInside` retains other callers is a question for the change, and an unused route should be removed
outright rather than left as a surface with no caller.

## Consequences

- **The Actors page starts Runs.** A probe is a Run like any other — one execution of a caller's
  workflow (**0023** §12), visible in the Runs surface, with a Temporal history, a two-dimensional
  status, and an ordinary Dataset. There is no "probe run" kind, and there must not be one; the only
  thing distinguishing it is which workflow type it is.

- **kontra now hosts a caller-half worker.** New process, new queue, one workflow type. It is the
  first kontra-owned worker that dispatches to an Actor, and the placement rule that follows is worth
  writing down: it dispatches only, it never *serves* an Actor, and it holds no per-Actor code.

- **A probe of a broken Actor fails like production fails.** The backing workflow retries `RunBatch`
  up to ten times with a two-minute heartbeat before the call fails, so a probe against a hanging
  `@actor.load` takes minutes, not seconds. That is the price of finding 4's dev-equals-prod position
  and it should be shown as progress rather than hidden behind a shorter timeout — a probe that gave
  up faster than production would report a failure production would not have had.

- **Isolated Units are the probe's most useful output and must be drawn.** `(results, dropped)` and the
  ref's `isolated` meta are the whole reason **0028** §4 made the tuple undestructurable-around; a
  probe UI that shows only `results` reproduces the failure mode that let a 15,814-target run report
  `completed` in seven minutes having scanned almost nothing (`runtime/handler/workflow.go:267`).

- **The five stale "404s on purpose" comments are wrong and should be corrected in the change that
  lands this**, to the invariant that is actually true: the orchestrator starts workflows and does not
  execute Batches. Leaving them would leave the next reader arguing against a route that has worked
  since 2026-08-15, and — as `methodCall.render.test.ts:13` shows — one of them is a *test* asserting
  an absence for a reason that no longer holds.

- **Two probes of one Actor run in parallel and share nothing**, because §2 keeps them unkeyed: each
  gets a private anonymous Session, its own actor instance and its own `self.*`. Two probes that
  *should* have shared durable state look like two independent calls, which is the correct reading of
  an unkeyed dispatch and the thing a keyed probe would have to make visible before it could be added.

## Alternatives considered

- **An activity that dispatches, called from the orchestrator.** Not rejected — **unavailable**.
  Finding 1: no SDK exposes a Nexus client outside workflow context. Recorded because it is the design
  everyone reaches for first, and because discovering it at implementation time costs a day.

- **Schedule `RunBatch` straight onto the Actor's session queue from the probe.** Rejected in Decision
  §4. Simpler, skips the backing workflow, and pays for it with six reimplemented behaviours, two of
  which fail silently, a seventh copy of a queue-name derivation, and a debug path that is no longer
  the production path.

- **Keep generating a caller and just make the page serve and start it for the operator.** Rejected:
  it automates the errand instead of removing it, and it leaves a generated file on disk that can
  diverge from what ran (**0030**). It also cannot answer the question the probe is for — "does this
  Method work *right now*" — without first writing to the operator's disk.

- **Host the probe in TypeScript in the orchestrator, restoring `workflows/nexusService.ts`.**
  Rejected on finding 5. The file's own docstring names the interpreter as its caller; TypeScript has
  no caller half to reuse (**0023** §22), so this buys a third independent implementation of the wire
  and another endpoint-name derivation, in the language least able to pin either. Cheaper to deploy,
  and the deployment is the part that is easy to fix later.

- **A general "run this graph" probe, restricted by validation to a single node.** Rejected, and it is
  the important rejection. A shape that *can* express a topology and refuses one by a check will
  eventually have the check relaxed for a good reason, and the interpreter returns without any single
  commit that reads as reintroducing it. The probe cannot express a second Method because it has no
  parameter for one. That is the difference between a decision and a validator.

- **A probe-specific Dataset lifetime** (deleted on close, or a shorter TTL). Rejected per Decision §5:
  a second lifetime for the same object, and it would delete exactly the evidence a debugging session
  is trying to accumulate. Untagged already expires; tagging already keeps.
