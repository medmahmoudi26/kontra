# 60. The commit map travels in the heartbeat

Date: 2026-10-06

## Status

**Accepted, and both halves are landed** — the second by the amendment at the end (2026-10-10): a
retry now RESUMES from the heartbeat, a new execution of the same Batch resumes from a listing of its
commit objects, and the Redis commit map is gone. The amendment also records the owner's decisions
A7, A8 and A9 (`PLAN.md` §5) as built. Sequel to **0059**, which stopped the store losing the commit
map. This moves it somewhere the store cannot reach.

## Context

ADR 0059 fixed the eviction: Redis now refuses a write rather than deleting an actor's state hash.
That removes the silent corruption, and it does not make the commit map **durable**. The hash still
carries a 24 h TTL and still lives in a cache, so the record a retry resumes from is one restart or
one expiry away from gone.

What a beat already carried was four numbers:

```python
{"node": "n7", "done": 2, "total": 3, "isolated": 1}
```

**None of them says which.** `done: 2` cannot be resumed from — nothing in that payload distinguishes
"units 0 and 1 committed" from "units 0 and 2 committed", and the difference is which work a retry
re-runs. So the beat was a progress display and the resume was a separate lookup, in the cache.

Temporal already has the right place for this. Heartbeat details are part of the activity's own
history, handed to the next attempt, and they survive anything the state store does.

## Decision

**A checkpoint rides in the heartbeat, and it says which.**

```
{"v":1,"batch_id":"b1","done":[[0,1]],"failed":[4],"manifest_ref":""}
```

`shared/conformance/checkpoint.json` is the contract; `runtime/go/checkpoint`,
`runtime/python/internals/checkpoint.py` and `control/orchestrator/src/checkpoint.ts` are the three
implementations. The corpus came first, then all three — the ground rule for a rule with more than
one implementation.

### `done` is a range set, which is a size decision

Heartbeat details are a Temporal payload with a size limit. A per-unit list of 10,000 integers is a
batch-size ceiling wearing a different hat; one contiguous run is one pair. Canonical form — inclusive
`[lo, hi]`, sorted, merged so no two ranges touch — is part of the contract rather than an
optimisation, because two implementations that agree on membership and disagree on encoding produce
different bytes for the same facts. The `wire` section of the corpus pins exact bytes for that reason:
a `done` rendered as `[{"lo":0,"hi":1}]` answers every membership question correctly and is unreadable
to its peers.

### `batch_id` is a guard, not a label

Unit indices are positions **within one batch**. A checkpoint from a different batch applied by index
would skip units in a batch that never ran them — silent data loss, and the most likely way to get
this wrong. So a mismatch is discarded **whole**, and an empty `batch_id` matches nothing, including
another empty one: an unidentified checkpoint is not evidence about any particular batch.

The id is the batch's **content hash**, which the engines already compute (`BatchID` / `batch_id`), so
this needs no new identity.

### A version it does not know is refused, not partly read

A v2 that moved a field's meaning would have a v1 reader resume from something it half understood.
`null` means "start from the beginning", which is always safe because the work is idempotent by
position. A malformed range is an error rather than a zero range, because `[0,0]` claims unit 0
committed.

### Where the counters and the checkpoint disagree, the checkpoint wins

`heartbeatRow` prefers it. The counters are derived twice — once by the actor for display, once from
the set a retry resumes from — and the set is the thing the run will act on, so it is the number an
operator should read.

### Published, not passed through `SetHeartbeat`

The Go engine's `SetHeartbeat(func(done, total, isolated int))` is exported and has callers outside
this repository. Widening it would break them for a payload they do not send. Instead the runner
publishes to `KontraActor.publishCheckpoint` immediately before it beats, and the host reads
`a.Checkpoint()` inside the callback — current state, not a lagging copy. Python's `_checkpoint()`
does the same thing in the same place.

