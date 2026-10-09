# 60. The commit map travels in the heartbeat

Date: 2026-10-06

## Status

**Accepted, and both halves are landed** — the second by the amendment at the end (2026-10-10): a
retry now RESUMES from the heartbeat, and the Redis commit map is gone. Sequel to **0059**, which
stopped the store losing the commit map. This moves it somewhere the store cannot reach.

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

## Amendment (2026-10-10): a retry resumes from the heartbeat, and the commit map is gone

PRD D1 settles the owner: *execution, progress, resume — Temporal; the activity heartbeat carries the
checkpoint.* Resume is therefore Temporal's, **within one activity execution, across its attempts**.
This is what was built, in both SDKs, corpus first.

### A finished Unit is an object; `manifest_ref` names where they live

When a Unit commits or is isolated, the engine writes ONE object holding its outcome — its output
refs, or its error and category — **synchronously, before the beat that reports it**:

```
commits/run={run}/actor={actor}/shard={shard}/batch={batch_id}/unit={i:05d}.json
{"v":1,"batch_id":"…","unit":3,"out":[{"$ref":{…}}]}
```

The checkpoint's `manifest_ref` is that batch prefix (with `KONTRA_S3_PREFIX` in front, exactly as a
pushed record's key has it). So a heartbeat stays a range set and a pointer — no batch-size ceiling —
and a reader gets each finished Unit's outputs back without them ever riding the payload.
`shared/conformance/commit.json` pins the key, the body's field set, and what a reader refuses; the arms
are `runtime/python/internals/test_commit_conformance.py` and
`runtime/go/unitstore/commit_conformance_test.go`.

**`commits/` is its own top-level prefix, not a corner of `units/`.** Everything under `units/run=<id>/`
ending in `.json` is a ROW to the live row tail (`rowTail.ts` counts them) and to a DuckDB glob over the
run, so a commit object there would have inflated every run's count by its Unit count. `run=` still
leads, for the reason `blobkey.json` gives.

**The body names its batch and its Unit**, although the key already does. That redundancy is the
integrity check: a body at the wrong key is refused instead of folded into a batch it does not describe.

### A retry folds the finished Units back

The host reads the last heartbeat's `checkpoint` (`activity.info().heartbeat_details` /
`activity.GetHeartbeatDetails`) and hands it to the engine. The engine accepts it only if
`checkpoint.accepted` / `checkpoint.Accepted` does — the one rule `resume_from` and the corpus share, so a
checkpoint the corpus discards cannot be folded back — then reads each finished Unit's object and puts
its outputs (or its failure) back in its slot. The Method is handed only the rest, and the result
carries every Unit's output once, in input order. Details present is the retry signal; the attempt
number adds nothing to it and is not consulted.

What happens when the checkpoint is honoured:

| The checkpoint… | and this worker… | Outcome |
|---|---|---|
| names finished Units and a `manifest_ref` | has an object store | **fold back**, run the rest |
| names finished Units, no `manifest_ref` | — | **re-run every Unit** (logged): the attempt that finished them had no object store, so their outputs were never durable anywhere |
| names finished Units and a `manifest_ref` | has **no** object store | **`CommitLost`**, non-retryable |
| names a Unit whose object is missing or refused | — | **`CommitLost`**, non-retryable, naming the key |

**A missing commit object is loud, not a re-run.** The beat is sent only after the object lands, so a
finished Unit with nothing at its key is the store having lost it, or this worker reading a different
store. Folding it as empty would drop its rows; re-running it would hide a store that is losing data.
Neither improves on the next attempt — it reads the same checkpoint and the same store — so the error
is non-retryable, `type: "CommitLost"` from both hosts, and the fold happens **before** the resource
loads, so the failure costs no Load.

**The no-S3 mode keeps running, and re-runs instead of resuming.** S3 owns the bytes (PRD D1), so the
inline path has nowhere durable to put a finished Unit's output. Refusing to run without a store was the
alternative; it would break every no-S3 dev and test loop for an optimisation, while re-running is what
the contract already permits — Method bodies tolerate replay. The unit tests keep running against an
in-memory store that implements the same three calls.

**A resumed attempt never beats less than the last one finished.** Temporal keeps only the LAST beat,
and the Go host's keepalive beats from the moment `RunBatch` is entered — through the fold and through
Load. So the host hands the prior checkpoint over before its beater exists (`ResumeFrom` publishes it
as is), it stays the published one while the fold refills the slots, and the engine publishes this
batch's own — already including the folded Units — before Load. Without that, a tick during a long
fold or a slow Load, followed by a second death, would hand attempt 3 a checkpoint missing everything
attempt 1 did. The first version of this published from the still-empty slots before the fold; the
mid-fold assertion in `TestAResumedAttemptNeverPublishesLessThanThePreviousOneFinished` is what caught
it. The same publish also stops a reused keyed instance from beating the PREVIOUS batch's checkpoint
into this one's. (Python beats only on a commit, so it has no such window.)

