"""paritygate — the caller workflow the parity gate runs, and the only thing it returns is counts.

WHY COUNTS AND NOT A STATUS. Every failure this gate is aimed at reported `completed`:

  · a chained dispatch isolated every Unit on a fleet, and looked fine on one box;
  · a wiped batch came back as a clean `completed` in seven minutes;
  · a node whose Units all failed reported success with empty output.

So this workflow makes four calls whose right answers are known integers, and hands them back for
the gate to compare. A run that says `completed` and returns the wrong integers fails the gate;
a run that says `completed` and returns nothing at all fails it too, because `--wait` gets no JSON.

THE FOUR CALLS ARE FOUR DIFFERENT QUESTIONS. `echo` is the straight line into a NAMED Dataset —
the one leg with something to query afterwards. `tally` is the CHAIN: its Batch is the ref `echo`
returned, never rows, so a `$ref` regression shows up here as `dropped == units` while every other
leg stays green. `boom` and `silent` are the pair that must not render identically: both return an
empty `results`, both leave an empty Dataset, both end `completed`, and only `dropped` separates
"lost everything" from "found nothing".

`(results, dropped)` is what makes that separation unmissable (ADR 0028 §4): a caller cannot bind
the survivors without naming the drops, so this file could not have been written to ignore them.

    kontra workflow serve tests/parity/workflow
    kontra workflow start tests/parity/workflow --wait --input '{"units": 12}'
"""

from __future__ import annotations

from datetime import timedelta

from temporalio import workflow

from actorkit import catalog

# Cheap, stateless and safe at module scope: nothing connects and nothing is looked up.
gate = catalog.actor("paritygate", "0.1.0")

# One timeout for every call. Generous, because the gate may be waiting on a cold image pull, and
# BOUNDED, because a gate that hangs forever is a gate nobody runs — the whole point is a command
# that comes back with an answer.
CALL_TIMEOUT = timedelta(minutes=10)


@workflow.defn
class ParityGate:
    @workflow.run
    async def run(self, req: dict | None = None) -> dict:
        req = req or {}
        units = int(req.get("units") or 12)
        name = str(req.get("dataset") or "parity-gate")

        items = [{"id": i, "text": f"u{i:04d}", "n": i} for i in range(units)]

        # 1. THE STRAIGHT LINE, into a named Dataset so there is something to query. The second
        #    positional argument is the caller naming where output goes (ADR 0028 §2); without it
        #    the results are a chainable Batch that materializes nothing, which is what leg 2 uses.
        echoed, echo_dropped = await gate.echo(
            items, catalog.dataset(name), schedule_to_close_timeout=CALL_TIMEOUT
        )

        # 2. THE CHAIN. `echoed` is a ref, not rows: nothing from leg 1 enters this workflow's
        #    history on the way to leg 2. This is the leg that was invisible locally.
        tallied, tally_dropped = await gate.tally(echoed, schedule_to_close_timeout=CALL_TIMEOUT)

        # 3. EVERY UNIT FAILS, and the call still returns. If this raises instead, the framework
        #    grew a failure policy it is not supposed to have (ADR 0023 §14) — which the gate
        #    would see as a workflow error rather than as counts, and would report as such.
        boomed, boom_dropped = await gate.boom(items, schedule_to_close_timeout=CALL_TIMEOUT)

        # 4. EVERY UNIT SUCCEEDS AND FINDS NOTHING. The control for 3.
        quiet, quiet_dropped = await gate.silent(items, schedule_to_close_timeout=CALL_TIMEOUT)

        return {
            "units": units,
            "dataset": name,
            "echo": leg(units, echoed, echo_dropped),
            "chain": leg(len(echoed), tallied, tally_dropped),
            "boom": leg(units, boomed, boom_dropped),
            "silent": leg(units, quiet, quiet_dropped),
        }


def leg(units: int, results, dropped) -> dict:
    """One call's three numbers.

    `len()` on either side costs no fetch — the count rides on the ref's meta — so a gate can
    assert on all four legs without materializing a single row into workflow history.
    """
    return {"units": units, "out": len(results), "dropped": len(dropped)}


if __name__ == "__main__":
    # No `task_queue=` here on purpose: `kontra workflow serve` sets KONTRA_WORKFLOW_QUEUE to the
    # queue derived from this file's content digest, and passing one would win over it — which is
    # the typed-queue mistake the derivation exists to prevent.
    catalog.serve([ParityGate])
