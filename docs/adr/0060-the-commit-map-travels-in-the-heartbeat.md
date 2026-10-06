# 60. The commit map travels in the heartbeat

Date: 2026-10-06

## Status

**Accepted, and the first half is landed.** Sequel to **0059**, which stopped the store losing the
commit map. This moves a copy of it somewhere the store cannot reach.

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
commit.

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
