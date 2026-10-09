# 62. A live report is the same render on a shorter clock

Date: 2026-10-08

## Status

**Accepted.** Extends **ADR 0055**; supersedes two of its sentences, named in §9.

## Context

ADR 0055 made a report *a rendered snapshot of what a Run returned*. The word *returned* is doing all
the work: `contextForRun` publishes `result` only when the status is exactly `completed`
(`report/context.ts`), so until a run ends there is nothing for a template to say, and the sweep that
renders closed runs ran on a **15-minute interval** — which is how long a finished run could show
nothing at all.

Two complaints, one from each end. An operator watching a long campaign has no report until it is
over. And an operator whose run *just* finished waits up to a quarter of an hour for a document that
is already computable.

The obvious shape — re-render on a timer while the run is open — does not work, and the reason is
worth stating because it is the first thing anyone tries: **`contextForRun` queries nothing.** Its
only awaits are the template, the close event, the identity and the next version number. So a
re-render against today's context is **byte-identical for the whole life of the run**, and a live
report built that way would stream the same `{% else %}` branch twenty times and call it progress.

A live report therefore needs new *context*, not new *plumbing*. Everything else follows from
deciding what a template may see while its run is still going.

## Decision

### 1. One renderer, two modes, and the second one never persists

The same LiquidJS instance, the same maximal-then-parse escaping, the same `code` tag, the same
64-filter allowlist, the same closed filesystem, the same `parseLimit`/`renderLimit`, the same
worker-thread isolation. Live mode is `report/live.ts` holding *state*, not a second engine.

**Nothing in the live path calls `declareVersion` or `putSecrets`.** `report_version` carries
`UNIQUE (run_id, render_key)` and `renderKey` hashes the context's data, so a run whose `result` moves
every tick would mint a version per tick and the version dropdown would become the tick history. The
single stored version is rendered at the end, by the path that always did it.

A **failed** live render publishes nothing either. The viewer keeps the last good document, and
nothing writes an `error` version — which `versionByKey` would find and skip for ever, welding the
report shut on what may have been a timeout. Unlike a template edit, the key does not move, so that
door has no handle on the inside.

### 2. Three new context values, and the sixth root

- **`datasets.<name>`** — `{rows, batches, last_commit_at, head, tail}` for the run's own Datasets.
  **Bounded at the context, not at the template:** `head` is clamped to 20 and `tail` to 50 in
  `buildContext`, so no template can ask for more and no author discovers that a report got slower as
  a Dataset grew. This is the sixth root, and it is added to `cli/reportlint.go`'s `contextRoots` as
  well as to the engine — an unknown root is an `UndefinedVariableError` under `strictVariables`,
  which is to say *after the run*, which is the one time a report cannot be fixed by editing it.
- **`run.progress`** — `{units_done, units_total, isolated, phase, updated_at}`. `units_done`
  **includes** `isolated`: ADR 0060 is explicit that a unit abandoned after repeated failure is
  finished, and counting it as outstanding makes a healthy run read as stuck for ever.
- **`result` while the run is OPEN** — the workflow's own `report` query. A workflow MAY define a
  query handler named `report` returning whatever partial it likes; the orchestrator calls it at
  render time. A query writes no history events, so this costs one round trip and nothing durable.

### 3. `renderKey` projects the new values out, and that is what makes this deployable

`renderKey` hashes `run` **whole**. A field added under it moves the key for every run that already
has a stored version: `versionByKey` misses, and the convergence pass re-renders the entire retention
window on deploy, growing a duplicate in every finished report's dropdown.

So `renderKey` projects `progress` out of `run` and excludes `datasets` entirely. **Projecting a field
out is backward-compatible** — a run that never carried `progress` hashes exactly as it did before —
and `report/context.test.ts` asserts that rather than assuming it.

`renderKey` must only ever be called with a **final** context. For an open run `result` is the
workflow's query, which also moves per tick; live renders are never persisted, so no key is minted
for one.

### 4. The trigger is an in-process bus, and Postgres `NOTIFY` is refused rather than deferred

