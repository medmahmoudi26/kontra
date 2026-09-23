"""nscheck — a whole run, infrastructure included, as one durable program.

THE QUESTION: for every domain in a list, which of its delegated nameservers fails to answer for
that domain? A nameserver in a zone's NS set that does not serve the zone is a lame delegation —
a resolution fault at best, and at worst a takeover, when the delegation points at a provider
where an unclaimed zone can simply be registered.

WHAT IS NEW HERE is the first four lines of the body. `examples/python/workflows/dnssweep.py`
shows the loop; this shows the loop WITH THE MACHINES IT RUNS ON inside the same scope:

    async with fleet.hold(tag="dns", machines=4) as f:  # capacity, and a Lease on it
        await f.place("nscheck", "0.1.0", sessions=8)   # what runs on it
        await f.ready()                                 # …and Workers actually POLLING, which is
        ...                                             #    not the same thing
    #  scope exit drops the Lease; the Machines go when the LAST one does

That is only trustworthy because this is a workflow. A script that provisions ten machines and
then dies leaves ten machines; this cannot, because the teardown is a replayable step in a program
Temporal finishes whether or not the process that started it still exists. Cloud machines that
outlive a run are a billing event and an OPSEC one.

    ┌ the control machine ──────┐        ┌ the fleet — created by this file ──┐
    │ NsCheck (this file)       │──ref───┤ nscheck@0.1.0  (Go actor) ×4       │
    │  up → page → delegate     │        │   delegation  domain → nameservers │
    │       → ask(chunk, tmp)   │──ref───┤   ask         (domain,ns) → verdict│
    │  → insert_from(tmp, …)    │        └────────────────────────────────────┘
    └───────────────────────────┘         (the fleet is gone before this line)

ONE ACTOR, TWO METHODS, ONE FLEET. A stack carries a single actor — which is also what NAMES the
fleet, `<actor>-<version>` — so a run split across two Actors would need two scopes. Folding both
halves into one Actor (ADR 0023 §9) is what keeps this to a single `fleet.up`.

TWO WIDTHS, AS ALWAYS. `delegation` fans out — one domain with four nameservers emits four units —
so a 100-domain page comes back as ~400 and is re-paged before `ask` sees it. Skipping that step
puts 400 units through one activity on one worker, past the measured 200/1000 guard, with no
isolation boundary between them.

THE FINDINGS ARE THE OUTPUT, NOT THE ERRORS. `ask` emits a row for every (domain, nameserver)
pair including every failure — a refusing server, an unresolvable one, a domain that delegates
nowhere. Nothing here treats a broken nameserver as a dropped Unit, because a dropped Unit is
absent from `results`, and for this run that would mean silently discarding exactly what it went
looking for. `dropped` counts inputs that were malformed, which is a different claim; a Method
call hands it back beside `results` (ADR 0028 §4) so a run that dropped everything cannot read
like a run that found nothing.

TWO ACTS, ONE PROVENANCE. Production stages every verdict into a TEMPORARY Dataset; acceptance
promotes only the lame delegations (`where="NOT ok"`) into `lame`, after the fleet is destroyed.
The promotion is a provenance-preserving copy — every promoted row keeps the Machine, Actor
version and Run that produced it — so `lame` names all four Machines rather than collapsing to
one. That separation is the point: the raw verdicts and the curated record are two things with a
gap between them, and the gap is where a human would triage.

Run it:

    kontra build --actor examples/go/nscheck          # publish the Artifact ONCE
    kontra workflow serve examples/python/workflows/nscheck
    kontra workflow start examples/python/workflows/nscheck --wait \
        --input '{"dataset": "domains", "into": "lame", "machines": 4}'

    # `lame` fills only when the run PROMOTES, after the fleet is gone — it IS the lame delegations:
    kontra db query "SELECT verdict, count(*) FROM lame GROUP BY 1 ORDER BY 2 DESC"
    # every promoted row still names the Machine that produced it — four Machines, not one:
    kontra db query "SELECT node, count(*) FROM lame GROUP BY 1 ORDER BY 1"
"""
from temporalio import workflow
from typing_extensions import TypedDict

from kontra import catalog, fleet


class NsCheckInput(TypedDict, total=False):
    """The knobs this run reads out of its request — a DESCRIPTION of the body, not a new gate.

    Every one of these keys is already read below (`dataset`, `into`, and the three `_num` knobs);
    writing them down is what turns the registered descriptor from `dict`'s "any object" — which
    every surface correctly draws as *declares no fields* — into a shape the Workflows page can
    generate a form from. It changes what the catalog ADVERTISES, not what the workflow accepts.

    A `TypedDict`, deliberately, not a dataclass or a pydantic model:

      • It is a plain `dict` at runtime, so `req.get(...)` and `_num(req, ...)` below are untouched —
        in particular the falsy-zero handling `_num` exists for keeps working verbatim. A dataclass
        would force attribute access and a rewrite of exactly the code the type is meant to describe.
      • `total=False`: every knob has a default in the body, so none is required. The generated form
        offers them; it does not demand them.
      • Temporal's default converter decodes an unknown key straight through (its `value_to_type`
        keeps keys the TypedDict does not name), so a caller passing a plain dict — with extra keys
        or none of these — still works exactly as before.
    """

    dataset: str    # which Dataset of domains to page over          (default "domains")
    into: str       # the Dataset the lame delegations promote into  (default "lame")
    machines: int   # Droplets in the fleet; 0 means zero, not four  (default 4, see `_num`)
    sessions: int   # Workers-per-Machine density, not scale         (default 8)
    size: int       # page size for both fan-out widths              (default 100)


