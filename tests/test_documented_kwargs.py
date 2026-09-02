"""Every keyword argument this repo tells somebody to type is a keyword the function takes.

THE CLI HALF OF THIS ALREADY EXISTS and the SDK half did not, which is precisely how the gap was
found. `cli/fleet_documented_flags_test.go` pins every `--flag` in a documented `kontra fleet …`
line against `fleetFlagSet`, and it caught `--campaign` and `--role` in three copy-pasteable
places. It cannot see Python, so `README.md`'s one front-page fleet snippet went on reading

    async with fleet.up(role="dns", machines=4, ...) as f:

long after `role=` was deleted — a `TypeError` on the first line a new reader would run, on the
page they would read first. Slice 09 fixed the line and recorded that nothing covered the class.
This is that cover.

WHY A SIGNATURE AND NOT A REGEX OVER THE SOURCE. `inspect.signature` is the function's own answer,
so a rename cannot leave this file agreeing with a list beside it. ADR 0035 rule two is about the
same failure one level up: a contract checked against a copy of itself is checked against nothing.

WHAT THIS DELIBERATELY DOES NOT DO. It does not check VALUES, only spellings, and it does not
follow `**kwargs`. A function that takes `**kwargs` is exempt and says so, rather than being
silently waved through by a check that could not see it either way.
"""

from __future__ import annotations

import ast
import inspect
import re
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent

#: The documented surfaces. A person reads these and types what they see.
#:
#: `.kontra/workflows/` IS GITIGNORED (`.gitignore:96`), and that is stated here rather than left to
#: be discovered. The path is real on a developer's box — the nscheck workflow lives there — so the
#: glob earns its place locally. It matches NOTHING in a fresh worktree or on CI, where the
#: directory does not exist at all, and a reader who assumed otherwise would credit this file with
#: covering the flagship workflow on every run. It does not. The `>= 5` guard below is carried
#: entirely by the other globs, deliberately, so this one going empty can never be what makes the
#: sweep vacuous.
DOC_GLOBS = (
    "README.md",
    "docs/wiki/*.md",
    # `examples/` LEFT THIS REPOSITORY (ADR 0038) and three globs pointed into it. They matched
    # nothing, which `test_the_examples_still_parse` caught only because it counts what it parsed —
    # the `>= 5` guard below was already satisfied by README.md and the wiki, so the sweep would
    # otherwise have gone on reporting success over a shrinking set of files.
    "testdata/workflows/*/description.md",
    "testdata/workflows/*/workflow.py",
    ".kontra/workflows/*/workflow.py",
)

#: `call spelling` -> the callable it resolves to, as `(module, dotted attribute)`. Kept explicit
#: rather than discovered, because a discovery pass that found nothing would report exactly like a
#: clean sweep.
#:
#: THE `f.` ENTRIES ARE METHODS ON THE SCOPE, and they are here because that is how the README and
#: every example spell them: `f.place("nscheck", "0.1.0", sessions=8)`. A keyword documented on a
#: method is exactly as typeable, and exactly as much of a `TypeError`, as one documented on a
#: function — `role=` on `fleet.up()` is what taught this file its lesson and there is nothing about
#: it that was special to module-level callables. The attribute is dotted so the resolution follows
#: the class rather than a second copy of the signature.
CALLS = {
    "fleet.up": ("actorkit.fleet", "up"),
    "fleet.hold": ("actorkit.fleet", "hold"),
    "f.place": ("actorkit.fleet", "Fleet.place"),
    "f.ready": ("actorkit.fleet", "Fleet.ready"),
    "catalog.actor": ("actorkit.catalog", "actor"),
    "catalog.dataset": ("actorkit.catalog", "dataset"),
}

KWARG = re.compile(r"\b([a-z_][a-z0-9_]*)\s*=")

#: What may precede a call spelling. WITHOUT IT `f.place(` MATCHES INSIDE `self.place(`, because
#: `re.escape("f.place")` is a substring of it — and the same hazard reaches every short spelling
#: this map will grow. A dot is excluded as well as a word character so `other.f.place(` is not read
#: as this call either.
BOUNDARY = r"(?:^|[^\w.])"


def _documented_files() -> list[Path]:
    found: list[Path] = []
    for glob in DOC_GLOBS:
        found.extend(sorted(REPO.glob(glob)))
    # THE SWEEP MUST FIND SOMETHING. Every glob is relative to the repo root, and a reshuffle that
    # moved `examples/` would leave this walking an empty set and reporting success over zero files.
    assert len(found) >= 5, f"only {len(found)} documented files found ({found}) — the globs no longer match this repo"
    return found


