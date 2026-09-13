"""firstrun — the workflow half of the template. It calls `firstactor` and writes a Dataset.

    kontra workflow serve examples/workflows/firstrun --watch
    kontra workflow start examples/workflows/firstrun --wait \\
      --input '{"hosts": ["example.com", "example.org"], "mode": "deep"}'

Or press **Run** on it in the console's Workflows surface, or from the VS Code pane — the same
run either way, because the form is derived from `FirstRunInput` exactly as the actor's is from
its `Target`.

── WHAT A WORKFLOW IS FOR, AND WHAT THE ACTOR IS FOR ───────────────────────────────────────────

The Actor is the work: it takes a Unit and pushes rows. The workflow is the DECISION — how many
Units, in what order, on how many Machines, and what to do when one fails. Keeping them apart is
what lets the same `firstactor` run on your laptop while you write it and on forty droplets
afterwards with nothing about it changed.

Everything below is ordinary Python, and it is also durable: this file runs inside a Temporal
workflow, so an orchestrator that dies mid-run resumes here rather than starting over. That is
also the constraint — no clocks, no sockets, no random in this file. The actor is where those go.

── THE FLEET IS OFF BY DEFAULT, AND THAT DEFAULT IS THE SAFE DIRECTION ──────────────────────────

`machines` defaults to 0, which dispatches to whoever is ALREADY serving the actor — on your
laptop that is `kontra serve`, and it costs nothing. Any other number provisions that many
Machines and spends real money, so it has to be asked for. See `_with_fleet` for what the
credential is and why it is a name rather than a token.
"""

from datetime import timedelta

from temporalio import workflow
from typing_extensions import TypedDict

# `do_fleet` IS IMPORTED AT MODULE SCOPE DELIBERATELY. It is a declaration, not a call to a cloud:
# constructing one builds an argument dict and talks to nothing, which is what lets it live in a
# workflow file at all. The provisioning happens inside `fleet.up()`, in an activity, off the
# workflow thread.
from kontra import catalog, fleet, speak
from kontra.fleet import do_fleet

#: The actor this workflow dispatches to, as (name, version). A TUPLE and not two arguments,
#: because the pair travels together everywhere and splitting them is how a workflow ends up
#: dispatching v0.1.0's method against v0.2.0's schema.
FIRSTACTOR = ("firstactor", "0.1.0")


class FirstRunInput(TypedDict, total=False):
    """The workflow's own form — derived the same way the actor's Method form is.

    `total=False` because every field has a default below; a form somebody has not filled in must
    still start a run, or the first thing a new install teaches is a validation error.
    """

    #: The apexes to expand. One Unit each.
    hosts: list[str]
    #: `quick` or `deep` — passed straight through to the actor's `mode`.
    mode: str
    #: Where the rows land. Defaults to `firstrun`.
    dataset: str
    #: 0 (the default) attaches to whoever is serving. Any other number PROVISIONS THAT MANY
    #: MACHINES and spends money — see `_with_fleet`.
    machines: int
    #: The name of the stored credential the fleet spends. Never the token itself.
    credential: str
    #: Where the Machines land. Empty means the control plane's own default.
    region: str


