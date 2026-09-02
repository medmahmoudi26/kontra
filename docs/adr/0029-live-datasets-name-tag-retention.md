# 29. A Dataset has a name and tags: derived identity, operator-owned retention

## Status

**Accepted.** Extends **0023** §11 — the `open`/`sealed`/`abandoned` lifecycle already says whether a
Dataset is still being written; this says what it is *called* and how long it survives. Follows
**0025**'s pattern for state that must outlive Temporal's retention, and obeys that ADR's standing
warning about the retention number itself. Applies **0017**'s rule — two authorities are never
merged into one field — to tags. Supersedes nothing.

Renumbered from 0028 on adoption: **0028** was taken by *emit decoupled from the Unit, the caller
redirects output*, which landed first. That ADR is not merely adjacent — it introduced the
**temporary Dataset**, which this one must not fight. A temporary Dataset is untagged by
construction and already has an explicit owner and an explicit deletion verb, so §3's sweeper is
the *second* thing that can remove a Dataset. The two must agree on which one owns a temp's
lifetime rather than both collecting it.

## Context

The Datasets surface shows a Dataset when it is finished. A run that streams rows for hours shows
nothing until it stops, and the run that most wants watching — the long one — is precisely the one
that publishes no named Dataset at all. Making that stream watchable needs two things the model does
not have: something to point at while it is still being written, and a reason for it to still be
there tomorrow.

Six things were established against the local controller before deciding anything. Temporal
**Server 1.31.0**, namespace `default`, 2026-08-19.

1. **`open` is already the live state.** `0023` §11 defines a Dataset as `open` "while its producer
   appends, and equally after a producer died mid-append". Nothing needs inventing for *whether* a
   Dataset is live; the gap is only that nothing names or streams it.

2. **Identity is the path, and a name→path registry was deliberately deleted.**
   `backend/src/data/datasets.ts` opens with it: `output/<actor>/version=<v>/dt=<dispatch>/`,
   and the previous scheme's `_kontra_dataset` registry — "whose only purpose was translating that
   hash back into a name" — is gone. Any naming design that reintroduces a lookup table walks that
   back.

3. **The datetime format already exists.** `parquet.ts:dtPartition(runStartedAt)` renders
   `2026-08-19T14-32-07` — UTC, ISO to the second, colons dashed for path safety. A second datetime
   format would be a second thing to get wrong.

4. **The ledger already holds both facts a run-grain name needs.**
   `materialization.ts` carries `runId` (:73) and `runStartedAt` (:96) on every record.

5. **Retention here is 24h, and no design may depend on that.** `Config.WorkflowExecutionRetentionTtl
   24h0m0s`. **0025** measured the same number and drew the rule this ADR inherits: it "is a namespace
   config, not a code constant. Nothing in this repo sets it; it is the dev server's default, and a
   production namespace would carry a different one." That the proposed untagged TTL is *also* 24h is
   a coincidence of the dev environment, and coupling them would make Dataset lifetime silently
   change with an ops setting nobody connected to storage.

6. **A search attribute cannot be written after the execution closes.** `UpsertSearchAttributes` is a
   workflow-internal command. The CLI confirms there is no outside path:
   `temporal workflow update-options` covers Worker Deployment/versioning override only, and
   `temporal batch` drives cancel/terminate/signal/reset. The namespace already aliases
   `Keyword01:KontraGraph Keyword02:KontraActor Keyword03:KontraTenant Keyword10:KontraRunId`, so the
   mechanism exists — it is the *window* that is closed, not the feature.

## Decision

### 1. A Dataset has a name and a set of tags

Name is one string. Tags are a **set**, mutated by add/remove, never assigned as a scalar — two
writers exist (below) and concurrent adds must converge instead of racing to overwrite.

### 2. The default name is DERIVED, never stored

```
wf-<workflow>-<version>--<dtPartition(runStartedAt)>Z--<runFragment(runId)>
wf-nscheck-0.1.0--2026-08-19T14-32-07Z--2faa3d
```

