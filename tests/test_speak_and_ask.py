"""The two things a workflow says out loud, and the difference between them.

`test_narration.py` and `test_hitl_ask.py` each hold one half in isolation, and both stay exactly
as they were: the mechanisms did not change. WHAT THIS FILE HOLDS IS THE PAIR — that `speak` and
`ask` are reachable side by side at the top level, that they are the SAME functions the older
names reach rather than second implementations, and that the distinction between them survives
being easy to reach.

THE DISTINCTION IS THE REASON THERE ARE TWO, so it is asserted rather than only documented:
`speak` costs history and RETURNS IMMEDIATELY; `ask` costs history AND STOPS THE RUN until a human
moves it. Confusing them turns a progress line into a stalled run, which is a failure nothing
downstream can distinguish from a hung worker.

THE WORKFLOW SEAM IS FAKED, not a cluster — the same monkeypatch-the-module seam both suites next
door use, here with BOTH halves on one fake so that "what this run wrote about itself" is a single
object. That is what makes the credential assertion meaningful over the pair: one run speaks and
asks, and the sentinel must be findable in neither the sentences nor the memo.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import logging
import os
import subprocess
import sys
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Callable, Optional

import pytest
from temporalio import workflow as temporal_workflow

import actorkit
from actorkit import ask, hitl, narrate, speak
from actorkit.narrate import MAX_SENTENCE_BYTES, MAX_SENTENCES, NarrationRefused

ROOT = Path(__file__).resolve().parent.parent
PKG_INIT = ROOT / "sdk" / "python" / "actorkit" / "__init__.py"

#: A value that must never be findable in anything the run writes about itself.
SENTINEL = "kontra-sentinel-9f2a1c-do-not-leak"

T0 = datetime(2026, 8, 27, 12, 0, 0, tzinfo=timezone.utc)


@dataclass
class Approval:
    approve: bool
    note: str = ""


# ---------------------------------------------------------------------------------------------
# The seam: one run, and everything it said out loud
# ---------------------------------------------------------------------------------------------


class _Instance:
    """The author's `@workflow.defn` object — what both modules hang per-run state on."""


class _Wait:
    def __init__(self, fn: Callable[[], bool], deadline: Optional[datetime]) -> None:
        self.fn = fn
        self.deadline = deadline
        self.wake = asyncio.Event()


class Fake:
    """The workflow both verbs think they are running inside.

    ONE FAKE FOR BOTH, unlike the two suites next door, because the question here is about the pair:
    which of the two parked, what each cost, and what the run as a whole wrote down. {@link history}
    is that last one — every sentence and every memo write, as one blob a sentinel can be searched
    for.
    """

    def __init__(self) -> None:
        self.instance = _Instance()
        self.now = T0
        #: `(duration, summary)` per timer, in the order the workflow issued them. A narration's
        #: whole footprint.
        self.timers: list[tuple[float, Optional[str]]] = []
        #: The run's memo, as `DescribeWorkflowExecution` would serve it — an ask's whole footprint.
        self.memo: dict[str, Any] = {}
        self.writes: list[dict[str, Any]] = []
        self.handlers: dict[str, Callable[..., None]] = {}
        self.waits: list[_Wait] = []

    # ── the temporalio.workflow surface the two modules use ──

    async def sleep(self, duration: float, *, summary: Optional[str] = None) -> None:
        self.timers.append((duration, summary))

    def upsert_memo(self, updates: dict[str, Any]) -> None:
        snapshot = {k: _clone(v) for k, v in updates.items()}
        self.writes.append(snapshot)
        self.memo.update(snapshot)

    def set_signal_handler(self, name: str, handler: Optional[Callable[..., None]]) -> None:
        if handler is None:
            self.handlers.pop(name, None)
        else:
            self.handlers[name] = handler

    async def wait_condition(
        self, fn: Callable[[], bool], *, timeout: Any = None, timeout_summary: str = ""
    ) -> None:
        deadline = None if timeout is None else self.now + _td(timeout)
        rec = _Wait(fn, deadline)
        self.waits.append(rec)
        while True:
            if fn():
                return
            if rec.deadline is not None and self.now >= rec.deadline:
                raise asyncio.TimeoutError()
            rec.wake.clear()
            await rec.wake.wait()

    # ── what a test does to it ──

    def answer(self, ask_id: str, value: Any, by: str = "") -> None:
        handler = self.handlers.get(f"{hitl.ANSWER_SIGNAL_PREFIX}{ask_id}")
        if handler is not None:
            handler({"value": value, "by": by})
        for rec in self.waits:
            rec.wake.set()

    # ── what a reader of this run would see ──

    @property
    def said(self) -> list[str]:
        """The sentences that reached history, in order."""
        return [s for _, s in self.timers if s is not None]

    def ask_entry(self, ask_id: str) -> dict[str, Any]:
        return self.memo[f"{hitl.ASK_MEMO_PREFIX}{ask_id}"]

    @property
    def history(self) -> str:
        """EVERYTHING this run wrote about itself, as one string.

        Both carriers together — the Summaries and every memo write, not just the final memo state,
        because a value that was written and then rewritten was still in history. A credential is
        searched for here rather than in one half, since the rule is about the pair.
        """
        return json.dumps({"summaries": self.said, "memo": self.writes}, default=str)


