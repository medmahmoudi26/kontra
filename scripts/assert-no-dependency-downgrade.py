#!/usr/bin/env python3
"""Fail if a change LOWERS a dependency version in any go.mod.

    python3 scripts/assert-no-dependency-downgrade.py <base-ref>

WHY THIS EXISTS. `osv-scanner` reported `aws-sdk-go-v2/service/s3 1.96.0 -> 1.97.3` for one module,
and the fix was applied as `go get …/service/s3@v1.97.3` in every module that named the package.
`runtime/handler` was already on **1.105.0**, so that command DOWNGRADED it by eight minor versions
— `go get @vX` pins exactly X, in both directions, and says `upgraded`/`downgraded` in a line that
scrolls past among thirty others.

WHAT IT COST, because the shape of the failure is the argument for this file. The handler is the one
module a container build compiles with `GOWORK=off`, so its own go.mod is
the whole truth there, while every developer's `go build` uses the workspace maximum and compiles
something else. Nothing failed to build. Instead a real run reached the claim-check store and got

    kontra.store_blob  ->  not found: S3100Continue

nine times with exponential backoff until the Nexus operation hit ScheduleToClose — presenting as a
hung actor, four layers from the dependency line that caused it.

WHY A DOWNGRADE AND NOT A DISAGREEMENT. Requiring every module to name the same version of a shared
dependency would be a stricter and much more attractive rule, and it is not available: 22 of the 65
dependencies shared across these modules already disagree, because each go.mod records its own MVS
result. That is normal and not a defect. A DOWNGRADE is a defect regardless of what the other
modules say, and it needs no in-tree baseline — a pull request always has a base ref.
"""

from __future__ import annotations

import pathlib
import re
import subprocess
import sys

# `name version` at the start of a require line, with or without a trailing `// indirect`. The name
# must contain a dot before the first slash, which is what distinguishes a module path from the
# `go`/`toolchain` directives and from a `replace` arrow.
REQUIRE = re.compile(r"^\s*([a-zA-Z0-9._~-]+\.[a-zA-Z0-9._~/-]+)\s+(v[0-9][^\s]*)")

GO_MODS = [
    "cli/go.mod",
    "runtime/go/go.mod",
    "runtime/handler/go.mod",
    "sdk/go/go.mod",
    "workspaces/demo/actors/desync/go.mod",
]


def requires(text: str) -> dict[str, str]:
    """Module path → version, for every require line in a go.mod."""
    out: dict[str, str] = {}
    for line in text.splitlines():
        if line.lstrip().startswith(("//", "replace", "exclude", "retract")):
            continue
        m = REQUIRE.match(line)
        if m:
            out[m.group(1)] = m.group(2)
    return out


def sortable(version: str) -> tuple:
    """A comparable key for a Go module version.

    `+incompatible` is metadata and never orders. A PRE-RELEASE sorts BELOW the release it precedes
    (`v2.0.0-beta.8 < v2.0.0`), which is also what makes pseudo-versions order sensibly: they are
    pre-releases whose first field is a timestamp, so `v0.0.0-20260407…` < `v0.0.0-20260811…`.
    """
    core, _, pre = version.lstrip("v").partition("+")[0].partition("-")
    nums = tuple(int(p) if p.isdigit() else 0 for p in core.split("."))
    nums += (0,) * (3 - len(nums))
    if not pre:
        # No pre-release outranks every pre-release of the same core version.
        return (nums, (1,))
    fields = tuple(
        (0, int(f), "") if f.isdigit() else (1, 0, f) for f in re.split(r"[.\-]", pre)
    )
    return (nums, (0,) + fields)


def at(ref: str, path: str) -> str | None:
    """A file's contents at a git ref, or None when it does not exist there."""
    p = subprocess.run(["git", "show", f"{ref}:{path}"], capture_output=True, text=True)
    return p.stdout if p.returncode == 0 else None


def here(path: str) -> str | None:
    """A file's contents in the WORKING TREE, or None when it is not there.

    Not `git show HEAD:…`. On a runner the checkout IS the commit, so the two agree; on a laptop
    they do not, and a check that silently ignores the edit in front of you is one you learn to run
    only after committing — which is exactly when it is least useful.
    """
    p = pathlib.Path(path)
    return p.read_text(encoding="utf-8") if p.is_file() else None


def downgrades(
    base: str,
    paths: list[str] = GO_MODS,
    *,
    before_at=at,
    now_at=here,
) -> list[tuple[str, str, str, str]]:
    """`(go.mod, module, was, now)` for every version this tree lowers relative to `base`.

    Both readers are injected so the comparison can be tested on two in-memory trees — the
    alternative is a test that builds git history, which tests git.
    """
    found: list[tuple[str, str, str, str]] = []
    for path in paths:
        before = before_at(base, path)
        if before is None:
            continue  # a new module cannot have downgraded anything
        after = now_at(path)
        if after is None:
            continue  # a deleted module is a deletion, not a downgrade
        was, now = requires(before), requires(after)
        for name, now_v in now.items():
            was_v = was.get(name)
            if was_v is None or was_v == now_v:
                continue
            if sortable(now_v) < sortable(was_v):
                found.append((path, name, was_v, now_v))
    return found


def main(argv: list[str]) -> int:
    if len(argv) != 2 or argv[1] in ("-h", "--help"):
        print(__doc__ if argv[1:2] in ([], ["-h"], ["--help"]) else "", end="")
        print(f"usage: {argv[0].rsplit('/', 1)[-1]} <base-ref>", file=sys.stderr)
        return 2
    base = argv[1]
    found = downgrades(base)
    if not found:
        print(f"no dependency version is lower than at {base}")
        return 0
    for path, name, was, now in sorted(found):
        print(f"::error file={path}::{name} goes BACKWARDS: {was} -> {now}")
    print(
        "\nA lower version is almost never intended. `go get pkg@vX` pins exactly X in BOTH\n"
        "directions, so applying one module's advisory fix across every module that names the\n"
        "package will silently downgrade any module that was ahead. If it IS intended, say so in\n"
        "the commit message and remove the module from this script's list.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
