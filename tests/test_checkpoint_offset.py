"""A checkpoint is an input index AND an output offset (slice 06, ADR 0028 §1).

Slice 04 moved records off the Unit; this pins the durability those records stand on. Two save
points, kept apart on purpose: the INPUT cursor (which Unit was last committed) and the OUTPUT
offset (how far through the append-only output we are). The tests that matter here are the ones that
really kill a host mid-Batch and count rows — a test that asserts the commit map's contents proves
nothing about the duplication that actually reaches production data.

The out-of-loop cases pin the mechanism ADR 0028 settled after three inference attempts failed: an
out-of-loop push is identified by an EXPLICIT KEY the author supplies, reconciled first-write-wins
across an isolation re-invoke, never inferred from content or control-flow position. The four cases
that broke the three prior mechanisms all hold here simultaneously.

The Go peer asserts the same behaviours in
runtime/go/engine/checkpoint_offset_test.go, so a drift in either host is a mismatch
rather than two green suites that disagree.
"""

import asyncio

import pytest

from kontra.batch import MissingPushKey
from test_actor_engine import FakeUnitStore, make_host


def _store_records(store):
    """Every distinct record the store holds, decoded — the durable row count a reader sees."""
    import json as _json

    return [_json.loads(v)[0] for v in store.blobs.values()]


def _resolved_results(out, store):
    """The call's results as records, resolving the `{"$ref": …}` entries a store-backed tail
    returns back through the store — so results and `_store_records` are compared like for like."""
    return [store.get_subunit(r["$ref"]["key"]) if isinstance(r, dict) and "$ref" in r else r
            for r in out["results"]]


# ---------------------------------------------------------------------------------------------
# The FOUR cases that must hold SIMULTANEOUSLY (ADR 0028 §consequence 6). Three prior mechanisms
# each passed some and failed others; an explicit key passes all four.
# ---------------------------------------------------------------------------------------------


def test_case1_constant_unconditional_out_of_loop_push_appears_once():
    """CASE 1. A constant push before the loop, keyed, appears exactly once across the isolation
    re-invoke that a mid-Batch failure triggers — the truncate-and-re-execute mechanism kept this,
    the accumulate mechanism kept this, and the key mechanism keeps it too."""

    async def method(self, batch, dataset):
        await dataset.push({"pre": "once"}, key="pre")
        async for unit in batch:
            if unit.value == "bad":
                raise ValueError("boom")
            await dataset.push({"u": unit.value})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["a", "bad", "c"], "run_id": "r", "node_id": "n"}))

    results = _resolved_results(out, store)
    assert results.count({"pre": "once"}) == 1, results
    assert [f["unit"] for f in out["failures"]] == ["bad"]
    # results and the durable store agree — one `pre` blob, no orphan a prefix reader would surface.
    assert [r for r in _store_records(store) if r == {"pre": "once"}] == [{"pre": "once"}]


def test_case2_self_guarded_out_of_loop_push_is_not_lost():
    """CASE 2. A one-time push guarded by self-state runs on entry 1 and NOT on entry 2 (self.*
    survives the in-memory re-invoke). truncate-and-re-execute LOST it; the key mechanism retains
    the slot, so it must NOT be lost — exactly once, in results and in the store."""

    async def method(self, batch, dataset):
        if not getattr(self, "_h", False):
            await dataset.push({"header": True}, key="hdr")
            self._h = True
        async for unit in batch:
            if unit.value == "bad":
                raise ValueError("boom")
            await dataset.push({"u": unit.value})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["a", "bad", "c"], "run_id": "r", "node_id": "n"}))

    results = _resolved_results(out, store)
    assert results.count({"header": True}) == 1, results
    assert [r for r in _store_records(store) if r == {"header": True}] == [{"header": True}]
    assert {"u": "a"} in results and {"u": "c"} in results