def _num(req: dict, key: str, default: int) -> int:
    """An integer knob, where ABSENT means the default and ZERO means zero.

    Written out rather than inlined because the idiomatic short form is a real hazard on this
    particular argument: `int(req.get("machines") or 4)` turns `machines: 0` — the way you ask a
    run to provision nothing — into four Droplets. It is the wrong answer in the expensive
    direction, and nothing raises.
    """
    v = req.get(key)
    return default if v is None else int(v)


@workflow.defn
class NsCheck:
    @workflow.run
    async def run(self, req: NsCheckInput) -> dict:
        # `.get(k) or default` is WRONG for a number here, and expensively so: `machines=0` is
        # falsy, so `req.get("machines") or 4` reads a request for NO machines as a request for
        # four, and provisions them. Anything whose zero is meaningful uses a real default.
        size = _num(req, "size", 100)
        out_name = req.get("into") or "lame"
        domains = catalog.dataset(req.get("dataset") or "domains")
        pairs = checked = dropped = 0

        # PRODUCE into a temporary Dataset, ACCEPT out of it — the two acts this run is built to
        # separate (temp-datasets slice 02). `ask` stages EVERY verdict into `tmp`, live and
        # queryable while the run is still going; only after the fleet is gone does a promotion
        # decide which of those verdicts become the record in `lame`. The gap between the two acts
        # is where triage fits — a workflow could park on a signal for as long as review takes,
        # because `tmp` outlives the fleet that filled it.
        async with catalog.dataset.temp() as tmp:
            # The fleet's lifetime is the PRODUCTION phase's, not the promotion's. `sessions` is
            # density, not scale: one Worker per Machine is fixed, so it is how much each does at once.
            # CAPACITY, THEN WHAT RUNS ON IT (ADR 0037). The Fleet is tagged `dns` rather than
            # named after this Actor: it is not this Run's to own, another Run may hold the same
            # capacity, and the Machines go when the LAST Lease drops — not when this scope exits.
            async with fleet.hold(tag="dns", machines=_num(req, "machines", 4)) as f:
                # `place` is idempotent desired state. Calling it again with a different
                # `sessions=` IS the scale operation, mid-Run, on Machines already working —
                # which is why there is no `scale()` verb.
                await f.place("nscheck", "0.1.0", sessions=_num(req, "sessions", 8))
                # POLLERS, not Pulumi: `place` returns while systemd is still starting, and a
                # Batch dispatched into that gap waits on a queue nobody is serving.
                await f.ready()
                # ONE SENTENCE PER PHASE, and this run has two: produce, then promote. It goes
                # here and NOT in the page loop below — a corpus of 40,000 domains is 400 pages,
                # and a sentence each is 2,000 history events and seven minutes of pure waiting.
                workflow.logger.info(
                    f"{len(f.inventory)} machine(s) polling; producing verdicts into tmp")

                async with catalog.actor("nscheck", "0.1.0") as ns:
                    # `order_by` is required: a materialized dataset stamps no row id, so paging
                    # without one may overlap or skip domains and nothing would raise.
                    async for batch in domains.batches(size, order_by="domain"):
                        delegated, drops = await ns.delegation(batch)  # 1 domain → N pairs
                        dropped += len(drops)
                        pairs += len(delegated)

                        # Re-page: the fan-out above is not this caller's to predict. Unconditional,
                        # because a Batch that already fits yields itself and schedules nothing.
                        async for chunk in delegated.batches(size):
                            # `tmp` is the output Dataset, handed to the call as its second argument
                            # (ADR 0028 §2) exactly as `chunk` is the first — and the author cannot
                            # tell it is temporary. The published Batch carries the Machine that ran
                            # this chunk, so a four-Machine run names every Machine; the promotion
                            # below carries that Machine THROUGH, so `lame` names four Machines and
                            # not one value for all 1,246 rows. Mid-run the rows are queryable in
                            # `tmp` (`kontra dataset list` shows it), not yet in `lame`.
                            verdicts, drops = await ns.ask(chunk, tmp)
                            dropped += len(drops)
                            checked += len(verdicts)

            # The fleet is destroyed; `tmp` outlived it. NOW promote — a separate, deliberate act,
            # not a flag on the producing call. `where="NOT ok"` makes `lame` the curated record of
            # the lame delegations, and the rows it gains keep the `node`/`version`/`run_id` of the
            # Run that produced them: promotion is a provenance-preserving COPY (~170 ms/40k rows,
            # measured in the PRD), never a re-stamp of this workflow over the producing one.
            promoted = await catalog.dataset(out_name).insert_from(tmp, where="NOT ok")
            # THE SECOND PHASE, AND WHAT IT MEANT. No derived turn can say that these rows are the
            # findings and the other 38,000 were fine — that is in the head of whoever wrote
            # `where="NOT ok"`, and this is the one place it can be put.
            workflow.logger.info(f"{promoted} lame of {checked} verdicts promoted into {out_name}")

        # `bundle` is the IMMUTABLE sha the mutable `latest` pointer resolved to, recorded so
        # "what did this run place" survives the next `kontra build`. `dropped` is isolation,
        # which will not fail this workflow (ADR 0023 §14) and so has to be visible; `promoted` is
        # how many of `checked` verdicts were lame enough to become the record.
        return {"into": out_name, "machines": len(f.inventory), "bundle": f.bundle_sha[:12],
                "pairs": pairs, "checked": checked, "promoted": promoted, "dropped": dropped}


if __name__ == "__main__":
    catalog.serve([NsCheck])
