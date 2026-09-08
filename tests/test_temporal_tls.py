"""The PYTHON ARM of `shared/conformance/temporal_tls.json`, and the sweep that keeps it honest.

Three languages derive one environment contract across eighteen call sites. What the corpus pins is
the DECISION — TLS on or off, which material is present, and which misconfigurations are refusals —
because that is what an operator configures and what must not disagree between the orchestrator, the
hosts and the CLI. A deployment where one process reads the contract differently is one process
talking plaintext to a server that accepts both, which looks like it works.
"""
from __future__ import annotations

import json
import pathlib
import re
import subprocess
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "runtime" / "python"))

from internals.temporal.tlsconfig import (  # noqa: E402
    TLS_VARS,
    connect_tls,
    describe,
    tls_requested,
)

ROOT = pathlib.Path(__file__).resolve().parents[1]
CORPUS = ROOT / "shared" / "conformance" / "temporal_tls.json"


def _corpus() -> dict:
    doc = json.loads(CORPUS.read_text(encoding="utf-8"))
    # A corpus of nothing passes every assertion below.
    assert len(doc["cases"]) >= 8, "the corpus shrank"
    assert len(doc["refusal_cases"]) >= 3, "the refusals shrank"
    blob = CORPUS.read_text(encoding="utf-8")
    for marker in ("OFF DOES NOT OVERRIDE A CA", "SILENT DOWNGRADE"):
        assert marker in blob, f"the corpus no longer carries {marker!r}"
    return doc


@pytest.fixture(scope="module")
def pair(tmp_path_factory) -> dict[str, str]:
    """A real, self-signed pair, generated per run.

    The corpus deliberately carries no key material — even a throwaway private key is not a thing to
    commit — and the Go arm validates the pair at configuration time, so arbitrary bytes would not
    do there. Generated with `openssl` because it is what this box has; the Python arm hands the
    bytes to the SDK without parsing them, so the contents matter only for cross-arm agreement.
    """
    d = tmp_path_factory.mktemp("tls")
    key, crt = d / "key.pem", d / "crt.pem"
    subprocess.run(
        ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
         "-subj", "/CN=kontra-test", "-keyout", str(key), "-out", str(crt)],
        check=True, capture_output=True,
    )
    return {"@ca": str(crt), "@crt": str(crt), "@key": str(key)}


def _env(case_env: dict, pair: dict[str, str]) -> dict:
    """Substitute `@ca`/`@crt`/`@key` for this run's real paths; pass anything else through — that
    is how the refusal rows carry `/nope/missing-ca.pem`."""
    return {k: pair.get(v, v) for k, v in case_env.items()}


@pytest.mark.parametrize("case", _corpus()["cases"], ids=lambda c: c["why"][:60])
def test_the_decision_matches_the_corpus(case, pair) -> None:
    env = _env(case["env"], pair)
    assert tls_requested(env) is case["tls"], case["why"]
    tls = connect_tls(env)

    if not case["tls"]:
        # PLAINTEXT IS THE DEFAULT, and it is `False` — what the SDK means by no TLS and what both
        # hosts effectively passed before this module existed.
        assert tls is False
        return

    assert tls is not False
    assert bool(getattr(tls, "server_root_ca_cert", None)) is case["ca"]
    assert bool(getattr(tls, "client_cert", None)) is case["client_pair"]
    assert (getattr(tls, "domain", None) or "") == case["server_name"]


@pytest.mark.parametrize("case", _corpus()["refusal_cases"], ids=lambda c: c["why"][:60])
def test_a_refusal_names_what_is_wrong_and_never_falls_back(case, pair) -> None:
    with pytest.raises(ValueError) as excinfo:
        connect_tls(_env(case["env"], pair))
    message = str(excinfo.value)
    for name in case["names"]:
        assert name in message, f"the error does not name {name!r}: {message}"


def test_the_switch_is_read_the_same_way_everywhere() -> None:
    doc = _corpus()
    for value in doc["truthy"]:
        assert tls_requested({"KONTRA_TEMPORAL_TLS": value}), value
    for value in doc["falsy"]:
        assert not tls_requested({"KONTRA_TEMPORAL_TLS": value}), value


