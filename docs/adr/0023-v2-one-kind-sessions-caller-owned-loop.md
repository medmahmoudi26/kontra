# 23. v2: one deployed kind, addressable Sessions, and the caller owns the loop

## Status

**Accepted — largely built.** A deliberate breaking change, not a migration. Supersedes **0012**
(the dispatcher shards) and **0015** in part (two of its four state tiers retire, §19). Retires the
**Activity** kind introduced in **0021**, and removes the graph interpreter that **0014**'s dataset
materialization hangs off. **0007** (refs as currency) and **0022**'s keyed identity survive intact
and are load-bearing here.

**Amended 2026-08-18 by 0028:** §3, §13, §15, §16, §18, and §23 are superseded. See 0028 for the
new shape: emit decoupled from Unit, output dataset as a caller parameter, and `(results, dropped)`
return shape. The amendments are recorded in 0028; the sections below remain as decided.

**Built** as of 2026-08-14: `@actor.method`, many and named (§5, §9); the `async with` **Session**
scope on its per-session queue (§4, §6, §7); `unit.emit` with the commit keyed on batch hash +
index (§17, §18); iterator-boundary isolation and re-invoke-with-remainder (§13); raise-by-default
with `isolate=True` (§15); healthcheck-means-end-me (§20) and the durable poison counter (§21); the
graph interpreter deleted (§12); and the Go callee half — `a.Method`, the **Batch**, `Serve` (§22,
§23). The **Dataset** library is built, both halves and both languages:
`dataset(name).batches(size, order_by=)` pages into ref-addressed **Batches**;
`dataset(name).writer()` — Go's `Writer()` + `defer Close` — publishes them, in §11's own
`open` / `sealed` / `abandoned`. A **Method** takes a **Batch** and returns one,
so one **Method**'s output feeds the next with no row entering the caller, and
`batch.batches(size)` re-pages a fanned-out result: a caller sizes its input pages, but a 1→N
**Method**'s output width is otherwise unbounded. The Go caller's side exists under §22's
reversal, its identity derivations pinned against the Python peer's goldens.

**Not built:** §19's retirement — `arun_state` and `session_state` are still exported from
`actorkit`. That is the whole remaining gap.

Two constraints the build surfaced, recorded because neither is obvious: a materialized
**Dataset** stamps no row id, so paging **requires** an explicit `order_by` and refuses without
one; and a **Dataset**'s lifecycle lives as an object beside the data rather than in the DuckLake
catalog, because the catalog's row count is a live SUM and cannot express "still appending".

The eighteen pre-v2 records were archived to `docs/adr/legacy/` by **0024**, which also carries
the table of v1 invariants v2 still honours. This ADR and 0024 are the whole of the live corpus.

**0009 and 0013 — the "step" ADRs — were deleted outright on 2026-08-13**, not superseded. The
concept had no implementation left to supersede: `@actor.step` had been renamed to `@actor.arun`,
`a.Step` had zero callers, and `step.proto`'s `StepOptions` and `kontra.v1.RetryPolicy` were read
by no SDK in any language — so 0009's "author-declared retry, implemented" described a contract
nothing wired, while the real retry was `MaximumAttempts: 3` hardcoded in `runtime/handler/workflow.go`.
Both kept reading as live design and kept sending agents to build a pipeline that does not exist.
Older ADRs still cite them; `docs/adr/legacy/README.md` carries the tombstone that explains why the
link is dead.

## Context

Three separate pressures pointed the same way.

The **Actor/Activity line was drawn on an implementation detail** — whether the code happened to
hold a session — and defending it cost a kind in the catalog, the CLI, the wire and both SDKs. A
**Monitor** was a third kind that is structurally a while-loop.

An **Actor had exactly one `arun`**, so chaining a transformation meant deploying another actor
and an edge between them. Every multi-step pipeline paid a graph for something a method call
expresses.

And the **graph interpreter owned too much**: sharding, failure policy, materialization, the
streaming cursor. A caller who wanted to drive dispatch themselves (**0021**) got none of it, which
made their own workflow a second-class citizen in kontra's own query surface.

The measured constraint that shapes everything below: a workflow that schedules one activity per
unit pays ~3–5 history events per unit against a 51,200-event ceiling. Measured on this stack,
`batchSize:1` capped near 5,100 units. So the caller may own the *loop*, but a **Method** call
must carry a **Batch**.

