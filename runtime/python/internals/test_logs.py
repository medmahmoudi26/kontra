"""What a correlated log record must and must not do (ADR 0050 §1, kontra#16)."""

from __future__ import annotations

import json
import logging
import os

import pytest

from internals import logs


@pytest.fixture(autouse=True)
def _clean_identity():
    token = logs.bind_run()
    yield
    logs._CURRENT.reset(token)


def _emit(logger_name="kontra.test", level=logging.INFO, msg="hello", **extra) -> dict:
    """One record through the filter and the formatter, as a dict."""
    record = logging.LogRecord(logger_name, level, __file__, 1, msg, None, None)
    for k, v in extra.items():
        setattr(record, k, v)
    assert logs.IdentityFilter().filter(record) is True
    return json.loads(logs.JsonFormatter().format(record))


def test_a_bound_run_lands_on_the_record():
    logs.bind_run(run_id="hunt-1789", node_id="n3", actor="desync", actor_version="1.0.0")
    out = _emit()
    assert out["run_id"] == "hunt-1789"
    assert out["node_id"] == "n3"
    assert out["actor"] == "desync"
    assert out["actor_version"] == "1.0.0"
    assert out["_msg"] == "hello"
    assert out["level"] == "info"


def test_absent_identity_is_a_record_without_the_label_not_a_dropped_record():
    """A Worker polling with NO Run in flight still logs, and those lines are exactly the ones that
    explain a Worker which never picked anything up. The acceptance criterion is explicit that an
    absent `_run_id` must not cost the record."""
    out = _emit(msg="polling kontra-desync-1-0-0, no work")
    assert "run_id" not in out
    assert out["_msg"] == "polling kontra-desync-1-0-0, no work"


def test_an_empty_run_id_is_dropped_rather_than_written_as_empty():
    # `""` reads as "no run"; absent reads as "not recorded". The console's `tenantOf` fix is the
    # same distinction one layer up, and conflating them is what made every tenant read as blank.
    logs.bind_run(run_id="", node_id="n1")
    out = _emit()
    assert "run_id" not in out
    assert out["node_id"] == "n1"


def test_identity_does_not_clobber_a_field_the_caller_set():
    logs.bind_run(actor="desync")
    out = _emit(actor="explicitly-mine")
    assert out["actor"] == "explicitly-mine"


def test_one_json_object_per_line_with_victorialogs_own_field_names():
    logs.bind_run(run_id="r1")
    line = logs.JsonFormatter().format(
        logging.LogRecord("kontra.test", logging.WARNING, __file__, 1, "partial", None, None)
    )
    assert "\n" not in line
    out = json.loads(line)
    # `_msg` and `_time` are what VictoriaLogs calls these, so no per-field mapping is needed on
    # the way in.
    assert out["_msg"] == "partial"
    assert out["_time"].endswith("Z")
    assert out["level"] == "warning"


def test_the_formatter_never_raises_on_a_bad_record():
    """A formatter that throws takes the log line AND the handler with it — and the one time that
    happens is while something else is already going wrong."""

    class Hostile:
        def __str__(self):
            raise RuntimeError("nope")

    record = logging.LogRecord("kontra.test", logging.ERROR, __file__, 1, "%s", (Hostile(),), None)
    line = logs.JsonFormatter().format(record)  # must not raise
    assert json.loads(line)["level"] == "error"


def test_the_endpoint_is_configuration_with_kontras_own_store_as_the_default():
    prev = os.environ.pop("KONTRA_OTLP_ENDPOINT", None)
    try:
        assert logs.otlp_endpoint() == "http://victorialogs:9428"
        os.environ["KONTRA_OTLP_ENDPOINT"] = "http://collector.internal:4318"
        assert logs.otlp_endpoint() == "http://collector.internal:4318"
    finally:
        os.environ.pop("KONTRA_OTLP_ENDPOINT", None)
        if prev is not None:
            os.environ["KONTRA_OTLP_ENDPOINT"] = prev


def test_shipping_is_off_unless_asked():
    prev = os.environ.pop("KONTRA_LOG_FORMAT", None)
    try:
        logger = logging.getLogger("kontra.test.shipping")
        logger.addHandler(logging.NullHandler())
        assert logs.configure_shipping(logger) is False, "a tmux pane must keep the human format"
        os.environ["KONTRA_LOG_FORMAT"] = "json"
        assert logs.configure_shipping(logger) is True
        assert any(isinstance(h.formatter, logs.JsonFormatter) for h in logger.handlers)
        # Idempotent: configuring twice must not stack filters on the handler.
        logs.configure_shipping(logger)
        for h in logger.handlers:
            assert sum(isinstance(f, logs.IdentityFilter) for f in h.filters) <= 1
    finally:
        os.environ.pop("KONTRA_LOG_FORMAT", None)
        if prev is not None:
            os.environ["KONTRA_LOG_FORMAT"] = prev


def test_identity_is_per_task_not_per_process():
    """One actor host runs many Sessions on one event loop. A module global would label every line
    with whichever Unit bound last — the same lie `vmagent.env` exists to prevent one layer down."""
    import asyncio

    async def one(run_id: str) -> str:
        logs.bind_run(run_id=run_id)
        await asyncio.sleep(0)
        got = _emit()
        return got["run_id"]

    async def both():
        return await asyncio.gather(one("run-a"), one("run-b"))

    assert asyncio.run(both()) == ["run-a", "run-b"]
