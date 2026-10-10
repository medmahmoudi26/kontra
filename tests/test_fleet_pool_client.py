"""`fleet.hold(profile=…)` — the Kubernetes fleet's caller (ADR 0066), against a real Temporal test
server with the infra container's pool activities faked. What it asserts is the CONTRACT the pool
activities rely on: the activity names, the queue, the fields, and the drop on every exit."""

from __future__ import annotations

import uuid
import warnings
from datetime import timedelta

import pytest
from temporalio import activity, workflow
from temporalio.exceptions import ApplicationError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from kontra import fleet

PLACEMENT_QUEUE = "kontra-placement"


@workflow.defn(sandboxed=False)
class HoldsAndPlaces:
    @workflow.run
    async def run(self, fail: bool) -> dict:
        async with fleet.hold(profile="do-fra") as f:
            placed = await f.place("enrich", "0.3.0", replicas=4)
            if fail:
                # An APPLICATION error fails the run; a bare exception would fail only the
                # workflow task, which Temporal retries until the run times out.
                raise ApplicationError("the body failed", non_retryable=True)
            return {"profile": f.profile, "nodes": f.nodes, "placed": placed, "lease": f.lease}


@workflow.defn(sandboxed=False)
class HoldsTheDefault:
    @workflow.run
    async def run(self) -> str:
        async with fleet.hold() as f:
            return f.profile


def _fakes(calls: list):
    @activity.defn(name="holdFleetPool")
    async def hold(arg: dict) -> dict:
        calls.append(("hold", arg))
        return {"profile": arg["profile"] or "local", "holds": 1, "nodes": 3}

    @activity.defn(name="placeOnFleetPool")
    async def place(arg: dict) -> dict:
        calls.append(("place", arg))
        return {"deployment": "enrich-0-3-0-abcd1234", "image": "r/enrich@sha256:" + "a" * 64, "replicas": arg["replicas"]}

    @activity.defn(name="dropFleetPool")
    async def drop(arg: dict) -> dict:
        calls.append(("drop", arg))
        return {"delivered": True}

    return [hold, place, drop]


def _run(wf, *args, calls):
    """One workflow run on a fresh time-skipping server (no pytest-asyncio in this repo)."""
    import asyncio

    async def go():
        wq = f"wf-{uuid.uuid4().hex[:8]}"
        async with await WorkflowEnvironment.start_time_skipping() as env:
            async with Worker(env.client, task_queue=PLACEMENT_QUEUE, activities=_fakes(calls)), \
                    Worker(env.client, task_queue=wq, workflows=[wf]):
                return await env.client.execute_workflow(
                    wf.run, *args, id=f"run-{uuid.uuid4().hex[:8]}", task_queue=wq,
                    execution_timeout=timedelta(minutes=5),
                )

    return asyncio.run(go())


def test_hold_place_and_drop_reach_the_pool_with_the_tenant_and_the_lease():
    calls: list = []
    out = _run(HoldsAndPlaces, False, calls=calls)
    kinds = [c[0] for c in calls]
    assert kinds == ["hold", "place", "drop"]
    hold, place, drop = (c[1] for c in calls)
    assert hold["profile"] == "do-fra" and hold["holderNamespace"] == "default"
    assert hold["lease"].startswith(hold["holder"] + "#")
    # The placement carries the tenant (the caller's namespace) and the same lease.
    assert place == {"profile": "do-fra", "lease": hold["lease"], "namespace": "default",
                     "actor": "enrich", "version": "0.3.0", "replicas": 4}
    assert drop == {"profile": "do-fra", "lease": hold["lease"]}
    assert out["nodes"] == 3 and out["lease"] == hold["lease"]


def test_a_failing_body_still_drops_the_hold():
    calls: list = []
    with pytest.raises(Exception):
        _run(HoldsAndPlaces, True, calls=calls)
    assert [c[0] for c in calls] == ["hold", "place", "drop"]


def test_no_profile_means_kontra_yamls_default_resolved_by_the_pool():
    calls: list = []
    assert _run(HoldsTheDefault, calls=calls) == "local"
    assert calls[0][1]["profile"] == ""
    # And the drop names the RESOLVED profile, so it reaches the pool that holds it.
    assert calls[-1] == ("drop", {"profile": "local", "lease": calls[0][1]["lease"]})


def test_the_warden_surface_still_works_and_says_it_is_going():
    with pytest.warns(DeprecationWarning, match="ADR 0066"):
        fleet.hold(tag="dns", machines=2)


def test_the_two_surfaces_do_not_mix():
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)
        with pytest.raises(ValueError, match="cannot be mixed"):
            fleet.hold(tag="dns", profile="do-fra")


def test_nodes_and_size_live_on_the_profile_for_now():
    with pytest.raises(ValueError, match="kontra.yaml"):
        fleet.hold(profile="do-fra", nodes=4)


def test_replicas_must_be_a_positive_whole_number():
    import asyncio

    f = fleet.PoolFleet("do-fra")
    f._lease = "x#1"
    with pytest.raises(ValueError, match="replicas"):
        asyncio.run(f.place("enrich", "0.3.0", replicas=0))
