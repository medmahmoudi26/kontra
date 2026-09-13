"""`kontra actor schema` must be the catalog's derivation, not a second one.

THE FAILURE THIS GUARDS is not a crash. If this command ever grew its own way of turning `takes=`
into JSON Schema, it would keep working — and slowly disagree with what the worker publishes and
what the runtime validates. A form built from one and a dispatch validated by the other is a form
that accepts input the Method rejects, with nothing anywhere reporting a fault.

So the assertion is equality against `operations_of`, called directly.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from internals.catalog import operations_of
from internals.loader import load_actor
from internals.schemadump import contract_of, main


def fixture_actor() -> str:
    """The fixture directory, resolved ONCE from this file.

    Asserted to exist rather than assumed. A path assembled with `..` segments that lands one
    directory short returns nothing, and every assertion below would then be checking an empty
    result — which is how a guard reports success while testing nothing.
    """
    p = Path(__file__).resolve().parents[3] / "testdata" / "fixtureactor"
    assert p.is_dir(), f"fixture actor not found at {p}; this test would otherwise prove nothing"
    assert (p / "actor.py").is_file(), f"{p} has no actor.py"
    return str(p)


def test_is_the_catalog_derivation_not_a_copy():
    path = fixture_actor()
    mine = contract_of(path)["methods"]
    theirs = operations_of(load_actor(path).registry)
    assert mine == theirs, (
        "`kontra actor schema` and the catalog disagree about this actor. They must be one "
        "derivation — a second one drifts silently the first time either is fixed."
    )
    # And it actually found something, so the equality above is not two empty lists.
    assert mine, "the fixture actor declared no Methods; this test proved nothing"
    assert "input" in mine[0] and "output" in mine[0], "the fixture's declared types produced no schema"


def test_reports_identity_and_a_resolved_dir():
    out = contract_of(fixture_actor())
    assert out["actor"]["name"] == "fixtureactor"
    assert out["actor"]["version"] == "0.1.0"
    assert Path(out["dir"]).is_absolute()


def test_method_narrows():
    out = contract_of(fixture_actor(), method="echo")
    assert [m["name"] for m in out["methods"]] == ["echo"]


def test_unknown_method_names_what_exists():
    """A typo must say what IS there. "no such method" alone sends someone to the wrong file."""
    with pytest.raises(SystemExit) as e:
        contract_of(fixture_actor(), method="nope")
    msg = str(e.value)
    assert "nope" in msg
    assert "echo" in msg, f"the refusal does not list the real Methods: {msg}"


def test_undeclared_types_are_omitted_never_null(tmp_path):
    """A Method with no `takes`/`emits` is dispatchable and simply advertises nothing.

    Omitted and null are different answers: a form renderer can skip an absent key, while `null`
    is a schema it will try to read. `operations_of` omits; this pins that it stays omitted through
    this path too.
    """
    # The directory name IS the actor name — `derive_identity` refuses a manifest that
    # disagrees with it — so the actor lives in a named subdirectory, not in tmp_path itself.
    d = tmp_path / "bare"
    d.mkdir()
    (d / "actor.json").write_text(json.dumps({
        "schemaVersion": "kontra.actor.v1", "name": "bare", "version": "0.1.0",
    }))
    (d / "actor.py").write_text(
        "from kontra import actor\n"
        "\n"
        "@actor.defn\n"
        "class Bare:\n"
        "    @actor.method()\n"
        "    async def run(self, batch, dataset):\n"
        "        pass\n"
    )
    out = contract_of(str(d))
    assert [m["name"] for m in out["methods"]] == ["run"]
    assert "input" not in out["methods"][0]
    assert "output" not in out["methods"][0]


def test_import_failure_is_reported_as_one(tmp_path, capsys):
    """A broken import is a fact about the author's code, not "this actor has no schema"."""
    d = tmp_path / "broken"
    d.mkdir()
    (d / "actor.json").write_text(json.dumps({
        "schemaVersion": "kontra.actor.v1", "name": "broken", "version": "0.1.0",
    }))
    (d / "actor.py").write_text("import a_package_that_does_not_exist\n")
    rc = main([str(d)])
    assert rc == 1
    err = capsys.readouterr().err
    assert "could not load the actor" in err
    assert str(d) in err, "the error does not name the actor it failed on"
    assert "a_package_that_does_not_exist" in err, "the real ImportError was swallowed"
