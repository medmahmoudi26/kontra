"""An actor DECLARING a credential slot, and resolving it through the operator's binding (issue 20).

Against a REAL HTTP server for `test_actor_secrets.py`'s reason: what is worth pinning is on the
wire — the path, the bearer identity, the actor VERSION that rides with the ask (a slot is declared
per version), and what each refusal status means to an author reading a traceback.

THE PEER IS `backend/src/secrets/slotRoutes.test.ts`, which pins the same path and the same
statuses from the other side. Neither process can see the other, so the contract is written twice
on purpose — if one moves, the other fails.
"""

from __future__ import annotations

import asyncio
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from actorkit import secrets
from actorkit.actor import ActorRegistry, Slot
from actorkit.retry import NonRetryableError

SENTINEL = "sk_live_SENTINEL_never_in_a_traceback_71c4"


class _Store:
    """The bit of the orchestrator this SDK talks to, and nothing else."""

    def __init__(self) -> None:
        self.status = 200
        self.value = SENTINEL
        self.url = ""
        self.calls: list[dict] = []
        self.auth: list[str | None] = []


@pytest.fixture()
def store(monkeypatch):
    state = _Store()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802 — BaseHTTPRequestHandler's spelling
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            state.calls.append({"path": self.path, "body": body})
            state.auth.append(self.headers.get("Authorization"))
            if state.status != 200:
                self.send_response(state.status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"error": "refused"}).encode())
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            # THE ANSWER CARRIES NO SECRET NAME, which is the property the indirection exists for:
            # the actor's author must not learn the operator's inventory by asking for what they
            # were granted. The route really answers this shape.
            self.wfile.write(json.dumps({"slot": body.get("slot"), "value": state.value}).encode())

        def log_message(self, *_args):  # keep pytest output readable
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    state.url = f"http://127.0.0.1:{server.server_port}"
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", state.url)
    monkeypatch.setenv("KONTRA_ACTOR_TOKEN", "kai1.pretend.token")
    yield state
    server.shutdown()


def registry(version: str = "0.2.0") -> ActorRegistry:
    """A fresh registry — the module singleton is process-global and shared with every other test."""
    r = ActorRegistry()
    r.actor_name = "probe"
    r.version = version
    return r


# --- declaring ------------------------------------------------------------------------------


def test_declaring_records_the_name_and_the_authors_sentence():
    r = registry()
    slot = r.slot("api_key", "the vendor key this actor calls with")
    assert isinstance(slot, Slot)
    assert list(r.slots) == ["api_key"]
    assert r.slots["api_key"].description == "the vendor key this actor calls with"


def test_declaring_the_same_slot_twice_is_the_same_slot():
    r = registry()
    r.slot("api_key", "the vendor key")
    r.slot("api_key")
    assert list(r.slots) == ["api_key"]
    assert r.slots["api_key"].description == "the vendor key"


def test_two_different_sentences_for_one_slot_raise():
    # One of them is what the operator reads when they decide what to grant. Keeping either
    # silently makes the page lie about the other.
    r = registry()
    r.slot("api_key", "the vendor key")
    with pytest.raises(TypeError, match="different descriptions"):
        r.slot("api_key", "something else entirely")


def test_the_handle_reads_the_version_at_CALL_time_not_at_declaration():
    # `actor.slot(...)` runs at import; the version is read out of actor.json later, in serve().
    # A handle that snapshotted it would resolve against "" forever.
    r = ActorRegistry()
    slot = r.slot("api_key")
    assert slot.version == ""
    r.version = "0.3.0"
    assert slot.version == "0.3.0"


def test_what_crosses_the_wire_is_names_and_sentences():
    import internals.catalog as catalog

    r = registry()
    r.slot("api_key", "the vendor key")
    r.slot("webhook_secret")
    assert catalog.slots_of(r) == [
        {"name": "api_key", "description": "the vendor key"},
        {"name": "webhook_secret"},
    ]


# --- resolving ------------------------------------------------------------------------------


def test_resolving_sends_the_slot_the_version_and_the_run(store):
    r = registry("0.2.0")
    slot = r.slot("api_key")
    assert slot.get_sync(run="nscheck-17") == SENTINEL

    call = store.calls[0]
    assert call["path"] == "/api/slots/resolve"
    # THE VERSION IS NOT OPTIONAL on this wire: a declaration is per version, and a request that
    # did not say which version is asking could only be checked against the union of all of them.
    assert call["body"] == {"slot": "api_key", "version": "0.2.0", "run": "nscheck-17"}
    assert store.auth[0] == "Bearer kai1.pretend.token"


