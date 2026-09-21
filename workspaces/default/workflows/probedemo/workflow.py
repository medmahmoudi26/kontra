"""probedemo — dispatch one Method over a small Batch and land the rows in a Dataset.

The smallest workflow that actually PRODUCES data. `ping` proves the wiring and writes nothing;
this one writes, so a Run has a Dataset the console can show a table of.

It touches nothing outside this machine: every target is the appliance's own HTTP surface, so the
Batch exercises the dispatch and the writer without reaching a third party.

    kontra workflow serve .kontra/workflows/probedemo --tmux
    kontra workflow start .kontra/workflows/probedemo --input '{"into": "probe_demo"}'

The derived turns say WHICH Method ran on how many Machines; `note` is where this file says what
it MEANT by them. One sentence per phase — the dispatch is one phase, however many targets it
carries — and it returns immediately, which is what separates it from `ask` (see the `approve`
workflow, which uses both).
"""
from temporalio import workflow

from actorkit import catalog, note

# The appliance's own surfaces. Harmless by construction — a HEAD against the API that is already
# serving this console, rather than somebody else's host.
TARGETS = [
    {"url": "http://localhost:8088/api/health", "host": "localhost"},
    {"url": "http://localhost:8088/api/workflows", "host": "localhost"},
    {"url": "http://localhost:8088/api/actors", "host": "localhost"},
    {"url": "http://localhost:8088/api/datasets", "host": "localhost"},
    {"url": "http://localhost:8088/api/runs", "host": "localhost"},
    {"url": "http://localhost:8088/nope-404", "host": "localhost"},
]


@workflow.defn
class ProbeDemo:
    @workflow.run
    async def run(self, req: dict | None = None) -> dict:
        into = (req or {}).get("into") or "probe_demo"

        # A PHASE, NOT A UNIT: one sentence for the whole dispatch, not one per target. Six targets
        # would be six seconds and thirty history events for a demo that takes about one, and the
        # same line in a 623-unit sweep is the mistake that only shows up in production.
        note(f"probing {len(TARGETS)} appliance endpoints into {into}")

        # `head(batch, out)` publishes the Method's output straight into the writer — the Batch's
        # ref IS the manifest, so nothing is reshaped and no rows pass through this workflow.
        async with catalog.actor("probe", "0.1.0") as probe, \
                   catalog.dataset(into).writer() as out:
            checked, dropped = await probe.head(TARGETS, out)

        # Isolation is not an error and will not fail this run, so the sentence carries it: a run
        # that dropped everything and one that found nothing must not read the same.
        note(f"{len(checked)} checked, {len(dropped)} dropped; {into} is sealed")

        return {
            "into": into,
            "targets": len(TARGETS),
            "checked": len(checked),
            # Named beside the count that succeeded, never folded into it: a Batch that dropped
            # units and one that legitimately found nothing are different facts.
            "dropped": len(dropped),
        }


if __name__ == "__main__":
    catalog.serve([ProbeDemo])