## Decision

Twelve forks, resolved in one session. Numbered because later ones depend on earlier ones.

1. **Materialization is caller-invokable.** Any workflow can publish a **Dataset**; the interpreter
   stops being the privileged producer. Without this the model can consume datasets but never
   produce one, and cannot compose with itself.
2. **A `@actor.method` receives the whole Batch**, not one unit. The caller passes a batch, so the
   method receives a batch.
3. **The author loops, the framework commits on yield.** The **Batch** arrives as a
   framework-owned iterator, so position is known; each `yield` is a commit point. This keeps
   per-unit commit, isolation and mid-batch resume while giving the author cross-unit context.

   **Amended 2026-08-18 by 0028:** The input cursor (position in the **Batch**) and output
   position (offset in the dataset) are now independent save points. The caller supplies the
   output dataset as a parameter; the author always pushes, the caller decides whether the
   destination has a name. The framework's save point is the input cursor only (which **Unit**
   was last processed); output offset is tracked separately.
4. **Activation is an explicit scope** — `async with crawler["acme.com"] as browser`. The workflow
   owns the lifetime, and the workflow is already durable.
5. **The decorator is `@actor.method`.** `@task.defn` and `@actor.step` both collide with words
   `CONTEXT.md` bans as **Unit** and node-position synonyms — which is how the retired `@actor.step`
   ended up stale in `contracts/` and both generated SDKs.
6. **A per-session task queue binds the calls.** `{actor}-{version}-s-{sessionId}`, polled only by
   the activating worker. The queue name is the address: no placement directory, no lease, no idle
   policy. Temporal's Worker Sessions would be the built-in answer and are Go/Java only.
7. **Losing the host fails the scope.** It raises; the caller reopens and resumes from the cursor
   it already holds, so the blast radius is one **Batch**. The invariant bought is the point: while
   a **Session** lives, `self.*` is coherent — state either survives or you get an exception.
8. **A Session is the scope, and state spans it.** Requires the commit map to key on batch cursor
   + index rather than bare unit index; keyed on position alone, batch N+1 replays batch N's
   outputs, which is the hazard **0022** §3 was written to stop.
9. **One deployed kind: the Actor.** An **Activity** is an **Actor** with one **Method** and no
   load/close. A **Monitor** is a caller's workflow whose loop runs on the control machine while
   the observation stays a **Method** call on the fleet.
10. **Keys are optional.** `crawler["acme.com"]` is a shared, exclusive virtual object with durable
    state; bare `crawler` is a private anonymous **Session**. Keying is a claim on a shared
    identity, never a tax on an ordinary dispatch — and mandatory keys would mean two independent
    scans of one host silently receiving each other's results.
11. **Datasets append as they go and are explicitly sealed.** `open` / `sealed` / `abandoned`, so a
    crashed **Run** leaves a visibly open **Dataset** rather than a short one that reads as
    finished. Same shape as the two-dimensional run status, applied to the artifact.
12. **Run survives redefined; Node, Chunk and Manifest retire.** A **Run** is one execution of a
    caller's workflow, which is already true in code (`rid` defaults to `info.workflow_id`).
13. **The framework isolates at the iterator boundary.** The **Batch** iterator knows which
    **Unit** is current, so a raise in the body is attributed to that **Unit**, recorded, and the
    **Method** re-invoked with the remainder — cheap and equivalent, because everything pushed so
    far is already committed. The naive loop with no try/except does the right thing. A **Method**
    is therefore entered several times per **Batch**: locals reset between entries, `self.*` does
    not.

    **Amended 2026-08-18 by 0028:** "yielded" is amended to "pushed" since 0028 §18 decouples
    emit from the Unit and moves output to a dataset parameter. The isolation boundary remains
    at the iterator — which **Unit** is current — and re-invocation resumes the loop with the
    remainder.
14. **Nothing fails a Batch on the framework's judgement.** No failure budget, no threshold. A
    dependency outage is detected by the caller, using a health **Method** the actor's author
    exposes — which is what multiple **Methods** are *for*, and better than a framework knob. Its
    limit is stated so it is not discovered: a health call is pre-flight, not mid-sweep.
