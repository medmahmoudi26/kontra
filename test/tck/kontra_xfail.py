"""pytest plugin: the pinned A2A TCK's expected outcomes, enforced (spec/relay.md §16 tck_*; D18, DM4-12, DM4-24).

Loaded into the TCK's own pytest with ``-p kontra_xfail`` and ``PYTHONPATH`` naming this directory. It reads the
file ``$TCK_EXPECTED_FAILURES`` names (test/tck/expected-failures.txt), marks every expected failure of the run's
bindings ``xfail(strict=True)``, and fails the run (exit status 1) when the outcomes differ from the file:

1. an expected failure of a binding the run tests did not end xfailed: it passed (strict XPASS), failed with an
   error, or was skipped (the TCK skips a test whose setup step was refused, e.g. a push config, instead of failing
   it);
2. a test that is not listed ended xfailed: the TCK xfails SHOULD and MAY requirements itself, so those must be
   listed too, each with its reason, and the xfailed set of a binding equals the file's expected failures for it;
3. an entry names no collected test (the TCK collects every binding's parametrization in every run and skips
   the bindings it was not asked for, so every entry, of any binding and any variant's section, must name a
   collected test);
4. a test of a binding the run tests ended skipped and is not listed as an expected skip, or an expected skip did
   not end skipped: the TCK skips whenever a prerequisite request fails or answers an unexpected state, so a gate
   regression would otherwise turn a passing test into a skip and pass the run. The skipped set of a binding, apart
   from the other bindings' tests ("Transport ... not configured"), equals the file's expected skips for it.

The file, one entry per line::

    <pytest node id>  # <reason, citing a2a-tck#<n>, D<n> or DM4-<n>>
    skip <pytest node id>  # <reason, likewise>

The first is an expected failure, the second an expected skip. Blank lines and lines starting with ``#`` are
comments. A line ``[<variant> ...]`` starts a section: the entries after it apply only to the runs whose
``$TCK_VARIANT`` it names (scripts/tck.sh sets it to direct, tunnel, caddy or auth); the entries before the first
section apply to every variant. A file with sections needs ``$TCK_VARIANT``, and that variant must be one some
section names.

A test's binding, an entry's as any other's, is the suffix of its node id (``[jsonrpc]``, ``-jsonrpc]``,
``[http_json]``, ``-http_json]``, ``[grpc]``, ``-grpc]``), else the TCK directory it is in
(``tests/compatibility/jsonrpc/`` …), else the suffix of its function's name (``_jsonrpc``; ``_rest`` or
``_http_json``; ``_grpc``: the TCK's unparametrized tests in its shared directories), else the prefix of its
class's name (``TestJsonRpc``; ``TestRest`` or ``TestHttpJson``; ``TestGrpc``), else every binding. The run's
bindings are its ``--transport`` (comma-separated; ``all`` is every binding).

A malformed file is a usage error (exit status 4): an entry without a reason or whose reason cites no issue or
decision, a node id listed twice, an unknown or empty section.
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, field

import pytest

BINDINGS = ("jsonrpc", "http_json", "grpc")

# A reason must say where the failure is tracked: an a2a-tck issue or pull request, or a Kontra decision.
_REFERENCE = re.compile(r"a2a-tck#\d+|\bDM4-\d+\b|\bD\d+\b")
_SECTION = re.compile(r"^\[(.*)\]$")
_VARIANT = re.compile(r"^[a-z0-9_-]+$")
# The binding of the TCK's unparametrized tests in its shared directories (core_operations/test_error_handling.py,
# test_transport_behavior.py, test_data_model.py): their function's name ends in it, or their class's name starts
# with it.
_FUNCTION_SUFFIX = (("_jsonrpc", "jsonrpc"), ("_http_json", "http_json"), ("_rest", "http_json"), ("_grpc", "grpc"))
_CLASS_PREFIX = re.compile(r"^Test(JsonRpc|HttpJson|Rest|Grpc)(?=[A-Z0-9_]|$)")
_CLASS_BINDING = {"JsonRpc": "jsonrpc", "HttpJson": "http_json", "Rest": "http_json", "Grpc": "grpc"}

XFAIL = "xfail"
SKIP = "skip"


@dataclass(frozen=True)
class Entry:
    node: str
    reason: str
    line: int
    bindings: frozenset[str]
    kind: str = XFAIL  # XFAIL: an expected failure; SKIP: an expected skip


@dataclass
class _State:
    entries: dict[str, Entry]  # the run's variant's
    listed: dict[str, int]  # every entry of every section, by node id, with its line: rule 3 checks them all
    run_bindings: frozenset[str]
    variant: str | None
    collected: set[str] = field(default_factory=set)
    deselected: set[str] = field(default_factory=set)
    phases: dict[str, list[pytest.TestReport]] = field(default_factory=dict)
    problems: list[str] = field(default_factory=list)
    summary: str = ""


def binding_of(node: str) -> frozenset[str]:
    """The bindings a test applies to: its parameter suffix, else its TCK directory, else its function's name suffix,
    else its class's name prefix, else every binding."""
    for b in BINDINGS:
        if node.endswith(f"[{b}]") or node.endswith(f"-{b}]"):
            return frozenset({b})
    for b in BINDINGS:
        if f"tests/compatibility/{b}/" in node:
            return frozenset({b})
    parts = node.split("::")
    function = parts[-1].split("[", 1)[0]
    for suffix, b in _FUNCTION_SUFFIX:
        if function.endswith(suffix):
            return frozenset({b})
    for cls in parts[1:-1]:
        m = _CLASS_PREFIX.match(cls)
        if m:
            return frozenset({_CLASS_BINDING[m.group(1)]})
    return frozenset(BINDINGS)


