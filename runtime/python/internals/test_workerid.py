"""Which Worker is this — the shape, and the two things it must never do.

EVERY EXPECTATION IS A HAND-WRITTEN LITERAL rather than a re-derivation. Asserting the identity
against an f-string built the same way would assert the implementation against itself: the suite
would stay green through a separator change, which is the one change that breaks
`shared/core/src/queues.ts::identityHost` and every poller listing that reads it.
"""

from __future__ import annotations

import os

import pytest

from internals import workerid


@pytest.fixture(autouse=True)
def _clean_env(monkeypatch):
    """The module reads the environment; a test that inherited the operator's would be a test whose
    result depends on whose laptop it ran on."""
    for var in ("KONTRA_BUNDLE_SHA", "KONTRA_ACTOR_VERSION", "KONTRA_WORKER_ROLE"):
        monkeypatch.delenv(var, raising=False)


def test_the_identity_is_pid_at_host_at_queue(monkeypatch):
    monkeypatch.setattr(workerid, "MACHINE", "kf-dns-01")
    monkeypatch.setattr(os, "getpid", lambda: 11)
    assert workerid.worker_identity("nscheck-0.1.0") == "11@kf-dns-01@nscheck-0.1.0"


def test_an_empty_queue_keeps_the_trailing_separator(monkeypatch):
    """What the Go SDK writes for a CLIENT, which polls nothing.

    Dropping the field would produce `11@kf-dns-01` — legal, parseable, and a second shape for a
    parser that already has one. The trailing `@` costs a character and removes a branch.
    """
    monkeypatch.setattr(workerid, "MACHINE", "kf-dns-01")
    monkeypatch.setattr(os, "getpid", lambda: 11)
    assert workerid.worker_identity("") == "11@kf-dns-01@"


def test_the_bundle_digest_is_the_build_id(monkeypatch):
    monkeypatch.setenv("KONTRA_BUNDLE_SHA", "9f2c4e" * 10)
    monkeypatch.setenv("KONTRA_ACTOR_VERSION", "0.3.1")
    # THE DIGEST WINS. A version names a release; a digest names the bytes, and the Machine has
    # already verified them — so when both are present the stronger answer is the one to send.
    assert workerid.build_id() == "9f2c4e" * 10


def test_the_version_is_the_fallback_and_absent_is_none(monkeypatch):
    monkeypatch.setenv("KONTRA_ACTOR_VERSION", "0.3.1")
    assert workerid.build_id() == "0.3.1"

    monkeypatch.delenv("KONTRA_ACTOR_VERSION")
    # NOT "" AND NOT "unknown". `None` is what makes the SDK apply its own default, which is the
    # right behaviour in a checkout where there is no Bundle and therefore no honest build id.
    # A sentinel string would be a label asserting something false about every dev run.
    assert workerid.build_id() is None


def test_empty_fields_are_omitted_not_written_blank(monkeypatch):
    """The same rule `logs.bind_run` follows: absent reads as "not recorded", `""` reads as
    "recorded as nothing", and those are different facts about a Worker."""
    monkeypatch.setattr(workerid, "MACHINE", "main-droplet")
    monkeypatch.setattr(workerid, "ROLE", "")
    monkeypatch.setattr(os, "getpid", lambda: 7)

    fields = workerid.worker_fields("desync-0.3.1-sessions")

    assert fields == {
        "worker": "7@main-droplet@desync-0.3.1-sessions",
        "machine": "main-droplet",
        "queue": "desync-0.3.1-sessions",
    }
    assert "role" not in fields
    assert "build_id" not in fields


def test_the_worker_field_is_byte_identical_to_the_identity(monkeypatch):
    """THE PROPERTY THE WHOLE THING RESTS ON.

    An operator who finds a suspicious log line filters `DescribeTaskQueue` on that exact string.
    If `worker_fields` derived the label differently from what `Worker(identity=...)` was given,
    that filter would return nothing and the line would look like it came from a Worker that does
    not exist.
    """
    monkeypatch.setattr(workerid, "MACHINE", "kf-desync-01")
    monkeypatch.setattr(workerid, "ROLE", "actor")
    monkeypatch.setattr(os, "getpid", lambda: 4147627)

    queue = "desync-0.3.1-s-01J8Z"
    assert workerid.worker_fields(queue)["worker"] == workerid.worker_identity(queue)
    assert workerid.worker_fields(queue)["worker"] == "4147627@kf-desync-01@desync-0.3.1-s-01J8Z"
