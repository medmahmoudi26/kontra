"""canary — the first thing anybody runs on kontra, and the whole product in one screen.

WHAT A READER IS SUPPOSED TO SEE. Not "a test passed". They should watch a Fleet come into
existence, a Worker pick up work on it, typed records arrive in a pane every two seconds while it
is still running, rows land in a queryable Dataset, and the Machines go away when the scope exits —
and be able to point at the line of code responsible for each one.

    @actor.method(takes=Probe, emits=Signal, streams=Sweep)
      await stream(Sweep(...))   ──► topic `canary/sweep` ──► the run's Workflow Stream
      await dataset.push(...)    ──► the lake ──────────────► Datasets, queryable in SQL

IT TOUCHES NOTHING. No network, no third party, no credential. A demo whose failure mode is "the
target was slow" teaches a first-time reader nothing about kontra, and a demo that needs an API key
is a demo most people never see. Each phase sleeps, which is the honest way to occupy wall-clock
time, and the sleep is the ONLY reason this takes any time at all.

── WHY THE CADENCE IS A DECLARED FIELD AND NOT A `sleep(2)` ────────────────────────────────────────

`every` is an author-visible parameter with a default of two seconds, and two seconds is not
arbitrary: it is the Workflow Stream client's own flush interval (`workflowstreams`,
`batch_interval=2s`). A record produced faster than that does not reach a reader faster — it waits
in the buffer — so a tighter loop buys latency nobody sees and costs Signals nobody wanted. Two
seconds is the floor where "every record is on screen as soon as it exists" is TRUE rather than
aspirational.

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

from kontra import actor, param, stream

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
    identity the log records and the stream records carry, so the three join on one string.
    """

    target: str
    step: int
    phase: str
    latency_ms: float
    ok: bool
    worker: str


@dataclass
class Sweep:
    """WHAT THIS METHOD SHOWS WHILE IT RUNS — declared here, in the actor that owns it.

    A workflow author never reads this file; they read the catalog, where this arrives beside the
    Method's `input` and `output`. That is what lets a console draw typed progress for a run whose
    actor it has never heard of.

    THE VOCABULARY IS THIS ACTOR'S. `target`, `phase`, `step` — not `done`/`total`/`percent`. A
    framework word here would be a framework word on every actor's pane, and the whole argument for
    typed streaming is that a crawler says `url` and a scanner says `host`.
    """

    target: str
    step: int
    of: int
    phase: str
    latency_ms: float
    #: Rows this Session has pushed so far. A number that stops climbing is a stall, which is the
    #: one thing a reader watching a pane is actually trying to detect.
    found: int


@dataclass
class CanaryParams:
    """Knobs, with defaults and descriptions because the console renders this shape as a form."""

    #: Seconds between records. The Workflow Stream flushes every 2s, so anything smaller buys
    #: latency nobody can see — see the module docstring.
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
        healthcheck should have. Progress is NOT this hook's job: it is per Method, it is typed,
        and it goes through `stream()`.
        """
        return None

    @actor.close
    async def stop(self):
        self._found = 0

    @actor.method(takes=Probe, emits=Signal, streams=Sweep)
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
        # container it is the container's. It rides on the row so the Dataset, the stream and the
        # log lines all name the same process.
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

                # SAID BEFORE THE SLEEP, NOT AFTER. A stall is exactly when a reader needs the
                # label, and a record that names what just FINISHED says nothing during the wait.
                await stream(
                    Sweep(
                        target=probe.target,
                        step=step + 1,
                        of=steps,
                        phase=phase,
                        latency_ms=latency,
                        found=self._found,
                    )
                )

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

                # DEBUG, NOT INFO, AND THE SPLIT IS ADR 0050's. The STREAM is state a machine
                # draws — it already carries this phase, typed, for the pane. A log line repeating
                # it at INFO would be the same fact twice at the reader's default level, which is
                # how a rail becomes something people stop reading. At DEBUG it is there for
                # somebody who has raised the floor because they are debugging this actor.
                log.debug("canary: %s %s (%.1f ms)", probe.target, phase, latency)

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