The specification asked for `NOTIFY kontra_run_data`. Four facts, each measured:

1. `resolveStoreUrl` falls through to the literal `'orchestrator.db'`, so the **default store is
   SQLite**, which has no `LISTEN`/`NOTIFY` at all.
2. `PgPoolLike` is `query` + `end` only — no client checkout, no `notification` event — so nothing
   can listen on it.
3. `createPool` is `max: 2`, *"Small on purpose"*, and a `LISTEN` holds its connection for the
   process's life.
4. `pool.query('LISTEN …')` would **succeed** and then deliver nothing for ever.

Green and inert is the one outcome worse than a visible failure, so it is refused on those grounds.

In-process is correct rather than merely expedient: the orchestrator is three roles in one PID
(**ADR 0031 §1**) and `resolveRoles` returns all three when `KONTRA_ROLES` is unset, which is the
shipped default — nothing in `docker-compose.yml` sets it. The process that commits a batch is the
process that serves the stream.

**But the split is expressible, so `observesCommits()` exists.** `KONTRA_ROLES=api` yields a process
that serves reports and never sees a commit, where a live view would otherwise stream a document
frozen for reasons nothing states. The snapshot event carries the reason instead. Redis pub/sub is
the named extension point if a split ever needs to work; `ioredis` is already a dependency.

The emit sits **after** `conn.run('COMMIT')` in `data/parquet.ts` and never inside the transaction: a
live report may only show rows that are durable, and an emit inside would announce a batch a
`ROLLBACK` then erased. Every dispatch is guarded, because `emitData` is called from the publish path
and a renderer throwing on a listener would fail an activity whose rows already landed — turning a
cosmetic report bug into a retried batch and a double insert.

### 5. Run-end becomes an event; the interval survives with a different job

`watchTerminal` parks `handle.result()` — a long poll on history under the SDK — **per watched run**,
and finalises on the terminal event. A watched run now freezes within seconds. `handle.result()`
*rejects* for a failed, cancelled, terminated or timed-out run, and the rejection **is** the signal;
such a run still gets a report, and the report is what says which happened.

**Per watched run, not per open run,** and that is the bound: at most one poll per live session, and
sessions are capped. A poll per open run could reach Temporal's per-namespace long-poll limiter, whose
`RESOURCE_EXHAUSTED` the SDK's default retry interceptor retries ten times on the shared channel
`/api/runs` uses — so it would have needed its own `Connection` and its own pool cap. Scoping it to
the runs a person is actually watching removes the problem instead of managing it.

**The 15-minute interval becomes 60 seconds and keeps its place, and deleting it would have been a
regression dressed as a cleanup.** `renderKey` covers the template hash, so fixing a typo in
`report.md` changes the key and the next pass re-renders every affected finished run — the back
catalogue heals itself. A terminal-event watcher is structurally blind to that: the run is closed, its
history is final, and no event will ever arrive for an edit to a file. **The pass is the convergence
loop for (template × run); completion was only its most visible input.** It is also reconciliation
for runs that ended with no watcher parked.

A pass that renders anything now says so. In steady state it should find nothing; a render is either a
template edit healing the back catalogue, which is expected and occasional, or a run the watcher
missed, which is not. Silence is the signal, and without the line the two are indistinguishable.

### 6. The stored version is rendered from the final context, never promoted from a live frame

An earlier draft promoted the last live render's bytes into the stored version, to make them equal.
That is wrong twice.

It contradicts ADR 0055's premise — a report snapshots what the run **returned**, which is only
knowable at the end — and it collides with the sweep. The sweep computes `renderKey` from the
*finished* context; a promoted frame would carry a key computed from a *mid-run* one, the keys would
never match, and the next pass would render fresh and insert a **second** version. The page defaults
to newest, so a user would watch a report freeze and then be silently handed a different document.

So the freeze is a **re-read**: on terminal, one scoped sweep pass over that single run renders and
stores it, and the console re-reads. The transition is **visible by design** — `result` appears and
the `{% if result %}` branch flips. A byte-identical final frame would be the bug, not the goal.

