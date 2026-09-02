"""Parking a workflow on a question, and what an operator can and cannot learn from it.

THE WORKFLOW SEAM IS FAKED, not a cluster. What is under test is the DECISION shape — what an ask
puts in the memo (which is what every reader outside the run sees), which signal answers it, what a
deadline does when it passes, and what never reaches history — and every one of those is a property
of `actorkit.hitl` rather than of Temporal. The fake below is the same monkeypatch-the-module seam
`test_workflows_client.py` uses for the caller SDK, extended with a virtual clock so a 24-hour
deadline expires in a test without anybody waiting for it.

WHAT A CLUSTER WOULD ADD, so nobody mistakes green here for proven: that `upsert_memo` really lands
on `DescribeWorkflowExecution`, and that a signal to a handler registered after it was sent is
really buffered. Both are Temporal's own guarantees, both are asserted in its own suite, and neither
is a thing this module can get wrong on its own.
"""

from __future__ import annotations

import asyncio
import logging
import re
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Callable, Optional

import pytest
from temporalio import workflow as temporal_workflow
from temporalio.exceptions import ApplicationError

from actorkit import hitl

ROOT = Path(__file__).resolve().parent.parent

#: A value that must be findable — or not findable — in whatever the run publishes about itself.
SENTINEL = "kontra-sentinel-9f2a1c-do-not-leak"

T0 = datetime(2026, 8, 25, 12, 0, 0, tzinfo=timezone.utc)


@dataclass
class Approval:
    approve: bool
    note: str = ""


# ---------------------------------------------------------------------------------------------
# The seam: one workflow instance, one memo, one clock, and the signals somebody sent it.
# ---------------------------------------------------------------------------------------------


class _Instance:
    """The author's `@workflow.defn` object — the only thing `hitl` hangs per-run state on."""


class _Wait:
    def __init__(self, fn: Callable[[], bool], deadline: Optional[datetime], summary: str) -> None:
        self.fn = fn
        self.deadline = deadline
        self.summary = summary
        self.wake = asyncio.Event()