def test_key_material_never_reaches_a_message(pair) -> None:
    # The PATH is named on purpose — an operator needs to know which setting pointed where, and a
    # path is a filesystem location. The BYTES are the secret.
    body = pathlib.Path(pair["@key"]).read_text(encoding="utf-8")
    secret_lines = [ln for ln in body.splitlines() if len(ln) > 40]
    assert secret_lines, "the fixture key has no long lines — this assertion would be vacuous"

    with pytest.raises(ValueError) as excinfo:
        connect_tls({"KONTRA_TEMPORAL_TLS_CERT": "/nope/missing.pem", "KONTRA_TEMPORAL_TLS_KEY": pair["@key"]})
    message = str(excinfo.value)
    for line in secret_lines:
        assert line not in message, "the error carries key material"
    assert "KONTRA_TEMPORAL_TLS_CERT" in message


def test_describe_names_the_mode_and_not_the_material(pair) -> None:
    assert describe("a:1", False) == "a:1 (plaintext)"
    full = connect_tls({
        "KONTRA_TEMPORAL_TLS_CA": pair["@ca"],
        "KONTRA_TEMPORAL_TLS_CERT": pair["@crt"],
        "KONTRA_TEMPORAL_TLS_KEY": pair["@key"],
        "KONTRA_TEMPORAL_TLS_SERVER_NAME": "temporal.internal",
    })
    assert describe("t:1", full) == "t:1 (TLS, private CA, client certificate, SNI temporal.internal)"


# --- the sweep -----------------------------------------------------------------------------------

CONNECT = re.compile(r"Client\.connect\(")


def _python_sources() -> dict[str, str]:
    skip = {".git", "node_modules", "dist", "__pycache__", "_gen", ".scratch", "build"}
    out: dict[str, str] = {}
    for p in ROOT.rglob("*.py"):
        rel = p.relative_to(ROOT)
        if any(part in skip for part in rel.parts):
            continue
        out[str(rel)] = p.read_text(encoding="utf-8", errors="replace")
    return out


def test_the_walk_finds_the_tree_it_is_walking() -> None:
    # NON-VACUOUS. A walk that returns nothing passes both assertions below, which is how a guard
    # reports success for a tree it never read — this repository's most repeated failure shape.
    sources = _python_sources()
    assert len(sources) > 50, f"only {len(sources)} Python sources under {ROOT}"
    assert "runtime/python/internals/temporal/host.py" in sources
    assert "runtime/python/internals/temporal/wfhost.py" in sources


def test_every_python_temporal_client_goes_through_one_module() -> None:
    sites, bare = [], []
    for rel, body in _python_sources().items():
        if rel.startswith("tests/") or "tlsconfig.py" in rel:
            continue
        for line in body.splitlines():
            if not CONNECT.search(line) or line.strip().startswith("#"):
                continue
            sites.append(f"{rel}: {line.strip()}")
            # The options are built a line or two above the connect, so the FILE is the unit.
            if "connect_tls(" not in body:
                bare.append(f"{rel}: {line.strip()}")
    # The count first: a regex that stopped matching would report a clean sweep over nothing.
    assert len(sites) >= 2, f"found {len(sites)} Client.connect sites — the pattern stopped matching"
    assert bare == [], "a Temporal client that bypasses connect_tls:\n  " + "\n  ".join(bare)


def test_nothing_else_reads_the_tls_environment() -> None:
    # One module, one reading. A call site consulting KONTRA_TEMPORAL_TLS_* itself would be a second
    # policy that agrees today and drifts later.
    offenders = [
        f"{rel} reads {var}"
        for rel, body in _python_sources().items()
        if not rel.startswith("tests/") and "tlsconfig.py" not in rel
        for var in TLS_VARS
        if var in body
    ]
    assert offenders == [], "the TLS environment is read outside tlsconfig:\n  " + "\n  ".join(offenders)
