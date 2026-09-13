"""What a narration costs, and where it lands, against a real Temporal server.

THE ONE THING THE UNIT TESTS CANNOT SEE. `test_narration.py` asserts that `say` issues a
zero-duration timer carrying a Summary, which is a decision this SDK owns. It cannot assert that
the server ACCEPTS a zero-duration timer, that the metadata survives onto `TimerStarted` rather
than being dropped somewhere in core, or what the whole thing costs a history — and "the SDK
writes a Summary nothing can read" is precisely the failure that would pass every test next door.

MARKED `e2e`, so it is out of the default run (`-m 'not e2e'`). It starts an ephemeral dev server
from the Temporal CLI, in memory, with no namespace, no worker fleet and no state left behind.

MEASURED 2026-08-25, Temporal CLI 1.7.1 / Server 1.31.0, temporalio 1.30.0: a workflow that says
nothing is 5 events; each sentence adds exactly 5 more — `TimerStarted` carrying it, `TimerFired`,
and the three-event workflow task the firing wakes. That number is the whole reason
{@link kontra.narrate.MAX_SENTENCES} exists, so it is asserted rather than remembered.
"""

from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
from typing import Any, Optional

import pytest
from temporalio import workflow
from temporalio.api.enums.v1 import EventType
from temporalio.client import Client, WorkflowFailureError
from temporalio.exceptions import ApplicationError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

with workflow.unsafe.imports_passed_through():
    from kontra import narrate

pytestmark = pytest.mark.e2e

QUEUE = "narrate-live"


def cli() -> Optional[str]:
    """The CLI the ephemeral server runs from, if one is already on the box.

    A FUNCTION, NOT A CONSTANT, because this module is re-imported inside the workflow sandbox to
    validate the definitions below — and `Path.home()` is one of the calls the sandbox restricts.
    A module-level constant fails every test in the file with `Failed validating workflow`, which
    names nothing about the real cause.

    Whatever is cached is preferred to a download, so a box with no network still runs this suite;
    `None` lets the SDK fetch one.
    """
    override = os.environ.get("KONTRA_TEMPORAL_CLI")
    if override:
        return override
    cached = Path.home() / ".temporalio" / "bin" / "temporal"
    return str(cached) if cached.exists() else None


@workflow.defn
class Narrating:
    @workflow.run
    async def run(self, sentences: list[str]) -> str:
        for sentence in sentences:
            await narrate.say(sentence)
        return "ok"


@workflow.defn
class Silent:
    """The control. It imports `narrate` with everything else and calls nothing."""

    @workflow.run
    async def run(self) -> str:
        return "ok"


async def _events(client: Client, wid: str) -> list[Any]:
    handle = client.get_workflow_handle(wid)
    return [e async for e in handle.fetch_history_events()]


def _summary(event: Any) -> Optional[str]:
    """The sentence off one event's user metadata, or `None`.

    A SIBLING OF THE ATTRIBUTE BAG — `user_metadata` hangs off the `HistoryEvent` itself, which is
    the same shape `control/orchestrator/src/history.ts` reads on the other side. The bytes are a JSON
    string because every SDK's default converter writes a `str` that way, and that is exactly the
    case `summaryOf` decodes.
    """
    if not event.HasField("user_metadata") or not event.user_metadata.HasField("summary"):
        return None
    return json.loads(event.user_metadata.summary.data.decode())


async def _run(sentences: list[str], wid: str) -> tuple[list[Any], list[Any]]:
    async with await WorkflowEnvironment.start_local(dev_server_existing_path=cli()) as env:
        async with Worker(env.client, task_queue=QUEUE, workflows=[Narrating, Silent]):
            await env.client.execute_workflow(Narrating.run, sentences, id=wid, task_queue=QUEUE)
            await env.client.execute_workflow(Silent.run, id=f"{wid}-silent", task_queue=QUEUE)
        return await _events(env.client, wid), await _events(env.client, f"{wid}-silent")


def test_a_sentence_lands_on_the_timer_that_carried_it_and_costs_five_events() -> None:
    """THE WHOLE CLAIM, against the server that has to honour it: a zero-duration timer is
    accepted, it fires, its user metadata survives onto `TimerStarted`, and a workflow that
    narrates nothing pays nothing for the surface being available."""
    said = ["119 apexes in scope, 37k subs expected", "first pass done — retrying the 41 drops"]
    told, quiet = asyncio.run(_run(said, "narrate-live-1"))

    # The sentences, in order, off the events that carried them.
    started = [e for e in told if e.event_type == EventType.EVENT_TYPE_TIMER_STARTED]
    assert [_summary(e) for e in started] == said
    # ZERO DURATION. The wait is not the point; the event is.
    assert all(e.timer_started_event_attributes.start_to_fire_timeout.ToNanoseconds() == 0 for e in started)
    # And every one of them fired, so nothing is left dangling on a run that ends after speaking.
    assert len([e for e in told if e.event_type == EventType.EVENT_TYPE_TIMER_FIRED]) == len(said)

    # THE PRICE, MEASURED. Five events a sentence is what `MAX_SENTENCES` is reasoned from, and a
    # change to either number without the other is the drift this asserts against.
    assert len(told) - len(quiet) == 5 * len(said)
    # A WORKFLOW THAT NARRATES NOTHING IS UNCHANGED: start, one workflow task, completion.
    assert len(quiet) == 5
    assert not any(_summary(e) for e in quiet)


def test_the_budget_bounds_what_a_tight_loop_can_do_to_a_history() -> None:
    """A `say` per Unit, against the real thing. The run SURVIVES — narration is decoration, and a
    run that died at hour six because its author was too talkative is a worse outcome than a
    long transcript — and the events it added are bounded by the budget, with the last sentence
    saying so in the transcript rather than in a log nobody is reading."""
    n = narrate.MAX_SENTENCES + 25
    told, quiet = asyncio.run(_run([f"unit {i} dispatched" for i in range(n)], "narrate-live-2"))

    sentences = [_summary(e) for e in told if e.event_type == EventType.EVENT_TYPE_TIMER_STARTED]
    assert len(sentences) == narrate.MAX_SENTENCES + 1
    assert "narration budget spent" in sentences[-1]
    assert len(told) - len(quiet) == 5 * (narrate.MAX_SENTENCES + 1)


def test_an_over_long_sentence_fails_the_run_rather_than_spinning_its_task() -> None:
    """THE FAILURE MODE A UNIT TEST CANNOT TELL APART. A plain exception inside a workflow fails
    the workflow TASK and retries it forever, so the run would report `running` with nothing moving
    and this test would hang rather than fail. It returns, with a non-retryable application error
    naming the rule — which is what makes the bound something an author finds out about."""

    async def scenario() -> ApplicationError:
        async with await WorkflowEnvironment.start_local(dev_server_existing_path=cli()) as env:
            async with Worker(env.client, task_queue=QUEUE, workflows=[Narrating, Silent]):
                with pytest.raises(WorkflowFailureError) as caught:
                    await asyncio.wait_for(
                        env.client.execute_workflow(
                            Narrating.run,
                            ["x" * (narrate.MAX_SENTENCE_BYTES + 1)],
                            id="narrate-live-3",
                            task_queue=QUEUE,
                        ),
                        timeout=60,
                    )
                return caught.value.cause  # type: ignore[return-value]

    cause = asyncio.run(scenario())
    assert isinstance(cause, ApplicationError)
    assert cause.type == "NarrationRefused"
    assert cause.non_retryable is True
    assert str(narrate.MAX_SENTENCE_BYTES) in str(cause)
