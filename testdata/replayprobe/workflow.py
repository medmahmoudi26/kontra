"""A tiny workflow that exists to be replayed — the fixture for `kontra workflow replay`.

Two activity calls in a fixed order. Swap them and every execution already in flight fails on
replay, which is the whole class of bug the determinism guard exists to catch.
"""

from datetime import timedelta

from temporalio import activity, workflow

OPTS = {"start_to_close_timeout": timedelta(seconds=10)}


@activity.defn(name="replayprobe_first")
async def first() -> str:
    return "one"


@activity.defn(name="replayprobe_second")
async def second() -> str:
    return "two"


@workflow.defn(name="ReplayProbeFixture")
class ReplayProbeFixture:
    """first, then second."""

    @workflow.run
    async def run(self) -> str:
        a = await workflow.execute_activity("replayprobe_first", **OPTS)
        b = await workflow.execute_activity("replayprobe_second", **OPTS)
        return f"{a}-{b}"
