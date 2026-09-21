"""The two DuckDB engines must be one version, because they share one catalog.

THERE ARE TWO, AND THIS IS THE ONLY PLACE THAT SAYS SO:

    the orchestrator  embeds `@duckdb/node-api` (control/orchestrator/package.json) — it answers
                      `/api/datasets/:name/preview` and the query workbench
    the CLI           shells out to a `duckdb` BINARY on PATH (cli/runs.go:duckdbBin) — it runs
                      `kontra dataset create|query|anew`

They open the SAME DuckLake catalog, and **DuckLake's catalog format is versioned with DuckDB**. So
these are not two independent choices that happen to be near each other; they are one decision
written in two files.

WHAT DRIFT COSTS, MEASURED against a catalog the embedded 1.5.4 engine had written:

    duckdb 1.1.3   HTTP 404 fetching the `ducklake` extension — it does not exist for that release
    duckdb 1.3.2   "Not implemented Error: Only DuckLake versions 0.1 and 0.2 are supported"
    duckdb 1.5.x   works

NONE OF THOSE MESSAGES NAMES A VERSION MISMATCH. They surface at the far end of a dataset write, as
an extension download or a catalog refusal, and an operator reading them has no reason to suspect
the Node package pinned in a different file. That is why this is a test and not a comment.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ORCH_PKG = ROOT / "control" / "orchestrator" / "package.json"
SELFCONTAINED = ROOT / "control" / "images" / "Dockerfile.selfcontained"
ORCHESTRATOR_IMAGE = ROOT / "control" / "images" / "Dockerfile.orchestrator"


def _embedded_version() -> str:
    """The `@duckdb/node-api` pin, without its packaging suffix — `1.5.4-r.1` -> `1.5.4`.

    The `-r.N` is the Node binding's own re-release counter and moves independently of DuckDB; the
    thing that has to match the CLI is the DuckDB version in front of it.
    """
    pkg = json.loads(ORCH_PKG.read_text())
    spec = pkg["dependencies"]["@duckdb/node-api"]
    m = re.match(r"^\D*(\d+\.\d+\.\d+)", spec)
    assert m, f"could not read a DuckDB version out of @duckdb/node-api spec {spec!r}"
    return m.group(1)


def _cli_version() -> str:
    m = re.search(r"^ARG DUCKDB_VERSION=(\S+)", SELFCONTAINED.read_text(), re.M)
    assert m, "Dockerfile.selfcontained no longer declares ARG DUCKDB_VERSION"
    return m.group(1)


def test_the_two_engines_are_pinned_to_one_duckdb_version() -> None:
    embedded, cli = _embedded_version(), _cli_version()
    assert embedded == cli, (
        f"the orchestrator embeds DuckDB {embedded} (@duckdb/node-api) and the image installs the "
        f"{cli} CLI. They share one DuckLake catalog and its format is versioned with DuckDB, so "
        f"the older of the two will refuse what the newer wrote — as 'Only DuckLake versions 0.1 "
        f"and 0.2 are supported', which names neither file. Move both, in one commit."
    )


def test_the_image_actually_installs_it() -> None:
    """A pin nothing installs is a comment.

    The whole bug this fixes was an image that shipped no `duckdb` at all, so every CLI dataset verb
    answered "duckdb not found on PATH — install it and retry" inside a container whose promise is
    that Docker is the only prerequisite.
    """
    body = SELFCONTAINED.read_text()
    assert "duckdb_cli-linux-" in body, "the selfcontained image no longer downloads the DuckDB CLI"
    assert "install -m 0755 /tmp/duckdb/duckdb /usr/local/bin/duckdb" in body, (
        "the DuckDB CLI is downloaded but not put on PATH — `duckdbBin()` looks there first"
    )


def test_the_orchestrator_image_carries_it_too() -> None:
    """`serveWorkflow` spawns `kontra` in the ORCHESTRATOR container, not the cli one.

    Shipping the CLI there without `duckdb` is shipping a binary that fails on a subset of its own
    commands, and the subset is exactly the dataset verbs.
    """
    body = ORCHESTRATOR_IMAGE.read_text()
    assert "COPY --from=spa-src /usr/local/bin/duckdb" in body, (
        "the orchestrator image ships `kontra` without the `duckdb` its dataset verbs shell out to"
    )


def test_both_arches_are_handled() -> None:
    """A Fleet Machine or a laptop may be arm64, and an unhandled TARGETARCH must FAIL THE BUILD
    rather than produce an image whose CLI is missing one binary."""
    body = SELFCONTAINED.read_text()
    assert "amd64) duckarch=amd64" in body and "arm64) duckarch=arm64" in body
    assert "no duckdb build for TARGETARCH" in body, (
        "an unknown TARGETARCH must stop the build; a silently-skipped install is the bug this "
        "whole file exists to prevent, one layer up"
    )
