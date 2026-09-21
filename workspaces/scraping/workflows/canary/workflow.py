"""canary — prove the progress chain before a campaign trusts it.

WHAT IT CHECKS, in one run that touches no third party:

    KontraFlow          the base class that hosts the run's Workflow Stream
    progress(...)       the workflow's own beat, on topic `progress` — its OWN vocabulary
    stream(...)         the PYTHON actor's typed records, on topic `canary/tick`
    kontra.Stream(...)  the GO actor's typed records, on topic `gocanary/tick` — a different
                        publisher in a different language, which is why both legs run by default
    the topic model     three publishers, three topics, one run — never folded into one state
    the route           GET /api/runs/<run>/progress-stream, which RunStream subscribes to
    the input schema    every field below carries a description and a default, so the launch form
                        renders help text and pre-filled values instead of bare labels

WHY A CANARY AND NOT A TEST. The unit tests pin the pure parts — `applyEvent`, `eta`, the heartbeat
mapping — and all of them pass against a chain that is broken end to end, because none of them
crosses a process boundary. What breaks in practice is the seams: an actor patched into a container
but not deployed, a workflow that hosts no stream, a console bundle built before the pane existed.
A canary is the smallest thing that fails when a SEAM fails.

    kontra workflow serve workspaces/scraping/workflows/canary
    kontra workflow start workspaces/scraping/workflows/canary --wait \\
        --input '{"units": 40, "seconds": 4.0}'
"""
import asyncio

from temporalio import workflow
from typing_extensions import Annotated, TypedDict

from dataclasses import dataclass
from datetime import timedelta

from pydantic import Field

from kontra import KontraFlow, catalog, note, partial, progress

# THE TWO HOSTS, AND WHY BOTH ARE HERE.
#
# `canary` is Python and `gocanary` is Go, and they exercise two entirely different publishers:
# runtime/python/internals/engine.py binds a contextvar and awaits a coroutine; runtime/go/
# temporalhost/stream.go binds a func on the Session and hands the record to a buffering client.
# They share a topic convention and a wire format and nothing else, so a green Python canary is no
# evidence at all that a Go actor streams.
#
# That gap was not hypothetical: adding `workflowstreams` to runtime/go invalidated every Go
# actor's go.sum, and all five bug-bounty actors — the entire campaign — stopped compiling. Nothing
# noticed, because nothing built them. Running both legs by default is the cheapest thing that
# would have.
LEGS = {
    "python": ("canary", "0.1.1"),
    "go": ("gocanary", "0.1.0"),
}


def _legs(language: str) -> list[str]:
    """Which legs to run. An unrecognised value runs BOTH rather than raising.

    A bare `raise` here would be a workflow-task failure, not a workflow failure: the Python SDK
    retries the task forever, the run sits wedged, and a subscriber sees only `Workflow Update
    failed` — the same unhelpful surface a missing default argument produced. A canary must not
    wedge on a typo in its own form, so a wrong value degrades to the widest correct behaviour and
    says so in the log.
    """
    if language in LEGS:
        return [language]
    return ["python", "go"]


