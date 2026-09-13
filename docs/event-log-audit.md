# Event-log audit — what kontra writes, and what a meter would see

*Measured 13 September 2026 against Temporal Server 1.31.0 on the live cluster (namespaces `default`
and `streamlab`), plus two probe workflows built on the Go SDK version this repo pins. Every number
here was observed, not estimated.*

**This document exists because the findings had nowhere durable to live.** They were produced as a
private artifact (readable only by one account) and handed over in `/tmp`. The issues they became are
under `.scratch/event-meter/`, which is gitignored — so without this file the measurements would be
re-derived by whoever picked the work up, which is hours of live-cluster probing to learn what is
written below.

**Do not re-derive these numbers. Do re-measure them** if the SDK version, the server version or the
batch defaults move: the reproduction steps are at the bottom.

## Verdict

The event log itself is in unusually good shape — this codebase reasons about history cost more
carefully than most, and the per-unit economics are fine (~0.17 events/unit at the default batch size
of 200). The problem is not what kontra *writes*. It is **what gets counted** — the only durable
event record sees roughly a quarter of the events — and **two perpetual workflows that cannot run
forever**, because nothing anywhere calls continue-as-new and Temporal's 51,200-event ceiling is
therefore a death date.

The work these became is `.scratch/event-meter/` (five issues). **F1, F2 and F4 are one work item**,
not three: fixing F2 makes F1 worse, which is what F4 is.

## Scope covered

- `/root/oss/kontra` — `runtime/handler`, `sdk/python/kontra`, `sdk/go/catalog`, `sdk/go/hitl`,
  `sdk/go/narrate`, `cli/warden`, `control/orchestrator/src` (incl. `workflows/*.ts`),
  `shared/core/src/history.ts`
- `/root/oss/kontra-workflows` (the `hunt` workflow as the real-workload shape), `/root/oss/kontra-actors`
- Live cluster: Temporal Server 1.31.0 (CLI 1.7.1 at `/root/.temporalio/bin/temporal`), namespaces
  `default` and `streamlab`. Cluster health `SERVING`.

---

## The cost model (all measured, not estimated)

### What one unscoped Method dispatch writes

A dispatch spans **two** workflows: the caller's (which the archive records) and the backing
`kontra.v1.ActorService.Run` (which the archive is configured to exclude).