15. **A Method call raises by default.** `browser.crawl(batch)` raises if any **Unit** failed;
    `browser.crawl(batch, isolate=True)` returns results and failures to inspect. The framework
    still isolates *inside* the **Batch** (13) — this is only what the caller sees. It keeps the
    caller deciding, while making the decision explicit rather than implied by silence, because
    after 14 the caller who forgets to check *is* the failure mode.

    **Amended 2026-08-18 by 0028:** This section is superseded. A **Method** call now returns
    `(results, dropped)`, a tuple the caller must destructure. The tuple is the forcing function:
    you cannot reach `results` without binding `dropped`, which is more visible than `isolate=True`.
    Raises-by-default and the `isolate=` flag both retire. See 0028 §4 for the replacement.
16. **Composition is the author's, and it is just function calls.** Chaining two stages inside one
    **Method** is `unit.emit(extract(crawl(unit.value)))` — ordinary calls in the one loop, zero
    history, nothing to design. No pipe operator, no declared topology (which would be the graph
    again, drawn in decorators). Only the dispatched **Method** commits, which is right —
    intermediates should not be durable. Accepted limit: a caller cannot combine two published
    **Methods** their author did not anticipate.

    *Simplified 2026-08-13* by §18's amendment — this was generator-to-generator piping while
    **Methods** were generators. Function calls compose identically in Go, which generators do not.

    **Amended 2026-08-18 by 0028:** The composition within a **Method** remains unchanged
    (function calls, no graph). The phrase "zero history" is sharpened: no history because only
    the **Method** call itself is durable, not intermediate function calls inside it. Composition
    is author-written function calls, not a declared topology.
17. **A committed Unit is keyed by the Batch's content hash plus its index.** Stable across a
    retry by construction, distinct across **Batches**, and — the property that matters after 7 —
    it survives a reopened scope, because a hash does not know its scope died. A sequence number
    would restart at zero on exactly the recovery path v2 makes normal.
