"""Python half of the cross-SDK commit-object contract.

The Go peer (`runtime/go/unitstore/commit_conformance_test.go`) drives the SAME fixture,
`shared/conformance/commit.json`. The key layout is what a checkpoint's `manifest_ref` names and a
retry reads back, so a drift is a resume that cannot find what the previous attempt committed — and
the decode table is what decides whether a body may be folded into a batch at all.
"""
import importlib.util as u
import json
import pathlib

import pytest

# parents[3] is the repo root: internals/ -> python/ -> runtime/ -> <root>. The depth
# test_blobkey_conformance.py counts, for the same reason: a wrong depth resolves outside the
# checkout and the fixture silently disappears.
ROOT = pathlib.Path(__file__).resolve().parents[3]
FIXTURE = ROOT / "shared" / "conformance" / "commit.json"


def _mod():
    spec = u.spec_from_file_location("us_commit", pathlib.Path(__file__).with_name("unitstore.py"))
    m = u.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


def _fx():
    fx = json.loads(FIXTURE.read_text())
    # NON-VACUOUS, and the interesting rows are still there: a corpus that shrank to its easy
    # cases passes every loop below.
    assert fx["keys"] and fx["encode"] and fx["decode"] and fx["list"], "fixture is empty"
    assert any(c["expect"]["outcome"] == "refused" for c in fx["decode"]), "no refusal rows"
    assert any(c["unit"] > 99999 for c in fx["keys"]), "no wide-index row"
    assert any(c["expect"].get("outcome") == "refused" for c in fx["list"]), "no listing refusal"
    assert any(c["keys"] and c["expect"].get("units") == [] for c in fx["list"]), \
        "no row of names that are not Units"
    return fx


def test_commit_prefix_and_key_match_the_corpus():
    m, fx = _mod(), _fx()
    assert fx["version"] == m.COMMIT_VERSION
    for c in fx["keys"]:
        prefix = m.commit_prefix(c["actor"], c["run"], c["actor_id"], c["batch_id"])
        assert prefix == c["prefix"], f'{c["why"]}:\n  got  {prefix}\n  want {c["prefix"]}'
        key = m.commit_key(prefix, c["unit"])
        assert key == c["key"], f'{c["why"]}:\n  got  {key}\n  want {c["key"]}'


def test_commits_never_land_under_the_row_prefix():
    """Everything under `units/` that ends in `.json` is a ROW to the live row tail. A commit object
    there inflates a run's row count by its Unit count — stated as a property, not left implicit in
    the corpus's spelling."""
    m, fx = _mod(), _fx()
    for c in fx["keys"]:
        assert not m.commit_prefix(c["actor"], c["run"], c["actor_id"], c["batch_id"]).startswith("units/")


def test_a_listing_means_what_the_corpus_says():
    """What a fresh execution folds back is decided by this parse, so a drift between the SDKs is
    one of them re-running Units the other would resume — or folding a name it should have skipped."""
    m, fx = _mod(), _fx()
    for c in fx["list"]:
        if c["expect"].get("outcome") == "refused":
            with pytest.raises(m.CommitInvalid):
                m.commit_units(c["prefix"], c["keys"], c["n"])
            continue
        got = m.commit_units(c["prefix"], c["keys"], c["n"])
        assert got == c["expect"]["units"], f'{c["why"]}:\n  got  {got}'


def test_every_key_the_writer_produces_lists_as_its_own_unit():
    """The round trip the two tables imply, stated: whatever `commit_key` writes, `commit_units`
    reads back as that Unit — the wide index included."""
    m, fx = _mod(), _fx()
    for c in fx["keys"]:
        assert m.commit_units(c["prefix"], [c["key"]], c["unit"] + 1) == [c["unit"]], c["why"]


def test_encoded_bodies_match_the_corpus():
    m, fx = _mod(), _fx()
    for c in fx["encode"]:
        got = m.encode_commit(c["batch_id"], c["unit"], c.get("out"), c.get("error"), c.get("category"))
        # Compared as parsed JSON: the reader is the same SDK as the writer, so key ORDER is not the
        # contract — the field set and the values are.
        assert json.loads(json.dumps(got)) == c["expect"], f'{c["why"]}:\n  got  {got}'


def test_every_encoded_body_decodes_back_to_what_was_written():
    m, fx = _mod(), _fx()
    for c in fx["encode"]:
        body = json.loads(json.dumps(m.encode_commit(
            c["batch_id"], c["unit"], c.get("out"), c.get("error"), c.get("category"))))
        got = m.decode_commit(body, c["batch_id"], c["unit"])
        if c.get("error"):
            assert got == {"out": [], "error": c["error"], "category": c["category"]}, c["why"]
        else:
            assert got == {"out": c["out"]}, c["why"]


def test_decode_table():
    m, fx = _mod(), _fx()
    for c in fx["decode"]:
        want = c["expect"]
        if want["outcome"] == "refused":
            with pytest.raises(m.CommitInvalid):
                m.decode_commit(c["body"], c["batch_id"], c["unit"])
            continue
        got = m.decode_commit(c["body"], c["batch_id"], c["unit"])
        if want["outcome"] == "committed":
            assert got == {"out": want["out"]}, f'{c["why"]}: got {got}'
        else:
            assert got == {"out": [], "error": want["error"], "category": want["category"]}, \
                f'{c["why"]}: got {got}'