**A previous attempt's `manifest_ref` is kept for writing, not only reading.** It differs from what this
worker derives only under prefix or layout skew between attempts, and splitting one batch's commits
across two prefixes would leave the next checkpoint pointing at half of them.

### What went, and what stayed

Gone from both engines and both `statekv`s: the tier-1 commit map (`{batch}-u{i}` fields), the
`batch-owner` field and the drop it triggered, the batch-boundary TTL renewal (`touch` / `Touch`) and
`drop` / `Drop`, whose only callers those were. Every write to the hash still slides its TTL.

Stayed on Redis, as the PRD leaves them: `unit_state` (tier 2, `{batch}-u{i}-ckpt`) and the poison
counter beside it (`-kills` / `-reloads`); `global_state` and `object_state` (tiers 3/4) live in another
store and are untouched. Their move is the hardening Phase 9 decision.

### Consequences, stated rather than discovered

- **Cross-execution resume is gone.** A re-dispatch on the same idempotency key, a workflow-level retry
  of the backing workflow, and a reopened scope after `SessionLost` (Python's is non-retryable by design,
  ADR 0023 §20) are NEW activity executions; heartbeat details do not cross executions, so they re-run
  the batch. Read from the removed code, not measured: the old map resumed only when run AND node
  matched — a new dispatch id made the `batch-owner` guard drop the hash first — so what loses resume
  in practice is a caller-pinned `node_id` or a re-run of the same `EntryInput`.
- **What keeps the PUBLISHED Dataset free of duplicates on such a re-run is that the caller publishes
  only the Batch a call returns** (`catalog.py` `_publish_to`, once per returned Batch) — a failed
  execution returns none, so none of its rows are published. Read from code, not measured. It is **not**
  `publishBatch`'s identity: `publishIdentity` (`data/parquet.ts`) covers the manifest sha AND the Machine
  (`node`), so it de-duplicates a retry of the publish activity itself and would not de-duplicate two
  successful executions of one batch on two Machines.
- **The raw `units/run=` prefix is not protected the same way.** A re-run whose records are not
  byte-identical leaves the first execution's blobs beside the second's, which the live row tail and a
  DuckDB glob over the run both count. This was already true for the in-flight Unit, and for every
  cross-execution retry the owner guard cleared; content-deterministic records (ADR 0015) are what
  keeps it from happening.
- **A hard kill still re-runs up to one throttle window of commits.** Heartbeats are throttled and a
  SIGKILL drops the unsent one (legacy ADR 0018 §4). Those Units' objects exist but the checkpoint does
  not name them, so they re-run, overwrite their objects, and re-push by content sha. Not data loss; work
  redone. (Measured in the Go SDK's `TestActivityEnvironment` while writing this: the beats a
  *successful* attempt sends after its first never reached the listener — they need not, the result
  supersedes them.)
- **A keyed actor's in-flight scratch now outlives a change of owner** until it is committed, isolated
  or expires (24 h). The owner guard used to drop it; it is keyed by the batch's content hash, so only an
  identical batch on the same key can read it.
- **Commit objects need a retention sweep.** `sweepUnits` (`data/maintenance.ts`) collects `units/` by
  run; nothing collects `commits/` yet. The layout leads with `run=` so the same run-grained rule
  applies; an object is only needed while its activity can still retry.

## Tests (amendment)

- `shared/conformance/commit.json`, both arms — key layout (including the row-prefix property and the
  index past five digits), encoded field sets, and the decode table with every refusal.
- `tests/test_resume_from_checkpoint.py` and `runtime/go/temporalhost/resume_test.go` — through
  Temporal's own activity test environments: attempt 1 commits k of n and dies; attempt 2, on a fresh
  activation table over an EMPTY state hash, runs exactly the n−k remaining Units and returns n Units of
  output with none duplicated in the envelope or the store; a checkpoint for another `batch_id` is
  discarded; a missing commit object fails the activity non-retryable as `CommitLost`.
- `tests/test_actor_engine.py` and `runtime/go/engine/resume_test.go` — the fold and its refusals
  (missing object, a body naming another Unit, no store here), the no-store re-run, the
  never-beat-less property, the reused-instance publish before Load, and prefix keeping.
- The new resume tests were run against deliberate breaks — the prior checkpoint ignored (each host,
  and the Go engine), a missing object folded as empty (both engines), the pre-Load publish removed (Go),
  the `CommitLost` mapping removed (Go host). Measured: every break turned at least one of them red.

