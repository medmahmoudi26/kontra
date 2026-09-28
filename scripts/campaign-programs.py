#!/usr/bin/env python3
"""Print a scope Dataset's program handles, ordered by asset count — the campaign's `programs` input.

    python3 scripts/campaign-programs.py --scope scope_h1paid            # a JSON array
    python3 scripts/campaign-programs.py --scope scope_h1paid --limit 5  # the five biggest

WHY THE CAMPAIGN CANNOT DO THIS ITSELF. A Dataset page comes back as a REF, and turning a ref into
rows is `kontra.fetch_blob` — an activity only an ACTOR HOST registers, on `<actor>-<version>`. The
campaign resolves its program list BEFORE any fleet exists, so no Actor is serving and no queue can
answer. The SDK does not refuse: it builds the queue name from two empty strings, schedules onto the
literal `"-shared"`, and the workflow waits there forever with no error anywhere.

Passing the list in is also better provenance. The run's Input card then names exactly which
programs the campaign covered, rather than a filter a reader has to re-run to reconstruct.

ORDERED BY ASSET COUNT DESCENDING, so `--limit` takes the programs that actually stress the
pipeline rather than whichever sorted first alphabetically.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.request

API = os.environ.get("KONTRA_ORCHESTRATOR_URL", "http://localhost:8088")


def _token() -> str:
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


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--scope", default="scope_h1paid")
    ap.add_argument("--limit", type=int, default=0, help="0 is uncapped")
    ap.add_argument("--counts", action="store_true", help="print handle and asset count, not JSON")
    a = ap.parse_args()

    sql = (f'SELECT program, count(*) AS n FROM "{a.scope}" '
           f"WHERE coalesce(program, '') <> '' GROUP BY 1 ORDER BY n DESC, program")
    body = json.dumps({"sql": sql, "limit": 5000}).encode()
    req = urllib.request.Request(
        f"{API}/api/datasets/query", data=body, method="POST",
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {_token()}"})
    with urllib.request.urlopen(req, timeout=120) as r:
        doc = json.load(r)

    rows = doc.get("rows") or []
    if a.limit:
        rows = rows[: a.limit]
    if a.counts:
        for program, n in rows:
            print(f"{n:>6}  {program}")
        print(f"\n{len(rows)} program(s), {sum(n for _, n in rows)} asset(s)", file=sys.stderr)
        return
    print(json.dumps([p for p, _ in rows]))


if __name__ == "__main__":
    main()
