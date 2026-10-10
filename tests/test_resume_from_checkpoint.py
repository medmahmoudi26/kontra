"""A retry resumes from the heartbeat, and a re-dispatch from the store, through Temporal's own
activity seam (PRD D1, §10.3; owner decision A7; ADR 0060).

The engine tests (`test_actor_engine.py`) hand `resume=` in directly. These go through the HOST's
`RunBatch` activity inside temporalio's `ActivityEnvironment`, so the two halves that tests of the
engine alone cannot see are real: what `activity.heartbeat` actually received on attempt 1, and what
`activity.info().heartbeat_details` actually hands attempt 2.

"A NEW PROCESS" IS MODELLED, NOT ASSUMED. A killed worker loses its activation table and nothing in
it survives, so attempt 2 runs with the host's `_SESSIONS` cleared and a FRESH state hash — which is
also the proof that resume no longer reads Redis. The object store is the one thing shared between
the attempts, because it is the one thing that outlives the worker.

"A NEW EXECUTION" IS MODELLED THE SAME WAY, with one difference that is the whole point: it has NO
heartbeat details. Temporal hands details to the next attempt of one activity and never across
executions, so a re-dispatch of the same Batch can only resume from what it LISTS in the store.
"""

from __future__ import annotations

import asyncio
import dataclasses

import pytest
from temporalio.exceptions import ApplicationError
from temporalio.testing import ActivityEnvironment

from kontra import ActorRegistry, MethodRegistration
from internals.engine import build_session_factory
from internals.engine import batch_id
from internals.unitstore import commit_key, commit_prefix, encode_commit
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
                            "manifest_ref": "commits/run=r/actor=t/actor_id=a1/batch=0000000000000000/"}}
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


# ---- across executions: a re-dispatch resumes from a LISTING of the batch's commits (A7) ----------


def _execution_dies_after(k, payload, store):
    """Execution 1 of a batch: commits k Units, then the worker dies and the execution ENDS (its
    retries exhausted, or the caller gave up on it). Returns the Units it ran."""
    ran = []

    async def dies(self, batch, dataset):
        async for unit in batch:
            if len(ran) == k:
                raise WorkerKilled()
            ran.append(unit.value)
            await dataset.push({"u": unit.value})

    run_batch, _ = _worker(dies, store)
    died, _ = _attempt(run_batch, payload)
    assert isinstance(died, WorkerKilled), died
    return ran


def _living(ran):
    async def lives(self, batch, dataset):
        async for unit in batch:
            ran.append(unit.value)
            await dataset.push({"u": unit.value})

    return lives


def test_a_re_dispatch_on_the_same_key_resumes_from_the_commit_objects():
    """THE cross-execution property (owner decision A7): execution 1 commits k of n and ends; the
    caller re-dispatches the SAME Batch on the SAME key — a new execution, a fresh process, NO
    heartbeat details, and a NEW node id, because `catalog.py` mints one per dispatch. It runs
    exactly the n-k Units that had not committed and returns n Units of output, none twice."""
    n, k = 6, 4
    units = [f"u{i}" for i in range(n)]
    store = FakeUnitStore()
    first = {"actor_id": "acme.com", "units": units, "method": "m", "run_id": "r", "node_id": "n-1"}
    assert _execution_dies_after(k, first, store) == units[:k]

    ran = []
    run_batch, kv = _worker(_living(ran), store)
    again = dict(first, node_id="n-2")
    out, beats = _attempt(run_batch, again)                    # attempt 1, no details

    assert ran == units[k:], "exactly the Units no earlier execution committed"
    assert out["done"] is True
    assert [r["u"] for r in _rows(out, store)] == units, "n Units of output, in input order"
    assert sorted(r["u"] for r in _store_rows(store)) == sorted(units), "no row twice in the store"
    assert beats[-1]["checkpoint"]["done"] == [[0, n - 1]], "the beat names the folded Units too"
    assert kv.d == {}, "the resume read nothing from, and wrote nothing to, the state hash"


def test_a_batch_an_earlier_execution_finished_runs_nothing():
    """An identical call made again on the same instance IS a retry (`batch_id`'s rule), and every
    Unit of it already has its commit object — so nothing runs and the outputs come back."""
    units = ["a", "b", "c"]
    payload = {"actor_id": "acme.com", "units": units, "method": "m", "run_id": "r", "node_id": "n"}
    store = FakeUnitStore()
    ran1, ran2 = [], []
    run_batch, _ = _worker(_living(ran1), store)
    out1, _ = _attempt(run_batch, payload)
    run_batch, _ = _worker(_living(ran2), store)
    out2, _ = _attempt(run_batch, dict(payload, node_id="n-again"))
    assert ran1 == units and ran2 == []
    assert out2["results"] == out1["results"], "the same refs, folded back"


