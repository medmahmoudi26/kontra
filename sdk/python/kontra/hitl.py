"""Park a workflow on a question a human has to answer — the author's half of HITL.

    from kontra import hitl

    answer = await hitl.ask(
        "Approve these 12 hosts?",
        takes=Approval,                       # a schema, per ask
        context={"dataset": "live", "n": 12}, # what the operator needs to decide
        deadline=timedelta(hours=4),          # or None to wait indefinitely
    )

{@link ask} IS ALSO A TOP-LEVEL VERB — `from kontra import ask` reaches this exact function.
A `workflow.logger` line reports and returns; this one BLOCKS, stopping the run until a person
moves it. They were never a pair (ADR 0050 §2 removed `speak`, and `note`/`partial` went after it).
Same function, either spelling; nothing here is a second implementation and no existing `hitl.ask`
caller changed.

THE QUESTION IS CONTEXTUAL, which is why none of it is a static declaration on the workflow or in
the catalog. What a run needs a human for depends on what it just found: the prompt, the shape of
the answer and the material to judge it by are all assembled at the moment it parks. A descriptor
could only ever declare that this workflow asks *something*.

── THE ASK IS A HISTORY EVENT, NOT A QUERY HANDLER ────────────────────────────────────────────

A query is the house idiom (`workflows/tmuxSession.ts`, `workflows/stack.ts`) and would be cheaper
than this. It fails in the two places that matter. A query is answered by a WORKER, so a parked run
whose worker is down could not be read at all — the exact unpolled-queue blindness this surface
exists to remove, and a parked run is precisely the run most likely to outlive the process that
parked it. And a query writes nothing to history, so the ask would never reach the archived reduced
log (ADR 0025) while the ANSWER would, since `signal` is already a reduced-log category. An archive
holding an answer and not its question is a record of somebody approving something unspecified.

So an ask is `workflow.upsert_memo` — one `WorkflowPropertiesModified` event per ask and one per
answer. Two properties fall out of that choice and both are load-bearing:

  • The memo rides on `DescribeWorkflowExecution` and on the visibility listing, so the orchestrator
    reads a run's pending asks with ONE RPC, no history scan, and NO WORKER ANYWHERE.
  • It is the run's own durable state, so an ask survives a worker restart with nothing to
    reconcile: the workflow replays, `ask()` re-runs, and it re-parks on the same question.

── WHAT MUST NEVER TRAVEL IN ONE ──────────────────────────────────────────────────────────────

`context` REACHES HISTORY IN THE CLEAR. The codec is a CLAIM-CHECK, NOT ENCRYPTION (ADR 0007):
under 128 KiB a value rides inline as plain JSON, and over it the value is moved to the blob store
and replaced by a ref that any reader of the run can dereference. Neither branch hides anything
from anyone who can read the run — which is exactly what makes an ask useful, and what makes a
credential in one a leak.
{@link redact} refuses to carry a value under a secret-shaped key rather than trusting the rule to
be remembered; it names the key it dropped, because a silently thinner context is a worse decision
aid than an obviously censored one.

THE ANSWER IS NEVER A BATCH VALUE. It comes back as the plain JSON the operator submitted, and
nothing here publishes it, pushes it to a Dataset or turns it into Units. An answer that became a
Batch would be content-addressed into the blob plane and materialized into the lake, which is a
permanent, queryable copy of a human's judgement in a place nobody chose to put it.

── THE DEADLINE IS THE AUTHOR'S ───────────────────────────────────────────────────────────────

Omitting `deadline` applies {@link DEFAULT_DEADLINE}. Passing `None` EXPLICITLY means wait forever,
which is legitimate: a durable workflow genuinely can, and an approval gate on a run that must
not proceed unattended is the case for it. On expiry {@link ask} RAISES {@link AskExpired} inside
the workflow — kontra does not choose retry, escalate, take a safe default or fail on the author's
behalf, because those are four different correct answers depending on what was being approved.

`AskExpired` is an `ApplicationError` on purpose. An uncaught plain exception fails the WORKFLOW
TASK in Python's SDK and retries it forever, so a run whose ask expired would sit at `running` with
nothing moving — the failure this whole surface exists to make visible, produced by the surface
itself.

── THE OPERATOR LABEL IS ATTRIBUTION, NEVER AUTHENTICATION ────────────────────────────────────

The appliance is loopback with no credential (ADR 0031), so there is no authenticated identity to
record and this SDK does not pretend otherwise. `by` is whatever the answering client said it was.
It is genuinely useful — on a shared box, or reading your own history six weeks later — and nothing
in kontra gates anything on it. Do not build one that does.
"""

