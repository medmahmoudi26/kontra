"""A retry resumes from the heartbeat, through Temporal's own activity seam (PRD D1, §10.3; ADR 0060).

The engine tests (`test_actor_engine.py`) hand `resume=` in directly. These go through the HOST's
`RunBatch` activity inside temporalio's `ActivityEnvironment`, so the two halves that tests of the
engine alone cannot see are real: what `activity.heartbeat` actually received on attempt 1, and what
`activity.info().heartbeat_details` actually hands attempt 2.

"A NEW PROCESS" IS MODELLED, NOT ASSUMED. A killed worker loses its activation table and nothing in
it survives, so attempt 2 runs with the host's `_SESSIONS` cleared and a FRESH state hash — which is
also the proof that resume no longer reads Redis. The object store is the one thing shared between
the attempts, because it is the one thing that outlives the worker.
"""

from __future__ import annotations

import asyncio
import dataclasses

import pytest
from temporalio.exceptions import ApplicationError
from temporalio.testing import ActivityEnvironment

from kontra import ActorRegistry, MethodRegistration
from internals.engine import build_session_factory
from internals.unitstore import commit_key
from test_actor_engine import FakeKV, FakeUnitStore


@pytest.fixture(autouse=True)
def a_fresh_process():
    from internals.temporal import host

    for table in (host._SESSIONS, host._LOCKS, host._SESSION_ACTORS):
        table.clear()
    yield
    for table in (host._SESSIONS, host._LOCKS, host._SESSION_ACTORS):
        table.clear()


class WorkerKilled(BaseException):
    """A death nothing in the engine catches — the shape of SIGKILL from inside the process."""


def _registry(method):
    reg = ActorRegistry()
    reg.actor_name, reg.version = "t", "0.1.0"
    reg.methods = {"m": MethodRegistration(fn=method, name="m", fn_name="m")}
    return reg


def _worker(method, store):
    """One worker PROCESS: its own activation table, its own Redis view, the shared object store."""
    from internals.temporal import host

    for table in (host._SESSIONS, host._LOCKS, host._SESSION_ACTORS):
        table.clear()
    factory = build_session_factory(_registry(method), store=store)
    kv = FakeKV()
    acts = host.build_activities(
        _registry(method), sessions=object(),
        session_factory=lambda aid, heartbeat=None: factory(aid, kv=kv, heartbeat=heartbeat))
    return {f.__name__: f for f in acts}["run_batch"], kv


def _attempt(run_batch, payload, *, attempt=1, details=None):
    """Run one attempt in an ActivityEnvironment. Returns (result or exception, beats)."""
    env = ActivityEnvironment()
    env.info = dataclasses.replace(env.info, attempt=attempt, heartbeat_details=list(details or []))
    beats = []
    env.on_heartbeat = lambda *d: beats.append(d[0])
    try:
        return asyncio.run(env.run(run_batch, payload)), beats
    except BaseException as e:  # noqa: BLE001 - the death IS the result of attempt 1
        return e, beats


def _rows(out, store):
    return [store.get_subunit(r["$ref"]["key"]) for r in out["results"]]


def _store_rows(store):
    import json as _json

    return [_json.loads(v)[0] for v in store.blobs.values()]


