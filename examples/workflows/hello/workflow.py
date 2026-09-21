"""hello — seeded first workflow. Places hello@0.1.0 on a local dockerFleet."""

from datetime import timedelta

from temporalio import workflow

from kontra import catalog, fleet, note
from kontra.fleet import docker_fleet

HELLO = ("hello", "0.1.0")


@workflow.defn
class Hello:
    @workflow.run
    async def run(self, req: dict | None = None) -> dict:
        out = catalog.dataset("hello")
        call_opts = {"schedule_to_close_timeout": timedelta(minutes=5)}
        async with fleet.up(docker_fleet(machines=1), actor=HELLO[0], version=HELLO[1], sessions=1) as f:
            await f.ready()
            note(f"{len(f.inventory)} machine(s) polling {HELLO[0]}")
            async with catalog.actor(*HELLO) as a:
                found, _ = await a.greet([{}], out, **call_opts)
        note(f"{len(found)} row(s) in {out.name}")
        return {"dataset": out.name, "rows": len(found)}


if __name__ == "__main__":
    catalog.serve([Hello])