class Fake:
    """The workflow the SDK thinks it is running inside.

    `wait_condition` is real asyncio over a VIRTUAL clock: it parks on an event, and the harness
    wakes it when a signal is delivered or when a test moves the clock. So an expiry is asserted at
    the instant the deadline passes rather than by sleeping through it, and a test never contains a
    real timeout that a slow machine turns into a flake.
    """

    def __init__(self, now: datetime = T0) -> None:
        self.instance = _Instance()
        self.now = now
        #: The run's memo, as Temporal would hold it — and as `DescribeWorkflowExecution` serves it.
        self.memo: dict[str, Any] = {}
        #: Every memo write in order. The write LOG matters as much as the final state: it is one
        #: history event each, and the audit question is whether question and answer are both in it.
        self.writes: list[dict[str, Any]] = []
        self.handlers: dict[str, Callable[..., None]] = {}
        self.waits: list[_Wait] = []
        #: Anything scheduled as an activity. Must stay EMPTY across an ask — see the Batch test.
        self.activities: list[Any] = []
        #: A CRASHED WORKER EMITS NO COMMANDS. Set while simulating one, so the cancellation used
        #: to unwind the coroutine does not write the memo entry a dying process never would.
        self.deaf = False

    # ── the temporalio.workflow surface `hitl` uses ──

    def upsert_memo(self, updates: dict[str, Any]) -> None:
        # Copied on the way in, because `hitl` mutates its own envelope to close an ask and a
        # shared reference would rewrite history retroactively — which is the one thing a history
        # cannot do, and would make this fake lie in the direction that hides bugs.
        if self.deaf:
            return
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
        rec = _Wait(fn, deadline, timeout_summary)
        self.waits.append(rec)
        while True:
            if fn():
                return
            if rec.deadline is not None and self.now >= rec.deadline:
                raise asyncio.TimeoutError()
            rec.wake.clear()
            await rec.wake.wait()

    # ── what a test does to it ──

    def signal(self, name: str, payload: Any) -> None:
        """Deliver one signal. Unknown names are DROPPED, exactly as an unregistered handler on a
        real workflow buffers rather than dispatches — a test that answers an ask nobody asked
        must not appear to succeed."""
        handler = self.handlers.get(name)
        if handler is not None:
            handler(payload)
        self._wake()

    def answer(self, ask_id: str, value: Any, by: str = "") -> None:
        self.signal(f"{hitl.ANSWER_SIGNAL_PREFIX}{ask_id}", {"value": value, "by": by})

    def advance(self, delta: timedelta) -> None:
        self.now += delta
        self._wake()

    def _wake(self) -> None:
        for rec in self.waits:
            rec.wake.set()

    @property
    def asks(self) -> dict[str, Any]:
        return {k: v for k, v in self.memo.items() if k.startswith(hitl.ASK_MEMO_PREFIX)}

    def ask_entry(self, ask_id: str) -> dict[str, Any]:
        return self.memo[f"{hitl.ASK_MEMO_PREFIX}{ask_id}"]


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
    monkeypatch.setattr(temporal_workflow, "upsert_memo", fake.upsert_memo)
    monkeypatch.setattr(temporal_workflow, "set_signal_handler", fake.set_signal_handler)
    monkeypatch.setattr(temporal_workflow, "wait_condition", fake.wait_condition)
    # The real adapter refuses to log outside a workflow event loop, which every line of this file
    # is. A plain logger keeps the SDK's own log lines exercised rather than skipped.
    monkeypatch.setattr(temporal_workflow, "logger", logging.getLogger("test.hitl"))

    def no_activities(*args: Any, **kwargs: Any) -> Any:
        raise AssertionError("hitl scheduled an activity; an ask is not work and an answer is not a Batch")

    monkeypatch.setattr(temporal_workflow, "execute_activity", no_activities)
    monkeypatch.setattr(temporal_workflow, "start_activity", no_activities)
    return fake


async def _parked(fake: Fake, coro: Any) -> asyncio.Task:
    """Start an ask and return once THIS ask has parked.

    Keyed on the memo write count rather than on "is anything pending": a run that has already
    asked once would satisfy the weaker condition before the second ask had written anything, and
    the test would then assert against the first ask's entry.
    """
    before = len(fake.writes)
    task = asyncio.ensure_future(coro)
    for _ in range(200):
        if len(fake.writes) > before and fake.waits:
            return task
        await asyncio.sleep(0)
    task.cancel()
    raise AssertionError("the ask never parked")


def ms(dt: datetime) -> int:
    return int(dt.timestamp() * 1000)


# ---------------------------------------------------------------------------------------------
# Asking
# ---------------------------------------------------------------------------------------------


def test_an_ask_publishes_its_own_question_before_anybody_answers(wf: Fake) -> None:
    """The whole point of emitting rather than answering a query: what a reader needs is IN the
    run's own durable state the moment it parks, with no worker consulted."""

    async def scenario() -> None:
        task = await _parked(
            wf,
            hitl.ask(
                "Approve these 12 hosts?",
                takes=Approval,
                context={"dataset": "live", "n": 12},
                deadline=timedelta(hours=4),
            ),
        )
        entry = wf.ask_entry("ask-1")
        assert entry["prompt"] == "Approve these 12 hosts?"
        assert entry["state"] == "pending"
        assert entry["askedAt"] == ms(T0)
        assert entry["deadlineAt"] == ms(T0 + timedelta(hours=4))
        assert entry["context"] == {"dataset": "live", "n": 12}
        # The schema the form renders from and the answer is validated against — derived from the
        # declared type through the same `actorkit.schema` the actor catalog uses.
        assert entry["schema"]["properties"]["approve"]["type"] == "boolean"
        assert entry["schema"]["required"] == ["approve"]
        assert not task.done()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(scenario())


