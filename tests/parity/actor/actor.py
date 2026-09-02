"""paritygate — the actor the parity gate deploys, and it is built out of failures that happened.

NOT AN EXAMPLE. `examples/python/` is a surface authors copy from, and every Method here exists to
produce an outcome a healthy actor never should. Its whole job is to make the gate's assertions
FALSIFIABLE: a gate that only ever sees success cannot tell a working control plane from one that
lost the work and said `completed`.

FOUR METHODS, FOUR SHAPES OF ANSWER, and three of them have a receipt in this repo:

  echo     one record per Unit. The straight line — units in must equal records out.
  tally    takes what `echo` emitted. The CHAIN, Method → Method with no rows in the workflow's
           history. A `$ref` bug once isolated EVERY Unit of a chained dispatch on a fleet while
           being invisible on one box, and the run reported `completed` with an empty Dataset.
  boom     raises on every Unit, so every Unit is isolated (ADR 0023 §13/§14) and the CALL still
           returns cleanly. This is the run that lost everything and looked finished — 15,814
           targets in seven minutes.
  silent   succeeds on every Unit and pushes nothing. The CONTROL for `boom`. A negative test
           needs one: "the Dataset is empty" proves nothing unless the same path can be shown to
           fill it, and `boom` and `silent` are indistinguishable from terminal status, from the
           row count, and from the log. Only `dropped` tells them apart, which is the entire
           argument for `(results, dropped)` being a tuple you cannot step past.

STDLIB ONLY, NO NETWORK, NO CLOCK. The gate asserts exact counts, so nothing here may depend on a
target being up, a DNS answer, or how long anything took. `echo`'s output is a pure function of its
input for the same reason `record` keys a blob by the sha of its own bytes: a re-push after a
death must produce the same bytes and overwrite itself, and a timestamp in a field would fork one
record into two.

Run it the way the gate does:

    kontra deploy --actor tests/parity/actor
    kontra serve  --actor tests/parity/actor --mode docker --replicas 2
"""

from dataclasses import dataclass

from actorkit import actor


@dataclass
class Item:
    """One Unit of work. `n` is carried through untouched so the gate can assert that the Unit
    that came out is the Unit that went in, and not merely that the COUNT matched."""

    id: int
    text: str = ""
    n: int = 1


@dataclass
class Echoed:
    id: int
    text: str
    shout: str
    n: int
    #: Which worker produced this record. The gate scales to more than one replica on purpose —
    #: a Batch that only ever lands on one of them would pass every count assertion while the
    #: fan-out was broken, so the Dataset has to be able to show two.
    #:
    #: NOT NAMED `by`, which is what it was called first. A field name becomes a COLUMN name in
    #: the lake, `by` is a DuckDB keyword, and `count(DISTINCT by)` came back as
    #: `Parser Error: syntax error at or near ")"` — an error about the query, three layers away
    #: from the dataclass that chose the word. Reserved words are the actor author's problem to
    #: avoid, because nothing between here and the lake rewrites them.
    worker: str


@dataclass
class Tally:
    id: int
    chars: int
    n: int


@actor.defn
class ParityGate:
    input = Item
    output = Echoed

    @actor.load
    async def load(self):
        # The worker's identity, read where a worker knows it. os.uname().nodename inside a
        # container is the container id, which is exactly what the gate wants here: it is a
        # per-replica string, so two replicas produce two values and one produces one.
        import os

        self.worker = os.uname().nodename

    @actor.method(takes=Item, emits=Echoed)
    async def echo(self, batch, dataset):
        """One record per Unit. The gate's `units in == records out` leg."""
        async for unit in batch:
            v = unit.value
            await dataset.push(Echoed(id=v.id, text=v.text, shout=v.text.upper(), n=v.n, worker=self.worker))

    @actor.method(takes=Echoed, emits=Tally)
    async def tally(self, batch, dataset):
        """The CHAIN target: it takes what `echo` emitted.

        Nothing about this Method is interesting except WHERE ITS BATCH COMES FROM. The caller
        hands it the ref `echo` returned rather than rows, so the Units are dereferenced by the
        host; a chained dispatch that mis-passes that ref isolates every Unit at once, and the
        symptom on one machine is nothing at all.
        """
        async for unit in batch:
            v = unit.value
            await dataset.push(Tally(id=v.id, chars=len(v.shout), n=v.n))

    @actor.method(takes=Item, emits=Echoed)
    async def boom(self, batch, dataset):
        """Raise on every Unit, so every Unit is isolated and the call still returns.

        A plain exception with a live resource is `_classify`'s last branch: the Unit is recorded
        as an `exhausted` failure and the Method is re-invoked with the remainder (ADR 0023 §13).
        Nothing raises out of the call — which is the point. The caller gets
        `(results=0, dropped=N)` and a workflow that only looked at status would call this a
        success.
        """
        async for unit in batch:
            raise RuntimeError(f"paritygate: unit {unit.value.id} was told to fail")

    @actor.method(takes=Item, emits=Echoed)
    async def silent(self, batch, dataset):
        """Succeed on every Unit and push nothing — `boom`'s control.

        Identical to `boom` at every surface that is not `dropped`: same terminal status, same
        empty Dataset, same silent log.
        """
        async for unit in batch:
            _ = unit.value


if __name__ == "__main__":
    actor.serve()
