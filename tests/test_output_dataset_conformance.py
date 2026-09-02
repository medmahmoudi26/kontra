"""The PYTHON arm of the output-Dataset author-surface contract (shared/conformance/output_dataset.json).

A per-SDK check against its own docs is not the two SDKs agreeing, and this repo has been bitten by
exactly that. So the granularities ADR 0028 §1 promises — one record per Unit, N per Unit, one per
three, one for the whole Batch, none at all — are pinned in a FIXTURE the Go peer asserts too
(runtime/go/engine/output_dataset_conformance_test.go), not just in each side's own tests.
Each side selects the body named by `case` and drives it through its real engine in inline mode (no
object store, so `results` holds the pushed records themselves), then compares to `expected`.

The bodies here are the contract's Python side — the same behaviour the Go test writes in Go, so a
drift in either surfaces as a fixture mismatch rather than as two green suites that disagree.
"""

import asyncio
import json
import pathlib

import pytest

from test_actor_engine import make_host

FIXTURE = (
    pathlib.Path(__file__).resolve().parents[1] / "shared" / "conformance" / "output_dataset.json"
)


def _body(case: str):
    """The Method the named case describes — the peer of bodyFor(...) on the Go side."""
    if case == "one-per-unit":
        async def m(self, batch, dataset):
            async for unit in batch:
                await dataset.push({"u": unit.value})
        return m
    if case == "one-per-three":
        async def m(self, batch, dataset):
            async for unit in batch:
                if unit.index % 3 == 0:
                    await dataset.push({"i": unit.index})
        return m
    if case == "n-per-unit":
        async def m(self, batch, dataset):
            async for unit in batch:
                for k in range(unit.value):
                    await dataset.push({"seed": unit.value, "k": k})
        return m
    if case == "none-for-a-unit":
        async def m(self, batch, dataset):
            async for unit in batch:
                if unit.value != "skip":
                    await dataset.push({"u": unit.value})
        return m
    if case == "whole-batch":
        async def m(self, batch, dataset):
            n = 0
            async for unit in batch:
                n += 1
            await dataset.push({"total": n}, key="total")  # no current Unit -> keyed tail
        return m
    raise AssertionError(f"no Python body for conformance case {case!r} — fixture and this disagree")


def _cases():
    fx = json.loads(FIXTURE.read_text())
    assert fx["cases"], "fixture is empty — a vacuously passing conformance test is worse than none"
    return fx["cases"]


@pytest.mark.parametrize("case", _cases(), ids=lambda c: c["case"])
def test_output_dataset_surface_matches_the_fixture(case):
    host = make_host(_body(case["case"]))
    out = asyncio.run(host.run_batch({"units": case["units"], "run_id": "r", "node_id": "n"}))
    assert out["done"] is True
    assert out["results"] == case["expected"]
