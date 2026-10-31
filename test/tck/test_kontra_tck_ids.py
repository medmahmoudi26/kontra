"""Self-test of kontra_tck_ids.py (M4 WP-02's review, D291), with pytester and a stand-in for the TCK's
tck/requirements/base.py; run by scripts/tck.sh with the other self-tests of test/tck."""

from __future__ import annotations

import json
import textwrap
from pathlib import Path

import pytest

BASE = '''
_SESSION = "s0000000"


def tck_id(name: str) -> str:
    return f"tck-{name}-{_SESSION}"
'''

# Each test records the id a prerequisite would get, twice; the catalogue's id is computed at import, as the TCK's
# requirement specs are.
TESTS = '''
import json, os
from tck.requirements.base import tck_id

CATALOGUE = tck_id("complete-task")


def record(name):
    with open(os.environ["IDS_OUT"], "a") as fh:
        fh.write(json.dumps([name, tck_id("input-required"), tck_id("input-required"), CATALOGUE]) + "\\n")


def test_one():
    record("one")


def test_two():
    record("two")
'''


@pytest.fixture
def tck(pytester: pytest.Pytester, monkeypatch: pytest.MonkeyPatch):
    """The stand-in TCK; returns run(*args) -> (RunResult, the ids each test recorded)."""
    for d in ("tck", "tck/requirements"):
        (pytester.path / d).mkdir(parents=True, exist_ok=True)
        (pytester.path / d / "__init__.py").write_text("")
    (pytester.path / "tck/requirements/base.py").write_text(BASE)
    pytester.makepyfile(test_ids=TESTS)
    out = pytester.path / "ids.jsonl"
    monkeypatch.setenv("IDS_OUT", str(out))
    monkeypatch.syspath_prepend(pytester.path)

    def run(*args: str) -> tuple[pytest.RunResult, dict[str, list[str]]]:
        out.unlink(missing_ok=True)
        r = pytester.runpytest_subprocess("-p", "no:cacheprovider", *args)
        ids = {}
        if out.exists():
            for line in out.read_text().splitlines():
                name, *got = json.loads(line)
                ids[name] = got
        return r, ids

    return run


def test_each_test_gets_fresh_ids(tck, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("PYTHONPATH", str(Path(__file__).resolve().parent))
    r, ids = tck("-p", "kontra_tck_ids")
    r.assert_outcomes(passed=2)
    one, two = ids["one"], ids["two"]
    assert one[0] == one[1] and two[0] == two[1], "an id changed within one test"
    assert one[0] != two[0], "two tests' prerequisites share a messageId: the second is a resend"
    assert "s0000000" not in one[0] + two[0]
    assert one[2] == two[2] == "tck-complete-task-s0000000", "the catalogue's ids computed at import changed"


def test_without_the_plugin_the_ids_are_the_sessions(tck) -> None:
    # The control: the TCK as pinned gives every test of the session the same prerequisite id.
    r, ids = tck()
    r.assert_outcomes(passed=2)
    assert ids["one"][0] == ids["two"][0] == "tck-input-required-s0000000"


def test_a_tck_without_its_session_fails_every_test(
    tck, pytester: pytest.Pytester, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("PYTHONPATH", str(Path(__file__).resolve().parent))
    (pytester.path / "tck/requirements/base.py").write_text(textwrap.dedent('''
        def tck_id(name):
            return f"tck-{name}-fixed"
    '''))
    r, _ = tck("-p", "kontra_tck_ids")
    r.assert_outcomes(errors=2)
    r.stdout.fnmatch_lines(["*kontra_tck_ids: tck.requirements.base has no _SESSION or tck_id*"])
