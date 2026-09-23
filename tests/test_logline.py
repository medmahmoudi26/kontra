"""What the log shipper may and may not infer from a line (`control/images/logline.py`).

THIS SUITE EXISTS BECAUSE THE THING IT TESTS USED TO BE UNTESTABLE. The parser was a heredoc inside
a shell function inside a compose entrypoint; nothing could import it, so nothing ran it, and the
failure mode of a logging component is silence — an empty rail is indistinguishable from a Run that
logged nothing, which is the confusion `routes/logs.ts::unreachable` was written to prevent.
"""

from __future__ import annotations

import importlib.util
import io
import json
from pathlib import Path

import pytest

_MODULE = Path(__file__).resolve().parents[1] / "control" / "images" / "logline.py"


def _load():
    """Import the shipper's parser from its path — it is a mounted file, not a package."""
    spec = importlib.util.spec_from_file_location("logline", _MODULE)
    assert spec and spec.loader, _MODULE
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


logline = _load()

FROZEN = "2026-09-21T20:00:00.000Z"


@pytest.fixture
def base():
    return logline.container_fields(
        "default", "desync", "kontra-desync-01", "kontra-desync-01", "desync/0.3.1/actor"
    )


def _parse(raw, base):
    return logline.parse(raw, base, clock=lambda: FROZEN)


# ── THE LABEL, NOT THE CONTAINER NAME ───────────────────────────────────────────────────────────


def test_the_warden_label_names_the_actor_its_version_and_its_half(base):
    """`kontra-([a-z0-9]+)` guessed the actor from the container name — a convention, not a fact,
    and one that says nothing about which HALF of a Worker a line came from."""
    assert base["actor"] == "desync"
    assert base["actor_version"] == "0.3.1"
    assert base["part"] == "actor"


def test_a_container_with_no_worker_label_still_ships(base):
    """A control-plane container carries `kontra.logs=true` and no `KONTRA_WORKER`. It must ship
    with the fields it does have rather than be skipped for the ones it does not."""
    got = logline.container_fields("default", "kontra-api", "kontra-api", "kontra-api", "")
    assert got["actor"] == "kontra-api"
    assert "actor_version" not in got
    assert "part" not in got


# ── KONTRA'S OWN FORMAT IS A PASS-THROUGH ───────────────────────────────────────────────────────


def test_the_emitters_run_id_worker_level_and_time_are_kept(base):
    """THE POINT OF THE REWRITE. All four of these used to be re-derived or invented by the
    shipper: the run id by regex over the message, the level by substring, the time by the
    shipper's own clock, and the worker not at all."""
    raw = json.dumps(
        {
            "_time": "2026-09-21T19:58:13.412Z",
            "_msg": "seed_limit 200 reached — the crawl is PARTIAL by request",
            "level": "warn",
            "run_id": "hunt-1789865677",
            "worker": "4147627@kf-desync-01@desync-0.3.1-sessions",
            "incomplete": True,
        }
    )
    out = _parse(raw, base)

    assert out["run_id"] == "hunt-1789865677"
    assert out["worker"] == "4147627@kf-desync-01@desync-0.3.1-sessions"
    assert out["level"] == "warn"
    assert out["_time"] == "2026-09-21T19:58:13.412Z"
    assert out["incomplete"] is True
    assert out["structured"] is True
    # `_msg` is VictoriaLogs' name on the way OUT of the emitter; the ingest URL names `msg`.
    assert out["msg"].startswith("seed_limit 200 reached")
    assert "_msg" not in out
    # The emitter stamped the time, so the record must NOT be marked as shipper-stamped.
    assert "ship_time" not in out


def test_a_record_cannot_set_a_stream_field(base):
    """CARDINALITY, NOT AUTHORITY.

    VictoriaLogs indexes a distinct stream per distinct combination of the stream fields, so an
    emitter putting a per-Session value in `actor` mints a stream per Session — an index-wide cost
    paid for one process's mistake, which fixing that process afterwards does not recover.

    `engine.py` bound `actor=self._actor_id` into exactly this field until recently, which is why
    this is a test and not a comment.
    """
    raw = json.dumps(
        {
            "_time": "2026-09-21T19:58:13.412Z",
            "_msg": "x",
            "actor": "c5eaf2b6a275",
            "machine": "somewhere-else",
            "tenant": "another-tenant",
        }
    )
    out = _parse(raw, base)

    assert out["actor"] == "desync"
    assert out["machine"] == "kontra-desync-01"
    assert out["tenant"] == "default"


