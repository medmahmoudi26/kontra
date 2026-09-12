"""Replay, proved end to end: run a workflow, keep its history, replay the code against it.

THE TEST THAT MATTERS IS THE SECOND ONE. A replay that passes proves very little on its own — an
empty replayer passes too. What has to be shown is that a NON-DETERMINISTIC edit is caught: reorder
two activity calls, replay the same history, and get a failure. That is the whole value of `15`'s
CI guard, demonstrated on a real history rather than asserted.

These need a Temporal to talk to and skip without one, because a test that silently passes when the
server is absent is worse than no test.
"""

from __future__ import annotations

import asyncio
import os
import uuid
from datetime import timedelta

import pytest
from temporalio import activity, workflow
from temporalio.client import Client
from temporalio.worker import Worker

from internals import casstore
from internals.replay import Outcome, replay_history

ADDRESS = os.environ.get("KONTRA_ADDRESS", "localhost:7233")


@activity.defn(name="replay_test_first")
async def first() -> str:
    return "one"


@activity.defn(name="replay_test_second")
async def second() -> str:
    return "two"


OPTS = {"start_to_close_timeout": timedelta(seconds=10)}


@workflow.defn(name="ReplayProbe")
class ReplayProbe:
    """first, then second."""

    @workflow.run
    async def run(self) -> str:
        a = await workflow.execute_activity("replay_test_first", **OPTS)
        b = await workflow.execute_activity("replay_test_second", **OPTS)
        return f"{a}-{b}"


@workflow.defn(name="ReplayProbe")
class ReplayProbeReordered:
    """THE SAME WORKFLOW TYPE, with the two calls swapped.

    This is the edit that looks harmless in review, passes every unit test, and breaks every
    execution that was already in flight when it deployed.
    """

    @workflow.run
    async def run(self) -> str:
        b = await workflow.execute_activity("replay_test_second", **OPTS)
        a = await workflow.execute_activity("replay_test_first", **OPTS)
        return f"{a}-{b}"


async def _client() -> Client:
    return await Client.connect(ADDRESS, namespace="default", data_converter=casstore.data_converter())


async def _run_and_capture() -> dict:
    """Run ReplayProbe once and return its history as JSON."""
    client = await _client()
    queue = f"replay-test-{uuid.uuid4().hex[:8]}"
    async with Worker(client, task_queue=queue, workflows=[ReplayProbe], activities=[first, second]):
        handle = await client.start_workflow(
            ReplayProbe.run, id=f"replay-probe-{uuid.uuid4().hex[:8]}", task_queue=queue
        )
        assert await handle.result() == "one-two"
    import json

    hist = await handle.fetch_history()
    return json.loads(hist.to_json())  # to_json is sync in temporalio 1.30


@pytest.fixture(scope="module")
def history() -> dict:
    """A real history, or a skip — but ONLY for a genuinely absent server.

    The first version caught every exception and skipped, which reported a real bug in this file
    (awaiting a sync `to_json`) as "needs a Temporal at localhost:7233". A fixture that turns your
    own mistakes into skips is a suite that is always green and never runs.
    """
    try:
        return asyncio.run(asyncio.wait_for(_run_and_capture(), timeout=60))
    except (OSError, ConnectionError, asyncio.TimeoutError) as exc:
        pytest.skip(f"needs a Temporal at {ADDRESS}: {exc}")
    except RuntimeError as exc:
        if "Failed client connect" in str(exc) or "tonic" in str(exc).lower():
            pytest.skip(f"needs a Temporal at {ADDRESS}: {exc}")
        raise


@pytest.mark.control_plane
def test_unchanged_code_replays_clean(history):
    result = asyncio.run(replay_history(history, [ReplayProbe]))
    assert result.outcome is Outcome.OK, result.detail
    assert result.workflow_type == "ReplayProbe"
    # Non-vacuous: a history with no events would "replay" trivially.
    assert len(history["events"]) > 3, "the captured history is too short to have exercised anything"


@pytest.mark.control_plane
def test_reordered_activities_are_caught(history):
    """The guard `15` puts in CI, demonstrated."""
    result = asyncio.run(replay_history(history, [ReplayProbeReordered]))
    assert result.outcome is Outcome.NONDETERMINISTIC, (
        "swapping two activity calls replayed clean — the determinism guard would catch nothing. "
        f"got {result.outcome} / {result.detail}"
    )
    assert result.detail, "a non-determinism failure must carry the reason"


@pytest.mark.control_plane
def test_no_workflows_is_unrunnable_not_nondeterministic(history):
    """A setup problem must not be reported as a fact about the code."""
    result = asyncio.run(replay_history(history, []))
    assert result.outcome is Outcome.UNRUNNABLE
    assert "no @workflow.defn" in result.detail


def test_unreadable_history_is_unrunnable():
    result = asyncio.run(replay_history({"nonsense": True}, [ReplayProbe]))
    assert result.outcome is Outcome.UNRUNNABLE
    assert "not readable" in result.detail or "no @workflow" in result.detail
