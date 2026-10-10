"""`scripts/vulngate.py` decides whether a reachable vulnerability fails the build.

It is the only thing standing between a real advisory and a green tick, so its two REFUSALS matter as
much as its pass: an allowlist entry that matches nothing, and one past its `review_by`. Both are
tested here, because a suppression file whose staleness checks do not fire is just a suppression file.
"""

import datetime
import importlib.util
import json
import subprocess
from pathlib import Path

import pytest
import yaml

_ROOT = Path(__file__).resolve().parent.parent


def _load():
    spec = importlib.util.spec_from_file_location("vulngate", _ROOT / "scripts" / "vulngate.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


vulngate = _load()

TODAY = datetime.date(2026, 10, 8)


def _finding(osv, *, function=None, package=None, fixed=""):
    frame = {"module": "github.com/docker/docker", "version": "v27.5.1+incompatible"}
    if package:
        frame["package"] = package
    if function:
        frame["function"] = function
    return {"finding": {"osv": osv, "fixed_version": fixed, "trace": [frame]}}


def _stream(*objs):
    # govulncheck emits CONCATENATED objects, not an array — which is why `messages` exists at all.
    return "".join(json.dumps(o) for o in objs)


# ── the parser ──────────────────────────────────────────────────────────────────────────────────


def test_messages_decodes_concatenated_objects_not_an_array():
    raw = _stream({"config": {"scan_level": "symbol"}}, {"progress": {"message": "x"}})
    assert [list(m)[0] for m in vulngate.messages(raw)] == ["config", "progress"]


def test_messages_tolerates_whitespace_and_newlines_between_objects():
    raw = '{"a": 1}\n\n  {"b": 2}\n'
    assert len(list(vulngate.messages(raw))) == 2


def test_messages_on_empty_input_yields_nothing():
    assert list(vulngate.messages("   \n ")) == []


# ── reachability ────────────────────────────────────────────────────────────────────────────────


def test_module_level_findings_are_not_reachable():
    """The whole point of govulncheck: a dependency that is merely REQUIRED is not a finding."""
    raw = _stream(_finding("GO-2026-9999"))
    assert vulngate.called(vulngate.messages(raw)) == {}


def test_symbol_level_findings_are_reachable_and_carry_the_symbol():
    raw = _stream(_finding("GO-2026-4883", package="docker/client", function="init"))
    assert vulngate.called(vulngate.messages(raw)) == {"GO-2026-4883": ["docker/client.init"]}


def test_the_same_advisory_reported_at_two_levels_counts_once():
    raw = _stream(
        _finding("GO-2026-4883"),
        _finding("GO-2026-4883", package="docker/client", function="init"),
    )
    assert list(vulngate.called(vulngate.messages(raw))) == ["GO-2026-4883"]


def test_duplicate_symbols_are_deduped_in_order():
    raw = _stream(
        _finding("GO-2026-4883", package="p", function="A"),
        _finding("GO-2026-4883", package="p", function="B"),
        _finding("GO-2026-4883", package="p", function="A"),
    )
    assert vulngate.called(vulngate.messages(raw)) == {"GO-2026-4883": ["p.A", "p.B"]}


def test_fixed_versions_records_the_first_report_per_advisory():
    raw = _stream(_finding("GO-2026-6629", fixed="v0.41.0"), _finding("GO-2026-4883", fixed=""))
    assert vulngate.fixed_versions(vulngate.messages(raw)) == {
        "GO-2026-6629": "v0.41.0",
        "GO-2026-4883": "",
    }


# ── the gate ────────────────────────────────────────────────────────────────────────────────────


def _allow(**over):
    entry = {
        "id": "GO-2026-4883",
        "modules": ["cli"],
        "package": "github.com/docker/docker",
        "why": "daemon code, reached through package init",
        "fix_requires": "moby/moby/v2",
        "review_by": "2026-12-15",
    }
    entry.update(over)
    return {"allow": [entry]}


def test_a_reachable_advisory_with_no_entry_fails():
    ok, lines = vulngate.evaluate({"GO-2026-7777": ["p.F"]}, _allow(), "cli", TODAY)
    assert not ok
    assert any("GO-2026-7777" in ln and "not allowlisted" in ln for ln in lines)
    assert any("p.F" in ln for ln in lines), "the failure must name what is called"


def test_an_allowlisted_reachable_advisory_passes():
    ok, _ = vulngate.evaluate({"GO-2026-4883": ["p.F"]}, _allow(), "cli", TODAY)
    assert ok


def test_an_entry_that_matches_nothing_fails_as_stale():
    """A suppression for a vulnerability that is gone is what silences the next real one."""
    ok, lines = vulngate.evaluate({}, _allow(), "cli", TODAY)
    assert not ok
    assert any("no longer reports it" in ln for ln in lines)


def test_an_expired_entry_fails_even_though_it_still_matches():
    ok, lines = vulngate.evaluate(
        {"GO-2026-4883": ["p.F"]}, _allow(review_by="2026-10-07"), "cli", TODAY
    )
    assert not ok
    assert any("expired on 2026-10-07" in ln for ln in lines)


def test_an_entry_expiring_today_is_still_valid():
    """`review_by` is the last good day, not the first bad one — an off-by-one here breaks a build
    at midnight for no reason anybody could find."""
    ok, _ = vulngate.evaluate(
        {"GO-2026-4883": ["p.F"]}, _allow(review_by="2026-10-08"), "cli", TODAY
    )
    assert ok


def test_an_entry_without_a_review_by_fails():
    allow = _allow()
    del allow["allow"][0]["review_by"]
    ok, lines = vulngate.evaluate({"GO-2026-4883": ["p.F"]}, allow, "cli", TODAY)
    assert not ok
    assert any("no `review_by`" in ln for ln in lines)


def test_a_malformed_review_by_fails_rather_than_crashing():
    ok, lines = vulngate.evaluate(
        {"GO-2026-4883": ["p.F"]}, _allow(review_by="December 2026"), "cli", TODAY
    )
    assert not ok
    assert any("not an ISO date" in ln for ln in lines)


def test_an_entry_does_not_cover_a_module_it_does_not_list():
    """Reachability is per module. `cli` importing the docker client says nothing about `sdk/go`, and
    an allowlist that leaked across modules would hide a genuinely new exposure."""
    ok, lines = vulngate.evaluate({"GO-2026-4883": ["p.F"]}, _allow(), "sdk/go", TODAY)
    assert not ok
    assert any("not allowlisted" in ln for ln in lines)


def test_nothing_reachable_and_nothing_allowlisted_passes():
    ok, lines = vulngate.evaluate({}, {"allow": []}, "runtime/go", TODAY)
    assert ok
    assert not [ln for ln in lines if "::error::" in ln]


# ── the file that actually ships ────────────────────────────────────────────────────────────────


def test_the_shipped_allowlist_is_well_formed():
    allow = json.loads((_ROOT / ".github" / "vuln-allow.json").read_text(encoding="utf-8"))
    assert allow.get("allow"), "the allowlist has no `allow` array"
    for entry in allow["allow"]:
        for key in ("id", "modules", "package", "why", "fix_requires", "review_by"):
            assert entry.get(key), f"{entry.get('id', '?')} is missing `{key}`"
        datetime.date.fromisoformat(entry["review_by"])
        assert entry["id"].startswith("GO-"), entry["id"]
        assert entry["modules"], f"{entry['id']} lists no modules"


def _scanned_modules() -> set[str]:
    """The `--module` values security.yml's govulncheck matrix actually runs the gate with.

    READ FROM THE WORKFLOW, NOT RESTATED HERE. This test used to carry its own copy of the four
    module names, so adding a fifth to the matrix and forgetting the test (or the reverse) could not
    fail anything. The matrix is `module:` plus any `include:` entries, which is how the one module
    that needs `GOWORK=off` is listed.
    """
    spec = yaml.safe_load((_ROOT / ".github" / "workflows" / "security.yml").read_text(encoding="utf-8"))
    matrix = spec["jobs"]["govulncheck"]["strategy"]["matrix"]
    return set(matrix.get("module") or []) | {e["module"] for e in matrix.get("include") or []}


def test_every_allowlisted_module_is_one_security_yml_actually_scans():
    """An entry for a module no job scans is dead text: `vulngate.py` is run per matrix entry, so it
    would never be read and never be reported stale."""
    scanned = _scanned_modules()
    allow = json.loads((_ROOT / ".github" / "vuln-allow.json").read_text(encoding="utf-8"))
    for entry in allow["allow"]:
        unknown = set(entry["modules"]) - scanned
        assert not unknown, f"{entry['id']} allowlists unscanned module(s) {sorted(unknown)}"


def test_every_go_module_in_the_tree_is_scanned_and_every_scanned_one_exists():
    """THE GATE COVERS WHAT SHIPS, IN BOTH DIRECTIONS.

    A go.mod no matrix entry names is a dependency set nothing checks for reachable advisories:
    `workspaces/demo/actors/desync` was exactly that, a standalone module outside `go.work` with its
    own HTTP and AWS dependencies, until it was added. And a matrix entry whose go.mod is gone fails
    in `setup-go` with a message about a missing file rather than about the module that was deleted.
    The tracked files are the source of truth, so an untracked scratch module never counts.
    """
    tracked = subprocess.run(
        ["git", "ls-files", "--", "go.mod", "*/go.mod"],
        cwd=_ROOT, check=True, capture_output=True, text=True,
    ).stdout.split()
    if not tracked:
        pytest.skip("not a git checkout, so there is no list of tracked go.mod files to compare")
    modules = {str(Path(p).parent) for p in tracked}
    scanned = _scanned_modules()
    assert modules - scanned == set(), f"go modules no govulncheck job scans: {sorted(modules - scanned)}"
    assert scanned - modules == set(), f"govulncheck matrix names modules with no go.mod: {sorted(scanned - modules)}"
