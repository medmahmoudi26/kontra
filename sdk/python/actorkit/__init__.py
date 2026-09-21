"""`actorkit` is now `kontra` — ADR 0044. This module keeps every existing import working.

    from actorkit import catalog, fleet, note       # still works, warns once
    from kontra   import catalog, fleet, note       # what to write instead

WHY THE NAME CHANGED. `actorkit` described one of the two surfaces it contains — the SDK's own
header says so — and a workflow, which is the caller, is not an Actor. "Actor" also stopped
distinguishing anything when ADR 0023 §9 collapsed three deployed kinds into one. The Go SDK's root
package has been `kontra` since it existed; this is Python catching up.

WHY THIS FILE EXISTS RATHER THAN A CLEAN RENAME. Every Actor anyone has written imports `actorkit`,
including ones outside these repositories that we cannot edit and do not know about. ADR 0042
deliberately froze this import name through the repo restructure for exactly that reason. A rename
without an alias is a silent break in somebody else's code, at `load()`, on a Machine, mid-run.

THE SUBMODULES ARE THE SAME OBJECTS, NOT COPIES, and that is the whole of the difficulty. `actorkit`
and `kontra` must not be two registries: an Actor declared through `@actorkit.actor.method` has to be
the one the runtime finds when it looks at `kontra.actor`, and a `Slot` declared through one has to
be the instance the other's `secrets` resolves. So this aliases into `sys.modules` rather than
re-importing — `actorkit.catalog is kontra.catalog` is a property this file guarantees and
`tests/test_sdk_alias.py` asserts.

Removing this is a separate decision on a separate day. ADR 0044 does not schedule it.
"""

from __future__ import annotations

import sys
import warnings

import kontra as _kontra

#: The submodules an author can reach. Every one is aliased into `sys.modules` so that
#: `from actorkit.catalog import shared_queue` and `import actorkit.fleet` both resolve to the module
#: `kontra` already loaded, rather than importing a second copy under a second name.
#:
#: `hitl`, `narrate` and `secrets` are in this list even though `kontra/__init__.py` deliberately does
#: NOT import them at module scope — two of them import temporalio at import time and one does network
#: I/O, and `import kontra` is required to cost neither. Aliasing is not importing: the entry is
#: created only for a submodule that has ALREADY been loaded, so the cost stays where it was.
_SUBMODULES = (
    "actor", "batch", "catalog", "contract", "fleet", "hitl", "say",
    "probe", "retry", "schema", "secrets", "testing", "version",
)

_WARNED = False


def _warn() -> None:
    """Once per process, and never from inside a workflow's replay.

    A `DeprecationWarning` is invisible by default in many runners, which is why the module docstring
    says it too — a deprecation nobody can see is a rename with extra steps.
    """
    global _WARNED
    if _WARNED:
        return
    _WARNED = True
    warnings.warn(
        "`actorkit` is now `kontra` (ADR 0044) — write `from kontra import ...`. "
        "This alias keeps working and is not scheduled for removal.",
        DeprecationWarning,
        stacklevel=3,
    )


def __getattr__(name: str) -> object:
    """Everything `kontra` exposes, including the two lazily-resolved verbs.

    Delegating through `getattr` rather than copying `kontra`'s namespace at import is what keeps
    `note` and `ask` lazy: `kontra.__getattr__` imports `say`/`hitl` on first use so that
    `import kontra` costs no temporalio, and a star-import here would have resolved both eagerly and
    undone it.
    """
    _warn()
    try:
        value = getattr(_kontra, name)
    except AttributeError:
        # A SUBMODULE THAT NOBODY HAS IMPORTED YET IS NOT AN ATTRIBUTE OF ITS PACKAGE, and
        # `from actorkit import batch` is the spelling that finds out. `kontra/__init__.py` imports
        # `catalog` and `fleet` at module scope and deliberately leaves the rest — `narrate` and
        # `say` and `hitl` import temporalio, `secrets` does network I/O — so delegating with `getattr` alone
        # answered for two of thirteen. Import it, then delegate, so the alias forwards the whole
        # package rather than the part that happened to be loaded.
        if name in _SUBMODULES:
            import importlib

            value = importlib.import_module(f"kontra.{name}")
        else:
            raise AttributeError(f"module 'actorkit' has no attribute {name!r}") from None
    # A submodule reached as an attribute is also reachable as `actorkit.<name>`; register it so the
    # two spellings cannot diverge later in the same process.
    if name in _SUBMODULES:
        sys.modules.setdefault(f"{__name__}.{name}", value)
    return value


def __dir__() -> list[str]:
    return sorted(set(dir(_kontra)) | set(_SUBMODULES))


# Alias every ALREADY-LOADED submodule up front, so `from actorkit.catalog import X` works without
# anyone having touched `actorkit.catalog` as an attribute first. `kontra/__init__.py` imports
# `catalog` and `fleet` at module scope; the rest arrive here as they are loaded elsewhere.
for _name in _SUBMODULES:
    _loaded = sys.modules.get(f"kontra.{_name}")
    if _loaded is not None:
        sys.modules.setdefault(f"{__name__}.{_name}", _loaded)
del _name, _loaded

__all__ = list(getattr(_kontra, "__all__", ()))
