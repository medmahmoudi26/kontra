"""`progress` — the Batch in flight, as a fact instead of a sentence.

── WHY THIS FILE IS `facts.py` AND NOT `progress.py` ───────────────────────────────────────────────

A submodule and a verb cannot share a name. `kontra/__init__` resolves `progress` lazily through
`__getattr__`, and `__getattr__` runs only when NORMAL attribute lookup fails — but importing
`kontra.progress` makes Python bind that submodule onto the parent package, after which
`kontra.progress` is the module and the verb is unreachable. It is order-dependent, so it would have
worked in most programs and broken in the one that imported the module first. `hitl` never hit this
because its verb is `ask`; this one would have.


── WHY THIS VERB IS BACK, AND WHAT IS DIFFERENT ────────────────────────────────────────────────────

`kontra/__init__.py` removed a `progress` verb and said exactly when it could return: *"They were not
redundant with a log line — they published TYPED STATE, which a sentence genuinely cannot carry. What
they published onto was a Temporal Workflow Stream, which lives in the WORKFLOW'S MEMORY and dies
with the workflow… They come back when there is a durable store under them."*

There is one, and there always was — it is the one the same paragraph recommends four lines earlier:
*"A LOG LINE costs no history, is not capped, and reaches the log store where it can be queried
across runs — and it is still there tomorrow."* A record's `extra=` fields are not prose. The
formatter in `runtime/python/internals/logs.py` writes every scalar on the record as its own JSON
key, vlagent parses those into fields, and VictoriaLogs indexes them under a retention that has
nothing to do with Temporal's. So typed state already had a durable store under it; what it did not
have was a declaration.

This is therefore NOT the old verb restored. The old one owned a transport and died with it. This one
owns a SCHEMA and borrows the transport that already outlives the Run.

── THE SCHEMA WAS ALREADY IN PRODUCTION, SPELLED BY HAND ───────────────────────────────────────────

MEASURED across the six caller workflows: 68 hand-built `logger` calls, carrying

    incomplete × 27     axis × 27     phase × 20     program × 16     seed_limit × 2     lane × 1

Nothing declared these, nothing validated them, and every author respelled them. The console's log
rail nevertheless FILTERS ON `incomplete` independently of level — so a schema that no code defines
is being read by a surface that had to guess it. The field names below are those names, unchanged,
because the point is to declare what is already true rather than to introduce a second vocabulary.

── WHAT IT WILL NOT DO ─────────────────────────────────────────────────────────────────────────────

It never raises. A progress fact is an aside; a call that gets its own arguments wrong must not be
the thing that fails a Run that is otherwise working. Bad values are dropped and the rest is still
emitted, which keeps the useful half of a careless call.

It does not replace `workflow.logger`. Prose is still the right medium for what does not fit a
field, and the existing lines stay. An author who emits nothing behaves exactly as they do today.
"""

from __future__ import annotations

from typing import Any, Dict, Optional

__all__ = ["progress"]


def _int(value: Any) -> Optional[int]:
    """An `int`, or nothing. `bool` is excluded deliberately — `done=True` is a mistake, and
    `int(True)` is `1`, which would render as real progress."""
    if isinstance(value, bool) or value is None:
        return None
    try:
        n = int(value)
    except (TypeError, ValueError):
        return None
    return n if n >= 0 else None


def _logger_and_program() -> tuple[Any, Optional[str]]:
    """The workflow's logger and its type name, or the module logger outside a workflow.

    IMPORTED INSIDE THE FUNCTION, not at module scope, and that is the same rule `ask` follows one
    file up: `kontra/__init__` is imported inside the Temporal workflow sandbox, and a top-level
    `temporalio` import here would put that dependency behind a plain `import kontra` for the CLI,
    the loaders and the tests — none of which are workflows.

    Outside a workflow this still works and still writes the same fields, so an ACTOR can report its
    own progress on the same schema. That is not the main case; refusing it would just mean an
    author writes the f-string again.
    """
    try:
        from temporalio import workflow  # noqa: PLC0415 - see the docstring

        if workflow.in_workflow():
            program: Optional[str] = None
            try:
                program = workflow.info().workflow_type
            except Exception:  # noqa: BLE001 - a fact we can do without, never a failure
                program = None
            return workflow.logger, program
    except Exception:  # noqa: BLE001 - temporalio absent, or not a workflow context
        pass

    import logging  # noqa: PLC0415

    return logging.getLogger("kontra.progress"), None