from __future__ import annotations

import asyncio
import json
import re
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any, Mapping, Optional

# Temporal AT MODULE SCOPE, which `lib/actor.py` and `lib/catalog.py` deliberately avoid — the same
# exemption `lib/contract.py` takes for `nexusrpc`, and for the same reason. This module is only
# ever imported from INSIDE a `@workflow.defn`, where temporalio is present by definition, and
# `AskExpired` must subclass `ApplicationError` at class-definition time to fail the workflow rather
# than its task. `kontra/__init__.py` therefore does NOT import it, so `import kontra` stays
# free of a Temporal dependency; authors reach it as `from kontra import hitl`.
from temporalio import workflow
from temporalio.exceptions import ApplicationError

#: The memo key one ask is filed under: `kontra.ask.<id>`. A PREFIX rather than one aggregate key,
#: because concurrent asks are normal here — parallel branches each needing a decision — and an
#: aggregate would make two branches parking in the same workflow task write over each other.
ASK_MEMO_PREFIX = "kontra.ask."

#: The signal one ask is answered by: `kontra.answer/<id>`. The ID IS IN THE NAME, not only in the
#: payload, so the reduced log can pair an answer with its question from event metadata alone —
#: `signalName` is a plain string field on the event, and reading it costs no payload decode.
ANSWER_SIGNAL_PREFIX = "kontra.answer/"

#: Applied when the author names no deadline. Long enough that an approval reaching a human the
#: next working morning still lands, short enough that a forgotten run does not hold a fleet for a
#: week. Passing `deadline=None` explicitly overrides it with "wait indefinitely".
DEFAULT_DEADLINE = timedelta(hours=24)

#: The most `context` may carry into the memo. Temporal's memo has a hard size ceiling and blowing
#: it fails the workflow AT PARK TIME — turning "ask a human" into "the run died". A context past
#: this is replaced by a note saying so, which is a legible ask rather than a dead run.
MAX_CONTEXT_BYTES = 32_768

#: The most a prompt may carry. A prompt is a sentence; anything longer belongs in `context`. A
#: prompt over this is cut and MARKED, because a question silently shortened is a question whose
#: meaning may have changed under the person answering it.
MAX_PROMPT_CHARS = 2_000
ELIDED = " … [prompt truncated by kontra]"

#: What a redacted value is replaced with. Visible on purpose — see {@link redact}.
REDACTED = "[redacted by kontra: an ask travels through history in the clear]"

#: Keys whose VALUE never reaches an ask. Matched on the key alone and case-insensitively, against
#: the whole key rather than a substring of it, so `context` and `tokens_used` survive while
#: `api_key` and `session_token` do not. Deliberately a short, boring list: a clever matcher that
#: censors half a decision aid is worse than one that misses an exotic spelling, because the author
#: can see what it dropped and the leak it is aimed at is the ordinary one.
SECRET_KEY_RE = re.compile(
    r"^(.*_)?(password|passwd|pwd|secret|secrets|token|api_?key|apikey|access_?key|"
    r"private_?key|credential|credentials|authorization|cookie|session_?key)(_.*)?$",
    re.IGNORECASE,
)

#: How deep {@link redact} walks. Bounded so a self-referential or pathological context cannot
#: recurse forever inside a workflow task.
_MAX_DEPTH = 8

#: Where per-run ask state hangs. ON THE WORKFLOW INSTANCE, not in a module global: `kontra` is a
#: sandbox PASSTHROUGH module (`internals/temporal/wfhost.py`), so a module-level dict here would be
#: shared by every workflow instance in the worker process and two runs asking at once would read
#: each other's answers.
_STATE_ATTR = "__kontra_hitl__"


class AskExpired(ApplicationError):
    """The deadline the author declared passed with nobody answering.

    RAISED INSIDE THE WORKFLOW, at the `await hitl.ask(...)` that parked, so the decision about
    what a timeout means stays with the person who knew what was being asked. Catch it to retry,
    to escalate, to take a safe default, or let it fail the run — all four are correct answers to
    different questions and kontra is not in a position to pick.

    An `ApplicationError` (see the module header): an uncaught plain exception would fail the
    WORKFLOW TASK and retry it forever, leaving the run reporting `running` with nothing moving.
    """

    def __init__(self, ask_id: str, prompt: str, waited: timedelta) -> None:
        super().__init__(
            f"ask {ask_id} expired after {_round(waited.total_seconds())}s unanswered: {prompt}",
            type="AskExpired",
            non_retryable=True,
        )
        self.ask_id = ask_id
        self.prompt = prompt
        self.waited = waited