18. **Emit is a call, and it names its Unit.** `await unit.emit(x)` / `unit.Emit(x)` — never a
    generator yield. Attribution survives any concurrency the author writes because provenance
    rides the receiver, not a position in control flow. This is what replaces `concurrent_aruns`:
    the framework stops owning the window and stops having to guess.

    *Amended 2026-08-13* — originally `yield unit.result(x)`. Prior art is unanimous that a
    durable step should be named explicitly rather than inferred from where you are in a loop:
    Restate journals `ctx.run(key, …)` and replays by journal entry, so loop shape is irrelevant;
    Kafka consumers loop freely and commit by offset; Lambda's SQS integration takes a batch and
    returns `batchItemFailures` by `itemIdentifier`. Yield-position was the one Python-shaped part
    of this design and it does not survive the trip to Go (§23). Consequence: **Methods are not
    generators**, which also makes 13's re-invoke-with-remainder an ordinary function call rather
    than a partly-consumed generator to restart.

    **Amended 2026-08-18 by 0028:** This section is superseded. Emit is now decoupled from the
    Unit. A **Method** receives a dataset parameter (the caller's third argument) and pushes
    records via `await dataset.push(x)` / `dataset.Push(x)`. The framework's save points are the
    input cursor and the output offset, independent. An author is free to emit zero, one, or many
    records per **Unit**; the question "what granularity does emit support?" disappears because
    output no longer names a **Unit**. Provenance is author-supplied inside each record. See 0028
    §1 and §18 for the new shape.
19. **Three state tiers, not four.** `self.*` (in-memory, the **Session**), `object_state` (key),
    `global_state` (name). **`session_state` and `arun_state` retire.** Every path that could read
    durable session state runs in the same process on the same instance, where `self.*` already
    works — a re-invocation after 13, a Temporal retry on the session queue that only that worker
    polls. The one path that loses `self.*` is host death, which fails the scope (7), so the
    reader is gone too. `arun_state` has the same problem: an isolated **Unit** is not resumed, so
    nothing reads its checkpoint. The rule becomes one sentence — durable means keyed, in-memory
    means scoped.

    *Amended 2026-08-14, when building it.* **`session_state` retires as decided. The per-Unit
    tier does not — it is RENAMED `unit_state`.**

    The half that was right: `session_state` had no reader, and deleting it cost nothing. Both
    SDKs lost it, along with the `s-` key scheme and the `s-index` TTL list.

    The half that was wrong: "an isolated **Unit** is not resumed, so nothing reads its
    checkpoint" is true of an ISOLATED **Unit** and only of that one. It misses the death that
    actually happens — a **Unit** that was IN FLIGHT when the *activity* died, which re-runs on
    the handler's retry (`MaximumAttempts: 3`) against the same session queue, the same worker
    and the same **Batch** hash, and therefore the same slot. It reads exactly what it wrote.
    `examples/python/crawl4ai` depends on this for its resumable tier, where one **Unit** is a
    crawl that walks for minutes: without the tier a host blip restarts the whole seed.
    `tests/test_state_key_congruence.py` now pins that reader directly, because the argument for
    deleting it was a claim about behaviour that no test held.

    The NAME goes regardless — `arun` was coined to mean one **Unit**'s run and v2 means nothing
    by it. `unit_state` is what the tier was always describing, and it is what Go already called
    it (`UnitState`, `unit.State()`), so the rename makes the two SDKs agree instead of differ.

    What this costs the headline: it is three DURABLE tiers only if you count the way the
    original did. Honestly stated — `self.*` is memory, not state; `unit_state` is resume
    scratch the framework drops on commit, not a place to keep facts; `object_state` and
    `global_state` are where durable facts live. The one-sentence rule survives intact, because
    scratch is not a fact.
20. **A dead resource fails the scope too — one rule, not two.** `@actor.healthcheck` stops meaning
    "reload me" and starts meaning "end me". Reloading in place is what the framework does today,
    and it silently resets `self.*` mid-**Session** — the exact behaviour 7 rejected for host loss.
    Barely more expensive than a reload, which reruns `@actor.load` anyway; the gain is that the
    **Session**'s promise has no third case.
21. **The framework counts poison Units and isolates them.** A **Unit** that has killed N scopes is
    recorded as a failure and skipped — today's `_MAX_UNIT_RELOADS`, ported to 17's key scheme.
    Without it, 20 plus 17 is a tight loop: fail the scope, reopen, resume at the poison **Unit**,
    die again. This is framework-internal machinery beside the commit map, **not** a fourth author
    tier, so 19 is untouched. The one place a threshold beat caller autonomy, because the failure
    mode here is an infinite loop rather than a quiet wrong answer.
22. **Go implements the callee half only.** **Actors** in Go; callers stay Python. Consistent with
    **0021** leaving the caller's side Python-only, and v2 makes that side much larger — the
    `async with` scope, the Dataset library, the paging loop. None of it is ported.

    *Reversed 2026-08-14.* **Go gets the caller's side too.** The original reasoning was a cost
    estimate, and the estimate was wrong: `go.temporal.io/sdk` is already a direct requirement of
    `sdk/go`, and a workflow-side Nexus client compiles against its existing dependency
    graph with `GOPROXY=off` — nothing is downloaded, nothing is vendored, no module gains a
    dependency. What remained was authoring, and authoring is not a reason to make a language
    second-class.

    The shape survives the trip because §18 already made it survive: emit names its **Unit**
    rather than a position in control flow, so nothing here needs `async`. `for batch, err :=
    range ds.Batches(ctx, 200)` is the peer of `async for batch in ds.batches(200)`, and
    `defer s.Close(ctx)` is the peer of `async with`. Both are the plain idiom of their language
    rather than a translation of the other's.

    The paging iterator is `iter.Seq2[*Batch, error]` and not a bare `iter.Seq`: a Go iterator
    cannot return an error, and the community settled on the two-value form post-1.23 precisely
    because it is the only shape where ignoring the error is visible at the call site. It is the
    honest peer of Python's raising `async for`.

    What this costs, stated: a **sixth** independent derivation of the session-queue name, since
    `runtime/handler/internal/identity` is unimportable by the decoupling rule. A drift there does not
    raise — it dispatches into a queue nobody polls and hangs until `ScheduleToStart`. The
    derivations are pinned against each other by test.
23. **Go gets the Batch too — full parity on the author's loop.** `func(s *Session, b Batch) error`
    with `for unit := range b.All()` and `unit.Emit(res)`; `go 1.26.4` means range-over-func is
    idiomatic, not contorted. Author-owned looping is v2's central idea and cross-unit context
    (a bulk API taking 100 at a time, one transaction per **Batch**, dedupe within a **Batch**) is
    not a Python luxury. A Go SDK that keeps the framework's per-unit callback would be v1 with new
    names, and would make `CONTEXT.md`'s **Method** entry false in one language.

    Note what Go gives up by this: `fn(*Session, Unit) ([]Unit, error)` made provenance and
    isolation *structural* — the returned units are unambiguously that unit's, and `return err`
    marks that unit failed. §18's amendment is what makes losing that cheap: `unit.Emit` recovers
    provenance explicitly, in the same shape as Python. `Emit` needs a mutex where Python's does
    not, because goroutines are genuinely parallel and asyncio is not.

    `a.Step(name, fn)` already exists and is **not** a rename away from this. Its doc comment reads
    "declaration order IS the data-flow contract: the first step consumes the input units, each
    step's output feeds the next" — declared topology, which §16 rejects. Steps become
    independently dispatchable **Methods** and the chaining goes away. `SessionLost(msg)` already
    exists and maps onto §20 unchanged.

    **Amended 2026-08-18 by 0028:** The per-unit callback shape (`fn(*Session, Unit)`) retires
    because emit no longer names a Unit. The new shape is `func(s *Session, b *Batch, ds
    *Dataset) error`. The dataset parameter is the caller's; emit becomes `ds.Push(x)`. The three
    parameters (self, batch, dataset) match Python's shape. Author-written concurrency is still
    supported because push is a method call on a parameter, not a side effect of the loop. See 0028
    §2 for the full shape.

## Considered and rejected

- **Landing this additively**, with `@actor.arun` reimplemented as a **Method** whose loop the
  framework owns. The wire barely moves — a **Method** call is an `EntryInput` with one more field
  — so this was available and was the recommendation. Rejected in favour of a flag day: carrying
  two models through the catalog, CLI, contracts and both SDKs was judged worse than the break.
- **A true virtual actor** — implicit activation with an idle timeout. Rejected: it needs a
  placement directory and a deactivation policy, `close` becomes best-effort, and it rebuilds
  precisely the Dapr machinery **0018** removed for not delivering its one guarantee.
- **Per-call activation** (load/close bracket every call). No affinity problem at all, but
  relaunching the held resource per call deletes the reason load/close exist.
- **Transparent re-activation** after host loss. Maximum uptime, but any `self.*` the author
  accumulated silently resets while their logic keeps running — the failure shape that reports
  `completed` with nothing in it.
- **Keeping Manifest** beside Dataset as the internal record. Two words for one idea, separated
  only by whether somebody named it; they would be used interchangeably within a month.

## Consequences

- **Flag day.** Every deployed actor image is invalid at once, the fleet needs a full redeploy, and
  anything in flight dies with it. This was chosen with that stated.
- **Keyed state outlives everything that used to bound it**, including a re-run. A **Method** that
  opens by checking a keyed dedupe set will, on a genuine re-run, do nothing and seal an empty
  **Dataset** — correctly. The example dialogue in `CONTEXT.md` exists to teach this.
- **Dynamic task queues.** One per live **Session**. Temporal treats queue names as strings, but an
  orphaned queue means calls sit until `ScheduleToStart` fires, which is what makes §7 a decision
  rather than an accident.
- **Concurrent read-only handlers still have no peer.** Every **Method** call takes its key
  exclusively, as in **0022**.
- **The whole `arun` vocabulary goes.** `@actor.arun` → `@actor.method` (5), `concurrent_aruns` →
  deleted, the author writes their own concurrency (18), `arun_state` → deleted (19). The word was
  coined to mean one unit's run and nothing in v2 means that any more. It survives in **0015**,
  **0016** and the git history, where a reader will meet it — the same situation as the retired
  **Turn**.
- **A Method is entered many times per Batch**, once per isolated failure (13). Author locals reset
  across entries while `self.*` persists. This is the sharpest thing an author must know that they
  would not guess, and belongs in the decorator's docstring rather than only here.
- **`isolate=True` is a call-site flag, so it can be forgotten in the safe direction only** — the
  default raises. Worth keeping that asymmetry if the API is ever revised.
- **Cross-Actor composition is unchanged and still pays for Refs.** 16 is an intra-**Actor**
  optimisation only; two **Actors** are two processes and the data travels as **0007** says.
