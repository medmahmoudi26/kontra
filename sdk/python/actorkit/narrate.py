"""An author's own sentence about their own run — the `say` half of the transcript.

    from actorkit import narrate

    await narrate.say(f"{len(apexes)} apexes in scope from the paid-programs list")
    ...
    await narrate.say(f"{live} of {len(apexes)} resolve; dispatching the crawler")

TWO NAMES, ONE SENTENCE. {@link speak} is this same call under the name the top level exposes —
`from actorkit import ask, speak` — because `speak` and `ask` are the two things a workflow says
out loud and an author reaches for them together. It delegates here rather than reimplementing
anything, so there is one budget, one redaction and one shape of turn however it was spelled;
`say` is the older name, every existing caller of it is untouched, and both remain correct.

DERIVED TURNS MAKE AN UNTOOLED WORKFLOW READABLE; NARRATION MAKES A TOOLED ONE EXPLAIN ITSELF.
Everything else in a transcript is inferred from event metadata — which Actor, which Method, how
many machines, what failed, how long. None of it can say what the run MEANT by any of it, because
that is not in the log: it is in the head of the person who wrote the workflow. `say` is the one
place they can put it, and it lands in the transcript at the point in the run where it was
written, in among the turns it is about.

── IT IS A TIMER, AND THAT IS THE WHOLE MECHANISM ─────────────────────────────────────────────

A narration is `workflow.sleep(0, summary=…)`: a zero-duration timer whose Temporal user metadata
IS the sentence. Four properties made that the only shape that works here.

  • METADATA, NOT PAYLOAD — the same route the Method name takes (`catalog.dispatch_summary`). A
    payload on this deployment may be a claim-check ref (ADR 0007), so a sentence carried in one
    would cost a blob GET per turn to read, and reading a 623-unit sweep would fan out into the
    object store. A Summary is read straight off the event by `control/orchestrator/src/history.ts`, and
    Temporal's own UI renders it on the timer's bar label — so both surfaces say the same
    sentence rather than diverging.
  • IT REACHES THE ARCHIVE. The reduced log is what ADR 0025 stores, and a Summary is part of a
    reduced event. So an author's sentences are still there when Temporal's 24-hour retention has
    dropped the history they came from — which is the requirement that rules out a memo. `hitl`
    writes one (`upsert_memo`) because a parked run must be READABLE by one RPC with no worker
    anywhere; but a memo's value is a payload, the reduced log decodes none, and a narration that
    never reached the archive would be a sentence with a 24-hour lifespan.
  • NO WORKER, NO REGISTRATION, NO QUEUE. An activity carries a Summary too, and it needs a
    function registered on the CALLER's own worker and a queue somebody polls — so a narration
    would be able to sit queued, retry, and time out. Those are absurd properties for a sentence.
  • IT IS DETERMINISTIC. A timer is a workflow command like any other: it replays, it needs no
    side channel, and `say` can be called from anywhere inside a workflow with nothing wired up.

The Python SDK attaches user metadata to exactly four commands — a timer, an activity, a child
workflow and a Nexus operation (`WorkflowCommand.user_metadata`; the marker commands behind
`workflow.patched` take none). Three of the four are things that RUN. Only the timer is a place
to put a sentence.

── HISTORY EVENTS ARE NOT FREE, AND THIS REPO HAS MEASURED THE WALL ───────────────────────────

MEASURED against Temporal Server 1.31.0, and asserted rather than remembered
(`tests/test_narration_live.py`, 2026-08-25): one narration is EXACTLY FIVE HISTORY EVENTS —
`TimerStarted` carrying the sentence, `TimerFired`, and the three-event workflow task the firing
wakes — and about ONE SECOND of wall clock, which is the server's own timer granularity and not
something this module can shorten.

Per phase both numbers are nothing: a second on a phase that took four minutes, five events on a
history of twenty thousand. Per Unit they are the failure this repo has already had twice — a
publish/subscribe pattern that wrote its data into history twice and died around 8k refs with
`GrpcMessageTooLarge`, and a blob-cursor poll loop that accounted for 86% of one workflow's
history. A `say` in the body of a 623-unit sweep is the same mistake in a friendlier costume, and
it would add ten minutes of pure waiting to the sweep on top of the events.

So the two ways to get this wrong are bounded, and DIFFERENTLY, because they are two different
mistakes with two different right costs:

  • A SENTENCE THAT IS TOO LONG IS AN AUTHORING ERROR. It is deterministic, it happens the first
    time the workflow runs, and it happens on sentence one. {@link say} RAISES
    {@link NarrationRefused} rather than truncating: a sentence cut in the middle may have
    changed its meaning under the person reading it, and a bound nobody is told about is not a
    bound. See {@link MAX_SENTENCE_BYTES}.
  • A SENTENCE PER UNIT IS A SHAPE ERROR, and it only shows up at scale — in production, on the
    run that mattered, which is exactly when killing the run is the wrong answer. Narration is
    decoration; a run that died at hour six because its author was too talkative is a worse
    outcome than a long transcript. So the run survives and the NARRATION is refused: past
    {@link MAX_SENTENCES} this module says so once, in the transcript, in the author's own
    channel, and then stays silent for the rest of the run. That is a hard ceiling on the events
    narration can ever add to a history — about a thousand — and it arrives early: a per-Unit
    loop trips it in its first two hundred Units, minutes in, not at 20,000 events. The one
    thing it must not be is silent, and it is not: the refusal is itself a turn.

`workflow.logger` carries both, because a run whose author is watching it should not have to read
its own transcript to find out.

── WHAT MUST NEVER TRAVEL IN ONE ──────────────────────────────────────────────────────────────

A narration REACHES HISTORY IN THE CLEAR, the same way an ask does: the codec is a claim-check,
not encryption (ADR 0007), and a Summary is far too small to be offloaded, so it rides inline as
plain text that any reader of the run can see. {@link redact} replaces the value of anything
written as a secret-shaped assignment — `token=…`, `Authorization: Bearer …` — because the
ordinary mistake is an f-string that interpolated a variable that happened to be in scope. It is
not a scanner and is not sold as one: a credential in a sentence that does not name it still
travels, which is why the rule above is a rule rather than a promise this function keeps.

AND IT IS NOT A PLACE FOR A PAYLOAD. `say` takes a sentence and nothing else — no `context=`, no
fields, no object. That is the design and not an omission: values belong in a Dataset, where they
are queryable, deduplicated and out of history. A narration says what the run means; the lake
says what it found.

── A WORKFLOW THAT NARRATES NOTHING IS UNCHANGED ──────────────────────────────────────────────

Nothing in this module runs unless `say` is called: no state on the instance, no command, no
event, no import from any workflow that does not reach for it. `actorkit/__init__.py` therefore
does not import it — the same exemption `hitl` takes, and for the same reason (temporalio at
module scope, so that {@link NarrationRefused} can subclass `ApplicationError` at class-definition
time). Authors reach it as `from actorkit import narrate`.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

# Temporal AT MODULE SCOPE, which `lib/catalog.py` and `lib/actor.py` deliberately avoid. Same
# exemption, same reason, as `lib/hitl.py`: this module is only ever imported from INSIDE a
# `@workflow.defn`, where temporalio is present by definition, and `NarrationRefused` must
# subclass `ApplicationError` at class-definition time to fail the RUN rather than its task.
from temporalio import workflow
from temporalio.exceptions import ApplicationError

# ONE BUDGET, NOT TWO. Every Summary this SDK writes — a dispatch's Method line, a Session's open,
# an author's sentence — is the same kind of thing in the same place, and 200 bytes is already the
# number with the reasoning attached: it renders on a Temporal bar label without eliding, and it is
# some 650× under the codec's 128 KiB offload threshold, which is what keeps a Summary inline and
# therefore readable without a fetch (`summaryOf` in control/orchestrator/src/history.ts REFUSES a
# claim-checked Summary rather than dereferencing it, so a sentence over that threshold would not
# be shortened — it would be gone).
from actorkit.catalog import SUMMARY_BUDGET

#: How many UTF-8 BYTES one sentence may take.
#:
#: BYTES, NOT CHARACTERS, because that is what the wire and the threshold above are counted in,
#: and the two differ on exactly the prose an operator is most likely to write — an em dash, a
#: hostname in a non-Latin script, the `·` this repo separates fields with.
#:
#: It is roughly two sentences of English. That is the size narration is FOR: a phase marker, a
#: count with its meaning attached, the reason the next thing is about to happen. Anything that
#: does not fit is not a narration — it is a payload, and belongs in a Dataset.
MAX_SENTENCE_BYTES = SUMMARY_BUDGET

#: How many sentences one run may narrate, before this module stops carrying them.
#:
#: PER EXECUTION, which is per history — so a run that continues-as-new starts a fresh budget,
#: correctly: the new leg is a new history and the old one is already closed and archived.
#:
#: The number is chosen from both ends. Five events a sentence puts the ceiling at ~1,000 events,
#: which cannot be what kills a workflow that Temporal will carry to 51,200 and that this repo has
#: measured dying of other things at 8,000 refs. And an author narrating a phase at a time writes
#: perhaps twenty in a long run, so the honest use never comes near it while the per-Unit
#: mistake trips it inside the first two hundred Units.
MAX_SENTENCES = 200

#: What a redacted value is replaced with. Short, because it is spent out of the same 200 bytes as
#: the sentence, and VISIBLE, because a sentence that quietly lost a word is a worse account than
#: one that says which word it would not carry.
REDACTED = "[redacted]"

#: A secret written as an ASSIGNMENT — `token=abc`, `api_key: abc`, `password = abc`. The shape a
#: leaked credential actually takes in a formatted string, and the only shape matched, because
#: anything looser eats prose: `token bucket refilled` must survive, and a matcher keyed on the
#: word alone would redact `bucket`.
#:
#: THE SECOND SPELLING of `hitl.SECRET_KEY_RE`'s word list, deliberately and not shared: that one
#: is anchored to a WHOLE key in a mapping (`^…$`), this one has to find a word inside a sentence,
#: and forcing one regex to do both would make the shared thing wrong for both. The drift risk is
#: real and the failure mode is the house one — a word only one of them knows is redacted in one
#: surface and not in the other, which is why neither is described as a boundary.
SECRET_ASSIGNMENT_RE = re.compile(
    r"\b(?:[A-Za-z0-9]+[_-])?"
    r"(?:password|passwd|pwd|secret|secrets|token|api[_-]?key|apikey|access[_-]?key|"
    r"private[_-]?key|credential|credentials|authorization|cookie|session[_-]?key)"
    r"(?:[_-][A-Za-z0-9]+)?"
    # THE SCHEME WORD IS STEPPED OVER, NOT CAPTURED. `Authorization: Bearer eyJhbGciOi…` is the
    # commonest spelling of all of these, and a matcher that took the first word after the colon
    # would redact `Bearer` and leave the credential standing beside it.
    r"\s*[:=]\s*(?:(?:bearer|basic|token)\s+)?(\S+)",
    re.IGNORECASE,
)

#: `Authorization: Bearer …` without the header name, which is how it is usually pasted, and
#: `Basic …` beside it. Bounded to something long enough to be a credential rather than a word, so
#: `bearer of bad news` is prose and `Bearer eyJhbGciOi…` is not.
BEARER_RE = re.compile(r"\b(?:bearer|basic)\s+([A-Za-z0-9._~+/=-]{12,})", re.IGNORECASE)

#: Where the per-run narration count hangs. ON THE WORKFLOW INSTANCE, never in a module global:
#: `actorkit` is a sandbox PASSTHROUGH module (`internals/temporal/wfhost.py`), so a module-level
#: counter would be shared by every workflow instance in the worker process and one talkative run
#: would silence every other run on the same host.
_STATE_ATTR = "__kontra_narration__"

#: The whole of the run's narration, once the budget is spent. Built to fit the same bound as any
#: other sentence, and it names the fix rather than only the rule.
_SPENT = (
    "kontra: narration budget spent — {n} sentences in one run, and the rest of this run is not "
    "narrated. Narrate a phase, not a Unit."
)


class NarrationRefused(ApplicationError):
    """A sentence this module would not write, raised at the `await narrate.say(...)` that wrote it.

    RAISED, NOT SWALLOWED, because every case that reaches here is deterministic: the same call
    refuses on every replay and on every run of the same code, so it is found the first time the
    workflow is exercised rather than in production. The alternative — carrying a bad sentence
    anyway — puts something in the transcript that is not what the author wrote, which is the one
    thing a record of somebody's own words must not do.

    An `ApplicationError`, and non-retryable. A plain exception raised inside a workflow fails the
    WORKFLOW TASK in Python's SDK and retries it forever, so a run refused here would sit at
    `running` with nothing moving — the invisible failure this whole surface exists to remove,
    produced by the surface itself.

    THE SENTENCE-PER-UNIT CASE DOES NOT COME THROUGH HERE, deliberately: see {@link MAX_SENTENCES}.
    """

    def __init__(self, why: str) -> None:
        super().__init__(why, type="NarrationRefused", non_retryable=True)
        self.why = why


@dataclass
class _Narration:
    """This workflow instance's narration, as the run holds it.

    Deterministic: `said` advances in call order, and a workflow's call order is what replay
    reproduces — so the budget is spent at the same sentence on a replay as on the original.
    """

    said: int = 0
    #: The budget was spent, the note was written, and nothing further is carried.
    stopped: bool = False


def _state() -> _Narration:
    inst = workflow.instance()
    st = getattr(inst, _STATE_ATTR, None)
    if st is None:
        st = _Narration()
        setattr(inst, _STATE_ATTR, st)
    return st


def _utf8(text: str) -> int:
    """How many bytes this string costs. The budget is in bytes; `len()` counts characters."""
    return len(text.encode("utf-8"))


def one_line(sentence: str) -> str:
    """`sentence` with every run of whitespace collapsed to one space, and trimmed.

    A SUMMARY IS SINGLE-LINE by Temporal's own definition — it is a bar label, rendered as
    single-line markdown — so a sentence built with a newline in it would break the surface it was
    written for. This is normalisation and not truncation: every word survives, in order, and the
    author's meaning with them. Nothing else about the prose is touched.
    """
    return re.sub(r"\s+", " ", sentence).strip()


def redact(sentence: str) -> str:
    """`sentence` with the value of any secret-shaped assignment replaced by {@link REDACTED}.

    NOT A SECURITY BOUNDARY and not sold as one — see the module header. It is a guard against the
    ordinary mistake, an f-string that interpolated something that happened to be in scope, and it
    catches that one where the sentence names what it is carrying.

    IT REPLACES RATHER THAN DROPPING THE SENTENCE, and it does not raise. A run that died because
    its narration was impolite would be an outage created by a safety rule, and the author's fix —
    stop putting the value in the sentence — is the same either way.
    """

    def cut(m: re.Match[str]) -> str:
        # Only the VALUE is replaced; the key stays, because "there was a token here and kontra
        # would not carry it" is a more useful line than a sentence with a hole in it.
        return m.group(0)[: m.start(1) - m.start(0)] + REDACTED

    # Assignments first, so a named header is redacted as a whole; the bare-scheme pass then picks
    # up the pasted `Bearer …` that named nothing, and cannot re-match what the first pass left
    # behind ({@link REDACTED} carries characters no credential pattern here accepts).
    return BEARER_RE.sub(cut, SECRET_ASSIGNMENT_RE.sub(cut, sentence))


def narration_summary(sentence: str) -> str:
    """The one line a narration puts on its own event — normalised, redacted, and within budget.

    A PLAIN FUNCTION, like `catalog.dispatch_summary`, and for the same reason: the whole
    discipline of what does and does not reach history is decidable without a Temporal cluster,
    so it is assertable in a unit test rather than only in an integration one.

    Raises {@link NarrationRefused} when the finished line is over {@link MAX_SENTENCE_BYTES}.
    THE BOUND IS CHECKED ON WHAT WOULD BE WRITTEN, not on what was passed in — redaction happens
    first, so a sentence is never refused for the size of a credential that was never going to be
    carried anyway.
    """
    line = redact(one_line(sentence))
    size = _utf8(line)
    if size > MAX_SENTENCE_BYTES:
        raise NarrationRefused(
            f"a narration is one line of at most {MAX_SENTENCE_BYTES} bytes and this one is "
            f"{size}: {line[:60]}… — shorten it, or put the detail in a Dataset. It is not "
            f"truncated, because half a sentence may mean something its author did not write."
        )
    return line


async def say(sentence: str) -> None:
    """Write one sentence into this run's transcript, here, at this point in the run.

        await narrate.say(f"{live} of {len(apexes)} apexes resolve; crawling those")

    It becomes a `narration` turn in the transcript, in among the derived turns, at the instant it
    was written — and a labelled timer in Temporal's own UI, which is the same fact seen from the
    other side.

    COSTS FIVE HISTORY EVENTS AND ABOUT A SECOND, both measured. Narrate a phase; never a Unit.
    The module header says what happens if you do it anyway, and {@link MAX_SENTENCES} is where.

    Raises {@link NarrationRefused} for a sentence that is not a sentence — the wrong type, or one
    over {@link MAX_SENTENCE_BYTES}. Both are deterministic and both are authoring errors.
    """
    if not isinstance(sentence, str):
        # A TYPE ERROR HERE WOULD BE INVISIBLE. `TypeError` is not a `FailureError`, so Python's
        # SDK fails the workflow TASK with it and retries forever; the run would report `running`
        # with nothing moving. And coercing with `str()` is worse than either — it is the back
        # door through which a dict of results becomes a payload in history.
        raise NarrationRefused(
            f"a narration is a sentence, not a {type(sentence).__name__} — values belong in a "
            f"Dataset, where they are queryable and out of history"
        )

    line = narration_summary(sentence)
    if not line:
        # NOTHING IS STRICTLY BETTER THAN A BLANK TURN. `summaryOf` reads an empty Summary as no
        # Summary, so an empty sentence would land as a bare timer with no text on it — a row in
        # the transcript where a sentence was meant to be, which reads as a bug in the reader
        # rather than as an empty f-string in the workflow. A workflow that narrates nothing is
        # unchanged, and this is that.
        workflow.logger.warning("narrate.say was given an empty sentence; nothing was written")
        return

    st = _state()
    if st.stopped:
        return
    if st.said >= MAX_SENTENCES:
        st.stopped = True
        workflow.logger.warning(
            "narrate.say: %d sentences in one run — the narration budget is spent and the rest "
            "of this run will not be narrated. Narrate a phase, not a Unit.",
            MAX_SENTENCES,
        )
        # THE REFUSAL IS ITSELF A TURN, which is the whole difference between refusing and going
        # quiet. An operator reading the transcript is told the account of this run is incomplete,
        # in the same place they would have read the rest of it.
        await _emit(_SPENT.format(n=MAX_SENTENCES))
        return

    st.said += 1
    await _emit(line)


async def speak(sentence: str) -> None:
    """Tell the operator where this run has got to. One sentence, and it RETURNS IMMEDIATELY.

        from actorkit import ask, speak

        for i, wave in enumerate(waves, 1):
            await speak(f"batch {i} of {n}")          # a line per PHASE, never one per Unit
            ...

    ── THE PAIR, AND WHY THERE ARE TWO ────────────────────────────────────────────────────────

    `speak` and `ask` are the two things a workflow says out loud, and the difference between
    them is the whole reason both exist:

      • `speak` COSTS HISTORY AND RETURNS IMMEDIATELY. Five events and about a second, then the
        run carries on. Nobody has to be watching, and nothing is waiting for anyone.
      • {@link actorkit.hitl.ask} COSTS HISTORY AND STOPS THE RUN, until a human answers it or
        its deadline expires.

    Reaching for the wrong one turns a progress line into a stalled run — a run sitting at
    `running` waiting for a person nobody told to look. That is why they are named as a pair and
    documented next to each other rather than left as two unrelated calls.

    ── A SENTENCE PER PHASE, NOT PER UNIT ─────────────────────────────────────────────────────

    IT IS EXACTLY {@link say} — the same timer, the same Summary, the same turn in the same
    transcript — so it inherits the same two bounds and neither is softened by being easier to
    reach. A sentence over {@link MAX_SENTENCE_BYTES} raises {@link NarrationRefused} rather than
    being truncated; past {@link MAX_SENTENCES} in one run the narration is refused (the run is
    not), once, in the transcript, and the rest of the run goes unnarrated.

    So the loop above narrates a WAVE, and a wave is a phase. A `speak` in the body of a
    623-Unit sweep is the shape error the budget exists to stop: it is invisible in a test, it
    adds ten minutes of pure waiting, and it only shows up at scale — in production, on the run
    you least wanted to lose. Counts, rows and results belong in a Dataset, where they are
    queryable and out of history; a narration says what the run MEANS by them.

    Raises {@link NarrationRefused} for a sentence that is not a sentence — the wrong type, or
    one over {@link MAX_SENTENCE_BYTES}. Both are deterministic and both are authoring errors.
    """
    # DELEGATION, NOT A SECOND IMPLEMENTATION. `say` is the older spelling and every existing
    # caller of it is untouched; two bodies would be two budgets, two redactions and eventually
    # two shapes of turn for the same sentence. One function, reached by either name.
    await say(sentence)


async def _emit(line: str) -> None:
    """The one command this module issues.

    A ZERO-DURATION TIMER, AWAITED. The wait is not the point — the event is — and the sentence is
    on `TimerStarted`, which is written before anything is waited for. So a fire-and-forget
    narration would still reach history, and it is not done, for two reasons that are about the
    transcript rather than about the sentence. A workflow that finished before its timer fired
    would CANCEL it, and `TimerCanceled` is a failure-category event: every sentence would leave
    one behind, and a run that failed after narrating would report its last failure as an author's
    remark. And an unawaited coroutine is an unfinished task at workflow completion, which
    Temporal warns about — a warning earned by a decoration is a warning nobody will read twice.

    So `say` pays the round trip. It is also, usefully, the most visible part of the price.
    """
    await workflow.sleep(0, summary=line)