def test_case3_content_varying_out_of_loop_push_folds_to_one():
    """CASE 3. A push whose content varies across the re-invoke (self._n moves while author locals
    reset) re-pushes with different bytes on entry 2. accumulate-and-content-dedup DUPLICATED it
    (two identities, two blobs). Keyed first-write-wins drops entry 2's re-push BEFORE the write:
    exactly one `seen` record, its first value (0), in results AND the store — no orphan blob."""

    async def method(self, batch, dataset):
        await dataset.push({"seen": getattr(self, "_n", 0)}, key="seen")
        async for unit in batch:
            if unit.value == "bad":
                raise ValueError("boom")
            self._n = getattr(self, "_n", 0) + 1
            await dataset.push({"u": unit.value})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["a", "bad", "c"], "run_id": "r", "node_id": "n"}))

    result_seen = [r["seen"] for r in _resolved_results(out, store) if "seen" in r]
    store_seen = [r["seen"] for r in _store_records(store) if "seen" in r]
    assert result_seen == [0], result_seen
    assert store_seen == [0], store_seen


def test_case4_prefix_shift_keeps_both_pushes_each_once():
    """CASE 4 — the prefix shift that broke the (group, ordinal) mechanism. A `warn` push appears
    ONLY on the re-invoke, ahead of an unconditional `data` push. Under a positional ordinal, warn
    drew the ordinal data had filled (first-write-wins DROPPED warn) and data slid to a new ordinal
    (DUPLICATED data) — the documented `[data,data]` + warn-lost failure. With an explicit key, warn
    carries a key nobody wrote (kept) and data carries a key already written (skipped): ONE data AND
    ONE warn, both present, each once."""

    async def method(self, batch, dataset):
        if getattr(self, "_bad", False):
            await dataset.push({"warn": "retry"}, key="warn")   # only on the re-invoke
        await dataset.push({"data": "always"}, key="data")      # unconditional, follows it
        async for unit in batch:
            self._bad = True
            if unit.value == "bad":
                raise ValueError("boom")
            await dataset.push({"u": unit.value})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["bad", "c"], "run_id": "r", "node_id": "n"}))

    results = _resolved_results(out, store)
    assert results.count({"data": "always"}) == 1, results
    assert results.count({"warn": "retry"}) == 1, results
    # The durable store agrees — one of each, no orphan and nothing lost.
    store_recs = _store_records(store)
    assert store_recs.count({"data": "always"}) == 1, store_recs
    assert store_recs.count({"warn": "retry"}) == 1, store_recs


# ---------------------------------------------------------------------------------------------
# Extras required by the slice: two isolations, keyed-after beside keyed-before, several keys,
# a task under batch.units, and the unkeyed-push-raises case.
# ---------------------------------------------------------------------------------------------


def test_a_constant_keyed_push_survives_two_isolations_in_one_batch():
    """Two Units isolate in one Batch, so the body is entered three times. A constant keyed pre
    push must fold to exactly one across all three entries — first-write-wins holds past the first
    re-invoke, not just it."""

    async def method(self, batch, dataset):
        await dataset.push({"pre": "once"}, key="pre")
        async for unit in batch:
            if unit.value in ("x", "y"):
                raise ValueError("boom")
            await dataset.push({"u": unit.value})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["a", "x", "y", "c"], "run_id": "r", "node_id": "n"}))

    assert [f["unit"] for f in out["failures"]] == ["x", "y"]
    assert _resolved_results(out, store).count({"pre": "once"}) == 1
    assert [r for r in _store_records(store) if r == {"pre": "once"}] == [{"pre": "once"}]


