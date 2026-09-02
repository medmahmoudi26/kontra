"""An author's own sentence, and everything this SDK will not carry for them.

THE WORKFLOW SEAM IS FAKED, not a cluster — the same monkeypatch-the-module seam
`test_hitl_ask.py` uses, and for the same reason. What is under test is the DECISION shape: which
command a narration issues, what reaches history, what is refused and how, and what a run that
narrates nothing is charged for. Every one of those is a property of `actorkit.narrate` rather
than of Temporal.

WHAT A CLUSTER WOULD ADD, so nobody mistakes green here for proven: that a zero-duration timer
really is accepted and really fires, and that its user metadata really lands on `TimerStarted`.
Both are Temporal's own guarantees. `test_narration_live.py` next door asserts them anyway,
because "the SDK writes a Summary nothing can read" is exactly the failure this file cannot see.

The reader's half is `backend/src/transcript.test.ts`, which pins the same shape from the
other side. There is no shared code between them, by design, so both sides must be pinned.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import re
from pathlib import Path
from typing import Any, Optional

import pytest
from temporalio import workflow as temporal_workflow

from actorkit import narrate
from actorkit.catalog import SUMMARY_BUDGET
from actorkit.narrate import MAX_SENTENCE_BYTES, MAX_SENTENCES, NarrationRefused, say

ROOT = Path(__file__).resolve().parent.parent

#: A value that must never be findable in anything the run writes about itself.
SENTINEL = "kontra-sentinel-9f2a1c-do-not-leak"


def utf8(text: str) -> int:
    """The budget's unit. `len()` counts characters, and the two differ on exactly the prose an
    operator is most likely to write."""
    return len(text.encode("utf-8"))


# ---------------------------------------------------------------------------------------------
# The seam: one workflow instance, and every command it issued
# ---------------------------------------------------------------------------------------------


class _Instance:
    """The author's `@workflow.defn` object — the only thing `narrate` hangs per-run state on."""


class Fake:
    """The workflow the SDK thinks it is running inside.

    It records COMMANDS, not effects. A narration's whole footprint is the commands it issues, so
    a fake that only reported "the sentence was carried" would miss the thing that matters — how
    many events one sentence costs a history, and whether anything else was scheduled beside it.
    """

    def __init__(self) -> None:
        self.instance = _Instance()
        #: `(duration, summary)` per timer, in the order the workflow issued them.
        self.timers: list[tuple[float, Optional[str]]] = []

    async def sleep(self, duration: float, *, summary: Optional[str] = None) -> None:
        self.timers.append((duration, summary))

    @property
    def said(self) -> list[str]:
        """The sentences that reached history, in order."""
        return [s for _, s in self.timers if s is not None]


@pytest.fixture
def wf(monkeypatch: pytest.MonkeyPatch) -> Fake:
    fake = Fake()
    monkeypatch.setattr(temporal_workflow, "instance", lambda: fake.instance)
    monkeypatch.setattr(temporal_workflow, "sleep", fake.sleep)
    # The real adapter refuses to log outside a workflow event loop, which every line of this file
    # is. A plain logger keeps the SDK's own log lines exercised rather than skipped.
    monkeypatch.setattr(temporal_workflow, "logger", logging.getLogger("test.narrate"))

    def nothing_else(*args: Any, **kwargs: Any) -> Any:
        raise AssertionError(
            "narrate issued a command other than a timer; a sentence needs no worker, no queue "
            "and no payload, and any of those would give it a way to hang"
        )

    for name in ("execute_activity", "start_activity", "execute_local_activity", "upsert_memo"):
        monkeypatch.setattr(temporal_workflow, name, nothing_else)
    return fake


def run(coro: Any) -> Any:
    """No pytest-asyncio in this repo (`test_redis_kv.py` records why); every scenario is its own
    loop, exactly as `test_hitl_ask.py` runs one."""
    return asyncio.run(coro)


# ---------------------------------------------------------------------------------------------
# What a sentence is, and what it costs
# ---------------------------------------------------------------------------------------------


