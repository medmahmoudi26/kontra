"""Minimal typed actor used as a self-contained test fixture (loader / manifest / schema
derivation). Not an example — kept tiny and stable so the unit tests don't depend on the
examples/ tree, which is rebuilt independently."""

from dataclasses import dataclass

from kontra import actor


@dataclass
class EchoInput:
    msg: str
    id: int | None = None


@dataclass
class EchoOutput:
    msg: str
    shout: str
    by: str
    id: int | None = None


@actor.defn
class Echo:
    input = EchoInput
    output = EchoOutput

    @actor.load
    async def open(self):
        self.tag = "echoed"

    @actor.method()
    async def shout(self, unit):
        return [{**unit, "shout": str(unit.get("msg", "")).upper(), "by": self.tag}]


if __name__ == "__main__":
    actor.serve()