Both build the checkpoint **from** `slots`/`failSlots` rather than maintaining it beside them. Two
structures tracking one fact drift, and the drift would be a checkpoint that disagrees with the
commits it describes.

## Consequences

- **The commit map now exists in two places**, and that is the intended intermediate state: Redis
  still holds it (and still answers the resume), while the heartbeat carries an authoritative copy in
  the run's history. Progress reporting already reads the new one.
- **Isolated Units are counted as finished.** A node that isolated three and committed seven has
  accounted for ten; a denominator that counts only commits never reaches its total, which is how a
  healthy run reads as stuck forever (`accountedFor`).
- An older SDK beats without a checkpoint, and a row built from such a beat falls back to the
  counters. That is a normal actor, not a broken one.

### What this does NOT yet do, and the reason is concrete

**It does not remove Redis from the commit path.** `commit` stores each Unit's **output**, not merely
a done-marker:

```python
await self._set(slot, {"out": unit.out})
```

What that costs depends on whether an object store is configured, and both cases rule out putting it
in the heartbeat as it stands:

- **With `KONTRA_S3_ENDPOINT` set**, each record is written to the object store at push time and the
  commit holds only `{"$ref": {key, size, sha256}}` — "Redis never holds payloads"
  (`docs/wiki/Durability-and-Failures.md`). Small per Unit, but still **O(units)**, and a heartbeat
  payload has a limit. Inlining the refs would make that limit a batch-size ceiling, which is the
  exact mistake the range set exists to avoid for `done`.
- **Without one**, the commit holds the records themselves, and those have no bound at all.

So the refs need to be addressed by **one pointer** rather than carried — which is what
`manifest_ref` is for, and why it is in the contract before it is in use. Doing it the other way round
would mean a resume that knows which Units finished and cannot say what they produced.

Still open from this phase, in order: `manifest_ref` populated and read; `resume_from` driving the
engines' `todo` instead of the per-unit Redis read loop; then `batch-owner` and the statekv commit map
deleted. Each of those changes the path that runs user batches, which is why none of them is in this
commit. **All three are done — see the amendment below.**

## Tests

- `shared/conformance/checkpoint.json` — canonical encoding (every insertion order), membership at the
  edges, exact wire bytes, and the resume table. The `discarded` cases are asserted twice: once for
  the todo list, once for the property that makes them safe.
- Three arms: `runtime/go/checkpoint` (12), `runtime/python/internals/test_checkpoint_conformance.py`
  (10), `control/orchestrator/src/checkpoint.test.ts` (13).
- `runtime/go/engine/checkpoint_publish_test.go` — the publication seam, including twenty encodings of
  the same maps, because **Go randomises map iteration** and an unsorted `failed` would make two
  encodings of one checkpoint differ.
- `tests/test_actor_engine.py` — the beat's field set is asserted exactly (a misspelled field reports
  0/0 forever rather than erroring), now including `checkpoint`, plus that it names *which* units and
  that the batch id is stable across every beat of one batch.
- `control/orchestrator/src/heartbeat.test.ts` — the checkpoint beats the counters, a missing or
  unreadable one falls back rather than zeroing the row, and `total` still comes from the beat because
  a checkpoint does not carry one.

**A note on running these.** `go test` caches results and does not track files outside the module, so a
corpus edit does not invalidate the Go arm — it answers `ok (cached)` for a contract it did not
re-execute. Measured while writing this. `ci.yml` and the `Makefile` now pass `-count=1`, and
`shared/conformance/README.md` says why.

## Amendment (2026-10-10): resume reads the heartbeat and the store, and the commit map is gone

PRD D1 settles the owner: *execution, progress, resume — Temporal; the activity heartbeat carries the
checkpoint. Bytes — S3.* The owner's decisions on the plan settle the three questions D1 left open:

