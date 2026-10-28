"""Self-test of kontra_xfail.py (M4 WP-02; the audit's four cases, the expected skips and the file rules), with pytester.

Run by scripts/tck.sh before the TCK, in the TCK's own environment (its pinned pytest):

    PYTHONPATH=test/tck uv run --frozen --project "$KONTRA_TCK_DIR" python -m pytest -p pytester \
        -p no:cacheprovider test/tck/test_kontra_xfail.py

Each case runs a small stand-in for the TCK: a conftest with the TCK's --transport option, and tests
parametrized over both bindings, as the TCK's are, that skip the bindings not asked for ("Transport ... not
configured", a2a-tck tests/compatibility/_test_helpers.py).
"""

from __future__ import annotations

import re
import textwrap

import pytest

CONFTEST = """
def pytest_addoption(parser):
    parser.addoption("--transport", dest="transport", default="all")
"""

# A stand-in for the TCK, laid out as the TCK is. test_must_requirement's ids end "-<binding>]" (two
# parametrizations), the others' "[<binding>]"; tests/compatibility/jsonrpc/ is a binding's own directory, with an
# unparametrized test; test_errors.py is shaped like the TCK's core_operations/test_error_handling.py, whose
# unparametrized tests carry their binding in their function's name (_rest, _jsonrpc) or their class's (TestRest...,
# TestJsonRpc...).
TESTS = {
    "tests/compatibility/core_operations/test_ops.py": """
        import pytest

        BINDINGS = ["jsonrpc", "http_json"]

        def configured(request, transport):
            raw = request.config.getoption("transport")
            if raw != "all" and transport not in raw.split(","):
                pytest.skip(f"Transport {transport!r} not configured")

        @pytest.mark.parametrize("transport", BINDINGS)
        def test_refused(request, transport):
            configured(request, transport)
            assert False, "the gate refused it"

        @pytest.mark.parametrize("transport", BINDINGS)
        def test_ok(request, transport):
            configured(request, transport)

        @pytest.mark.parametrize("transport", BINDINGS)
        def test_setup_refused(request, transport):
            configured(request, transport)
            pytest.skip("CreatePushNotificationConfig failed")

        @pytest.mark.parametrize("transport", BINDINGS)
        def test_should(request, transport):
            configured(request, transport)
            pytest.xfail("SHOULD requirement not met")

        @pytest.mark.parametrize("transport", BINDINGS)
        @pytest.mark.parametrize("requirement", ["REQ-1", "REQ-2"])
        def test_must_requirement(request, transport, requirement):
            configured(request, transport)
            assert requirement != "REQ-1", "REQ-1 failed"
    """,
    "tests/compatibility/core_operations/test_errors.py": """
        import pytest

        def configured(request, transport):
            raw = request.config.getoption("transport")
            if raw != "all" and transport not in raw.split(","):
                pytest.skip(f"Transport {transport!r} not configured")

        class TestCoreInputValidation:
            def test_malformed_request_rejected_rest(self, request):
                configured(request, "http_json")
                assert False, "the TCK's REST client defect"

            def test_malformed_request_rejected_jsonrpc(self, request):
                configured(request, "jsonrpc")

        class TestRestErrorStructure:
            def test_error_uses_aip193_format(self, request):
                configured(request, "http_json")

        class TestJsonRpcErrorStructure:
            def test_error_object_structure(self, request):
                configured(request, "jsonrpc")
    """,
    "tests/compatibility/jsonrpc/test_only.py": """
        import pytest

        def test_jsonrpc_only(request):
            raw = request.config.getoption("transport")
            if raw != "all" and "jsonrpc" not in raw.split(","):
                pytest.skip("JSON-RPC transport not configured")
            assert False, "jsonrpc only"
    """,
}

OPS = "tests/compatibility/core_operations/test_ops.py"
ERRORS = "tests/compatibility/core_operations/test_errors.py"

