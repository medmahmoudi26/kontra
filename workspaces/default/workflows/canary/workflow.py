"""canary — the same Actor, called locally and then on a fleet, in one program.

THE QUESTION IT ANSWERS: does this installation still work, end to end, and does it work THE SAME
WAY on this machine as it does on a Machine? Every other workflow here does one or the other. A
local run proves the wiring — the interpreter, the queue, the Nexus endpoint, the codec — and
proves nothing about a droplet. A fleet run proves the droplet and takes four minutes and real
money to tell you the codec was misconfigured. This one is both halves of that, in that order, so
the cheap half fails first and the expensive half only ever runs against a wiring that works.

    ┌ this machine ─────────────┐        ┌ the fleet — created by this file ──┐
    │ Canary (this file)        │──ref───┤ nscheck@0.1.0  (local worker)      │  phase 1
    │  local → fleet → compare  │──ref───┤ nscheck@0.1.0  ×N  (Machines)      │  phase 2
    └───────────────────────────┘        └────────────────────────────────────┘

WHY THE SAME ACTOR TWICE AND NOT TWO ACTORS. A divergence between local and fleet is the failure
this is for — a stale Artifact on the Machines, a codec that passes through locally and truncates
over S3, a version pinned differently in two places — and it is only visible if BOTH halves are
handed the same input and their answers are compared. Two different actors would tell you nothing
about the placement.

`machines: 0` RUNS THE LOCAL HALF ONLY, and it is the default. Provisioning costs money and
outlives the tab; a canary you are afraid to run is not a canary. Ask for Machines explicitly.

Run it:

    kontra actor register examples/go/nscheck        # declare it — no serving, no running
    kontra serve --actor examples/go/nscheck         # the LOCAL half's worker
    kontra workflow register .kontra/workflows/canary
    kontra workflow serve canary --queue canary --tmux
    kontra workflow start Canary --queue canary --wait \
        --input '{"domains": ["example.com", "iana.org"]}'

    # …and with Machines, which SPENDS MONEY:
    kontra workflow start Canary --queue canary --wait \
        --input '{"domains": ["example.com"], "machines": 2}'
"""
from dataclasses import dataclass
from datetime import timedelta

from temporalio import workflow

from actorkit import ask, catalog, fleet, note
from actorkit.catalog import Batch

#: The Actor both halves call. Module scope: an ActorHandle is cheap and stateless, and nothing
#: happens until a dispatch.
NSCHECK = ("nscheck", "0.1.0")

#: What a run looks at when nothing is passed. Two domains that answer, chosen because they are
#: stable and because a canary that needed a Dataset to exist would fail on a fresh installation
#: for a reason that has nothing to do with what it is testing.
DEFAULT_DOMAINS = ["example.com", "iana.org"]


@dataclass
class Proceed:
    """The answer to the money question. `proceed` is required; `why` is the operator's note."""

    proceed: bool
    why: str = ""


def _num(req: dict, key: str, default: int) -> int:
    """An integer knob where ABSENT means the default and ZERO means zero.

    Written out rather than `int(req.get(key) or default)`, which is the idiomatic short form and a
    real hazard on `machines`: it reads `machines: 0` — the way you ask for no Droplets — as a
    request for the default number of them. Wrong in the expensive direction, and nothing raises.
    """
    v = req.get(key)
    return default if v is None else int(v)