@dataclass
class _Slot:
    """One live ask, as the workflow holds it while it waits."""

    id: str
    #: The memo envelope — the exact dict the read route serves back.
    envelope: dict[str, Any]
    answered: bool = False
    value: Any = None
    by: str = ""


@dataclass
class _Asks:
    """This workflow instance's asks. Deterministic: the counter advances in call order, and a
    workflow's call order is what replay reproduces."""

    n: int = 0
    live: dict[str, _Slot] = field(default_factory=dict)


def _state() -> _Asks:
    inst = workflow.instance()
    st = getattr(inst, _STATE_ATTR, None)
    if st is None:
        st = _Asks()
        setattr(inst, _STATE_ATTR, st)
    return st


def _now_ms() -> int:
    """The workflow's own clock in epoch ms. `workflow.now()` is replay-stable; `time.time()` is
    not, and an `askedAt` that moved on replay would be a nondeterminism error at park time."""
    return int(workflow.now().timestamp() * 1000)


def _round(seconds: float) -> float:
    return round(seconds, 1)


def redact(value: Any, _depth: int = 0) -> Any:
    """Drop the value of anything under a secret-shaped key, recursively.

    NOT A SECURITY BOUNDARY and not sold as one. It is a guard against the ordinary mistake —
    passing a config dict straight into `context` because it happened to be in scope — and it
    catches that one reliably. A secret under a key it does not recognise still travels, which is
    why the rule in the module header is stated as a rule and not as a promise this function keeps.

    IT REPLACES RATHER THAN DELETING, and the replacement says why. A key that vanished would read
    to the operator as a fact the workflow did not have, which is a different (and worse) statement
    than "the workflow had this and kontra would not carry it".

    IT DOES NOT RAISE. A parked run that died because its context was impolite is an outage created
    by a safety rule, and the author's fix — drop the field — is the same either way.
    """
    if _depth >= _MAX_DEPTH:
        return value
    if isinstance(value, Mapping):
        return {
            str(k): REDACTED if SECRET_KEY_RE.match(str(k)) else redact(v, _depth + 1)
            for k, v in value.items()
        }
    if isinstance(value, (list, tuple)):
        return [redact(v, _depth + 1) for v in value]
    return value


def _fit(context: Any) -> Any:
    """`context` bounded to {@link MAX_CONTEXT_BYTES}, or a note saying it was not carried.

    THE BOUND IS THE POINT. Temporal's memo has a hard ceiling and exceeding it fails the workflow
    at the `upsert_memo` — so an ask carrying one page too many of sample rows would not park, it
    would kill the run. A note in its place leaves an answerable question with a visibly missing
    aid, which is the failure an operator can act on.
    """
    if context is None:
        return None
    try:
        size = len(json.dumps(context, default=str).encode())
    except (TypeError, ValueError):
        return {"kontra": "context was not JSON-serialisable and was not carried"}
    if size <= MAX_CONTEXT_BYTES:
        return context
    return {
        "kontra": (
            f"context was {size} bytes, over the {MAX_CONTEXT_BYTES}-byte limit, and was not "
            f"carried — attach a Dataset name or a sample rather than the whole thing"
        )
    }


def _schema_of(takes: Any) -> Optional[dict[str, Any]]:
    """The JSON Schema the answer is validated against and the form is rendered from.

    A DICT PASSES STRAIGHT THROUGH, so an author who wants a schema this SDK cannot derive writes
    one. A type goes through the same `kontra.schema` pydantic derivation the actor catalog uses
    — one derivation, so an ask's form and a Method's form cannot disagree about the same class.

    The import is PASSED THROUGH the workflow sandbox explicitly. pydantic under the sandbox's
    import proxy is both slow and a re-import of a large module per workflow instance, and the
    derivation itself is pure — the same type yields the same document on every replay.
    """
    if takes is None:
        return None
    if isinstance(takes, Mapping):
        return dict(takes)
    with workflow.unsafe.imports_passed_through():
        from kontra.schema import schema_of

    return schema_of(takes)


