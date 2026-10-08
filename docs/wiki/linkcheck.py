#!/usr/bin/env python3
"""Every relative link in the wiki, the README and the threat model resolves.

    python3 docs/wiki/linkcheck.py

INVARIANT: a link into the tree names a path that exists at HEAD. These pages
reference source files by path constantly — that is what makes them checkable
rather than decorative — and a path that has moved reads as a working link.

IT CHECKS NON-`.md` TARGETS, WHICH IS THE POINT. An `.md`-only check passes on
`[/handler](../../handler)` (a directory that moved to `runtime/handler`) and on
`[queues.json](../../conformance/queues.json)` (a corpus that lives under
`shared/`). Both were dead for exactly that reason. So a directory and a data
file are resolved the same way a page is.

Three target kinds, three rules:
  * a filesystem path          — must exist, directory or file, any extension
  * `../../wiki/<Page>`        — GitHub's repo-relative wiki idiom, which renders
                                 as <repo>/wiki/<Page> from the root README.
                                 Resolved against `docs/wiki/<Page>.md` instead
                                 of the filesystem, where it points nowhere.
  * `[[Page]]`                 — the wiki's own link form; must be a page here

Anchors are checked too, in-page and cross-file, against GitHub's heading slugs.
Links inside fenced code blocks are examples rather than claims, and are skipped.

This file is not published: `.github/workflows/wiki.yml` mirrors `docs/wiki/*.md`
and nothing else, so a `.py` beside the pages reaches no reader of the wiki.
"""

import re
import sys
from pathlib import Path

WIKI = Path(__file__).resolve().parent
ROOT = WIKI.parent.parent
FILES = sorted(WIKI.glob("*.md")) + [ROOT / "README.md", ROOT / "docs/THREAT_MODEL.md"]

LINK = re.compile(r"\[(?:[^\]]|\\\])*\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)")
WIKILINK = re.compile(r"\[\[([^\]|]+)(?:\|[^\]]*)?\]\]")
HEADING = re.compile(r"^#{1,6}\s+(.*?)\s*$", re.M)
FENCE = re.compile(r"^(```|~~~).*?^\1", re.M | re.S)

PAGES = {p.stem for p in WIKI.glob("*.md")}
WIKI_PREFIX = "../../wiki/"


def slug(heading: str) -> str:
    """GitHub's anchor slug: punctuation dropped, spaces to hyphens, lowercased."""
    h = re.sub(r"`|\*|\[\[|\]\]|\[|\]\([^)]*\)", "", heading).strip().lower()
    return re.sub(r"\s+", "-", re.sub(r"[^\w\s-]", "", h))


def headings(path: Path) -> set[str]:
    return {slug(h) for h in HEADING.findall(path.read_text())}


def check(path: Path) -> list[tuple[int, str, str]]:
    # Blanked rather than deleted, so reported line numbers still match the file.
    text = FENCE.sub(lambda m: "\n" * m.group(0).count("\n"), path.read_text())
    anchors = headings(path)
    out: list[tuple[int, str, str]] = []

    def at(idx: int) -> int:
        return text.count("\n", 0, idx) + 1

    for m in LINK.finditer(text):
        target = m.group(1)
        if target.startswith(("http://", "https://", "mailto:")):
            continue
        if target.startswith("#"):
            if slug(target[1:]) not in anchors:
                out.append((at(m.start()), target, "no such anchor in this page"))
            continue
        rel, _, frag = target.partition("#")
        if not rel or rel == WIKI_PREFIX.rstrip("/"):
            continue
        if rel.startswith(WIKI_PREFIX):
            page = rel[len(WIKI_PREFIX) :]
            if page not in PAGES:
                out.append((at(m.start()), target, "no such wiki page"))
            continue
        resolved = (path.parent / rel).resolve()
        if not resolved.exists():
            out.append((at(m.start()), target, "target does not exist"))
        elif frag and resolved.suffix == ".md" and slug(frag) not in headings(resolved):
            out.append((at(m.start()), target, "no such anchor in target"))

    for m in WIKILINK.finditer(text):
        page = m.group(1).strip()
        if page not in PAGES:
            out.append((at(m.start()), f"[[{page}]]", "no such wiki page"))
    return out


def main() -> int:
    broken = 0
    for f in FILES:
        for line, target, why in check(f):
            print(f"{f.relative_to(ROOT)}:{line}: {target} — {why}")
            broken += 1
    print(f"{len(FILES)} files, {broken} broken link(s)", file=sys.stderr)
    return 1 if broken else 0


if __name__ == "__main__":
    sys.exit(main())
