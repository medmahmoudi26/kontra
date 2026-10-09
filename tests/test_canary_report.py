"""The canary's return value and its `report.md` agree, field for field.

`kontra workflow serve` lints a template's syntax and its context ROOTS, and stops there: it does
not know the workflow's return type, so `{{ result.recrods }}` passes the lint and fails the render
of every canary on every install. The renderer runs with `strictVariables`, so a misspelt field is
not an empty cell, it is a stored error version where the first report anybody reads should be.

This is the check the lint does not make, for the one workflow where it matters most: every
`result.<field>` and `t.<field>` the template reads must exist on `CanaryResult` /
`TargetOutcome`. It also pins `_result`'s three outcomes, which are what the template branches on.

The rendering itself is `control/orchestrator/src/report/canaryTemplate.test.ts`; the whole thing,
on a fresh install, in a browser, is the console e2e `e2e/canary-report.spec.ts`.
"""

from __future__ import annotations

import dataclasses
import importlib.util
import json
import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
CANARY = ROOT / "workspaces" / "default" / "workflows" / "canary"


@pytest.fixture(scope="module")
def wf():
    spec = importlib.util.spec_from_file_location("canary_workflow", CANARY / "workflow.py")
    mod = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    return mod


def _fields(cls) -> set[str]:
    return {f.name for f in dataclasses.fields(cls)}


def test_every_field_the_template_reads_is_one_the_workflow_returns(wf):
    text = (CANARY / "report.md").read_text()

    used_result = set(re.findall(r"\bresult\.([a-z_]+)", text))
    used_target = set(re.findall(r"\bt\.([a-z_]+)", text))
    # The loop variable must be what the per-target fields are read through, or the second set is
    # silently empty and this test checks nothing about the table.
    assert "{% for t in result.targets" in text
    assert used_result and used_target

    assert used_result <= _fields(wf.CanaryResult), (
        f"report.md reads result.{sorted(used_result - _fields(wf.CanaryResult))} "
        "which CanaryResult does not have")
    assert used_target <= _fields(wf.TargetOutcome), (
        f"report.md reads t.{sorted(used_target - _fields(wf.TargetOutcome))} "
        "which TargetOutcome does not have")


def _call(wf, **over):
    args = {"targets": ["alpha", "beta"], "steps": 5, "every": 2.0, "provider": "docker",
            "machines": 1, "sessions": 1, "dataset": "canary_signals", "records": 10,
            "complete": True, "voided": "", "dropped": {}, "dropped_count": 0,
            "run": "canary-1791506965"}
    args.update(over)
    return wf._result(**args)


def test_a_clean_run_sweeps_every_target(wf):
    r = _call(wf)
    assert r.complete and r.records == r.expected == 10
    assert [(t.target, t.records, t.outcome, t.error) for t in r.targets] == [
        ("alpha", 5, "swept", None), ("beta", 5, "swept", None)]
    assert r.voided is None
    assert r.summary.startswith("All 2 targets swept: 10 of 10 records landed in canary_signals")
    assert "1 docker machine that is now destroyed" in r.summary
    assert "where run_id = 'canary-1791506965'" in r.query


def test_a_dropped_target_is_named_with_its_error(wf):
    r = _call(wf, records=5, complete=False, dropped_count=1,
              dropped={"beta": "canary: refusing beta because fail_on names it"})
    assert [(t.target, t.records, t.outcome) for t in r.targets] == [
        ("alpha", 5, "swept"), ("beta", 0, "dropped")]
    assert r.targets[1].error == "canary: refusing beta because fail_on names it"
    assert r.summary.startswith("1 of 2 targets dropped: 5 of 10 records")


def test_drops_that_could_not_be_named_leave_no_target_called_swept(wf):
    r = _call(wf, records=5, complete=False, dropped_count=1, dropped={})
    assert [(t.target, t.records, t.outcome) for t in r.targets] == [
        ("alpha", None, "unknown"), ("beta", None, "unknown")]
    assert r.summary.startswith("1 of 2 targets dropped")


def test_a_voided_sweep_claims_nothing_about_any_target(wf):
    r = _call(wf, records=0, complete=False, voided="ActivityError('boom')")
    assert {t.outcome for t in r.targets} == {"voided"}
    assert all(t.records == 0 for t in r.targets)
    assert r.voided == "ActivityError('boom')"


def test_the_result_reaches_the_report_as_json_with_nulls_not_empty_strings(wf):
    """A Liquid template treats "" as TRUE. The template's `{% if result.voided %}` and
    `{% if t.error %}` are only false for a clean run because these arrive as null."""
    from temporalio.converter import DataConverter

    payloads = DataConverter.default.payload_converter.to_payloads([_call(wf)])
    decoded = json.loads(payloads[0].data)
    assert decoded["voided"] is None
    assert decoded["targets"][0] == {"target": "alpha", "records": 5, "outcome": "swept",
                                     "error": None}
    assert set(decoded) == _fields(wf.CanaryResult)
