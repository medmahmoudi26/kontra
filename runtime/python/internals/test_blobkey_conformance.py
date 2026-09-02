"""Python half of the cross-SDK blob-key contract.

The Go host asserts the SAME fixture (shared/conformance/blobkey.json). A divergence between
the two implementations means the reader sees two layouts and silently loses half the data —
and these two have already drifted once, so this is checked rather than reviewed.
"""
import datetime as dt
import importlib.util as u
import json
import pathlib

# parents[3] is the repo root: internals/ -> python/ -> runtime/ -> <root>. Count carefully: a
# wrong depth resolves to a path OUTSIDE the checkout and the fixture silently disappears. The
# DEPTH SURVIVED the sdk/runtime split (actorkit/python/internals was three deep too) — the
# FIXTURE moved, to the one shared/conformance/ tree, and only that.
ROOT = pathlib.Path(__file__).resolve().parents[3]
FIXTURE = ROOT / "shared" / "conformance" / "blobkey.json"


def _mod():
    spec = u.spec_from_file_location("us", pathlib.Path(__file__).with_name("unitstore.py"))
    m = u.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


def test_blob_key_matches_cross_sdk_fixture():
    fx = json.loads(FIXTURE.read_text())
    when = dt.datetime.strptime(fx["dt"], "%Y-%m-%d").replace(tzinfo=dt.timezone.utc)
    assert fx["cases"], "fixture is empty — a vacuously passing conformance test is worse than none"
    m = _mod()
    for c in fx["cases"]:
        got = m.blob_key_at(when, c["actor"], c["run"], c["node"], c["unit"], c["sha"])
        assert got == c["expect"], f'{c["why"]}:\n  got  {got}\n  want {c["expect"]}'