def test_a_keyed_push_after_the_loop_coexists_with_a_keyed_push_before_it():
    """A keyed pre push and a keyed post push share the tail across an isolation re-invoke. The pre
    push runs on both entries (folds to one by its key); the post push runs ONLY on the entry where
    the loop completes. Distinct keys keep them in distinct slots, so both survive exactly once —
    the failure the positional scheme could only avoid with a load-bearing group split."""

    async def method(self, batch, dataset):
        await dataset.push({"header": getattr(self, "_n", 0)}, key="hdr")
        async for unit in batch:
            if unit.value == "bad":
                raise ValueError("boom")
            self._n = getattr(self, "_n", 0) + 1
            await dataset.push({"u": unit.value})
        await dataset.push({"summary": "done"}, key="sum")

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["a", "bad", "c"], "run_id": "r", "node_id": "n"}))

    results = _resolved_results(out, store)
    assert [r["header"] for r in results if "header" in r] == [0]
    assert [r["summary"] for r in results if "summary" in r] == ["done"]
    store_recs = _store_records(store)
    assert [r["header"] for r in store_recs if "header" in r] == [0]
    assert [r["summary"] for r in store_recs if "summary" in r] == ["done"]


def test_several_distinct_keys_in_one_entry_each_fold_to_one():
    """Three distinct out-of-loop keys in one entry, all content-varying. Each is keyed on its own,
    so on the re-invoke each re-push finds its key filled and is dropped: three records survive,
    each its first value — not six, not one."""

    async def method(self, batch, dataset):
        n = getattr(self, "_n", 0)
        await dataset.push({"alpha": n}, key="alpha")
        await dataset.push({"beta": n}, key="beta")
        await dataset.push({"gamma": n}, key="gamma")
        async for unit in batch:
            if unit.value == "bad":
                raise ValueError("boom")
            self._n = getattr(self, "_n", 0) + 1
            await dataset.push({"u": unit.value})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["a", "bad", "c"], "run_id": "r", "node_id": "n"}))

    results = _resolved_results(out, store)
    assert [r["alpha"] for r in results if "alpha" in r] == [0]
    assert [r["beta"] for r in results if "beta" in r] == [0]
    assert [r["gamma"] for r in results if "gamma" in r] == [0]


def test_a_keyed_push_from_a_task_spawned_under_batch_units():
    """A task spawned under `batch.units` has no current Unit, so its push rides the tail and must
    carry a key (ADR 0028). Keyed per Unit, every record lands exactly once and the whole Batch
    commits at Method exit — the concurrent path with identity told, not inferred from a
    non-deterministic arrival order."""

    async def method(self, batch, dataset):
        async def one(unit):
            await dataset.push({"u": unit.value}, key=f"u-{unit.value}")

        await asyncio.gather(*(one(u) for u in batch.units))

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": ["a", "b", "c"]}))
    assert out["done"] is True
    assert sorted(r["u"] for r in out["results"]) == ["a", "b", "c"]


def test_an_unkeyed_out_of_loop_push_raises_at_the_call_site():
    """The refusal. A push with no current Unit and no key is an author error, raised immediately —
    not accepted with a generated key (a fourth guess) and not silently duplicated later. The
    message names the fix. The failure surfaces as the whole call raising, since there is no Unit to
    blame it on."""

    async def method(self, batch, dataset):
        await dataset.push({"pre": "unkeyed"})       # no key, no current Unit -> raises
        async for unit in batch:
            await dataset.push({"u": unit.value})

    host = make_host(method)
    with pytest.raises(MissingPushKey) as ei:
        asyncio.run(host.run_batch({"units": ["a", "b"]}))
    assert "key=" in str(ei.value)


def test_an_unkeyed_push_from_a_batch_units_task_raises():
    """The same refusal on the concurrent path: a task under `batch.units` has no current Unit, so
    an unkeyed push there raises rather than riding the tail on an inferred identity."""

    async def method(self, batch, dataset):
        async def one(unit):
            await dataset.push({"u": unit.value})    # no key under batch.units -> raises

        await asyncio.gather(*(one(u) for u in batch.units))

    host = make_host(method)
    with pytest.raises(MissingPushKey):
        asyncio.run(host.run_batch({"units": ["a", "b"]}))


# ---------------------------------------------------------------------------------------------
# Really kill a host mid-Batch and count rows (ADR 0028 §consequence 5)
# ---------------------------------------------------------------------------------------------


