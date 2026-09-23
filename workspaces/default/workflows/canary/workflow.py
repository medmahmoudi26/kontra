"""canary — provision a Fleet, sweep on it, and watch it happen. The first run anybody does.

WHAT A READER SEES, IN ORDER, AND WHERE EACH ONE COMES FROM:

    a typed form              CanaryInput below — every field has a default and a description,
                              so the launch form renders help text and pre-filled values
    Machines coming up        `fleet.hold(...)`, whose scope IS the Fleet's lifetime
    a Worker taking work      `f.place(...)` then `f.ready()`
    a line every 2 seconds    the ACTOR's own logger, on the run's log rail
    rows you can SQL          `catalog.dataset("canary_signals")`
    the Machines going away   the scope exiting — a replayable step, not a line in a script
    the run's own narration   `workflow.logger`, replay-aware, carrying the run id

THE POINT IS THAT NOTHING HERE IS A MOCK. It is a real Fleet, a real Worker and a real Dataset; the
only pretend part is that the actor sleeps instead of talking to somebody else's estate, which is
the one thing a first run should NOT do.

── ONE CHANNEL FOR "WHERE IS IT", AND IT IS THE LOG ────────────────────────────────────────────────

This workflow used to also publish `progress(...)` records on their own topic, and the actor
published typed `Sweep` records on another, for a console pane that drew both. Both are gone.

A Workflow Stream lives in the workflow's MEMORY and dies with the workflow. The run finishes in
under a minute, so by the time anybody has loaded the console and signed in there is nothing left
to subscribe to — the pane's ordinary state was an empty box, which reads as a broken feature. The
log goes to VictoriaLogs and the rows go to the lake, and both are still there tomorrow. A first
run must not be the demo of a channel that is usually empty.

So: `workflow.logger` says what the RUN is doing, the actor's logger says what the SWEEP is doing,
and the Dataset says what came of it. The verbs come back when there is a durable store behind
them.

── WHY IT IS NOT FASTER THAN IT IS ─────────────────────────────────────────────────────────────────

The sweep itself is `targets x steps x every` seconds and nothing else; at the defaults that is 20
seconds of deliberate, visible work. Everything before it is the Fleet, and a Fleet is not free:

    docker    seconds — Warden containers on the Compose network
    cloud     ~50s to converge, plus cloud-init, plus the Worker's first poll

That asymmetry is the honest reason `provider` defaults to `docker`. A first run should not depend
on a cloud credential, and a reader who wants the cloud path changes one word in the form.

Run it:

    kontra deploy --actor workspaces/default/actors/canary
    kontra workflow serve workspaces/default/workflows/canary
    kontra workflow start Canary --wait
"""

from datetime import timedelta

from pydantic import Field
from temporalio import workflow
from typing_extensions import Annotated, TypedDict

from kontra import catalog, fleet
from kontra.fleet import docker_fleet, do_fleet

#: The Actor this run places and calls. One actor, deliberately: a first run should have exactly
#: one moving part to point at.
ACTOR = ("canary", "1.0.0")

#: What a run sweeps when nothing is passed. Names rather than hostnames, because nothing is
#: resolved — a reader who sees `example.com` here will reasonably assume DNS is involved.
DEFAULT_TARGETS = ["alpha", "beta"]