| # | Decision | As built here |
|---|---|---|
| **A7** | Keep cross-execution resume: a re-dispatch of the same Batch on the same instance resumes | A new execution LISTS the batch's commit objects and folds back what an earlier one finished. Both SDKs. |
| **A8** | S3 is mandatory; tests use an in-memory store | `serve()` refuses to start an actor that declares a Method without `KONTRA_S3_ENDPOINT`. Both SDKs. The resume tests run on an in-memory store; the store-less engine branch survives only as an in-process test seam (below). |
| **A9** | `unit_state` lives in S3 scratch objects, deleted on commit | **Not done here.** `unit_state` and the poison counter stay in the actor's Redis hash, where this change leaves them (below). |

So resume has two halves. **Within one activity execution** it is Temporal's: the previous attempt's
last heartbeat says which Units finished. **Across executions** it is the store's: Temporal never hands
heartbeat details to a new execution, so the batch's commit prefix is listed instead. Both fold the same
objects back the same way. This is what was built, in both SDKs, corpus first.

### A finished Unit is an object; `manifest_ref` names where they live

When a Unit commits or is isolated, the engine writes ONE object holding its outcome — its output
refs, or its error and category — **synchronously, before the beat that reports it**:

```
commits/run={run}/actor={actor}/actor_id={actor_id}/batch={batch_id}/unit={i:05d}.json
{"v":1,"batch_id":"…","unit":3,"out":[{"$ref":{…}}]}
```

The checkpoint's `manifest_ref` is that batch prefix (with `KONTRA_S3_PREFIX` in front, exactly as a
pushed record's key has it). So a heartbeat stays a range set and a pointer — no batch-size ceiling —
and a reader gets each finished Unit's outputs back without them ever riding the payload.
`shared/conformance/commit.json` pins the key, the body's field set, what a reader refuses, and what a
listing means; the arms are `runtime/python/internals/test_commit_conformance.py` and
`runtime/go/unitstore/commit_conformance_test.go`.

**`commits/` is its own top-level prefix, not a corner of `units/`.** Everything under `units/run=<id>/`
ending in `.json` is a ROW to the live row tail (`rowTail.ts` counts them) and to a DuckDB glob over the
run, so a commit object there would have inflated every run's count by its Unit count. `run=` still
leads, for the reason `blobkey.json` gives.

**`actor_id=` is the instance, and it is why A7 is a LIST.** It is the id both hosts already key the
live instance by: the idempotency key, else the Session, else run and node joined
(`runtime/handler/workflow.go`; a Phase 2 caller derives it by the same rule). The first version of
this layout had the dispatch's node id there (`shard=`), the way `units/` does. A keyed re-dispatch
carries a **fresh** node id — `catalog.py` mints one per call — so a node-keyed prefix could never be
found by the execution that re-runs the same Batch on the same key. Keyed by the instance, it is, and
the lookup is one LIST of one known prefix rather than a scan of the run.

**`run=` still scopes it, on purpose.** A Unit's `out` is refs into `units/run=<id>/`, so another run
folding them would return rows its own Dataset does not hold. Another run re-dispatching the same Batch
on the same key therefore runs it.

**The body names its batch and its Unit**, although the key already does. That redundancy is the
integrity check: a body at the wrong key is refused instead of folded into a batch it does not describe.

### What an attempt folds back

The host reads the last heartbeat's `checkpoint` (`activity.info().heartbeat_details` /
`activity.GetHeartbeatDetails`) and hands it to the engine. Details present is the retry signal; the
attempt number adds nothing to it and is not consulted.