def test_the_ask_is_answered_by_a_signal_that_names_it(wf: Fake) -> None:
    """`kontra.answer/<id>`, not one signal keyed by a payload field. The id is in the NAME so the
    reduced log can pair an answer with its question from event metadata alone."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Go?", takes=Approval, deadline=None))
        assert set(wf.handlers) == {"kontra.answer/ask-1"}
        wf.answer("ask-1", {"approve": True, "note": "checked the sample"}, by="mo")
        answer = await task
        # Coerced to the declared type: the author gets their class, not a dict to re-parse.
        assert answer == Approval(approve=True, note="checked the sample")

    asyncio.run(scenario())


def test_an_answered_ask_records_who_said_so_and_stops_being_pending(wf: Fake) -> None:
    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Go?", deadline=None))
        wf.advance(timedelta(minutes=7))
        wf.answer("ask-1", True, by="mo@laptop")
        await task
        entry = wf.ask_entry("ask-1")
        assert entry["state"] == "answered"
        assert entry["by"] == "mo@laptop"
        assert entry["answeredAt"] == ms(T0 + timedelta(minutes=7))
        assert hitl.pending() == []

    asyncio.run(scenario())


def test_an_answer_with_no_operator_label_records_none_rather_than_inventing_one(wf: Fake) -> None:
    """SELF-ASSERTED MEANS OPTIONAL. There is no authenticated identity to fall back to, so an
    unlabelled answer is unlabelled — a default of 'operator' or 'localhost' would be kontra
    asserting something nobody said."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Go?", deadline=None))
        wf.answer("ask-1", True)
        await task
        assert "by" not in wf.ask_entry("ask-1")

    asyncio.run(scenario())


def test_a_second_answer_is_ignored_so_attribution_cannot_be_rewritten(wf: Fake) -> None:
    """The workflow may already have acted on the first answer. A late second one that silently
    replaced it would make the transcript's attribution false about a decision already taken."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Go?", deadline=None))
        wf.answer("ask-1", "first", by="alice")
        assert await task == "first"
        wf.answer("ask-1", "second", by="mallory")
        assert wf.ask_entry("ask-1")["by"] == "alice"

    asyncio.run(scenario())


# ---------------------------------------------------------------------------------------------
# Concurrency
# ---------------------------------------------------------------------------------------------


def test_two_branches_parking_at_once_produce_two_asks_answerable_independently(wf: Fake) -> None:
    """Parallel branches each needing judgement is normal here, which is why the read route is a
    LIST. Two asks must not share a memo key, a signal name or an answer."""

    async def scenario() -> None:
        left = asyncio.ensure_future(hitl.ask("Approve the DNS sweep?", deadline=None))
        right = asyncio.ensure_future(hitl.ask("Approve the HTTP sweep?", deadline=None))
        for _ in range(200):
            if len(wf.asks) == 2 and len(wf.waits) == 2:
                break
            await asyncio.sleep(0)
        assert set(wf.asks) == {"kontra.ask.ask-1", "kontra.ask.ask-2"}
        assert set(wf.handlers) == {"kontra.answer/ask-1", "kontra.answer/ask-2"}

        wf.answer("ask-2", "http yes", by="mo")
        assert await right == "http yes"
        # ANSWERING ONE LEAVES THE OTHER PENDING. This is the assertion a singular route could not
        # have been widened to later without breaking its own contract.
        assert not left.done()
        assert wf.ask_entry("ask-1")["state"] == "pending"
        assert wf.ask_entry("ask-2")["state"] == "answered"

        wf.answer("ask-1", "dns yes")
        assert await left == "dns yes"

    asyncio.run(scenario())


# ---------------------------------------------------------------------------------------------
# Deadlines
# ---------------------------------------------------------------------------------------------


def test_omitting_a_deadline_applies_the_default(wf: Fake) -> None:
    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Go?"))
        assert wf.ask_entry("ask-1")["deadlineAt"] == ms(T0 + hitl.DEFAULT_DEADLINE)
        assert wf.waits[0].deadline == T0 + hitl.DEFAULT_DEADLINE
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(scenario())


def test_passing_none_explicitly_waits_indefinitely(wf: Fake) -> None:
    """A durable workflow genuinely can wait forever, so the author is allowed to say so. The
    absence of a deadline is DECLARED — no `deadlineAt` on the ask — rather than expressed as a
    very large one, which a countdown would render as a number nobody meant."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Go?", deadline=None))
        assert "deadlineAt" not in wf.ask_entry("ask-1")
        assert wf.waits[0].deadline is None
        wf.advance(timedelta(days=365))
        await asyncio.sleep(0)
        assert not task.done()
        wf.answer("ask-1", True)
        assert await task is True

    asyncio.run(scenario())


