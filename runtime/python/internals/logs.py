"""Run identity on every log record (ADR 0050 §1; kontra#16).

A LOG LINE IS ONLY AS USEFUL AS ITS ``run_id``. Journald gives ``machine`` and ``unit``; it cannot
say which **Run** a line belongs to, and "show me the logs for this run" is the question the console
needs to answer. This module puts the identity the engine already has onto the records the engine
already writes.

── WHY A FILTER AND A FORMATTER, AND NOT AN OTLP EXPORTER ──────────────────────────────────────────

The path already exists and it is journald: ``workflow.logger`` writes to the host's stdout, systemd
turns that into a journal entry, and ``vlagent`` ships journals (kontra#15). Adding an OTLP exporter
here would be a SECOND path off the Machine, with its own buffering, its own failure mode and its own
thing to be unreachable — beside one that already buffers to disk and is already installed.

So the identity travels as STRUCTURED JSON ON THE LINE, which vlagent parses into fields and
VictoriaLogs indexes. ``KONTRA_OTLP_ENDPOINT`` remains the seam for a deployment that wants records
delivered somewhere else: it is read here, and a value routes the records to that collector instead.
The store stays the operator's choice, which is the property that makes this commercial rather than a
house style — someone already running Datadog or SigNoz re-points one variable.

── ABSENT IS A RECORD WITHOUT THE LABEL, NEVER A RECORD DROPPED ────────────────────────────────────

A Worker polling with no Run in flight still logs, and those lines are exactly the ones that explain a
Worker that never picked anything up. The filter always returns ``True``; identity it does not have
is a key it does not write.
"""

from __future__ import annotations

import contextvars
import json
import logging
import os
from typing import Any, Dict, Optional

from internals import workerid

__all__ = [
    "RunIdentity",
    "bind_run",
    "bind_worker",
    "current_run",
    "IdentityFilter",
    "JsonFormatter",
    "configure_shipping",
    "otlp_endpoint",
    "temporal_context",
]


class RunIdentity(dict):
    """What a line belongs to. A plain dict so a caller can add fields we did not anticipate."""


#: The Run the current task is working on. A CONTEXTVAR rather than a global, because one actor host
#: runs many Sessions concurrently on one event loop — a global would label every line with whichever
#: Unit happened to bind last, which is the same lie `vmagent.env` exists to prevent one layer down.
_CURRENT: contextvars.ContextVar[Optional[RunIdentity]] = contextvars.ContextVar(
    "kontra_run_identity", default=None
)


def bind_run(**fields: Any) -> contextvars.Token:
    """Bind the Run identity for this task. Returns the token to reset with.

    Empty values are DROPPED rather than written as empty strings — `""` for a run id reads as "no
    run" where absent reads as "not recorded", and the console's own `tenantOf` fix is the same
    distinction one layer up.
    """
    clean = RunIdentity({k: v for k, v in fields.items() if v not in (None, "")})
    return _CURRENT.set(clean)


def current_run() -> Optional[RunIdentity]:
    return _CURRENT.get()


#: THE QUEUE THIS PROCESS BOOTED ON, for records written outside any Temporal context.
#:
#: A MODULE GLOBAL and not a contextvar, unlike the Run above, because it is a fact about the
#: PROCESS rather than about the task: one host boots on one queue and that never varies per
#: coroutine. Records written INSIDE an activity do better than this — `temporal_context` reads the
#: real queue off `activity.info()`, which on a Session's worker is the Session's own queue and not
#: this one. So this is the fallback for the lines that have no task: boot, shutdown, and the
#: polling-with-nothing-in-flight lines ADR 0050 §1 exists to keep.
_BOOT_QUEUE = ""


def bind_worker(queue: str) -> None:
    """Name the queue this process serves. Called once, at boot, by the host that knows it."""
    global _BOOT_QUEUE
    _BOOT_QUEUE = queue or ""


