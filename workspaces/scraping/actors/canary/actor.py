"""canary — the smallest actor that exercises the streaming path, and touches nothing.

WHAT IT IS FOR. Before a campaign runs against somebody else's production estate, this proves the
chain end to end on work that cannot hurt anyone:

    @actor.method(streams=TickProgress)   the TYPE, declared by the actor that owns it
      -> await stream(TickProgress(...))  one record, on topic `canary/tick`
      -> the run's Workflow Stream        -> GET /api/runs/<run>/progress-stream -> the console

IT SENDS NO NETWORK TRAFFIC. A canary whose failure mode is "the target was slow" tells you nothing
about the thing under test. Each unit sleeps, which is the honest way to occupy wall-clock time.

TWO HOOKS, TWO JOBS. `@actor.healthcheck` answers whether the SESSION is alive and its raise ends
it — that is all it does, and it returns nothing. Progress is per METHOD, typed, and published by
`stream()`, which can end nothing. They used to be one function, and the cost of that was a
crawler whose entire operator-facing signal was `{"contexts": 2}`: two browser tabs, never which
program or which page, because the value came from a probe written to answer "reload or isolate?".
"""

import asyncio
from dataclasses import dataclass

from kontra import actor, param, stream


@dataclass
class Tick:
    """One unit of pretend work.

    `label` rides along so the beat can name what it is on, exactly as a crawler names a URL and a
    scanner names a host — the field an operator actually reads.
    """

    label: str
    seconds: float = 1.0


@dataclass
class Beat:
    """What one unit produced. Deliberately boring: the OUTPUT is not what this actor is for.

    THE FIELD IS `worker`, NOT `node`, AND THAT IS NOT A STYLE CHOICE. The framework stamps its
    own provenance columns onto every pushed record, and `node` is one of them. An output type
    that declares it collides at INSERT time, in the materializer, long after the Method returned:

        Binder Error: Duplicate column name "node" in INSERT

    which `publishBatch` then retried eight times while the run sat at RUNNING with no clue in the
    actor's log — the Method had already succeeded. Found by this canary on its first honest run,
    which is the entire reason it exists.
    """

    label: str
    slept: float
    worker: str


@dataclass
class TickProgress:
    """WHAT THIS METHOD SHOWS WHILE IT RUNS — declared here, in the actor that owns it.

    A workflow author never reads this file; they read the catalog, where this arrives beside the
    Method's `input` and `output`. That is what lets a console draw typed fields for a run whose
    actor it has never heard of.

    The vocabulary is THIS actor's. A crawler would say `url` and `contexts`, a registry monitor
    `repo` and `layers`. Nothing here is a framework word.
    """

    label: str       # the unit currently being worked
    done: int        # units this Session has finished
    slept: float     # seconds spent on THIS unit, so a stall is a number that stops climbing


@dataclass
class CanaryParams:
    # Params get descriptions and defaults for the same reason the workflow's inputs do: the
    # console renders this shape as a form, and a field with neither is a box with a label and no
    # help. See `workflows/canary/workflow.py` for the full argument.
    fail_on: str = ""      # a label to raise on, so unit isolation can be exercised deliberately
    beat_every: float = 0.25   # how often the author's own loop yields, so `at` moves visibly


