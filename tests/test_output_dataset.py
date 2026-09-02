"""The output Dataset: a Method pushes records to its third parameter (ADR 0028).

The per-**Unit** emit is gone. The author pushes to `dataset`, names no Unit, and the record's
provenance lives inside it. These tests pin the two things that buys:

  • EVERY GRANULARITY IS ONE CALL, no flag — one record per Unit, N per Unit, one per three Units,
    one for the whole Batch, or none at all for a Unit (ADR 0028 §1). The question "what
    granularity does emit support?" disappears because push no longer names a Unit.
  • A BODY IS UNIT-TESTABLE by substituting the Dataset — no host, no queue, no object store
    (`actorkit.testing`). This is the forcing argument for a parameter over a `yield` or a bare
    `emit` callable.

Durability is UNCHANGED here (the mechanism swap is slice 06): a push commits against the Unit the
iterator is handing out, exactly as emit did. A failing store surfaces at the next checkpoint and
does not silently lose the record.
"""

from dataclasses import dataclass

import pytest

import asyncio

from internals.engine import batch_id, unit_slot
from test_actor_engine import FakeUnitStore, make_host


# ---------------------------------------------------------------------------------------------
# Granularity — one API, no flag (ADR 0028 §1)
# ---------------------------------------------------------------------------------------------


def test_one_record_for_the_whole_batch_pushed_outside_the_loop():
    """A push made AFTER the loop has no current Unit, so it rides the Batch tail under an explicit
    key and folds into results after the per-Unit output. `async with` is not involved; the loop
    simply ended."""

    async def method(self, batch, dataset):
        n = 0
        async for unit in batch:
            n += 1
        await dataset.push({"total": n}, key="total")

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": ["a", "b", "c"]}))
    assert out["done"] is True
    assert out["results"] == [{"total": 3}]          # the one aggregate, nothing per-Unit


def test_one_record_per_three_units():
    async def method(self, batch, dataset):
        async for unit in batch:
            if unit.index % 3 == 0:
                await dataset.push({"i": unit.index})

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": list("abcdefghi")}))
    assert out["done"] is True
    assert out["results"] == [{"i": 0}, {"i": 3}, {"i": 6}]


def test_n_records_for_one_unit_and_none_for_another():
    """1 -> N and 1 -> 0 in the same Batch: the count is the author's business, and a Unit that
    pushes nothing is not a failure — it just produced no rows."""

    async def method(self, batch, dataset):
        async for unit in batch:
            for k in range(unit.value):
                await dataset.push({"seed": unit.value, "k": k})

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": [2, 0, 1]}))
    assert out["done"] is True
    assert out["results"] == [{"seed": 2, "k": 0}, {"seed": 2, "k": 1}, {"seed": 1, "k": 0}]
    assert out["failures"] == []                     # the 0-record Unit did not fail


def test_not_pushing_for_a_unit_is_ordinary():
    async def method(self, batch, dataset):
        async for unit in batch:
            if unit.value != "skip":
                await dataset.push({"u": unit.value})

    host = make_host(method)
    out = asyncio.run(host.run_batch({"units": ["a", "skip", "b"]}))
    assert out["done"] is True
    assert out["results"] == [{"u": "a"}, {"u": "b"}]
    assert out["failures"] == []


# ---------------------------------------------------------------------------------------------
# push returns nothing; a failing store surfaces, and does not silently lose the record
# ---------------------------------------------------------------------------------------------


class FailingUnitStore:
    """A store whose every write fails — a disk-full SeaweedFS is a bare 500 on each PUT. The
    isolation counters do not catch it, which is exactly why a failed push must surface rather
    than be swallowed."""

    blobs: dict = {}

    def put_subunit(self, *a, **k):
        raise RuntimeError("disk full")

    def get_subunit(self, key):
        raise KeyError(key)


def test_push_returns_nothing():
    """The Kafka-producer contract (ADR 0028 §3): push is fire-and-forget, so it returns None and
    an author cannot branch on its result."""

    seen = []

    async def method(self, batch, dataset):
        async for unit in batch:
            seen.append(await dataset.push({"u": unit.value}))

    host = make_host(method)
    asyncio.run(host.run_batch({"units": ["a", "b"]}))
    assert seen == [None, None]


