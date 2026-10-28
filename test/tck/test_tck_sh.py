"""Checks of scripts/tck.sh's own text (M4 WP-02's review), run by scripts/tck.sh with the other self-tests of
test/tck. The script cannot run without a gate, Temporal, Postgres and Caddy, so these read it: each pins a defect
the review found, which a full caddy run on an unsynced TCK checkout, or a KONTRA_TEST_PG with '&', showed."""

from __future__ import annotations

import re
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "tck.sh"


def code() -> list[str]:
    """The script's lines without comments."""
    return [line for line in SCRIPT.read_text().splitlines() if not line.lstrip().startswith("#")]


def first(pattern: str) -> int:
    for n, line in enumerate(code()):
        if re.search(pattern, line):
            return n
    raise AssertionError(f"scripts/tck.sh has no line matching {pattern!r}")


def test_ssl_cert_file_is_never_in_the_scripts_environment() -> None:
    # uv honours SSL_CERT_FILE: exported as Caddy's local CA, it failed every download from PyPI ("invalid peer
    # certificate: UnknownIssuer"), so a caddy run on an unsynced TCK checkout died before the TCK started.
    text = "\n".join(code())
    assert not re.search(r"\bexport\s+SSL_CERT_FILE\b", text)
    assert not re.search(r"^\s*SSL_CERT_FILE=", text, re.MULTILINE)
    assert re.search(r'tck_env=\(SSL_CERT_FILE="\$ca"\)', text), "the TCK's command gets the CA through tck_env"


def test_the_tck_environment_is_synced_before_anything_starts() -> None:
    sync = first(r"\buv sync --frozen\b")
    assert sync < first(r"^\s*start\s"), "uv sync runs after a process started"
    assert sync < first(r"\bstart caddy\b")
    runs = [line for line in code() if re.search(r'&&\s*(?:env\s+\S+\s+)?uv run\b', line)]
    assert len(runs) == 2, runs  # the self-tests and the TCK
    for line in runs:
        assert "--no-sync" in line and "--frozen" in line, f"a uv run that may sync: {line.strip()}"


def test_the_template_is_filled_literally() -> None:
    # sed "s|@POSTGRES@|$postgres|" turned each '&' of a KONTRA_TEST_PG with several query parameters into the
    # matched text, and the run died "has a placeholder this script does not fill".
    text = "\n".join(code())
    assert not re.search(r"\bsed\b[^\n]*@[A-Z_]+@", text), "a sed fills a placeholder"
    assert "test/tck/kontra_tck_config.py" in text
    for name in ("LISTEN", "PUBLIC_URL", "POSTGRES", "TEMPORAL", "SELLER"):
        assert f"KONTRA_TCK_FILL_{name}=" in text


def test_the_tck_runs_with_both_plugins() -> None:
    line = next(line for line in code() if "-p kontra_xfail" in line)
    assert "-p kontra_tck_ids" in line
