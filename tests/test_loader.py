from pathlib import Path

import pytest

from internals.loader import load_actor

# A small, self-contained fixture actor (not an example) so these tests don't depend on the
# examples/ tree, which is rebuilt independently.
FIXTURE_ECHO = Path(__file__).resolve().parent / "fixtures" / "echo"


def test_directory_actor_loads_manifest():
    loaded = load_actor(str(FIXTURE_ECHO))
    assert loaded.registry.actor_name == "echo"
    assert loaded.manifest is not None
    assert loaded.manifest.name == "echo"
    assert loaded.actor_dir == FIXTURE_ECHO


def test_file_actor_resolves_manifest_from_parent():
    loaded = load_actor(str(FIXTURE_ECHO / "actor.py"))
    assert loaded.manifest is not None
    assert loaded.actor_dir.name == "echo"


def test_manifest_name_mismatch_fails(tmp_path):
    actor_dir = tmp_path / "demo"
    actor_dir.mkdir()
    (actor_dir / "actor.py").write_text(
        "from actorkit import actor\n"
        "@actor.load\n"
        "async def load(self): ...\n"
        "@actor.method\n"
        "async def method(self, batch): return None\n"
    )
    (actor_dir / "actor.json").write_text(
        '{"schemaVersion":"kontra.actor.v1","name":"wrong","version":"0.1.0"}'
    )
    with pytest.raises(SystemExit, match="does not match"):
        load_actor(str(actor_dir))


def test_a_dev_checkout_imports_the_same_file_a_wheel_ships():
    """THE REGRESSION THIS REPLACES A SHIM WITH. There used to be TWO `actorkit/__init__.py` — the
    real one under a directory called `lib/`, and a hand-written copy at the repo root whose
    `__path__` redirected to it, because the seam directory `actorkit/` shadowed the import name in
    a checkout. Two files, one exports list, kept in sync by a test that compared their `__all__`.

    The seam directory is now `sdk/python/` and the package inside it is named after itself, so the
    shadowing is gone and so is the copy. What is left to guard is that it stays gone: a dev
    checkout must import THE file a wheel contains, at `sdk/python/actorkit/__init__.py`, and there
    must be no second module anywhere claiming the name."""
    import actorkit

    root = Path(__file__).resolve().parent.parent
    pkg = root / "sdk" / "python" / "actorkit"
    assert [Path(p).resolve() for p in actorkit.__path__] == [pkg.resolve()]
    assert Path(actorkit.__file__).resolve() == (pkg / "__init__.py").resolve()
    assert not (root / "actorkit").exists(), "the seam directory came back and will shadow the name"
