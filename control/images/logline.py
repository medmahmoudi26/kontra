"""One container log line -> one VictoriaLogs record. The parsing half of `logship.sh`.

── WHY IT IS A FILE AND NOT A HEREDOC ──────────────────────────────────────────────────────────────

This was forty lines of Python inside a shell function inside a compose entrypoint. Nothing could
import it, so nothing tested it, and the failure mode of a logging component is that it produces
exactly what a working one produces when nothing logged: an empty rail. `tests/test_logline.py`
runs this module directly, which is only possible because it is a module.

── THE LINE IS PARSED, NOT GUESSED AT ──────────────────────────────────────────────────────────────

The version this replaces recovered the run id from the message TEXT with a regular expression and
inferred the level by looking for the substring "ERROR". Both were reconstructing structure thrown
away one layer up — the engine holds the run id (`internals/logs.py::bind_run`) and the logger
holds the level. It did that because nothing ever set `KONTRA_LOG_FORMAT=json`;
`cli/warden/warden_ca.go::workerDefaults` and the Machine's env file now do.

Three shapes arrive and all three are RECOGNISED rather than sniffed at. A TEXT line gets no run
id: a wrong correlation is worse than an absent one, because a line attributed to the wrong Run is
evidence pointing at an innocent execution.
"""

from __future__ import annotations

import datetime
import json
import sys

#: pino writes the level as a NUMBER. Its own constants, not a guess at a range.
PINO_LEVELS = {10: "trace", 20: "debug", 30: "info", 40: "warn", 50: "error", 60: "fatal"}

#: THE STREAM FIELDS ARE THE SHIPPER'S, AND A RECORD MAY NOT SET THEM.
#:
#: Everything else on a structured record wins over what is derived from the container — an actor
#: host knows its own run, level and timestamp better than a shipper reading a container name does.
#: These four are the exception, and the reason is cardinality rather than authority: VictoriaLogs
#: indexes a distinct STREAM per distinct combination, so one emitter putting a per-Session value in
#: `actor` mints a stream per Session. That is an index-wide cost paid for one process's mistake,
#: and fixing the emitter afterwards does not recover it.
#:
#: It is not hypothetical. `engine.py` bound `actor=self._actor_id` — a session id — into this exact
#: field until the change that added this comment.
STREAM_FIELDS = ("tenant", "actor", "machine", "unit")


def rfc3339(ms: float) -> str:
    """Epoch milliseconds -> what VictoriaLogs parses as `_time`."""
    dt = datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc)
    return dt.isoformat().replace("+00:00", "Z")


def now() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")


def container_fields(tenant: str, actor: str, machine: str, unit: str, worker_label: str) -> dict:
    """What the SHIPPER knows: the tenant it serves and the container it is reading.

    `worker_label` is the Warden's own `KONTRA_WORKER` label, `<name>/<version>/<part>` — split
    rather than re-derived from the container name. It is what tells a reader whether a line came
    from the actor half or the handler half of one Worker, which the name cannot say.
    """
    base = {"tenant": tenant, "actor": actor, "machine": machine, "unit": unit}
    bits = worker_label.split("/") if worker_label else []
    if len(bits) == 3:
        base["actor"], base["actor_version"], base["part"] = bits
    return base


def parse(raw: str, base: dict, clock=now) -> dict:
    """One line -> the record to ingest. Never raises.

    `clock` is injected so a test can assert the timestamp rule rather than assert around it.
    """

    def merge(doc: dict) -> dict:
        """The record's fields over the container's — except the stream fields, which are ours."""
        out = {**base, **doc}
        out.update({k: v for k, v in base.items() if k in STREAM_FIELDS})
        out["structured"] = True
        return out

    try:
        doc = json.loads(raw)
        if not isinstance(doc, dict):
            raise ValueError("not an object")
    except Exception:  # noqa: BLE001 - a text line is ordinary, not an error
        return {
            **base,
            "_time": clock(),
            "ship_time": True,
            "level": "info",
            "structured": False,
            "msg": raw,
        }

    if "_msg" in doc and "_time" in doc:
        # KONTRA'S OWN FORMAT (`internals/logs.py::JsonFormatter`). The emitter already stamped the
        # run, the worker, the level and the time, so this is a pass-through.
        out = merge(doc)
        out["msg"] = doc.get("_msg", "")
        out.pop("_msg", None)
        return out

    if "msg" in doc and isinstance(doc.get("level"), int):
        # PINO, which is what `server.ts` builds Fastify with.
        out = merge(doc)
        out["level"] = PINO_LEVELS.get(doc["level"], "info")
        stamped = isinstance(doc.get("time"), (int, float))
        out["_time"] = rfc3339(doc["time"]) if stamped else clock()
        if not stamped:
            out["ship_time"] = True
        out.pop("time", None)
        return out

    # JSON, but not a shape this knows. Keep every field — it is already structured, and dropping
    # it to text would lose more than guessing at its schema would gain.
    out = merge(doc)
    out.setdefault("msg", doc.get("message", raw))
    out.setdefault("level", "info")
    out["_time"] = clock()
    out["ship_time"] = True
    return out


def main(argv: list[str], stdin, stdout) -> int:
    base = container_fields(*argv[1:6])
    for line in stdin:
        line = line.rstrip("\n")
        if not line:
            continue
        try:
            stdout.write(json.dumps(parse(line, base)) + "\n")
        except Exception:  # noqa: BLE001
            # A SHIPPER THAT DIES ON ONE LINE SHIPS NOTHING AFTER IT. One unrepresentable record is
            # a record lost; an exception out of this loop is every record after it lost, silently.
            continue
        stdout.flush()
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv, sys.stdin, sys.stdout))