def temporal_context() -> Dict[str, Any]:
    """What TEMPORAL already knows about this record, asked of Temporal.

    ── WHY THIS IS READ RATHER THAN PASSED ─────────────────────────────────────────────────────────

    `bind_run` below carries the identity the ENGINE has — the Run, the node, the actor — because
    those are kontra's own concepts and the handler sends them in the batch payload. Everything in
    THIS function is Temporal's: the workflow id, the run id Temporal means by that phrase, the
    attempt, the task queue and the activity. Re-deriving those from a payload would be a second
    source for a fact the SDK holds authoritatively, and the failure mode is silent — a payload
    field that stops being sent leaves a label that is simply absent, with nothing to compare it to.

    ── THE SDK HAS A TODO WHERE THE WORKER SHOULD BE ───────────────────────────────────────────────

    `activity.Info._logger_details` and `workflow.Info._logger_details` are what
    `activity.logger` / `workflow.logger` put on every record, and BOTH carry the same comment:

        # TODO(cretz): worker ID?

    So the one field that answers "which Worker wrote this" is the one Temporal's own logger
    adapters do not include yet. That is the whole of what is added here on top of theirs — the
    identity is not invented, it is the string this process already passed to `Worker(identity=)`.

    ── IT NEVER RAISES ─────────────────────────────────────────────────────────────────────────────

    `activity.info()` and `workflow.info()` raise outside their contexts, which is most of the
    time: a boot line, a shipper, a unit test. A logging filter that raised would take the record
    AND the handler with it, at the exact moment something else is already going wrong.
    """
    out: Dict[str, Any] = {}
    queue = _BOOT_QUEUE

    try:  # noqa: SIM105 - the two contexts are asked separately; see below
        from temporalio import activity

        info = activity.info()
    except Exception:  # noqa: BLE001 - not in an activity, which is a normal state
        info = None
    if info is not None:
        queue = info.task_queue or queue
        out.update(
            {
                "task_queue": info.task_queue,
                "attempt": info.attempt,
                "activity_id": info.activity_id,
                "activity_type": info.activity_type,
                "workflow_id": info.workflow_id or "",
                "workflow_run_id": info.workflow_run_id or "",
                "workflow_type": info.workflow_type or "",
                "namespace": info.namespace,
            }
        )

    if info is None:
        # A WORKFLOW, WHICH IS A DIFFERENT CONTEXT AND NOT A FALLBACK OF THE FIRST. Asking for both
        # unconditionally would mean calling `workflow.info()` from inside an activity, where it
        # raises — cheap, but it makes the normal path go through an exception on every line.
        try:
            from temporalio import workflow as _wf

            winfo = _wf.info()
        except Exception:  # noqa: BLE001 - not in a workflow either
            winfo = None
        if winfo is not None:
            queue = winfo.task_queue or queue
            out.update(
                {
                    "task_queue": winfo.task_queue,
                    "attempt": winfo.attempt,
                    "workflow_id": winfo.workflow_id,
                    "workflow_run_id": winfo.run_id,
                    "workflow_type": winfo.workflow_type,
                    "namespace": winfo.namespace,
                    # IN A WORKFLOW, THE RUN IS THE WORKFLOW. The engine binds `run_id` per Unit
                    # inside an ACTIVITY, where a Run is a dispatch — but a workflow body has no
                    # such bind, and it is the thing every other surface calls a run: `kontra runs
                    # list` prints the workflow id under RUN-ID, the console routes `/runs/<that>`,
                    # and its log rail queries `run_id:"<that>"`. Leaving it unset is what left the
                    # rail empty for every workflow-emitted line; filling it from anywhere else
                    # would file those lines under an id no surface resolves.
                    "run_id": winfo.workflow_id,
                }
            )

    # THE WORKER, WHICH IS THE FIELD THIS FUNCTION EXISTS FOR. Derived from the queue Temporal just
    # named, so a line written on a Session's own worker says so — `worker_fields` composes the
    # identical string `Worker(identity=...)` was given, which is what lets an operator paste it
    # into `temporal task-queue describe` and get the process back.
    out.update(workerid.worker_fields(queue))
    return {k: v for k, v in out.items() if v not in (None, "")}


class IdentityFilter(logging.Filter):
    """Stamp the Run, the Temporal context and the Worker onto every record. Never drops one."""

    def filter(self, record: logging.LogRecord) -> bool:
        # TEMPORAL FIRST, THE ENGINE'S BIND SECOND, AND NEITHER CLOBBERS THE CALLER. Order matters
        # only where the two overlap, and they are written not to: Temporal owns `workflow_run_id`,
        # the engine owns kontra's `run_id`, and conflating them is how "which run" gets two
        # answers. A caller's own field on the record still beats both — see the loop below.
        for key, value in temporal_context().items():
            if not hasattr(record, key):
                setattr(record, key, value)
        ident = _CURRENT.get()
        if ident:
            for key, value in ident.items():
                # Never clobber a field the caller set explicitly on the record.
                if not hasattr(record, key):
                    setattr(record, key, value)
        return True


#: Fields `logging` puts on every record. Anything NOT here is something a caller (or the filter
#: above) added, and is worth shipping.
_STANDARD = frozenset(
    """args asctime created exc_info exc_text filename funcName levelname levelno lineno module
    msecs message msg name pathname process processName relativeCreated stack_info thread
    threadName taskName""".split()
)