@workflow.defn
class Canary:
    @workflow.run
    async def run(self, req: dict | None = None) -> dict:
        req = req or {}
        domains = list(req.get("domains") or DEFAULT_DOMAINS)
        machines = _num(req, "machines", 0)
        units = [{"domain": d} for d in domains]

        # `workflow.logger`, NOT print(): it is replay-aware, so a line is written once rather than
        # again on every replay, and it carries the run id — which is what makes the tmux pane
        # beside the editor readable when two runs are in flight. This is the line an operator
        # watches for to know the run reached the worker at all.
        workflow.logger.info("canary: %d domain(s), machines=%d", len(domains), machines)

        # `note` is for the OPERATOR and for the log store; both now land in the same place (ADR 0050).
        # Both, deliberately: one is the account this run leaves behind, the other is a live line
        # on a terminal. One sentence per phase — the local sweep is one phase however many
        # domains it carries.
        note(f"checking {len(domains)} domain(s) on this machine first")

        local = await self._sweep(units, where="local")
        workflow.logger.info("canary: local half done — %s", local["summary"])
        note(f"local half done — {local['summary']}")

        remote: dict | None = None
        if machines > 0:
            # THE SCOPE IS THE FLEET'S LIFETIME. Exiting it destroys the Machines, and it is a
            # replayable step in a durable program rather than a line in a script that might not
            # run — which is the whole reason a run that provisions anything is a workflow.
            workflow.logger.info("canary: bringing up %d machine(s)", machines)

            # THE GATE, AND THE ONE PLACE THIS PROGRAM SPENDS MONEY. Everything above is free and
            # everything below bills by the hour, so the run stops here and asks a person. The
            # context carries what the decision needs — how many Machines, which Actor, what the
            # cheap half already found — because an operator who has to open another tab to answer
            # is an operator who answers late.
            answer = await ask(
                f"Provision {machines} machine(s) and run the fleet half?",
                takes=Proceed,
                context={
                    "machines": machines,
                    "actor": f"{NSCHECK[0]}@{NSCHECK[1]}",
                    "domains": domains,
                    "local_summary": local["summary"],
                },
                deadline=timedelta(hours=1),
            )
            if not bool(getattr(answer, "proceed", False)):
                # A DECLINE IS NOT A FAILURE. The local half ran and its answer stands; what is
                # absent is the comparison, and `agree: None` already means exactly that.
                note("declined at the gate — no Machines provisioned")
                return {
                    "domains": domains, "local": local, "fleet": None, "agree": None,
                    "declined": True, "why": str(getattr(answer, "why", "")),
                    "at": workflow.now().isoformat(), "run": workflow.info().workflow_id,
                }

            note(f"approved — bringing up {machines} machine(s)")
            # The tag was already this scope's own word for the capacity — `up()` took it beside
            # an actor and a version, and `hold()` takes it alone, because under ADR 0037 the tag
            # IS the Fleet's name. What runs on the capacity is the next line, not this one.
            async with fleet.hold(
                tag=req.get("tag") or "canary",
                machines=machines,
            ) as f:
                await f.place(NSCHECK[0], NSCHECK[1], sessions=_num(req, "sessions", 2))
                # `place` returns while systemd is still starting. A Batch dispatched into that gap
                # waits on a queue nobody is serving, which looks like a hung run.
                await f.ready()
                workflow.logger.info("canary: fleet ready — %d machine(s)", len(f.inventory))
                remote = await self._sweep(units, where="fleet")
                remote["machines"] = len(f.inventory)
                remote["bundle"] = f.bundle_sha[:12]
            workflow.logger.info("canary: fleet destroyed — %s", remote["summary"])
            note(f"fleet half done on {remote['machines']} machine(s) — {remote['summary']}; Machines destroyed")

        agree = remote is None or _same(local["verdicts"], remote["verdicts"])
        # THE COMPARISON IS THE OUTPUT. A canary that returned two result sets and left the reader
        # to diff them by eye would be reporting the raw material of the answer rather than the
        # answer itself.
        workflow.logger.info("canary: agree=%s", agree)
        note(
     "local and fleet agree" if remote is not None and agree
     else ("local and fleet DISAGREE — that is the failure this canary exists to catch"
           if remote is not None else "local half only; no comparison was made")
 )
        return {
            "domains": domains,
            "local": local,
            "fleet": remote,
            # None when nothing ran on a fleet — which is NOT "they agreed". An absent comparison
            # and a passing one must never render the same (ADR 0017).
            "agree": None if remote is None else agree,
            "at": workflow.now().isoformat(),
            "run": workflow.info().workflow_id,
        }

    async def _sweep(self, units: list[dict], *, where: str, size: int = 100) -> dict:
        """One pass of the two Methods, wherever a worker happens to be serving them.

        IDENTICAL FOR BOTH HALVES, and that is the point: the caller does not know or care whether
        the worker is in a tmux pane on this box or on a Droplet in sfo3. Dispatch goes to the
        Actor's Nexus endpoint and the endpoint names a task queue; WHO is polling it is the
        placement, and the placement is the only thing phase 2 changes.
        """
        ns = catalog.actor(*NSCHECK)

        # ONE DOMAIN IN, N PAIRS OUT. `delegation` fans out (ADR 0023 §18): a well-run domain has
        # two to four nameservers, and the pairing has to EXIST as Units before `ask` can be handed
        # a Batch of them.
        #
        # `dispatch_ref` + `Batch.from_ref`, NOT `dispatch(...).results` — AND THAT IS THE WHOLE
        # TRICK OF CHAINING TWO METHODS. An actor host with an object store commits each emitted
        # record to its own blob and returns `{"$ref": …}` entries (ADR 0007's blob plane: it is
        # why a 10,000-unit result costs the workflow history nothing). Handing that LIST to the
        # next Method passes the envelopes through verbatim, so `unit.Str("domain")` reads a
        # `$ref` object and returns "". Passing a BATCH instead goes through `_resolved_ref`, which
        # dereferences once, in one activity, without materializing anything into this workflow.
        #
        # MEASURED, HERE, BEFORE THIS COMMENT EXISTED: `pairs: 2, verdicts: 0, isolated: 0` — a
        # `completed` run reporting that a working actor found nothing. The pane is what showed
        # the `$ref`, which is the entire argument for the pane.
        pairs = Batch.from_ref(
            await ns.dispatch_ref(units, method="delegation"),
            actor=NSCHECK[0],
            version=NSCHECK[1],
        )
        workflow.logger.info("canary: %s delegation -> %d pair(s)", where, len(pairs))

        rows: list = []
        dropped = pairs.isolated
        # RE-PAGE UNCONDITIONALLY. A Batch that already fits yields itself and schedules nothing,
        # so this costs a 1:1 Method nothing — and a fan-out that turned 100 domains into 400 pairs
        # does not become one oversized activity on one worker with no isolation boundary.
        async for chunk in pairs.batches(size):
            out = Batch.from_ref(
                await ns.dispatch_ref(chunk, method="ask"),
                actor=NSCHECK[0],
                version=NSCHECK[1],
            )
            # `.rows()` — THE END OF THE PIPELINE, where materializing is the point. Everything
            # above this line moved refs; this workflow has to COMPARE two sets of verdicts, so
            # somewhere it must hold actual records, and `rows()` is the one call that says so out
            # loud. It is affordable here because a canary looks at a handful of domains; the same
            # call on a 400-domain run is what publishing to a Dataset is for.
            rows.extend(await out.rows())
            dropped += out.isolated

        workflow.logger.info(
            "canary: %s ask -> %d verdict(s), %d dropped, first=%r",
            where,
            len(rows),
            dropped,
            rows[0] if rows else None,
        )
        ok = sum(1 for r in rows if r.get("ok"))
        summary = f"{where}: {len(rows)} verdict(s), {ok} ok"
        workflow.logger.info("canary: %s", summary)
        return {
            "where": where,
            "pairs": len(pairs),
            "verdicts": rows,
            "ok": ok,
            # ISOLATION IS NOT AN ERROR AND MUST NOT BE INVISIBLE (ADR 0023 §14). A dropped Unit is
            # simply absent from the results, so a run that lost everything and a run that found
            # nothing render identically unless the count is carried out.
            "isolated": dropped,
            "summary": summary,
        }


def _same(a: list, b: list) -> bool:
    """Do the two halves agree about every (domain, nameserver)?

    COMPARED BY THE KEY, NOT BY ORDER. Two Machines answer in whatever order they finish, and a
    list compare would report a divergence on every fleet run that worked perfectly.

    `detail` IS NOT COMPARED. It carries a resolver's own error text, which differs between a
    Droplet's resolver and this box's for reasons that are not a divergence — the VERDICT is the
    claim, and that is what has to match.
    """
    def keyed(rows: list) -> dict:
        return {(r.get("domain", ""), r.get("ns", "")): r.get("verdict", "") for r in rows}

    return keyed(a) == keyed(b)


if __name__ == "__main__":
    catalog.serve([Canary])