def _td(timeout: Any) -> timedelta:
    return timeout if isinstance(timeout, timedelta) else timedelta(seconds=float(timeout))


def _clone(value: Any) -> Any:
    if isinstance(value, dict):
        return {k: _clone(v) for k, v in value.items()}
    if isinstance(value, list):
        return [_clone(v) for v in value]
    return value


@pytest.fixture
def wf(monkeypatch: pytest.MonkeyPatch) -> Fake:
    fake = Fake()
    monkeypatch.setattr(temporal_workflow, "instance", lambda: fake.instance)
    monkeypatch.setattr(temporal_workflow, "now", lambda: fake.now)
    monkeypatch.setattr(temporal_workflow, "sleep", fake.sleep)
    monkeypatch.setattr(temporal_workflow, "upsert_memo", fake.upsert_memo)
    monkeypatch.setattr(temporal_workflow, "set_signal_handler", fake.set_signal_handler)
    monkeypatch.setattr(temporal_workflow, "wait_condition", fake.wait_condition)
    # The real adapter refuses to log outside a workflow event loop, which every line of this file
    # is. A plain logger keeps the SDK's own log lines exercised rather than skipped.
    monkeypatch.setattr(temporal_workflow, "logger", logging.getLogger("test.verbs"))

    def no_activities(*args: Any, **kwargs: Any) -> Any:
        raise AssertionError(
            "a verb scheduled an activity; neither a sentence nor a question is work, and either "
            "would then be able to sit queued, retry and time out"
        )

    for name in ("execute_activity", "start_activity", "execute_local_activity"):
        monkeypatch.setattr(temporal_workflow, name, no_activities)
    return fake


def run(coro: Any) -> Any:
    """No pytest-asyncio in this repo (`test_redis_kv.py` records why); every scenario is its own
    loop, exactly as both suites next door run one."""
    return asyncio.run(coro)


async def parked(fake: Fake, coro: Any) -> asyncio.Task:
    """Start an ask and return once it has parked — keyed on the memo write, which is the moment
    the question became answerable."""
    before = len(fake.writes)
    task = asyncio.ensure_future(coro)
    for _ in range(200):
        if len(fake.writes) > before and fake.waits:
            return task
        await asyncio.sleep(0)
    task.cancel()
    raise AssertionError("the ask never parked")


# ---------------------------------------------------------------------------------------------
# The pair, at the top level
# ---------------------------------------------------------------------------------------------


def test_the_two_verbs_are_importable_side_by_side_from_the_top_level() -> None:
    """`from actorkit import ask, speak` — the whole point of the slice, in one line.

    Both are the SAME OBJECTS the older names reach, not wrappers around them. A wrapper would be a
    second place for the budget, the redaction and the docstring to drift."""
    assert ask is hitl.ask
    assert speak is narrate.speak
    assert {"ask", "speak"} <= set(actorkit.__all__)
    # `dir()` too, because a lazy attribute is invisible to it by default and a surface an author
    # cannot find from the REPL is a surface they will not use.
    assert {"ask", "speak"} <= set(dir(actorkit))


def test_an_unknown_name_is_still_an_attribute_error() -> None:
    """The lazy accessor answers for exactly two names and nothing else — an `AttributeError` that
    became an `ImportError`, or a missing module import, would be a worse failure than the one
    Python already gives for a typo."""
    with pytest.raises(AttributeError, match="speaks"):
        actorkit.speaks  # noqa: B018