class CanaryInput(TypedDict, total=False):
    """EVERY FIELD CARRIES A DEFAULT AND A DESCRIPTION, and that is the whole point of the class.

    `catalog.py::workflow_descriptor` derives the shape with `TypeAdapter(tp).json_schema()`, so a
    bare `targets: list[str]` reaches the console as `{"title": "Targets", "type": "array"}` — a
    label and nothing else, which renders as a column of empty boxes with no help and no pre-filled
    values. `Annotated[..., Field(default=..., description=...)]` puts both into the JSON Schema,
    where the form renderer can use them.

    A Python `#` comment cannot do this. Comments do not exist at runtime.
    """

    targets: Annotated[list[str], Field(
        default=DEFAULT_TARGETS,
        description="What to sweep. One target is one Unit — the unit of failure and the unit of "
                    "resumption, so a Worker killed halfway resumes at the target it reached.")]
    steps: Annotated[int, Field(
        default=5,
        description="Phases per target. Each one logs a line and pushes a row, so targets x steps "
                    "is both the number of lines you will watch go by and the number of rows the "
                    "Dataset ends up with.")]
    every: Annotated[float, Field(
        default=2.0,
        description="Seconds between records. One record is one line on the run's log rail and "
                    "one row in the Dataset, so this is the pace the run reads at.")]
    machines: Annotated[int, Field(
        default=1,
        description="How many Machines the Fleet has. One is enough to prove placement; the "
                    "sweep is serial within a Session either way.")]
    sessions: Annotated[int, Field(
        default=1,
        description="Live Sessions per Machine — density, where `machines` is scale.")]
    provider: Annotated[str, Field(
        default="docker",
        description="Where the Machines land. \"docker\" is a real Fleet of Warden containers on "
                    "the Compose network and needs no credential. \"cloud\" is DigitalOcean, and "
                    "needs KONTRA_CONTROLLER to be an address a Droplet can actually reach — a "
                    "Compose service name is not one.")]
    fail_on: Annotated[str, Field(
        default="",
        description="Name a target and that ONE Unit raises, so per-unit isolation is something "
                    "you watch rather than something you are told. Empty by default: the first "
                    "run anybody sees has nothing red in it.")]
    minutes: Annotated[int, Field(
        default=10,
        description="schedule_to_close for the sweep Batch. Exists so a deliberately long "
                    "`every` cannot hang a run forever.")]


def _tag() -> str:
    """This Run's Fleet tag — short, unique per Run, and the same on every replay.

    THE TAG IS THE FLEET'S IDENTITY. `fleet.hold(..., tag=X)` joins the Fleet called X, and a
    placement is that Fleet's WHOLE desired state — so two Runs on one tag cannot both place, and
    the second is refused. The tag was the constant `"canary"`, which made "press Run twice" a
    broken state rather than a second run.

    ── IT IS NOT THE WORKFLOW ID, AND THE RULE IS WHY ──────────────────────────────────────────

    `TAG_RE` is `^[a-z][a-z0-9-]{1,15}$` — 2 to 16 characters, because the tag becomes a
    DigitalOcean tag, an inventory group and part of every Machine's name. `canary-1790186209` is
    seventeen, so passing the workflow id straight through refused every Run with

        tag 'canary-1790186209' invalid: lowercase letters, digits and dashes, 2-16 chars

    The last eleven characters of the id are its timestamp, which is what distinguishes one Run of
    this workflow from another, and `c` in front keeps the leading-letter rule. Sanitised rather
    than assumed: a workflow id may legally carry `/`, `_` and uppercase, none of which a tag may.

    DETERMINISTIC, because it is derived from the id alone. A replay computes the same string, so
    the Fleet a retry rejoins is the Fleet it held before — which is the property `fleet.hold`
    needs and a random suffix would break.
    """
    wid = workflow.info().workflow_id.lower()
    tail = "".join(c if (c.isascii() and (c.isalnum() or c == "-")) else "-" for c in wid)[-11:]
    return f"c{tail}"[:16]


def _provider(name: str, machines: int):
    """Which Fleet, as an object rather than a flag.

    AN UNRECOGNISED VALUE FALLS BACK TO `docker` RATHER THAN RAISING. A bare `raise` here is a
    workflow-TASK failure, not a workflow failure: the SDK retries the task forever, the run sits
    wedged, and a subscriber sees `Workflow Update failed` with nothing to act on. A demo must not
    wedge on a typo in its own form, so a wrong value degrades to the safe path and says so.
    """
    if name == "cloud":
        return do_fleet(machines=machines)
    if name != "docker":
        workflow.logger.warning(
            "canary: provider %r is not \"docker\" or \"cloud\" — using docker", name)
    return docker_fleet(machines=machines)


