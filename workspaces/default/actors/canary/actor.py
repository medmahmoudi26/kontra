"""canary — the first thing anybody runs on kontra, and the whole product in one screen.

WHAT A READER IS SUPPOSED TO SEE. Not "a test passed". They should watch a Fleet come into
existence, a Worker pick up work on it, a line arrive every two seconds saying what it is doing
while it is still doing it, rows land in a queryable Dataset, and the Machines go away when the
scope exits — and be able to point at the line of code responsible for each one.

    @actor.method(takes=Probe, emits=Signal)
      log.info(...)              ──► the run's log rail ────► every line carries the run id
      await dataset.push(...)    ──► the lake ──────────────► Datasets, queryable in SQL

TWO CHANNELS, NOT THREE. An earlier version of this file also published a typed `Sweep` record
through `stream()`, on its own topic, for a pane that drew it live. It is gone, and what replaced
it is the two things above: the LOG says what is happening, the DATASET TAIL says what came of it.

The reason is durability, not taste. A Workflow Stream lives in the workflow's memory and dies with
the workflow, so the pane's most common state was an EMPTY BOX — a 55-second run is already over by
the time a browser has loaded and signed in. The log survives in VictoriaLogs and the rows survive
in the lake, so the run page says the same thing five minutes later as it did live. Typed streaming
comes back when there is a durable store under it; until then a demo must not ship a pane whose
usual state is blank.

IT TOUCHES NOTHING. No network, no third party, no credential. A demo whose failure mode is "the
target was slow" teaches a first-time reader nothing about kontra, and a demo that needs an API key
is a demo most people never see. Each phase sleeps, which is the honest way to occupy wall-clock
time, and the sleep is the ONLY reason this takes any time at all.

── WHY THE CADENCE IS A DECLARED FIELD AND NOT A `sleep(2)` ────────────────────────────────────────

`every` is an author-visible parameter with a default of two seconds. Two seconds is the floor at
which a reader watching the rail always has something newer than their last glance without the rail
turning into a wall nobody reads — and it is also one row in the Dataset, so `targets x steps` is
both the lines you watch and the rows you get. A tighter loop makes those two numbers bigger and
the demo no clearer.

── THE UNIT IS THE UNIT OF FAILURE ─────────────────────────────────────────────────────────────────

One target is one Unit, and the framework commits each as the author's loop moves past it (ADR 0023
§18). A Worker killed halfway resumes at the target it reached rather than starting the sweep again.
`fail_on` exists so a reader can SEE that: name a target and it raises, that one Unit is isolated,
the Batch carries on, and the run reports the loss instead of dying. It is empty by default, because
the first run anybody sees must have nothing red in it.
"""

import asyncio
import logging
from dataclasses import dataclass

from kontra import actor, param

#: THE ACTOR'S OWN LOGGER, and the demo is incomplete without one.
#:
#: MEASURED: across five canary runs, EXACTLY ONE log line per run carried a `run_id` — the
#: engine's own `loaded resource (open #1)`. Nothing else did, because nothing else logged:
#: `logs.bind_run` stamps the identity onto records the Python logging system emits during a
#: Batch, and this actor emitted none. So the run page's log rail had one line to show at best,
#: and zero whenever that single line had not been indexed yet — which reads as "this Run logged
#: nothing" and is the exact confusion the whole logging path exists to prevent.
#:
#: A real actor narrates. This one has to as well, or the first run anybody does teaches them that
#: the rail is empty by nature.
#:
#: `kontra.*` SO IT SORTS WITH THE ENGINE'S. The name is what the rail groups and colours by, and
#: an actor inventing its own top-level namespace is one more thing a reader has to learn.
log = logging.getLogger("kontra.canary")


@dataclass
class Probe:
    """One target to sweep — the Method's `takes`.

    A dataclass and not a dict, so the catalog carries a SHAPE: the console renders a typed form
    for this without having read a line of this file, which is the property that makes an actor
    usable by somebody who cannot open its source.
    """

    target: str
    #: How many phases to spend on it. Named `steps` rather than `depth` because it is a count of
    #: things a reader will literally watch go by in the pane, not a tree depth.
    steps: int = 5


@dataclass
class Signal:
    """One row in the Dataset — the Method's `emits`.

    WHY `worker` AND NOT `node`. The framework stamps its own provenance columns onto every pushed
    record, and `node` is one of them. An output type that declares it collides at INSERT time, in
    the materializer, long after the Method returned:

        Binder Error: Duplicate column name "node" in INSERT

    — which the publisher then retried while the run sat at RUNNING with nothing in the actor's log
    to explain it. The column that names the process is `worker`, which is also the Temporal worker
    identity every log line from this Batch carries — so the rail and the Dataset join on one
    string, and "which process produced this row" is answerable from either end.
    """

    target: str
    step: int
    phase: str
    latency_ms: float
    ok: bool
    worker: str


@dataclass
class CanaryParams:
    """Knobs, with defaults and descriptions because the console renders this shape as a form."""

    #: Seconds between records. One record is one log line and one Dataset row — see the module
    #: docstring for why two seconds is the floor rather than an arbitrary pause.
    every: float = 2.0
    #: Name a target here and that ONE Unit raises, so per-unit isolation is visible rather than
    #: claimed. Empty on purpose: the first run anybody sees has nothing red in it.
    fail_on: str = ""


#: The phases a sweep goes through, in order. Named so the pane reads as a program doing something
#: rather than a counter going up — "resolve", "connect", "probe" is a story; "step 3 of 5" is not.
PHASES = ("resolve", "connect", "handshake", "probe", "settle")


