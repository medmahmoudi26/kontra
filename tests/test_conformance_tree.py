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
  * `Component.Path "control/orchestrator/node_modules"` in the bundle manifest — the reverse case, where the
    rewrite was WRONG because that path describes the extracted artifact and not the repo, and
    `control/orchestrator/node_modules` is the right answer twice elsewhere in the same file. Eight install
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
CORPUS_DIR = ROOT / "shared" / "conformance"

#: Where a driver could live. `node_modules` and build output are not source.
SKIP = {".git", "node_modules", "dist", "__pycache__", "build", "tmp", ".claude", "_gen"}

#: `conformance/<name>.json`, in any of the spellings a driver uses — a slash path, a pathlib
#: chain, or a filepath.Join argument list.
REFERENCE = re.compile(r"""conformance["'/\s,)\]]{1,12}?([a-z_]+\.json)""")

#: A LITERAL relative path to a corpus, as Go and TypeScript spell it:
#: `"../../../shared/conformance/queues.json"`. Resolvable from the file that contains it, which is
#: what makes it checkable — unlike a pathlib chain rooted at `parents[N]`.
RELATIVE_PATH = re.compile(r"""["'](\.\.(?:/\.\.)*/[\w/-]*conformance/[\w/]+\.json)["']""")

#: THE SAME PATH, ASSEMBLED FROM SEPARATE ARGUMENTS — `filepath.Join("..", "..", "conformance",
#: "blobkey.json")`. It is a different regex because it is a different failure: every fragment reads
#: correctly on its own, so a rename that moves the tree leaves each argument true and the JOIN
#: wrong, and no grep for the old path finds it.
#:
#: MEASURED, WHICH IS WHY IT EXISTS. When `conformance/` became `shared/conformance/`, the guard
#: above caught every literal spelling and `cli/install/objstore/s3_test.go` kept opening
#: `filepath.Join("..", "..", "..", "conformance", "blobkey.json")` — one directory short, invisible
#: to a regex looking for slashes, and red only when that one package's tests were run.
#:
#: Anchored on a FIRST argument of `".."`, so it matches the cwd-relative form a Go test uses and
#: skips `filepath.Join(filepath.Dir(self), "..", …)`, which is rooted at the file rather than at
#: the working directory and is checked by the driver failing to load.
JOINED_PATH = re.compile(
    r"""filepath\.Join\(\s*((?:"\.\."\s*,\s*)+(?:"[\w.-]+"\s*,\s*)*?"[\w-]*conformance"\s*,\s*"[\w]+\.json")\s*\)"""
)


def _joined_to_relative(args: str) -> str:
    """Turn a Join argument list into the path it builds.

    The example is spelled with a REAL corpus name on purpose: `test_every_corpus_reference_resolves`
    reads this file too, and an invented placeholder name makes that guard fail on this module's own
    prose — which is how a guard becomes something people delete rather than fix.

        `"..", "..", "conformance", "blobkey.json"`  ->  `../../conformance/blobkey.json`
    """
    return "/".join(re.findall(r'"([^"]+)"', args))


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


def test_every_relative_corpus_path_resolves() -> None:
    """A driver whose ASSEMBLED path does not reach the corpus tree.

    THE GAP THIS CLOSES, found by walking into it. `test_every_corpus_reference_resolves` below
    checks that the corpus a driver NAMES exists — under `CORPUS_DIR`, which this file computes
    for itself. It never checks that the driver's own path arrives there. So when the tree moved
    from `conformance/` to `shared/conformance/`, this module passed while ten Python drivers and
    seventeen Go ones opened a file that was no longer at the end of their `../../../` climb.

    That is the module header's own first bullet — a path that is ASSEMBLED, where every fragment
    reads correctly — and the guard written for it was checking the wrong half.

    Only literal relative paths are resolvable from here: a `parents[1] / "shared" / "conformance"`
    chain depends on the file's own location and is checked by the driver failing to import. What
    IS checkable is every `"../…/conformance/<name>.json"` string, which is the spelling Go uses
    throughout and the one that broke silently.
    """
    broken: list[str] = []
    for src in _sources():
        # NOT THIS FILE. Its docstring above quotes an example path, and a guard that fails on its
        # own prose is a guard people delete.
        if src.resolve() == pathlib.Path(__file__).resolve():
            continue
        text = src.read_text(encoding="utf-8", errors="replace")
        found = [m.group(1) for m in RELATIVE_PATH.finditer(text)]
        # ASSEMBLED PATHS TOO. `filepath.Join("..", "..", "conformance", "blobkey.json")` is the same path
        # with every fragment separately correct, which is exactly why a tree move leaves it wrong
        # and a grep for the old string finds nothing. See JOINED_PATH.
        found += [_joined_to_relative(m.group(1)) for m in JOINED_PATH.finditer(text)]
        for rel in found:
            if not (src.parent / rel).resolve().exists():
                broken.append(
                    f"{src.relative_to(ROOT)} opens {rel!r}, which resolves to "
                    f"{(src.parent / rel).resolve()} and is not there"
                )
    assert broken == [], "a driver's relative path no longer reaches the corpus:\n  " + "\n  ".join(broken)


def test_the_joined_path_guard_can_see_an_assembled_path() -> None:
    """The guard above is only worth having if `JOINED_PATH` actually matches.

    NON-VACUOUS BY CONSTRUCTION. A regex that matched nothing would make
    `test_every_relative_corpus_path_resolves` pass exactly as it did while
    `cli/install/objstore/s3_test.go` was opening a file that had not existed since the tree
    moved — a guard that reports success because it looked at nothing, which is the failure this
    repository has now hit in three different shapes.

    So: it must match the real spelling, reconstruct it correctly, and NOT match the file-relative
    form, which is rooted at the source file rather than the working directory.
    """
    cwd_relative = 'os.ReadFile(filepath.Join("..", "..", "..", "shared", "conformance", "blobkey.json"))'
    m = JOINED_PATH.search(cwd_relative)
    assert m is not None, "JOINED_PATH no longer matches the spelling it was written for"
    assert _joined_to_relative(m.group(1)) == "../../../shared/conformance/blobkey.json"

    # The broken spelling this guard was written after — one directory short, every fragment true.
    was_broken = 'filepath.Join("..", "..", "..", "conformance", "blobkey.json")'
    m2 = JOINED_PATH.search(was_broken)
    assert m2 is not None, "the exact historical break is no longer matched"
    assert _joined_to_relative(m2.group(1)) == "../../../conformance/blobkey.json"

    # File-relative: rooted at the source file, not the cwd, so resolving it from `src.parent`
    # would be wrong. It is checked by the driver failing to load instead.
    file_relative = 'filepath.Join(filepath.Dir(self), "..", "..", "shared", "conformance", "queues.json")'
    assert JOINED_PATH.search(file_relative) is None

    # And it is exercised against the tree, not only against these literals: at least one real
    # source file carries a joined corpus path, or this guard is protecting nothing.
    joined_in_tree = [
        src.relative_to(ROOT)
        for src in _sources()
        if src.resolve() != pathlib.Path(__file__).resolve()
        and JOINED_PATH.search(src.read_text(encoding="utf-8", errors="replace"))
    ]
    assert joined_in_tree, "no source uses filepath.Join for a corpus path — retire this guard or fix it"


def test_every_corpus_reference_resolves() -> None:
    """A driver naming a corpus that is not there.

    NAME ONLY — see `test_every_relative_corpus_path_resolves` for why that is not enough.
    """
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
    `shared/conformance/README.md` records that on purpose. The claim is only that it has one.
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