def test_a_failing_store_surfaces_at_the_checkpoint_and_does_not_commit_the_unit():
    """A write failure is HELD by push, then raised at the next checkpoint — before the Unit it
    struck commits. That ordering is load-bearing: a committed Unit is skipped on retry, so
    committing one whose record never persisted would lose the record for good (ADR 0028
    §consequence 5)."""

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"u": unit.value})

    host = make_host(method, store=FailingUnitStore())
    with pytest.raises(RuntimeError, match="disk full"):
        asyncio.run(host.run_batch({"units": ["a"], "run_id": "r", "node_id": "n"}))

    # The Unit did NOT commit — a retry re-runs it and re-pushes, so nothing was silently lost.
    bid = batch_id("method", ["a"], {})
    assert unit_slot(bid, 0) not in host._kv.d


def test_a_failing_store_fails_the_whole_call_rather_than_isolating_one_unit():
    """A broken store is systemic, not one bad Unit. Isolating would report `dropped` and let the
    Batch 'complete', which is how a whole run once reported success against a dead store."""

    async def method(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"u": unit.value})

    host = make_host(method, store=FailingUnitStore())
    with pytest.raises(RuntimeError, match="disk full"):
        asyncio.run(host.run_batch({"units": ["a", "b", "c"], "run_id": "r", "node_id": "n"}))


# ---------------------------------------------------------------------------------------------
# Concurrent / tail pushes are still made durable at push-time (a crawler streams as it goes)
# ---------------------------------------------------------------------------------------------


def test_concurrent_pushes_are_durable_at_push_time_not_at_return():
    """webcrawl runs seeds concurrently and pushes from each task — there is no current Unit, so
    each record rides the tail under its own explicit key. They must still be written to the store
    AS they are pushed, keyed by content sha, so a downstream cursor sees them before the Batch
    returns."""

    async def method(self, batch, dataset):
        async def one(unit):
            await dataset.push({"u": unit.value}, key=f"u-{unit.value}")

        await asyncio.gather(*(one(u) for u in batch.units))

    store = FakeUnitStore()
    host = make_host(method, store=store)
    out = asyncio.run(host.run_batch({"units": ["a", "b"], "run_id": "r", "node_id": "n"}))
    assert out["done"] is True
    assert len(out["results"]) == 2 and all("$ref" in r for r in out["results"])
    assert len(store.blobs) == 2                     # one durable blob per push, at push-time


# ---------------------------------------------------------------------------------------------
# The body is unit-testable by substituting the Dataset — no host, no store (ADR 0028)
# ---------------------------------------------------------------------------------------------


@dataclass
class Verdict:
    host: str
    ok: bool


def test_a_method_body_is_unit_tested_by_substituting_the_dataset():
    """The criterion that proves the parameter. `stub_batch` is the input the caller passes first,
    `collecting_dataset` the destination it passes second — backed by a list, not the lake. No
    host, no queue, no object store."""
    from actorkit.testing import collecting_dataset, stub_batch

    async def ask(self, batch, dataset):
        async for unit in batch:
            await dataset.push(Verdict(host=unit.value["host"], ok=unit.value["host"] != "down"))

    ds = collecting_dataset()
    asyncio.run(ask(object(), stub_batch([{"host": "a"}, {"host": "down"}]), ds))
    assert ds.records == [{"host": "a", "ok": True}, {"host": "down", "ok": False}]


def test_a_typed_emits_value_and_a_plain_dict_both_push():
    """`push` takes the Method's declared `emits` type OR a plain dict, exactly as emit did — the
    typed value is reduced to JSON at the boundary, so both land as the same record shape."""
    from actorkit.testing import collecting_dataset, stub_batch

    async def m(self, batch, dataset):
        async for unit in batch:
            await dataset.push(Verdict(host=unit.value, ok=True))     # typed
            await dataset.push({"host": unit.value, "ok": False})     # plain dict

    ds = collecting_dataset()
    asyncio.run(m(object(), stub_batch(["a"]), ds))
    assert ds.records == [{"host": "a", "ok": True}, {"host": "a", "ok": False}]


def test_the_declared_takes_type_is_honoured_by_the_stub_batch():
    """A stub Batch coerces to the Method's `takes` just as the host does, so `unit.value.host`
    works in a body under test the same way it works in production."""
    from actorkit.testing import collecting_dataset, stub_batch

    @dataclass
    class Pair:
        host: str
        ns: str = "ns1"

    async def ask(self, batch, dataset):
        async for unit in batch:
            await dataset.push({"host": unit.value.host, "ns": unit.value.ns})

    ds = collecting_dataset()
    asyncio.run(ask(object(), stub_batch([{"host": "a"}], takes=Pair), ds))
    assert ds.records == [{"host": "a", "ns": "ns1"}]     # the default applied via coercion
