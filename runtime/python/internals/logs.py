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

__all__ = [
    "RunIdentity",
    "bind_run",
    "current_run",
    "IdentityFilter",
    "JsonFormatter",
    "configure_shipping",
    "otlp_endpoint",
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


class IdentityFilter(logging.Filter):
    """Stamp the bound Run identity onto every record. Never drops one."""

    def filter(self, record: logging.LogRecord) -> bool:
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