### 7. Per-block diffing keys on the mdast child index, and a block's identity covers its bytes

There is nothing to split: the renderer already returns a tree, and `snapshot.root.children` **are**
the top-level blocks. Writing a Markdown splitter would be a second definition of a block boundary.

**The subtree alone is not a block's identity.** A `code` node carries a `blockId` indexing a
*sibling* map holding the actual bytes, so hashing the subtree alone makes a block whose content
changed hash as *unchanged* — and it would never be patched. The referenced entries are folded into
the hash, sorted by id.

A changed child count is not diffable by index — one more row in a `{% for %}` shifts every block
after it — so that case sends the document whole. A render whose output is identical sends **no
frame**, not an empty patch. A block whose serialised form exceeds 1 MiB is replaced by a marker
naming where to read it.

### 8. No `reveal`/`raw` while live, and the reason is disclosure rather than tidiness

`/report/blocks/:blockId/reveal` defaults to the **latest stored version** when `?version=` is absent,
and `blockId` is **positional** (`b1`, `b2`, …) and exists only on `{% code %}` nodes. A reveal against
a live block would therefore serve a *persisted* version's block of the same ordinal — a cross-block
disclosure, not a 404. Live snapshots ship nodes only; both byte routes are untouched.

This also means ADR 0055's positional-id invariant — *a re-render of the same run produces the same
ids, so a stored link to `b2` keeps meaning what it meant* — stays literally true rather than
true-with-an-asterisk. It holds for a finished run; live mode simply never mints such a link.

### 9. What this supersedes in ADR 0055

- *"`result` is `null` when the run did not complete"* → `result` is `null` for every **terminal
  non-completion**, and is the workflow's `report` query while the run is **open**. The terminal case
  is unchanged and still load-bearing; what changes is that `{% if result %}` is true mid-run for a
  workflow defining the handler, so **`run.status` is the completion test** from here on.
- *"`run`, `workflow`, `input`, `result`, `report`. No environment … nothing else is ever put in"* →
  plus `datasets`. Still an allowlist built rather than a view filtered.

Everything else in ADR 0055 stands, including §4's rule that a ref is resolved from object storage
and never from an actor — which is why bulk content is a ref and `datasets.head/tail` is bounded.

## CORRECTION, 2026-10-09: the consumer shipped without a producer

**Everything above describes a context that nothing fills.** Found by the session driving the live
install, after an operator said a live report "just finishes and gives the final report".

`buildContext` accepts `progress`, `datasets` and `partial`. `contextForRun` passes **none of them**,
`renderOnce` passes neither progress nor datasets, and **nothing anywhere calls the `report`
workflow query**. So for an open run `run.progress` is null, `datasets` is `{}` and `result` is null
— and the document is byte-identical from the first frame to the last.

That is the exact defect this ADR opens by diagnosing in the original brief: *re-rendering while a
run is open produces a byte-identical document for the run's whole life.* The diagnosis was right;
the implementation reproduced it. The context shape, the clamping, the `datasets` root, the lint
entry, the SSE route, the hub, the block diffing and the patch protocol were all built and tested —
and the handful of lines that would have made them carry anything were not.

**What the tests proved, and why they passed anyway.** `report/context.test.ts` builds contexts by
hand and asserts the shape. `report/live.test.ts` drives the hub with stub renders. The console
suite drives a stub SSE stream. The workflow suite calls its own query handler directly. Each half
was verified against a fixture of the other half. **Nothing exercised the seam**, so every suite was
green over a feature that could not work. A browser check caught a chrome bug; it did not catch
this, because a live report with an unchanging context looks exactly like a slow one.

Being wired now on `canary-report`: a ~2s tick for open runs with viewers, `run.progress`,
`datasets.<name>` from the pushed-record tail, and the `report` query. `duration_s` is fixed here —
it read 0 for every open run, since it keyed on `closedAt > startedAt`.

