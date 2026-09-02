"""Print named fields of a one-row query result, space-separated, for a shell to `read`.

A HELPER RATHER THAN AN INLINE PIPELINE, because the inline version was wrong twice. Scraping the
last integer out of `kontra dataset query`'s rendered table gets you the `1` from `1 row · 13ms`,
so a Dataset holding twelve rows measures as one — a gate assertion wrong in the lenient
direction, which is worse than no assertion at all. The fix is to read the exported JSON, and JSON
parsing does not fit in a `grep -Eo`.

    kontra dataset query D --sql "SELECT count(*) AS n ..." --export out.json
    read -r N <<<"$(python3 scripts/lib/one-row.py out.json n)"

A missing file, unreadable JSON or a missing field all print `?` for that field rather than
raising: the caller is a shell assertion that must report "measured ?" and fail, not die.
"""

import json
import sys


def main(argv: list[str]) -> int:
    if len(argv) < 3:
        print("usage: one-row.py <file.json> <field> [field...]", file=sys.stderr)
        return 2
    path, fields = argv[1], argv[2:]
    try:
        with open(path) as fh:
            doc = json.load(fh)
    except Exception:
        print(" ".join("?" for _ in fields))
        return 0
    # An export is a list of row objects; a single object is accepted too, because which one a
    # backend produces is not a thing the caller should have to know.
    row = doc[0] if isinstance(doc, list) and doc else (doc if isinstance(doc, dict) else {})
    print(" ".join(str(row.get(f, "?")) for f in fields))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
