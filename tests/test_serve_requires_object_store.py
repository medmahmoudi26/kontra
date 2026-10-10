"""S3 is mandatory for an actor that commits Units (owner decision A8, ADR 0060).

The actor used to start without `KONTRA_S3_ENDPOINT` and degrade: records collected inline, no
commit objects, and every retry or re-dispatch quietly re-running the whole Batch. It now refuses to
start, BEFORE anything connects, and says which variable is missing. These drive the real
`serve_async` with everything that would leave the process faked out, so what is asserted is the
order of the boot, not a helper in isolation.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest


class _Registry:
    def __init__(self, actor_dir: Path, methods) -> None:
        self.actor_name = "enrich"
        self.version = "0.3.0"
        self.actor_dir = actor_dir
        self.methods = methods


def _serve(monkeypatch, registry) -> dict:
    from internals import metrics
    from internals.temporal import connect as kconnect
    from internals.temporal import host

    seen: dict = {"connected": False}

    async def fake_connect(queue, **_):
        seen["connected"] = True
        return object()

    class _Worker:
        async def run(self):
            return None

    monkeypatch.setattr(kconnect, "connect", fake_connect)
    monkeypatch.setattr(kconnect, "actor_worker", lambda *a, **k: _Worker())
    monkeypatch.setattr(metrics, "serve", lambda *a, **k: None)
    monkeypatch.setattr(host, "publish_catalog", lambda *a, **k: None)
    monkeypatch.setattr(host, "_install_blob_reader", lambda *a, **k: None)
    asyncio.run(host.serve_async(registry, address="x:1", namespace="ns"))
    return seen


def test_an_actor_with_a_method_and_no_object_store_does_not_start(monkeypatch, tmp_path: Path):
    monkeypatch.delenv("KONTRA_S3_ENDPOINT", raising=False)
    connected = []
    with pytest.raises(RuntimeError, match="KONTRA_S3_ENDPOINT") as e:
        connected.append(_serve(monkeypatch, _Registry(tmp_path, {"scan": object()})))
    assert "enrich" in str(e.value), "the refusal names the actor it refused"
    assert connected == [], "refused before anything connected"


def test_with_an_object_store_configured_it_starts(monkeypatch, tmp_path: Path):
    monkeypatch.setenv("KONTRA_S3_ENDPOINT", "http://seaweed:8333")
    assert _serve(monkeypatch, _Registry(tmp_path, {"scan": object()}))["connected"] is True


def test_an_actor_that_declares_no_method_commits_nothing_and_is_not_refused(monkeypatch, tmp_path: Path):
    """A load-only actor's Batch passes through: there is no Unit outcome to make durable."""
    monkeypatch.delenv("KONTRA_S3_ENDPOINT", raising=False)
    assert _serve(monkeypatch, _Registry(tmp_path, {}))["connected"] is True
