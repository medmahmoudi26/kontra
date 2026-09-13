"""ping — the smallest possible caller workflow, for checking the wiring.

It dispatches nothing, provisions nothing and writes nothing. That is the point: when Serve or Run
misbehaves, this separates "the control plane is broken" from "my run is broken", and it does
it without spending a cent or touching a target.

    Serve it on a queue, press Run, and a Run appears in the list. If it does not, the problem is
    the wiring — the tmux session, the interpreter, the queue name — and not your workflow.

Run it:

    kontra workflow serve ping
    kontra workflow start ping --wait --input '{"note": "hello"}'
"""
from temporalio import workflow

from kontra import catalog


@workflow.defn
class Ping:
    @workflow.run
    async def run(self, req: dict | None = None) -> dict:
        # `workflow.now()` rather than the clock: a workflow is replayed, and a real clock would
        # give a different answer on every replay.
        return {
            "ok": True,
            "note": (req or {}).get("note", ""),
            "at": workflow.now().isoformat(),
            "run": workflow.info().workflow_id,
        }


if __name__ == "__main__":
    catalog.serve([Ping])
