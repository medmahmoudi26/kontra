"""The PYTHON ARM of what an actor says it needs to run (shared/conformance/resources.json).

`resources` (PRD §6) and its alias `needs` (ADR 0040) normalise to one `{cpus, memory}` shape in
Kubernetes' units. The Go arm is `cli/actorresources_conformance_test.go`, which drives the same rows
through the CLI's `readManifest`; this one drives them through the worker's own `load_manifest`.

THE ROW THAT TELLS THE TWO READERS APART is `"cpus": true`. In Python a bool IS an int, so a reader
written the obvious way accepts it as one CPU while Go's decoder refuses it — and a corpus without
that row would let the two agree on every input that does not matter.
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest

from internals.manifest import ManifestError, load_manifest

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "shared" / "conformance" / "resources.json"

DOC = json.loads(FIXTURE.read_text(encoding="utf-8"))
CASES = DOC["cases"]


def _actor(tmp_path: Path, keys: dict) -> Path:
    body = {"schemaVersion": "kontra.actor.v1", "name": "enrich", "version": "0.3.0", **keys}
    (tmp_path / "actor.json").write_text(json.dumps(body), encoding="utf-8")
    return tmp_path


def test_the_corpus_still_carries_the_inputs_that_diverge() -> None:
    """A corpus that shrank passes every reader; these rows are why the file exists."""
    assert len(CASES) >= 30
    manifests = json.dumps([c["manifest"] for c in CASES])
    for needle in ('"256m"', '"cpus": true', '"needs"', '"resources"'):
        assert needle in manifests, f"the corpus no longer carries {needle}"
    assert DOC["keys"] == ["cpus", "memory"]


@pytest.mark.parametrize("case", CASES, ids=[c["why"][:60] for c in CASES])
def test_load_manifest_matches_the_corpus(case: dict, tmp_path: Path) -> None:
    actor_dir = _actor(tmp_path, case["manifest"])
    if "refused" in case:
        with pytest.raises(ManifestError) as err:
            load_manifest(actor_dir)
        for word in case["refused"]:
            assert word in str(err.value), f"the refusal must name {word!r}: {err.value}"
        return
    m = load_manifest(actor_dir)
    assert m is not None
    got = m.resources.as_dict() if m.resources is not None else None
    assert got == case["resources"]


@pytest.mark.parametrize("literal", ["NaN", "Infinity", "-Infinity"])
def test_a_non_finite_cpu_count_is_refused(literal: str, tmp_path: Path) -> None:
    """Not a corpus row, because it cannot be one: these are not JSON, and the Go reader's decoder
    refuses the whole file before it reaches the field. Python's json module PARSES them, and
    `NaN <= 0` is False — so this is a hole only the Python reader has."""
    (tmp_path / "actor.json").write_text(
        '{"schemaVersion": "kontra.actor.v1", "name": "enrich", "version": "0.3.0", '
        f'"resources": {{"cpus": {literal}}}}}',
        encoding="utf-8",
    )
    with pytest.raises(ManifestError, match="cpus"):
        load_manifest(tmp_path)


@pytest.mark.parametrize(
    "actor_dir",
    ["examples/python/hello", "examples/python/firstactor", "testdata/fixtureactor"],
)
def test_the_shipped_examples_still_load(actor_dir: str) -> None:
    """The first thing an author copies, in the alias and in Docker's units."""
    m = load_manifest(ROOT / actor_dir)
    assert m is not None and m.resources is not None
    assert m.resources.as_dict() == {"cpus": 1, "memory": "256Mi"}
