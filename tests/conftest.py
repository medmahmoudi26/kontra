import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
# THREE ROOTS, ONE PER PACKAGE, and no repo-root entry: `import actorkit` resolves from
# `sdk/python/`, where the package is a REAL directory named after itself. It used to resolve from
# the repo root, off a hand-written `actorkit/__init__.py` shim that pointed `__path__` at the
# package files under a directory called `lib/` — the seam dir shadowed the import name, so the
# shim was the only way the Temporal workflow sandbox (which re-imports fresh and path-scans) and
# pytest agreed. The seam dir is gone, so the shadowing is gone, and with it the shim and the
# second copy of __init__ it kept in sync.
#
# `import internals` from `runtime/python/`; `from kontra.v1 import …` from `sdk/python/_gen`.
# Inserted at 0 in order, so the LAST entry ends up first.
for _p in (ROOT / "sdk" / "python" / "_gen", ROOT / "runtime" / "python", ROOT / "sdk" / "python"):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))
