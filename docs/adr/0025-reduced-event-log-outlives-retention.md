# 25. The reduced event log outlives retention: the blob plane, swept after close, kept

## Status

**Accepted.** Extends **0007** — the claim-check blob plane grows a second named, non-content-addressed
object, alongside the one **0023** §11 already put there. Does not touch **0023** §1: the archive is
explicitly **not** a **Dataset**, and the reason is that ADR's own definition of who publishes one.
Supersedes nothing. Implemented by `control/orchestrator/src/historyArchive.ts` over the reducer in
`control/orchestrator/src/history.ts`.

## Context

A **Dataset** outlives the **Run** that wrote it by design: it is durable, queryable, and still
there months later. The Run's *story* is not. Once Temporal's retention drops the execution, the
rows survive and the account of how they came to exist does not — and every other slice of the
monitoring plane made that account richer, which makes losing it worse. The 294-second
`nscheck-1786831339` run that this whole feature exists because of is 305 events, of which four
rows are the 156 seconds the console could not explain until slice 05 taught the log to drill.

Five things were measured on the local controller before deciding anything. All of them are on
Temporal **Server 1.31.0** (`temporal server start-dev`), namespace `default`, 2026-08-16.

1. **Retention is 24 hours here, and it is a namespace config, not a code constant.**
   `temporal operator namespace describe` reports `Config.WorkflowExecutionRetentionTtl 24h0m0s`.
   Nothing in this repo sets it; it is the dev server's default, and a production namespace would
   carry a different one. So no design may depend on a particular number — only on the fact that
   there is one.

2. **The cluster holds only the last few hours, and every execution in it is closed.**
   `temporal workflow list` returns nothing older than the `nscheck-1786831339` run (3 hours old at
   the time of writing) and nothing in a non-terminal state. The campaigns from previous weeks —
   whose Datasets are still in the lake and still queryable — have no history at all.

3. **A run that dies is CLOSED, which is the fact this ADR turns on.** The issue behind this
   decision worried that "a run that dies never reaches a clean close", and concluded that the
   archive might therefore have to be written incrementally, as the run went. That worry dissolves
   under one probe: a workflow started on a task queue nobody polls — so it never ran a single
   workflow task — and then terminated reports
   `closeTime: 2026-08-16T01:04:59Z`, `closeEvent: EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED`,
   `historyLength: 3` (probe `kontra-archive-probe-1`, deleted afterwards). **Failed, terminated,
   timed-out and cancelled executions are all closed executions**, and retention is measured from
   the close. A run whose story matters most is therefore exactly as archivable as one that
   finished — *provided the thing doing the archiving is outside the run*. A caller-side archive
   step would indeed have had the problem the issue described: the step that writes the archive is
   the step a dying run does not reach.

4. **Temporal's own history archival is off, and is the wrong shape anyway.**
   `Config.HistoryArchivalState Disabled`, `Config.HistoryArchivalUri` empty. See the alternatives.

5. **The reduced log is small, measured rather than assumed.** Archiving the three runs this
   cluster still holds wrote **38,123 / 53,635 / 53,011 bytes** — the last being that 305-event
   run, so **~174 bytes an event**, because the reducer keeps a type, an offset, a duration, an
   attempt, one line of metadata and (since slice 05) a link, and never a payload (0007). At
   `EVENT_CAP = 1000` a log cannot exceed roughly **175 KB**, whatever the run did. That bound is
   what makes "keep it" an honest answer rather than a deferred storage problem.

And one property of the existing store that decided *where*: the blob plane **already holds named,
non-content-addressed objects**. `datasetStateKey(name)` → `datasets/<name>/_state.json` is written
with a plain `store.put` by `publishBatch`/`closeDataset` and read back by `datasetState`
(`control/orchestrator/src/data/datasets.ts`, `control/orchestrator/src/activities/datasets.ts`). It sits in the
same bucket as `cas/` and `units/`. Verified in the code before it was written down here, because
the issue's own framing — "invents a second class of thing stored there that is not
content-addressed" — is only a real cost if that class does not already exist. It does.

## Decision

- **The archive lives in the claim-check blob plane, under the run key, in the hive layout with
  `run=` first**: `history/run=<workflowId>/dt=<YYYY-MM-DDTHH-MM-SS>/log.json`. `run=` leads
  because that ordering is a measured decision, not a taste — it bought a 50× LIST win — and
  because the read this key exists to serve is always scoped to one run. `dt=` is the run's START
  instant, in the same partition spelling `units/` and `output/` use, and it is there so that a
  **reused workflow id gets its own object** rather than overwriting the story of the run before
  it. Reading takes the newest `dt=` under the run's prefix: one narrow LIST plus one GET, on a
  page that by construction has nothing else to show.

