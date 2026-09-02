"""The PYTHON ARM of the Run-id slug contract (shared/conformance/slug.json).

WHAT THIS REPLACES, AND WHY THE THING IT REPLACES WAS NOT A TEST. `actorkit.catalog._slug`'s
docstring said "`tests/test_temp_dataset.py` pins the pair". There is no such file, and there never
was: this side of the contract had NOTHING asserting it, while the Go peer
(`sdk/go/catalog/dataset_test.go`) asserted `runs_2026-08-19T14_49_20_00_00_sweep` with the comment
"want the same mapping Python's `_slug` makes" — a value hand-copied out of an implementation that
nobody ran. One phantom citation and one hand-copied golden, guarding a derivation with three
writers. ADR 0035 rule two: a contract with two writers gets a corpus.

THE THIRD WRITER IS THE ORCHESTRATOR. `control/orchestrator/src/data/parquet.ts:safeName` sanitises the name
again on its way to a DuckLake table and an object key, and both SDK comments claimed it applies
"the same rule". It does not — see the corpus's `measured` field for the three places it differs
and for why none of them is live. What this arm pins is the half that is genuinely this file's: the
mapping and the bound, on the inputs where an implementation can plausibly diverge.
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest

from actorkit.catalog import _SLUG_MAX, _slug

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "shared" / "conformance" / "slug.json"

DOC = json.loads(FIXTURE.read_text(encoding="utf-8"))
CASES = DOC["cases"]


def test_the_corpus_is_not_empty_and_still_carries_the_inputs_that_diverge() -> None:
    """A corpus that silently shrank to nothing would pass every case below.

    The named inputs ARE the point of the file. A corpus of `nscheck-1787150959` and nothing else
    is satisfied by every wrong implementation of this function, which is finding 1 of ADR 0035
    with a new file name.
    """
    assert len(CASES) >= 12
    ins = [c["in"] for c in CASES]
    # Over the DECODED inputs, never the file's text: `́` and a literal combining acute are the
    # same input and only one of them survives an editor.
    blob = "\n".join(ins)
    for ch in ("/", ":", "'", ";", "é", "\U0001f4e6", "́", "."):
        assert ch in blob, f"the corpus no longer exercises {ch!r}"
    assert "" in ins, "the empty id is the fallback's only trigger"
    assert any(len(s) > _SLUG_MAX for s in ins), "nothing in the corpus reaches the bound"
    assert any(c["slug"] != c["safe_name"] for c in CASES), (
        "the corpus no longer records a row where the orchestrator rewrites the SDKs' answer, "
        "which is the divergence it exists to keep visible"
    )


@pytest.mark.parametrize("case", CASES, ids=[c["why"][:48] for c in CASES])
def test_the_slug_matches_the_corpus(case: dict) -> None:
    assert _slug(case["in"]) == case["slug"]


def test_the_bound_and_the_fallback_are_the_corpus_declares_them() -> None:
    """The two SDK-only rules, asserted against the file rather than against a literal here.

    They are declared at the top of the corpus because the Go arm has to agree with them and has no
    way to import this constant. A change to `_SLUG_MAX` that the fixture does not follow is the
    exact drift this whole file exists to catch.
    """
    assert _SLUG_MAX == DOC["bound"]
    assert _slug("") == DOC["empty"]
