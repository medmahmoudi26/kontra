#!/usr/bin/env python3
"""Build EVERY paying program's scope as ONE kontra Dataset.

    python3 scripts/paying-scope.py --platform h1 --out /tmp/h1paid \
        --header 'X-Bug-Bounty: HackerOne-medmahmoudi'
    kontra dataset create scope_h1paid --from /tmp/h1paid.scope.jsonl

── WHY THIS EXISTS BESIDE program-scope.py ─────────────────────────────────────────────────────

`program-scope.py` builds ONE program, named by handle, into `scope_<name>`. That is the right
shape when a program is a project: you pick it, you enrich it, you keep it.

It is the wrong shape for a campaign across the whole paying surface. HackerOne has 460 programs
that pay and publish a web asset, and running that one program at a time means 460 Datasets whose
only distinguishing content is a value already sitting in their `program` column — a catalog no
human can read, and 460 places for a schema to drift. So this emits ONE Dataset with `program` as
the discriminator, which is what `hunt`'s `scope` input was added to consume.

EVERYTHING ELSE IS program-scope.py's, DELIBERATELY. `classify` is imported rather than re-spelled
because it is the safety boundary — `*.` is the only glob that expands, and two bugs of exactly
that shape (a path trimmed before the glob check, a CIDR's /8 read as a path) were caught by its
tests before any actor ran. `row` and `_header_block` likewise. A second copy of any of them is a
second place for that to regress, on the one file where regressing means scanning something
nobody scoped.

── WHAT `--paid` MEANS HERE ────────────────────────────────────────────────────────────────────

`is_bbp` is HackerOne's PER-ASSET `eligible_for_bounty`, so "paying program" and "paying asset"
are different questions and this answers the second. 8x8 publishes `voapi.8x8.com` as in-scope,
Critical, and worth no bounty; a campaign aimed at paying scope should not spend its rate limit
there. The program-level view is derived: a program is in this Dataset because at least one of
its assets pays.

── THE EXCLUSIONS ARE AN OUTPUT ────────────────────────────────────────────────────────────────

`in_scope = 0` rows are written to their own file, for the same reason program-scope.py writes
them: a wildcard's expansion has to have the carve-outs subtracted AFTER enumeration, and a
Dataset holding only in-scope rows cannot answer "is this host carved out?".
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import pathlib
import re
import sys
from collections import Counter

_HERE = pathlib.Path(__file__).resolve().parent

# Both neighbours have a hyphen in their name, so neither is importable by `import`.
def _load(name: str):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), _HERE / f"{name}.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


_ps = _load("program-scope")
classify, row, _header_block, connect, NON_HTTP = (
    _ps.classify, _ps.row, _ps._header_block, _ps.connect, _ps.NON_HTTP)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--platform", default="h1", help="bbscope platform key (h1, it)")
    ap.add_argument("--paid", action="store_true", default=True,
                    help="keep only is_bbp=1 assets (the default; this is the paying surface)")
    ap.add_argument("--all-assets", dest="paid", action="store_false",
                    help="keep every in-scope asset, paying or not")
    ap.add_argument("--header", action="append", default=[], dest="headers",
                    help="one header line stamped on every desync request, e.g. "
                         "'X-Bug-Bounty: HackerOne-medmahmoudi'. REPEAT the flag for more; there "
                         "is deliberately no in-string separator (see _header_block).")
    ap.add_argument("--out", required=True, help="path prefix for the two JSONL files")
    args = ap.parse_args()

    block = _header_block(args.headers)
    cur = connect().cursor()
    cur.execute(
        """SELECT p.handle, t.target, t.category, t.in_scope, t.is_bbp,
                  p.platform, p.url, t.first_seen_at, t.last_seen_at
             FROM targets_raw t JOIN programs p ON p.id = t.program_id
            WHERE p.platform = %s AND p.disabled = 0 AND p.is_ignored = 0
            ORDER BY p.handle, t.target""", (args.platform,))
    rows = cur.fetchall()
    if not rows:
        sys.exit(f"no rows for platform {args.platform!r} — is the bbscope DB populated?")

    scope, excluded = [], []
    seen = set()
    dropped = Counter()

    for handle, target, category, in_scope, is_bbp, platform, purl, first, last in rows:
        # Free text from the platform: at least one target begins with a literal TAB.
        target = (target or "").replace("\t", " ").replace("\n", " ").replace("\r", " ").strip()
        if not target:
            continue
        kind, seed = classify(target)

        if not in_scope:
            excluded.append({
                "program": handle, "target": target, "kind": kind,
                "host": (target[2:] if target.startswith("*.") else
                         re.sub(r"^https?://", "", target).split("/")[0]),
                "category": category, "platform": platform, "handle": handle,
            })
            continue
        if category in NON_HTTP:
            dropped["non-http"] += 1
            continue
        if args.paid and not is_bbp:
            dropped["unpaid"] += 1
            continue

        # THE PROGRAM IS THE HANDLE. `hunt` filters `WHERE program = '<program>'` in every query
        # it runs, so this column is the only thing that keeps 460 programs' authorisations from
        # bleeding into each other inside one Dataset. It has to be the same string a caller
        # passes as `program`, which is the handle as HackerOne spells it.
        r = row(handle, target, kind, seed, platform, handle, purl, is_bbp, category, first, last)
        if block:
            r["header_block"] = block
        if kind == "unexpandable":
            # KEPT AND MARKED, never dropped: an unexpandable glob is surface somebody scoped,
            # and a triage queue is the right place for it. Its empty seed is what keeps it away
            # from a crawler, and `hunt` filters the kind out before it can become a Target.
            dropped["unexpandable"] += 1

        # DEDUPED WITHIN A PROGRAM, NOT ACROSS ONE. Two programs legitimately scope the same
        # apex (a vendor and its customer), and collapsing those would silently attribute one
        # program's finding to the other — and stamp the wrong researcher marker on the request.
        key = (handle, kind, seed or target)
        if key in seen:
            continue
        seen.add(key)
        scope.append(r)

    out = pathlib.Path(args.out)
    sp, ex = out.with_suffix(".scope.jsonl"), out.with_suffix(".excluded.jsonl")
    sp.write_text("".join(json.dumps(r, sort_keys=True) + "\n" for r in scope))
    ex.write_text("".join(json.dumps(r, sort_keys=True) + "\n" for r in excluded))

    kinds = Counter(r["kind"] for r in scope)
    progs = {r["program"] for r in scope}
    print(f"{args.platform}: {len(progs)} program(s), {len(scope)} scope row(s)", file=sys.stderr)
    for k in sorted(kinds):
        print(f"  {k:14} {kinds[k]}", file=sys.stderr)
    print(f"  {'-> scope':14} {sp}", file=sys.stderr)
    print(f"  {'-> excluded':14} {len(excluded)} rows, {ex}", file=sys.stderr)
    for k, v in sorted(dropped.items()):
        print(f"  dropped {k:9} {v}", file=sys.stderr)
    if not block:
        # THE MARKER IS WHAT MAKES THE TRAFFIC ATTRIBUTABLE. Unmarked scanning of 460 programs is
        # how a researcher gets their source address blocked and their account reviewed.
        print("  WARNING: no --header given — every probe will go out UNATTRIBUTED",
              file=sys.stderr)


if __name__ == "__main__":
    main()
