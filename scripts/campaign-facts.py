#!/usr/bin/env python3
"""Collect every number the campaign report states, from the lake, in one pass.

    python3 scripts/campaign-facts.py --run campaign-1790559894 \
        --obs observations_c3 --points injection_points_c3 --leads desync_leads_c3 \
        --out /tmp/campaign-facts.json

WHY A SCRIPT AND NOT A PASTE. A report whose numbers were typed by hand is a report nobody can
re-derive. Every figure below is one SQL statement against a durable Dataset, and the statement is
kept beside the answer — so the slide, the HTML and the lake can be checked against each other by
anybody with the run id.

EVERY QUERY IS RUN-SCOPED WHERE THE DATASET IS SHARED. `observations` and `desync_leads` outlive
any one campaign; filtering only by program would silently fold in every previous sweep and make
the traffic total grow every time anybody re-scanned.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.parse
import urllib.request

API = os.environ.get("KONTRA_ORCHESTRATOR_URL", "http://localhost:8088")


def _token() -> str:
    """The workbench's read token, off this installation's own config.

    POST /api/datasets/query is `checkBearer` and FAILS CLOSED — with no token it answers 503
    naming the variable, which is the right default for a route that reads a run's content. Read
    from config.yaml rather than the environment so this script needs no wrapper.
    """
    for var in ("KONTRA_EXPLORE_TOKEN", "KONTRA_STATE_TOKEN"):
        if os.environ.get(var):
            return os.environ[var]
    home = os.environ.get("KONTRA_HOME") or ".kontra"
    for path in (os.path.join(home, "config.yaml"), ".kontra/config.yaml"):
        try:
            text = open(path).read()
        except OSError:
            continue
        for key in ("explore", "state"):
            m = re.search(rf'^\s+{key}:\s*"([^"]+)"', text, re.M)
            if m and m.group(1):
                return m.group(1)
    sys.exit("no explore token: set KONTRA_EXPLORE_TOKEN or run from a checkout with .kontra/config.yaml")


TOKEN = _token()


# Datasets that could not be read AT ALL this run — a missing table, a dead API, a bad token.
# A name in here means "we could not look", which is not the same fact as "we looked and there was
# nothing", and the report renders the two differently. See `one`.
UNREADABLE: set[str] = set()


def q(dataset: str, sql: str) -> list[dict]:
    """One query, through the orchestrator, as a list of dicts.

    THE API AND NOT THE CLI, because `kontra dataset query` renders a fixed-width TABLE for a human
    and has no machine format — parsing that back would break on the first value containing two
    spaces, which is every `point_rationale` in the dataset.
    """
    body = json.dumps({"sql": sql, "limit": 5000}).encode()
    req = urllib.request.Request(
        f"{API}/api/datasets/query", data=body, method="POST",
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {TOKEN}"})
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            doc = json.load(r)
    except Exception as exc:  # noqa: BLE001
        # A MISSING DATASET IS A FACT, NOT A CRASH. A campaign that skipped an axis has no rows for
        # it, and the report has to be able to say "not run" rather than die assembling itself.
        msg = str(exc)
        print(f"  ! {dataset}: {msg[:160]}", file=sys.stderr)
        # ONLY THE DATASET'S OWN FAILURES CONDEMN THE DATASET.
        #
        # "this table does not exist" and "that column does not exist" are different facts, and
        # treating them alike is how ONE bad query blanked every figure from a dataset that was
        # reading fine: a `programs_covered` probe referencing a column this derived Dataset does
        # not carry marked injection_points_h1 unreadable, and 9,159 points and 127 hosts turned
        # into "—" on a report where they had been correct a minute earlier.
        #
        # A missing table or a wiped Parquet file is unreadable and every figure from it is
        # unknown. A binder error is a bug in ONE query, and the other queries still hold.
        if any(s in msg for s in ("Catalog Error", "does not exist!", "404", "NoSuchKey",
                                  "HTTP Error", "IO Error")):
            UNREADABLE.add(dataset)
        return []
    cols = [c["name"] for c in doc.get("columns", [])]
    return [dict(zip(cols, row)) for row in doc.get("rows", [])]


def _run_output(run: str) -> dict:
    """What the workflow returned, off /api/runs/<id>/io.

    GATED, like every run route — it reads a run's content, so it takes the same bearer the
    workbench does. Absent (a run still going, or one Temporal has dropped) is an empty dict and a
    named warning, never a zero: "the run returned nothing" and "we could not read what it
    returned" are different facts and only one of them belongs on a slide.
    """
    req = urllib.request.Request(
        f"{API}/api/runs/{urllib.parse.quote(run, safe='')}/io",
        headers={"Authorization": f"Bearer {TOKEN}"})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            doc = json.load(r)
    except Exception as exc:  # noqa: BLE001
        print(f"  ! run output unavailable: {str(exc)[:140]}", file=sys.stderr)
        return {}
    out = doc.get("output")
    if isinstance(out, dict):
        return out
    print("  ! the run has not returned yet — the four headline numbers will read as unknown",
          file=sys.stderr)
    return {}

def one(dataset: str, sql: str, key: str, default=0):
    """One scalar off one dataset — or `None` when the dataset could not be read at all.

    WHY THIS IS NOT ALLOWED TO RETURN 0 ON FAILURE. A table that does not exist and a table with no
    matching rows both come back as "no rows". Only the second one is a zero. Collapsing them puts
    "0 requests sent" on a slide under a green COMPLETED — which reads as a claim about the SCAN
    ("we swept it and it was clean") when it is really a claim about the REPORT ("we never looked").

    That is the same failure that baked zeros into `snap-on_tools`: a half-mounted SDK raised, the
    caller defaulted it to 0, and the zero entered the record as a finding. `None` propagates to the
    report as "not run", which is the honest rendering and is visibly different from a real zero.
    """
    rows = q(dataset, sql)
    if dataset in UNREADABLE:
        return None
    return rows[0].get(key, default) if rows else default


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--run", required=True, help="the campaign run id; scopes every shared dataset")
    ap.add_argument("--obs", default="observations_c3")
    ap.add_argument("--points", default="injection_points_c3")
    ap.add_argument("--leads", default="desync_leads_c3")
    ap.add_argument("--techniques", default="techniques")
    ap.add_argument("--scope", default="scope_h1paid")
    ap.add_argument("--out", default="/tmp/campaign-facts.json")
    a = ap.parse_args()

    # Child run ids are `<campaign>-hunt-<tag>-<program>` and `<campaign>-surface-<program>`, so a
    # LIKE on the campaign id catches every row this campaign wrote without naming the programs.
    scoped = f"run_id LIKE '{a.run}%'"
    facts: dict = {"run": a.run, "datasets": {
        "observations": a.obs, "injection_points": a.points, "leads": a.leads,
        "techniques": a.techniques, "scope": a.scope}}

    # ── THE RUN'S OWN OUTPUT ──────────────────────────────────────────────────────────────────
    #
    # The four headline numbers come from what the WORKFLOW RETURNED, not from a second SQL pass.
    # They have to: `machines_used` and `fleet_cost_usd` are facts about capacity that existed for
    # the length of the run and no longer does, so nothing in the lake can recompute them
    # afterwards. The lake figures below are the CHECK on them, not the source.
    print("run output")
    facts["run_output"] = _run_output(a.run)

    print("scope")
    facts["scope"] = {
        "rows": one(a.scope, f'SELECT count(*) AS n FROM "{a.scope}"', "n"),
        "programs": one(a.scope, f'SELECT count(DISTINCT program) AS n FROM "{a.scope}"', "n"),
        "hosts": one(a.scope, f'SELECT count(DISTINCT host) AS n FROM "{a.scope}"', "n"),
        "crawlable": one(a.scope, f"SELECT count(*) AS n FROM \"{a.scope}\" WHERE seed <> ''", "n"),
        "no_seed": one(a.scope, f"SELECT count(*) AS n FROM \"{a.scope}\" WHERE coalesce(seed,'') = ''", "n"),
        "by_kind": q(a.scope, f'SELECT kind, count(*) AS n FROM "{a.scope}" GROUP BY 1 ORDER BY n DESC'),
    }

    print("injection points")
    facts["points"] = {
        # WHAT THIS RUN ACTUALLY COVERED, which is not the size of the scope it was pointed at.
        # The masthead used to describe coverage from `scope.programs` — the number of programs in
        # scope_h1paid — so a run that reached nine of them still announced 462. Counted from the
        # rows this campaign itself wrote, so it can only ever report ground that was walked.
        # No `{scoped}` here: an injection-point Dataset is DERIVED, so it carries the crawl's own
        # columns and none of the framework stamps — there is no `run_id` to filter on. Scoping
        # comes from the dataset NAME instead: `--points injection_points_h1` is this campaign's
        # own, which is why every campaign is given a fresh one.
        "programs_covered": one(
            a.points,
            f'SELECT count(DISTINCT program) AS n FROM "{a.points}" WHERE is_injection_point', "n"),
        "total": one(a.points, f'SELECT count(*) AS n FROM "{a.points}"', "n"),
        "claimed": one(a.points, f'SELECT count(*) AS n FROM "{a.points}" WHERE is_injection_point', "n"),
        "hosts": one(a.points, f'SELECT count(DISTINCT host) AS n FROM "{a.points}" WHERE is_injection_point', "n"),
        "by_kind": q(a.points, f'SELECT point_kind, count(*) AS points, count(DISTINCT host) AS hosts '
                               f'FROM "{a.points}" WHERE is_injection_point GROUP BY 1 ORDER BY points DESC'),
        "off_scope_hosts": one(a.points, f'SELECT count(DISTINCT host) AS n FROM "{a.points}" '
                                         f'WHERE NOT is_injection_point', "n"),
        "sample": q(a.points, f'SELECT host, point_kind, point_id, point_rationale, seen_count '
                              f'FROM "{a.points}" WHERE is_injection_point '
                              f'ORDER BY seen_count DESC, host, point_id LIMIT 12'),
    }

    print("technique corpus")
    facts["techniques"] = {
        "rows": one(a.techniques, f'SELECT count(*) AS n FROM "{a.techniques}"', "n"),
        "by_class": q(a.techniques, f'SELECT class, count(*) AS n FROM "{a.techniques}" GROUP BY 1 ORDER BY n DESC'),
    }

    print("observations")
    facts["observations"] = {
        "rows": one(a.obs, f'SELECT count(*) AS n FROM "{a.obs}" WHERE {scoped}', "n"),
        "requests": one(a.obs, f'SELECT coalesce(sum(requests),0) AS n FROM "{a.obs}" WHERE {scoped}', "n"),
        "hosts": one(a.obs, f'SELECT count(DISTINCT host) AS n FROM "{a.obs}" WHERE {scoped}', "n"),
        "egress": one(a.obs, f"SELECT count(DISTINCT egress) AS n FROM \"{a.obs}\" "
                             f"WHERE {scoped} AND egress <> ''", "n"),
        "by_axis": q(a.obs, f'SELECT axis, class, count(*) AS observations, '
                            f'coalesce(sum(requests),0) AS requests_sent, '
                            f"count(DISTINCT host) AS hosts, count(DISTINCT egress) AS source_ips "
                            f'FROM "{a.obs}" WHERE {scoped} GROUP BY 1,2 ORDER BY requests_sent DESC'),
        "by_egress": q(a.obs, f'SELECT egress AS source_ip, count(*) AS observations, '
                              f'coalesce(sum(requests),0) AS requests_sent '
                              f"FROM \"{a.obs}\" WHERE {scoped} AND egress <> '' "
                              f'GROUP BY 1 ORDER BY requests_sent DESC'),
        # THE THREE OUTCOMES, KEPT APART — "we could not test this", "this had no claim to make"
        # and "this is clean" are different facts and a report that folds them lies by omission.
        "erratic": one(a.obs, f'SELECT count(*) AS n FROM "{a.obs}" WHERE {scoped} AND erratic', "n"),
        "voided": one(a.obs, f'SELECT count(*) AS n FROM "{a.obs}" WHERE {scoped} AND voided', "n"),
        "signalling": one(a.obs, f'SELECT count(*) AS n FROM "{a.obs}" WHERE {scoped} AND signal_count > 0', "n"),
        "void_reasons": q(a.obs, f"SELECT void_reason, count(*) AS n FROM \"{a.obs}\" "
                                 f"WHERE {scoped} AND voided AND coalesce(void_reason,'') <> '' "
                                 f'GROUP BY 1 ORDER BY n DESC LIMIT 8'),
        "signals": q(a.obs, f'SELECT unnest(signals) AS signal, count(*) AS n FROM "{a.obs}" '
                            f'WHERE {scoped} AND signal_count > 0 GROUP BY 1 ORDER BY n DESC LIMIT 12'),
    }

    print("leads")
    facts["leads"] = {
        "rows": one(a.leads, f'SELECT count(*) AS n FROM "{a.leads}" WHERE {scoped}', "n"),
        "proof": one(a.leads, f'SELECT count(*) AS n FROM "{a.leads}" WHERE {scoped} AND is_proof', "n"),
        "sample": q(a.leads, f'SELECT host, endpoint, axis, class, technique, signal_count, is_proof '
                             f'FROM "{a.leads}" WHERE {scoped} '
                             f'ORDER BY is_proof DESC, signal_count DESC LIMIT 12'),
    }

    with open(a.out, "w") as fh:
        json.dump(facts, fh, indent=2, default=str)
    print(f"\nwrote {a.out}")
    show = lambda v: "not run" if v is None else f"{v}"  # noqa: E731
    print(f"  {show(facts['observations']['rows'])} observation(s), "
          f"{show(facts['observations']['requests'])} request(s), "
          f"{show(facts['observations']['egress'])} source address(es), "
          f"{show(facts['leads']['rows'])} lead(s)")

    # LOUD, AND LAST, because this is the line that decides whether the report can be shown to
    # anyone. Every figure sourced from a dataset named here renders as "—" rather than a number,
    # and a reader who takes that em-dash for a zero draws the opposite conclusion from the truth.
    if UNREADABLE:
        print(f"\n  \033[1;33mWARNING\033[0m  {len(UNREADABLE)} dataset(s) could not be read: "
              f"{', '.join(sorted(UNREADABLE))}")
        print("            Every figure from them reads '—' (not run), NOT 0. If the campaign was")
        print("            supposed to populate them, the report is not ready to present.")


if __name__ == "__main__":
    main()