def _coerce(takes: Any, value: Any) -> Any:
    """The operator's answer as the type the author declared, where one was declared.

    BEST EFFORT, AND THE PLAIN VALUE OTHERWISE. The answer was already validated against this
    schema by the route that accepted it, so a coercion failure here means the two disagree about
    the same document — a real bug, but one whose right cost is the author seeing a dict rather
    than a parked run failing at the moment somebody finally answered it.
    """
    if takes is None or isinstance(takes, Mapping):
        return value
    with workflow.unsafe.imports_passed_through():
        from kontra.schema import coerce
    try:
        return coerce(takes, value)
    except Exception:  # noqa: BLE001 — see the docstring; the plain value is the safe answer
        workflow.logger.warning(
            "hitl: an answer validated against its schema did not coerce to %r; "
            "returning the plain value",
            takes,
        )
        return value


def _ask_id(explicit: Optional[str], n: int) -> str:
    """`ask-3`, or the author's own id.

    DETERMINISTIC, from a counter rather than from `workflow.uuid4()`. Both replay identically, and
    a counter additionally reads as itself in a URL, in a log line and in the `signalName` the
    reduced log records — which is the whole reason the id is in the signal name at all.
    """
    if explicit:
        return explicit
    return f"ask-{n}"


def _envelope(
    ask_id: str,
    prompt: str,
    schema: Optional[dict[str, Any]],
    context: Any,
    asked_at: int,
    deadline_at: Optional[int],
) -> dict[str, Any]:
    """One ask as the memo carries it — and as `GET /api/runs/:id/asks` serves it back.

    ONE SHAPE, NOT TWO. The field names are `control/orchestrator/src/transcript.ts`'s `Ask` verbatim, so
    nothing between here and the transcript translates: a rename on either side is a missing field
    a test catches, rather than a silent mapping layer that drops one.
    """
    env: dict[str, Any] = {
        "id": ask_id,
        "prompt": prompt if len(prompt) <= MAX_PROMPT_CHARS else prompt[:MAX_PROMPT_CHARS] + ELIDED,
        "askedAt": asked_at,
        "state": "pending",
    }
    if schema is not None:
        env["schema"] = schema
    if context is not None:
        env["context"] = context
    if deadline_at is not None:
        env["deadlineAt"] = deadline_at
    return env


async def ask(
    prompt: str,
    *,
    takes: Any = None,
    context: Any = None,
    deadline: Any = DEFAULT_DEADLINE,
    id: Optional[str] = None,
) -> Any:
    """Park this workflow on a question, and return what a human answered.

        from kontra import ask

        answer = await ask("Approve these 12 hosts?", takes=Approval, context={"n": 12})

    WHY THIS IS NOT A LOG LINE. `workflow.logger.info(...)` RETURNS IMMEDIATELY and costs no
    history; `ask` costs history AND STOPS THE RUN, until a human answers or the deadline expires.
    Reaching for this one where a progress line was meant does not produce a chattier transcript —
    it produces a stalled run, waiting on a person nobody told to look. Say what a run is doing
    with `workflow.logger`; use `ask` only where the run genuinely must not proceed unattended.

    Args:
        prompt: the sentence the operator reads.
        takes: the shape of the answer — a dataclass/pydantic type, or a JSON Schema dict. It is
            what the form renders from AND what the answer route validates against before it
            signals, so an ask with no `takes` accepts whatever it is handed.
        context: what the operator needs to decide — the Dataset, the counts, the sample. NEVER a
            credential: it reaches history in the clear. See {@link redact}.
        deadline: a `timedelta`, or `None` to wait indefinitely. Omitted applies
            {@link DEFAULT_DEADLINE}. On expiry this RAISES {@link AskExpired} rather than
            deciding for you.
        id: an id of your own, when a stable one matters (a retried leg re-asking the same
            question). Defaults to `ask-<n>` in call order.

    Returns:
        the answer as the operator submitted it, coerced to `takes` where that is a type. It is a
        plain value and never a Batch — see the module header.

    Raises:
        AskExpired: the deadline passed unanswered.
    """
    st = _state()
    st.n += 1
    ask_id = _ask_id(id, st.n)
    if ask_id in st.live:
        # TWO LIVE ASKS UNDER ONE ID would share a memo key and a signal name, so answering either
        # would answer whichever the handler table happened to hold — a wrong answer delivered to
        # a human's decision. Re-USING an id after the first one settled is fine and supported;
        # this refuses only the overlap.
        raise ValueError(
            f"ask id {ask_id!r} is already pending on this run — two live asks cannot share one id"
        )
    key = f"{ASK_MEMO_PREFIX}{ask_id}"
    signal = f"{ANSWER_SIGNAL_PREFIX}{ask_id}"

    asked_at = _now_ms()
    wait_for: Optional[timedelta] = None
    deadline_at: Optional[int] = None
    if deadline is not None:
        wait_for = deadline if isinstance(deadline, timedelta) else timedelta(seconds=float(deadline))
        deadline_at = asked_at + int(wait_for.total_seconds() * 1000)

    env = _envelope(ask_id, prompt, _schema_of(takes), _fit(redact(context)), asked_at, deadline_at)
    slot = _Slot(id=ask_id, envelope=env)
    st.live[ask_id] = slot

    # REGISTERED BEFORE THE MEMO IS WRITTEN. The memo is what makes the ask answerable, so a signal
    # can arrive the instant after it lands; the SDK does buffer signals that reach an unregistered
    # handler, but relying on that ordering is a race nobody would find until it mattered.
    workflow.set_signal_handler(signal, _answerer(slot))
    workflow.upsert_memo({key: env})
    workflow.logger.info("hitl: parked on %s — %s", ask_id, env["prompt"])

    try:
        await workflow.wait_condition(
            lambda: slot.answered,
            timeout=wait_for,
            # Temporal treats this as a TIMER ID and shows it on the bar in its own UI. The ask's
            # id, not its prompt: a timer named after a sentence reads as a sentence that timed
            # out, and this one is here so the deadline timer is drillable back to its question.
            timeout_summary=f"kontra.ask/{ask_id}",
        )
    except asyncio.TimeoutError:
        _close(key, env, "expired", {"expiredAt": _now_ms()})
        st.live.pop(ask_id, None)
        raise AskExpired(ask_id, env["prompt"], timedelta(milliseconds=_now_ms() - asked_at)) from None
    except asyncio.CancelledError:
        # THE RUN WAS CANCELLED WHILE WAITING FOR A HUMAN, which is a different ending from an
        # expiry and from an answer. Recorded as its own state so the ask stops being offered —
        # an answerable question on a cancelled run is a form that signals nothing.
        _close(key, env, "abandoned", {"abandonedAt": _now_ms()})
        st.live.pop(ask_id, None)
        raise

    _close(key, env, "answered", {"answeredAt": _now_ms(), "by": slot.by})
    st.live.pop(ask_id, None)
    return _coerce(takes, slot.value)


