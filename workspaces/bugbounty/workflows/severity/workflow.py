"""severity — run the POISON half of the oracle, for an observer that is not a kontra worker.

── WHAT THIS ANSWERS THAT `verify` CANNOT ──────────────────────────────────────────────────────

`verify` runs `mode=reproduce`: two arms from THIS machine, and it establishes that the attack is
what moves the response rather than noise. It cannot establish who is hurt, and `reach.go` says so
in its own package doc — "the only thing measured is whether OUR OWN next request came back wrong".
Benign self-pipelining produces exactly that signature: control clean, attack firing, because it is
our own connection on both arms.

The discriminator is two egress addresses: poison from one, watch from another. If a response
naming OUR canary arrives on a connection THIS machine never opened, the desync crosses clients and
the finding is global rather than IP-locked.

── WHY THE OBSERVER IS NOT IN HERE ─────────────────────────────────────────────────────────────

`desync.reach` has a `mode=observe` and it polls from whichever machine the ACTOR runs on. Every
actor in this cluster runs on one box, so `poison` and `observe` would share an egress — and the
Verdict row refuses to answer the IP-locked question when both arms do, which is correct and makes
the built-in observer useless here without a Fleet.

The observer used instead is the Caido instance, which is a separate host with a different egress
(measured: this box 24.144.94.142, Caido 161.35.120.153). It is driven over its MCP API and polls
the same endpoint with a benign request while this workflow poisons. So this workflow is
deliberately HALF an experiment, and its verdict row says `mode=poison` and nothing about impact.

── WHY ONE LEAD AT A TIME, ENFORCED ────────────────────────────────────────────────────────────

`host` and `variant` are both REQUIRED and there is no "whole program" form. Poisoning sends an
attack repeatedly at a shared front-end, which is the one thing this system does that can hand a
wrong response to somebody who is not us. A driver that accepts `{"program": "visa"}` and fans out
over 71 hosts is one typo away from doing that at scale, and the cost of requiring two more fields
is two more fields.

The smuggled prefix is still an incomplete request for a path that does not exist, and this
workflow never completes it — the blast radius of a hit is that one stranger gets one 404 for our
canary path instead of their response.

    kontra workflow start workspaces/bugbounty/workflows/severity --wait --input \\
      '{"host": "pay.8x8.com", "endpoint": "/api/thankyou",
        "variant": "around-colon-09-preserve", "rounds": 120}'
"""
from temporalio import workflow
from typing_extensions import TypedDict

from datetime import timedelta

from kontra import catalog

DESYNC = ("desync", "1.2.2")


class SeverityInput(TypedDict, total=False):
    host: str          # REQUIRED — exactly one host
    variant: str       # REQUIRED — exactly one technique
    endpoint: str      # narrow further; empty takes every endpoint of that (host, variant)
    leads: str
    into: str
    scope: str
    rounds: int
    rate_ms: int
    call_minutes: int
    heartbeat_seconds: int


def _num(req, key, default):
    v = req.get(key)
    return default if v is None else int(v)


@workflow.defn
class Severity:
    @workflow.run
    async def run(self, req: SeverityInput) -> dict:
        host = (req.get("host") or "").strip()
        variant = (req.get("variant") or "").strip()
        # REFUSED, NOT DEFAULTED. An empty host here would select the whole leads table.
        if not host or not variant:
            workflow.logger.warning(
                "severity needs BOTH host and variant — it poisons a live front-end and "
                "will not infer which one",
                extra={"incomplete": True, "axis": "leads", "phase": "poison"})
            return {"poisoned": 0, "reason": "host and variant are required"}

        leads = catalog.dataset(req.get("leads") or "desync_leads_v2")
        verdicts = catalog.dataset(req.get("into") or "reach_verdicts")
        scope_name = req.get("scope") or "scope_h1paid"
        endpoint = (req.get("endpoint") or "").strip()

        where = ["NOT COALESCE(l.notes, '') = 'retracted'",
                 f"l.host = '{host}'", f"l.technique = '{variant}'"]
        if endpoint:
            where.append(f"l.endpoint = '{endpoint}'")

        # THE SAME PROJECTION `verify` USES, including the scope join — `header_block` carries the
        # researcher marker, and poison traffic is the last thing that should go out unattributed.
        # NOTE the join is on program AND host: `scope_8x8.program` is '8x8' while these leads say
        # '8x8-bounty', so passing that scope silently yields an empty marker. `scope_h1paid` is
        # the one that matches.
        query = f"""
            SELECT
                l.host || '#' || l.endpoint || '#' || l.technique AS lead_id,
                l.program, l.host, l.port, l.scheme, l.endpoint,
                coalesce(max(s.host_header), l.host) AS host_header,
                coalesce(max(s.sni), l.host)         AS sni,
                coalesce(max(s.header_block), '')    AS header_block,
                l.class, l.technique AS variant
            FROM "{leads.name}" l
            LEFT JOIN "{scope_name}" s
                   ON s.program = l.program AND s.host = l.host
            WHERE {' AND '.join(where)}
            GROUP BY l.program, l.host, l.port, l.scheme, l.endpoint, l.class, l.technique
        """

        params = {
            "mode": "poison",
            "rounds": _num(req, "rounds", 120),
            "rate_ms": _num(req, "rate_ms", 400),
        }
        call_opts = {"schedule_to_close_timeout":
                     timedelta(minutes=_num(req, "call_minutes", 30)),
                     "debug_heartbeat_seconds": _num(req, "heartbeat_seconds", 600)}

        pages = []
        try:
            async for batch in leads.batches(10, order_by="host, endpoint, variant", query=query):
                pages.append(batch)
        except Exception as exc:  # noqa: BLE001 - the reason is the announcement
            workflow.logger.warning(
                f"no lead matches {host} / {variant}"
                + (f" / {endpoint}" if endpoint else "") + f" — {exc!r}",
                extra={"incomplete": True, "axis": "leads", "phase": "poison"})
            return {"poisoned": 0, "reason": "no matching lead"}

        workflow.logger.info(
            f"poisoning {host} ({variant}"
            + (f", {endpoint}" if endpoint else ", every endpoint")
            + f") for {params['rounds']} round(s) — observe from the OTHER egress now")

        rows = 0
        async with catalog.actor(*DESYNC) as d:
            for i, page in enumerate(pages, 1):
                out, _ = await d.reach(page, verdicts, params=params, **call_opts)
                rows += len(out)
                workflow.logger.info(f"poison: page {i}/{len(pages)}, +{len(out)} verdict row(s)")

        workflow.logger.info(
            f"poisoning done: {rows} row(s) in {verdicts.name}. The impact answer is on the "
            f"OBSERVER's side — this half reports only its own egress.")
        return {"poisoned": rows, "host": host, "variant": variant, "endpoint": endpoint,
                "rounds": params["rounds"]}


if __name__ == "__main__":
    catalog.serve([Severity])
