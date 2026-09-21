"""`note` and `partial` — what replaced `speak` (ADR 0050 §2).

── WHY `speak` IS GONE ─────────────────────────────────────────────────────────────────────────────

Narration put AUTHORED SENTENCES in Temporal's history, which is the engine's own record and costs
nothing extra only for what the engine writes. A sentence cost ~5 history events and ~1 second, and
the budget that made that survivable capped a Run at 200 of them. For no property a log line lacks:

    logs are not narration at higher volume; narration was logs in the wrong place.

Now that a line carries its `run_id` (kontra#16) and leaves the Machine with a disk buffer behind it
(kontra#15), the console can answer "what did this Run say" from the log store — across runs, which
is a question narration could never answer at all.

── THE CONDITION, WHICH IS THE WHOLE RISK ──────────────────────────────────────────────────────────

A completeness claim demoted to `log.info` has gone from IN THE RUN RECORD to one line among
thousands, and that is strictly worse than narration was. About a third of the sentences this
replaced were claims that the RESULT IS NOT WHAT A READER WOULD ASSUME:

    splitting axis ABANDONED … everything past this point in url order is UNSCANNED, not clean
    seed_limit 200 reached — the crawl is PARTIAL by request
    exchanges_8x8 is still open — this reads a partial crawl as whole

So there are TWO functions and not one. `note` is progress. `partial` is a claim about the result,
and it emits at WARNING with `incomplete=true` — a structured field the console's logs rail filters
on INDEPENDENTLY of the level, so raising the floor to `error` cannot hide it. Choosing between them
is the author's judgement and it is the one thing this module asks for:

    would a reader be wrong about the result if they missed this line?

── NEITHER BLOCKS, AND `ask` STILL DOES ────────────────────────────────────────────────────────────

`ask` is untouched and is not a pair with this. It BLOCKS the workflow until a human answers, and a
log line cannot block a workflow — which is why removing narration says nothing about it.
"""

from __future__ import annotations

from typing import Any, TypedDict

from kontra.redaction import one_line, redact

__all__ = ["note", "partial"]


def _logger() -> Any:
    """Temporal's replay-aware logger when we are inside a workflow, else an ordinary one.

    `workflow.logger` writes ONCE for a line rather than again on every replay, which is the property
    that makes logging from workflow code correct at all. Outside a workflow — a unit test, an
    activity, a script — `logging` is right and the import must not be required.
    """
    try:
        from temporalio import workflow

        if workflow.in_workflow():
            return workflow.logger
    except Exception:  # noqa: BLE001 - no temporalio, or not in a workflow context
        pass
    import logging

    return logging.getLogger("kontra.run")


#: The topic every kontra run publishes progress on. One name, so a consumer subscribing to a
#: run it has never seen still knows what to ask for.
PROGRESS_TOPIC = "progress"


def note(sentence: str, **fields: Any) -> None:
    """Say where the Run is. INFO.

    Redacted and single-lined on the way out, for the reason the old narration was: an f-string that
    interpolated something in scope is the ordinary mistake, and this catches the case where the
    sentence names what it is carrying. Not a security boundary — see `kontra.redaction`.

    NOT ASYNC, and that is the visible difference from `speak`. There is nothing to await: a log line
    is not a command, writes no event, and cannot fail the Run. An author who writes `await note(...)`
    gets a TypeError that says so, which is better than a silent no-op.
    """
    _logger().info(redact(one_line(sentence)), extra=_extra(fields))


def partial(sentence: str, **fields: Any) -> None:
    """Say that the RESULT is not what a reader would assume. WARNING, and `incomplete=true`.

    Use this and not `note` whenever a reader who missed the line would be WRONG about what the Run
    produced — an abandoned axis, a limit reached, a batch voided, a phase skipped, a source read
    while it was still being written.

    `incomplete` is a separate field rather than a level because the two say different things:
    WARNING is "this looks wrong", `incomplete` is "the answer you are about to act on is smaller
    than it appears". The console filters on it independently of the level for exactly that reason,
    so a reader who raised the floor still sees every one of these.

    Name the AXIS or PHASE in `fields` where you can — that is what makes a LogsQL query find every
    Run in a window whose result was incomplete, which is the capability narration never had and the
    reason this change was worth making.
    """
    _logger().warning(redact(one_line(sentence)), extra=_extra({**fields, "incomplete": True}))


def _extra(fields: dict) -> dict:
    """Structured fields for the record, minus anything that would collide with `logging`'s own.

    A key like `message` or `name` on `extra` raises inside `logging` itself, which would turn a
    progress line into a crashed Run. Dropped rather than renamed: an author who hits this wants to
    know their field did not land, and a silently renamed key is worse than an absent one.
    """
    reserved = {"message", "asctime", "name", "msg", "args", "levelname", "levelno", "exc_info"}
    return {k: v for k, v in fields.items() if k not in reserved}


