"""Scope-classification tests for scripts/normalize-scope.py.

Every case here is a safety assertion, not a formatting preference: the wrong answer points a
crawler or an enumerator at something a program did not publish.
"""
import importlib.util
import pathlib

import pytest

_SPEC = importlib.util.spec_from_file_location(
    "normalize_scope",
    pathlib.Path(__file__).resolve().parents[1] / "scripts" / "normalize-scope.py",
)
normalize_scope = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(normalize_scope)
classify = normalize_scope.classify


@pytest.mark.parametrize(
    "raw,kind,seed",
    [
        # URLs verbatim — the path is part of what was scoped.
        ("http://api.example.com/v1/", "url", "http://api.example.com/v1/"),
        ("https://Example.com", "url", "https://example.com"),
        # "*." is the one glob meaning "every subdomain of this apex".
        ("*.example.com", "wildcard", "example.com"),
        ("*.sub.example.com", "wildcard", "sub.example.com"),
        # Bare hosts -> https seeds; a scoped PATH is preserved, never widened.
        ("example.com", "domain", "https://example.com"),
        ("example.com/only/this", "domain", "https://example.com/only/this"),
        ("1.2.3.4", "ip", "http://1.2.3.4"),
    ],
)
def test_classify_accepts(raw, kind, seed):
    assert classify(raw) == (kind, seed)


@pytest.mark.parametrize(
    "raw",
    [
        # Prefix / infix globs name SOME hosts, not all of them.
        "*uat.example.com",
        "dev*.example.com",
        "dcfgateway*.example.com",
        # Path globs scope ONE PATH. Reducing these to the apex and enumerating the whole
        # domain is the exact bug the actor version shipped with until a test caught it.
        "example.com/hz/mycd/*",
        "api.example.com/*",
        "https://*.example.com/a",
        # A CIDR needs an expansion policy we deliberately do not have. Trimming "/8" as a path
        # would turn 16 million hosts into the single IP 10.0.0.0.
        "10.0.0.0/8",
        "192.168.0.0/16",
        # Not a host at all.
        "*",
        "",
        "   ",
        "localhost",
    ],
)
def test_classify_refuses(raw):
    kind, seed = classify(raw)
    assert kind == "unexpandable", f"{raw!r} classified {kind!r} — it must be refused"
    assert seed == "", f"{raw!r} is unexpandable but produced a seed {seed!r}"


def test_unexpandable_never_carries_a_seed():
    """A seed is what downstream stages consume; an unexpandable row with one becomes a target."""
    for raw in ["*uat.x.com", "x.com/a/*", "10.0.0.0/8", "*", "", "localhost"]:
        kind, seed = classify(raw)
        if kind == "unexpandable":
            assert seed == ""


def _rows(stdin_text, monkeypatch, capsys):
    """Run main() over TSV text and return the emitted JSON objects."""
    import io, json
    monkeypatch.setattr("sys.stdin", io.StringIO(stdin_text))
    normalize_scope.main()
    return [json.loads(l) for l in capsys.readouterr().out.splitlines()]


def test_bounty_flag_is_carried_but_never_filters(monkeypatch, capsys):
    """
    The per-asset bounty flag rides along; it must not decide what is emitted.

    HackerOne marks eligibility per ASSET, so a paying program routinely publishes in-scope,
    Critical assets that pay nothing — the program does exactly that with `api.example.com` and with
    `*.example.com` itself. Scope is decided in extract-scope.sql; dropping unpaid assets here would
    silently re-impose the narrower rule one layer down, where nobody would look for it.
    """
    rows = _rows(
        "api.example.com\th1\texample-bounty\thttps://hackerone.com/example-bounty\t0\n"
        "*.example.com\th1\texample-bounty\thttps://hackerone.com/example-bounty\t0\n"
        "admin.example.com\th1\texample-bounty\thttps://hackerone.com/example-bounty\t1\n",
        monkeypatch, capsys,
    )
    assert len(rows) == 3, "an unpaid asset must still be emitted"
    by_target = {r["target"]: r for r in rows}
    assert by_target["api.example.com"]["bounty"] is False
    assert by_target["admin.example.com"]["bounty"] is True
    # …and the unpaid wildcard still classifies as expandable, so subfinder gets the apex.
    assert by_target["*.example.com"]["kind"] == "wildcard"
    assert by_target["*.example.com"]["seed"] == "example.com"


def test_missing_bounty_column_defaults_false(monkeypatch, capsys):
    """A 4-column line is the older extract format; it must still parse."""
    rows = _rows("a.example.com\th1\tprog\thttps://h1/prog\n", monkeypatch, capsys)
    assert rows[0]["bounty"] is False
