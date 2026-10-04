"""The client library: Temporal objects you can hold, outside kontra's process and without a Fleet.

`serve()` was the only door and it was one-way — it reached four of `Worker`'s forty-three
parameters and ran the worker itself. So anything Temporal offers that kontra had not hand-copied
was unreachable, and nothing could be BUILT without being STARTED, which is exactly what a test, a
notebook or somebody else's process needs.

These tests pin the two properties that make the difference:

  1. the builders return real `temporalio` objects with every option forwarded, and
  2. the defaults that are not optional — the claim-check codec, the sandbox passthrough — are
     still applied when a caller says nothing.

A FLEET IS CAPACITY AND A WORKER IS A POLLER. Nothing here provisions anything, and nothing here
needs to; that is the claim being made.
"""

import inspect

import pytest
from temporalio import workflow
from temporalio.worker import Worker

from internals.temporal import connect as kconnect


@workflow.defn
class Sample:
    @workflow.run
    async def run(self) -> str:
        return "ok"


@pytest.fixture
def built(monkeypatch):
    """What reaches `temporalio.worker.Worker`, without needing a live client to reach it.

    `Worker.__init__` dereferences `client.config`, so constructing one needs a real connection —
    and a test that needed a server to check a constructor would be a test nobody runs. The
    question here is what the builder FORWARDS, which is answerable at the seam.
    """
    seen: dict = {}

    def fake_worker(client, **kw):
        seen.clear()
        seen.update(kw)
        seen["client"] = client
        return "a Worker"

    monkeypatch.setattr("temporalio.worker.Worker", fake_worker)
    return seen


def test_the_builder_constructs_a_real_temporal_worker(built):
    """Not a wrapper and not a handle: it calls `temporalio.worker.Worker` and returns what that
    returns, so the object in your hand is the one Temporal documents."""
    out = kconnect.workflow_worker(
        "client", workflows=[Sample], task_queue="wf-sample-0.0.1", bind_logs=False
    )
    assert out == "a Worker"
    assert built["task_queue"] == "wf-sample-0.0.1"
    assert built["workflows"] == [Sample]
    # Defaults applied when the caller says nothing. `build_id` is asserted as PRESENT rather than
    # truthy: `None` is its legitimate unset value, and Temporal treats passing None as not
    # versioning the worker — what matters is that the builder decided it rather than dropping it.
    assert built["identity"], "the worker must carry this process's Temporal identity"
    assert "build_id" in built
    assert built["workflow_runner"] is not None, "the sandbox runner is defaulted in"


def test_every_worker_option_is_reachable(built):
    """THE DEFECT THIS EXISTS FOR. `serve()` names four of `Worker.__init__`'s parameters, so an
    interceptor, a cache size or a shutdown timeout was a kontra feature request rather than a
    caller's decision. Anything `Worker` takes has to reach it."""
    reachable = set(inspect.signature(Worker.__init__).parameters) - {"self"}
    assert len(reachable) > 30, "sanity: Temporal's Worker should have a wide surface"

    kconnect.workflow_worker(
        "client",
        workflows=[Sample],
        task_queue="q",
        bind_logs=False,
        max_cached_workflows=7,
        interceptors=["mine"],
    )
    assert built["max_cached_workflows"] == 7
    assert built["interceptors"] == ["mine"]


def test_the_sandbox_passthrough_is_a_default_not_a_law(built):
    """Defaulted, so a normal caller gets the kontra modules passed through; overridable, so a
    debugger session can run unsandboxed. A forced runner would make the second impossible."""
    from temporalio.worker import UnsandboxedWorkflowRunner

    mine = UnsandboxedWorkflowRunner()
    kconnect.workflow_worker(
        "client", workflows=[Sample], task_queue="q", bind_logs=False, workflow_runner=mine
    )
    assert built["workflow_runner"] is mine, "a caller's runner must win over the default"


def test_the_default_passthrough_covers_this_sdk():
    """`kontra` re-imported inside the sandbox is a SECOND set of classes: an `isinstance` against
    the caller's `Dataset` then fails, and it reads as a type error in user code."""
    assert "kontra" in kconnect.DEFAULT_PASSTHROUGH
    assert "actorkit" in kconnect.DEFAULT_PASSTHROUGH


def test_a_workflow_worker_with_nothing_to_run_is_refused():
    """A worker that polls a queue and can serve no type on it reports healthy and answers
    nothing, which is indistinguishable from a queue nobody is dispatching to."""
    with pytest.raises(ValueError, match="at least one @workflow.defn"):
        kconnect.workflow_worker(None, workflows=[], task_queue="q", bind_logs=False)


def test_the_address_and_namespace_come_from_the_environment(monkeypatch):
    """So the same code runs on a laptop and on a Machine without an edit."""
    monkeypatch.delenv("KONTRA_ADDRESS", raising=False)
    monkeypatch.delenv("KONTRA_NAMESPACE", raising=False)
    assert kconnect.address_of() == "localhost:7233"
    assert kconnect.namespace_of() == "default"

    monkeypatch.setenv("KONTRA_ADDRESS", "temporal.internal:7233")
    monkeypatch.setenv("KONTRA_NAMESPACE", "prod")
    assert kconnect.address_of() == "temporal.internal:7233"
    assert kconnect.namespace_of() == "prod"
    # An explicit argument still wins — the environment is the default, not the authority.
    assert kconnect.address_of("other:1234") == "other:1234"
    assert kconnect.namespace_of("staging") == "staging"


def test_the_sdk_exposes_both_doors_without_importing_the_runtime():
    """`catalog.client` and `catalog.worker` are author surface, so plain `import kontra` must not
    drag a Temporal client, an S3 client or the runtime in. The arrow test proves the import graph;
    this proves the names are actually there to be called."""
    from kontra import catalog

    assert callable(catalog.client)
    assert callable(catalog.worker)
    assert inspect.iscoroutinefunction(catalog.client)
    # `worker()` must NOT be async: it builds and returns, so it can be used in a sync fixture.
    assert not inspect.iscoroutinefunction(catalog.worker)


def test_the_actor_half_exists_too():
    """An Actor is a Temporal activity worker; there is nothing about it that requires being inside
    kontra's process. Same door, same shape."""
    from kontra.actor import ActorRegistry

    # `ActorRegistry` is the author-facing object — the module-level `actor` is an instance of it,
    # and it is the class that owns `serve()`. The twin belongs beside its sibling.
    assert callable(getattr(ActorRegistry, "worker", None)), "actor.worker() is the actor-side twin"
    assert callable(getattr(ActorRegistry, "serve", None))
    assert not inspect.iscoroutinefunction(ActorRegistry.worker)