| The attempt has… | and this worker… | Outcome |
|---|---|---|
| a checkpoint `accepted` for THIS batch, naming finished Units and a `manifest_ref` | has an object store | **fold back** those Units, run the rest |
| the same, but **no** object store here | — | **`CommitLost`**, non-retryable: re-running would hide a skewed fleet |
| no details (a new execution, or an attempt that died before its first beat), a checkpoint for another batch, or one naming nothing finished | has an object store | **list** the batch's prefix; **fold back** every Unit listed, run the rest |
| a checkpoint naming finished Units with no `manifest_ref` (an attempt from before S3 was mandatory) | has an object store | logged; **list**, which finds nothing that attempt wrote |
| a Unit to fold whose object is missing or refused, or a listed index the batch cannot hold | — | **`CommitLost`**, non-retryable, naming the key |
| a LIST or GET that fails for any other reason | — | the error as itself, **retryable** |

The checkpoint is accepted only through `checkpoint.accepted` / `checkpoint.Accepted` — the one rule
`resume_from` and the corpus share, so a checkpoint the corpus discards cannot be folded back. What a
listing may name is `commit_units` / `CommitUnits`, pinned by `commit.json` §list: only `unit=` + at
least five digits + `.json` directly under the prefix is a Unit; anything else is skipped rather than
guessed at; an index past the batch's length is refused, because the batch id hashes its Units and such
an object is not this batch's. The Method is handed only what is left, and the result carries every
Unit's output once, in input order. All of it happens **before the resource loads**, so a failure costs
no Load.

**One LIST per attempt that reaches the listing, and it is usually empty**: a Batch nobody ran before
has nothing under its prefix. That is one request on the first attempt of every dispatch. It is not
measured here; the parity gate's per-unit PUT measurement (WP-21's risk) is where it belongs.

**A missing commit object is loud, not a re-run.** The beat is sent only after the object lands, so a
finished Unit with nothing at its key is the store having lost it, or this worker reading a different
store. Folding it as empty would drop its rows; re-running it would hide a store that is losing data.
Neither improves on the next attempt — it reads the same checkpoint and the same store — so the error
is non-retryable, `type: "CommitLost"` from both hosts. A listed key that will not decode is the same
fact one step earlier.

**A resumed attempt never beats less than the last one finished.** Temporal keeps only the LAST beat,
and the Go host's keepalive beats from the moment `RunBatch` is entered — through the listing, the fold
and Load. So the host hands the prior checkpoint over before its beater exists (`ResumeFrom` publishes
it as is), it stays the published one while the fold refills the slots, and then the engine **seeds**:
it publishes this batch's own checkpoint — already naming every folded Unit — and beats it with matching
counts, before Load and before the Method runs. Without that, a tick during a long fold or a slow Load,
followed by a second death, would hand attempt 3 a checkpoint missing everything attempt 1 did. The
first version of this published from the still-empty slots before the fold; the mid-fold assertion in
`TestAResumedAttemptNeverPublishesLessThanThePreviousOneFinished` is what caught it. The seed is also
PR #43's review fix — a reused keyed instance must not beat the PREVIOUS batch's checkpoint into this
one's — which landed on `dev` first and is folded in here: one publish point, after the fold, not two.
(Python beats only on a commit, so it has no such window.)

**A previous attempt's `manifest_ref` is kept for writing, not only reading.** It differs from what this
worker derives only under prefix or layout skew between attempts, and splitting one batch's commits
across two prefixes would leave the next checkpoint pointing at half of them.

### A8: S3 is mandatory for an actor that commits

`serve()` (Python `internals/temporal/host.py`, Go `temporalhost.serve`) refuses to start an actor that
declares a Method when no object store is configured, **before anything connects**, naming
`KONTRA_S3_ENDPOINT`. In Go that also covers a store that failed to open, which `engine.Configure` used
to log and run on without. The reason is the one D1 gives: the bytes are S3's. Without a store a
finished Unit's output is durable nowhere, so every retry and every re-dispatch re-ran everything, and
the actor still polled and returned batches as if healthy. The first version of this amendment kept
that mode running "so no-S3 dev loops keep working"; the owner's decision reverses it. An actor that
declares no Method commits nothing (its Batch passes through) and is not refused.