def test_an_expired_deadline_raises_inside_the_workflow(wf: Fake) -> None:
    """kontra does not choose retry, escalate or fail on the author's behalf. It raises where they
    parked, and what a timeout MEANS is decided by the person who knew what was being asked."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Approve?", deadline=timedelta(hours=4)))
        wf.advance(timedelta(hours=4))
        with pytest.raises(hitl.AskExpired) as raised:
            await task
        assert raised.value.ask_id == "ask-1"
        assert "Approve?" in str(raised.value)
        # An ApplicationError, so an uncaught expiry FAILS THE RUN instead of failing the workflow
        # task forever — which would leave it reporting `running` with nothing moving.
        assert isinstance(raised.value, ApplicationError)
        assert raised.value.non_retryable is True

    asyncio.run(scenario())


def test_an_expired_ask_stops_being_offered_and_says_it_expired(wf: Fake) -> None:
    """A form that signals a workflow which has moved on is worse than no form."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Approve?", deadline=timedelta(hours=1)))
        wf.advance(timedelta(hours=1))
        with pytest.raises(hitl.AskExpired):
            await task
        entry = wf.ask_entry("ask-1")
        assert entry["state"] == "expired"
        assert entry["expiredAt"] == ms(T0 + timedelta(hours=1))

    asyncio.run(scenario())


def test_an_author_may_catch_an_expiry_and_carry_on(wf: Fake) -> None:
    """The point of raising rather than deciding: taking a safe default is one of the four correct
    answers, and it has to be expressible without kontra having picked it."""

    async def scenario() -> None:
        async def body() -> str:
            try:
                return await hitl.ask("Approve?", deadline=timedelta(minutes=30))
            except hitl.AskExpired:
                return "declined-by-default"

        task = await _parked(wf, body())
        wf.advance(timedelta(minutes=30))
        assert await task == "declined-by-default"

    asyncio.run(scenario())