@workflow.defn
class FirstRun:
    @workflow.run
    async def run(self, req: FirstRunInput) -> dict:
        hosts = req.get("hosts") or ["example.com"]
        mode = req.get("mode") or "quick"
        out = catalog.dataset(req.get("dataset") or "firstrun")

        # ONE UNIT PER HOST. The shape of each dict is the actor's `Target` — the two are checked
        # against each other at dispatch, so a field renamed on one side fails here with the name
        # in the message rather than inside the Method.
        units = [{"host": h, "mode": mode} for h in hosts]

        # `schedule_to_close_timeout` IS NOT OPTIONAL IN PRACTICE. Without one, a Method that hangs
        # hangs the run forever and the surfaces report it as `running` — the invisible failure
        # this product exists to remove. Five minutes is right for a template and wrong for a
        # crawl; pick it for the work.
        call_opts = {"schedule_to_close_timeout": timedelta(minutes=5)}

        machines = int(req.get("machines") or 0)
        if machines == 0:
            await speak(f"attached to whoever is serving {FIRSTACTOR[0]}@{FIRSTACTOR[1]}")
            rows = await self._expand(units, out, call_opts)
        else:
            rows = await self._with_fleet(req, machines, units, out, call_opts)

        await speak(f"{rows} candidate(s) in {out.name}")
        return {"hosts": len(hosts), "candidates": rows, "dataset": out.name}

    async def _expand(self, units, out, call_opts) -> int:
        """The dispatch itself. Four lines, and three of them are the `async with`.

        THE SCOPE IS THE LEASE. `catalog.actor(...)` holds one for its body and drops it on exit —
        including on an exception, which is what stops a failed run from leaving a Machine
        reserved. Nothing here has to remember to release anything.
        """
        async with catalog.actor(*FIRSTACTOR) as a:
            found, _ = await a.expand(units, out, **call_opts)
        return len(found)

    async def _with_fleet(self, req, machines, units, out, call_opts) -> int:
        """Provision Machines, place the actor on them, dispatch, and destroy them.

        ── THE CREDENTIAL IS A NAME, NEVER A TOKEN ─────────────────────────────────────────────

        `credential="do-prod"` names a secret this control plane holds. The value is resolved on
        the Controller at converge time and never enters this file, this run's arguments, or
        workflow history — which is the whole reason it is a name. A token written here would be
        replayed into history on every worker that picks the run up and kept for the namespace's
        entire retention.

        PUT THE VALUE IN THE SECRET STORE FIRST — the console's Secrets surface, or:

            curl -fsS -X PUT http://127.0.0.1:8088/api/secrets/do-prod \\
              -H 'content-type: application/json' \\
              -d '{"value": "dop_v1_…"}'

        It is written to `~/.kontra/secrets` on the control plane, which is `0600` and gitignored,
        and read back only by the converge. `kontra fleet up` with an unknown name fails BEFORE
        provisioning anything, which is the failure you want — the other order bills you first.

        ── AND THE SCOPE EXIT DESTROYS THEM ────────────────────────────────────────────────────

        The Machines die when this block ends, on success or on exception. `kontra workflow
        cancel` runs scope exits and so destroys them; `terminate` skips scope exits and leaves
        them billing, which is why cancel is the one to reach for.
        """
        spec = do_fleet(
            machines=machines,
            # Empty means the control plane's default rather than a region hard-coded in a
            # template — a fleet that lands somewhere the Controller cannot reach hangs every
            # dispatch on `resolveBatch` and looks like a stuck run, not a placement error.
            **({"region": req["region"]} if req.get("region") else {}),
            **({"credential": req["credential"]} if req.get("credential") else {}),
        )
        # `actor=` AND `version=` ARE REQUIRED, and `up` places them itself — it is sugar over
        # `hold` + `place`, and it costs ONE converge rather than two. A separate `f.place(...)`
        # afterwards is the older spelling and provisions the Machines before it knows what goes
        # on them.
        async with fleet.up(
            spec, actor=FIRSTACTOR[0], version=FIRSTACTOR[1], sessions=1
        ) as f:
            # WAIT FOR POLLERS, not for droplets. A Machine that exists and is not yet polling its
            # queue takes a dispatch that nothing answers, which is a run that looks hung.
            await f.ready()
            await speak(f"{len(f.inventory)} machine(s) polling {FIRSTACTOR[0]}")
            return await self._expand(units, out, call_opts)


if __name__ == "__main__":
    # WITHOUT THIS BLOCK NOTHING SERVES. `kontra workflow serve` runs this file with `python`, and
    # a file that only DEFINES a workflow defines it and exits — which used to be reported as a
    # clean exit 0 and no worker. `catalog.serve` is what registers the contract the console's form
    # is drawn from and then blocks on the queue.
    catalog.serve([FirstRun])