# Entries that make a jsonrpc run and an http_json run pass: every failure, xfail and skip of both bindings, apart from
# the other binding's tests.
GOOD = f"""
# the stand-in's failures
{OPS}::test_refused[jsonrpc]  # D20: refused
{OPS}::test_refused[http_json]  # D20: refused
{OPS}::test_should[jsonrpc]  # a2a-tck#234: the TCK xfails it itself
{OPS}::test_should[http_json]  # a2a-tck#234: the TCK xfails it itself
{OPS}::test_must_requirement[REQ-1-jsonrpc]  # DM4-12: REQ-1
{OPS}::test_must_requirement[REQ-1-http_json]  # DM4-12: REQ-1
tests/compatibility/jsonrpc/test_only.py::test_jsonrpc_only  # a2a-tck#1: jsonrpc only, by its directory
{ERRORS}::TestCoreInputValidation::test_malformed_request_rejected_rest  # a2a-tck#999: http_json only, by its name
# the stand-in's skips
skip {OPS}::test_setup_refused[jsonrpc]  # D20: its push config is refused
skip {OPS}::test_setup_refused[http_json]  # D20: its push config is refused
"""


def esc(s: str) -> str:
    """s as an fnmatch pattern that matches s itself (node ids have [ and ])."""
    return re.sub(r"([\[\]])", r"[\1]", s)


@pytest.fixture
def tck(pytester: pytest.Pytester, monkeypatch: pytest.MonkeyPatch):
    """Writes the stand-in TCK and returns run(file text, *args, variant=None) -> RunResult."""
    pytester.makeconftest(CONFTEST)
    for path, src in TESTS.items():
        p = pytester.path / path
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(textwrap.dedent(src))
    monkeypatch.delenv("TCK_VARIANT", raising=False)

    def run(text: str, *args: str, variant: str | None = None) -> pytest.RunResult:
        f = pytester.path / "expected-failures.txt"
        f.write_text(textwrap.dedent(text))
        monkeypatch.setenv("TCK_EXPECTED_FAILURES", str(f))
        if variant is not None:
            monkeypatch.setenv("TCK_VARIANT", variant)
        return pytester.runpytest("-p", "kontra_xfail", "-p", "no:cacheprovider", "-o", "asyncio_default_fixture_loop_scope=function",
                                   "-rA", *args)

    return run


# The stand-in has 17 tests; in a jsonrpc run 4 pass, 4 xfail and 9 skip (8 of them the other binding's), in an
# http_json run 3 pass, 4 xfail and 10 skip (9 the other binding's: test_jsonrpc_only by its directory,
# test_malformed_request_rejected_jsonrpc by its name, TestJsonRpcErrorStructure by its class).
@pytest.mark.parametrize("binding", ["jsonrpc", "http_json"])
def test_the_listed_failures_pass_the_run(tck, binding: str) -> None:
    r = tck(GOOD, f"--transport={binding}")
    passed = 4 if binding == "jsonrpc" else 3
    r.assert_outcomes(passed=passed, skipped=17 - passed - 4, xfailed=4)
    assert r.ret == pytest.ExitCode.OK, r.stdout.str()
    r.stdout.fnmatch_lines([f"kontra_xfail: variant -, bindings {binding}: 4 of 4 expected failures xfailed, "
                            "1 of 1 expected skips skipped"])


# Case 1a: a listed test that passes fails the run (strict XPASS), and the plugin says why.
def test_a_listed_test_that_passes_fails_the_run(tck) -> None:
    r = tck(GOOD + f"{OPS}::test_ok[jsonrpc]  # D20: stale\n", "--transport=jsonrpc")
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines([
        "*XPASS(strict)*",
        esc(f"{OPS}::test_ok[jsonrpc]") + ": listed (line *), but xpassed(strict), not xfailed",
        "*1 problem(s), so the run fails",
    ])


