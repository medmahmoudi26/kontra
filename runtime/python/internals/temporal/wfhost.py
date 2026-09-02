"""The LOCAL workflow worker: your workflows, your machine, the fleet's actors.

The actor host (`internals/temporal/host.py`) serves activities in the cloud. This serves
workflows wherever you run it — a laptop, a tmux pane, a droplet — and the only thing it needs
from the fleet is Temporal's address. Your workflow then reaches deployed actors over Nexus and
deployed activity bundles over their task queue, so the code that decides WHAT runs lives with
you while the code that does the work lives out there.

THREE THINGS ARE NOT LEFT TO THE CALLER, because all three fail in the same shape — fine on a
small run, dead on a real one:

  • THE CODEC. An actor's result comes back through the claim-check codec whenever it exceeds
    128 KiB. A client without it fails with `Unknown payload encoding binary/claim-check-v1`,
    retried to exhaustion. Small batches pass; a scope run does not. So the converter is built
    in, exactly as the actor host builds it, from the same KONTRA_S3_* env.
  • THE SANDBOX PASSTHROUGH. `actorkit` is pure, stateless Python, and re-importing it per
    workflow instance under the sandbox's import proxy buys nothing and can trip over the dev
    checkout's `__path__` shim. It is passed through by default; add your own with
    `passthrough_modules=`.
  • **LOGGING.** `workflow.logger.info(...)` is the one line an author writes to watch their own
    run, and it went NOWHERE. Temporal routes it through the `temporalio` logger, and Python's
    logging emits nothing at all until somebody configures a handler — so a workflow that
    logged every step printed an empty pane, and the pane beside the editor (which exists
    precisely so a run is watchable) showed two boot lines and then silence for the whole run.
    Nothing failed and nothing said so. See `_configure_logging`.
"""

from __future__ import annotations

import asyncio
import contextlib
import inspect
import logging
import os
from typing import Any, Sequence

log = logging.getLogger("kontra.wfhost")

# Passed through the workflow sandbox by default: the SDK seam itself. Everything else stays
# sandboxed, which is the point of the sandbox — this is the one module we know is safe because
# we wrote it and it holds no mutable global state.
DEFAULT_PASSTHROUGH = ("actorkit",)


def _configure_logging() -> None:
    """Make `workflow.logger` actually reach the pane.

    THE LINE AN AUTHOR WRITES TO WATCH THEIR RUN WENT NOWHERE. `workflow.logger.info(...)` is the
    documented way to log from a workflow — it is replay-aware, so a line is written once rather
    than again on every replay — and it routes through the `temporalio` logger. Python's logging
    emits NOTHING until a handler is configured, and a worker process configures none, so every
    one of those calls was discarded. The tmux pane beside the editor exists so that a run is
    watchable and a worker that dies on boot is visible; for a run that was working it showed two
    boot lines and then nothing at all, for its entire duration.

    STDOUT, because that is what the pane is. `capture-pane` reads what the process wrote to its
    terminal; a handler on stderr would work equally well and a file handler would not work at all.

    UNBUFFERED, via `flush=True` semantics — `logging.StreamHandler` flushes per record, which is
    what makes a line appear WHILE the run is going rather than when the process exits. A pane that
    only fills in at the end is not a pane you can watch.

    NEVER OVERRIDES A CALLER'S SETUP. An author who called `logging.basicConfig` themselves, or who
    is embedding this in a larger program, has said what they want; `basicConfig` is already a
    no-op when the root logger has handlers, and the explicit check makes that a decision rather
    than a coincidence of the stdlib's behaviour.

    KONTRA_LOG_LEVEL is the knob, because INFO is right for watching a run and wrong for a fleet
    Machine writing to journald at volume.
    """
    level = os.environ.get("KONTRA_LOG_LEVEL", "INFO").upper()
    if not logging.getLogger().handlers:
        logging.basicConfig(
            level=level,
            format="%(asctime)s %(levelname)s %(name)s: %(message)s",
            datefmt="%H:%M:%S",
        )
    # The workflow logger is a CHILD of `temporalio`, which the SDK leaves at the root's level.
    # Setting it explicitly is what makes KONTRA_LOG_LEVEL=DEBUG mean the workflow's own lines and
    # not just the SDK's.
    logging.getLogger("temporalio").setLevel(level)