Every component is already on the materialization record or the workflow manifest, so the default
costs no storage and needs no index — it is a *rendering* of facts the ledger holds, which is what
keeps finding 2 intact. Three constraints on it:

- **Derived from the manifest's name and version, not the task queue string.** They are spelled the
  same (`workflowQueue()` in `cli/workflow.go` and `backend/src/workflowControl.ts`), but
  `--queue` can be overridden at start: the queue is transport, the manifest is identity.
- **UTC, via `dtPartition`.** Two controllers exist (sfo3, nyc1); a local-time name would denote two
  different instants depending on which box wrote it. The UI renders local.
- **The run id fragment is in the name, not only in metadata.** It disambiguates two runs in the same
  second, and — the reason that matters — it keeps a Dataset traceable to its run *after Temporal has
  deleted the execution*, which for any kept Dataset is the normal case rather than the edge one.

  **CORRECTED ON IMPLEMENTATION.** This was first written as `runId[:6]`, and that spelling does not
  work against the run ids this repo mints. Both start paths produce `<type>-<unixseconds>`
  (`workflowControl.ts` behind `POST /api/runs`, and `cli/workflow.go`), and the ledger's `runId` IS
  that workflow id — so the first six characters are the workflow *name's* prefix and carry no run
  entropy whatever: every nscheck run in history renders `--nschec`. Measured, not argued: two
  nscheck runs one second apart produced the same fragment, and the unit test that claimed otherwise
  passed only because it hand-fed UUID-shaped ids nothing in the system produces. The fragment is
  therefore the first six hex of a **digest** of the full run id, which disambiguates under every id
  scheme including ids that differ only in a trailing character. A digest is not reversible and does
  not need to be — the full `runId` is stamped on the listing row beside the name, and is the key a
  tag or rename addresses, so traceability after Temporal deletes the execution rides that field
  rather than the fragment. The rationale above stands; only the spelling changed.

**CORRECTED ON IMPLEMENTATION: the workflow half is NOT already on the ledger, so it is snapshotted
at start.** "Every component is already on the materialization record or the workflow manifest, so
the default costs no storage" was wrong about the first component, and the error was not cosmetic —
it produced a name that denoted the wrong thing. A materialization record carries the **Actor**'s
name and version, not the caller's; Temporal knows the workflow *type* and forgets it after
retention; and the run id is `<type>-<unixseconds>`, a lowercased class name with no version that an
`--id` can replace outright. The first implementation therefore passed the producing Actor's identity
into the renderer, and a workflow `foo` dispatching an actor `bar` named its output `wf-bar-…` while
a single Run writing two actor tables got **two** names — contradicting the Consequence below, which
is the whole reason the name is run-grain.

The fix is this ADR's own inheritance from **0025**: what must outlive Temporal is *snapshotted into
a record* at the moment it is known. Both start paths write the caller's manifest `name`/`version`
against the `runId` — `workflowControl.startRun()` behind `POST /api/runs`, and `cli/workflow.go`
through `PUT /api/runs/:runId/workflow` — into `data/runWorkflows.ts`. Three things keep finding 2
intact, and they are the test of whether this is a registry:

- **Nothing is keyed by a name and nothing looks a name up.** The deleted `_kontra_dataset` table
  mapped a *name to a path*; this maps a *Run to the identity it ran under*. It is provenance, of the
  same kind as the tags §4 already stores against `runId`.
- **The name is still a rendering.** No name is stored, no index is added beyond the row's own
  primary key, and a rename is still the only stored *name*.
- **Deleting every row loses nothing that cannot be re-derived.** An unrecorded Run FALLS BACK to the
  producing Actor's identity — exactly what shipped first — so every Dataset written before this
  record existed still renders a name. The fallback is permanent and documented, not a migration
  window: it is actor-grain, and that is visible for what it is.

So the default now costs **one small row per Run**, not zero. That is the honest price of §2 meaning
what it says.