- **It is not a Dataset, and that is a statement about who publishes.** ADR 0023 §1 made
  materialization *caller-invokable* — a **Dataset** is a thing a caller publishes, and its
  lifecycle (`open`/`sealed`/`abandoned`) is a caller declaring what it did. This log is written by
  the system on the caller's behalf, about the caller. Making it a Dataset would put a system
  artefact in the operator's Dataset listing, give it a lifecycle nobody is entitled to declare,
  and blur the one line 0023 drew. The blob plane costs nothing to extend and says the right thing:
  this is evidence, kept beside `units/`, not output.

- **The artefact is the reducer's output, verbatim, in a versioned envelope.** `mapHistory`'s
  `RunHistory` — payload-free, the same `EVENT_CAP = 1000`, the same `HEAD_KEEP = 25`, the same
  explicit `elided` count — wrapped as `{v, runId, startedAt, closedAt, archivedAt, history}`. The
  live path and the archived path are the same object, produced by the same function, so there is
  no second reducer to keep honest and no second cap to keep in step. A log with a hole says so
  after retention exactly as it said so before.

- **It is written by a sweep in the orchestrator, after the run closes — never from inside the
  caller's workflow.** The sweep lists the runs Temporal can still see, keeps the closed ones,
  skips those already archived, reads each one's history on the same raw-service path the console
  uses, and puts the object. It runs **in the `orchestrator-api` process**, on an interval
  (`KONTRA_HISTORY_ARCHIVE_MS`, default 15 minutes; `KONTRA_HISTORY_ARCHIVE=off` disables it).

  Why the API process and not the materializer, which is the established non-API worker role: the
  API process already holds both authorities this needs — the memoized Temporal client, which is
  also what registers the Search Attributes the discovery query depends on, and the `ObjectStore` —
  and it is the process that serves the archive back, so the thing that writes the evidence and the
  thing that reads it live or die together. The materializer is deliberately the opposite of that:
  activities only, no workflows, one slot, a hard `mem_limit`, and *relocatable* — the deployment
  docs say it should not run on the controller at all. A background loop that must run inside the
  retention window has no business living in the one process the operator is expected to move to
  another host, where its silence would be indistinguishable from having nothing to archive.

- **The archive is kept, not expired.** It is payload-free and capped by construction (measured
  above: ~175 KB worst case per run), so there is no storage argument for dropping it, and a
  **Dataset** outlives its **Run** — its story should too. `sweepUnits`, the only retention sweep
  in this repo, lists `units/` and nothing else, so `history/` is outside it already; this ADR is
  what makes that silence deliberate rather than accidental.

- **The history route falls back to the archive on NOT FOUND and on nothing else.** `GET
  /api/runs/:runId/history` serves the live history when Temporal has one, the archive when
  Temporal answers gRPC code 5, and a 502 when Temporal is unwell — a cluster outage must never
  quietly serve a stale log as if it were live. The response carries `archived: true` and
  `archivedAt`, and the console labels it.

- **An execution-pinned request is never served from the archive.** `?exec=` exists because a
  workflow id is reusable and asking by id alone answers with the latest execution; the archive
  records no execution id, so it cannot honour that pin. It answers 404 rather than a plausible
  wrong one — the same rule `history.ts` already applies to a link that points into another
  namespace.