def test_the_run_is_omitted_rather_than_sent_empty(store):
    registry().slot("api_key").get_sync()
    assert "run" not in store.calls[0]["body"]


def test_the_async_form_is_the_same_call(store):
    assert asyncio.run(registry().slot("api_key").get()) == SENTINEL
    assert store.calls[0]["path"] == "/api/slots/resolve"


def test_an_actor_with_no_version_is_refused_locally(store):
    # The fix is actor.json, not the credential — so it is said here, before a round trip that
    # would come back as a slot problem.
    r = ActorRegistry()
    with pytest.raises(secrets.SecretUnavailable, match="declared per version"):
        r.slot("api_key").get_sync()
    assert store.calls == []


def test_no_identity_is_a_local_refusal_naming_the_variable(store, monkeypatch):
    monkeypatch.delenv("KONTRA_ACTOR_TOKEN")
    with pytest.raises(secrets.SecretUnavailable, match="KONTRA_ACTOR_TOKEN"):
        registry().slot("api_key").get_sync()


# --- the refusals an author actually hits ------------------------------------------------------


@pytest.mark.parametrize(
    "status,expected",
    [
        (401, "identity was rejected or has expired"),
        # 403 and 409 are OPPOSITE problems and the two easiest to collapse into one "no":
        # 403 is the actor asking for something it never declared — a code fix;
        # 409 is the operator not having granted it — a console fix.
        (403, "does not DECLARE it"),
        (409, "BOUND NOTHING to it"),
        (404, "no longer exists"),
        (410, "REVOKED"),
    ],
)
def test_each_refusal_names_the_act_that_fixes_it(store, status, expected):
    store.status = status
    with pytest.raises(secrets.SecretUnavailable, match=expected):
        registry().slot("api_key").get_sync()


def test_a_refusal_is_never_retryable(store):
    # MEASURED, and it is why (see the module docstring of actorkit/secrets.py): one bad credential
    # in a loop that retried produced 202 authentication POSTs and a green empty run.
    store.status = 409
    with pytest.raises(NonRetryableError):
        registry().slot("api_key").get_sync()


def test_an_unreachable_orchestrator_never_prints_the_request(store, monkeypatch):
    # A urllib request object's repr carries its headers, which is where the identity token is.
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://127.0.0.1:1")
    try:
        registry().slot("api_key").get_sync()
    except secrets.SecretUnavailable as e:
        # `from None` clears __cause__ and SUPPRESSES the implicit context — the printed traceback
        # is what matters, and it is the one that would have carried the Authorization header.
        assert e.__cause__ is None and e.__suppress_context__ is True
        assert "kai1.pretend.token" not in str(e)
    else:  # pragma: no cover
        raise AssertionError("expected SecretUnavailable")


# --- declaring over the wire -------------------------------------------------------------------


def test_declaring_posts_to_the_slot_route(store):
    status = secrets.declare(
        store.url, "probe", "0.2.0", [{"name": "api_key", "description": "the vendor key"}]
    )
    assert status == 200
    assert store.calls[0]["path"] == "/api/slots/declare"
    assert store.calls[0]["body"] == {
        "actor": "probe",
        "version": "0.2.0",
        "slots": [{"name": "api_key", "description": "the vendor key"}],
    }
    # NO OPERATOR TOKEN. A declaration grants nothing — it says what an actor will ask for, and the
    # answer to that ask is the operator's binding.
    assert store.auth[0] is None


def test_serving_prints_the_slots_and_does_not_refuse_to_serve(store, capsys):
    # An unbound slot at serve time is the ordinary first state of every actor that needs a
    # credential: the operator cannot bind what has not been declared yet. Refusing here would be a
    # deadlock with a good excuse — the refusal belongs at the run.
    import internals.catalog as catalog

    r = registry()
    r.slot("api_key", "the vendor key")
    catalog.publish_slots(store.url, r, "probe", "0.2.0")
    said = capsys.readouterr().out
    assert "api_key" in said and "slots/declare" in said


def test_a_slotless_actor_posts_nothing(store):
    import internals.catalog as catalog

    catalog.publish_slots("http://127.0.0.1:1", registry(), "probe", "0.2.0")
    assert store.calls == []
