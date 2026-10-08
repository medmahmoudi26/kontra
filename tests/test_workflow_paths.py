"""Every directory a CI job walks into exists in this checkout.

WHY THIS EXISTS. This is the FOURTH time in one restructure that a path broke because it is not
spelled anywhere — it is ACCUMULATED. `tests/test_conformance_tree.py` catches the three that were
built out of separate arguments in source (`filepath.Join(root, "orchestrator", "src")`,
`ROOT / "sdk" / "conformance"`, a climb at the wrong depth). This catches the shell's version of the
same shape:

    cd backend        # was `cd orchestrator`, and the rename fixed it
    pnpm exec tsc
    cd web            # was `orchestrator/web`, and the rename could not see it

Nothing in that file ever contains the string `orchestrator/web`, so the sweep that moved
`orchestrator/web` to `frontend/` matched nothing here, both hops looked individually plausible, and
the parity gate died on `cd: web: No such file or directory` — after a five-minute install, in the
one job that boots a real appliance, which is the slowest possible place to learn it.

WHAT IT CHECKS. It resolves each step's working directory the way the shell does: the job's
`defaults.run.working-directory`, overridden by the step's own, then each `cd` in the script applied
in order to what the previous one left behind. Every resulting directory must exist.

WHAT IT DOES NOT CHECK. A `cd` whose argument interpolates anything (`$RUNNER_TEMP`, `${{ ... }}`,
a glob) is skipped rather than guessed at — the value is not knowable from the checkout, and a test
that invents one would fail on correct workflows. That is a real hole, and it is the right hole:
every path this bug class has produced was a literal.
"""
from __future__ import annotations

import os.path
import pathlib
import re

import pytest
import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
WORKFLOWS = ROOT / ".github" / "workflows"

#: A whole-line `cd <literal>`, which is the only form these scripts use. A `cd` joined to another
#: command by `&&` or `;` is deliberately not matched — it does not persist to the next line, so it
#: cannot accumulate, which is the failure this file is about.
CD = re.compile(r"^\s*cd\s+([^\s;&|]+)\s*$")

#: Not knowable from a checkout: a shell or Actions expression, a glob, or a home-relative path.
UNRESOLVABLE = re.compile(r"[$*?~]")


def _workflows() -> list[pathlib.Path]:
    return sorted(p for p in WORKFLOWS.glob("*.y*ml"))


def _steps(job: dict) -> list[dict]:
    return [s for s in (job.get("steps") or []) if isinstance(s, dict)]


def _default_dir(spec: dict) -> str | None:
    run = ((spec.get("defaults") or {}).get("run") or {})
    return run.get("working-directory")


def _walk(script: str, start: pathlib.PurePosixPath) -> list[tuple[str, pathlib.PurePosixPath]]:
    """Where each `cd` in one script lands, in order, starting from `start`."""
    landings: list[tuple[str, pathlib.PurePosixPath]] = []
    cwd = start
    for line in script.splitlines():
        m = CD.match(line)
        if not m:
            continue
        arg = m.group(1).strip("'\"")
        if UNRESOLVABLE.search(arg) or arg == "-":
            # The cwd is no longer knowable, so neither is anything after it in this script.
            return landings
        cwd = pathlib.PurePosixPath(os.path.normpath(str(cwd / arg) if not arg.startswith("/") else arg))
        landings.append((line.strip(), cwd))
    return landings


#: `mkdir foo`, `mkdir -p a/b` — a directory the job MAKES, so its absence from the checkout is
#: correct rather than a bug. Flags are dropped; every operand counts.
MKDIR = re.compile(r"^\s*mkdir\s+(.*)$")

#: `git clone <url> <dir>` MAKES A DIRECTORY TOO, and `wiki.yml` is why this is here: it clones the
#: repository's own wiki into `wiki/` and the next step does `cd wiki`, which no checkout can
#: contain. Recognition and not proof, exactly as the `actions/checkout` case above — but the
#: alternative is a permanent failure on a job that is correct, which is how an assertion gets
#: deleted rather than fixed. Matched anywhere in the line because the real one is inside `if ! …`.
GIT_CLONE = re.compile(r"\bgit\s+clone\s+(.*)$")

#: Where a shell word stops being an argument: a redirection, a separator, a terminator.
CLONE_STOP = re.compile(r"(?:[;&|]|\d?>)")