- **Scope: the run's own log. Children are not archived.** A run's `kontra-fleet/dns` child and its
  twelve dispatch backing workflows are separate executions with their own retention, and the
  console already says so plainly at the point a drill fails ("a child is dropped for retention on
  its own schedule, independently of the run that started it"). The key layout above accommodates
  them — a child is just another workflow id with its own `dt=` — so this is a bounded extension,
  not a rewrite, and it is deliberately not taken here: archiving every workflow a log links to
  turns one put per run into up to a thousand history reads per run, and that cost wants its own
  measurement rather than a guess.

## Alternatives considered

- **A Dataset like any other**, so the log is queryable in the same surface as run output. Elegant
  and self-hosting, and rejected on 0023 §1 as above: a Dataset is caller-published. There is a
  second, mechanical reason worth recording — a DuckLake table is created from its first batch's
  schema, so the reduced log's shape (a nested `link`, an optional `execId`) would either flatten
  into a wide row or become a JSON column, and any later field on `RunEvent` becomes a migration of
  a table nobody queries with SQL. The log is read one run at a time, by id, by a browser. That is
  a GET, not a query.

- **Temporal's own history archival** (`HistoryArchivalState`, a blobstore URI per namespace). It
  is *measured off* on this cluster, and `start-dev` does not configure a provider — but the
  disqualifier is shape, not configuration. Archival stores the **raw** history, including every
  claim-checked payload ref, and reading it back goes through the data converter — which on this
  deployment is the claim-check codec, so rendering one run's log would fan out into thousands of
  blob GETs. That is precisely the fan-out `history.ts` and the raw-service read exist to avoid.
  It would also put retrieval on a separate API with its own semantics, to obtain something the
  console would then have to reduce anyway.

- **Writing the archive incrementally, from inside the caller's workflow**, on the theory that a
  run that dies never reaches a clean close. This is what the issue expected to be necessary, and
  finding (3) is what retires it: a dying run *is* a closed run, visible to anything watching for
  closed runs. It would also have been the wrong thing on its own terms — an archive step inside
  the caller's workflow appends *system* events to the very history it is trying to preserve, needs
  an activity (and a task queue, and a failure policy) in every caller in every language, and makes
  the archive a thing a caller can forget to do.

- **A Temporal Schedule driving an archive workflow.** Durable, overlap-guarded, and the natural
  home for a periodic job in a system that already runs Temporal. Rejected for now because it needs
  a workflow registered on a worker the API process does not host, and because the guard is worth
  less than it looks: an overlap policy only skips if the scheduled workflow is still running, and
  this sweep is idempotent by key anyway — two sweepers write the same bytes to the same key. Worth
  revisiting the day the orchestrator hosts its own worker for anything else.

- **Accept the loss and say so in the UI.** Honest, and it is still what the console does for the
  runs that closed before this existed and for every child. As the whole answer it fails the PRD's
  definition of done, which asks what the run's story was *after* retention drops the execution.

## Consequences

- **The archive covers exactly the runs the console can list, and no others.** Discovery is
  `listRuns()`, so whatever that finds is what gets archived.

  > **UPDATED 2026-08-20 — the gap this predicted has closed.** As written, `listRuns()` found runs
  > by the dispatches they made (`KontraRunId` on the handler's backing workflows), so a caller's
  > workflow that dispatched nothing was not discoverable as a run and was therefore not archived
  > either. This paragraph called that "inherited, not introduced", and said it closes when
  > discovery does. Discovery now works by subtraction — every execution that is not one of kontra's
  > own workflow types is a run — so a zero-dispatch run is listed from its first event and is
  > archived on the same schedule as any other. Verified against the live controller with `ping`,
  > whose entire purpose is to dispatch nothing: it appears in `/api/runs` with `dispatches: 0`.
  >
  > The coupling stated here is unchanged and is why this note belongs in the ADR rather than only
  > in a commit: the archive still covers exactly what the console lists, so a future narrowing of
  > discovery silently narrows the archive too.

- **The sweep must run inside the retention window, and its own discovery ages out with it.** The
  backing workflows discovery reads are closed executions too, so they are dropped on the same
  schedule as the run. At 24 hours and a 15-minute interval there are ~96 chances to archive any
  given run; an orchestrator that is down for a day loses the runs that closed while it was down,
  and nothing else. That is a real hole, it is bounded, and it is the price of an external sweep.

- **A run archived once is never re-read.** The sweep skips any run whose object already exists, so
  the steady-state cost is one visibility query plus a HEAD per closed run per interval, not a
  history read. The first pass after this ships is the expensive one. MEASURED against the live
  controller: the first pass reported `{scanned: 3, closed: 3, archived: 3, present: 0, gone: 0,
  failed: 0}` and wrote the three keys quoted above; the second reported
  `{archived: 0, present: 3}` and read no history at all.

- **`history/` is a new top-level prefix in the bucket**, beside `cas/`, `units/`, `datasets/`,
  `output/` and `standalone/`. Nothing deletes it. An operator who wants it gone deletes the prefix
  by hand, deliberately, which is the correct amount of friction for the only surviving account of
  what a run did.

- **The console gains a state it did not have: a log that is real but not live.** An archived log
  must not offer to follow — a follow cannot resume on a history that will never grow — so `follow`
  is disabled and labelled where it is shown. And the case that this ADR cannot fix is now stated
  rather than rendered as silence: a run whose execution is gone *and* which has no archive says
  so, instead of sitting on "reading the history…" forever, which is what it did before.

- **`archived` is now part of the history contract** in three places with no shared code:
  `RunHistory` in `control/orchestrator/src/history.ts`, the same interface in
  `frontend/src/run/api.ts`, and the label in the console's `EventLog`. It is optional
  everywhere and absent means live, so a server older than this ADR reads as live rather than as
  broken — which is the correct default, since it *is* live.
