"""One Batch through the fixture Actor, against a real control plane.

THIS IS THE LEG `scripts/parity-gate.sh` COUNTS. ADR 0031's central claim is that the single
binary really runs an actor end to end; this is the test that makes that a measurement rather
than an assertion, and the gate refuses a green run in which it did not execute.

WHY IT MOVED HERE. It used to be `examples/python/beacon/test_beacon_e2e.py`, and ADR 0038 moved
every actor to kontra-actors. The gate cannot depend on another repository's test suite, so the
smallest possible version of that leg lives here against `testdata/fixtureactor` — which exists
for exactly this reason and is not an example.

WHAT WAS LOST, said plainly rather than left to be discovered: `make test-examples-python E2E=1`
ran FOUR real actors' suites against the binary. This runs one deliberately-boring actor. It
still proves the binary serves an actor, routes a Batch to it, and returns records — which is
what ADR 0031 §5 asks for — but it no longer proves that four different authoring shapes survive
the appliance. That coverage lives in kontra-actors' CI now, against its own control plane.

    kontra up                                       # the control plane
    kontra serve --actor testdata/fixtureactor      # the Container this dispatches to
    pytest tests/test_fixture_actor_e2e.py
"""

from __future__ import annotations

import asyncio
import os
import uuid
from datetime import timedelta

import pytest
from temporalio import workflow
from temporalio.client import Client
from temporalio.worker import Worker

with workflow.unsafe.imports_passed_through():
    from kontra import catalog
    from internals import casstore

pytestmark = pytest.mark.control_plane

ACTOR = "fixtureactor"
VERSION = "0.1.0"
UNITS = int(os.environ.get("KONTRA_E2E_UNITS", "8"))

fixture = catalog.actor(ACTOR, VERSION)


@workflow.defn
class FixtureE2E:
    """One Batch through a deployed Actor, and both halves of what came back."""

    @workflow.run
    async def run(self, n: int) -> dict:
        items = [{"value": f"e2e-{i}"} for i in range(n)]
        echoed, dropped = await fixture.echo(
            items, schedule_to_close_timeout=timedelta(minutes=3)
        )
        rows = await echoed.rows()
        return {
            "units": n,
            "out": len(echoed),
            "dropped": len(dropped),
            "values": sorted(str(r["value"]) for r in rows),
        }


def address() -> str | None:
    """The control plane to dial, or None — in which case there is nothing to test here.

    A FUNCTION, NOT A MODULE CONSTANT: the variable is set by the gate that starts the binary,
    which happens after this module is imported in some orderings.
    """
    return os.environ.get("KONTRA_ADDRESS") or None


def test_the_fixture_actor_dispatches_against_a_real_control_plane():
    addr = address()
    if not addr:
        pytest.skip(
            "no KONTRA_ADDRESS — this leg needs a control plane. Start one with `kontra up` "
            "(scripts/parity-gate.sh does), then serve testdata/fixtureactor."
        )
    result = asyncio.run(_dispatch(addr))
    _receipt(f"{ACTOR}@{VERSION} -> {addr}  {result['out']}/{result['units']} out, {result['dropped']} dropped")

    # UNITS IN == RECORDS OUT, AND NOTHING DROPPED. Three assertions rather than one, because the
    # three failures they separate all render as a completed run: an empty Batch (out == 0), a
    # partial one (out < units), and one where the loss was recorded honestly and nobody looked
    # (dropped > 0).
    assert result["dropped"] == 0, f"the control plane dropped {result['dropped']} of {UNITS} Units"
    assert result["out"] == UNITS, f"{UNITS} Units in, {result['out']} records out"
    assert result["values"] == sorted(f"e2e-{i}" for i in range(UNITS)), (
        "the records that came back are not the Units that went in"
    )


def _receipt(line: str) -> None:
    """Leave proof on disk that this leg RAN, for a caller that cannot see stdout.

    `scripts/parity-gate.sh` refuses a green run in which nothing e2e actually executed — a suite
    where every such test skipped is green and means nothing, which is ADR 0031 §5's warning
    exactly. It used to count a line this test prints; pytest captures stdout and replays it only
    for FAILURES, so a passing leg left no line and the gate reported "they all skipped" about the
    run that had just proven the binary. A file append is a side effect pytest does not own.

    Best-effort and never fatal: no receipt path means nobody is counting, and a receipt that
    cannot be written must not fail an assertion that already passed.
    """
    path = os.environ.get("KONTRA_E2E_RECEIPT")
    if not path:
        return
    try:
        with open(path, "a") as fh:
            fh.write(line + "\n")
    except OSError:
        pass


async def _dispatch(addr: str) -> dict:
    # The codec, not a bare client: an actor result over 128 KiB is a claim-check ref, and a client
    # without the converter dies on `Unknown payload encoding binary/claim-check-v1` — a failure
    # that only appears once a Batch is big enough, so a demo passes and a real run does not. This
    # Batch is small; the converter is here so the test is not a special case.
    client = await Client.connect(
        addr,
        namespace=os.environ.get("KONTRA_NAMESPACE", "default"),
        data_converter=casstore.data_converter(),
    )
    # A QUEUE NOBODY ELSE POLLS. A uuid rather than a name, so two of these running at once (a gate
    # and a developer) cannot steal each other's workflow tasks.
    queue = f"e2e-fixture-{uuid.uuid4().hex[:12]}"
    async with Worker(client, task_queue=queue, workflows=[FixtureE2E]):
        return await client.execute_workflow(
            FixtureE2E.run,
            UNITS,
            id=f"fixture-e2e-{uuid.uuid4().hex[:12]}",
            task_queue=queue,
            # BOUNDED, AND TIGHTER THAN IT LOOKS. Every failure on this path is a WAIT: a queue
            # nobody polls, a Nexus endpoint that does not exist, an activity proxied to a role
            # that is not running. The suite has to come back with an answer.
            execution_timeout=timedelta(minutes=5),
        )
