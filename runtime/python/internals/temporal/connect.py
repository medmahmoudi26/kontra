"""The Temporal Client and Worker this repository builds — as objects, not as a process.

── WHY THIS MODULE EXISTS ──────────────────────────────────────────────────────────────────────

`wfhost.serve_workflows_async` and `host.serve_async` each connected a Client and constructed a
Worker, with the same five pieces of wiring written twice and a third partial copy in
`_spawn_session_worker`. Two of those pieces are not optional and not obvious:

  * `casstore.data_converter()` — the claim-check codec. Without it everything works until a
    payload passes 128 KiB, then dies with `Unknown payload encoding binary/claim-check-v1`. A
    demo passes and a real run does not.
  * `SandboxedWorkflowRunner(... with_passthrough_modules("kontra", "actorkit"))` — the workflow
    sandbox blocks this SDK's own imports otherwise.

The rest — TLS, Temporal's three-field identity on BOTH the client and the worker, `build_id` —
is the kind of thing that is correct in the copy somebody read and absent from the copy somebody
wrote.

── AND WHY IT RETURNS OBJECTS ──────────────────────────────────────────────────────────────────

`serve()` used to be the only door, and it was a one-way one: it reached four of `Worker`'s
forty-three parameters and ran the worker itself, so anything Temporal offers that kontra had not
hand-copied was unreachable, and nothing could be built without being started.

These builders return the REAL `temporalio` objects with `**kwargs` forwarded. That buys three
things kontra cannot otherwise give an author: every Worker option works, Temporal's own
documentation applies to the thing in your hand, and a Client or a Worker can be constructed in a
test, a notebook or somebody else's process — **with or without a Fleet**, because a Worker polling
a queue is not a Fleet and never was.
"""

from __future__ import annotations

import os
from typing import Any, Sequence

from internals import casstore, logs, workerid
from internals.temporal.tlsconfig import connect_tls

#: Modules the workflow sandbox must NOT re-import in its own namespace. The author's SDK and the
#: actor kit are passed through because a sandboxed copy of them is a second set of classes: an
#: `isinstance` against the caller's `Dataset` fails, and the failure reads as a type error in
#: user code.
DEFAULT_PASSTHROUGH = ("kontra", "actorkit")


def address_of(address: str = "") -> str:
    return address or os.environ.get("KONTRA_ADDRESS", "localhost:7233")


def namespace_of(namespace: str = "") -> str:
    return namespace or os.environ.get("KONTRA_NAMESPACE", "default")


async def connect(
    task_queue: str,
    *,
    address: str = "",
    namespace: str = "",
    **kwargs: Any,
):
    """A connected `temporalio.client.Client`, wired the way everything here needs it.

    `task_queue` is not what the client polls — a Client polls nothing. It names the IDENTITY this
    process answers to, and the client carries it as well as the Worker because the two record
    different halves of the same story: the client's identity is on the calls this process MAKES
    (starting a workflow, completing an activity), the Worker's on the tasks it TAKES. Left to the
    default, half of what a Machine did is attributed to `<pid>@<hostname>` and the other half to
    the worker identity — one process wearing two names in one Run's history.

    `**kwargs` reaches `Client.connect` untouched, so an interceptor, a different data converter or
    a retry config is a caller's decision rather than a kontra feature request.
    """
    from temporalio.client import Client

    kwargs.setdefault("data_converter", casstore.data_converter())
    kwargs.setdefault("tls", connect_tls())
    kwargs.setdefault("identity", workerid.worker_identity(task_queue))
    return await Client.connect(address_of(address), namespace=namespace_of(namespace), **kwargs)


def _worker(client, task_queue: str, **kwargs: Any):
    """The shared `Worker` construction: identity and build id defaulted, everything forwarded.

    `setdefault` RATHER THAN POSITIONAL ARGUMENTS, so a caller can override any of it. The point of
    these builders is that nothing is hidden — only defaulted.
    """
    from temporalio.worker import Worker

    kwargs.setdefault("identity", workerid.worker_identity(task_queue))
    kwargs.setdefault("build_id", workerid.build_id())
    return Worker(client, task_queue=task_queue, **kwargs)


def workflow_worker(
    client,
    *,
    workflows: Sequence[type],
    task_queue: str,
    activities: Sequence[Any] = (),
    passthrough_modules: Sequence[str] = (),
    bind_logs: bool = True,
    **kwargs: Any,
):
    """A `temporalio.worker.Worker` that can run these workflows. Returns it; does not start it.

    The sandbox runner is DEFAULTED, not forced: `kwargs.setdefault("workflow_runner", ...)` means
    a caller who wants `UnsandboxedWorkflowRunner` for a debugger session can say so, which is one
    of the reasons this function exists at all.

    `bind_logs` names the queue records written outside any task fall back to — boot, shutdown, and
    the polling-with-nothing-in-flight lines, which are exactly the ones that explain a Worker that
    never picked anything up. A caller building a Worker inside a process that already bound its
    own queue passes False.
    """
    from temporalio.worker.workflow_sandbox import SandboxedWorkflowRunner, SandboxRestrictions

    if not workflows:
        raise ValueError("a workflow worker needs at least one @workflow.defn class to run")
    if bind_logs:
        logs.bind_worker(task_queue)
    kwargs.setdefault(
        "workflow_runner",
        SandboxedWorkflowRunner(
            restrictions=SandboxRestrictions.default.with_passthrough_modules(
                *DEFAULT_PASSTHROUGH, *passthrough_modules
            )
        ),
    )
    return _worker(
        client,
        task_queue,
        workflows=list(workflows),
        activities=list(activities),
        **kwargs,
    )


def actor_worker(client, registry, *, task_queue: str, bind_logs: bool = True, **kwargs: Any):
    """A `temporalio.worker.Worker` serving one Actor's Methods. Returns it; does not start it.

    The activity set is derived from the registry rather than passed, because an Actor's Methods
    ARE its activities — a caller who could pass their own would be building a worker that answers
    to the Actor's queue without answering for the Actor.
    """
    # INSIDE THE FUNCTION, because `host` imports this module: both names live there, and a
    # module-scope import here would be a cycle. At call time the cycle is already resolved.
    from internals.temporal.host import build_activities, live_sessions

    if bind_logs:
        logs.bind_worker(task_queue)
    kwargs.setdefault("activities", build_activities(registry, sessions=live_sessions(registry)))
    return _worker(client, task_queue, **kwargs)


__all__ = [
    "connect",
    "workflow_worker",
    "actor_worker",
    "address_of",
    "namespace_of",
    "DEFAULT_PASSTHROUGH",
]