def test_a_sentence_becomes_a_turn_at_the_point_it_was_written(wf: Fake) -> None:
    """The whole surface, in one call: prose in, one line of metadata out, nothing else."""
    run(say("119 apexes in scope, 37k subs expected"))
    assert wf.said == ["119 apexes in scope, 37k subs expected"]


def test_the_command_is_a_zero_duration_timer_and_nothing_else(wf: Fake) -> None:
    """THE MECHANISM, asserted rather than described. A timer is the only command a workflow can
    issue that carries user metadata and runs nothing — an activity would carry a Summary too, and
    would need a function registered on the caller's own worker and a queue somebody polls, which
    would give a sentence a way to sit queued, retry and time out.

    Zero duration because the wait is not the point; the event is. The fixture fails the test if
    anything else was scheduled.
    """
    run(say("scope resolved, dispatching the crawler"))
    assert wf.timers == [(0, "scope resolved, dispatching the crawler")]


def test_sentences_arrive_in_the_order_they_were_written(wf: Fake) -> None:
    """A transcript is an account of a run, and an account whose sentences are out of order is a
    different account. Ordering is the timer's, which is the workflow's own command order."""

    async def scenario() -> None:
        await say("seeding from the paid-programs scope")
        await say("119 apexes resolved")
        await say("dispatching the crawler across 10 machines")

    run(scenario())
    assert wf.said == [
        "seeding from the paid-programs scope",
        "119 apexes resolved",
        "dispatching the crawler across 10 machines",
    ]


def test_a_workflow_that_narrates_nothing_is_unchanged(wf: Fake) -> None:
    """THE ACCEPTANCE CRITERION THAT IS ABOUT ABSENCE. Nothing in this module runs unless `say` is
    called: no state on the instance, no command, no event. A narration surface that charged every
    workflow a marker to be available would have made every existing run longer to buy nothing."""
    assert wf.timers == []
    assert not hasattr(wf.instance, narrate._STATE_ATTR)


def test_the_sentence_is_normalised_to_one_line(wf: Fake) -> None:
    """A Summary is single-line by Temporal's own definition — it is a bar label. A sentence built
    across a triple-quoted string would otherwise break the surface it was written for.

    NORMALISATION, NOT TRUNCATION: every word survives, in order.
    """
    run(say("  119 apexes in scope,\n  37k subs\texpected  "))
    assert wf.said == ["119 apexes in scope, 37k subs expected"]


# ---------------------------------------------------------------------------------------------
# The bound is enforced, never silently applied
# ---------------------------------------------------------------------------------------------


def test_the_bound_is_the_same_number_as_every_other_summary() -> None:
    """ONE BUDGET, NOT TWO. A dispatch's Method line, a Session's open and an author's sentence are
    the same kind of thing in the same place, and 200 bytes is already the number with the
    reasoning attached — it renders on a Temporal bar label without eliding, and it is some 650×
    under the codec's 128 KiB offload threshold, which is what keeps a Summary readable without a
    fetch."""
    assert MAX_SENTENCE_BYTES == SUMMARY_BUDGET == 200


def test_a_sentence_over_the_bound_is_refused_rather_than_truncated(wf: Fake) -> None:
    """THE DIFFERENCE THIS FILE EXISTS FOR. Half a sentence may mean something its author did not
    write — `do not delete the production dataset` cut at 200 bytes is a different instruction —
    so the bound raises. A bound nobody is told about is not a bound; it is a silent edit of
    somebody's own words.
    """
    with pytest.raises(NarrationRefused) as caught:
        run(say("x" * (MAX_SENTENCE_BYTES + 1)))
    # The message names the bound and the overrun, because the author's fix depends on both.
    assert str(MAX_SENTENCE_BYTES) in str(caught.value)
    assert str(MAX_SENTENCE_BYTES + 1) in str(caught.value)
    # And NOTHING was written. A refusal that had already emitted the line would be the worst of
    # both: a truncated sentence in history and an exception in the workflow.
    assert wf.timers == []


def test_a_sentence_exactly_at_the_bound_is_carried(wf: Fake) -> None:
    """The boundary is inclusive, and it is asserted rather than assumed — an off-by-one here
    refuses a legal sentence, which is the same class of silent surprise in the other direction."""
    run(say("x" * MAX_SENTENCE_BYTES))
    assert utf8(wf.said[0]) == MAX_SENTENCE_BYTES