def test_reaching_the_pair_does_not_put_temporal_behind_import_actorkit() -> None:
    """THE INVARIANT THE LAZINESS EXISTS FOR, and the reason `narrate`/`hitl` are not imported in
    `actorkit/__init__.py` in the first place: both import temporalio at module scope, `actorkit`
    is what the workflow sandbox re-imports per instance, and every non-workflow caller of this
    package is entitled to import it without a Temporal dependency.

    A SUBPROCESS, because `sys.modules` in this session already holds temporalio — imported by this
    very file — so the question is unanswerable in-process. It inherits THIS session's sys.path
    rather than relying on the cwd: since `actorkit/` stopped being a repo-root directory, running
    from the repo root no longer puts the package on the path by itself.
    """
    probe = (
        "import sys, actorkit;"
        "print('bare', 'temporalio' in sys.modules);"
        "from actorkit import ask, speak;"
        "print('verbs', 'temporalio' in sys.modules)"
    )
    env = {**os.environ, "PYTHONPATH": os.pathsep.join(p for p in sys.path if p)}
    out = subprocess.run(
        [sys.executable, "-c", probe], cwd=ROOT, env=env,
        capture_output=True, text=True, check=True,
    ).stdout.split()
    assert out[:2] == ["bare", "False"], f"import actorkit dragged in temporalio: {out}"
    # …and naming a verb DOES pay for it, which is the other half of the claim: the cost is paid by
    # the author who reaches for one, inside a workflow, where temporalio is present by definition.
    assert out[2:] == ["verbs", "True"], out


def test_the_shipped_package_file_carries_the_lazy_accessor() -> None:
    """`sdk/python/actorkit/__init__.py` is what a wheel contains AND what a checkout imports —
    there is one file now, where there used to be that file plus a repo-root dev shim whose
    `__all__` a test in test_loader.py compared against it.

    This still reads the file from disk rather than the imported module, and that is the point: it
    is the half `__all__` cannot see. `speak`/`ask` are resolved by a module `__getattr__`, so an
    exports list can agree while the accessor is missing, and `from actorkit import speak` would
    then fail on exactly the surface an author uses."""
    ns: dict = {}
    exec(compile(PKG_INIT.read_text(), "pkg-init", "exec"), ns)
    assert ns["__getattr__"]("speak") is narrate.speak
    assert ns["__getattr__"]("ask") is hitl.ask
    assert {"ask", "speak"} <= set(ns["__all__"])


# ---------------------------------------------------------------------------------------------
# The distinction: one reports, one waits for a person
# ---------------------------------------------------------------------------------------------


def test_speak_reports_and_returns_immediately(wf: Fake) -> None:
    """The whole surface, in one call: prose in, one line of metadata out, and the run carries on.
    Nothing parked, nothing to answer, nobody consulted."""
    run(speak("119 apexes in scope, 37k subs expected"))
    assert wf.said == ["119 apexes in scope, 37k subs expected"]
    assert wf.writes == [], "a narration wrote a memo; nothing about it is answerable"
    assert wf.waits == [], "a narration waited on something; it is not supposed to be able to"


def test_ask_stops_the_run_until_a_human_moves_it(wf: Fake) -> None:
    """The other half of the distinction, asserted rather than only documented: the coroutine does
    NOT complete when the question is published. It completes when somebody answers."""

    async def scenario() -> Any:
        task = await parked(
            wf, ask("Approve these 12 hosts?", takes=Approval, context={"hosts": 12})
        )
        assert not task.done(), "an ask that returned without an answer is a progress line"
        assert wf.ask_entry("ask-1")["state"] == "pending"
        assert wf.said == [], "an ask narrated; the two verbs must not pay each other's cost"
        wf.answer("ask-1", {"approve": True, "note": "checked the sample"}, by="mo")
        return await task

    answer = run(scenario())
    assert getattr(answer, "approve", None) is True
    assert wf.ask_entry("ask-1")["state"] == "answered"


def test_one_run_can_do_both_and_the_transcript_reads_as_both(wf: Fake) -> None:
    """The idiom the pair exists for: say where you are, then stop for a person, then say what
    happened. Two carriers, one run, in the order they were written."""

    async def scenario() -> None:
        await speak("batch 1 of 3 — 12 hosts resolved")
        task = await parked(wf, ask("Approve these 12 hosts?", takes=Approval))
        wf.answer("ask-1", {"approve": True}, by="mo")
        await task
        await speak("approved by mo; dispatching the sweep")

    run(scenario())
    assert wf.said == ["batch 1 of 3 — 12 hosts resolved", "approved by mo; dispatching the sweep"]
    assert wf.ask_entry("ask-1")["by"] == "mo"


# ---------------------------------------------------------------------------------------------
# Being easier to reach must not make it easier to misuse
# ---------------------------------------------------------------------------------------------