#: WHAT TEMPORAL'S OWN LOGGER ADAPTERS PUT ON A RECORD, and what this formatter was silently
#: throwing away.
#:
#: `workflow.logger` and `activity.logger` are `LoggerAdapter`s that attach their context as a
#: NESTED DICT under these keys (`temporalio/workflow/_sandbox.py`, `temporalio/activity.py`). The
#: scalar test below — `isinstance(value, (str, int, float, bool))` — is exactly right for keeping
#: a stray object out of the line, and it dropped both of these on the floor: every author who
#: reached for the documented `workflow.logger` got a record carrying `workflow_id`, `attempt` and
#: `task_queue`, none of which survived to the store.
#:
#: So they are FLATTENED rather than excluded. One level deep, because that is how deep they are.
_TEMPORAL_EXTRAS = ("temporal_workflow", "temporal_activity")


class JsonFormatter(logging.Formatter):
    """One JSON object per line — what vlagent parses into fields.

    `_msg` and `_time` are VictoriaLogs' own names for the message and the timestamp, so a record
    needs no per-field mapping on the way in. Everything else rides as a plain key.

    IT NEVER RAISES. A formatter that throws takes the log line AND the handler with it, and the one
    time that happens is while something else is already going wrong.
    """

    def format(self, record: logging.LogRecord) -> str:
        out: Dict[str, Any] = {
            "_time": self.formatTime(record, "%Y-%m-%dT%H:%M:%S") + f".{int(record.msecs):03d}Z",
            "level": record.levelname.lower(),
            "logger": record.name,
        }
        try:
            out["_msg"] = record.getMessage()
        except Exception:  # noqa: BLE001 - see the docstring
            out["_msg"] = str(record.msg)
        for key, value in record.__dict__.items():
            if key in _STANDARD or key.startswith("_"):
                continue
            if key in _TEMPORAL_EXTRAS and isinstance(value, dict):
                # Temporal's context, one level deep — see `_TEMPORAL_EXTRAS`.
                #
                # `hasattr(record, sub)` AND NOT `sub not in out`: the adapter's extras are on the
                # record BEFORE the filter runs, so `out` ordering would make the nested copy win a
                # race it should lose. Asking the record instead is order-independent — a key the
                # filter already stamped (from `info()` directly) is skipped here and emitted by
                # the scalar branch below, which is the one derivation we want in the line.
                for sub, subvalue in value.items():
                    # TEMPORAL'S `run_id` IS NOT KONTRA'S, AND FLATTENING IT STOLE THE NAME.
                    #
                    # The adapter's nested context calls Temporal's workflow-run uuid `run_id`.
                    # Lifted one level as-is, it lands on the very field this module exists to
                    # stamp — and it wins, because the adapter's extras are on the record before
                    # anything else could claim it. Every workflow-emitted line was therefore
                    # filed under a uuid no kontra surface uses: `kontra runs list`, the run page
                    # and `/api/logs/query` all mean the WORKFLOW id by "run", so the run page's
                    # log rail queried `run_id:"campaign-…"` and matched nothing, for every
                    # workflow ever served. The rail was not broken; it was looking under the
                    # right name for a value filed under the wrong one.
                    #
                    # `temporal_context()` above already publishes the same value as
                    # `workflow_run_id`, which is where it belongs — so this renames rather than
                    # drops, and no fact is lost.
                    if sub == "run_id":
                        sub = "workflow_run_id"
                    if not hasattr(record, sub) and isinstance(subvalue, (str, int, float, bool)):
                        out[sub] = subvalue
                continue
            if isinstance(value, (str, int, float, bool)):
                out[key] = value
        if record.exc_info:
            out["error"] = self.formatException(record.exc_info)
        try:
            return json.dumps(out, default=str)
        except Exception:  # noqa: BLE001
            return json.dumps({"_msg": out.get("_msg", ""), "level": out["level"]})


def otlp_endpoint() -> str:
    """Where records are delivered when something other than journald should carry them.

    Configuration with kontra's own VictoriaLogs as the default (kontra#15), so the common case needs
    nothing set and an operator with their own collector changes one variable.
    """
    return os.environ.get("KONTRA_OTLP_ENDPOINT", "http://victorialogs:9428")


def configure_shipping(logger: Optional[logging.Logger] = None) -> bool:
    """Turn on identity-stamped structured logging. Returns whether it changed anything.

    OFF UNLESS ASKED, via `KONTRA_LOG_FORMAT=json`. A developer watching a tmux pane wants the human
    format `_configure_logging` sets; a fleet Machine writing to journald at volume wants this. The
    pane is the default because that is the surface somebody is looking at while they decide.
    """
    if os.environ.get("KONTRA_LOG_FORMAT", "").lower() != "json":
        return False
    root = logger or logging.getLogger()
    changed = False
    for handler in root.handlers:
        if not any(isinstance(f, IdentityFilter) for f in handler.filters):
            handler.addFilter(IdentityFilter())
            changed = True
        if not isinstance(handler.formatter, JsonFormatter):
            handler.setFormatter(JsonFormatter())
            changed = True
    return changed
