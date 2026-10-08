#!/usr/bin/env python3
"""Fail on a REACHABLE vulnerability unless it is allowlisted, with a reason and an expiry.

    govulncheck -format json ./... | python3 scripts/vulngate.py --module cli

Reads govulncheck's JSON on stdin, prints every called symbol it found, and exits 1 unless each
advisory is covered by an entry in `.github/vuln-allow.json` for this module.

WHY THIS EXISTS RATHER THAN `govulncheck ./...` ALONE. Two advisories this repository is exposed to
have no fixed version. `github.com/docker/docker` ships the daemon and the client in one
`+incompatible` module, so a client user's package `init` reaches daemon symbols and govulncheck
reports it; the only module carrying a fix is `github.com/moby/moby/v2`, which is a different import
path and not a version bump. A gate that cannot be made green teaches everyone to ignore it — the
exact failure `security.yml`'s own header warns about — so the decision goes in the tree, in a file
a pull request has to touch, instead of into a `|| true`.

AND AN ALLOWLIST THAT CANNOT GO STALE, which is the half that makes this different from a suppression
file. Two refusals keep it honest:

  * AN ENTRY THAT MATCHES NOTHING FAILS. The vulnerability is gone, or was renamed, or the dependency
    was dropped — and a suppression nobody can see is still suppressing. Left alone, a file like this
    only ever grows, and the entry that silences the next real finding was added for a finding that
    no longer exists.
  * AN ENTRY PAST ITS `review_by` FAILS. "No fix available" is a statement about a date. `moby/v2`
    leaving beta turns both docker entries here from unfixable into a migration, and nothing would
    otherwise tell us.

Both are deliberate build breaks with the id in the message, not warnings.
"""

from __future__ import annotations

import argparse
import datetime
import json
import sys
from typing import Any, Iterator

# govulncheck's JSON is a stream of concatenated objects, not an array, so it cannot be `json.load`ed.
DECODER = json.JSONDecoder()


def messages(text: str) -> Iterator[dict[str, Any]]:
    """Decode govulncheck's concatenated JSON objects in order."""
    i, n = 0, len(text)
    while i < n:
        while i < n and text[i].isspace():
            i += 1
        if i >= n:
            return
        obj, end = DECODER.raw_decode(text, i)
        if isinstance(obj, dict):
            yield obj
        i = end


def called(msgs: Iterator[dict[str, Any]]) -> dict[str, list[str]]:
    """Advisory id → the symbols OUR code calls, deduped and ordered.

    ONLY SYMBOL-LEVEL FINDINGS COUNT. govulncheck emits a finding per level for the same advisory:
    module (the dependency is required), package (it is imported), and symbol (something is called).
    Only the last has a `function` on its first trace frame, and only the last is what "reachable"
    means — gating on the others would report every transitive dependency with any advisory and
    report nothing useful.
    """
    out: dict[str, list[str]] = {}
    for m in msgs:
        finding = m.get("finding")
        if not isinstance(finding, dict):
            continue
        osv = finding.get("osv")
        trace = finding.get("trace")
        if not osv or not isinstance(trace, list) or not trace:
            continue
        frame = trace[0]
        if not isinstance(frame, dict) or not frame.get("function"):
            continue
        where = ".".join(p for p in (frame.get("package"), frame.get("function")) if p)
        out.setdefault(osv, [])
        if where and where not in out[osv]:
            out[osv].append(where)
    return out


def fixed_versions(msgs: Iterator[dict[str, Any]]) -> dict[str, str]:
    """Advisory id → the version it is fixed in, as govulncheck reports it ("" when there is none)."""
    out: dict[str, str] = {}
    for m in msgs:
        finding = m.get("finding")
        if isinstance(finding, dict) and finding.get("osv"):
            out.setdefault(finding["osv"], finding.get("fixed_version") or "")
    return out


def entries_for(allow: dict[str, Any], module: str) -> dict[str, dict[str, Any]]:
    """The allowlist entries that apply to `module`, keyed by advisory id."""
    out: dict[str, dict[str, Any]] = {}
    for entry in allow.get("allow", []):
        if module in entry.get("modules", []):
            out[entry["id"]] = entry
    return out


def evaluate(
    reachable: dict[str, list[str]],
    allow: dict[str, Any],
    module: str,
    today: datetime.date,
) -> tuple[bool, list[str]]:
    """`(ok, lines)`. `lines` are printed whether or not the gate passes."""
    applicable = entries_for(allow, module)
    problems: list[str] = []
    lines: list[str] = []

    for osv in sorted(reachable):
        symbols = reachable[osv]
        entry = applicable.get(osv)
        if entry is None:
            problems.append(
                f"::error::{osv} is REACHABLE in {module} and is not allowlisted. "
                f"Fix it, or add an entry to .github/vuln-allow.json with a `why` and a `review_by`. "
                f"Called: {', '.join(symbols) or 'unknown'}"
            )
            continue
        lines.append(f"  allowed  {osv}  {entry.get('package', '?')}  — {entry.get('why', '')}")
        for s in symbols:
            lines.append(f"             calls {s}")

    for osv, entry in sorted(applicable.items()):
        if osv not in reachable:
            problems.append(
                f"::error::{osv} is allowlisted for {module} but govulncheck no longer reports it. "
                f"Remove the entry — a suppression for a vulnerability that is gone is what silences "
                f"the next real one."
            )
            continue
        by = entry.get("review_by")
        if not by:
            problems.append(f"::error::{osv}'s allowlist entry has no `review_by`, which is required.")
            continue
        try:
            deadline = datetime.date.fromisoformat(by)
        except ValueError:
            problems.append(f"::error::{osv}'s `review_by` is '{by}', which is not an ISO date.")
            continue
        if deadline < today:
            problems.append(
                f"::error::{osv}'s allowlist entry expired on {by}. Re-review it: "
                f"{entry.get('fix_requires', 'check whether a fix now exists')}"
            )

    return not problems, lines + problems


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--module", required=True, help="the module path this report is for, e.g. `cli`")
    ap.add_argument("--allow", default=".github/vuln-allow.json")
    ap.add_argument("--today", help="override for tests; ISO date")
    args = ap.parse_args(argv)

    raw = sys.stdin.read()
    if not raw.strip():
        print(f"::error::govulncheck produced no output for {args.module}", file=sys.stderr)
        return 1

    msgs = list(messages(raw))
    reachable = called(iter(msgs))
    with open(args.allow, encoding="utf-8") as fh:
        allow = json.load(fh)

    today = datetime.date.fromisoformat(args.today) if args.today else datetime.date.today()
    ok, lines = evaluate(reachable, allow, args.module, today)

    print(f"{args.module}: {len(reachable)} reachable advisor{'y' if len(reachable) == 1 else 'ies'}")
    for line in lines:
        print(line)
    if ok and not reachable:
        print("  none")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