def _makes(step: dict) -> set[str]:
    """Directories this step creates, which later steps may therefore walk into.

    Three producers, because all three appear in these workflows: `actions/download-artifact`,
    whose `path:` is materialised by the action; `actions/checkout` with a `path:`, which is how
    ANOTHER REPOSITORY arrives since ADR 0038; and a plain `mkdir` in a script. Without this the
    guard would demand `release/` from the checkout — a false positive that would make it noise,
    and a guard nobody trusts is worse than none.

    THE CHECKOUT CASE WAS ADDED RATHER THAN WORKED AROUND, and it is worth being exact about what
    that buys. A directory another repository is checked out into cannot be VERIFIED from this
    checkout — no rule here can — so this is recognition, not proof. What it avoids is the shortcut:
    writing the hop as `cd $GITHUB_WORKSPACE/_sibling/kontra-console` would have satisfied
    `UNRESOLVABLE`, and `_walk` ABANDONS A SCRIPT at the first unresolvable `cd` — so every later
    hop in that step would have gone unchecked too, silently. A literal path keeps the walk going,
    and ties the name to the one step that declares it.
    """
    made: set[str] = set()
    uses = str(step.get("uses") or "")
    if "download-artifact" in uses or "actions/checkout" in uses:
        target = str((step.get("with") or {}).get("path") or "").strip()
        if target and not UNRESOLVABLE.search(target):
            made.add(os.path.normpath(target))
    script = step.get("run")
    if isinstance(script, str):
        for line in script.splitlines():
            m = MKDIR.match(line)
            if m:
                for word in m.group(1).split():
                    if word.startswith("-") or UNRESOLVABLE.search(word):
                        continue
                    made.add(os.path.normpath(word.strip("'\"")))
                continue
            c = GIT_CLONE.search(line)
            if c:
                rest = CLONE_STOP.split(c.group(1), 1)[0]
                words = [w.strip("'\"") for w in rest.split() if not w.startswith("-")]
                # TWO OPERANDS OR NOTHING. `git clone <url>` alone derives the directory from the
                # URL's basename, and the URL is the operand carrying `${{ secrets… }}` — so the
                # name would be a guess about a string this file cannot resolve. An explicit
                # destination is the only case worth recognising.
                if len(words) >= 2 and not UNRESOLVABLE.search(words[-1]):
                    made.add(os.path.normpath(words[-1]))
    return made


def _targets() -> list[tuple[str, str, pathlib.PurePosixPath]]:
    """(workflow, what named it, directory) for every place a job runs a command."""
    out: list[tuple[str, str, pathlib.PurePosixPath]] = []
    for wf in _workflows():
        spec = yaml.safe_load(wf.read_text(encoding="utf-8")) or {}
        top = _default_dir(spec)
        for job_name, job in (spec.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            base = _default_dir(job) or top or "."
            # Grows as the job runs: a step may legitimately enter what an earlier one made.
            made: set[str] = set()
            for i, step in enumerate(_steps(job)):
                here = step.get("working-directory") or base
                if UNRESOLVABLE.search(str(here)):
                    made |= _makes(step)
                    continue
                label = step.get("name") or f"step {i}"
                where = f"{wf.name}:{job_name}: {label}"
                start = pathlib.PurePosixPath(os.path.normpath(str(here)))
                if step.get("working-directory") and str(start) not in made:
                    out.append((wf.name, f"{where} — working-directory", start))
                script = step.get("run")
                if isinstance(script, str):
                    for line, landed in _walk(script, start):
                        if str(landed) in made:
                            continue
                        out.append((wf.name, f"{where} — `{line}`", landed))
                made |= _makes(step)
    return out


def test_there_are_workflows_to_check() -> None:
    """Without this, every assertion below passes by having nothing to say."""
    assert _workflows(), f"no workflow files under {WORKFLOWS.relative_to(ROOT)}"
    assert _targets(), "no job names a directory, which cannot be right"


@pytest.mark.parametrize("_wf, where, target", _targets(), ids=lambda v: None)
def test_every_working_directory_exists(_wf: str, where: str, target: pathlib.PurePosixPath) -> None:
    """A job that walks into a directory this checkout does not have."""
    resolved = (ROOT / str(target)).resolve()
    assert resolved.is_dir(), f"{where} lands in `{target}`, which does not exist"
    assert ROOT in resolved.parents or resolved == ROOT, f"{where} escapes the checkout: `{target}`"