class Progress(TypedDict, total=False):
    """The three keys a GENERIC reader can position. Everything else is the author's.

    ── WHY THIS IS ALMOST EMPTY ────────────────────────────────────────────────────────────────

    It briefly carried `program`, and that was a category error: `program` is a BUG BOUNTY word.
    A registry monitor has a repository, a reddit scrape has a subreddit, a transcoder has a file,
    and none of them owe the framework a "program". A progress type that names one workspace's
    domain makes every other workspace either lie or go undescribed.

    So the framework reserves only what a generic pane can DRAW without knowing the domain:

        at       what this is working on RIGHT NOW — a URL, a host, a file, a repo. Universal
                 because "which one of the things am I on" is a question every actor has.
        done     units finished  ─┐ together they are a BAR. A reader needs a denominator and
        total    units in total  ─┘ there is no domain-free way to infer one.

    Anything else an author passes rides along verbatim and is rendered as a labelled value. That
    is the whole point: `progress(subreddit="askhistorians", posts=412)` and
    `progress(program="visa", withdrawn=3)` are both first-class, and neither needs this file to
    learn a new word.
    """

    at: str
    done: int
    total: int


def progress(**fields: Any) -> None:
    """Publish where this Run is, onto its Temporal Workflow Stream.

    OPEN BY DESIGN — `**fields: Any`, not a closed shape. See {@link Progress} for the three keys
    a generic reader can position and why there are only three. An author describes their own work
    in their own words:

        progress(phase="crawl", program=program, at=url, done=n, total=len(pages))   # bugbounty
        progress(subreddit=sub, posts=seen, done=n, total=len(subs))                 # scraping
        progress(repo=name, layers=k, at=digest)                                     # registry

    THE AUTHOR WRITES NOTHING ELSE — no `WorkflowStream()` in `__init__`, no `self.stream`, no
    private `_emit` copy-pasted into the next workflow. The only requirement is that the class
    inherits `kontra.KontraFlow`, for the reason that class documents.

    ── WHY NOT `note` ──────────────────────────────────────────────────────────────────────────

    `note` is a SENTENCE for a human reading afterwards; this is STATE for a machine drawing now.
    A pane cannot parse "crawl: page 12/26" back into a bar without a regex that breaks the moment
    somebody rewords the line — and rewording a log line is not supposed to be a breaking change.

    ── WHAT IS SWALLOWED, AND WHAT IS NOT ──────────────────────────────────────────────────────

    Only the two "there is nowhere to publish" cases: called outside a workflow, or an SDK without
    `workflow_streams`. Both are ordinary — a workflow under unit test has no Temporal context and
    must not fail for wanting to report progress.

    A `TypeError` from an unserialisable value is NOT swallowed: it is the author's bug, and a
    silently dropped beat is exactly the failure this module exists to stop.
    """
    try:
        from temporalio import workflow
    except ImportError:
        return
    try:
        inst = workflow.instance()
    except Exception:  # noqa: BLE001 - not in a workflow (a unit test, or an actor process)
        inst = None
    stream = getattr(inst, "_kontra_stream", None) if inst is not None else None
    if stream is None:
        # DEGRADED, NOT SILENT. Outside a workflow there is nowhere to publish and nothing is
        # wrong. Inside one it means the class does not inherit `kontra.KontraFlow`, and the author
        # WILL be looking at an empty pane wondering why — so the values still reach the log store.
        _logger().info(redact(one_line("progress " + " ".join(
            f"{k}={v}" for k, v in fields.items()))), extra=_extra(dict(fields)))
        return
    stream.topic(PROGRESS_TOPIC).publish(dict(fields))


class KontraFlow:
    """Inherit this to make `progress()` work. That is the whole contract.

    WHY A BASE CLASS RATHER THAN NOTHING AT ALL. The first design created the stream lazily on the
    first `progress()` call, so an author wrote nothing whatsoever — and the library rejects it:

        RuntimeError: WorkflowStream must be constructed directly from the workflow's
                      @workflow.init method, not from 'progress'.

    That rule is not arbitrary. The stream installs a signal handler, an update handler and a
    query handler, and Temporal requires the handler set to be fixed before the first activation
    so a signal arriving during replay lands on the same handlers it did originally. A stream
    conjured mid-run is a workflow whose history cannot be replayed.

    So SOMETHING has to happen at construction, and a base class is the smallest something: one
    word on the class line, no `__init__`, no import of `temporalio.contrib`, no `self.stream`
    threaded to the four places that report.

    An author who defines their own `__init__` must call `super().__init__()` — the ordinary rule
    for base classes, and `progress()` degrades to a log line rather than raising if they forget.
    """

    def __init__(self) -> None:
        try:
            from temporalio.contrib.workflow_streams import WorkflowStream
        except ImportError:
            return  # an SDK without streams: `progress` degrades to logging
        self._kontra_stream = WorkflowStream()


