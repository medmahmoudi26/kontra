"""The actor kontra's own tests dispatch to. NOT AN EXAMPLE — do not copy this.

WHY IT EXISTS. `scripts/parity-gate.sh` proves the single binary really runs an actor end to
end, and the `build-actor` CI templates build a real actor directory. Both need one to exist,
and ADR 0038 removed every other actor from this repository. So this is the smallest thing that
satisfies both, owned by the test suite rather than offered as a starting point.

WHY IT IS DELIBERATELY BORING. An example teaches; a fixture must not. It has one Method, no
dependencies beyond the SDK, no network, no state, and no interesting failure mode — because
every one of those would be a thing that could break for a reason unrelated to what the gate is
actually testing. If you want something to copy, copy from kontra-actors.

WHAT IT MUST KEEP. `echo` returns one row per Unit, which is what lets the gate assert a COUNT
rather than merely that nothing raised. A fixture that emitted nothing would pass a gate that
only checked the exit status, and the gate would then be measuring its own plumbing.
"""

from dataclasses import dataclass

from actorkit import actor


@dataclass
class In:
    value: str


@dataclass
class Out:
    value: str
    seen: str


@actor.defn
class FixtureActor:
    @actor.load
    async def open(self) -> None:
        # Named so a pane, a journal line and a failing gate all say the same word.
        self.tag = "fixture"

    @actor.method(takes=In, emits=Out)
    async def echo(self, batch, dataset) -> None:
        async for unit in batch:
            await dataset.push(Out(value=unit.value.value, seen=self.tag))

    @actor.close
    async def close(self) -> None:
        self.tag = ""


if __name__ == "__main__":
    actor.serve()