def _accepted(dotted: str) -> set[str] | None:
    """The parameter names, or None when the callable takes **kwargs and cannot refuse anything."""
    module_name, attr = CALLS[dotted]
    module = pytest.importorskip(module_name)
    fn: object | None = module
    for part in attr.split("."):
        fn = getattr(fn, part, None)
        if fn is None:
            return set()
    params = inspect.signature(fn).parameters
    if any(p.kind is inspect.Parameter.VAR_KEYWORD for p in params.values()):
        return None
    return {
        name
        for name, p in params.items()
        if p.kind in (inspect.Parameter.POSITIONAL_OR_KEYWORD, inspect.Parameter.KEYWORD_ONLY)
        # `self` is not a keyword anybody types, and leaving it in would accept `self=` as
        # documented on any method here.
        and name != "self"
    }


def _calls_in(text: str) -> list[tuple[str, str, int]]:
    """(dotted name, argument text, 1-based line) for every documented call we pin.

    Brace-matched rather than line-matched: the README's own example spans one line but
    `examples/.../workflow.py` wraps its arguments across five, and a line-at-a-time reader would
    check the first and miss the rest — the shape of miss this whole file exists to prevent.
    """
    out: list[tuple[str, str, int]] = []
    for dotted in CALLS:
        for m in re.finditer(BOUNDARY + re.escape(dotted) + r"\s*\(", text):
            depth, i = 0, m.end() - 1
            while i < len(text):
                if text[i] == "(":
                    depth += 1
                elif text[i] == ")":
                    depth -= 1
                    if depth == 0:
                        break
                i += 1
            out.append((dotted, text[m.end() : i], text[: m.start()].count("\n") + 1))
    return out


@pytest.mark.parametrize("dotted", sorted(CALLS))
def test_the_signature_is_readable_and_not_empty(dotted: str) -> None:
    """Without this, an import failure would empty every set below and pass everything."""
    accepted = _accepted(dotted)
    if accepted is None:
        pytest.skip(f"{dotted} takes **kwargs and refuses no keyword")
    assert accepted, f"{dotted} reports no parameters — the assertions below would accept anything"


def test_every_documented_keyword_argument_exists() -> None:
    dead: list[str] = []
    checked = 0

    for path in _documented_files():
        text = path.read_text(encoding="utf-8", errors="replace")
        for dotted, args, line in _calls_in(text):
            accepted = _accepted(dotted)
            if accepted is None:
                continue
            for kw in KWARG.findall(args):
                checked += 1
                if kw not in accepted:
                    dead.append(
                        f"{path.relative_to(REPO)}:{line} documents {dotted}({kw}=…), "
                        f"which it does not take. It accepts: {', '.join(sorted(accepted))}"
                    )

    # THE SECOND VACUITY GUARD. The regexps above are the kind that stop matching after an
    # innocuous formatting change, and a zero-match sweep reports exactly like a clean one.
    assert checked > 0, (
        "no documented keyword argument was found in any file — the call or kwarg pattern has "
        "stopped matching and this test is now asserting nothing"
    )
    # ADR 0040 IS ACCEPTED AND NOT YET CARRIED OUT, which is the one case where documentation
    # legitimately runs ahead of the signature. It renames the three axes to
    # `machines`/`containers`/`workers` — `f.place(containers=3, workers=8)` is its own example —
    # and its consequences say why it has not landed: the rename moves the `KONTRA_WORKER` label
    # and the `<name>-<version>-sessions` task queue, so a Fleet must be DRAINED before upgrading
    # or a Warden starts duplicates it cannot see.
    #
    # LISTED, NOT SUPPRESSED, and the list is checked in both directions. An entry that starts
    # existing must be deleted from here — otherwise this file would keep excusing a keyword that
    # is now real, and the next genuine drift behind the same name would be invisible.
    pending = {("f.place", "containers")}
    for call, kw in sorted(pending):
        accepted = _accepted(call)
        assert accepted is None or kw not in accepted, (
            f"{call}({kw}=…) exists now, so ADR 0040 has landed — delete it from `pending` here, "
            "which is what keeps this test honest about everything else."
        )
    excused = [d for d in dead if any(f"{c}({k}=" in d for c, k in pending)]
    dead = [d for d in dead if d not in excused]

    assert not dead, (
        "documentation names keyword arguments that do not exist:\n  "
        + "\n  ".join(dead)
        + "\n\nA person following this gets a TypeError on the first line they run. If the "
        "parameter was renamed, the documentation has to move with it."
    )


def test_the_examples_still_parse() -> None:
    """A workflow example that does not compile is a broken instruction with no runtime to catch it."""
    parsed = 0
    for path in _documented_files():
        if path.suffix != ".py":
            continue
        ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        parsed += 1
    assert parsed > 0, "no example workflow was parsed — the .py globs match nothing"