def test_another_instance_does_not_resume_this_ones_batch():
    """The listing is keyed by the instance. Another key (or an unkeyed dispatch, whose actor id is
    its own run and node) running the same Units must run them all: their commits are not its."""
    units = ["a", "b", "c"]
    store = FakeUnitStore()
    _execution_dies_after(2, {"actor_id": "acme.com", "units": units, "method": "m",
                              "run_id": "r", "node_id": "n"}, store)
    ran = []
    run_batch, _ = _worker(_living(ran), store)
    out, _ = _attempt(run_batch, {"actor_id": "other.org", "units": units, "method": "m",
                                  "run_id": "r", "node_id": "n"})
    assert ran == units and out["done"] is True


def test_another_run_does_not_resume_this_runs_batch():
    """`run=` scopes the listing. A Unit's output is refs into its run's `units/` prefix, so folding
    them into another run would hand that run rows its Dataset does not hold."""
    units = ["a", "b", "c"]
    store = FakeUnitStore()
    _execution_dies_after(2, {"actor_id": "acme.com", "units": units, "method": "m",
                              "run_id": "r1", "node_id": "n"}, store)
    ran = []
    run_batch, _ = _worker(_living(ran), store)
    _attempt(run_batch, {"actor_id": "acme.com", "units": units, "method": "m",
                         "run_id": "r2", "node_id": "n"})
    assert ran == units


def test_a_discarded_checkpoint_still_resumes_from_the_listing():
    """A retry whose previous beat describes ANOTHER batch (a keyed instance's last one) has nothing
    to read from the heartbeat about this one — and this batch's own commit objects are still the
    record of what finished. They are listed and folded back, not ignored with the beat."""
    n, k = 4, 2
    units = [f"u{i}" for i in range(n)]
    payload = {"actor_id": "acme.com", "units": units, "method": "m", "run_id": "r", "node_id": "n"}
    store = FakeUnitStore()
    _execution_dies_after(k, payload, store)
    stale = {"node": "n", "done": 1, "total": 1, "isolated": 0,
             "checkpoint": {"v": 1, "batch_id": "0000000000000000", "done": [[0, 0]], "failed": [],
                            "manifest_ref": ""}}
    ran = []
    run_batch, _ = _worker(_living(ran), store)
    out, _ = _attempt(run_batch, payload, attempt=2, details=[stale])
    assert ran == units[k:]
    assert [r["u"] for r in _rows(out, store)] == units


def test_a_listed_unit_the_batch_cannot_hold_fails_loudly_and_without_retry():
    """The batch id hashes its Units, so an object under its prefix naming a Unit past its length is
    not this batch's. Folding it is impossible and skipping it would hide whatever put it there:
    CommitLost, non-retryable, naming the key — and nothing runs."""
    units = ["a", "b", "c"]
    store = FakeUnitStore()
    bid = batch_id("m", units, {})
    prefix = commit_prefix("t", "r", "acme.com", bid)
    store.put_commit(commit_key(prefix, 7), encode_commit(bid, 7, []))
    ran = []
    run_batch, _ = _worker(_living(ran), store)
    failed, _ = _attempt(run_batch, {"actor_id": "acme.com", "units": units, "method": "m",
                                     "run_id": "r", "node_id": "n"})
    assert isinstance(failed, ApplicationError), failed
    assert failed.type == "CommitLost" and failed.non_retryable is True
    assert commit_key(prefix, 7) in str(failed)
    assert ran == []


def test_a_listed_object_that_names_another_unit_fails_loudly():
    """The listing says WHICH keys exist; the body's own batch and Unit are still the integrity
    check. A copy of unit 0's body at unit 1's key is refused, exactly as it is on a heartbeat
    resume."""
    units = ["a", "b", "c"]
    store = FakeUnitStore()
    bid = batch_id("m", units, {})
    prefix = commit_prefix("t", "r", "acme.com", bid)
    store.put_commit(commit_key(prefix, 1), encode_commit(bid, 0, []))
    run_batch, _ = _worker(_living([]), store)
    failed, _ = _attempt(run_batch, {"actor_id": "acme.com", "units": units, "method": "m",
                                     "run_id": "r", "node_id": "n"})
    assert isinstance(failed, ApplicationError) and failed.type == "CommitLost", failed
    assert "names unit 0, expected 1" in str(failed)
