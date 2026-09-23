"""Which Worker is this — asked of Temporal's own vocabulary rather than invented beside it.

A LOG LINE AND A STREAM RECORD BOTH NAME A RUN AND NEITHER NAMES A WORKER. `logs.py` puts the Run
on every record and `engine.py` puts the Session on every stream record, so "what did this Run do"
is answerable and "which of the eleven Workers in this Fleet said it" is not. On a packed Machine
running four Workers, or a Fleet where one Droplet is grinding an abandoned sweep while ten are
idle, that is the question — and it was the one thing no signal carried.

── TEMPORAL ALREADY HAS THE FIELD, AND IT WAS LEFT AT ITS DEFAULT ──────────────────────────────────

Every Worker has an IDENTITY, and it is not a kontra concept: the server records it on
`WorkflowTaskStarted` and `ActivityTaskStarted`, returns it from `DescribeTaskQueue` as the poller
list, and shows it on a pending activity. `panels/pollers.ts` already reads it — the Monitor's
"which Machine is live" column is that string, parsed. So the identity is ALREADY the join key
between history, the poller listing and the Fleet; what was missing is that nothing set it
deliberately and nothing stamped it onto the two signals a human actually reads.

The SDK's own note on the field says it plainly (`@temporalio/worker`, `WorkerOptions.identity`):

    Note that in most production environments, the `identity` value set by default may be unhelpful
    for traceability purposes. It is highly recommended that you set this value to something that
    will allow you to efficiently identify that particular Worker container/process/logs in your
    infrastructure.

── THE SHAPE IS TEMPORAL'S, NOT OURS, AND THAT IS THE WHOLE POINT ──────────────────────────────────

`<pid>@<hostname>@<queue>` is what the Go SDK writes by default; Python writes `<pid>@<hostname>`
and drops the queue. So this is not a new grammar — it is the Go default, adopted on the Python
side so ONE parser reads both. `shared/core/src/queues.ts::identityHost` takes field two as the
host and is therefore unchanged by this, which is the test that this extends Temporal's convention
rather than replacing it: a string that needed a new parser would be a kontra scheme wearing
Temporal's field.

The queue earns its place in field three because a Machine runs MANY Workers — an actor host on
`desync-0.3.1-sessions`, a Session worker on `desync-0.3.1-s-<id>`, a workflow host on the
caller's queue. `<pid>@<host>` cannot tell those apart in a poller listing, and the pid that could
changes on every restart.

── build_id IS NOT VERSIONING HERE, AND IT PAYS FOR ITSELF AT BOOT ─────────────────────────────────

`build_id` names the CODE, where identity names the PROCESS. It is set here for two reasons and
neither of them is Worker Versioning, which stays off:

  • A Bundle already has a content identity — the sha256 of its own bytes, which `cli/bundle.go`
    computes, the OCI layer digest carries and the Machine re-verifies on arrival. Handing that to
    Temporal makes "which build is this Worker running" a server-side fact rather than something
    reconstructed from a deploy log.
  • THE DEFAULT IS EXPENSIVE. Unset, the Python SDK "automatically generates a best-effort
    identifier by traversing and computing hashes of all modules in the codebase" — on every Worker
    boot, for a value nobody was reading. An actor Bundle carries the SDK, the runtime and the
    author's tree; that traversal is pure boot latency.

`use_worker_versioning` STAYS FALSE and `deployment_config` is not used. Both change which Worker
is given which task, which is a routing decision that deserves its own ADR and its own rollout —
not a side effect of wanting a label. `deployment_config` is the documented successor to `build_id`
and is still marked experimental in the SDK; when it lands, this is the one function that changes.
"""

from __future__ import annotations

import os
import socket

__all__ = ["MACHINE", "ROLE", "build_id", "worker_identity", "worker_fields"]


#: THE MACHINE THIS PROCESS RUNS ON — field two of the Temporal identity, which is exactly what the
#: Fleet's poller listing shows per Machine (`11@kf-dns-01@nscheck-0.1.0` is `pid@host@queue`).
#:
#: Snapshotted at import because a hostname does not change under a live process and this is read
#: once per Batch. On the Fleet it is the Droplet's name (`kf-dns-01`): a Worker is a pair of
#: systemd units ON the Machine, not a container, so no container id stands between the two
#: (control/orchestrator/src/infra/programs/machine.ts). In compose it is the container_name, which
#: `docker-compose.yml` pins for this exact reason.
MACHINE = socket.gethostname()

#: WHAT THIS PROCESS IS FOR, when the queue does not already say it. An actor host's queue names
#: its actor; the orchestrator's roles share a process and a hostname and would otherwise be one
#: undifferentiated `kontra-api`. Absent is fine — it is a label, not an address.
ROLE = os.environ.get("KONTRA_WORKER_ROLE", "")


def build_id() -> str | None:
    """The CODE this Worker is running, as a value somebody else already computed.

    `KONTRA_BUNDLE_SHA` is the Bundle's own sha256 — the OCI layer digest the Machine verified on
    arrival — so a Worker's build id and the artifact a reader can `oras pull` are the same string.
    Falling back to the actor version rather than to nothing, because a version is a weaker answer
    than a digest and both are stronger than the module-hash walk the SDK would do instead.

    `None` when neither is set, which is the local dev case: there is no Bundle, so there is no
    honest build id, and the SDK's default is then the right behaviour rather than a wrong label.
    """
    return os.environ.get("KONTRA_BUNDLE_SHA") or os.environ.get("KONTRA_ACTOR_VERSION") or None


def worker_identity(queue: str) -> str:
    """`<pid>@<hostname>@<queue>` — the Go SDK's default shape, written by the Python side too.

    The pid is FIRST and it is deliberate that it is not dropped: two Workers of one actor on one
    packed Machine differ by nothing else, and a poller listing that collapsed them would report
    half the capacity that exists. `MACHINE` is what survives a restart and is therefore what
    `identityHost` reads; the pid is what separates co-tenants.

    An empty queue still produces the trailing `@`, matching what the Go SDK writes for a CLIENT
    that polls nothing — so a parser never has to special-case the shorter string.
    """
    return f"{os.getpid()}@{MACHINE}@{queue}"


def worker_fields(queue: str) -> dict[str, str]:
    """The same identity as LOG AND STREAM FIELDS, so a line joins to a history event by equality.

    `worker` is byte-identical to what `Worker(identity=...)` was given, which is the property that
    matters: an operator who finds a suspicious line filters `DescribeTaskQueue` on that exact
    string, or greps one Run's history for it, with no re-derivation in between. Splitting it into
    `machine` and `queue` as well is not redundancy — those are what a human filters a rail by,
    and asking them to parse a compound key in a query box is how a filter goes unused.

    Empty values are omitted rather than written blank, matching `logs.bind_run`: absent reads as
    "not recorded" where `""` reads as "recorded as nothing".
    """
    fields = {
        "worker": worker_identity(queue),
        "machine": MACHINE,
        "queue": queue,
        "role": ROLE,
        "build_id": build_id() or "",
    }
    return {k: v for k, v in fields.items() if v}