def progress(
    phase: str,
    axis: str,
    *,
    done: Any = None,
    total: Any = None,
    program: Optional[str] = None,
    incomplete: bool = False,
    detail: Optional[str] = None,
    **fields: Any,
) -> None:
    """Report where a **Run** has got to, once, as typed fields.

        progress("crawl", "pages", done=12, total=380, program="acme")
        progress("crawl", "pages", total=380, program="acme")     # the denominator, up front
        progress("scan", "hosts", done=9, total=40, incomplete=True, program="acme")

    `phase` is what the Run is doing (`crawl`, `scan`, `fold`); `axis` is what is being counted
    (`pages`, `hosts`, `seeds`). Both ride as their own fields, so the console can group by them
    rather than parse them back out of a sentence.

    THE DENOMINATOR IS MEANT TO ARRIVE BEFORE THE WORK. `total` with no `done` is a complete and
    deliberate call — it is how a reader who opens the rail at second zero learns how big the thing
    is. `surface/workflow.py` already does this by hand, one line above the loop.

    `incomplete=True` says THE RESULT IS NOT WHAT A READER WOULD ASSUME — an abandoned axis, a limit
    reached, a phase skipped. It raises the record to WARNING and, more importantly, keeps reaching
    the rail's existing `incomplete` filter, which works independently of level so that raising the
    floor cannot hide it. The question at the call site is the one `__init__` already asks: would a
    reader be wrong about the result if they missed this?

    `program` is stamped on every fact, because a campaign runs several at once and a reader cannot
    join a line to a workflow by eye. Omitted, it falls back to the workflow's own type name, so the
    field is present even when the author has no program concept.

    Extra keyword arguments ride as their own fields — `lane=2`, `seed_limit=500` — which is what
    the hand-built lines were already doing inside their `extra=` dicts.

    NEVER RAISES, and carries no credential: this reaches the log store, which is no more private
    than history is.
    """
    try:
        logger, wf_type = _logger_and_program()

        out: Dict[str, Any] = {}
        if isinstance(phase, str) and phase:
            out["phase"] = phase
        if isinstance(axis, str) and axis:
            out["axis"] = axis

        n_done = _int(done)
        n_total = _int(total)
        if n_done is not None:
            out["done"] = n_done
        if n_total is not None:
            out["total"] = n_total

        name = program if isinstance(program, str) and program else wf_type
        if name:
            out["program"] = name
        # WRITTEN ONLY WHEN TRUE. `incomplete: false` on every ordinary line would triple the
        # cardinality of the one field the rail filters on, to say nothing — and the filter asks
        # whether the key is set, so a false would have to be excluded there instead.
        if incomplete:
            out["incomplete"] = True

        # The author's own fields — `lane=2`, `seed_limit=500` — which is what the hand-built lines
        # were already putting in their `extra=` dicts.
        #
        # NO GUARD AGAINST SHADOWING A DECLARED FIELD, because Python is already the guard: every
        # declared name is a real parameter, so `progress(..., total=2, **{"total": 999})` is
        # `TypeError: got multiple values for keyword argument 'total'` before this function runs.
        # An earlier version filtered `key in _DECLARED` here; that branch was unreachable.
        #
        # SCALARS ONLY, because the formatter emits scalars only — a dict or a list would be built
        # here, dropped there, and look like a field that silently never arrives.
        for key, value in fields.items():
            if isinstance(value, (str, int, float, bool)):
                out[key] = value

        # `extra=` IS THE POINT. The sentence is what a person reads; `out` is what the formatter
        # turns into indexed fields, and it is the half that makes this a fact rather than a line.
        # `%s` with one argument rather than an f-string in the message position, so a value
        # containing a percent sign cannot break `record.getMessage()`.
        emit = logger.warning if incomplete else logger.info
        emit("%s", _sentence(out, detail), extra=out)
    except Exception:  # noqa: BLE001 - see the module docstring: an aside never fails a Run
        return


def _sentence(out: Dict[str, Any], detail: Optional[str]) -> str:
    """The human-readable half.

    A fact still has to READ, because the rail is a list of lines before it is a set of fields, and
    the surfaces that consume the fields are not the only thing anybody opens. Derived rather than
    composed by the author — which is the whole ergonomic point — and deliberately close to the
    shape the hand-built lines already had, so the rail does not visibly change spelling on the day
    this lands.
    """
    head = f"[{out['program']}] " if "program" in out else ""
    what = "/".join(x for x in (out.get("phase"), out.get("axis")) if x)
    if "done" in out and "total" in out:
        count = f": {out['done']}/{out['total']}"
    elif "done" in out:
        count = f": {out['done']}"
    elif "total" in out:
        # The announcement. Said as a quantity rather than as `0/380`, because "none done yet" and
        # "this is how big it is" are different claims and only the second one is being made.
        count = f": {out['total']} to do"
    else:
        count = ""
    tail = f" — {detail}" if isinstance(detail, str) and detail else ""
    mark = " — INCOMPLETE" if out.get("incomplete") else ""
    return f"{head}{what}{count}{mark}{tail}"