def test_the_bound_is_in_bytes_and_not_in_characters(wf: Fake) -> None:
    """The wire counts bytes and so does the offload threshold this bound protects. A rule counted
    in characters would let a sentence of CJK or of em dashes through at nearly three times the
    size it claimed — on exactly the prose least likely to be checked by an English-speaking
    reviewer."""
    sentence = "日" * ((MAX_SENTENCE_BYTES // 3) + 1)
    assert len(sentence) < MAX_SENTENCE_BYTES < utf8(sentence)
    with pytest.raises(NarrationRefused):
        run(say(sentence))
    assert wf.timers == []


def test_the_refusal_fails_the_run_rather_than_its_workflow_task(wf: Fake) -> None:
    """A plain exception raised inside a workflow fails the WORKFLOW TASK in Python's SDK and
    retries it forever, so a run refused here would sit at `running` with nothing moving — the
    invisible failure this whole surface exists to remove, produced by the surface itself."""
    from temporalio.exceptions import ApplicationError

    with pytest.raises(ApplicationError) as caught:
        run(say("x" * 1000))
    assert caught.value.non_retryable is True
    assert caught.value.type == "NarrationRefused"


# ---------------------------------------------------------------------------------------------
# It is prose, and it is not a payload
# ---------------------------------------------------------------------------------------------


def test_say_takes_a_sentence_and_offers_nowhere_to_put_a_payload() -> None:
    """THE DESIGN, NOT AN OMISSION. Values belong in a Dataset, where they are queryable,
    deduplicated and out of history. A `context=` beside the sentence — the shape `hitl.ask`
    legitimately has, because an operator must be able to JUDGE what they are approving — would be
    a per-turn payload in a surface whose whole constraint is that history is not free."""
    params = list(inspect.signature(say).parameters)
    assert params == ["sentence"]


def test_a_value_passed_where_a_sentence_belongs_is_refused(wf: Fake) -> None:
    """Coercing with `str()` would be worse than either alternative: it is the back door through
    which a dict of results becomes a payload in history, one turn at a time. And a `TypeError`
    would be invisible — it is not a `FailureError`, so it fails the workflow task and retries."""
    for value in ({"rows": 12}, [1, 2, 3], 12, None):
        with pytest.raises(NarrationRefused):
            run(say(value))  # type: ignore[arg-type]
    assert wf.timers == []


def test_an_empty_sentence_writes_nothing_rather_than_a_blank_turn(wf: Fake) -> None:
    """`summaryOf` in backend/src/history.ts reads an empty Summary as NO Summary, so an
    empty sentence would land as a bare timer with no text on it — a row in the transcript where a
    sentence was meant to be, which reads as a bug in the reader rather than as an empty f-string
    in the workflow. Nothing is strictly better."""
    run(say(""))
    run(say("   \n\t "))
    assert wf.timers == []


# ---------------------------------------------------------------------------------------------
# A sentence never carries a credential
# ---------------------------------------------------------------------------------------------


def test_a_credential_written_as_an_assignment_does_not_reach_history(wf: Fake) -> None:
    """The ordinary mistake: an f-string that interpolated something that happened to be in scope.
    A narration reaches history in the clear — the codec is a claim-check, not encryption (ADR
    0007), and a Summary is far too small to be offloaded — so anything in one is readable by
    anybody who can read the run."""
    for sentence in (
        f"authenticating with api_key={SENTINEL}",
        f"authenticating with api_key: {SENTINEL}",
        f"retrying with token = {SENTINEL}",
        f"header Authorization: Bearer {SENTINEL}",
        f"the client sent password={SENTINEL} and gave up",
    ):
        run(say(sentence))
    assert not any(SENTINEL in line for line in wf.said), wf.said
    # REPLACED, NOT DROPPED: "there was a token here and kontra would not carry it" is a more
    # useful line than a sentence with a hole in it.
    assert all(narrate.REDACTED in line for line in wf.said), wf.said
    assert wf.said[0] == f"authenticating with api_key={narrate.REDACTED}"


def test_a_bare_bearer_token_is_redacted_even_when_nothing_names_it(wf: Fake) -> None:
    """The way a token is usually pasted — the scheme word and the credential, with the header name
    long gone."""
    run(say(f"Bearer {SENTINEL} was rejected"))
    assert SENTINEL not in wf.said[0]
    assert wf.said[0] == f"Bearer {narrate.REDACTED} was rejected"


def test_prose_that_merely_mentions_a_secret_word_survives(wf: Fake) -> None:
    """A matcher keyed on the word alone would redact `bucket`, and a guard that mangles ordinary
    sentences is one authors route around. Only the ASSIGNMENT shape is matched — which is the
    shape a leaked credential actually takes in a formatted string."""
    for sentence in (
        "token bucket refilled to 40",
        "3 hosts rejected the credential and were dropped",
        "bearer of bad news: 41 of 119 apexes do not resolve",
    ):
        run(say(sentence))
    assert wf.said == [
        "token bucket refilled to 40",
        "3 hosts rejected the credential and were dropped",
        "bearer of bad news: 41 of 119 apexes do not resolve",
    ]


def test_redaction_never_raises_and_never_costs_the_author_the_bound(wf: Fake) -> None:
    """A run that died because its narration was impolite would be an outage created by a safety
    rule. And the bound is checked on what would be WRITTEN, so a sentence is never refused for the
    size of a credential that was never going to be carried anyway."""
    run(say("dispatching with token=" + "z" * 400))
    assert wf.said[0].endswith(narrate.REDACTED)
    assert utf8(wf.said[0]) <= MAX_SENTENCE_BYTES


def test_the_guard_is_not_described_as_a_boundary() -> None:
    """A credential in a sentence that does not name it still travels, and the module says so. A
    guard sold as a boundary is how the rule stops being taught."""
    doc = narrate.redact.__doc__ or ""
    assert "NOT A SECURITY BOUNDARY" in doc


# ---------------------------------------------------------------------------------------------
# A sentence per phase; never a sentence per Unit
# ---------------------------------------------------------------------------------------------


def test_a_sentence_per_unit_is_refused_and_the_transcript_says_so(wf: Fake) -> None:
    """HISTORY EVENTS ARE NOT FREE, AND THIS REPO HAS MEASURED THE WALL: a publish/subscribe
    pattern that wrote its data into history twice died around 8k refs with `GrpcMessageTooLarge`,
    and a blob-cursor poll loop accounted for 86% of one workflow's history. One narration costs
    two events of its own plus the workflow task its firing wakes; a `say` in the body of a
    623-unit sweep is the same mistake in a friendlier costume.

    THE RUN SURVIVES AND THE NARRATION DOES NOT. Narration is decoration, and a run that died
    at hour six because its author was too talkative is a worse outcome than a long transcript —
    which is why this case does not raise while an over-long sentence does. What it must not be is
    silent, and it is not: the refusal is itself a turn, in the same channel, where the operator
    reading the account is told that the account is incomplete.
    """

    async def a_tight_loop() -> None:
        for unit in range(MAX_SENTENCES + 50):
            await say(f"unit {unit} dispatched")

    run(a_tight_loop())
    # Every sentence up to the budget, then exactly one more line: the refusal.
    assert len(wf.said) == MAX_SENTENCES + 1
    assert wf.said[MAX_SENTENCES - 1] == f"unit {MAX_SENTENCES - 1} dispatched"
    assert "narration budget spent" in wf.said[-1]
    assert "Narrate a phase, not a Unit." in wf.said[-1]


def test_the_budget_is_a_hard_ceiling_on_what_narration_can_add_to_a_history(wf: Fake) -> None:
    """The ceiling is the whole reason to prefer a budget to a warning. However many times a run
    calls `say`, the events narration contributes are bounded — about a thousand, against the
    51,200 Temporal will carry and the 8,000 refs this repo has measured dying of other things."""

    async def a_much_tighter_loop() -> None:
        for _ in range(MAX_SENTENCES * 10):
            await say("dispatched")

    run(a_much_tighter_loop())
    assert len(wf.timers) == MAX_SENTENCES + 1


def test_the_refusal_itself_fits_the_bound_it_is_announcing(wf: Fake) -> None:
    """A note about a budget that broke the budget would be the joke the whole file is about."""

    async def scenario() -> None:
        for _ in range(MAX_SENTENCES + 1):
            await say("x")

    run(scenario())
    assert utf8(wf.said[-1]) <= MAX_SENTENCE_BYTES


def test_the_budget_is_per_run_and_not_per_worker_process(monkeypatch: pytest.MonkeyPatch) -> None:
    """`actorkit` is a sandbox PASSTHROUGH module (`internals/temporal/wfhost.py`), so a
    module-level counter would be shared by every workflow instance in the worker process — and
    one talkative run would silence every other run on the same host, in a way that would only
    ever show up under load. The state hangs on the workflow instance for exactly that reason.

    It is also what makes a continued run correct: continue-as-new is a new execution and a new
    history, so it is entitled to a new budget.
    """
    first, second = Fake(), Fake()
    monkeypatch.setattr(temporal_workflow, "logger", logging.getLogger("test.narrate"))

    async def scenario(fake: Fake, n: int) -> None:
        monkeypatch.setattr(temporal_workflow, "instance", lambda: fake.instance)
        monkeypatch.setattr(temporal_workflow, "sleep", fake.sleep)
        for _ in range(n):
            await say("x")

    run(scenario(first, MAX_SENTENCES + 10))
    run(scenario(second, 3))
    assert len(first.timers) == MAX_SENTENCES + 1
    assert second.said == ["x", "x", "x"]


# ---------------------------------------------------------------------------------------------
# The reader on the other side
# ---------------------------------------------------------------------------------------------


def test_the_reader_pins_the_same_carrier(wf: Fake) -> None:
    """A TWO-WRITER CONTRACT WITH NO REGISTRATION STEP, like the Method Summary next door. Nothing
    fails loudly on a drift — the transcript would simply stop having narration in it, and every
    sentence would render as a raw timer — so the carrier is pinned from this side too."""
    reader = (ROOT / "shared" / "core" / "src" / "transcript.ts").read_text()
    # The event a sentence rides on, and the field it rides in.
    assert "if (e.type.startsWith('Timer'))" in reader
    assert "e.type === 'TimerStarted' && e.summary" in reader
    # A sentence is an instant. `TimerStarted` ends in `Started`, so this is the line that stops
    # every narrated run from reading as one with unfinished business in it.
    assert "open: false, text: e.summary" in reader
    # And the writer really writes it that way — `_emit` is the only command this module issues.
    assert "workflow.sleep(0, summary=line)" in inspect.getsource(narrate._emit)


def test_the_reader_still_reads_a_sentence_carried_some_other_way() -> None:
    """The Go SDK has no narration yet, and when it grows one it must not need a second branch in
    the reader. What makes a narration there is a Summary on an event that is about nothing else —
    the timer is how THIS SDK writes one, not what one is.

    THE GUARD MAY EXCLUDE, IT MAY NOT ENUMERATE. It grew a second term when an ask's memo upsert
    turned out to carry a Summary and was being read as a sentence, so what is pinned is the shape:
    `e.summary` alone opens the branch, and everything after it is a NEGATION — a list of things
    this is not. A guard that instead grew a positive term (`e.type === ... &&`) would be the
    reader deciding in advance which SDK may narrate, which is the drift this test exists to catch
    and the one a bare substring match on the old line could not tell apart from a rename.
    """
    reader = (ROOT / "shared" / "core" / "src" / "transcript.ts").read_text()
    generic = reader.split("// ── an author's own sentence, written some other way ──")[1]
    guard = generic.split("turns.push(rawTurn(e));")[0]
    opened = re.search(r"^\s*if \(e\.summary(?P<rest>.*)\) \{$", guard, re.M)
    assert opened, "the generic branch no longer opens on a Summary alone"
    for term in opened.group("rest").split("&&"):
        term = term.strip()
        if term:
            assert term.startswith("!"), f"the generic branch narrowed on a positive term: {term}"