def _watch_targets(workflows: Sequence[type]) -> tuple[list[tuple[str, list[str]]], list[str]]:
    """What watch mode re-imports and can mark broken: one (source file, type names) pair per file,
    plus every folder to watch.

    Each pair is a source the probe re-imports on save and the TYPES Temporal routes on that were
    DECLARED IN THAT FILE (`@workflow.defn(name=…)` when the author overrode it, the class name
    otherwise) — the probe posts those as broken when a save makes that file stop importing, because
    a file that will not import cannot be asked what it declares. `dirs` is every folder those
    sources live in, so a sibling module edited beside a workflow triggers a re-derivation too.

    ONE GROUP PER FILE, not one file plus every name. This returned the FIRST served file and the
    names of ALL of them, so a serve spanning two files re-imported one and, when that one broke,
    posted the other file's healthy workflow as BROKEN — carrying a SyntaxError from a file it has
    nothing to do with, its schemas overwritten to NULL and its queue replaced by the broken file's.
    A start routed on that record goes to a queue where nothing serves it and reports running
    forever, which is the exact failure the `queue` field exists to prevent.

    A class whose source cannot be located (`inspect.getfile` raises for one defined in a REPL or a
    C extension) contributes NO target: there is no file to re-import for it, and a name with no file
    is precisely the name that used to ride along on someone else's failure.
    """
    from temporalio import workflow

    groups: dict[str, list[str]] = {}
    for cls in workflows:
        defn = workflow._Definition.from_class(cls)
        if defn is None:
            continue
        with contextlib.suppress(TypeError, OSError):
            groups.setdefault(os.path.abspath(inspect.getfile(cls)), []).append(defn.name)
    dirs = sorted({os.path.dirname(f) for f in groups})
    return list(groups.items()), dirs