# Case 1b: a listed test that skips fails the run (the TCK skips the push tests whose CreatePushNotificationConfig
# was refused), although pytest itself counts no failure.
def test_a_listed_test_that_skips_fails_the_run(tck) -> None:
    text = GOOD.replace(f"skip {OPS}::test_setup_refused[http_json]", f"{OPS}::test_setup_refused[http_json]")
    r = tck(text, "--transport=http_json")
    r.assert_outcomes(passed=3, skipped=10, xfailed=4)
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines([esc(f"{OPS}::test_setup_refused[http_json]") + ": listed (line *), but skipped, not xfailed"])


# Case 2: a test the TCK xfails itself (a SHOULD requirement) that is not listed fails the run.
def test_an_unlisted_xfail_fails_the_run(tck) -> None:
    text = GOOD.replace(f"{OPS}::test_should[jsonrpc]  # a2a-tck#234: the TCK xfails it itself\n", "")
    r = tck(text, "--transport=jsonrpc")
    r.assert_outcomes(passed=4, skipped=9, xfailed=4)
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines([esc(f"{OPS}::test_should[jsonrpc]") + ": xfailed, but not listed (list it with its reason)"])


# Case 3: an entry that names no collected test fails the run, whatever its binding.
@pytest.mark.parametrize("node", [
    pytest.param(f"{OPS}::test_gone[jsonrpc]", id="no-such-test"),
    pytest.param(f"{OPS}::test_refused[grpc]", id="no-such-binding"),
    pytest.param("tests/compatibility/core_operations/test_typo.py::test_x", id="no-such-file"),
])
def test_an_entry_naming_no_test_fails_the_run(tck, node: str) -> None:
    r = tck(GOOD + f"{node}  # D18: stale\n", "--transport=http_json")
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines([esc(node) + ": listed (line *), but names no collected test"])


# Case 3 holds for the entries of every section, not only the run's variant's: a typo in [auth] fails a direct run.
def test_an_entry_of_another_variant_naming_no_test_fails_the_run(tck) -> None:
    node = f"{OPS}::test_gone[jsonrpc]"
    r = tck(GOOD + f"[auth]\n{node}  # D18: stale\n[direct]\n{OPS}::test_ok[jsonrpc]  # D18: not this binding\n",
            "--transport=http_json", variant="direct")
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines([esc(node) + ": listed (line *), but names no collected test", "*; 1 problem(s), so the run fails"])


# Case 4: an unlisted failure fails the run as pytest fails it; the other binding's entries do not apply.
def test_an_unlisted_failure_fails_the_run(tck) -> None:
    text = GOOD.replace(f"{OPS}::test_refused[http_json]  # D20: refused\n", "")
    r = tck(text, "--transport=http_json")
    r.assert_outcomes(passed=3, failed=1, skipped=10, xfailed=3)
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r2 = tck(text, "--transport=jsonrpc")
    assert r2.ret == pytest.ExitCode.OK, r2.stdout.str()


def test_a_listed_failure_of_another_binding_is_not_marked(tck) -> None:
    # test_refused[http_json] skips in a jsonrpc run: listed for http_json only, it is no problem there.
    r = tck(GOOD, "--transport=jsonrpc")
    assert r.ret == pytest.ExitCode.OK
    assert "test_refused[http_json]: listed" not in r.stdout.str()


def test_all_bindings(tck) -> None:
    r = tck(GOOD, "--transport=jsonrpc,http_json")
    r.assert_outcomes(passed=7, skipped=2, xfailed=8)
    assert r.ret == pytest.ExitCode.OK, r.stdout.str()


SECTIONED = GOOD.replace(f"skip {OPS}::test_setup_refused[jsonrpc]  # D20: its push config is refused\n", "") + f"""
[direct tunnel]
{OPS}::test_setup_refused[jsonrpc]  # D20: only these variants fail it this way

[auth]
skip {OPS}::test_setup_refused[jsonrpc]  # D20: the auth variant skips it
{OPS}::test_ok[jsonrpc]  # DM4-12: only the auth variant
"""


