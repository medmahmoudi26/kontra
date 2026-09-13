# 46. The meter is a second listing, not a wider archive

Date: 2026-09-13

## Status

Accepted.

Follows an event-log audit (`docs/event-log-audit.md`), whose findings F1, F2 and F4 this decides.
Amends nothing in [ADR 0025](0025-the-reduced-log-is-the-durable-record.md) — it says what that
archive is **for**, which 0025 never had to.

## Context

kontra may bill by Temporal event. Before any of that is built, an audit asked whether the event log
could carry a bill. It cannot, today, and the reasons are measured rather than argued:

**One Method call writes 34 events and the durable record sees 9.** The backing
`kontra.v1.ActorService.Run` is 25 of them, and `sweepClosedRuns` lists over `listRuns()`, which
subtracts `KONTRA_INTERNAL_WORKFLOW_TYPES`. That subtraction is **correct for the Runs page** — the
page is about a user's runs and a wall of `wardenWorkflow` rows would bury them — and it is the
reason the meter is blind to roughly three quarters of what kontra writes.

**The two perpetual workflows are excluded twice over**: by type, and again by `isArchivable`, which
requires a closed execution. `wardenWorkflow` and `leaseWorkflow` never close. The workflows that
burn events around the clock are precisely the ones the record can never see.

**And they are on a clock.** Nothing in kontra calls continue-as-new, in any of the three SDKs, and
nothing reads `GetContinueAsNewSuggested()`. Temporal terminates at 51,200 events: a held lease at
12.5 events/h reaches it in 170 days, a dark Warden at 5 events/h in 426.

**Fixing that makes the record worse.** `historyArchive.ts` reads by workflow id with no `execId`,
and `fetchRunHistory` documents what that means — *"answers with whichever ran last"*. The day
continue-as-new lands, every leg but the final one goes unarchived.

### What was unknown, and is now measured

The audit left attribution open. Checked on the live cluster, 2026-09-13:

| Execution | `Kontra*` search attributes |
|---|---|
| `kontra.v1.ActorService.Run` | `KontraActor`, `KontraRunId` |
| `wardenWorkflow` (×6) | **none** |
| `BeaconCall` (a caller) | **none** |
| `sweepDatasetsWorkflow` | **none** |

`KontraTenant` **is registered on the namespace and is populated on nothing.** So the backing
workflow can be attributed today and a perpetual one cannot — which decides more of this than any
argument about units.

## Decision

**1. The bill is counted from `historyLength`, never from fetched events.** Done
([F3](../event-log-audit.md)): `DescribeWorkflowExecution` answers it in one RPC without reading an
event. The reduced log's display caps — `HISTORY_MAX_PAGES`, `EVENT_CAP` — stay exactly as they are,
because they are about a screen and not about a total.

**2. The meter is a SECOND listing, and `KONTRA_INTERNAL_WORKFLOW_TYPES` does not move.** A metering
pass lists only those types and records `historyLength` per execution, including open ones. The Runs
page keeps the exclusion it is right to have.

*The alternative was widening the archive, and it is worse in both directions.* A Runs page that
listed every Warden is a page nobody reads; an archive that stored full histories for perpetual
workflows stores an unbounded, ever-growing object for a workflow that never closes. The two readers
want opposite things from one query, which is the whole reason to stop sharing it.

**3. The unit is the EVENT, and the bill records `historySizeBytes` beside it.** Temporal Cloud
meters *actions*, which is a different and partly-private derivation; kontra cannot compute an
action count from a history it can read, and a unit whose definition lives in another vendor's
pricing page is not one to build a meter on. Events are countable, checkable by the customer
against their own `temporal workflow describe`, and already the thing every number in the audit is
expressed in. **Bytes ride along because they are free from the same call** and because the first
question a large customer asks is which of the two they are being charged for.

**4. Attribution is `KontraRunId` where a run exists, and `KontraTenant` everywhere else — which
means `KontraTenant` must first be populated.** It is registered and unset today, so that is a
prerequisite and not a detail. Until it is set, a perpetual workflow's events are attributable to
the **installation** and to nothing finer, and a bill must say so rather than silently attributing
them to whoever happens to be running.

**5. Continue-as-new and chain-aware metering land in ONE change.** F2's remedy breaks F1 further
(F4). Sweeping by `firstExecutionRunId`, or metering per execution rather than per workflow id,
ships with the handover or the meter under-reports the long workflows by however many legs they ran.

## Consequences

**The archive stops being the only durable record of what was written, and stays the only durable
record of what HAPPENED.** ADR 0025's reduced log is a story for a human — turns, drills, an account
of a run. The meter is a column of integers. Conflating them is what produced a record that could
see 26% of the events; separating them is this decision.

**A metering pass costs a listing plus one `describe` per execution**, both bounded and neither
growing with history. It reads nothing a payload codec would have to rehydrate.

**Two perpetual workflows must learn to hand over**, and that is a workflow-code change gated by
`workflow.GetVersion` — an execution that is already running cannot change shape. Both carry trivial
state (a decision and two counters in the Warden; the lease map in the Lease), which is why they were
described as "written for it" rather than needing redesign.

**`KontraTenant` becomes load-bearing**, so the change that populates it is a prerequisite for
billing anything perpetual, and it should be asserted by a test rather than observed once.

**What this does NOT decide**: pricing, whether a wedged execution is billable (it writes no events,
so under this decision it costs nothing — see F7), and whether the meter reads visibility or
describes per execution. That last one is a cost-and-freshness trade to measure, not to argue.

## The alternative that was rejected, and why

**Widening `KONTRA_INTERNAL_WORKFLOW_TYPES`** — deleting the exclusion so one listing serves both.
It is one line and it is wrong in the direction that matters: the comment on that constant reasons
explicitly about the failure direction for a **Runs page**, where under-reporting internal noise is
correct. A bill wants the opposite. One listing cannot have both failure directions, and the one
that would have been silently inherited is the one that loses money.
