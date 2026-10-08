"""Python half of the cross-SDK Checkpoint contract.

The Go peer (`runtime/go/checkpoint`) and the orchestrator (`control/orchestrator/src/checkpoint.ts`)
assert the SAME fixture, `shared/conformance/checkpoint.json`. A divergence means a checkpoint one
side writes is a checkpoint the other side mis-reads — and because the safe failure is "start over",
a drift shows up as work silently re-run or silently skipped rather than as an error.
"""
import importlib.util as u
import json
import pathlib

# parents[3] is the repo root: internals/ -> python/ -> runtime/ -> <root>. The same depth
# test_blobkey_conformance.py counts, and for the same reason: a wrong depth resolves outside the
# checkout and the fixture silently disappears.
ROOT = pathlib.Path(__file__).resolve().parents[3]
FIXTURE = ROOT / "shared" / "conformance" / "checkpoint.json"


def _mod():
    spec = u.spec_from_file_location("ck", pathlib.Path(__file__).with_name("checkpoint.py"))
    m = u.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


def _fx():
    fx = json.loads(FIXTURE.read_text())
    # NON-VACUOUS. Every test below loops over one of these lists, so an empty fixture would pass
    # all of them — this repository's most repeated failure shape.
    assert fx["canonical"] and fx["members"] and fx["resume"], "fixture is empty"
    return fx


def test_range_set_canonical_encoding():
    m, fx = _mod(), _fx()
    for c in fx["canonical"]:
        rs = m.RangeSet()
        for i in c["add"]:
            rs.add(i)
        assert rs.ranges() == c["expect"], f'{c["why"]}:\n  got  {rs.ranges()}\n  want {c["expect"]}'
        assert len(rs) == c["count"], f'{c["why"]}: count got {len(rs)} want {c["count"]}'


def test_range_set_round_trips_through_its_own_encoding():
    # A set built FROM ranges must encode back to the same ranges, or a checkpoint changes shape
    # every time it passes through a heartbeat.
    m, fx = _mod(), _fx()
    for c in fx["canonical"]:
        once = m.RangeSet(c["expect"]).ranges()
        twice = m.RangeSet(once).ranges()
        assert once == c["expect"], c["why"]
        assert twice == once, f'{c["why"]}: not stable under re-encoding'


def test_range_set_membership():
    m, fx = _mod(), _fx()
    for c in fx["members"]:
        rs = m.RangeSet(c["ranges"])
        for i in c["in"]:
            assert i in rs, f'{c["why"]}: {i} should be a member of {c["ranges"]}'
        for i in c["out"]:
            assert i not in rs, f'{c["why"]}: {i} should NOT be a member of {c["ranges"]}'


def test_resume_from():
    m, fx = _mod(), _fx()
    for c in fx["resume"]:
        got = m.resume_from(c["checkpoint"], c["batch_id"], c["units"])
        assert got == c["todo"], f'{c["why"]}:\n  got  {got}\n  want {c["todo"]}'


def test_a_discarded_checkpoint_yields_every_unit():
    """The `discarded` cases are the ones where resuming would LOSE work, so they are asserted
    twice: once for the todo list, and once for the property that makes them safe."""
    m, fx = _mod(), _fx()
    discarded = [c for c in fx["resume"] if c.get("discarded")]
    assert len(discarded) >= 4, "the fixture should carry every discard reason"
    for c in discarded:
        got = m.resume_from(c["checkpoint"], c["batch_id"], c["units"])
        assert got == list(range(c["units"])), f'{c["why"]}: a discard must start from the beginning'


def test_no_details_means_start_from_the_beginning():
    # A first attempt has no heartbeat details at all, and `None` must not be mistaken for an empty
    # checkpoint that happens to match.
    m = _mod()
    assert m.resume_from(None, "b1", 3) == [0, 1, 2]
    assert m.resume_from({}, "b1", 3) == [0, 1, 2]
    assert m.resume_from("not a dict", "b1", 3) == [0, 1, 2]


def test_details_round_trip():
    m = _mod()
    ck = m.Checkpoint(batch_id="b1", manifest_ref="s3://m")
    for i in (0, 1, 2, 5):
        ck.commit(i)
    ck.isolate(3)
    back = m.Checkpoint.from_details(ck.to_details())
    assert back is not None
    assert back.batch_id == "b1"
    assert back.manifest_ref == "s3://m"
    assert back.done.ranges() == [[0, 2], [5, 5]]
    assert back.failed == {3}


def test_the_version_is_written_not_assumed():
    m = _mod()
    assert m.Checkpoint(batch_id="b").to_details()["v"] == m.VERSION


def test_wire_bytes():
    """THE BYTES, not just the values.

    An encoder that renders `done` as `[{"lo":0,"hi":1}]` agrees with its peers on every membership
    question in this file and is unreadable to them. This is the assertion that catches that, and it
    is in the corpus rather than hand-copied between languages because the repo has already been
    burned by a golden that two implementations could not both be wrong against
    (`shared/conformance/README.md`).

    `separators` matches Go's compact encoder. Python's default puts a space after `:` and `,`.
    """
    m, fx = _mod(), _fx()
    assert fx["wire"], "fixture has no wire cases"
    for c in fx["wire"]:
        ck = m.Checkpoint(batch_id=c["batch_id"], manifest_ref=c["manifest_ref"])
        for i in c["commit"]:
            ck.commit(i)
        for i in c["isolate"]:
            ck.isolate(i)
        got = json.dumps(ck.to_details(), separators=(",", ":"))
        assert got == c["bytes"], f'{c["why"]}:\n  got  {got}\n  want {c["bytes"]}'


def test_wire_bytes_decode_back():
    """And the other direction: every byte string in the corpus must decode to the same facts."""
    m, fx = _mod(), _fx()
    for c in fx["wire"]:
        ck = m.Checkpoint.from_details(json.loads(c["bytes"]))
        assert ck is not None, c["why"]
        assert ck.batch_id == c["batch_id"]
        assert ck.manifest_ref == c["manifest_ref"]
        for i in c["commit"]:
            assert i in ck.done, f'{c["why"]}: {i} should be committed'
        assert ck.failed == set(c["isolate"]), c["why"]
