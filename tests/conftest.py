import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
# THREE ROOTS, ONE PER PACKAGE, and no repo-root entry: `import kontra` resolves from
# `sdk/python/`, where the package is a REAL directory named after itself. It used to resolve from
# the repo root, off a hand-written `actorkit/__init__.py` shim that pointed `__path__` at the
# package files under a directory called `lib/` — the seam dir shadowed the import name, so the
# shim was the only way the Temporal workflow sandbox (which re-imports fresh and path-scans) and
# pytest agreed. The seam dir is gone, so the shadowing is gone, and with it the shim and the
# second copy of __init__ it kept in sync.
#
# `import internals` from `runtime/python/`; `import kontra` AND `from kontra.v1 import …` both from
# `sdk/python`, because ADR 0044 put the generated stubs inside the SDK package. There is no third
# entry any more, and there must not be: a second `kontra` root on sys.path is exactly the collision
# that ADR — a REGULAR package beats a namespace package and does not merge with it, so `import
# kontra` would succeed while `kontra.v1` vanished.
# Inserted at 0 in order, so the LAST entry ends up first.
for _p in (ROOT / "runtime" / "python", ROOT / "sdk" / "python"):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))