def test_sections_apply_to_their_variants(tck) -> None:
    # test_setup_refused skips, so listing it as a failure for direct fails a direct run; the auth entries do not
    # apply there.
    r = tck(SECTIONED, "--transport=jsonrpc", variant="direct")
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    out = r.stdout.str()
    assert "test_setup_refused[jsonrpc]: listed" in out
    assert "test_ok[jsonrpc]: listed" not in out
    # In an auth run the direct section does not apply, and test_ok is an entry that passes: strict XPASS.
    r = tck(SECTIONED, "--transport=jsonrpc", variant="auth")
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines([esc(f"{OPS}::test_ok[jsonrpc]") + ": listed (line *), but xpassed(strict), not xfailed"])
    assert "test_setup_refused[jsonrpc]: listed" not in r.stdout.str()


@pytest.mark.parametrize("text,variant,error", [
    pytest.param(f"{OPS}::test_refused[jsonrpc]\n", None, "has no reason", id="no-reason"),
    pytest.param(f"{OPS}::test_refused[jsonrpc]  #\n", None, "has no reason", id="empty-reason"),
    pytest.param(f"{OPS}::test_refused[jsonrpc]  # refused by the gate\n", None,
                 "cites no a2a-tck#<n>, D<n> or DM4-<n>", id="no-reference"),
    pytest.param(f"{OPS}::test_refused[jsonrpc]  # D20: a\n{OPS}::test_refused[jsonrpc]  # D20: b\n", None,
                 "is listed already", id="twice"),
    pytest.param("test_refused[jsonrpc]  # D20: a\n", None, "is not a pytest node id", id="not-a-node-id"),
    pytest.param("skip  # D20: a\n", None, "is not a pytest node id", id="skip-without-node-id"),
    pytest.param(f"skips {OPS}::test_refused[jsonrpc]  # D20: a\n", None, "is not a pytest node id", id="skips"),
    pytest.param(f"skip {OPS}::test_refused[jsonrpc]\n", None, "has no reason", id="skip-without-reason"),
    pytest.param(f"skip {OPS}::test_refused[jsonrpc]  # refused\n", None, "cites no a2a-tck#<n>, D<n> or DM4-<n>",
                 id="skip-without-reference"),
    pytest.param(f"{OPS}::test_refused[jsonrpc]  # D20: a\nskip {OPS}::test_refused[jsonrpc]  # D20: b\n", None,
                 "is listed already", id="a-failure-and-a-skip"),
    pytest.param(SECTIONED, None, "set TCK_VARIANT", id="sections-without-variant"),
    pytest.param(SECTIONED, "caddy", "TCK_VARIANT='caddy' is not a variant any section names", id="unknown-variant"),
    pytest.param("[direct]\n[auth]\n" + f"{OPS}::test_ok[jsonrpc]  # D20: x\n", "auth", "an empty section",
                 id="empty-section"),
    pytest.param("[Direct!]\n", "direct", "a section names variants", id="bad-section"),
])
def test_a_malformed_file_is_a_usage_error(tck, text: str, variant: str | None, error: str) -> None:
    r = tck(text, "--transport=jsonrpc", variant=variant)
    assert r.ret == pytest.ExitCode.USAGE_ERROR
    assert error in r.stderr.str()