**CORRECTED ON IMPLEMENTATION (2): finding 4 is false for every Run this repo now starts, so the
Run is resolved from THREE authorities, not one.** "The ledger already holds both facts a run-grain
name needs" was measured against the graph interpreter's dispatches. That path is gone. The actorkit
path writes lake rows through `publishBatch` and **no materialization record at all** — measured on
the local controller, `/api/datasets/runs` returns 50 dispatches and not one of them is a Run the
actorkit path started, while `lame` holds five such Runs' rows. The visible consequence was the
report that opened this correction: a completed `nscheck` run left `lame_demo` (430 rows) and
`tmp_4e9b1b23` (623 rows) listed with **no run and no name**, one of them a temporary Dataset whose
owning Run was recorded on it the whole time.

`withDatasetNames` therefore resolves the Run from whichever authority knows it, in this order —
and they are not interchangeable, each knowing something the others structurally cannot:

1. **The ledger's DispatchRef**, matched on `(actor, version, dt)`. The only one that names an
   Actor's output table, and the only one that exists for a Dataset written before `run_id` was
   stamped on rows. Unchanged.
2. **The temporary Dataset's owner marker.** The only one that answers *before any row lands*, so
   an empty temp still says whose it is — and an ownership fact rather than a fact about rows.
3. **The lake's own per-file `run_id` statistics**, and only when they name exactly one Run. This
   is not a new store and not a scan: DuckLake keeps `min_value`/`max_value` per data file per
   column, so the set rides the metadata query that already counts rows and bytes. It is the same
   `run_id` a promoted row *keeps* rather than has re-stamped, which is what makes a promoted
   Dataset attributable at all.

This does not walk back finding 2. Nothing is keyed by a name, nothing is looked up to FIND a
Dataset, and no name is stored — the third authority reads the ROWS' own provenance, which is the
most direct possible evidence of which Run wrote them.

**The name's datetime now comes from the row's own `dt` partition, for all three.** A `dt=` value
IS `dtPartition(runStartedAt)` by construction (`writeDatasetParquet`), so feeding it back through
`parseDtPartition` renders the byte-identical string the ledger path rendered, and authorities 2
and 3 — which have no run-start timestamp — need no second source. One rule instead of a ledger
branch and a lake branch. `parseDtPartition` lives beside `dtPartition` and is pinned to round-trip
with it, because an inverse spelled elsewhere is the "second datetime format" this section warns
about.

**And the singular is refused where it would be a lie.** A durable Dataset accumulates from many
Runs, so the listing row carries the SET (`contributingRuns`) and fills the singular `runId` — the
key a tag or rename addresses — only when that set has exactly one member. A partition two Runs
share is named by neither. That is **0017**'s rule applied to a count rather than to a status: the
plural fact and the singular one cannot contradict each other, because the singular exists only
where a singular answer does.

Stored, all of it keyed by `runId`: the *deviation* — an explicit rename, and the tags (§4) — and
the *provenance* the derivation cannot reconstruct, which is the caller workflow's manifest identity.

### 3. Retention is kontra's own policy, and its clock starts at last write

- **Untagged: a TTL constant owned by this repo, default 24h.** Deliberately *not* read from the
  namespace config (finding 5).
- **Tagged: kept.**
- **The TTL runs from last write, not creation.** A run that appends for thirty hours would otherwise
  have its earliest chunks collected while it is still writing to them.
- **Collection is a sweep with a grace window, not a delete at exactly T+TTL.** Tagging is frequently
  retroactive — an operator tags at hour 23 because that is when the run turned out to be
  interesting — so the sweep must not race the tag.

**CLARIFIED ON IMPLEMENTATION: what a tag does to a TEMPORARY Dataset.** "Tagged: kept" is written
for durable output, and the Datasets page offers the same affordance on a temp — correctly, because
§4's record is keyed by `runId` and a temp has exactly one, its owner. The two rules meet without
contradicting, and the resolution is stated here because it is not derivable from either alone:

- **A tag does not extend a temp's lifetime, because a temp has no clock to extend.** The sweep keeps
  every temporary Dataset whatever its age or tag (`classifySweep`, `kept-temporary`), so tagged and
  untagged temps have exactly the same disposition. The reported reason differs; the outcome does not.
