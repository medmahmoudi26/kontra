"""canary — provision a Fleet, sweep on it, and watch it happen. The first run anybody does.

WHAT A READER SEES, IN ORDER, AND WHERE EACH ONE COMES FROM:

    a typed form              CanaryInput below — every field has a default and a description,
                              so the launch form renders help text and pre-filled values
    Machines coming up        `fleet.hold(...)`, whose scope IS the Fleet's lifetime
    a Worker taking work      `f.place(...)` then `f.ready()`
    typed records, every 2s   the ACTOR's `stream(Sweep(...))`, on topic `canary/sweep`
    a bar that means it       this workflow's own `progress(...)`, on topic `progress`
    rows you can SQL          `catalog.dataset("canary_signals")`
    the Machines going away   the scope exiting — a replayable step, not a line in a script
    log lines throughout      `workflow.logger`, replay-aware, carrying the run id

THE POINT IS THAT NOTHING HERE IS A MOCK. It is a real Fleet, a real Worker, a real Dataset and a
real stream; the only pretend part is that the actor sleeps instead of talking to somebody else's
estate, which is the one thing a first run should NOT do.

── THREE PUBLISHERS, THREE TOPICS, ONE RUN ─────────────────────────────────────────────────────────

`progress(...)` is the WORKFLOW's vocabulary and knows the whole run's denominator. `stream(...)`
inside the actor is the METHOD's, and knows only its own Batch. They are deliberately not folded
together: a single flat state is what makes a bar jump between a batch's denominator and a run's on
alternate records. The console draws them as separate cards because they answer different questions.

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

from kontra import KontraFlow, catalog, fleet, progress
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
        description="Phases per target. Each one publishes a typed record and pushes a row, so "
                    "targets x steps is both the number of records you will watch and the number "
                    "of rows the Dataset ends up with.")]
    every: Annotated[float, Field(
        default=2.0,
        description="Seconds between records. 2.0 is the Workflow Stream's own flush interval — "
                    "anything smaller buys latency nobody can see and costs Signals nobody wanted.")]
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
class Canary(KontraFlow):
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

        workflow.logger.info(
            "canary: %d target(s) x %d step(s) = %d record(s), %d %s machine(s)",
            len(targets), steps, total, machines, provider)

        # PUBLISHED BEFORE ANYTHING HAPPENS, so a pane opened at the very start draws a bar
        # immediately instead of waiting for a second event to infer the denominator.
        progress(phase="fleet", done=0, total=total, machines=machines,
                 where=provider, note="bringing the Fleet up")
        workflow.logger.info(
            f"canary: {len(targets)} target(s), {steps} step(s) each, on {machines} "
            f"{provider} machine(s)")

        rows: list = []
        voided = ""
        # THE SCOPE IS THE FLEET'S LIFETIME. Exiting it drops the Lease and the Machines die when
        # the last one goes — and it is a REPLAYABLE step in a durable program rather than a line
        # in a script that might not run. That is the whole reason a run that provisions anything
        # is a workflow and not a shell script: a script that dies leaves the Machines standing.
        async with fleet.hold(_provider(provider, machines), tag="canary") as f:
            workflow.logger.info("canary: fleet held — placing %s@%s", *ACTOR)
            progress(phase="place", done=0, total=total, machines=machines,
                     where=provider, note="placing the actor")

            await f.place(ACTOR[0], ACTOR[1], sessions=sessions)
            # `place` returns while systemd (or the container) is still starting. A Batch
            # dispatched into that gap waits on a queue nobody is serving, which is
            # indistinguishable from a hung run — so the readiness gate is not optional.
            await f.ready()

            workflow.logger.info("canary: worker ready — sweeping %d unit(s)", len(units))
            progress(phase="sweep", done=0, total=total, machines=machines,
                     where=provider, note="sweeping")

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
                    voided = repr(exc)
                    workflow.logger.warning(
                        f"canary sweep voided: {voided}",
                        extra={"incomplete": True, "axis": "targets", "phase": "sweep"})
                    workflow.logger.error("canary: sweep voided — %s", voided)

            progress(phase="sweep", done=len(rows), total=total, machines=machines,
                     where=provider, note="sweep complete")
            workflow.logger.info("canary: %d row(s) into %s", len(rows), out.name)

        # The scope has exited here, which means the Lease is dropped and the Machines are gone.
        workflow.logger.info("canary: fleet released")
        progress(phase="done", done=len(rows), total=total, machines=0,
                 where=provider, note="fleet released")

        complete = not voided and len(rows) == total
        if not complete:
            # ADR 0050 §2: RAISE THE LEVEL WHEN THE RESULT IS NOT WHAT A READER WOULD ASSUME.
            # `incomplete=true` is what makes this findable across every Run rather than only by
            # somebody already reading this one.
            workflow.logger.warning(
                "canary: INCOMPLETE — %d of %d record(s)%s",
                len(rows), total, f"; {voided}" if voided else "",
                extra={"incomplete": True, "axis": "targets"})
            workflow.logger.info(f"canary INCOMPLETE — {len(rows)} of {total} record(s)")
        else:
            workflow.logger.info(
                f"canary complete: {len(rows)} record(s) into {out.name}; Machines destroyed")

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
