"""resumable — a sweep that outlives Temporal's history ceiling, in two workflows (kontra#12).

═══ THE PROBLEM THIS SHOWS THE ANSWER TO ═══

Temporal TERMINATES a workflow at 51,200 history events. A Run that sweeps a large corpus writes
events per page, so a big enough corpus kills the Run before it finishes — and what dies is the
thing holding the **Fleet**.

`continue_as_new` is the answer, and using it naively does not work here: it destroys the frame, so
`async with fleet.hold(...)` unwinds, the **Lease** drops, and the **Machines** the next leg needs
are torn down between legs.

═══ THE SPLIT, AND WHY IT IS TWO WORKFLOWS ═══

    Resumable (parent)   holds the Fleet and the output Dataset; NEVER continues-as-new
      └─ ResumableSweep  pages, dispatches, continues-as-new as often as it likes

A CONTINUE-AS-NEW CHAIN IS ONE EXECUTION FROM THE PARENT'S VIEW — `ChildWorkflowExecutionStarted`
and `…Completed`, however many times the child hands over. So the parent's history is flat whatever
the corpus size, and the **Fleet** scope never unwinds mid-sweep. That is the guarantee: the Lease
cannot drop while the sweep runs, because nothing unwinds it.

═══ WHAT THIS COSTS, STATED PLAINLY ═══

It pushes a Temporal implementation detail — *why is my sweep a separate workflow?* — into the
author's file. kontra#12 proposes hiding it behind `dataset.sweep()`, and that is the better
authoring model IF it can be built soundly. It is not built, for a reason recorded on that issue:
making the **Lease** id stable across a handover without collapsing two scopes in one Run onto one
id is an open design question, and getting it wrong destroys **Machines** a live scope is using.

Until then THIS IS THE PATTERN THAT WORKS, and it needs no library code at all — both halves are
Temporal primitives the SDK already exposes.

═══ WHY THE LEASE IS SAFE ACROSS THE BOUNDARY ═══

Stated rather than assumed. The holder id is `workflow.info().workflow_id`, and a workflow id
SURVIVES continue-as-new — this repo already relies on exactly that for `dispatch_ref`, so that one
logical run writes to one `units/run=…` partition. The **Lease**'s expiry check asks Temporal whether
the holder is still RUNNING, and a continued chain reports `running` for its whole length. Both
halves already hold; the split is what stops the scope from dropping the Lease before either is
consulted.
"""

from datetime import timedelta

from temporalio import workflow

from kontra import catalog, fleet, note, partial
from kontra.fleet import docker_fleet

PROBE = ("probe", "0.1.0")


@workflow.defn
class Resumable:
    """Capacity and output. Holds what must outlive the sweep, and never hands over."""

    @workflow.run
    async def run(self, req: dict | None = None) -> dict:
        req = req or {}
        size = int(req.get("size") or 100)
        source = str(req.get("dataset") or "domains")
        order_by = str(req.get("order_by") or "domain")
        into = str(req.get("into") or "resumable_out")

        out = catalog.dataset(into)
        async with fleet.up(
            docker_fleet(machines=int(req.get("machines") or 1)),
            actor=PROBE[0],
            version=PROBE[1],
            sessions=int(req.get("sessions") or 4),
        ) as f:
            await f.ready()
            note(f"{len(f.inventory)} machine(s) polling {PROBE[0]}; sweeping {source}")

            # THE ONE STRUCTURAL LINE. The child may continue-as-new for as long as the corpus
            # takes; this scope sees a SINGLE child execution and does not unwind, so the Fleet
            # above stays up across every handover.
            #
            # THE ID IS DERIVED, NOT MINTED. `workflow.uuid4()` here would be replay-stable and
            # would also make a RETRIED parent start a SECOND sweep — and the whole point of the
            # cursor is that there is exactly one.
            tally: dict = await workflow.execute_child_workflow(
                ResumableSweep.run,
                args=[source, size, out.name, order_by],
                id=f"{workflow.info().workflow_id}-sweep",
                execution_timeout=timedelta(hours=12),
            )

        note(f"swept {tally['scanned']} row(s) into {into}")
        return {"into": into, "machines": len(f.inventory), **tally}


@workflow.defn
class ResumableSweep:
    """The sweep. Holds nothing that must survive a handover, which is what makes it free to hand
    over."""

    @workflow.run
    async def run(
        self,
        source: str,
        size: int,
        into: str,
        order_by: str,
        cursor: int = 0,
        tally: dict | None = None,
    ) -> dict:
        # COUNTERS ONLY ACROSS A HANDOVER, NEVER AN ACCUMULATED RESULT LIST. That is the
        # Batch-Iterator pattern's own rule and the reason a Batch is a ref rather than its rows:
        # carrying results across the boundary puts the whole corpus in the next leg's INPUT, which
        # is the history cost this split exists to avoid.
        t: dict = tally or {"scanned": 0, "dropped": 0}
        rows = catalog.dataset(source)
        out = catalog.dataset(into)

        # THE SESSION IS REOPENED PER LEG, and that is the design rather than a cost to apologise
        # for: losing the host fails the scope and the caller resumes from the cursor it holds. A
        # handover is that same event without the failure, and it costs one `open_session`.
        async with catalog.actor(*PROBE) as a:
            # `order_by` IS REQUIRED and it is a correctness requirement here above all: a
            # materialized dataset stamps no row id, so LIMIT/OFFSET over it has no defined order
            # and two pages may overlap or skip with nothing raising. A cursor that resumes into an
            # unordered scan is the silent-loss shape this whole file exists to avoid.
            async for batch in rows.batches(size, order_by=order_by, start=cursor):
                seen, drops = await a.check(batch, out)
                t["scanned"] += len(seen)
                t["dropped"] += len(drops)
                cursor += len(batch)

                # THE FREE SIGNAL, READ INSTEAD OF DISCARDED. The server sets it on every workflow
                # task at 4,096 events.
                #
                # CHECKED AT A PAGE BOUNDARY, not inside the fan-out: `start=` addresses a PAGE, so
                # handing over mid-page would carry a position no cursor can express and the next
                # leg would re-dispatch work it had already done.
                if workflow.info().is_continue_as_new_suggested():
                    note(f"handing over at {cursor} rows; {t['scanned']} scanned so far")
                    workflow.continue_as_new(source, size, into, order_by, cursor, t)

        if t["dropped"]:
            # A COMPLETENESS CLAIM, not progress (ADR 0050 §2). Rows the sweep could not read are
            # rows the caller's result does not contain, and a reader who missed this line would be
            # wrong about what was covered.
            partial(
                f"{t['dropped']} row(s) dropped across the sweep — {into} does not cover them",
                axis="sweep", phase="scan", dropped=t["dropped"],
            )
        return t


if __name__ == "__main__":
    catalog.serve([Resumable, ResumableSweep])
