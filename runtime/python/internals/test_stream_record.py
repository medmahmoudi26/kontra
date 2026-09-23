"""What a stream record carries — the Python arm of `runtime/go/temporalhost/stream_test.go`.

THE TWO HOSTS PUBLISH THROUGH ENTIRELY DIFFERENT CODE. Python binds a contextvar and awaits a
coroutine; Go binds a func on the Session and hands the record to a buffering client. So a green
Go suite was never evidence that a Python actor's records carry the same keys — which is the
argument `workspaces/scraping/actors/gocanary` exists to make one layer up, and this file makes
one layer down. Every assertion here has a named peer in the Go file.
"""

from __future__ import annotations

import dataclasses

import pytest

from internals import engine, workerid


def test_the_record_carries_the_session_id_not_the_actor_name():
    """Peer: TestTheRecordCarriesTheSessionIDNotTheActorName.

    `actor` means the SESSION's id in both languages. The Go host sent `h.name` once, so one run's
    stream carried the same key meaning two different things depending on which leg wrote it. The
    name is not lost by this — it is in the TOPIC, which is where a subscriber reads it from.
    """
    body = engine._stream_body("node-1", "c5eaf2b6a275", "", {"label": "unit-3"})
    assert body["actor"] == "c5eaf2b6a275"
    assert body["node"] == "node-1"
    assert body["label"] == "unit-3"


def test_the_record_names_the_worker_that_wrote_it():
    """Peer: TestTheRecordNamesTheWorkerThatWroteIt."""
    worker = "4147627@kf-desync-01@desync-0.3.1-sessions"
    body = engine._stream_body("n1", "s1", worker, {"label": "unit-3"})
    assert body["worker"] == worker
    # `node` says which unit of work; `worker` says which box to go and look at. When six nodes
    # are healthy and one Machine is sick, those are different questions.
    assert body["node"] == "n1"
    assert body["actor"] == "s1"


def test_the_worker_field_is_the_identity_temporal_was_given(monkeypatch):
    """Peer: TestTheWorkerFieldIsTheIdentityTemporalWasGiven.

    An operator who finds a record pastes `worker` into `temporal task-queue describe` and must
    get this process back. A second derivation that agrees today is still a second derivation.
    """
    monkeypatch.setattr(workerid, "MACHINE", "kf-desync-01")
    queue = "desync-0.3.1-s-01J8Z"
    body = engine._stream_body("n1", "s1", workerid.worker_identity(queue), {})
    assert body["worker"] == workerid.worker_identity(queue)
    assert body["worker"].endswith("@" + queue)


def test_an_unknown_worker_is_omitted_rather_than_written_empty():
    """Peer: TestAnUnknownWorkerIsOmittedRatherThanWrittenEmpty.

    A record written outside an activity has no Worker to name, and `""` would read as one that
    declined to say. Same rule as `logs.bind_run` one module over.
    """
    body = engine._stream_body("n1", "s1", "", {"label": "unit-3"})
    assert "worker" not in body


@pytest.mark.parametrize("field", ["node", "actor", "worker"])
def test_an_authors_own_field_is_not_overwritten(field):
    """Peers: TestAnAuthorsOwnNodeFieldIsNotOverwritten, TestAnAuthorsOwnWorkerFieldIsNotOverwritten.

    A record that genuinely carries `node` — a scheduler reporting which worker it PLACED
    something on — means it, and having the engine overwrite it with whichever worker happens to
    be publishing would be a lie the author cannot see or prevent.
    """
    body = engine._stream_body("n1", "s1", "11@host@queue", {field: "the-authors-own"})
    assert body[field] == "the-authors-own"


def test_a_dataclass_flattens_to_its_fields():
    """Peer: TestAStructFlattensToItsJSONTags.

    An author's declared type IS the record, so it must flatten to exactly the keys their
    `streams=` schema describes — plus the engine's routing keys and nothing else.
    """

    @dataclasses.dataclass
    class CrawlProgress:
        at: str
        contexts: int

    body = engine._stream_body("n1", "s1", "", CrawlProgress(at="https://example.com", contexts=2))
    assert body == {"at": "https://example.com", "contexts": 2, "node": "n1", "actor": "s1"}


def test_a_non_object_record_is_kept_under_value():
    """Peer: TestANonObjectRecordIsKeptUnderValue.

    `await stream(f"fetched {url}")` is a natural first thing to try given the verb's name.
    Returning the framework's routing keys and nothing else would ship a record with the author's
    value silently gone.
    """
    body = engine._stream_body("n1", "s1", "", "fetched https://example.com")
    assert body["value"] == "fetched https://example.com"