@actor.defn
class Canary:
    input = Tick
    output = Beat
    params = CanaryParams

    @actor.load
    async def start(self):
        """Nothing to open. Declared anyway so the canary exercises the load/close lifecycle the
        real actors use — a canary that skipped it would pass on a runtime where load is broken."""
        self._at = ""
        self._done = 0

    @actor.healthcheck
    async def alive(self):
        """THE ACTOR OVERALL: is the Session still usable? Nothing else.

        There is no resource to lose here, so it can only say "yes" — which is the shape every
        healthcheck should have. Progress is NOT this hook's job: it is per Method, it is typed,
        and it goes through `stream()`.
        """
        return None

    @actor.close
    async def stop(self):
        self._at = ""

    @actor.method(takes=Tick, emits=Beat, streams=TickProgress)
    async def tick(self, batch, dataset):
        """Sleep once per unit, naming the unit while it sleeps.

        THE AUTHOR OWNS THE LOOP (ADR 0023 §18) and the framework commits each Unit as the loop
        moves past it — which is exactly why this canary is a loop over units rather than one long
        activity: a worker killed halfway resumes at the unit it reached, and proving that is half
        the point of running it.
        """
        import os

        # `param.get`, not `self.params` — params reach an actor through the module-level accessor
        # the runtime binds per Batch, and `batch.units` is the list the framework already coerced
        # into `Tick` (the declared `takes=`). Getting either wrong is how the first run of this
        # canary dropped every unit it was given, which is exactly the class of mistake a canary
        # is supposed to catch before a campaign does.
        fail_on = str(param.get("fail_on", "") or "")
        beat_every = float(param.get("beat_every", 0.25))
        node = os.environ.get("KONTRA_NODE", "local")
        # NO DENOMINATOR HERE, AND `batch.units` IS THE REASON. It reads like an accessor and is
        # a CONSUMING one:
        #
        #     def units(self): return [self._hand_out() for _ in range(len(self._pending))]
        #
        # so `len(batch.units)` before the loop hands out every Unit and leaves `async for`
        # nothing to iterate. MEASURED: canary-1789931619 reported 20/20 units and 0 beats in 17
        # seconds having slept for none of them — the Method body never ran once.
        #
        # `webcrawl` uses `batch.units` on purpose, to take them all and run them concurrently.
        # Mixing it with `async for` is the trap. The Batch's size belongs to the WORKFLOW, which
        # knows it without consuming anything, so it is published there and not here.
        # `async for unit in batch`, NOT `for unit in batch.units`, and `unit.value`, NOT the
        # Unit. Both were got wrong on the way here and both failed loudly, which is the entire
        # argument for having a canary:
        #
        #   for t in batch.units           -> AttributeError: 'Unit' object has no attribute 'label'
        #   for unit in batch.units        -> MissingPushKey: push() with no current Unit
        #
        # The async iteration is what makes the framework COMMIT each Unit as the loop moves past
        # it (ADR 0023 §18) and what gives `push` a Unit to attribute the record to. Reading
        # `batch.units` walks the same list with neither, so a Method that used it would look
        # correct, commit nothing, and lose its whole Batch to a retry.
        async for unit in batch:
            t: Tick = unit.value
            # NAMED BEFORE THE SLEEP, not after. A stall is when a reader needs the label, and a
            # beat that names what just FINISHED is a beat that says nothing during the wait.
            self._at = t.label
            await stream(TickProgress(label=t.label, done=self._done, slept=0.0))
            # THE LAST SEGMENT, BECAUSE THE WHOLE LABEL IS UNGUESSABLE. The workflow prefixes
            # every label with the run id (`canary-1789943883/python/unit-3`) so that two runs
            # with identical arguments cannot hash to the same Batch and replay instead of
            # working. `t.label == fail_on` therefore could never match anything a person could
            # type BEFORE the run existed — and `fail_on` is set on the launch form, which is
            # exactly then. The form's own example is "unit-3", so the documented value silently
            # did nothing and the isolation path this canary exists to exercise never ran.
            #
            # On the separator rather than a bare `endswith`: `fail_on="3"` would otherwise match
            # unit-13 and unit-23 too, turning one deliberate failure into three.
            if fail_on and (t.label == fail_on or t.label.endswith("/" + fail_on)):
                raise RuntimeError(f"canary asked to fail on {t.label!r}")
            slept = 0.0
            while slept < t.seconds:
                step = min(beat_every, t.seconds - slept)
                await asyncio.sleep(step)
                slept += step
            self._done += 1
            # AFTER, TOO — the pair is what makes a stall legible: `slept` stops climbing while
            # `label` stays put, which reads differently from a worker that has simply gone quiet.
            await stream(TickProgress(label=t.label, done=self._done, slept=round(slept, 3)))
            await dataset.push(Beat(label=t.label, slept=round(slept, 3), worker=node))


if __name__ == "__main__":
    actor.serve()
