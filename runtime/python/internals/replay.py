"""Replay a run's history against the workflow code on disk — with no clock against you.

WHY REPLAY AND NOT A LIVE DEBUGGER. Temporal's own VSCode extension takes this approach and it fits
kontra better than attaching to a running worker does: a replay has **no clock**. No
`HeartbeatTimeout`, no `StartToClose`, no server waiting on a poller. You can sit on a breakpoint for
an hour and nothing times out, nothing retries, nothing notices. Attaching to a live activity gives
you about two minutes (`runActivityOptions`), which is the gap `13` is about.

WHAT REPLAY DOES NOT COVER, said here because a green replay reads as "it works". **Activity code is
not run.** Temporal's replayer executes WORKFLOW code and feeds activity results from the recorded
history. A kontra actor's Method IS an activity, so a breakpoint inside one will never be hit by
this. This covers the caller's decisions — batch splitting, chaining, branching, early exit — which
is exactly the class of bug that only shows up at fleet scale.

TWO THINGS MUST MATCH THE WORKER OR THE ANSWER IS A LIE:

  the data converter   `casstore.data_converter()` carries the ClaimCheck codec. A Replayer built
                       without it cannot decode payloads and reports that as a replay FAILURE —
                       which reads as "your workflow is non-deterministic" when it means "I could
                       not decode the input".
  the sandbox runner   `wfhost.py` serves under `SandboxedWorkflowRunner` with a passthrough list.
                       Replaying under different restrictions can raise inside the sandbox for
                       reasons the real worker never hits, which is a false positive of the most
                       expensive kind.

THREE OUTCOMES, NOT TWO. Clean, non-deterministic, and could-not-run are different answers: the
second is a fact about the workflow code, the third is a fact about this setup. Collapsing them
sends someone to rewrite a workflow because a codec was missing.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import Any, Sequence


class Outcome(str, Enum):
    OK = "ok"
    NONDETERMINISTIC = "nondeterministic"
    UNRUNNABLE = "unrunnable"


@dataclass
class ReplayResult:
    outcome: Outcome
    run_id: str
    workflow_type: str
    detail: str = ""

    def as_json(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "outcome": self.outcome.value,
            "runId": self.run_id,
            "workflowType": self.workflow_type,
        }
        if self.detail:
            out["detail"] = self.detail
        return out


def _runner():
    """The SAME sandbox the worker serves under (`wfhost.py`), plus the module we imported by path.

    THE PROBE MODULE MUST PASS THROUGH. A workflow loaded from a file path lives in `sys.modules`
    under `__kontra_wfwatch_probe__` and exists nowhere on the import path, so the sandbox — whose
    job is to re-import a workflow's module fresh — cannot find it and fails validation. Left out,
    every replay of unchanged code reports `Failed validating workflow`, which reads as a
    non-determinism finding and is nothing of the kind.
    """
    from temporalio.worker.workflow_sandbox import SandboxedWorkflowRunner, SandboxRestrictions

    from internals.temporal.wfhost import DEFAULT_PASSTHROUGH
    from internals.wfwatch import _PROBE_MODULE

    return SandboxedWorkflowRunner(
        restrictions=SandboxRestrictions.default.with_passthrough_modules(
            *DEFAULT_PASSTHROUGH, _PROBE_MODULE
        )
    )


def _with_cause(exc: BaseException) -> str:
    """The message plus its cause chain.

    Temporal raises `Failed validating workflow <Name>` with the real reason on `__cause__`, and a
    bare validation message sends someone hunting a determinism bug that does not exist.
    """
    parts = [f"{type(exc).__name__}: {exc}"]
    seen, cur = 0, exc.__cause__
    while cur is not None and seen < 4:
        parts.append(f"  caused by {type(cur).__name__}: {cur}")
        cur, seen = cur.__cause__, seen + 1
    return "\n".join(parts)


def workflow_classes(file: str) -> list[type]:
    """The `@workflow.defn` classes a file DECLARES.

    Imported under a module name that is not `__main__`, so a workflow file ending in
    `if __name__ == "__main__": catalog.serve([...])` yields its classes without standing a worker
    up. `defn_classes` is the catalog's own predicate — asking Temporal which classes are
    workflows, rather than guessing from a naming convention.
    """
    from internals.catalog import defn_classes
    from internals.wfwatch import _import_fresh

    return defn_classes(_import_fresh(file))


async def fetch_history(workflow_id: str, *, run_id: str = "") -> dict[str, Any]:
    """A run's history as JSON, through a client that holds kontra's codec.

    The codec matters even here: without it the payloads come back as claim-check references rather
    than values, and a history saved that way replays against nothing.
    """
    from temporalio.client import Client

    from internals import casstore
    from internals.temporal.tlsconfig import connect_tls

    import os

    address = os.environ.get("KONTRA_ADDRESS", "localhost:7233")
    namespace = os.environ.get("KONTRA_NAMESPACE", "default")
    client = await Client.connect(
        address, namespace=namespace, data_converter=casstore.data_converter(), tls=connect_tls()
    )
    # THE POSITIONAL IS THE WORKFLOW ID. `run_id` pins ONE attempt and is optional. Passing a
    # workflow id in the run_id slot made the Rust core SEGFAULT rather than reject it, so the
    # shapes are kept apart here rather than guessed at from one argument.
    handle = client.get_workflow_handle(workflow_id, run_id=run_id or None)
    history = await handle.fetch_history()
    # `to_json` is SYNC in temporalio 1.30 — awaiting it raises "object str can't be used in 'await'
    # expression", which a broad except in a caller will happily report as "no Temporal".
    return json.loads(history.to_json())


async def replay_history(history_json: dict[str, Any], workflows: Sequence[type]) -> ReplayResult:
    """Replay one history against `workflows`, reporting which of the three outcomes it is."""
    from temporalio.client import WorkflowHistory
    from temporalio.worker import Replayer

    from internals import casstore

    try:
        history = WorkflowHistory.from_json("replay", history_json)
    except Exception as exc:  # noqa: BLE001
        return ReplayResult(Outcome.UNRUNNABLE, "", "", f"the history is not readable: {exc}")

    wf_type = ""
    try:
        started = history_json["events"][0]["workflowExecutionStartedEventAttributes"]
        wf_type = started["workflowType"]["name"]
    except Exception:  # noqa: BLE001
        pass

    if not workflows:
        return ReplayResult(
            Outcome.UNRUNNABLE, history.run_id, wf_type,
            "no @workflow.defn classes were found to replay against",
        )

    try:
        replayer = Replayer(
            workflows=list(workflows),
            workflow_runner=_runner(),
            data_converter=casstore.data_converter(),
        )
    except Exception as exc:  # noqa: BLE001
        return ReplayResult(Outcome.UNRUNNABLE, history.run_id, wf_type, f"could not build a replayer: {exc}")

    try:
        await replayer.replay_workflow(history, raise_on_replay_failure=True)
    except Exception as exc:  # noqa: BLE001
        # A workflow type the given classes do not define is a SETUP problem, not a determinism one:
        # the operator pointed replay at the wrong file. Saying "non-deterministic" here would send
        # them to rewrite code that was never executed.
        text = _with_cause(exc)
        if "not registered" in text or "Workflow class" in text:
            return ReplayResult(
                Outcome.UNRUNNABLE, history.run_id, wf_type,
                f"{text}\n  the file you pointed at does not declare {wf_type or 'this workflow'}",
            )
        # A SANDBOX VALIDATION FAILURE IS A SETUP PROBLEM, not a finding about the workflow. It means
        # the replayer could not even construct the code — usually an import the sandbox cannot
        # satisfy — and reporting it as non-determinism would send somebody to rewrite code that
        # never ran.
        if "Failed validating workflow" in text:
            return ReplayResult(
                Outcome.UNRUNNABLE, history.run_id, wf_type,
                f"{text}\n  the replayer could not construct this workflow; this is not a "
                "determinism finding",
            )
        return ReplayResult(Outcome.NONDETERMINISTIC, history.run_id, wf_type, text)

    return ReplayResult(Outcome.OK, history.run_id, wf_type)


def load_history_file(path: str) -> dict[str, Any]:
    p = Path(path)
    if not p.is_file():
        raise SystemExit(f"no history file at {p}")
    try:
        return json.loads(p.read_text())
    except json.JSONDecodeError as exc:
        raise SystemExit(f"{p} is not valid JSON: {exc}") from exc
