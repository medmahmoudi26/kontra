#!/usr/bin/env python3
"""Normalise a bbscope paid-in-scope export into kontra target rows.

bbscope is a TOOL, not an actor: it holds a database credential, does no fan-out worth
distributing, and its output is an input to kontra rather than a stage of it. So the scope pass
is this script plus `kontra db anew`, and the result lives in the DuckLake catalog where every
other dataset lives.

Reads TSV on stdin: target <TAB> platform <TAB> program_handle <TAB> program_url [<TAB> is_bbp]
Writes JSONL on stdout: {target, seed, kind, platform, program, program_url, bounty}

`bounty` is HackerOne's PER-ASSET `eligible_for_bounty`, carried through so a run can narrow
to paying assets at dispatch time without re-deriving scope. It is deliberately NOT a scope
filter here: scope is decided in scripts/extract-scope.sql, which keeps every asset a paying
program accepts submissions on. The two flags are genuinely different — the program publishes
`api.example.com` and `*.example.com` as in-scope, Critical, and worth no bounty. Absent (a 4-column
line) it defaults to False.

The classification is the safety boundary, and it refuses more than it accepts:

    url          http(s)://…              -> crawl verbatim; the path is part of what was scoped
    wildcard     *.example.com            -> ENUMERATE example.com (subfinder)
    domain       example.com[/path]       -> crawl https://…, path preserved
    ip           1.2.3.4                  -> crawl http://<ip>
    unexpandable *uat.x, dev*.x, x/a/*,   -> EMITTED with an empty seed, never scanned
                 CIDRs, bare hostnames

`*.` is the ONLY glob that unambiguously means "every subdomain of this apex". A prefix or infix
glob names SOME hosts and a path glob scopes ONE PATH — expanding either would scan things the
program did not publish. Two bugs of exactly that shape were caught by tests before the actor
version ever ran: a path trimmed before the glob check turned `x.com/hz/mycd/*` into the whole
domain, and a `/8` trimmed as a path turned a 16M-host CIDR into a single IP. Both are pinned by
tests/test_normalize_scope.py.
"""
from __future__ import annotations

import ipaddress
import json
import sys

# Only in_scope=1 AND is_bbp=1 rows should ever reach this script — that filter belongs in the
# SQL, where it cannot be forgotten by a caller. This is the second line of defence, not the
# first: it decides HOW to scan, never WHETHER something is in scope.

def classify(raw: str) -> tuple[str, str]:
    """Return (kind, seed). An empty seed means "never scan this"."""
    t = (raw or "").strip().lower()
    if not t:
        return "unexpandable", ""

    if t.startswith(("http://", "https://")):
        # A glob anywhere in a URL makes both host and path ambiguous.
        return ("unexpandable", "") if "*" in t else ("url", t)

    if t.startswith("*."):
        apex = t[2:]
        if any(c in apex for c in "*/?#") or "." not in apex:
            return "unexpandable", ""
        return "wildcard", apex

    # Any other glob — prefix (*uat.x), infix (dev*.x) or path (x/a/*) — is unexpandable.
    # Checked BEFORE the path is trimmed, or a path-scoped entry collapses to its apex.
    if "*" in t:
        return "unexpandable", ""

    # CIDR before path trimming: "10.0.0.0/8" would otherwise lose its "/8" as a path and parse
    # as the single host 10.0.0.0. A range needs an expansion policy we deliberately lack.
    try:
        ipaddress.ip_network(t, strict=False)
        if "/" in t:
            return "unexpandable", ""
    except ValueError:
        pass

    host = t
    for sep in "/?#":
        i = host.find(sep)
        if i >= 0:
            host = host[:i]
    try:
        ipaddress.ip_address(host)
        return "ip", "http://" + host
    except ValueError:
        pass

    if "." not in host:
        return "unexpandable", ""
    return "domain", "https://" + t


def main() -> None:
    seen: set[tuple[str, str]] = set()
    counts: dict[str, int] = {}
    for line in sys.stdin:
        parts = line.rstrip("\n").split("\t")
        if len(parts) < 4:
            continue
        target, platform, program, program_url = parts[0], parts[1], parts[2], parts[3]
        bounty = len(parts) > 4 and parts[4].strip() == "1"
        kind, seed = classify(target)
        counts[kind] = counts.get(kind, 0) + 1
        # De-duplicate on (seed, kind): the same asset is routinely published by a program's paid
        # and VDP variants, and by several programs at once.
        key = (seed or target, kind)
        if key in seen:
            continue
        seen.add(key)
        print(json.dumps({
            "target": target, "seed": seed, "kind": kind,
            "platform": platform, "program": program, "program_url": program_url,
            "bounty": bounty,
        }, sort_keys=True))
    for k in sorted(counts):
        print(f"  {k:14} {counts[k]}", file=sys.stderr)
    print(f"  {'unique rows':14} {len(seen)}", file=sys.stderr)


if __name__ == "__main__":
    main()