def test_speak_keeps_narrations_refusal_rather_than_truncating(wf: Fake) -> None:
    """A sentence cut in the middle may mean something its author did not write, and a bound
    nobody is told about is not a bound. The new name inherits the old one's refusal exactly."""
    with pytest.raises(NarrationRefused) as excinfo:
        run(speak("x" * (MAX_SENTENCE_BYTES + 1)))
    assert str(MAX_SENTENCE_BYTES) in str(excinfo.value)
    assert wf.said == [], "the refused sentence reached history anyway"


def test_a_sentence_per_unit_spends_the_same_budget_however_it_is_spelled(wf: Fake) -> None:
    """THE SHAPE ERROR, which is the one `speak` could plausibly have made worse by being easier to
    reach. It does not: the budget is per RUN and shared with `say`, because they are one function.
    The run survives, the narration stops, and the transcript says so once."""

    async def scenario() -> None:
        for i in range(MAX_SENTENCES + 5):
            await speak(f"unit {i} dispatched")
        # …and the older name is silent too, which is what "one budget" means. Two implementations
        # would give a talkative run two ceilings and twice the events.
        await narrate.say("and one more, from the other name")

    run(scenario())
    assert len(wf.said) == MAX_SENTENCES + 1
    assert "Narrate a phase, not a Unit." in wf.said[-1]


def test_speaks_documented_example_is_the_progress_idiom(wf: Fake) -> None:
    """What an author copies is the docstring's example, so the docstring's example is the shape
    that is safe at scale — a line per phase, with the per-Unit mistake named beside it."""
    doc = speak.__doc__ or ""
    assert 'await speak(f"batch {i} of {n}")' in doc
    assert "PHASE" in doc and "Unit" in doc


def test_the_distinction_is_stated_where_an_author_reads_it() -> None:
    """It is the whole reason there are two verbs, so it is in the code and not only in a spec.
    Each half names the other: an author who found one has met the pair."""
    spoken = speak.__doc__ or ""
    asked = ask.__doc__ or ""
    assert "RETURNS IMMEDIATELY" in spoken and "ask" in spoken
    assert "STOPS THE RUN" in asked and "speak" in asked
    # The package header carries the pair as a pair, which is where an author meets it first.
    assert "from actorkit import ask, speak" in PKG_INIT.read_text()


def test_speak_is_the_same_function_and_not_a_second_implementation() -> None:
    """One body, reached by two names. A copy would be a second budget, a second redaction and
    eventually a second shape of turn for the same sentence."""
    assert "await say(sentence)" in inspect.getsource(narrate.speak)
    assert inspect.iscoroutinefunction(speak)


# ---------------------------------------------------------------------------------------------
# What neither may carry
# ---------------------------------------------------------------------------------------------


def test_neither_verb_carries_a_credential_into_the_runs_own_history(wf: Fake) -> None:
    """BOTH REACH HISTORY IN THE CLEAR. The codec is a claim-check, not encryption (ADR 0007): a
    small value rides inline as plain JSON and anyone who can read the run can read it. So the rule
    is one rule over the pair, and this asserts it over the pair — one run that speaks and asks,
    and a sentinel findable in neither carrier.

    WHAT THIS DOES NOT PROVE, because neither guard is a boundary and neither is sold as one: a
    credential in a sentence that names nothing, or in a prompt, still travels. The guards catch
    the ORDINARY mistake — an f-string or a config dict that happened to be in scope — which is the
    one that actually happens.
    """

    async def scenario() -> None:
        await speak(f"authenticating with api_key={SENTINEL}")
        task = await parked(
            wf,
            ask(
                "Approve these 12 hosts?",
                takes=Approval,
                context={"hosts": 12, "api_key": SENTINEL, "nested": {"session_token": SENTINEL}},
            ),
        )
        wf.answer("ask-1", {"approve": True}, by="mo")
        await task

    run(scenario())

    assert SENTINEL not in wf.history, "a credential reached the run's own history"

    # AND IT SAYS SO rather than going quiet: a sentence that lost a word without explanation is a
    # worse account than one that names what would not be carried.
    assert wf.said[0] == f"authenticating with api_key={narrate.REDACTED}"
    context = wf.ask_entry("ask-1")["context"]
    assert context["api_key"] == hitl.REDACTED
    assert context["nested"]["session_token"] == hitl.REDACTED
    # …and the decision aid survives, which is the reason redaction replaces rather than deletes:
    # a thinner context an operator cannot see the shape of is a worse aid than a censored one.
    assert context["hosts"] == 12