- **A tag does not make a temp durable, and must not.** Temp-ness is the owner marker's answer and
  nothing in the record store touches it, so the badge, the DELETE route and the sweep keep reading
  one authority. A tag that flipped the marker would make the temp undeletable by the only verb that
  deletes it (ADR 0028) and hand it to the clock instead — the opposite of what the operator asked for.
- **A tag does not block `deleteTemporaryDataset`.** The explicit verb still removes a tagged temp: a
  tag marks, it does not lock, and the orphan policy stays explicit-only.
- **What a tag on a temp DOES keep is the same Run's DURABLE output**, because the record is Run-grain
  — the grain §2's name already has. Tagging the staging table of a Run that promoted into `lame`
  keeps `lame`'s rows past the TTL, which is the sensible reading of "keep this": the staging table
  goes when triage is done, the answer stays.
- **Therefore the delete confirmation NAMES the tags.** A page that draws a tag as "kept" beside a
  button that removes it regardless is the two-deleters-disagreeing shape §5 exists to prevent — so
  the disagreement is spent on the operator's screen at the moment of the decision, not on their data.

### 4. The Dataset record owns tags. `KontraTag` is a projection

Both write paths are supported, and they are different intents, not redundancy:

| path | who | when | means |
|---|---|---|---|
| publish parameter | the author | in-workflow | *this kind of run always matters* — policy |
| control-plane mutation | the operator | any time after | *this run turned out to matter* — judgment |

Only the first can reach Temporal, and only while the execution is live. So:

> The record is authoritative. The search attribute is a fast query path over the live window and is
> **never read as truth**.

In-workflow tagging is therefore a parameter on publication — `publish(rows, name=…, tag=…)` — which
writes the record and *mirrors* to `KontraTag`. It is not an `UpsertSearchAttributes` call that
happens to also be a tag; that ordering would leave the record, which is what the sweeper reads,
saying untagged.

**The two will legitimately disagree, permanently.** Tag a Dataset three hours after its run closed
and the search attribute says untagged forever, because nothing can write it. That is a frozen index,
not drift — recorded here so that nobody later builds a reconciliation job that cannot work.

### 5. The sweeper reads the record, never the search attribute

A collector querying Temporal for `KontraTag IS NULL` would delete every Dataset tagged after its run
closed — which is most kept Datasets, since that is what the operator path is *for*. This is the
data-loss bug a literal reading of "Temporal is the source of truth" produces, and it is the specific
reason §4 fixes ownership rather than leaving it to convention.

If the sweep is a Temporal Schedule, it must **await** the work: `SKIP` overlap is a no-op against a
workflow that returns immediately, which has already bitten the dispatcher in this repo. One
heartbeating activity.

## Consequences

- **Temporal stays authoritative for the live window, and only for it.** Execution status, timings,
  parameters and `KontraRunId` are Temporal's for 24h. Everything expected to outlive that is
  snapshotted into a record keyed by `runId`, following **0025** — the caller's workflow identity at
  START (§2's correction), the tag at publication or whenever an operator says so (§4). The *name*
  is the exception and stays a rendering of those records; it is never stored unless an operator
  renames it.
- **A run-grain name over actor-grain storage.** Physical identity stays `output/<actor>/version=…/
  dt=…`; the name labels the run's output, which may span several actor tables. It is a label, not a
  new physical layout, so the `run=`-first blob layout and its measured 50× LIST win are untouched.
- **Bucket-lifecycle GC is rejected.** Expiring by prefix would need tagging to move objects to a
  permanent prefix, which fights that same layout. The sweeper costs code; the prefix scheme costs
  the read path.
- **`ExecutionStatus` and Dataset lifetime remain separate questions**, per **0017**. A completed run
  can own a Dataset that is still `open`, and a killed run can own one that is kept.

## Open

- **Grain of a rename.** A run producing several actor tables has one name and several paths; whether
  an operator may rename one table independently is undecided. Defaulting to no.
- **Cross-controller collision.** Names are per-controller and sfo3/nyc1 can mint the same string.
  Out of scope while the streams stay per-control-plane.