async def serve_workflows_async(
    workflows: Sequence[type],
    *,
    task_queue: str,
    activities: Sequence[Any] = (),
    address: str = "",
    namespace: str = "",
    passthrough_modules: Sequence[str] = (),
    max_concurrent_activities: int | None = None,
    watch: bool = False,
) -> None:
    # FIRST, before anything can log. A worker that configured logging after connecting would
    # discard whatever the connection said on the way — which is exactly the material you want when
    # the address is wrong.
    _configure_logging()

    from temporalio.client import Client
    from temporalio.worker import Worker
    from temporalio.worker.workflow_sandbox import (
        SandboxedWorkflowRunner,
        SandboxRestrictions,
    )

    from internals import casstore
    from internals.catalog import publish_workflow_catalog

    if not workflows:
        raise ValueError("serve() needs at least one @workflow.defn class to run")
    # `kontra workflow serve` sets KONTRA_WORKFLOW_QUEUE to the queue DERIVED from the folder's
    # content (wf-<name>-<digest>, GitHub #15), so a module serves on the right queue without an
    # edit and without a typed --queue. An explicit argument still wins, for a hand-run worker.
    task_queue = task_queue or os.environ.get("KONTRA_WORKFLOW_QUEUE", "")
    if not task_queue:
        raise ValueError(
            "serve() needs a task_queue — it is the address your workflows answer on. "
            "Pass task_queue=, or run this through `kontra workflow serve <folder>`, which sets "
            "KONTRA_WORKFLOW_QUEUE to the queue derived from the folder"
        )

    address = address or os.environ.get("KONTRA_ADDRESS", "localhost:7233")
    namespace = namespace or os.environ.get("KONTRA_NAMESPACE", "default")

    client = await Client.connect(
        address, namespace=namespace, data_converter=casstore.data_converter()
    )

    # SELF-REGISTRATION, exactly where the actor host does it (`internals/temporal/host.py`).
    #
    # A workflow described itself to nobody. `GET /api/workflows` lists `.kontra/workflows/` by
    # filename, byte count and mtime, so every surface that showed a workflow could say a file
    # existed and nothing about what it accepts, what it returns or what it is for — all three
    # already written in the run signature and the class docstring, none of them surviving to a
    # reader. Serving is the one moment the truth is available, because these are the classes
    # this process is about to register with Temporal.
    #
    # Best-effort and it never raises: a catalog that cannot be reached must not stop a worker
    # from serving, which is what would make the catalog a dependency of running code.
    # THE QUEUE RIDES WITH IT. This worker is about to poll `task_queue`, which makes it the only
    # honest answer to "which queue do I start this on" — and without it a caller has the type and
    # not the address, which is a run that starts, routes to a queue nobody serves it on, and
    # reports `running` forever.
    publish_workflow_catalog(workflows, queue=task_queue)

    runner = SandboxedWorkflowRunner(
        restrictions=SandboxRestrictions.default.with_passthrough_modules(
            *DEFAULT_PASSTHROUGH, *passthrough_modules
        )
    )
    kwargs: dict[str, Any] = {}
    if max_concurrent_activities is not None:
        kwargs["max_concurrent_activities"] = max_concurrent_activities

    worker = Worker(
        client,
        task_queue=task_queue,
        workflows=list(workflows),
        activities=list(activities),
        workflow_runner=runner,
        **kwargs,
    )
    names = ", ".join(getattr(w, "__name__", str(w)) for w in workflows)
    log.info("[wfhost] %s on %s (%s)", names, task_queue, address)
    print(f"[wfhost] {names} -> {task_queue} @ {address}", flush=True)
    if not os.environ.get("KONTRA_S3_ENDPOINT"):
        # Not fatal — a no-S3 setup is a legitimate local mode, and the actor side is passthrough
        # under the same condition. But it is the difference between "works" and "works until the
        # batch is big", so it is said out loud once at boot rather than discovered at 200 KiB.
        print(
            "[wfhost] KONTRA_S3_ENDPOINT unset: claim-check codec is PASSTHROUGH — "
            "results over 128 KiB from a store-backed actor will not decode",
            flush=True,
        )

    # OPT-IN, and a plain serve behaves exactly as before: `kontra workflow serve --watch` sets
    # KONTRA_WORKFLOW_WATCH, and without it there is no watcher and no behaviour change at all.
    watch = watch or os.environ.get("KONTRA_WORKFLOW_WATCH", "") not in ("", "0", "false", "False")
    if not watch:
        await worker.run()
        return

    # WATCH MODE: the worker keeps running while a loop re-derives the contract on every save. The
    # queue it re-publishes is `task_queue` — the one THIS worker is still polling — not a queue
    # re-derived from the edited folder, because the running code is still the code that will answer.
    from internals.wfwatch import watch_and_reprobe

    watch_targets, watch_dirs = _watch_targets(workflows)
    if not os.environ.get("KONTRA_ORCHESTRATOR_URL") or not watch_targets:
        # Nothing to publish to (or nothing to re-import), so a watcher would spawn probes that do
        # nothing. Said out loud rather than started silently — the loop's whole value is the form
        # in the browser, and there is no browser to reach here.
        reason = "no KONTRA_ORCHESTRATOR_URL" if watch_targets else "no importable source file"
        print(f"[wfwatch] --watch has nothing to do ({reason}); serving without it", flush=True)
        await worker.run()
        return

    print(f"[wfwatch] watching {', '.join(watch_dirs)} — save to regenerate the form", flush=True)
    watcher = asyncio.create_task(
        watch_and_reprobe(targets=watch_targets, dirs=watch_dirs, queue=task_queue)
    )
    try:
        await worker.run()
    finally:
        watcher.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await watcher


def serve_workflows(workflows: Sequence[type], **kw: Any) -> None:
    """Blocking entrypoint — the peer of `actor.serve()` for the caller's side."""
    asyncio.run(serve_workflows_async(workflows, **kw))