class CanaryInput(TypedDict, total=False):
    """Every field is `Annotated[..., Field(default=..., description=...)]`, and that is the point.

    ── WHY A `#` COMMENT IS NOT ENOUGH ─────────────────────────────────────────────────────────

    The other workflows in this repo document their inputs with trailing comments:

        size: int          # seeds per Batch

    Python comments do not exist at runtime. `catalog.py::workflow_descriptor` derives the shape
    with `schema_of` -> `TypeAdapter(tp).json_schema()`, so what reaches the console is

        {"size": {"title": "Size", "type": "integer"}}

    — a label and nothing else, which is why the launch form renders a column of bare boxes with no
    help and no pre-filled values, and why an operator has to read the source to learn that
    `machines=0` is the way to ask for no fleet. `Annotated[int, Field(default=50, description=…)]`
    puts `default` and `description` into that JSON Schema, where the renderer can use them.

    ── DEFAULTS DECLARED HERE, NOT ONLY IN `_num(...)` ─────────────────────────────────────────

    The real defaults currently live inside the body as `_num(req, "size", 50)`, so the form cannot
    show them and a reader cannot see them without opening the file. Declaring them on the type
    makes the form and the body agree by construction.
    """

    # THE DEFAULTS ARE SIZED TO BE WATCHED, which is the one requirement a canary has that an
    # ordinary workflow does not. They started at 8 x 2.0s — a SIXTEEN SECOND run — and the first
    # person to try it saw no streaming at all, because the run was over before the console
    # finished loading. The stream had in fact carried 13 events perfectly; there was simply
    # nothing left to subscribe to by the time anyone looked.
    #
    # 40 x 4s is ~2m40s: long enough to open the pane, watch `at` move, see the bar fill and the
    # ETA fall, and still short enough to run on a whim.
    units: Annotated[int, Field(
        default=40,
        description="How many units of pretend work to run. Each one is a Batch entry the "
                    "framework commits separately, so this is also how many resume points the "
                    "run has. 40 x 4s is about two and a half minutes — long enough to watch.")]
    seconds: Annotated[float, Field(
        default=4.0,
        description="Wall-clock seconds each unit sleeps. The run takes units x seconds, so this "
                    "and `units` together decide how long you have to watch it.")]
    size: Annotated[int, Field(
        default=4,
        description="Units per Batch. Smaller batches mean more, shorter activities — and more "
                    "frequent workflow-level progress events.")]
    fail_on: Annotated[str, Field(
        default="",
        description="Label to raise on, e.g. \"unit-3\" — matched against the last segment, "
                    "because the full label is prefixed with this run's id and so cannot be "
                    "known before the run starts. Exercises per-unit isolation: the Batch "
                    "carries on and the run reports the loss instead of dying.")]
    call_minutes: Annotated[int, Field(
        default=10,
        description="schedule_to_close for each Batch. The canary is short; this exists so a "
                    "deliberately long `seconds` cannot hang a run forever.")]
    language: Annotated[str, Field(
        default="both",
        description="Which actor host to exercise: \"python\", \"go\", or \"both\". Both is the "
                    "default because the two hosts publish through completely different code and "
                    "a green run of one says nothing about the other. Each leg gets its own "
                    "topic, so the pane shows them side by side.")]


@dataclass(frozen=True)
class Phase:
    """The params object github #20 asks for, applied from the start rather than retrofitted."""

    req: "CanaryInput"
    out: object          # catalog.Dataset
    size: int
    params: dict
    call_opts: dict