def _answerer(slot: _Slot):
    """The signal handler for one ask.

    FIRST ANSWER WINS. A duplicate is dropped rather than overwriting, because the workflow may
    already have acted on the first one and a second that silently replaced it would make the
    transcript's attribution false. The handler stays registered after the answer for exactly that
    reason: an unregistered name buffers the signal and reports an unfinished handler at close,
    where absorbing it here is a no-op that leaves the audit trail intact.
    """

    def deliver(payload: Any = None) -> None:
        if slot.answered:
            workflow.logger.warning("hitl: a second answer to %s was ignored", slot.id)
            return
        body = payload if isinstance(payload, Mapping) else {"value": payload}
        slot.value = body.get("value")
        # SELF-ASSERTED. Whatever the answering client called itself — see the module header.
        slot.by = str(body.get("by") or "")
        slot.answered = True

    return deliver


def _close(key: str, env: dict[str, Any], state: str, extra: Mapping[str, Any]) -> None:
    """Rewrite one ask's memo entry to its ending.

    THE SCHEMA IS DROPPED AND THE CONTEXT IS KEPT. The schema is the largest field and is derivable
    from the workflow's own source, so carrying it on a settled ask is memo weight for nothing. The
    context is the material the decision was made ON, and a settled ask that kept only its prompt
    would record somebody approving something whose particulars are gone.

    THE ANSWER'S VALUE IS NOT WRITTEN BACK. It arrived as a signal and is already in history where
    the operator put it; copying it into the memo would make a second, longer-lived copy of a
    human's judgement in a place nobody chose to put it, and would grow the memo by the size of
    every answer a long run ever took.
    """
    env.pop("schema", None)
    env["state"] = state
    env.update({k: v for k, v in extra.items() if v not in (None, "")})
    workflow.upsert_memo({key: env})


def pending() -> list[dict[str, Any]]:
    """What THIS workflow is currently parked on, in ask order.

    The workflow's own view of it. Every other reader — the route, the transcript, the chrome —
    reads the memo, which is the same envelopes and needs no worker to answer.
    """
    return [dict(slot.envelope) for slot in _state().live.values()]


__all__ = [
    "ask",
    "pending",
    "redact",
    "AskExpired",
    "ASK_MEMO_PREFIX",
    "ANSWER_SIGNAL_PREFIX",
    "DEFAULT_DEADLINE",
    "MAX_CONTEXT_BYTES",
    "REDACTED",
    "ELIDED",
]