**The tick reverses this ADR's "trigger on data arrival, not a timer", and that reversal is right.**
The rule was written because a timer re-renders an unchanging context — true while nothing filled
it, and not an argument once it changes between ticks.

## Consequences

### 10. Redaction is inherited, and that decided the architecture

`renderWorker.ts` passes `redactLatin1: redactHttp, redactText: redactSentence` into the render, so
redaction happens at the **render boundary**. A new context root inherits the `http_headers` rule
without new wiring.

This is why the orchestrator reads the Dataset rather than the workflow publishing report content into
a durable store. An earlier design put facts in the Temporal **memo**: `upsert_memo` redacts nothing,
and `redact()` is the wrong rule for target-derived data anyway — the corpus measures
`Cookie: a=1; b=2` → `Cookie: [redacted] b=2` (half-redacted, reads as safe) and
`{"password": "hunter2"}` → **untouched**. Since `datasets.head/tail` is captured headers, URLs and
response fragments, keeping it inside the one boundary that already has the `http_headers` rule is the
whole argument.

### 11. The caps are three, because one is satisfied by a flood

50 viewers per run → 503. That alone is satisfied both by a flood onto one id and by a flood spread
over ids that do not exist — and `rowTail.ts` records the measured incident: one unauthenticated
client opened 250 streams on 250 **fictional** run ids and got 250 pollers, 750 LISTs in five seconds,
because the id was never validated. So there are global arms too (64 runs, 256 viewers), the run id is
resolved against Temporal **before any per-run state is minted**, and 503 is used rather than 429
because the service is at capacity rather than the caller over a quota they cannot discover.

### 12. The route spelling is part of the auth posture

`GATES` is longest-prefix over the raw Fastify url, so `/api/runs/:runId/report/live` inherits
`exploreToken` from the `/api/runs/:runId/report` entry and needs no new entry. The `:id` spelling
would fall through to `/api/runs` and be published as `runToken` — a **different credential from the
one the handler checks** — and `openapi.test.ts` cannot catch that, because it verifies that paths
documented OPEN are open. Two such mismatches already exist in this tree and are worth their own fix.

`admitReport` is exported and shared rather than copied. Two implementations of one gate is how a
documented posture and an enforced one drift apart.

### 13. `fetch` + `ReadableStream`, not `EventSource`

The route is bearer-gated, `EventSource` cannot set a header, and the console installs its credential
by **wrapping `window.fetch`**, which `EventSource` does not go through. So reconnection is written by
hand, following `run/logstream.ts`. A token in the query string was rejected there because it lands in
the orchestrator's access log, and that rejection stands.

**A connection is not progress; a frame is.** Resetting the backoff on a successful connect gives a hot
reconnect loop against a server that accepts the stream and then closes it — which is what a redeploy
looks like for a second. Only data resets it, and only to the first non-zero step.

404, 409 and 503 are **refusals**, not dropped connections, and are not retried.

### 14. What this does NOT solve

- **A quadratic regex in the render path now costs its time per tick.** 18 seconds once is 18 seconds
  every tick. Anything added that parses an interpolation, an info string or a block marker needs a
  measured linearity check, not an eyeballed one. The existing `thousands` formatter is safe only
  because `Math.abs(n).toString()` bounds its input to 21 characters — a bound living in the caller,
  which a later refactor removes without noticing.
- **A run started by `kontra workflow start` pins no template** — the CLI dials Temporal directly and
  nothing maps a run id to a folder. Such a run renders the default template, live and final alike.
- **Perspective and Arrow are declined, not deferred pending taste.** The context bounds what a
  template can show, so there is nothing to virtualise. A 600 KB gzip budget in CI is what makes that
  decision durable; this console already removed `ag-grid` for the same cost with nothing to catch it.
- **The `report` query needs a live worker.** A workflow whose worker is gone answers no query, so
  `result` falls back to `null` and the template shows its `{% else %}` branch. That is the honest
  answer, not a degradation to paper over.
- **Nothing here reduces the render cost itself.** A template that is expensive to render is expensive
  once per data change, and the debounce plus the one-in-flight rule is the whole mitigation.
