#!/usr/bin/env python3
"""Build ONE program's scope as a kontra Dataset, in the shape both actors can eat.

    python3 scripts/program-scope.py example-bounty --as the program --paid-only \
        --include api.example.com --out /tmp/scope

Writes two JSONL files — `<out>.scope.jsonl` and `<out>.excluded.jsonl` — for
`kontra db create`. It reads bbscope's Postgres directly, which is the same source
`scripts/extract-scope.sql` reads and the same one the `bbscope` actor holds a credential for.

── WHY THIS EXISTS BESIDE extract-scope.sql ────────────────────────────────────────────────────

`extract-scope.sql` + `normalize-scope.py` build `scope_paid`: EVERY paying program, in the shape
a crawler wants — `{target, seed, kind, platform, program, program_url, bounty}`. That shape has a
`seed` and no `host`, so `desync` cannot read it: its `Target` is
`{program, host, port, scheme, sni, host_header, endpoint, header_block, class}`.

The recon-enriched `scope_example` had the opposite problem — a `host` and no `seed` — which is why
`webcrawl` could never crawl it (`_crawl_seed` reads `unit.value.get("seed")`, gets "", and pushes
"not an http(s) url"), why `http_events_example` was never written, and why `exchanges_example` had to be
loaded by hand outside any Run.

So this emits the UNION. Units cross the wire as dicts — nothing constructs the declared
dataclass (sdk/python/kontra/actor.py:417 "nothing consumes them any more") — so the columns each
actor does not know about cost it nothing.

── WHAT IS IN SCOPE IS STILL DECIDED IN SQL ────────────────────────────────────────────────────

`classify()` is imported from normalize-scope.py rather than re-spelled, because it is the safety
boundary: `*.` is the only glob that expands, and two bugs of exactly that shape (a path trimmed
before the glob check, a CIDR's /8 trimmed as a path) were caught by its tests before an actor
ever ran. A second copy is a second place for that to regress.

── THE EXCLUSIONS ARE AN OUTPUT, NOT A FILTER ──────────────────────────────────────────────────

`bbscope_scope` carries `in_scope = 1` rows only, so the published dataset cannot answer "is this
host carved out?" — and the answer matters most exactly where a wildcard expands. `*.wavecell.com`
is a PAYING the program wildcard, and `the program.wavecell.com`, `feedback.wavecell.com` and `www.wavecell.com`
are each explicitly out of scope: subfinder will return all three. So the `in_scope = 0` rows are
written as their own Dataset, to be subtracted AFTER expansion, where the exclusion applies.
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import pathlib
import re
import sys

import psycopg2
import yaml

_HERE = pathlib.Path(__file__).resolve().parent

# normalize-scope.py has a hyphen in its name, so it is not importable by `import`.
_spec = importlib.util.spec_from_file_location("normalize_scope", _HERE / "normalize-scope.py")
_ns = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_ns)
classify = _ns.classify

# Categories that describe something with no HTTP surface. Dropped from the scope rows and
# reported, never silently: a mobile package id in a crawl seed list is a row that can only ever
# produce "not an http(s) url".
NON_HTTP = {"android", "ios", "binary", "code", "other"}


def connect():
    """bbscope's own DSN, with the password's `@` handled.

    The password contains an `@`, so a naive urlsplit reads the host as `<tail-of-password>@host`
    and psycopg2 fails with `could not translate host name`. lib/pq misparses it the same way,
    which is why the bbscope actor's DSN needs `%40`. Split on the LAST `@` instead of encoding,
    so this reads the operator's file as it is rather than requiring it be rewritten.
    """
    raw = yaml.safe_load(open(pathlib.Path.home() / ".bbscope.yaml"))["db_url"]
    m = re.match(r"^\w+://(.+)@([^@]+)$", raw)
    if not m:
        sys.exit("db_url in ~/.bbscope.yaml is not a postgres URL")
    user, pw = m.group(1).split(":", 1)
    host_port, db = m.group(2).split("/", 1)
    host, _, port = host_port.partition(":")
    return psycopg2.connect(host=host, port=int(port or 5432), user=user,
                            password=pw, dbname=db.split("?")[0])


def row(program, target, kind, seed, platform, handle, program_url, bounty, category, first, last):
    """One scope row, wide enough for `webcrawl.Seed` AND `desync.Target`."""
    host, scheme, port, endpoint = "", "https", 443, "/"
    if kind == "wildcard":
        # `classify` returns a wildcard's seed as the BARE APEX, not a URL — it is subfinder's
        # input, not a crawler's. Carry it as `host` (the column every other stage reads) and
        # blank the seed, so a wildcard row cannot reach `webcrawl` at all. Keying this off
        # `seed` being empty instead of off `kind` is the bug this replaces: "wavecell.com" is
        # truthy, the URL regex did not match it, and every wildcard row got `host = ""` — which
        # silently made the exclusion subtraction below match nothing.
        host = seed or (target[2:] if target.startswith("*.") else target)
        seed = ""
    elif seed:
        m = re.match(r"^(https?)://([^/?#]+)([^?#]*)", seed)
        if m:
            scheme = m.group(1)
            host, _, p = m.group(2).partition(":")
            port = int(p) if p else (443 if scheme == "https" else 80)
            endpoint = m.group(3) or "/"
    return {
        "program": program,
        "target": target,
        "kind": kind,
        "seed": seed,
        "host": host,
        "port": port,
        "scheme": scheme,
        "host_header": host,
        "sni": host,
        "endpoint": endpoint,
        "header_block": "",
        "class": "",
        "platform": platform,
        "handle": handle,
        "program_url": program_url,
        "bounty": bool(bounty),
        "category": category,
        "source": "bbscope",
        "first_seen_at": str(first or ""),
        "last_seen_at": str(last or ""),
    }


def _header_block(lines: list[str]) -> str:
    """Render header lines into desync's `${header_block}` slot, refusing anything malformed.

    ── THE SEPARATOR BUG THIS REPLACES ─────────────────────────────────────────────────────────

    The first spelling took one string and split it on ';'. A User-Agent contains a semicolon
    ("Mozilla/5.0 (X11; Linux x86_64) ..."), so it split mid-value and produced

        User-Agent: Mozilla/5.0 (X11\r\nLinux x86_64) ...

    — a CRLF injected into our OWN request. On a scanner whose entire job is detecting where a
    CRLF changes how a server frames a request, that is the worst possible defect: every probe
    would carry a real header injection, every baseline would be malformed, and the resulting
    observations would look like findings. Hence one flag per header and no in-string separator.

    ── AND THE GUARD, BECAUSE THE MARKER IS THE ONE THING THAT MUST BE WELL-FORMED ─────────────

    This block is what makes the traffic attributable to a named researcher. A marker that
    malforms the request is worse than no marker: it is unattributed AND hostile.
    """
    out = []
    for raw in lines:
        if "\r" in raw or "\n" in raw:
            sys.exit(f"--header {raw!r} contains a bare CR or LF")
        name, sep, value = raw.partition(":")
        if not sep or not name.strip() or not value.strip():
            sys.exit(f"--header {raw!r} is not `Name: value`")
        if not re.fullmatch(r"[A-Za-z0-9!#$%&'*+.^_`|~-]+", name.strip()):
            sys.exit(f"--header {raw!r} has a name that is not a valid HTTP token")
        out.append(f"{name.strip()}: {value.strip()}\r\n")
    return "".join(out)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("handle", help="the platform handle, e.g. example-bounty")
    ap.add_argument("--as", dest="program", required=True,
                    help="short program name; becomes the scope_<name> suffix and the `program` column")
    ap.add_argument("--paid-only", action="store_true",
                    help="keep only is_bbp=1 assets (a PER-ASSET narrowing, not the scope rule)")
    ap.add_argument("--include", action="append", default=[],
                    help="keep this target even when --paid-only would drop it; repeatable")
    ap.add_argument("--header", action="append", default=[], dest="headers",
                    help="one header line stamped on every desync request for this program, "
                         "e.g. 'X-PP-BB: HackerOne-medmahmoudi'. REPEAT the flag for more; there "
                         "is deliberately no in-string separator (see _header_block).")
    ap.add_argument("--out", required=True, help="path prefix for the two JSONL files")
    args = ap.parse_args()

    keep = {t.strip().lower() for t in args.include}
    block = _header_block(args.headers)
    conn = connect()
    cur = conn.cursor()
    cur.execute(
        """SELECT t.target, t.category, t.in_scope, t.is_bbp,
                  p.platform, p.handle, p.url, t.first_seen_at, t.last_seen_at
             FROM targets_raw t JOIN programs p ON p.id = t.program_id
            WHERE p.handle = %s AND p.disabled = 0 AND p.is_ignored = 0
            ORDER BY t.target""", (args.handle,))
    rows = cur.fetchall()
    if not rows:
        sys.exit(f"no rows for handle {args.handle!r} — is the bbscope DB populated?")

    scope, excluded = [], []
    seen = set()
    dropped = {"non-http": 0, "unpaid": 0, "unexpandable": 0}

    for target, category, in_scope, is_bbp, platform, handle, purl, first, last in rows:
        # Free text from the platform: at least one target begins with a literal TAB.
        target = (target or "").replace("\t", " ").replace("\n", " ").replace("\r", " ").strip()
        if not target:
            continue
        kind, seed = classify(target)

        if not in_scope:
            excluded.append({
                "program": args.program, "target": target, "kind": kind,
                "host": (target[2:] if target.startswith("*.") else
                         re.sub(r"^https?://", "", target).split("/")[0]),
                "category": category, "platform": platform, "handle": handle,
            })
            continue

        if category in NON_HTTP:
            dropped["non-http"] += 1
            continue
        if args.paid_only and not is_bbp and target.lower() not in keep:
            dropped["unpaid"] += 1
            continue

        r = row(args.program, target, kind, seed, platform, handle, purl, is_bbp,
                category, first, last)
        # THE MARKER RIDES ON THE SCOPE ROW, not on a run's params, because `header_block` is a
        # per-TARGET slot in desync's request template and the required header differs per
        # program — PayPal wants `X-PP-BB`, the program wants its username appended to `User-Agent`. A
        # row that names its own program can carry its own marker; a run-wide param could not,
        # and a scan that mixes two programs would stamp one of them wrongly.
        if args.headers:
            r["header_block"] = block
        if kind == "unexpandable":
            # KEPT AND MARKED, never dropped: an unexpandable glob is surface somebody scoped, and
            # a triage queue is the right place for it. It carries an empty seed so nothing routes
            # it to a crawler or an enumerator.
            dropped["unexpandable"] += 1
        key = (r["kind"], r["seed"] or r["target"])
        if key in seen:
            continue
        seen.add(key)
        scope.append(r)

    out = pathlib.Path(args.out)
    sp, ex = out.with_suffix(".scope.jsonl"), out.with_suffix(".excluded.jsonl")
    sp.write_text("".join(json.dumps(r, sort_keys=True) + "\n" for r in scope))
    ex.write_text("".join(json.dumps(r, sort_keys=True) + "\n" for r in excluded))

    kinds = {}
    for r in scope:
        kinds[r["kind"]] = kinds.get(r["kind"], 0) + 1
    print(f"{args.handle} -> {args.program}", file=sys.stderr)
    for k in sorted(kinds):
        print(f"  {k:14} {kinds[k]}", file=sys.stderr)
    print(f"  {'scope rows':14} {len(scope)}  -> {sp}", file=sys.stderr)
    print(f"  {'excluded':14} {len(excluded)}  -> {ex}", file=sys.stderr)
    for k, v in dropped.items():
        if v:
            print(f"  dropped {k:9} {v}", file=sys.stderr)
    if keep:
        got = {r["target"].lower() for r in scope} & keep
        missing = keep - got
        # A --include that matched nothing is a typo that silently narrows the scan.
        if missing:
            sys.exit(f"--include named {sorted(missing)} but no such in-scope target exists")
        print(f"  {'included':14} {sorted(got)}", file=sys.stderr)


if __name__ == "__main__":
    main()