# ── PINO, THE ORCHESTRATOR'S OWN LOGGER ─────────────────────────────────────────────────────────


def test_pinos_numeric_level_becomes_a_word_the_rail_can_filter(base):
    out = _parse(json.dumps({"level": 50, "time": 1789865677123, "msg": "query: unauthorized"}), base)
    assert out["level"] == "error"
    assert out["msg"] == "query: unauthorized"
    assert out["structured"] is True


def test_pinos_epoch_milliseconds_become_the_records_time(base):
    out = _parse(json.dumps({"level": 30, "time": 1789865677123, "msg": "up"}), base)
    # The EMITTER's clock, not the shipper's — so the rail orders by when things happened.
    assert out["_time"].startswith("2026-")
    assert out["_time"].endswith("Z")
    assert "ship_time" not in out
    assert "time" not in out, "pino's raw epoch must not ride along beside `_time`"


def test_a_pino_line_with_no_time_is_marked_as_shipper_stamped(base):
    out = _parse(json.dumps({"level": 30, "msg": "up"}), base)
    assert out["_time"] == FROZEN
    assert out["ship_time"] is True


# ── TEXT ────────────────────────────────────────────────────────────────────────────────────────


def test_a_text_line_ships_and_is_marked_unstructured(base):
    out = _parse("[host] desync@0.3.1 -> queue @ temporal:7233", base)
    assert out["msg"] == "[host] desync@0.3.1 -> queue @ temporal:7233"
    assert out["structured"] is False
    assert out["ship_time"] is True
    assert out["level"] == "info"


def test_no_run_id_is_invented_from_the_message_text(base):
    """THE REGEX IS GONE AND MUST NOT COME BACK.

    `\\b([a-z][a-z0-9_]*-\\d{9,})\\b` matched anything of that shape anywhere in a line — a URL, a
    quoted id from another run, an error message naming a workflow that failed. A line attributed
    to the wrong Run is evidence pointing at an innocent execution, which is worse than a line with
    no Run at all. The fix for a text line is one environment variable at the emitter.
    """
    out = _parse("finished crawl for hunt-1789865677 and starting webcrawl-1789865999", base)
    assert "run_id" not in out


def test_a_level_is_not_inferred_from_the_message_text(base):
    """The old shipper read `"ERROR" in msg`, so a line that MENTIONED an error was one."""
    out = _parse("recovered from ERROR state, continuing normally", base)
    assert out["level"] == "info"


# ── THE SHAPES NOBODY PLANNED FOR ───────────────────────────────────────────────────────────────


def test_unknown_json_keeps_its_fields_rather_than_being_flattened_to_text(base):
    out = _parse(json.dumps({"event": "scrape", "status": 200}), base)
    assert out["event"] == "scrape"
    assert out["status"] == 200
    assert out["structured"] is True


def test_a_json_array_is_a_text_line_not_a_record(base):
    out = _parse('[1, 2, 3]', base)
    assert out["structured"] is False
    assert out["msg"] == "[1, 2, 3]"


def test_one_bad_line_does_not_stop_the_stream():
    """A shipper that dies on one line ships nothing after it — and says nothing about either."""
    stdin = io.StringIO(
        "\n".join(
            [
                json.dumps({"_time": "2026-09-21T19:58:13.412Z", "_msg": "first"}),
                "",  # blank lines are skipped, not shipped
                "a plain line",
                json.dumps({"_time": "2026-09-21T19:58:14.412Z", "_msg": "last"}),
            ]
        )
        + "\n"
    )
    stdout = io.StringIO()
    rc = logline.main(
        ["logline.py", "default", "desync", "kontra-desync-01", "kontra-desync-01", ""],
        stdin,
        stdout,
    )
    assert rc == 0
    lines = [json.loads(line) for line in stdout.getvalue().splitlines()]
    assert [line["msg"] for line in lines] == ["first", "a plain line", "last"]