| # | Events | Workflow | Metered? |
|---|---|---|---|
| 1 | `NexusOperationScheduled`, `NexusOperationStarted`, `WorkflowTask` ×3 | caller | **yes** |
| 2 | `WorkflowExecutionStarted`, `WorkflowTask` ×3 | backing | no |
| 3 | `MarkerRecorded` (generated Nexus scaffolding's local activity) | backing | no |
| 4 | `UpsertWorkflowSearchAttributes` ×2 (kontra's, plus the SDK's `BuildIds`) | backing | no |
| 5 | `RunBatch` — `ActivityTask` ×3 + `WorkflowTask` ×3 | backing | no |
| 6 | `Close` — `ActivityTask` ×3 + `WorkflowTask` ×3 | backing | no |
| 7 | `StoreBlob` — `ActivityTask` ×3 + `WorkflowTask` ×3 | backing | no |
| 8 | `WorkflowExecutionCompleted` | backing | no |
| 9 | `NexusOperationCompleted`, `WorkflowTask` ×3 | caller | **yes** |

**9 caller events + 25 backing events = 34 per Method call. ~26% visible to the meter.**

### Per-construct reference

| Construct | Events | Charged in | Metering status |
|---|---|---|---|
| Nexus dispatch (caller side) | 9 | caller workflow | metered |
| Backing `RunWorkflow` | 25 | `kontra.v1.ActorService.Run` | excluded by type |
| Activity round trip | 6 | whichever workflow schedules it | — |
| Watch that timed out (dark Machine) | 5 | `wardenWorkflow` | never archived — never closes |
| Narration (`speak` / `say`) | 5 | caller workflow | metered, capped at 200 sentences |
| Dataset page | 6 | caller workflow | metered |
| **One Method call, end to end** | **34** | split across two workflows | **26% visible** |

A 200-unit batch therefore costs ~0.17 events per unit. **The batching decision in ADR 0023 is doing
its job; per-unit cost is not the problem.** Which events get counted is.

### Server limits (1.31.0 defaults)

Read from `go.temporal.io/server@v1.32.0-162.1/common/dynamicconfig/constants.go`:

| Threshold | Events | Bytes |
|---|---|---|
| `limit.historyCount.suggestContinueAsNew` | 4,096 | 4 MB |
| `limit.historyCount.warn` | 10,240 | 10 MB |
| `limit.historyCount.error` (server terminates) | 51,200 | 50 MB |

### Horizons for the two perpetual workflows

| Workflow | Condition | Events/h | Events/mo | Suggest (4,096) | Warn (10,240) | **Terminate (51,200)** |
|---|---|---|---|---|---|---|
| `leaseWorkflow` | Fleet held, default 1 h TTL | 12.5 | 9,131 | 13.7 d | 34.1 d | **170.7 d (5.6 mo)** |
| `wardenWorkflow` | Machine dark, 1 h backoff ceiling | 5.0 | 3,652 | 34.1 d | 85.3 d | **426.7 d (14.0 mo)** |

The lease workflow's own header states its horizon and then declines the remedy —
`control/orchestrator/src/workflows/lease.ts:55`:

> "At the default one-hour TTL a Fleet held for a week costs ~2,100 events, so Temporal's 51,200
> history limit is roughly five months of continuous holding — which is why there is no
> continue-as-new here and why a ticking design would have needed one."

That reasoning holds for the *rate* and not for the *total*. The backoff made the bleed slow; it did
not make it stop.

---

## Findings

### F1 — The only durable event record excludes most of the events
**Critical · billing integrity · measured**
`control/orchestrator/src/visibility.ts:95` · `historyArchive.ts:281` · `temporalClient.ts:196`

Retention on `default` is 24 hours (`Config.WorkflowExecutionRetentionTtl 24h0m0s`, verified live),
so the ADR 0025 reduced log is the only record of an event that outlives a day. It is written by
`sweepClosedRuns` over `listRuns()`, and that listing subtracts `KONTRA_INTERNAL_WORKFLOW_TYPES`:

```
kontra.v1.ActorService.Run   ← one per Method call, 25 events each
stackWorkflow
tmuxSessionWorkflow
sweepDatasetsWorkflow
wardenWorkflow               ← one per Machine, never closes
```

That is the right call for a Runs page and the wrong one for a meter. The backing workflow is 25 of
the 34 events a Method call writes. The warden and lease workflows are excluded **twice over**: by
type, and again by `isArchivable`, which requires `closedAt > 0 && status !== 'running'` — and those
two never close. The workflows that burn events around the clock are precisely the ones the meter
can never see.

### F2 — No workflow anywhere calls continue-as-new
**Critical · availability · measured**
repo-wide, Go + Python + TypeScript

Zero hits outside generated protobuf scaffolding
(`runtime/handler/_gen/kontra/v1/actor_service_temporal.pb.go:500-502`, which nothing invokes).
Nothing reads `GetContinueAsNewSuggested()`, `is_continue_as_new_suggested`, or any history-length
accessor either. The server sets the suggest flag at 4,096 events on **every workflow task** — a held
lease trips it at 13.7 days and a dark Warden at 34 days — and the signal is discarded in all three
SDKs.

The ceiling also bounds ordinary Runs. At roughly 21 events per 200-row page, a single sweep tops out
near **490,000 units** before the server terminates it mid-run, with no leg to resume from. ADR 0023
records the same wall from the other side: `batchSize:1` measured out at ~5,100 units.

### F3 — The archive's event counter silently caps at 20,000
**High · billing integrity · measured**
`control/orchestrator/src/temporalClient.ts:421` — `HISTORY_PAGE = 1000`, `HISTORY_MAX_PAGES = 20`

`fetchRunHistory` stops after 20 pages and sets `truncated`; `mapHistory` then reports
`scanned = raw.length`, which is now the cap rather than the count. **Any run between 20,001 and the
51,200 ceiling is legal, completes normally, and is recorded as exactly 20,000 events — an undercount
of up to 61%.**

The elision itself is honest: `EVENT_CAP = 1000` / `HEAD_KEEP = 25` in `shared/core/src/history.ts:136,139`
keeps head and tail and writes `elided` beside them. It is the *total* that goes wrong, and a bill
built on `scanned` would inherit it. The authoritative count is free — `DescribeWorkflowExecution`
returns `historyLength` (and `historySizeBytes`) in one RPC without reading a single event.

### F4 — Fixing F2 would break the meter further
**High · coupling**
`historyArchive.ts:289` · `temporalClient.ts:443`

The sweep calls `read(run.runId)` with **no `execId`**, and `fetchRunHistory` documents what that
means: asking by workflow id alone "answers with whichever ran last". `isArchivable` also treats a
continued chain as `running` for its whole length.

So the day continue-as-new lands — the remedy for F2 — every leg but the final one goes unarchived
and unbilled. **These two findings have to be fixed in the same change, not in sequence.**

### F5 — Events can be lost with nothing but a counter to show for it
**Medium · billing integrity**
`historyArchive.ts:288` (`out.gone`) · `historyArchive.ts:48,322` (15-min interval)

A run whose history retention drops between the listing and the read is tallied as `gone` and
skipped — counted, but with no record of which run or how many events went with it. The sweep runs on
one 15-minute timer (`DEFAULT_INTERVAL_MS = 15 * 60_000`) inside `orchestrator-api`, is a no-op when
the object store is unconfigured or `KONTRA_HISTORY_ARCHIVE=off`, and the listing is capped at
`LIST_LIMIT = 200` (`temporalClient.ts:51`). Any window longer than the 24-hour retention loses those
events permanently and silently.

### F6 — A marker event per dispatch that buys nothing
**Low · efficiency · measured**
`sdk/go/catalog/workflows.go:485`

The Go SDK wraps `workflow.Now(ctx)` in `workflow.SideEffect` to mint a node-id suffix, costing a
`MarkerRecorded` event on every dispatch. `workflow.Now` is already replay-deterministic, so the
marker records a value that needed no recording — and it does not solve the collision the wrapper
looks like it is guarding, since every call inside one workflow task returns the same instant either
way. Python's equivalent path uses `workflow.uuid4()` and pays nothing. A workflow-scoped counter
would be free and deterministic.

Two smaller ones alongside it:
- A Nexus dispatch costs **9** events against an activity's **6**, because `NexusOperationStarted`
  wakes a workflow task of its own.
- `Fleet.ready()` (`sdk/python/kontra/fleet.py:1194`, defaults `timeout=10min, poll=10s`) polls every
  10 s for up to 10 minutes — ~11 events per iteration per placement while it waits.

### F7 — Every running workflow on the cluster is wedged
**Medium · health, not billing · measured**

All nine open executions end on a `WorkflowTaskScheduled` that nobody polls:

| Workflow id | Type | Events | Stuck since |
|---|---|---|---|
| `kontra-warden/wdn-live1788864503079175248` | `wardenWorkflow` | 16 | 2026-09-08 |
| `kontra-warden/wdn-live1788390160294355298` | `wardenWorkflow` | 26 | 2026-09-02 |
| `kontra-warden/wdn-live1788378598485797190` | `wardenWorkflow` | 21 | 2026-09-02 |
| `kontra-warden/wdn-live1788109167178399263` | `wardenWorkflow` | 17 | 2026-08-30 |
| `kontra-warden/wdn-live1788101939671959508` | `wardenWorkflow` | 16 | 2026-08-30 |
| `actor-beacon-beaconcall-1787924865-beacon-3800505a` | `kontra.v1.ActorService.Run` | 8 | 2026-08-28 |
| `beaconcall-1787924865` | `BeaconCall` | 9 | 2026-08-28 |
| `kontra-dataset-retention-workflow-2026-08-26T15:00:00Z` | `sweepDatasetsWorkflow` | 2 | 2026-08-26 (never ran a task) |
| `streamlab-idle-a52526ae` (ns `streamlab`) | `StreamWF` | 10 | 2026-08-06 |

The good news: a wedged workflow is **free**. The warden histories show a 14-day gap with no new
events, so the server does not re-time-out an unpolled task. But they never terminate, they are
excluded from the Runs page by type, and nothing reaps them — so the set only grows, and each one
resumes the moment a worker returns to its queue.

---

## Remediation, in order

**1. FIRST — Meter from `historyLength`, not from fetched events.**
Take the count from `DescribeWorkflowExecution` — one RPC, no pages, no payloads, authoritative, and
`historySizeBytes` comes with it. Retires F3 outright and decouples the bill from the reduced log's
display caps, which can then stay exactly as they are.

**2. SECOND — Count internal workflows, in a second listing.**
Leave `KONTRA_INTERNAL_WORKFLOW_TYPES` alone — it is right about the Runs page. Add a *metering* pass
that lists **only** those types and records `historyLength` per execution, **including open ones**.
Attribute through `KontraRunId` (the backing workflow already upserts it, gated behind
`workflow.GetVersion(ctx, "kontra-search-attributes", ...)`) and through `KontraTenant` for the
perpetual ones.

**3. THIRD — Read the flag the server is already sending.**
Have the two perpetual loops check `GetContinueAsNewSuggested()` at the top of each iteration and
hand over when it is set. Both are written for it — both carry trivial state (a `wardenDecision` plus
two counters in the Warden; the lease map in the Lease). **Do this together with the fix for F4:**
sweep the whole chain by `firstExecutionRunId`, or meter per execution rather than per workflow id,
so continuing does not silently drop legs.

**4. FOURTH — Name what the sweep loses.**
`out.gone` should carry the run ids, not just a count, and a pass that cannot reach the object store
should be loud. Consider a retention TTL longer than the sweep interval by a wide margin — at 24 hours
and 15 minutes the margin is thin for anything billable.

**5. THEN — Reap the wedged executions, and drop the redundant marker.**
Neither is urgent. F7 is a tidiness and capacity question rather than a cost one; F6 is one event per
dispatch, worth taking when that file is next open.

---

## What is already right (do not "fix" these)

Worth stating plainly, because it is unusual — this codebase reasons about history events more
carefully than most. The findings above are at the seams, not in the design.

- **Narration is hard-capped.** `MAX_SENTENCES = 200` at 5 events each (`sdk/python/kontra/narrate.py:152`)
  — a ceiling of ~1,000 events that refuses the sentence rather than killing the run, and says so in
  the transcript. The module header documents the two prior incidents it exists to prevent.
- **The Warden starts no timers.** Backoff is expressed as the next watch's `ScheduleToStartTimeout`,
  which costs nothing until it fires. `TestTheWatcherStartsNoTimer` reads the finished history back
  and fails on any timer event.
- **Heartbeats write nothing.** Liveness rides Temporal's mutable state, not history — asserted by
  watching a Machine beat for thirty seconds and finding zero new events.
- **The Lease is condition-blocked.** One timer in flight, re-armed only on change. Measured at
  0 events/hour while a Fleet is held and nothing happens to it, with a 200 ms-TTL control beside it
  in the same test.
- **Batches are refs, never rows.** ~110 bytes in history against a claim-checked payload plane
  (ADR 0007), so a 50 MB batch cannot be inlined by accident. The `isinstance` test at the dispatch
  seam is deliberate.
- **Failure counters bound the hot loops.** `wardenFailuresBeforeStopping = 5` stops a watch that
  fails instantly and forever instead of letting it spend six events a round trip into the ceiling.

---

## Open questions the audit did NOT resolve

1. **Per *event* or per *action*?** Temporal Cloud bills "actions", which is a different unit from
   history events. Which one the product intends to bill changes the whole model. Not decided.
2. **Attribution for perpetual workflows.** A Warden has no `KontraRunId`. `KontraTenant` exists as a
   custom search attribute alias on `default` (`Keyword03`) but **I did not verify it is actually
   populated** on warden or lease executions. Check before designing metering.
3. **Visibility vs. describe.** Whether the metering pass should read `ListWorkflowExecutions` or
   describe per execution — the former is cheaper, the latter is authoritative for open runs.
4. **Whether this needs an ADR.** It changes what ADR 0025's archive is *for* (a story for humans vs.
   a meter). The `KONTRA_INTERNAL_WORKFLOW_TYPES` comment explicitly reasons about the failure
   direction for a Runs page, not for a bill.