def run_bindings(transport: str | None) -> frozenset[str]:
    """The bindings a run tests, from the TCK's --transport ("all" or a comma-separated list)."""
    if transport is None or transport.strip() in ("", "all"):
        return frozenset(BINDINGS)
    return frozenset(t.strip() for t in transport.split(",") if t.strip())


def load(path: str, variant: str | None) -> tuple[dict[str, Entry], dict[str, int]]:
    """The entries of the file at path that apply to variant, and the line of every node id the file lists in any
    section. Raises pytest.UsageError on a malformed file."""
    try:
        with open(path, encoding="utf-8") as fh:
            lines = fh.read().splitlines()
    except OSError as e:
        raise pytest.UsageError(f"kontra_xfail: TCK_EXPECTED_FAILURES: {e}") from e

    problems: list[str] = []
    entries: dict[str, Entry] = {}
    listed: dict[str, int] = {}
    sections: set[str] = set()
    applies = True  # before the first section, entries apply to every variant
    section_line = 0
    section_count = 0
    for n, raw in enumerate(lines, start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        m = _SECTION.match(line)
        if m:
            if section_line and section_count == 0:
                problems.append(f"line {section_line}: an empty section")
            names = [v for v in re.split(r"[ ,]+", m.group(1)) if v]
            if not names or not all(_VARIANT.match(v) for v in names):
                problems.append(f"line {n}: a section names variants, e.g. [direct tunnel caddy]: {line!r}")
            sections.update(names)
            applies = variant in names
            section_line, section_count = n, 0
            continue
        node, sep, reason = line.partition("#")
        node, reason = node.strip(), reason.strip()
        kind = XFAIL
        if node.startswith(SKIP + " "):
            kind, node = SKIP, node[len(SKIP) + 1:].strip()
        section_count += 1
        if not sep or not reason:
            problems.append(f"line {n}: {node!r} has no reason ('<node id>  # <reason>')")
            continue
        if " " in node or "::" not in node:
            problems.append(f"line {n}: {node!r} is not a pytest node id (or 'skip <node id>')")
            continue
        if not _REFERENCE.search(reason):
            problems.append(f"line {n}: the reason of {node!r} cites no a2a-tck#<n>, D<n> or DM4-<n>")
            continue
        listed.setdefault(node, n)
        if not applies:
            continue
        if node in entries:
            problems.append(f"line {n}: {node!r} is listed already (line {entries[node].line})")
            continue
        entries[node] = Entry(node, reason, n, binding_of(node), kind)
    if section_line and section_count == 0:
        problems.append(f"line {section_line}: an empty section")
    if sections:
        if not variant:
            problems.append(f"the file has sections for the variants {sorted(sections)}: set TCK_VARIANT")
        elif variant not in sections:
            problems.append(f"TCK_VARIANT={variant!r} is not a variant any section names ({sorted(sections)})")
    if problems:
        raise pytest.UsageError(f"kontra_xfail: {path}:\n  " + "\n  ".join(problems))
    return entries, listed


def outcome(reports: list[pytest.TestReport]) -> str:
    """One test's outcome from its phase reports: passed, failed, error, skipped, xfailed, xpassed or xpassed(strict)."""
    by = {r.when: r for r in reports}
    setup, call, teardown = by.get("setup"), by.get("call"), by.get("teardown")
    if setup is not None and setup.failed:
        return "error"
    if call is not None and call.failed:
        if str(call.longrepr).startswith("[XPASS(strict)]"):
            return "xpassed(strict)"
        return "failed"
    if teardown is not None and teardown.failed:
        return "error"
    for r in (setup, call):
        if r is not None and r.skipped:
            return "xfailed" if hasattr(r, "wasxfail") else "skipped"
    if call is not None and call.passed:
        return "xpassed" if hasattr(call, "wasxfail") else "passed"
    return "not run"


def skip_reason(reports: list[pytest.TestReport]) -> str:
    """Why a skipped test skipped, as pytest reports it ("Skipped: " dropped), or ""."""
    for r in reports:
        if r.skipped and isinstance(r.longrepr, tuple) and len(r.longrepr) == 3:
            return str(r.longrepr[2]).removeprefix("Skipped: ")
    return ""


def pytest_configure(config: pytest.Config) -> None:
    path = os.environ.get("TCK_EXPECTED_FAILURES")
    if not path:
        raise pytest.UsageError("kontra_xfail: set TCK_EXPECTED_FAILURES to the expected-failures file")
    variant = os.environ.get("TCK_VARIANT") or None
    try:
        transport = config.getoption("transport")
    except ValueError:  # not the TCK: no --transport option
        transport = None
    entries, listed = load(path, variant)
    config.pluginmanager.register(
        _Plugin(config, _State(entries, listed, run_bindings(transport), variant)), "kontra_xfail_state")


class _Plugin:
    """The run's state and the hooks that need it."""

    def __init__(self, config: pytest.Config, state: _State) -> None:
        self.config = config
        self.st = state

    @pytest.hookimpl(tryfirst=True)
    def pytest_collection_modifyitems(self, items: list[pytest.Item]) -> None:
        # tryfirst: before -k and -m deselect anything, so that every collected test counts for rule 3.
        st = self.st
        for item in items:
            st.collected.add(item.nodeid)
            e = st.entries.get(item.nodeid)
            if e is not None and e.kind == XFAIL and e.bindings & st.run_bindings:
                item.add_marker(pytest.mark.xfail(reason=e.reason, strict=True, run=True))

    def pytest_deselected(self, items: list[pytest.Item]) -> None:
        self.st.deselected.update(item.nodeid for item in items)

    def pytest_runtest_logreport(self, report: pytest.TestReport) -> None:
        self.st.phases.setdefault(report.nodeid, []).append(report)

    @pytest.hookimpl(tryfirst=True)
    def pytest_sessionfinish(self, session: pytest.Session) -> None:
        st = self.st
        outcomes = {node: outcome(rs) for node, rs in st.phases.items()}
        mine = {n: e for n, e in st.entries.items() if e.bindings & st.run_bindings}
        for node in sorted(set(st.listed) - st.collected):
            st.problems.append(f"{node}: listed (line {st.listed[node]}), but names no collected test")
        for node, e in sorted(mine.items()):
            if node not in st.collected or node in st.deselected:
                continue
            got = outcomes.get(node, "not run")
            if e.kind == XFAIL and got != "xfailed":
                st.problems.append(f"{node}: listed (line {e.line}), but {got}, not xfailed")
            elif e.kind == SKIP and got != "skipped":
                st.problems.append(f"{node}: listed as a skip (line {e.line}), but {got}, not skipped")
        for node, got in sorted(outcomes.items()):
            if got == "xfailed" and node not in st.entries:
                st.problems.append(f"{node}: xfailed, but not listed (list it with its reason)")
            elif got == "skipped" and node not in st.entries and binding_of(node) & st.run_bindings:
                st.problems.append(f"{node}: skipped ({skip_reason(st.phases[node]) or 'no reason'}), but not listed "
                                   "(list it as 'skip <node id>  # <reason>', or fix what skipped it)")

        failures = {n: e for n, e in mine.items() if e.kind == XFAIL}
        skips = {n: e for n, e in mine.items() if e.kind == SKIP}
        xfailed = sum(1 for n in failures if outcomes.get(n) == "xfailed")
        skipped = sum(1 for n in skips if outcomes.get(n) == "skipped")
        deselected = sum(1 for n in mine if n in st.deselected)
        st.summary = (f"kontra_xfail: variant {st.variant or '-'}, bindings {', '.join(sorted(st.run_bindings))}: "
                      f"{xfailed} of {len(failures)} expected failures xfailed, {skipped} of {len(skips)} expected "
                      "skips skipped")
        if deselected:
            st.summary += f", {deselected} deselected"
        if st.problems:
            st.summary += f"; {len(st.problems)} problem(s), so the run fails"
            if session.exitstatus in (pytest.ExitCode.OK, pytest.ExitCode.NO_TESTS_COLLECTED):
                session.exitstatus = pytest.ExitCode.TESTS_FAILED
            _junit_property(self.config, "kontra_xfail", "\n".join(st.problems))

    def pytest_terminal_summary(self, terminalreporter) -> None:  # noqa: ANN001
        st = self.st
        if not st.summary:
            return
        if st.problems:
            terminalreporter.section("kontra_xfail: outcomes that differ from the expected failures", red=True)
            for p in st.problems:
                terminalreporter.write_line(p)
        terminalreporter.write_line(st.summary, red=bool(st.problems), green=not st.problems)


def _junit_property(config: pytest.Config, name: str, value: str) -> None:
    """Puts the problems in the JUnit report too, so that a report read alone does not look green."""
    try:
        from _pytest.junitxml import xml_key
    except ImportError:  # pragma: no cover - a pytest without it
        return
    xml = config.stash.get(xml_key, None)
    if xml is not None:
        xml.add_global_property(name, value)