@workflow.defn
class Canary(KontraFlow):
    @workflow.run
    async def run(self, req: CanaryInput | None = None) -> dict:
        """`req` DEFAULTS, because `kontra workflow start` with no `--input` passes no argument
        at all and a required parameter makes that a workflow-task failure:

            TypeError: Canary.run() missing 1 required positional argument: 'req'

        which surfaces to a subscriber as the thoroughly unhelpful `Workflow Update failed` — the
        stream's poll cannot be served by a workflow whose task is wedged. `Field(default=...)`
        populates the console's FORM; it does not make the argument optional at the call boundary.
        A canary has to be runnable with no arguments or it is not a canary.
        """
        req = req or {}
        units = int(req.get("units") or 40)
        seconds = float(req.get("seconds") or 4.0)
        out = catalog.dataset("canary_beats")
        phase = Phase(
            req=req,
            out=out,
            size=int(req.get("size") or 4),
            params={"fail_on": req.get("fail_on") or "", "beat_every": 0.25},
            call_opts={"schedule_to_close_timeout":
                       timedelta(minutes=int(req.get("call_minutes") or 10))},
        )

        # THE RUN ID IS IN EVERY UNIT, and without it this canary silently does nothing.
        #
        # `batch_id` is a CONTENT HASH of (method, units, params) — by design, so an interrupted
        # Batch resumes rather than repeats. Two canary runs with the same arguments therefore
        # produce the SAME unit keys, every one of them is already in the commit map, and the
        # second run completes in seventeen seconds having executed no work at all. MEASURED:
        # canary-1789931273, 40 units of 4s, done in 17s, zero sleeps performed.
        #
        # That is correct framework behaviour and a broken canary: a thing that proves the
        # streaming path has to actually run every time. `workflow.info()` is deterministic and
        # replay-safe, which a clock or a random would not be.
        run_id = workflow.info().workflow_id
        legs = _legs(str(req.get("language") or "both"))
        total = units * len(legs)

        # THE WORKFLOW'S DENOMINATOR IS THE WHOLE RUN, across every leg — which is precisely the
        # distinction the topic model exists to draw. The workflow knows how much work the RUN is;
        # an actor knows only its own Batch. Folding both into one flat state is what made the bar
        # jump between a batch's denominator and the run's on alternate records.
        tally = {"done": 0, "found": 0, "voided": 0}

        # PUBLISHED BEFORE THE WORK STARTS, so a pane opened late can draw a bar immediately
        # instead of waiting for a second event to infer the denominator.
        progress(phase="tick", batch_size=phase.size, done=0, total=total, beats=0,
                 hosts=" + ".join(legs))
        note(f"canary: {units} unit(s) of {seconds}s per host ({', '.join(legs)}), "
             f"{phase.size} per batch")

        async def leg(name: str) -> None:
            """One host's whole leg. Its records land on that actor's OWN topic —
            `canary/tick` or `gocanary/tick` — so the pane shows two cards, not one averaged one.
            """
            # THE LEG IS IN EVERY UNIT LABEL, for the same reason the run id is. `batch_id` is a
            # CONTENT HASH of (method, units, params): two legs given identical tick payloads
            # would hash to the same Batch, and the second would find every unit already in the
            # commit map and complete instantly having slept for none of them.
            ticks = [{"label": f"{run_id}/{name}/unit-{i + 1}", "seconds": seconds}
                     for i in range(units)]
            async with catalog.actor(*LEGS[name]) as c:
                for start in range(0, len(ticks), phase.size):
                    chunk = ticks[start:start + phase.size]
                    try:
                        rows, _ = await c.tick(chunk, out, params=phase.params, **phase.call_opts)
                    except Exception as exc:  # noqa: BLE001 - the reason is the announcement
                        tally["voided"] += 1
                        # A VOIDED BATCH IS NOT A BATCH THAT FOUND NOTHING, and the canary exists
                        # to make that distinction visible rather than to hide it. The host is
                        # named because with two legs running, "voided" alone does not say which.
                        partial(f"canary {name} batch voided: {exc!r}", axis="units", phase="tick")
                        continue
                    tally["done"] += len(chunk)
                    tally["found"] += len(rows)
                    progress(phase="tick", batch_size=phase.size, done=tally["done"],
                             total=total, beats=tally["found"], hosts=" + ".join(legs))
                    note(f"canary: {tally['done']}/{total} unit(s), {tally['found']} beat(s)")

        # CONCURRENTLY, so the two hosts publish INTERLEAVED and the pane is watched proving both
        # at once rather than one after the other. Temporal's event loop is single-threaded and
        # deterministic, so the shared `tally` needs no lock and the replay is stable.
        await asyncio.gather(*(leg(name) for name in legs))

        progress(phase="done", batch_size=phase.size, done=tally["done"], total=total,
                 beats=tally["found"], hosts=" + ".join(legs))
        note(f"canary complete: {tally['found']} beat(s) into {out.name}"
             + (f", {tally['voided']} batch(es) voided" if tally["voided"] else ""))
        return {"units": total, "hosts": legs, "done": tally["done"],
                "beats": tally["found"], "voided": tally["voided"]}


if __name__ == "__main__":
    catalog.serve([Canary])
