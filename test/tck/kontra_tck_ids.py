"""pytest plugin: fresh TCK message ids for every test (spec/relay.md §16 tck_*; D291).

Loaded into the pinned A2A TCK's own pytest with ``-p kontra_tck_ids`` (scripts/tck.sh), beside kontra_xfail; no TCK
file is changed. The TCK names its messages ``tck_id(name)`` = ``tck-<name>-<_SESSION>`` (a2a-tck
tck/requirements/base.py), one ``_SESSION`` per pytest session, so every test that creates the same prerequisite
task (an input-required task, a completed one) sends the same request with the same messageId. The gate answers
such a request as a resend, with the first task (spec/relay.md §15; A2A §3.3.1 lets an agent detect duplicates by
message id), and once an earlier test has completed or canceled that task the later tests skip ("Expected task
state 'TASK_STATE_INPUT_REQUIRED' but got 'TASK_STATE_COMPLETED'"): CORE-CANCEL-001, STREAM-SUB-001/002,
CORE-MULTI-005 and the history tests never ran through the gate.

An autouse fixture gives ``tck.requirements.base._SESSION`` a fresh value for each test, so each test's
prerequisites are new tasks, as the TCK means them to be ("each test run produces fresh IDs"). The ids the TCK's
requirement catalogue computed at import keep the session's value. A TCK without ``tck.requirements.base._SESSION``
is an error in every test, so a pin that moves it cannot silently bring the resends back.
"""

from __future__ import annotations

import importlib
import uuid

import pytest

MODULE = "tck.requirements.base"


@pytest.fixture(autouse=True)
def kontra_fresh_tck_ids(monkeypatch: pytest.MonkeyPatch) -> None:
    base = importlib.import_module(MODULE)
    if not isinstance(getattr(base, "_SESSION", None), str) or not callable(getattr(base, "tck_id", None)):
        pytest.fail(f"kontra_tck_ids: {MODULE} has no _SESSION or tck_id: the pinned TCK changed (D291)",
                    pytrace=False)
    monkeypatch.setattr(base, "_SESSION", uuid.uuid4().hex[:8])