def test_a_host_killed_mid_batch_and_retried_produces_the_exact_row_count():
    """The acceptance criterion that matters (PRD §10.3, unit half). A host dies mid-Batch with
    records already durable, Temporal retries the activity with the last heartbeat's checkpoint, and
    the run produces EXACTLY the expected rows — no duplicate (the committed Units are folded back
    from their commit objects, not re-run), no loss (the in-flight Unit re-runs and re-pushes by
    content sha into the same blob)."""

    class HostKilled(BaseException):
        pass

    ran, died = [], []

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            if unit.value == "c" and not died:
                await dataset.push({"u": "c"})       # durable before the death...
                died.append(1)
                raise HostKilled()                    # ...and the Unit never commits
            await dataset.push({"u": unit.value})

    store = FakeUnitStore()
    host = make_host(method, store=store)
    beats = []
    host._heartbeat = beats.append
    units = ["a", "b", "c", "d"]
    with pytest.raises(HostKilled):
        asyncio.run(host.run_batch({"units": units, "run_id": "r", "node_id": "n"}))

    out = asyncio.run(host.run_batch({"units": units, "run_id": "r", "node_id": "n"},
                                     resume=beats[-1]["checkpoint"]))
    assert out["done"] is True
    assert ran == ["a", "b", "c", "c", "d"], "a committed Unit ran twice"
    # Exactly one row per input Unit, in input order — the envelope AND the durable store agree.
    assert [r["u"] for r in _resolved_results(out, store)] == units
    got = sorted(r["u"] for r in _store_records(store))
    assert got == ["a", "b", "c", "d"], got


# ---------------------------------------------------------------------------------------------
# Output from a failed Unit stays; the Unit is still dropped (ADR 0028 §consequence 3)
# ---------------------------------------------------------------------------------------------


def test_records_pushed_by_a_unit_that_then_raises_are_kept_and_the_unit_is_dropped():
    """The behaviour change stated as intended, not discovered. A Unit that pushes records and
    then raises leaves those records in the durable store — nscheck's own header argues findings
    from failures ARE the output — and is still reported as dropped. The records are durable at
    push time, so nothing rolls them back; the Unit lands in `failures`, not in `results`."""

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"finding": f"{unit.value}-1"})
            await dataset.push({"finding": f"{unit.value}-2"})
            if unit.value == "boom":
                raise ValueError("bad after two findings")

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["a", "boom", "c"], "run_id": "r", "node_id": "n"}))

    # 'boom' is dropped, not committed — but its two findings are still in the store.
    assert [f["unit"] for f in out["failures"]] == ["boom"]
    kept = sorted(r["finding"] for r in _store_records(store))
    assert "boom-1" in kept and "boom-2" in kept, kept
    # results carries only the committed Units' output (a,c), never the dropped Unit's — those
    # rows live in the dataset, read by a streaming consumer, not folded into the call's return.
    assert len(out["results"]) == 4               # a-1,a-2,c-1,c-2


# ---------------------------------------------------------------------------------------------
# A Method taking the whole Batch commits everything at Method exit
# ---------------------------------------------------------------------------------------------


def test_a_whole_batch_method_commits_everything_at_method_exit():
    """Giving up per-Unit commit (ADR 0028 §1): an author who takes `batch.units` to run them
    concurrently has no iterator position to commit by, so nothing commits until the Method
    returns — and then everything does. Each push carries a key (no current Unit), the full row
    count comes back, and every Unit's slot is committed in the map."""

    async def method(self, batch, dataset):
        async def one(unit):
            await dataset.push({"u": unit.value}, key=f"u-{unit.value}")

        await asyncio.gather(*(one(u) for u in batch.units))

    host = make_host(method)
    units = ["a", "b", "c"]
    out = asyncio.run(host.run_batch({"units": units}))
    assert out["done"] is True
    assert sorted(r["u"] for r in out["results"]) == ["a", "b", "c"]