## One loose thread, unrelated to billing

`Batch.done` (`sdk/python/kontra/catalog.py:468`, populated in `Batch.from_ref` at :509 from the
handler's `ref.Meta["done"]`) is **read by nothing outside tests** — only `tests/test_workflows_client.py`
asserts on it. The handler sets `done=false` when a Method returned before covering its input, so the
signal exists and nobody resumes on it. Noticed in passing, not pursued, not in the report. Possible
latent resume bug; worth a look independently.

---

## Reproducing the measurements

Event counts came from real histories on the live cluster (`temporal workflow show`) and from two
probe workflows built against the Go SDK version this repo pins (1.47.0), each reading its own
history back and counting by event type: one reproducing the Warden's timeout loop, one reproducing
`RunWorkflow`'s command sequence. The probes ran in the `default` namespace as `evprobe-*` and aged
out with the 24-hour retention.

Server limits are the 1.31.0 defaults read from `common/dynamicconfig/constants.go`: suggest
continue-as-new at 4,096 events / 4 MB, warn at 10,240 / 10 MB, terminate at 51,200 / 50 MB.

The lease rate of 12.5 events/h is derived from the figure `workflows/lease.ts`'s own header states
(~2,100 events/week). The warden rate of 5 events/h was **measured**, and is one lower than the six
that file estimates — a watch nobody takes writes no `ActivityTaskStarted`.

**The house pattern for keeping these honest already exists**: `warden_workflow_live_test.go`,
`lease.test.ts` and `tests/test_narration_live.py` all assert event counts against a real history.
That pattern is the reason these numbers were knowable at all, and any change in this area should
add to it.
