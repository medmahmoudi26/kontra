"""The PYTHON ARMS of shared/conformance/queues.json — the caller's SDK and the actor host, both of them.

WHY THE CORPUS REPLACED THE TABLE THAT WAS HERE. This file used to hold a hand-copied dict of
`(name, version) -> queue` under the instruction "The Go peer is
handler/internal/identity/identity_test.go — keep the table in sync." Three such tables existed,
each keeping itself in sync with the others by somebody remembering, across four languages and
eight derivations of one string. The bookkeeping had already drifted before the code did: the
comment in `sdk/go/catalog` called itself "a SIXTH independent derivation", the one in
`backend/src/panels/pollers.ts` "a fourth", the one in `backend/src/nexusRegistry.ts` "THE FIFTH",
and no two of them were counting the same set. One of them named a peer file
(`backend/src/workflows/nexusService.ts`) that is not in the tree at all.

THE FAILURE MODE IS THIS FILE'S ORIGINAL SENTENCE and it has not changed: the actor registers,
polls a queue nobody schedules onto, and reports as a healthy idle Worker while every run hangs
until StartToClose. That is strictly worse than a mismatch that fails at activation, because
nothing about it looks wrong.

Two modules are asserted here and they are separate on purpose (the decoupling rule): the caller's
`actorkit.catalog`, which dispatches, and `internals.temporal.host`, which binds. Neither may
import the other, so the corpus is what makes them one contract.
"""
from __future__ import annotations

import json
import re
from pathlib import Path

import pytest

from actorkit import catalog
from internals.temporal.host import session_actor_id, session_task_queue, task_queue

ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "shared" / "conformance" / "queues.json"

CORPUS = json.loads(FIXTURE.read_text(encoding="utf-8"))
SHARED = CORPUS["shared"]["cases"]
SESSIONS = CORPUS["sessions"]["cases"]
SESSION = CORPUS["session"]["cases"]
ENDPOINT = CORPUS["endpoint"]["cases"]


def _ids(cases: list[dict]) -> list[str]:
    return [c["why"][:60] for c in cases]


def test_the_corpus_is_not_empty_and_still_carries_the_inputs_that_break() -> None:
    """A corpus that silently shrank to nothing passes every case below.

    The named inputs ARE the file: an empty version is the DIY/CLI path's whole contract, and a
    space and a non-ASCII rune are what separate an UNSANITISED queue name from a SANITISED
    endpoint name. A corpus of easy rows is exactly the guard this one replaced — ADR 0035 §1's
    finding, in a different contract.
    """
    assert len(SHARED) >= 6 and len(SESSIONS) >= 4 and len(SESSION) >= 3 and len(ENDPOINT) >= 10
    blob = json.dumps(CORPUS, ensure_ascii=False)
    for token in ("my actor", "café", "naïve", "\U0001f4e6", "-shared", "-sessions", "-s-"):
        assert token in blob, f"the corpus no longer exercises {token!r}"
    assert any(c["version"] == "" for c in SHARED), "the DIY/CLI path is gone from the corpus"


@pytest.mark.parametrize("case", SHARED, ids=_ids(SHARED))
def test_the_shared_queue_matches_the_corpus(case: dict) -> None:
    assert catalog.shared_queue(case["name"], case["version"]) == case["expect"]


@pytest.mark.parametrize("case", SESSIONS, ids=_ids(SESSIONS))
def test_the_sessions_queue_matches_the_corpus(case: dict) -> None:
    """Both Python derivations, the caller's and the host's.

    They are the two halves of the same failure: the caller dispatches onto this queue and the
    host binds it, so a divergence between these two lines alone is a batch that sits in a queue
    this very process is not polling.
    """
    assert catalog.sessions_queue(case["name"], case["version"]) == case["expect"]
    assert task_queue(case["name"], case["version"]) == case["expect"]