def test_a_batch_killed_after_k_of_n_commits_resumes_with_only_the_rest():
    """THE acceptance property, at the unit level: attempt 1 commits k of n and the worker dies;
    attempt 2, on another process with an empty state hash, runs EXACTLY the n-k Units that did not
    commit, and the batch returns n Units of output, none twice — in the envelope and in the store."""
    n, k = 7, 4
    units = [f"u{i}" for i in range(n)]
    payload = {"actor_id": "a1", "units": units, "method": "m", "run_id": "r", "node_id": "n"}
    store = FakeUnitStore()

    ran1 = []

    async def dies(self, batch, dataset):
        async for unit in batch:
            if len(ran1) == k:
                raise WorkerKilled()
            ran1.append(unit.value)
            await dataset.push({"u": unit.value})

    run_batch, _ = _worker(dies, store)
    died, beats1 = _attempt(run_batch, payload)
    assert isinstance(died, WorkerKilled), died
    assert ran1 == units[:k]
    assert beats1[-1]["checkpoint"]["done"] == [[0, k - 1]], beats1[-1]

    ran2 = []

    async def lives(self, batch, dataset):
        async for unit in batch:
            ran2.append(unit.value)
            await dataset.push({"u": unit.value})

    run_batch, kv2 = _worker(lives, store)
    out, beats2 = _attempt(run_batch, payload, attempt=2, details=[beats1[-1]])

    assert ran2 == units[k:], "exactly the n-k Units that had not committed"
    assert out["done"] is True
    assert [r["u"] for r in _rows(out, store)] == units, "n Units of output, in input order"
    assert sorted(r["u"] for r in _store_rows(store)) == sorted(units), "no row twice in the store"
    assert beats2[-1]["checkpoint"]["done"] == [[0, n - 1]]
    assert kv2.d == {}, "the resume read nothing from, and wrote nothing to, the state hash"


def test_a_checkpoint_for_a_different_batch_is_discarded():
    """Heartbeat details from some other batch — unit indices that mean nothing here — must not
    skip a single Unit. Everything runs."""
    units = ["a", "b", "c"]
    store = FakeUnitStore()
    ran = []

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            await dataset.push({"u": unit.value})

    run_batch, _ = _worker(method, store)
    stale = {"node": "n", "done": 3, "total": 3, "isolated": 0,
             "checkpoint": {"v": 1, "batch_id": "0000000000000000", "done": [[0, 2]], "failed": [],
                            "manifest_ref": "commits/run=r/actor=t/shard=n/batch=0000000000000000/"}}
    out, _ = _attempt(run_batch, {"actor_id": "a1", "units": units, "method": "m",
                                  "run_id": "r", "node_id": "n"}, attempt=2, details=[stale])
    assert ran == units
    assert [r["u"] for r in _rows(out, store)] == units


def test_a_missing_commit_object_fails_the_activity_loudly_and_without_retry():
    """A Unit the checkpoint calls finished with no commit object is data the store lost. The
    activity fails NON-RETRYABLE and names it — a retry would read the same checkpoint and the
    same store, and the quiet alternatives each lose or hide something."""
    units = ["a", "b", "c"]
    payload = {"actor_id": "a1", "units": units, "method": "m", "run_id": "r", "node_id": "n"}
    store = FakeUnitStore()
    ran = []

    async def dies(self, batch, dataset):
        async for unit in batch:
            if len(ran) == 2:
                raise WorkerKilled()
            ran.append(unit.value)
            await dataset.push({"u": unit.value})

    run_batch, _ = _worker(dies, store)
    _, beats = _attempt(run_batch, payload)
    ck = beats[-1]["checkpoint"]
    del store.commits[commit_key(ck["manifest_ref"], 0)]

    run_batch, _ = _worker(dies, store)
    failed, _ = _attempt(run_batch, payload, attempt=2, details=[beats[-1]])
    assert isinstance(failed, ApplicationError), failed
    assert failed.type == "CommitLost" and failed.non_retryable is True
    assert "unit 0" in str(failed) and commit_key(ck["manifest_ref"], 0) in str(failed)
    assert ran == ["a", "b"], "nothing ran on the attempt that could not fold its commits back"


def test_a_first_attempt_ignores_the_absence_of_details():
    """No details is the ordinary first attempt, and it must not be mistaken for anything else."""
    store = FakeUnitStore()
    ran = []

    async def method(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            await dataset.push({"u": unit.value})

    run_batch, _ = _worker(method, store)
    out, beats = _attempt(run_batch, {"actor_id": "a1", "units": ["a", "b"], "method": "m",
                                      "run_id": "r", "node_id": "n"})
    assert ran == ["a", "b"] and out["done"] is True
    assert beats[-1]["checkpoint"]["manifest_ref"].startswith("commits/run=r/")
