"""The determinism guard: every captured history must still replay against current code.

WHAT THIS CATCHES that no other test can. Temporal workflow code is replayed against its own history
every time a worker picks up a mid-flight execution. Reorder two activity calls, add one before an
existing one, or change a branch that was already taken, and every RUNNING instance fails with a
non-determinism error. The edit looks harmless, the unit tests pass, and the damage lands on
executions that started before the deploy — where nobody is looking.

SO THE FIXTURES ARE CHECKED IN. A history is the contract between yesterday's code and today's, and
this repo already has the rule for a contract with two writers: it gets a corpus, not two literals
(`shared/conformance/`). These are the same family.

WHAT IT DOES NOT PROVE, and the README beside each fixture repeats it: activity code is not run.
Replay executes WORKFLOW code and feeds activity results from the history as recorded values. A
green corpus says the caller's decisions still line up; it says nothing about whether a Method works.
"""

from __future__ import annotations

import asyncio
import json
from pathlib import Path

import pytest

from internals.replay import Outcome, replay_history, workflow_classes

# ONE resolution of the corpus root, asserted to exist. A path assembled per-test with `..` segments
# that lands one directory short finds zero fixtures — and a walk that finds nothing reports success,
# which is how a guard passes while testing nothing. This repo has been bitten by exactly that.
CORPUS = Path(__file__).resolve().parents[3] / "testdata"

# How many fixtures must exist. Raising this is a deliberate act; it is here so that DELETING a
# fixture, or mis-resolving the root, fails loudly rather than quietly shrinking the guard.
MINIMUM_FIXTURES = 1


def discover() -> list[Path]:
    """Every directory holding both a `workflow.py` and a `history.json`."""
    assert CORPUS.is_dir(), f"corpus root not found at {CORPUS}; this guard would test nothing"
    return sorted(d for d in CORPUS.iterdir() if (d / "workflow.py").is_file() and (d / "history.json").is_file())


def test_the_corpus_is_not_empty():
    """THE GUARD ON THE GUARD. Everything below iterates; an empty iteration is a green suite."""
    found = discover()
    assert len(found) >= MINIMUM_FIXTURES, (
        f"found {len(found)} replay fixtures under {CORPUS}, expected at least {MINIMUM_FIXTURES}. "
        "A corpus that finds nothing passes every test below without replaying anything."
    )


@pytest.mark.parametrize("fixture", discover(), ids=lambda p: p.name)
def test_history_still_replays(fixture: Path):
    history = json.loads((fixture / "history.json").read_text())
    assert history.get("events"), f"{fixture.name}/history.json has no events"

    workflows = workflow_classes(str(fixture / "workflow.py"))
    assert workflows, f"{fixture.name}/workflow.py declares no @workflow.defn class"

    result = asyncio.run(replay_history(history, workflows))
    assert result.outcome is Outcome.OK, (
        f"{fixture.name} no longer replays against its own workflow code.\n"
        f"outcome={result.outcome.value}\n{result.detail}\n\n"
        "If this is an intentional change to a workflow, every execution already in flight will "
        "fail on it. Version the workflow rather than editing it in place."
    )


@pytest.mark.parametrize("fixture", discover(), ids=lambda p: p.name)
def test_each_fixture_explains_itself(fixture: Path):
    """A history is large and unreadable. Without a note, a failure cannot be told apart from a
    fixture that was always wrong."""
    readme = fixture / "README.md"
    assert readme.is_file(), f"{fixture.name} has no README.md saying what shape it captures"
    assert len(readme.read_text().strip()) > 200, f"{fixture.name}/README.md is too thin to be useful"