@pytest.mark.parametrize("case", SESSION, ids=_ids(SESSION))
def test_the_per_session_queue_matches_the_corpus(case: dict) -> None:
    """ADR 0023 §6, and the derivation whose drift is worst.

    The handler builds it from ITS task queue plus the Session id it was dispatched with, the host
    from (name, version) plus the id it was opened with. A mismatch is a scope whose Method calls
    sit in a queue nobody polls until ScheduleToStart, which reads exactly like a slow actor.
    """
    name, version, sid = case["name"], case["version"], case["session_id"]
    assert catalog.session_queue(name, version, sid) == case["expect"]
    assert session_task_queue(name, version, sid) == case["expect"]


def test_a_session_queue_needs_a_session_id() -> None:
    """The corpus's `refuses_without_a_session_id`, in Python's idiom.

    Raising is how this language says what Go says by returning the empty string. What both sides
    must agree on is that `{shared}-s-` is never produced: it is a real queue that outlives the
    scope, so every Session of that actor would share it — the pinning gone, and nothing failing.
    """
    r = CORPUS["session"]["refuses_without_a_session_id"]
    assert r["session_id"] == "", "the corpus's refusal row carries an id, so it proves nothing"
    with pytest.raises(ValueError):
        catalog.session_queue(r["name"], r["version"], r["session_id"])
    with pytest.raises(ValueError):
        session_task_queue(r["name"], r["version"], r["session_id"])


def test_the_two_session_planes_never_collide() -> None:
    """One Session's queue is polled by exactly one worker; the actor's sessions queue is polled by
    all of them. A scoped call that landed on the plural runs against a process that never
    activated the Session, holds none of its state, and REPORTS SUCCESS."""
    for case in SESSION:
        name, version, sid = case["name"], case["version"], case["session_id"]
        assert catalog.session_queue(name, version, sid) != catalog.sessions_queue(name, version)


@pytest.mark.parametrize("case", ENDPOINT, ids=_ids(ENDPOINT))
def test_the_endpoint_name_matches_the_corpus(case: dict) -> None:
    assert catalog.endpoint_name(case["name"], case["version"]) == case["expect"]


def test_every_endpoint_name_in_the_corpus_is_servable() -> None:
    """The pattern is the corpus's, not this file's: the cluster enforces it, and a name that
    fails it is refused at create time with a message about the NAME rather than about the
    missing version."""
    servable = re.compile(CORPUS["endpoint"]["servable"])
    for case in ENDPOINT:
        assert servable.match(case["expect"]), case["why"]


def test_a_queue_name_is_not_sanitised_and_an_endpoint_name_is() -> None:
    """THE PROPERTY BEHIND THE ADVERSARIAL ROWS, asserted directly so a future maintainer who
    shares one sanitiser between the two rules reads one line instead of six mismatched strings.

    Temporal accepts a space in a task queue name; the Nexus endpoint registry does not. Cleaning
    up the queue would route to a queue nobody polls, which is silent; leaving the endpoint dirty
    fails at create time, which is loud. Opposite rules, one pair of inputs.
    """
    assert catalog.shared_queue("my actor", "0.1.0") == "my actor-0.1.0"
    assert " " not in catalog.endpoint_name("my actor", "0.1.0")


def test_a_key_is_the_actor_a_session_activates_and_a_bare_scope_is_private() -> None:
    """Congruent with handler/workflow.go's actor-id derivation for a scoped dispatch: the
    idempotency key first (ADR 0022 — that is what makes `object_state` addressable and shared),
    then the Session id, which gives an unkeyed scope a private identity of its own. Keying is a
    claim on a shared identity, never a tax on an ordinary dispatch (ADR 0023 §10).

    NOT IN THE CORPUS, and deliberately: this is a rule about which instance ONE dispatch
    activates, not a name two processes must spell the same, so there is nothing for a second
    language to drift against.
    """
    assert session_actor_id("3f9a", "acme.com") == "acme.com"
    assert session_actor_id("3f9a", "") == "3f9a"
    assert session_actor_id("3f9a") == "3f9a"