def test_no_file_is_a_usage_error(pytester: pytest.Pytester, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("TCK_EXPECTED_FAILURES", raising=False)
    pytester.makepyfile("def test_x(): pass")
    r = pytester.runpytest("-p", "kontra_xfail", "-p", "no:cacheprovider")
    assert r.ret == pytest.ExitCode.USAGE_ERROR
    assert "set TCK_EXPECTED_FAILURES" in r.stderr.str()


def test_deselected_entries_are_not_problems(tck) -> None:
    r = tck(GOOD, "--transport=jsonrpc", "-k", "not test_should")
    assert r.ret == pytest.ExitCode.OK, r.stdout.str()
    r.stdout.fnmatch_lines(["kontra_xfail: variant -, bindings jsonrpc: 3 of 4 expected failures xfailed, "
                            "1 of 1 expected skips skipped, 1 deselected"])


def test_the_junit_report_carries_the_problems(tck, pytester: pytest.Pytester) -> None:
    report = pytester.path / "junit.xml"
    r = tck(GOOD + f"{OPS}::test_ok[jsonrpc]  # D20: stale\n", "--transport=jsonrpc", f"--junitxml={report}")
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    xml = report.read_text()
    assert 'name="kontra_xfail"' in xml and "test_ok[jsonrpc]: listed" in xml


# Case 5 (M4 WP-02's review): a test of the run's binding that skips and is not listed fails the run, because the TCK
# skips whenever a prerequisite request fails, so a regression would otherwise turn a pass into a skip silently; the
# same test of the other binding skips "not configured" and needs no entry.
def test_an_unlisted_skip_fails_the_run(tck) -> None:
    text = GOOD.replace(f"skip {OPS}::test_setup_refused[http_json]  # D20: its push config is refused\n", "")
    r = tck(text, "--transport=http_json")
    r.assert_outcomes(passed=3, skipped=10, xfailed=4)
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines([esc(f"{OPS}::test_setup_refused[http_json]") +
                            ": skipped (CreatePushNotificationConfig failed), but not listed *",
                            "*; 1 problem(s), so the run fails"])
    r2 = tck(text, "--transport=jsonrpc")
    assert r2.ret == pytest.ExitCode.OK, r2.stdout.str()


def test_an_unlisted_skip_of_a_test_of_every_binding_fails_the_run(tck, pytester: pytest.Pytester) -> None:
    # A test with no binding in its id, directory, name or class (the TCK's test_should_requirement[no-requirements]
    # is parametrized; this one is not) counts for every binding.
    (pytester.path / "tests/compatibility/core_operations/test_plain.py").write_text(
        "import pytest\n\ndef test_prerequisite():\n    pytest.skip('Expected task state INPUT_REQUIRED')\n")
    r = tck(GOOD, "--transport=jsonrpc")
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines(["tests/compatibility/core_operations/test_plain.py::test_prerequisite: skipped "
                            "(Expected task state INPUT_REQUIRED), but not listed *"])
    r = tck(GOOD + "skip tests/compatibility/core_operations/test_plain.py::test_prerequisite  # DM4-24: x\n",
            "--transport=http_json")
    assert r.ret == pytest.ExitCode.OK, r.stdout.str()


# A listed skip that passes, fails or xfails fails the run: the skips, as the failures, are exactly the file's.
@pytest.mark.parametrize("node,got", [
    pytest.param(f"{OPS}::test_ok[jsonrpc]", "passed", id="passed"),
    pytest.param(f"{OPS}::test_should[jsonrpc]", "xfailed", id="xfailed"),
    pytest.param(f"{OPS}::test_must_requirement[REQ-1-jsonrpc]", "failed", id="failed"),
])
def test_a_listed_skip_that_does_not_skip_fails_the_run(tck, node: str, got: str) -> None:
    text = "\n".join(line for line in GOOD.splitlines() if not line.startswith(f"{node}  #")) + "\n"
    r = tck(text + f"skip {node}  # D20: stale\n", "--transport=jsonrpc")
    assert r.ret == pytest.ExitCode.TESTS_FAILED
    r.stdout.fnmatch_lines([esc(node) + f": listed as a skip (line *), but {got}, not skipped"])
    assert f"{node}: xfailed, but not listed" not in r.stdout.str()


# Case 6 (M4 WP-02's review): the TCK's unparametrized tests in its shared directories carry their binding in their
# function's name or their class's name. An entry for one counts for that binding only: listed for http_json, it is
# no problem in a jsonrpc run, where it skips "not configured" (before, it was "skipped, not xfailed" there, so such
# a test could never be listed).
def test_a_test_named_for_its_binding_counts_for_that_binding_only(tck) -> None:
    node = f"{ERRORS}::TestCoreInputValidation::test_malformed_request_rejected_rest"
    r = tck(GOOD, "--transport=jsonrpc")
    assert r.ret == pytest.ExitCode.OK, r.stdout.str()
    assert f"{node}: listed" not in r.stdout.str()
    r = tck(GOOD, "--transport=http_json")
    assert r.ret == pytest.ExitCode.OK, r.stdout.str()
    r.stdout.fnmatch_lines(["XFAIL " + esc(node) + " - a2a-tck#999*"])


T = "tests/compatibility"
EVERY = {"jsonrpc", "http_json", "grpc"}


@pytest.mark.parametrize("node,want", [
    (f"{T}/core_operations/test_error_handling.py::TestCoreInputValidation::test_malformed_request_rejected_rest", {"http_json"}),
    (f"{T}/core_operations/test_error_handling.py::TestCoreInputValidation::test_malformed_request_rejected_jsonrpc", {"jsonrpc"}),
    (f"{T}/core_operations/test_data_model.py::TestIgnoreUnrecognizedFields::test_extra_fields_ignored_rest", {"http_json"}),
    (f"{T}/core_operations/test_error_handling.py::TestExtendedCardNotConfigured::test_extended_card_not_configured_grpc", {"grpc"}),
    (f"{T}/core_operations/test_error_handling.py::TestCapabilityExtensionRequired::test_missing_required_extension_returns_error_http_json", {"http_json"}),
    (f"{T}/core_operations/test_error_handling.py::TestRestErrorStructure::test_error_uses_aip193_format", {"http_json"}),
    (f"{T}/core_operations/test_error_handling.py::TestJsonRpcErrorStructure::test_a2a_error_codes_in_range", {"jsonrpc"}),
    (f"{T}/core_operations/test_error_handling.py::TestGrpcErrorStructure::test_grpc_error_for_nonexistent_task", {"grpc"}),
    (f"{T}/core_operations/test_transport_behavior.py::TestGrpcProto3::test_response_uses_proto3", {"grpc"}),
    (f"{T}/core_operations/test_transport_behavior.py::TestRestFormat::test_response_content_type", {"http_json"}),
    (f"{T}/core_operations/test_requirements.py::test_must_requirement[CORE-LIST-001-jsonrpc]", {"jsonrpc"}),
    (f"{T}/core_operations/test_requirements.py::test_should_requirement[no-requirements-http_json]", {"http_json"}),
    (f"{T}/core_operations/test_artifacts.py::TestMessageResponse::test_returns_message_with_text_part[grpc]", {"grpc"}),
    (f"{T}/http_json/test_http_status.py::TestHttpJsonStatusCodes::test_content_type_not_supported_returns_415", {"http_json"}),
    (f"{T}/jsonrpc/test_error_codes.py::TestJsonRpcErrorCodeMappings::test_push_notification_not_supported_error", {"jsonrpc"}),
    # a class or function that only looks like it names a binding, and a test that names none
    (f"{T}/core_operations/test_x.py::TestRestartable::test_resume", EVERY),
    (f"{T}/core_operations/test_x.py::TestGrpcish::test_x", EVERY),
    (f"{T}/core_operations/test_x.py::test_rest_api_shape", EVERY),
    (f"{T}/core_operations/test_requirements.py::test_must_requirement[no-requirements]", EVERY),
])
def test_binding_of(node: str, want: set[str]) -> None:
    import kontra_xfail

    assert kontra_xfail.binding_of(node) == frozenset(want)
