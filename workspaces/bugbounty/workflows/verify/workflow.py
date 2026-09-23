"""verify — does a lead REPRODUCE, and is the attack what causes it?

THE QUESTION `hunt` CANNOT ANSWER. `hunt` sees each technique once. Its matched control rules out
"this server does it to everyone" — that is what `control_survived` means and it is why the
2026-09-18 retraction cannot happen the same way twice. But one observation of a differential is
still one observation, and a 302 that moved once could have moved for a reason nobody sent.

`desync.reach` has answered this since it was written and NOTHING HAS EVER CALLED IT. The Method
is declared, `Lead` and `Verdict` are shaped for it, the two-arm design is in its package doc —
and a grep across every workflow in this workspace finds no caller. So the severity half of this
engine has been dead code beside a scanner that produces `is_proof` rows, which is the worst
possible place for a gap: the leads look finished.

    mode=reproduce   TWO ARMS ON THE SAME HOST, from this machine.
                       control   benign / benign / benign
                       attack    benign / ATTACK / benign
                     Count how often step 3 moved in each. A lead that fires as often with no
                     attack in the middle is noise that survived the stability gate, and the
                     inline control cannot see it because the inline control varies the GADGET,
                     not whether an attack happened at all.

    mode=poison      send the attack repeatedly; report only this machine's egress.
    mode=observe     poll the same endpoint from a DIFFERENT machine and count anomalies.

THIS WORKFLOW RUNS `reproduce` ONLY. `poison`/`observe` answer "does it reach OTHER USERS", which
needs two machines with different egress addresses — a Fleet, and therefore money — and the
Verdict row refuses to answer the ip-locked question when both arms share an egress. That is a
deliberate second step, not something to slip into a verification pass.

    kontra workflow start workspaces/bugbounty/workflows/verify --wait \\
        --input '{"leads": "desync_leads_v2", "program": "nba-public"}'
"""
from temporalio import workflow
from typing_extensions import TypedDict

from datetime import timedelta

from kontra import catalog

DESYNC = ("desync", "1.2.2")

VERDICTS = "reach_verdicts"


class VerifyInput(TypedDict, total=False):
    program: str        # narrow to one program; empty verifies every proof in `leads`
    leads: str          # which leads table (default `desync_leads`)
    into: str           # where verdicts land (default `reach_verdicts`)
    scope: str          # scope dataset, for the researcher marker (default `scope_h1paid`)
    batch_per_arm: int  # probes per arm, per lead
    rate_ms: int
    proofs_only: bool   # verify only `is_proof` rows (default True)
    size: int
    call_minutes: int
    heartbeat_seconds: int


def _num(req, key, default):
    v = req.get(key)
    return default if v is None else int(v)


@workflow.defn
class Verify:
    @workflow.run
    async def run(self, req: VerifyInput) -> dict:
        leads = catalog.dataset(req.get("leads") or "desync_leads")
        verdicts = catalog.dataset(req.get("into") or VERDICTS)
        scope_name = req.get("scope") or "scope_h1paid"
        program = req.get("program") or ""

        where = ["NOT COALESCE(notes, '') = 'retracted'"]
        if program:
            where.append(f"l.program = '{program}'")
        if req.get("proofs_only", True):
            where.append("l.is_proof")

        # THE MARKER HAS TO COME ALONG. `reach` renders its own requests exactly as `smuggle`
        # does, so a verification pass without `header_block` is the noisiest traffic this system
        # sends going out UNATTRIBUTED — the same trap the sweep phase fell into.
        #
        # `class` and `variant` are what let `reach` rebuild the exact technique: the Verdict is
        # about ONE member of the corpus, and re-deriving it from a rendered request would be a
        # second implementation of the renderer.
        query = f"""
            SELECT
                l.host || '#' || l.endpoint || '#' || l.technique AS lead_id,
                l.program, l.host, l.port, l.scheme, l.endpoint,
                -- `host_header` AND `sni` COME FROM SCOPE, NOT FROM THE LEAD. A lead row does not
                -- carry them — `desync_leads` has no such columns — and the bare-IP case is not
                -- an edge case in this corpus: #3475402 is a $3000 critical that only reproduces
                -- against a backend IP with the ORIGINAL hostname still in the Host header. A
                -- verification that re-probed with the IP as its own Host would land on a default
                -- vhost and report the finding as unreproducible.
                coalesce(max(s.host_header), l.host) AS host_header,
                coalesce(max(s.sni), l.host)         AS sni,
                -- The researcher marker, for the same reason the sweep phase needed it: this is
                -- the noisiest traffic the system sends and it must not go out unattributed.
                coalesce(max(s.header_block), '')    AS header_block,
                l.class, l.technique AS variant
            FROM "{leads.name}" l
            LEFT JOIN "{scope_name}" s
                   ON s.program = l.program AND s.host = l.host
            WHERE {' AND '.join(where)}
            GROUP BY l.program, l.host, l.port, l.scheme, l.endpoint, l.class, l.technique
        """

        params = {
            "mode": "reproduce",
            "batch_per_arm": _num(req, "batch_per_arm", 12),
            "rate_ms": _num(req, "rate_ms", 400),
            "program": program,
        }
        call_opts = {"schedule_to_close_timeout":
                     timedelta(minutes=_num(req, "call_minutes", 45)),
                     "debug_heartbeat_seconds": _num(req, "heartbeat_seconds", 600)}

        pages = []
        try:
            # `variant`, NOT `technique`. The pager wraps this query as
            # `SELECT * FROM (<query>) AS _q ORDER BY <order_by>`, so `order_by` must name a
            # column the PROJECTION emits — and this one aliases `l.technique AS variant`,
            # because that is the field name `reach`'s Lead input reads. Naming the source column
            # binds against the inner table that the wrapper cannot see.
            async for batch in leads.batches(_num(req, "size", 10),
                                             order_by="host, endpoint, variant", query=query):
                pages.append(batch)
        except Exception as exc:  # noqa: BLE001 - the reason is the announcement
            # AN EMPTY FIRST PAGE RAISES (catalog.py), and here that is the ordinary case rather
            # than a fault: a program with no proofs has nothing to verify. Saying so is not the
            # same as failing.
            workflow.logger.warning(
                f"nothing to verify in {leads.name}"
                + (f" for {program}" if program else "")
                + f" — {exc!r}",
                extra={"incomplete": True, "axis": "leads", "phase": "verify"})
            return {"program": program, "verified": 0, "verdicts": 0}

        workflow.logger.info(
            f"verifying {len(pages)} page(s) of leads against desync@{DESYNC[1]} "
            f"— {params['batch_per_arm']} probes per arm, two arms each")

        rows = 0
        async with catalog.actor(*DESYNC) as d:
            for i, page in enumerate(pages, 1):
                out, _ = await d.reach(page, verdicts, params=params, **call_opts)
                rows += len(out)
                workflow.logger.info(
                    f"verify: page {i}/{len(pages)}, +{len(out)} verdict(s), {rows} total")

        # THE VERDICT IS NOT COMPUTED HERE. Two arms produce two rows and the comparison is
        # `attack.hits` against `control.hits` — a SQL question over a durable table, asked by
        # whoever is writing the report, with the counts visible. Collapsing it to a boolean in
        # this workflow would hide the one number a triager argues about.
        workflow.logger.info(
            f"verification complete: {rows} verdict row(s) in {verdicts.name}. "
            f"Compare arms: SELECT lead_id, arm, n, hits FROM {verdicts.name} ORDER BY lead_id, arm")
        return {"program": program, "verified": len(pages), "verdicts": rows}


if __name__ == "__main__":
    catalog.serve([Verify])