**Tests use an in-memory store** that answers every call the engine makes on S3 — the record write,
and the commit object's put, get and list: every resume test in both SDKs, host-level and engine-level,
runs on one. **What is not
done, stated:** the engines' store-less branch still exists, because the older engine suites
(`tests/test_methods.py`, most of `tests/test_actor_engine.py`, Go's `actorWith`) assert inline records
and moving them onto the in-memory store means resolving refs in every assertion. It is now reachable
only from an in-process test — `serve()` never starts it — and deleting it is that test migration, not
a behaviour change.

### A9: `unit_state` stays where it was

The owner's default for A9 is S3 scratch objects (`scratch/{batch_id}/u{NNNNN}.json`), overwritten on
set and deleted on commit. **This change does not do it**, because it is not small: it is a PUT per
`set_unit_state`, a delete operation neither unit store has, a decision about the poison counter that
lives beside it, and the >2 MB payload and kill-mid-Unit tests the plan lists (WP-23). So `unit_state`
(tier 2, `{batch}-u{i}-ckpt`) and the poison counter beside it (`-kills` / `-reloads`) stay in the
actor's Redis hash, keyed by the batch's content hash under the 24 h write TTL — which means workers
still dial Redis for them, as well as for `global_state`/`object_state` (tiers 3/4, untouched, the
Phase 9 decision).

### What went

Gone from both engines and both `statekv`s: the tier-1 commit map (`{batch}-u{i}` fields), the
`batch-owner` field and the drop it triggered, the batch-boundary TTL renewal (`touch` / `Touch`) and
`drop` / `Drop`, whose only callers those were. Every write to the hash still slides its TTL.

### Consequences, stated rather than discovered

- **Cross-execution resume is wider than the Redis map's was, and one case changes behaviour.** Read
  from the removed code, not measured: the map resumed only when the instance, the run AND the node
  matched, because a dispatch with another run/node made the `batch-owner` guard drop the hash first —
  and a keyed dispatch mints a fresh node every time, so a keyed re-dispatch in practice re-ran. The
  listing resumes on the instance, the run and the batch's content hash, so a keyed re-dispatch now
  resumes, which is A7's point. The other side of it: **a workflow that deliberately dispatches the
  same Units with the same params on the same key twice in one run now gets the first call's outputs
  back instead of fresh work.** That is what `batch_id`'s own doc has always said ("an identical call,
  made twice on purpose, IS a retry and replays"); the guard used to hide it. A caller that wants fresh
  work varies the params, which makes it another Batch.
- **What resumes, and what re-runs.** Within an execution: a retry, from the heartbeat. Across
  executions on the same instance and run — a re-dispatch on the same idempotency key, a reopened keyed
  scope after `SessionLost` (non-retryable by design, ADR 0023 §20), a workflow-level retry of the
  backing workflow, a caller-pinned `node_id` — from the listing. An unkeyed scope reopens with a new
  Session id, which is a new instance, and re-runs; so does anything in another run.
- **What keeps the PUBLISHED Dataset free of duplicates on a re-run is that the caller publishes only
  the Batch a call returns** (`catalog.py` `_publish_to`, once per returned Batch) — a failed execution
  returns none, so none of its rows are published. Read from code, not measured. It is **not**
  `publishBatch`'s identity: `publishIdentity` (`data/parquet.ts`) covers the manifest sha AND the
  Machine (`node`), so it de-duplicates a retry of the publish activity itself and would not
  de-duplicate two successful executions of one batch on two Machines.
- **The raw `units/run=` prefix is not protected the same way.** A re-run Unit whose records are not
  byte-identical leaves the first run's blobs beside the second's, which the live row tail and a DuckDB
  glob over the run both count. A Unit folded back is not re-run, so this is now limited to the Units in
  flight at a death and to re-runs across instances or runs; content-deterministic records (ADR 0015)
  are what keeps it from happening at all.
- **A hard kill still re-runs up to one throttle window of commits** within an execution. Heartbeats are
  throttled and a SIGKILL drops the unsent one (legacy ADR 0018 §4). Those Units' objects exist but the
  checkpoint does not name them, so the retry — which trusts the checkpoint when it names this batch —
  re-runs them, overwrites their objects, and re-pushes by content sha. Not data loss; work redone.
  Listing on every attempt would close this too, at one LIST per retry; it is not done here, so the
  within-execution half stays exactly what D1 says. (Measured in the Go SDK's `TestActivityEnvironment`
  while writing this: the beats a *successful* attempt sends after its first never reached the listener
  — they need not, the result supersedes them.)
- **A keyed actor's in-flight scratch now outlives a change of owner** until it is committed, isolated
  or expires (24 h). The owner guard used to drop it; it is keyed by the batch's content hash, so only an
  identical batch on the same key can read it — which, under A7, is the batch that should.
- **Commit objects need a retention sweep.** `sweepUnits` (`data/maintenance.ts`) collects `units/` by
  run; nothing collects `commits/` yet. The layout leads with `run=` so the same run-grained rule
  applies, and under A7 an object is needed for as long as its run can re-dispatch the Batch — so the
  sweep belongs after the run closes, not after the activity does.
- **Runs in flight at upgrade time** hold their commit map in Redis, which nothing reads any more. Drain
  before deploying (WP-21's risk, unchanged).

## Tests (amendment)

- `shared/conformance/commit.json`, both arms — key layout (the instance segment, injection in every
  segment, the row-prefix property, the index past five digits), encoded field sets, the decode table
  with every refusal, and the `list` table: which names are Units, that order and duplicates do not
  matter, that a sibling batch's key is not this one's, and that an index past the batch is refused.
  Plus the round trip: every key the writer produces lists back as its own Unit.
- `tests/test_resume_from_checkpoint.py` and `runtime/go/temporalhost/resume_test.go` — through
  Temporal's own activity test environments, on a fresh activation table over an EMPTY state hash.
  Within an execution: attempt 1 commits k of n and dies; attempt 2 runs exactly the n−k remaining Units
  and returns n Units of output, none duplicated in the envelope or the store, and (Go) its first beat
  already names exactly attempt 1's Units. A checkpoint for another `batch_id` is discarded; a missing
  commit object fails the activity non-retryable as `CommitLost`. Across executions (A7): a re-dispatch
  on the same key with a NEW node id and no details runs exactly the n−k; a Batch an earlier execution
  finished runs nothing and returns the same refs; another instance and another run each run
  everything; a listed index the batch cannot hold, and (Python) a listed body naming another Unit, fail
  as `CommitLost`. A discarded checkpoint still resumes from the listing (Python host; Go engine).
- `tests/test_actor_engine.py` and `runtime/go/engine/resume_test.go` — the fold and its refusals
  (missing object, a body naming another Unit, no store here), the never-beat-less property, the
  reused-instance publish before Load, prefix keeping; a reopened keyed scope resumes from the listing;
  a fresh execution lists exactly once and (Go) has published the folded Units before Load; a failing
  LIST is retryable and nothing runs or loads.
- `tests/test_serve_requires_object_store.py` and Go's `TestServeRefusesACommittingActorWithNoObjectStore`
  (A8) — refused before anything connects, naming the variable and the actor; a load-only actor and an
  actor with a store start.
- Deliberate breaks, measured while writing this: the listing turned off (each SDK) turns the
  cross-execution and listing-refusal tests red; the instance segment dropped from the prefix turns
  "another instance" red, and the run segment dropped turns "another run" red (each SDK). The earlier breaks
  — the prior checkpoint ignored (each host, and the Go engine), a missing object folded as empty (both
  engines), the pre-Load publish removed (Go), the `CommitLost` mapping removed (Go host) — each turned
  at least one resume test red when the first version of this amendment was written.
