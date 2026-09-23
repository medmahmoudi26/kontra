"""`progress` and `KontraFlow` — STATE for a machine to draw, not sentences for a human to read.

── WHAT USED TO BE HERE, AND WHY IT IS NOT ─────────────────────────────────────────────────────────

`speak` went first (ADR 0050 §2): narration put AUTHORED SENTENCES in Temporal's history, costing ~5
events and ~1 second each against a 200-per-Run budget, for no property a log line lacks.

`note` and `partial` replaced it and have now gone the same way, one layer further down. They were
thin wrappers over the logger — `note` was `logger.info`, `partial` was `logger.warning` plus an
`incomplete=true` field — and a wrapper that adds one dict to a stdlib call is a second API to learn
for something the author already knows how to spell:

    workflow.logger.info(f"{n} hosts in {len(waves)} wave(s)")
    workflow.logger.warning("seed_limit reached — the crawl is PARTIAL", extra={"incomplete": True})

`incomplete` survives them and is still the field that matters: the console's logs rail filters on it
INDEPENDENTLY of the level, so raising the floor to `error` cannot hide a claim that the result is
smaller than it appears. It is now the author's to set, which is the same judgement `partial` asked
for, spelled where the reader can see it.

WHAT THE REMOVAL COST, AND WHERE IT IS OWED BACK. `note`/`partial` ran every sentence through
`redact(one_line(...))`. A bare `workflow.logger` call does not, so that guard belongs on the HANDLER
beside `IdentityFilter` (`runtime/python/internals/logs.py`) — which covers every record, including
the library lines and engine errors that never called `note` in the first place.

── WHY THIS MODULE STILL EXISTS ────────────────────────────────────────────────────────────────────

`progress` is NOT a log line and could not be replaced by one. A pane cannot parse "crawl: page
12/26" back into a bar without a regex that breaks the moment somebody rewords the sentence — and
rewording a log line is not supposed to be a breaking change. So progress is published as STRUCTURED
STATE onto the run's Temporal Workflow Stream, and `KontraFlow` is the one word on a class line that
makes that stream exist. See {@link Progress} for the three keys a generic reader can position.

── `ask` IS UNRELATED AND STILL BLOCKS ─────────────────────────────────────────────────────────────

`ask` was never a pair with any of this. It BLOCKS the workflow until a human answers, and neither a
log line nor a stream event can block a workflow.
"""

from __future__ import annotations

from typing import Any, TypedDict

from kontra.redaction import one_line, redact

__all__ = ["PROGRESS_TOPIC", "KontraFlow", "Progress", "progress"]


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