def test_a_cancelled_run_records_its_ask_as_abandoned_not_expired(wf: Fake) -> None:
    """Three endings, three words. A run somebody stopped is not a deadline nobody met, and an
    operator finding a cancelled run should not read that a human failed to answer in time."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Approve?", deadline=timedelta(hours=4)))
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert wf.ask_entry("ask-1")["state"] == "abandoned"

    asyncio.run(scenario())


# ---------------------------------------------------------------------------------------------
# Surviving the worker
# ---------------------------------------------------------------------------------------------


def test_a_run_parked_across_a_worker_restart_re_parks_on_the_same_question(wf: Fake) -> None:
    """THE CASE THE QUERY IDIOM COULD NOT SERVE. The ask lives in the run's own durable state, not
    in the process that asked it — so the worker dying loses nothing, and the replay that follows
    re-derives byte-identical facts rather than a second, differently-timed ask.

    Replay is simulated the way Temporal produces it: the same deterministic clock, a FRESH
    workflow instance (a new process holds no Python objects from the old one), and the answer
    delivered afterwards.
    """

    async def scenario() -> None:
        first = await _parked(wf, hitl.ask("Approve these 12 hosts?", deadline=timedelta(hours=4)))
        before = _clone(wf.ask_entry("ask-1"))

        # The worker dies mid-park. A CRASH IS NOT A CANCELLATION: a dying process emits no
        # commands, so the unwind below writes nothing — which is the whole property under test,
        # that what a reader sees was already durable before the process was lost.
        wf.deaf = True
        first.cancel()
        with pytest.raises(asyncio.CancelledError):
            await first
        wf.deaf = False
        assert wf.ask_entry("ask-1")["state"] == "pending"

        # A new worker replays the run. Fresh instance, same clock, same call order.
        wf.instance = _Instance()
        second = await _parked(wf, hitl.ask("Approve these 12 hosts?", deadline=timedelta(hours=4)))
        after = wf.ask_entry("ask-1")
        assert after["id"] == before["id"] == "ask-1"
        assert after["askedAt"] == before["askedAt"]
        assert after["deadlineAt"] == before["deadlineAt"]
        assert after["state"] == "pending"

        wf.answer("ask-1", True, by="mo")
        assert await second is True

    asyncio.run(scenario())


# ---------------------------------------------------------------------------------------------
# What must never travel
# ---------------------------------------------------------------------------------------------


def test_a_secret_shaped_key_never_reaches_the_runs_own_history(wf: Fake) -> None:
    """The codec is a claim-check, not encryption: under 128 KiB a context rides inline, in the
    clear, to anyone who can read the run. So the sentinel must be absent from EVERY memo write —
    each of which is one history event — and not merely from the final state."""

    async def scenario() -> None:
        task = await _parked(
            wf,
            hitl.ask(
                "Approve?",
                context={"api_key": SENTINEL, "nested": {"password": SENTINEL}, "n": 12},
                deadline=None,
            ),
        )
        assert SENTINEL not in repr(wf.writes)
        assert SENTINEL not in repr(wf.memo)
        entry = wf.ask_entry("ask-1")
        # REPLACED, NOT DELETED, and the replacement says why: a key that vanished would read as a
        # fact the workflow did not have, which is a different and worse statement.
        assert entry["context"]["api_key"] == hitl.REDACTED
        assert entry["context"]["nested"]["password"] == hitl.REDACTED
        assert entry["context"]["n"] == 12
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(scenario())


def test_an_answers_value_is_never_written_back_into_the_runs_history(wf: Fake) -> None:
    """The answer arrived as a signal and is already in history where the operator put it. Copying
    it into the memo would make a second, longer-lived copy of a human's judgement in a place
    nobody chose — and would grow the run's memo by every answer it ever took."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Paste the approval note", deadline=None))
        wf.answer("ask-1", {"note": SENTINEL}, by="mo")
        assert await task == {"note": SENTINEL}
        assert SENTINEL not in repr(wf.memo)
        assert SENTINEL not in repr(wf.writes)
        # What the settled ask DOES keep: the question and who answered it. An archive holding an
        # answer without its question records somebody approving something unspecified.
        entry = wf.ask_entry("ask-1")
        assert entry["prompt"] == "Paste the approval note"
        assert entry["by"] == "mo"

    asyncio.run(scenario())


def test_an_answer_is_a_plain_value_and_never_becomes_a_batch(wf: Fake) -> None:
    """A Batch is content-addressed into the blob plane and materialized into the lake. An answer
    that became one would be a permanent, queryable copy of a human's judgement. The `wf` fixture
    fails any activity schedule, which is how an accidental publish would show up here."""
    from actorkit.batch import Batch

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Go?", deadline=None))
        wf.answer("ask-1", [{"host": "a"}, {"host": "b"}])
        answer = await task
        assert not isinstance(answer, Batch)
        assert answer == [{"host": "a"}, {"host": "b"}]
        assert wf.activities == []

    asyncio.run(scenario())


