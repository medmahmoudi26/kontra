"""An actor fetching its OWN secret, authenticated as itself (issue 19).

Against a REAL HTTP server rather than a patched `urlopen`, because the things worth pinning here
are on the wire: the path, the bearer header carrying the worker's identity, and what each refusal
status means. A monkeypatched transport would assert that the code calls the function it calls.

THE PEER IS `control/orchestrator/src/secrets/routes.test.ts`, which pins the same path and the same four
statuses from the other side. Neither test can see the other's process, so the contract is written
twice on purpose — if one moves, the other fails.
"""

from __future__ import annotations

import asyncio
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from actorkit import secrets
from actorkit.retry import NonRetryableError

SENTINEL = "dop_v1_SENTINEL_never_in_the_clear_9f3c"


class _Store:
    """The bit of the orchestrator this SDK talks to, and nothing else."""

    def __init__(self) -> None:
        self.status = 200
        self.value = SENTINEL
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
            self.wfile.write(
                json.dumps({"name": body["name"], "version": 1, "value": state.value}).encode()
            )

        def log_message(self, *_args):  # keep pytest output readable
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", f"http://127.0.0.1:{server.server_port}")
    monkeypatch.setenv("KONTRA_ACTOR_TOKEN", "kai1.pretend.token")
    yield state
    server.shutdown()


def test_fetches_its_own_secret_over_the_documented_route(store):
    assert secrets.get_sync("shodan-key") == SENTINEL
    assert store.calls[0]["path"] == "/api/secrets/resolve"
    assert store.calls[0]["body"] == {"name": "shodan-key"}
    # AUTHENTICATED AS ITSELF: the worker's identity rides on the request, or it is nobody.
    assert store.auth[0] == "Bearer kai1.pretend.token"


def test_pins_a_version_when_asked(store):
    secrets.get_sync("shodan-key", version=2)
    assert store.calls[0]["body"] == {"name": "shodan-key", "version": 2}


def test_the_async_form_is_the_one_an_actor_load_uses(store):
    # `@actor.load` is async and holds the loop the host heartbeats on, so the blocking call runs
    # in a thread. Awaiting it must return the same value. `asyncio.run` rather than a plugin
    # marker: this suite has no pytest-asyncio, and one test is not a reason to add a dependency.
    assert asyncio.run(secrets.get("shodan-key")) == SENTINEL


def test_does_not_cache_so_a_rotation_takes_effect(store):
    assert secrets.get_sync("shodan-key") == SENTINEL
    store.value = "rotated-value"
    # A process-lifetime cache would keep a REVOKED value alive on every worker that had loaded it,
    # which is the state revocation exists to end.
    assert secrets.get_sync("shodan-key") == "rotated-value"
    assert len(store.calls) == 2


@pytest.mark.parametrize(
    ("status", "expected"),
    [
        (401, "expired"),
        (403, "does not belong to this actor"),
        (404, "no secret by that name"),
        (410, "REVOKED"),
    ],
)
def test_every_refusal_says_what_to_do_about_it(store, status, expected):
    store.status = status
    with pytest.raises(secrets.SecretUnavailable) as e:
        secrets.get_sync("shodan-key")
    assert expected in str(e.value)
    assert "shodan-key" in str(e.value)


def test_a_refusal_is_terminal_rather_than_a_retry_loop(store):
    # MEASURED: one bad credential in a retrying load produced 202 authentication POSTs and a green
    # empty run. A secret that cannot be resolved is permanent until a human acts.
    store.status = 403
    with pytest.raises(NonRetryableError):
        secrets.get_sync("shodan-key")


def test_says_which_variable_is_missing_rather_than_failing_obscurely(monkeypatch):
    monkeypatch.delenv("KONTRA_ORCHESTRATOR_URL", raising=False)
    monkeypatch.setenv("KONTRA_ACTOR_TOKEN", "kai1.pretend.token")
    with pytest.raises(secrets.SecretUnavailable, match="KONTRA_ORCHESTRATOR_URL"):
        secrets.get_sync("shodan-key")

    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://127.0.0.1:1")
    monkeypatch.delenv("KONTRA_ACTOR_TOKEN", raising=False)
    with pytest.raises(secrets.SecretUnavailable, match="no identity"):
        secrets.get_sync("shodan-key")


def test_an_unreachable_store_names_the_store_and_not_the_request(monkeypatch):
    monkeypatch.setenv("KONTRA_ORCHESTRATOR_URL", "http://127.0.0.1:1")
    monkeypatch.setenv("KONTRA_ACTOR_TOKEN", f"kai1.{SENTINEL}.token")
    with pytest.raises(secrets.SecretUnavailable) as e:
        secrets.get_sync("shodan-key")
    # `from None` on every raise: an exception chain prints the urllib request, whose repr carries
    # the headers — which is where the identity token is.
    assert SENTINEL not in str(e.value)
    assert e.value.__cause__ is None
    assert "unreachable" in str(e.value)


def test_the_value_is_in_no_exception_the_module_can_raise(store):
    # The one thing a "secret cannot be used" error must never do is quote the secret.
    store.status = 500
    store.value = SENTINEL
    with pytest.raises(secrets.SecretUnavailable) as e:
        secrets.get_sync("shodan-key")
    assert SENTINEL not in str(e.value)


def test_it_is_not_the_stdlib_secrets_module():
    # `from actorkit import secrets` must not shadow the stdlib for anything else in the process.
    import secrets as stdlib_secrets

    assert stdlib_secrets is not secrets
    assert hasattr(stdlib_secrets, "token_hex")
    assert os.path.basename(secrets.__file__) == "secrets.py"