def _phase(step: int) -> str:
    """Phase name for a step, wrapping if a caller asks for more steps than there are names."""
    return PHASES[step % len(PHASES)]


def _latency(target: str, step: int) -> float:
    """A plausible latency that is the SAME on every run for the same input.

    DETERMINISTIC, NOT RANDOM, and that is a demo decision rather than a purity one: two people
    running this side by side should see the same numbers, and a screenshot in the docs should
    match what the reader gets. `random` here would also make the Dataset's contents differ between
    a run and its replay, which is a bad habit to teach in the first file somebody reads.
    """
    base = sum(ord(c) for c in target) % 37
    return round(8.0 + base + step * 2.5, 1)


@actor.defn
class Canary:
    input = Probe
    output = Signal
    params = CanaryParams

    @actor.load
    async def start(self):
        """Nothing to open — declared anyway so the demo exercises the load/close lifecycle every
        real actor uses. A canary that skipped it would pass on a runtime where load is broken."""
        self._found = 0

    @actor.healthcheck
    async def alive(self):
        """IS THE SESSION STILL USABLE? Nothing else.

        There is no resource to lose here, so it can only say yes — which is the shape every
        healthcheck should have. "How far along is it" is NOT this hook's question: that is what
        the Method's own log lines say, as they happen.
        """
        return None

    @actor.close
    async def stop(self):
        self._found = 0

    @actor.method(takes=Probe, emits=Signal)
    async def sweep(self, batch, dataset):
        """Walk each target through its phases, saying so as it goes.

        `async for unit in batch`, NOT `for unit in batch.units`. The async iteration is what makes
        the framework COMMIT each Unit as the loop moves past it and what gives `push` a Unit to
        attribute the record to. `batch.units` is a CONSUMING accessor — reading it hands out every
        Unit and leaves `async for` nothing to iterate, which presents as a Method that reports
        every unit done having executed none of them.
        """
        import os

        every = float(param.get("every", 2.0))
        fail_on = str(param.get("fail_on", "") or "")
        # The Temporal worker identity of the process running this Batch. `machine.ts` writes it
        # into the Worker's environment; on a Fleet Machine it is the Droplet's name, in a
        # container it is the container's. It rides on the row so the Dataset and the log lines
        # name the same process.
        worker = os.environ.get("KONTRA_MACHINE") or os.environ.get("HOSTNAME") or "local"

        async for unit in batch:
            probe: Probe = unit.value
            steps = max(1, int(probe.steps))

            if fail_on and probe.target == fail_on:
                # VISIBLE, AND ONLY THIS UNIT. The Batch carries on, the run reports the loss, and
                # the reader sees per-unit isolation do its job rather than reading about it.
                #
                # LOGGED AT ERROR BEFORE IT RAISES, because the exception reaches the caller as a
                # failed Unit and a reader watching the rail should see the REASON there rather
                # than only the consequence on the run page.
                log.error("canary: refusing %s — fail_on names it", probe.target)
                raise RuntimeError(f"canary: refusing {probe.target} because fail_on names it")

            # ONE LINE PER TARGET, AT THE TOP. `logs.bind_run` has stamped the Run, the Worker and
            # the Temporal context onto it already, so this is what makes the run page's rail a
            # rail rather than a single engine line — see the module note on `log`.
            log.info("canary: sweeping %s — %d phase(s) at %.1fs", probe.target, steps, every)

            for step in range(steps):
                phase = _phase(step)
                latency = _latency(probe.target, step)

                # SAID BEFORE THE SLEEP, NOT AFTER, and at INFO.
                #
                # BEFORE, because a stall is exactly when a reader needs the label: a line that
                # names what just FINISHED says nothing for the two seconds somebody is staring at
                # the rail wondering whether it is stuck.
                #
                # INFO, because this IS the progress channel now. It used to be DEBUG, on the
                # grounds that the typed `Sweep` record already carried the phase for a pane and a
                # log line would be the same fact twice at the reader's default level. That
                # argument died with the stream: at DEBUG this line is invisible to everybody who
                # has not gone looking, and the rail goes silent for `steps x every` seconds per
                # target — which is the entire duration of the demo.
                #
                # The counter rides on the line rather than the row it describes: a number that
                # stops climbing is a stall, and that is the one thing a reader watching a rail is
                # actually trying to detect.
                log.info(
                    "canary: %s %s (step %d/%d, %.1f ms) — %d row(s) so far",
                    probe.target, phase, step + 1, steps, latency, self._found)

                await dataset.push(
                    Signal(
                        target=probe.target,
                        step=step + 1,
                        phase=phase,
                        latency_ms=latency,
                        ok=True,
                        worker=worker,
                    )
                )
                self._found += 1

                await asyncio.sleep(every)

            # THE RESULT, PER TARGET. A reader who missed the whole sweep still gets one line per
            # target saying it finished and how much it produced.
            log.info("canary: %s done — %d row(s) pushed", probe.target, steps)


if __name__ == "__main__":
    # THE ENTRYPOINT RUNS THIS FILE AS A SCRIPT — `python3 -u /actor/canary/actor.py` — so the
    # module has to serve itself. Without this the process defines the Actor, reaches the end of
    # the file and exits 0, the supervisor reports "a process exited — tearing down", and the
    # Warden restarts it forever. Nothing in the loop says "you forgot to call serve()".
    actor.serve()
