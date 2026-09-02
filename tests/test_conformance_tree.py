"""Every corpus is driven, and every driver's path resolves.

WHY THIS EXISTS. Five times in this restructure a path stopped resolving, and not once did it fail
loudly in a way anybody noticed at the time. Each one is spelled in no file — it is ASSEMBLED, so a
sweep for the old directory matches nothing and every fragment reads correctly on its own:

  * `filepath.Join(root, "orchestrator", "src")` — a path built from SEPARATE ARGUMENTS, invisible
    to a regex sweep over `orchestrator/`. Fifteen Python tests were red for a day.
  * `ROOT / "sdk" / "conformance"` — the same shape in pathlib's operator spelling, which a sed for
    `"sdk", "conformance"` does not match either. Two more.
  * `filepath.Join("..", "..", "..", ...)` — right destination, wrong DEPTH, because the file moved
    one level down and the climb did not.
  * `cd backend` … `cd web` across two lines of one CI script. Nothing in that file ever contained
    the string `orchestrator/web`, so the rename was a no-op there; the parity gate died after a
    five-minute install. `tests/test_workflow_paths.py` is the guard for that one.
  * `Component.Path "backend/node_modules"` in the bundle manifest — the reverse case, where the
    rewrite was WRONG because that path describes the extracted artifact and not the repo, and
    `backend/node_modules` is the right answer twice elsewhere in the same file. Eight appliance
    jobs each built a correct bundle to report a hash.

A missing corpus is the worst of the three, because a driver that cannot read its fixture is one
edit away from being a driver that skips. So this walks the tree and checks both directions: no
driver names a corpus that is not there, and no corpus sits with nobody asserting it.

It is deliberately a text scan, which is the thing ADR 0035 rule two says not to build a CONTRACT
out of. This is not a contract — it is an inventory, and the failure it catches is a path, which is
the one thing a substring genuinely is.
"""
from __future__ import annotations

import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[1]
CORPUS_DIR = ROOT / "conformance"

#: Where a driver could live. `node_modules` and build output are not source.
SKIP = {".git", "node_modules", "dist", "__pycache__", "build", "tmp", ".claude", "_gen"}

#: `conformance/<name>.json`, in any of the spellings a driver uses — a slash path, a pathlib
#: chain, or a filepath.Join argument list.
REFERENCE = re.compile(r"""conformance["'/\s,)\]]{1,12}?([a-z_]+\.json)""")


def _sources() -> list[pathlib.Path]:
    """Every source file under ROOT, minus the directories that are not source.

    SKIPPED RELATIVE TO ROOT, NOT ABSOLUTELY, and that is the whole of this function's history.
    It matched against `p.parts`, which for `/root/kontra-local/backend/src/x.ts` is fine and for
    `/root/kontra-local/.claude/worktrees/agent-…/backend/src/x.ts` is not: `.claude` is in SKIP,
    so EVERY path under a git worktree matched it and this walk returned ZERO files. Both tests in
    this module then passed or failed for a reason that had nothing to do with the tree — the
    reference check vacuously (no sources, no references) and the driven check by naming every
    corpus in the repo as undriven.

    The same shape as the three bugs the module header lists: a path assumption that is true where
    it was written and false one directory move away. Here the move is a worktree, which is where
    an agent works.
    """
    out = []
    for p in ROOT.rglob("*"):
        if any(part in SKIP for part in p.relative_to(ROOT).parts):
            continue
        if p.suffix in {".py", ".go", ".ts", ".tsx"} and p.is_file():
            out.append(p)
    return out


def test_the_walk_finds_the_tree_it_is_walking() -> None:
    """The guard on the guard. Both assertions below are silent when `_sources()` returns nothing,
    and it returned nothing under every git worktree in this repo until 2026-08-28."""
    sources = _sources()
    assert len(sources) > 200, f"only {len(sources)} source files under {ROOT} — the walk is broken"
    names = {p.name for p in sources}
    assert "identity.go" in names and "catalog.py" in names and "pollers.ts" in names


def test_every_corpus_reference_resolves() -> None:
    """A driver naming a corpus that is not there."""
    missing: list[str] = []
    for src in _sources():
        text = src.read_text(encoding="utf-8", errors="replace")
        for m in REFERENCE.finditer(text):
            name = m.group(1) or m.group(2)
            if name and not (CORPUS_DIR / name).exists() and not (CORPUS_DIR / "codec" / name).exists():
                missing.append(f"{src.relative_to(ROOT)} names conformance/{name}, which does not exist")
    assert missing == []


def test_every_corpus_is_driven() -> None:
    """A corpus nobody asserts. Counted per file, and the count is named in the failure.

    Not a claim about HOW MANY arms a corpus has — `output_dataset.json` has two of three today and
    `conformance/README.md` records that on purpose. The claim is only that it has one.
    """
    corpora = sorted(p.name for p in CORPUS_DIR.glob("*.json"))
    assert corpora, "the corpus tree is empty, so every other assertion here is vacuous"

    blob = "\n".join(
        src.read_text(encoding="utf-8", errors="replace")
        for src in _sources()
        if "conformance" in src.name or "conformance" in str(src.parent)
        or "conformance" in src.read_text(encoding="utf-8", errors="replace")
    )
    undriven = [name for name in corpora if name not in blob]
    assert undriven == [], f"no test reads: {undriven}"