@workflow.defn
class Canary:
    """Provisions a Fleet, sweeps on it, and lets you watch every part of it happen.

    THE FIRST PARAGRAPH OF THIS DOCSTRING IS THE WORKFLOW'S DESCRIPTION, everywhere. `catalog.py`
    derives it with `first_paragraph(cls.__doc__)` and publishes it on the descriptor beside
    `input` and `output`, so it is what the console's launch form prints above the fields and what
    `kontra workflow ls` prints beside the name. A `@workflow.defn` class with no docstring reaches
    every reader as a bare type name — which is the state this one was in.

    It is the CLASS's docstring and not `run`'s, because `run`'s belongs to the signature: it is
    where the argument's defaulting is explained, and that is a note for somebody editing this file
    rather than for somebody deciding whether to press Run.
    """

    @workflow.run
    async def run(self, req: CanaryInput | None = None) -> dict:
        """`req` DEFAULTS, because `kontra workflow start` with no `--input` passes no argument at
        all, and a required parameter makes that a workflow-task failure:

            TypeError: Canary.run() missing 1 required positional argument: 'req'

        `Field(default=...)` populates the console's FORM; it does not make the argument optional
        at the call boundary. A canary has to be runnable with no arguments or it is not a canary.
        """
        req = req or {}
        targets = [str(t) for t in (req.get("targets") or DEFAULT_TARGETS)]
        steps = max(1, int(req.get("steps") or 5))
        every = float(req.get("every") or 2.0)
        machines = max(1, int(req.get("machines") or 1))
        sessions = max(1, int(req.get("sessions") or 1))
        provider = str(req.get("provider") or "docker")
        fail_on = str(req.get("fail_on") or "")

        out = catalog.dataset("canary_signals")
        units = [{"target": t, "steps": steps} for t in targets]
        total = len(targets) * steps

        # THE DENOMINATOR FIRST, BEFORE ANYTHING HAPPENS. A reader who opens the rail at second
        # zero should already know how big this run is, so the lines that follow mean something.
        # It is one line rather than the two it used to be: the same fact printed twice, once with
        # `%s` and once with an f-string, is how a rail teaches people to skim past it.
        workflow.logger.info(
            "canary: %d target(s) x %d step(s) = %d record(s), on %d %s machine(s)",
            len(targets), steps, total, machines, provider)
        workflow.logger.info("canary: bringing the Fleet up")

        rows: list = []
        voided = ""
        # THE SCOPE IS THE FLEET'S LIFETIME. Exiting it drops the Lease and the Machines die when
        # the last one goes — and it is a REPLAYABLE step in a durable program rather than a line
        # in a script that might not run. That is the whole reason a run that provisions anything
        # is a workflow and not a shell script: a script that dies leaves the Machines standing.
        # ── ONE FLEET PER RUN, AND THE TAG IS WHAT MAKES IT ONE ─────────────────────────────────
        #
        # The tag was the constant `"canary"`, so every Run of this workflow held the SAME Fleet.
        # A placement is the WHOLE Fleet's desired state, so the second Run's `place()` is refused
        # — converging it would delete the first Run's placement and stop its Workers.
        #
        # MEASURED, twice, and neither failure mode is acceptable in the first thing anybody runs:
        #
        #   press Run twice      the second Run is refused at `place()` and WEDGES. The refusal is
        #                        a plain RuntimeError in workflow code, which Temporal retries as a
        #                        workflow TASK for ever — 301 seconds at RUNNING with an empty run
        #                        page, against 69 seconds for the Run that won.
        #   skip place() when
        #   the Fleet is shared  wrong for the same race. `shared` means another Run holds a LEASE,
        #                        not that it has PLACED — during a race neither has, so `ready()`
        #                        then fails with "0 Machine(s) with nothing on them".
        #
        # `place()`'s own message names both fixes, and the second is the one a canary wants: hold
        # a Fleet under a tag nobody else is using. Two Runs then provision two Machines instead of
        # contending for one, which for a demo with `machines=1` is exactly right — a canary should
        # be self-contained, and "what happens if I press it twice" should not be a question.
        #
        # NOTHING HERE TOUCHES RETRY SEMANTICS. The wedge is real and it belongs to the platform,
        # not to this file: an author error raised inside a workflow retries the task for ever by
        # design, because that is what lets a human fix the code and have the run resume. This
        # workflow simply stops making a call that cannot succeed.
        async with fleet.hold(_provider(provider, machines), tag=_tag()) as f:
            workflow.logger.info("canary: fleet held — placing %s@%s", *ACTOR)
            await f.place(ACTOR[0], ACTOR[1], sessions=sessions)
            # `place` returns while systemd (or the container) is still starting. A Batch
            # dispatched into that gap waits on a queue nobody is serving, which is
            # indistinguishable from a hung run — so the readiness gate is not optional.
            await f.ready()

            # THE HANDOVER, NAMED. Every line after this one and before "sweep finished" comes
            # from the ACTOR, on a Machine, and carries that Worker's identity — so a reader who
            # sees the rail go quiet knows exactly which process went quiet.
            workflow.logger.info(
                "canary: worker ready — handing %d unit(s) to %s@%s, a line every %.1fs",
                len(units), *ACTOR, every)

            async with catalog.actor(*ACTOR) as c:
                try:
                    rows, _ = await c.sweep(
                        units, out,
                        params={"every": every, "fail_on": fail_on},
                        schedule_to_close_timeout=timedelta(minutes=int(req.get("minutes") or 10)),
                    )
                except Exception as exc:  # noqa: BLE001 - the reason is the announcement
                    # A VOIDED BATCH IS NOT A BATCH THAT FOUND NOTHING, and this demo exists to
                    # make that distinction visible rather than to hide it behind a zero.
                    #
                    # ERROR, ONCE. It used to be logged twice — a WARNING carrying the structured
                    # fields and an ERROR carrying the sentence — which put the same failure on the
                    # rail at two levels, so a reader filtering to errors saw half of it and a
                    # reader at warning saw it twice. The fields ride on the ERROR.
                    voided = repr(exc)
                    workflow.logger.error(
                        "canary: sweep voided — %s", voided,
                        extra={"incomplete": True, "axis": "targets", "phase": "sweep"})

            workflow.logger.info(
                "canary: sweep finished — %d of %d row(s) into %s", len(rows), total, out.name)

        # The scope has exited here, which means the Lease is dropped and the Machines are gone.
        workflow.logger.info("canary: fleet released — %d machine(s) destroyed", machines)

        complete = not voided and len(rows) == total
        if not complete:
            # ADR 0050 §2: RAISE THE LEVEL WHEN THE RESULT IS NOT WHAT A READER WOULD ASSUME.
            # `incomplete=true` is what makes this findable across every Run rather than only by
            # somebody already reading this one.
            workflow.logger.warning(
                "canary: INCOMPLETE — %d of %d record(s)%s",
                len(rows), total, f"; {voided}" if voided else "",
                extra={"incomplete": True, "axis": "targets"})
        else:
            workflow.logger.info(
                "canary: complete — %d record(s) in %s, Machines destroyed", len(rows), out.name)

        return {
            "targets": targets,
            "steps": steps,
            "records": len(rows),
            "expected": total,
            "dataset": out.name,
            "provider": provider,
            "machines": machines,
            "complete": complete,
            **({"voided": voided} if voided else {}),
            "run": workflow.info().workflow_id,
        }


if __name__ == "__main__":
    catalog.serve([Canary])
