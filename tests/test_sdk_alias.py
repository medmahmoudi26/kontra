"""`actorkit` still works, and it is the SAME package as `kontra` — ADR 0044.

WHAT THIS IS PROTECTING. Every Actor anyone has written imports `actorkit`, including ones outside
these repositories that we cannot edit and do not know about; ADR 0042 deliberately froze the import
name through the repo restructure for exactly that reason. So the alias is not politeness, it is the
thing that stops the rename being a silent break in somebody else's code at `load()`, on a Machine,
mid-run.

AND THE HARD PART IS IDENTITY, NOT REACHABILITY. `actorkit` and `kontra` must not be two registries.
An Actor declared through one has to be the one the runtime finds through the other, and a `Slot`
declared through one has to be the instance the other's `secrets` resolves. Two module objects with
the same source would pass every "can I import it" check and fail every one of those.
"""
from __future__ import annotations

import subprocess
import sys
import warnings

import pytest

import kontra

# Every submodule the alias claims to forward. Kept as a literal rather than read from the alias, so
# that deleting one from `_SUBMODULES` fails here instead of silently narrowing what is forwarded.
FORWARDED = (
    "actor", "batch", "catalog", "contract", "fleet", "hitl", "say",
    "probe", "retry", "schema", "secrets", "testing", "version",
)


def test_the_alias_is_the_same_package_not_a_copy() -> None:
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)
        import actorkit

        for name in FORWARDED:
            through_alias = getattr(actorkit, name)
            through_kontra = getattr(kontra, name, None)
            if through_kontra is None:  # lazily imported by kontra/__init__ only on use
                through_kontra = __import__(f"kontra.{name}", fromlist=[name])
            assert through_alias is through_kontra, (
                f"actorkit.{name} is a DIFFERENT object from kontra.{name} — two registries, and an "
                f"Actor declared through one would be invisible to the other"
            )


def test_submodule_imports_resolve_to_the_same_module() -> None:
    # `from actorkit.catalog import X` and `import actorkit.fleet` are both spellings real actors
    # use, and neither goes through `__getattr__`.
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)
        from actorkit.catalog import shared_queue  # noqa: F401
        import actorkit.fleet as alias_fleet

    from kontra.catalog import shared_queue as kontra_shared_queue
    import kontra.fleet as kontra_fleet

    assert shared_queue is kontra_shared_queue
    assert alias_fleet is kontra_fleet
    assert sys.modules["actorkit.fleet"] is sys.modules["kontra.fleet"]


def test_the_alias_warns_once_and_names_the_replacement() -> None:
    # A DeprecationWarning is invisible by default in many runners, which is why the alias states
    # itself in its docstring too — but where it IS visible it must say what to write instead.
    out = subprocess.run(
        [sys.executable, "-W", "always::DeprecationWarning", "-c",
         "import warnings\n"
         "with warnings.catch_warnings(record=True) as w:\n"
         "    warnings.simplefilter('always')\n"
         "    from actorkit import catalog, fleet, note\n"
         "    import actorkit\n"
         "    d = [x for x in w if issubclass(x.category, DeprecationWarning)]\n"
         "    print(len(d)); print(d[0].message if d else '')\n"],
        capture_output=True, text=True, check=False,
    )
    assert out.returncode == 0, out.stderr[-2000:]
    count, message = out.stdout.strip().split("\n", 1)
    assert count == "1", f"the alias warned {count} times; it must warn once per process"
    assert "kontra" in message and "actorkit" in message
    assert "ADR 0044" in message


def test_the_lazy_verbs_stay_lazy_through_the_alias() -> None:
    # `import kontra` must cost no temporalio — `say` and `hitl` import it at module scope, so
    # the top-level `note`/`ask` are resolved by `__getattr__` on first use. A star-import in the
    # alias would have resolved both eagerly and undone it, which no assertion about identity
    # would have caught.
    for module in ("kontra", "actorkit"):
        out = subprocess.run(
            [sys.executable, "-c",
             f"import warnings; warnings.simplefilter('ignore')\n"
             f"import sys, {module}\n"
             f"print('temporalio' in sys.modules)\n"],
            capture_output=True, text=True, check=False,
        )
        assert out.returncode == 0, out.stderr[-2000:]
        assert out.stdout.strip() == "False", f"import {module} pulled temporalio"


def test_the_generated_stubs_are_reachable_alongside_the_sdk() -> None:
    # THE COLLISION ADR 0044 WAS FOUND BY. The proto package is `kontra.v1`; the SDK is now `kontra`.
    # A regular package (one with __init__.py) beats a namespace package and does NOT merge with it,
    # so two `kontra` roots on one sys.path means `import kontra` succeeds while `kontra.v1` vanishes
    # — nine tests, one cause. The stubs therefore generate INTO the package, and this is the
    # assertion that they still do.
    import kontra.v1.entry_pb2 as entry

    assert entry.__name__ == "kontra.v1.entry_pb2"
    assert kontra.__file__ is not None, "kontra must be a regular package, not a namespace one"


@pytest.mark.parametrize("name", ["nonexistent", "_private", "catalogue"])
def test_a_missing_attribute_says_actorkit_not_kontra(name: str) -> None:
    # The error an author reads should name the module they typed, not the one behind it.
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)
        import actorkit

        with pytest.raises(AttributeError, match="actorkit"):
            getattr(actorkit, name)