def test_an_oversized_context_leaves_an_answerable_question_rather_than_killing_the_run(
    wf: Fake,
) -> None:
    """Temporal's memo has a hard ceiling and blowing it fails the workflow AT PARK TIME. A note in
    place of the material is a legible ask with a visibly missing aid; the alternative is a run
    that died because somebody attached one page too many of sample rows."""

    async def scenario() -> None:
        task = await _parked(
            wf,
            hitl.ask("Approve?", context={"rows": ["x" * 64] * 2000}, deadline=None),
        )
        entry = wf.ask_entry("ask-1")
        assert "rows" not in entry["context"]
        assert str(hitl.MAX_CONTEXT_BYTES) in entry["context"]["kontra"]
        assert entry["state"] == "pending"
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(scenario())


# ---------------------------------------------------------------------------------------------
# The cross-language shape
# ---------------------------------------------------------------------------------------------


def test_the_envelope_is_the_transcripts_ask_field_for_field(wf: Fake) -> None:
    """ONE SHAPE, WRITTEN TWICE — the house decoupling rule. `control/orchestrator/src/transcript.ts`
    declares what a reader of an ask expects; this module emits it. Nothing translates between
    them, so a rename on either side has to be a test failure here rather than a field that
    silently arrives as `undefined` in a rendered form.

    `state` is the one field this side has and that one does not, deliberately: the transcript
    derives pending/expired from `answeredAt` and `deadlineAt` against a clock it was handed, while
    the RUN knows which of the four endings actually happened. Extra is safe; missing is not.
    """
    declared = (ROOT / "shared" / "core" / "src" / "transcript.ts").read_text()
    block = declared.split("export interface Ask {", 1)[1].split("\n}", 1)[0]
    ts_fields = set(re.findall(r"^\s{2}(\w+)\??:", block, re.MULTILINE))
    assert ts_fields == {"id", "prompt", "askedAt", "schema", "context", "deadlineAt", "answeredAt", "by"}

    async def scenario() -> None:
        task = await _parked(
            wf,
            hitl.ask("Go?", takes=Approval, context={"n": 1}, deadline=timedelta(hours=2)),
        )
        pending_fields = set(wf.ask_entry("ask-1"))
        wf.answer("ask-1", {"approve": True}, by="mo")
        await task
        answered_fields = set(wf.ask_entry("ask-1"))

        emitted = (pending_fields | answered_fields) - {"state"}
        assert emitted <= ts_fields, f"emitted fields the transcript cannot read: {emitted - ts_fields}"
        # Everything the transcript declares NON-optional must be on every ask, in both states.
        required = set(re.findall(r"^\s{2}(\w+):", block, re.MULTILINE))
        assert required <= pending_fields and required <= answered_fields

    asyncio.run(scenario())


def test_two_live_asks_cannot_share_one_id(wf: Fake) -> None:
    """They would share a memo key and a signal name, so answering either would answer whichever
    the handler table happened to hold — a wrong answer delivered to a human's decision. Re-using
    an id after the first settled is supported; only the overlap is refused."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("Go?", id="approve", deadline=None))
        with pytest.raises(ValueError, match="already pending"):
            await hitl.ask("Go again?", id="approve", deadline=None)
        wf.answer("approve", True)
        assert await task is True
        # Settled, so the id is free again — the retried-leg case the argument exists for.
        again = await _parked(wf, hitl.ask("Go once more?", id="approve", deadline=None))
        assert wf.ask_entry("approve")["prompt"] == "Go once more?"
        again.cancel()
        with pytest.raises(asyncio.CancelledError):
            await again

    asyncio.run(scenario())


def test_a_prompt_too_long_to_carry_says_it_was_cut(wf: Fake) -> None:
    """A question silently shortened is a question whose meaning may have changed under the person
    answering it. The cut is marked so they can see there was more."""

    async def scenario() -> None:
        task = await _parked(wf, hitl.ask("A" * (hitl.MAX_PROMPT_CHARS + 50), deadline=None))
        assert wf.ask_entry("ask-1")["prompt"].endswith(hitl.ELIDED)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(scenario())
