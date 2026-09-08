"""The Nexus service contract every kontra actor serves — the Python side of it.

Temporal's model for calling deployed code across a team boundary is a **shared service
definition**: one `@nexusrpc.service` class, imported by the caller and the handler alike, so
the caller depends on the contract and never on the callee's code. That is exactly kontra's
situation — the handler is Go, the caller is Python, and they meet only at a wire — so this is
that class rather than a bag of strings at the call site.

The Go side declares the same shape from `shared/contracts/kontra/v1/actor_service.proto` via
`protoc-gen-go-temporal`; there is no code sharing between them and there cannot be. So the two
literals below — the service name and the operation name — are a cross-language contract in the
same sense as the task-queue derivations, and they are pinned by test on both sides. A drift
here is not a type error, it is a call to an endpoint that answers nothing.

The payload types are `TypedDict`s on purpose. They give the caller real static checking of the
field set while staying **plain dicts at runtime**, so the bytes on the wire are byte-identical
to what `control/orchestrator/src/workflows/interpreter.ts` sends — which is the property that lets an
actor deployed a month ago be called today. A dataclass would have serialized differently
(`params: null` where the interpreter omits the key), and "differently" across a contract like
this one is how a namespace boundary silently drops data.

`nexusrpc` is imported at module scope here, which is why this module is imported LAZILY from
`workflows.py`: `import kontra` must stay free of a Temporal dependency, the same rule
`lib/actor.py` follows. Import it directly when you want the types:

    from kontra.contract import KontraActorService, EntryInput, BareRef
"""

from __future__ import annotations

from typing import Any, TypedDict

import nexusrpc

#: The one Nexus service every actor serves (ADR 0001 — one op, one way in). VERBATIM the peer
#: of runtime/handler/internal/identity.NexusServiceName and orchestrator nexusService.SERVICE_NAME.
SERVICE_NAME = "kontra.actor"


class BareRef(TypedDict, total=False):
    """The content-addressed claim-check ref — `entry.proto` BareRef.

    `size` is the byte length of the JSON-serialized payload, and `meta` carries the original
    payload metadata. The caller integrity-checks `sha256` against the CAS on fetch.
    """

    sha256: str
    size: int
    meta: dict[str, str]


class EntryInput(TypedDict, total=False):
    """The single argument to the `run` operation — `entry.proto` EntryInput.

    Exactly one of `units` / `input_ref` carries the batch. The actor runs ONE batch per run
    (ADR 0012 — the dispatcher shards, not the actor), so this is exactly the proto field
    set: no session or chunk sharding knobs.

    `total=False` because the wire omits unset optionals rather than sending nulls, matching the
    interpreter. Build one with `kontra.catalog.entry_input()` rather than by hand — the
    omission rule is part of the contract and lives there.
    """

    units: list[Any]
    input_ref: BareRef
    return_ref: bool
    run_id: str
    idempotency_key: str
    node_id: str
    expected_digest: str
    params: dict[str, Any]
    method: str
    session_id: str


@nexusrpc.service(name=SERVICE_NAME)
class KontraActorService:
    """The contract: one operation, `run`, `EntryInput` in and a `BareRef` out.

    One operation is the whole point (ADR 0001): an actor IS a Nexus operation, so dispatch has
    exactly one shape whether the target is in this namespace or another one. It is a
    workflow-run operation on the handler side, so it completes when the actor's backing
    workflow does — which is what makes `await ...execute_operation(...)` mean "the batch is
    finished", not "the batch was accepted".
    """

    run: nexusrpc.Operation[EntryInput, BareRef]


__all__ = ["KontraActorService", "EntryInput", "BareRef", "SERVICE_NAME"]
