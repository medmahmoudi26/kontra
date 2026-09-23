"""Provision the machines your run needs, FROM the workflow that needs them.

`kontra.catalog` calls Actors that are already deployed. This module is the step before that:
it makes them exist, for exactly as long as the work does.

    from kontra import catalog, fleet
    from temporalio import workflow

    @workflow.defn
    class Recon:
        @workflow.run
        async def run(self, req: dict) -> dict:
            async with fleet.up(fleet.do_fleet(machines=4, region="nyc3", credential="do-prod"),
                                actor="nscheck", version="0.1.0", sessions=8) as f:
                await f.ready()
                async with catalog.actor("nscheck", "0.1.0") as ns:
                    ...
            # the scope's exit drops this run's LEASE. The Machines die only if it was the last one

TWO DOORS, AND THE SECOND ONE SEPARATES CAPACITY FROM WHAT RUNS ON IT (ADR 0037):

    async with fleet.hold(tag="dns", machines=4) as f:   # capacity, and a Lease on it
        await f.place("nscheck", "0.1.0", sessions=8)    # what runs on it
        await f.ready()

`up()` is SUGAR over exactly that pair and nothing else — see {@link up} — so every workflow written
against it keeps working, and keeps costing what it cost. What splitting them buys is the one thing
`up()` cannot express: `place()` is IDEMPOTENT DESIRED STATE, so calling it again with a different
`sessions=` is how a **Run** rescales a **Fleet** it is already using. There is no `scale()` verb and
there is not going to be one; the second call IS the scale operation.

WHAT SPLITTING THEM COSTS, WHICH ADR 0037 NAMES AND THIS FILE MUST NOT HIDE: `place()` can fail
after `hold()` has succeeded — the Artifact will not resolve, the digest is unsigned, the install
does not come up — and at that moment the **Machines** are up, they are billing, and there is
nothing on them. `up()` cannot reach that state: it resolves the Artifact BEFORE the first Droplet
exists, so the same typo costs one activity rather than a whole provision. The window is real, it is
priced in minutes and money, and {@link Fleet.place} raises {@link PlacementFailed} saying so rather
than re-raising a Pulumi error that reads like any other.

THE SCOPE IS THE POINT, and it is the same bargain `catalog.dataset(...).writer()` and
`catalog.actor(...)` already make: the resource lives for the length of an `async with`, and the
exit is not a verb you have to remember. What makes it trustworthy HERE rather than merely tidy is
that a caller's workflow is durable. A script that provisions ten Droplets and then dies leaves ten
Droplets; a workflow that does it cannot, because letting go is a replayable step in a program
Temporal will finish even if this process is gone. Cloud machines that outlive the run are a
billing event and an OPSEC one — fleet Machines reach hostile infrastructure, and an orphan nobody
is tracking is worse than a failure.

WHAT THE EXIT DOES IS DROP A **LEASE**, NOT DESTROY A FLEET (ADR 0037). A **Fleet** is capacity, and
several runs may hold one at once; its **Machines** are destroyed when the LAST claim drops, by a
**Lease** workflow (`control/orchestrator/src/workflows/lease.ts`) rather than by whichever scope happened to finish first.
For a run that is alone on its fleet — every example in this repo — nothing observable changes. For
two runs on one fleet, it is the difference between capacity and a race.

AND EVERY **LEASE** EXPIRES ON A CLOCK, which is the part a scope could never do for itself. The
durability argument above has one hole in it: a **Run** that is never resumed never exits its scope,
so a control plane that dies holding a fleet leaves the fleet. The clock closes it — at a deadline
the **Lease** workflow asks Temporal whether each holder is still RUNNING and destroys the **Machines** when
none of them are. A live run is never reaped by it and pays nothing to stay held: no heartbeat, no
renew loop, no events at all between hold and drop.

WHAT THIS IS NOT. It is not a second way to run Actors. `up()` returns nothing you dispatch to —
placement and dispatch stay separate verbs, exactly as `kontra fleet up` and `kontra dispatch` are
separate commands. The fleet is WHERE work runs; `catalog` is what runs it. A combined
`f.run(actor(...))` would read well in a README and would fuse the one boundary this system keeps
deliberately open: which Machines exist is declarative state, and which Batches go through them is
a loop you write.

HOW IT WORKS, so nothing here is magic. `up()` starts `stackWorkflow` as a CHILD WORKFLOW on the
infra queue — the same workflow `kontra fleet up` reaches over HTTP, with the same id, which is the
stack's fqn. That identity is load-bearing: Pulumi's DIY lock has no compare-and-swap and no TTL,
so Temporal's workflow-id uniqueness is what actually serialises writers. It also means a run that
tries to claim a fleet somebody is already converging FAILS at the start rather than corrupting it,
and that failure is the correct answer — the two runs wanted the same Machines.

READY MEANS POLLERS, NOT PULUMI. `pulumi up` reporting `succeeded` says the Droplets exist and the
install script exited 0. Systemd has not necessarily brought a handler up yet, and a Batch
dispatched into that gap sits on a queue nobody is polling until ScheduleToStart fires. So
`ready()` asks Temporal who is actually polling. Registration says an actor exists; only a poller
says it can run.

PACKING, AND WHAT `spread=` MEANS NOW THAT IT DOES SOMETHING (ADR 0037, slice 11). A **Fleet** takes
SEVERAL placements, so several **Workers** share a **Machine** and share its egress address:

    async with fleet.hold(tag="dns", machines=4) as f:
        await f.place("nscheck", "0.1.0", sessions=8)
        await f.place("subfinder", "0.2.0", spread=True)   # one Worker per Machine
        await f.ready()

  • PACKING IS REQUESTED BY PLACING A SECOND ARTIFACT, NOT BY A FLAG. There is no `pack=`, because
    `place()` is it: the second call is the request, and its consequence — the two **Workers** on a
    **Machine** share its source address — is the thing to know before making it.
  • A PLACEMENT PUTS AT MOST ONE **WORKER** ON ANY ONE **MACHINE**. Two of one `<actor>@<version>`
    there would carry the same `KONTRA_WORKER` label, write the same units and poll the same queue,
    so nothing could tell them apart (`cli/warden/driver.go`, and `cli/warden/warden.go:reconcile` already refuses
    the duplicate out loud). More concurrency for ONE Artifact on ONE **Machine** is `sessions=`,
    which is density and is the axis ADR 0037 §6 says packing does not replace.
  • `workers=N` IS HOW MANY **MACHINES** THIS PLACEMENT LANDS ON, one **Worker** each. Unset means
    every **Machine**, which is what every **Fleet** did before packing existed. `N` above the
    **Machine** count is a refusal naming `sessions=`.
  • `spread=True` PINS IT TO EVERY **MACHINE** and refuses a `workers=` beside it — see
    {@link Fleet.place} for what it does and, more importantly, for what it does NOT promise.

WHAT IT DOES NOT DO YET, stated here so it is not discovered the hard way:

  • A MACHINES-ONLY CONVERGE IS ONLY SAFE WHEN THIS SCOPE IS ALONE ON THE FLEET, and that one
    sentence generates three behaviours below. Pulumi's desired state is TOTAL: a converge that
    omits a placement does not leave it alone, it REMOVES it, and removing it runs that **Worker**'s
    teardown with no error anywhere, on either side. So `hold()` converges only when the **Lease** workflow says
    this **Lease** is the only one, and `place()` refuses on a shared **Fleet**. Within ONE scope
    that hazard is closed rather than avoided: every converge carries EVERY placement this scope has
    made, so two Actors survive one another's converges. Across two scopes it is still a refusal,
    because this side cannot see what the co-tenant placed. `up()` is unaffected: its placement rides
    the entry converge, so the desired state it sends is complete, exactly as before ADR 0037.
  • NO ROLLING HEALTH GATE. Pulumi converges all Machines and reports at the end; there is no
    "stop if the first three fail" (see docs/adr/0019). `ready(at_least=...)` is the partial-fleet
    knob, and it gates the DISPATCH rather than the provision.
  • A PLACEMENT CANNOT RESERVE A MACHINE. "never on the Controller" and "one Artifact to a Machine"
    are not sayable here; `spread=True` asks for one Worker of THIS placement per Machine and says
    nothing about what else is on them. A **Worker** that must not share its **Machine** with
    anything needs a **Fleet** of its own — a different `tag`.

WHERE IT LANDS, AND WHAT IT SPENDS, ARE CODE (ADR 0034 §3, §4). `do_fleet(...)` carries a
DigitalOcean fleet's region, VPC, size, image, machine count and the NAME of the credential it
uses; another cloud gets its own class, because the configurations are not the same shape. Only the
credential's NAME crosses — the infra worker resolves the value at the point of use, so rotating
that secret and starting a new fleet uses the new value with no restart, and a missing or revoked
one fails at the start of `up()` naming the secret, before a Machine exists. The bare word `fleet`
is the DOOR and is reserved for the local Docker case; it is not a provider.
"""

from __future__ import annotations

import math
import asyncio
import functools
import re
import sys
from dataclasses import dataclass, field
from datetime import timedelta
from types import ModuleType
from typing import Any, Mapping

# The stack workflow and the queue it is served on — `control/orchestrator/src/infra.ts:INFRA_QUEUE` and
# `control/orchestrator/src/workflows/stack.ts`. Written independently on this side, like every other
# cross-language literal in the SDK, so `tests/test_fleet_client.py` pins the table.
INFRA_QUEUE = "kontra-infra"
STACK_WORKFLOW = "stackWorkflow"
#: The session converge, on the same worker as the stack — `activities/infra.ts`.
CONVERGE_SESSIONS_ACTIVITY = "convergeFleetSessions"

#: The two **Lease** calls (ADR 0037), on the caller queue beside the two reads above —
#: `control/orchestrator/src/activities/lease.ts`. Written independently here, like everything else that crosses.
HOLD_LEASE_ACTIVITY = "holdFleetLease"
DROP_LEASE_ACTIVITY = "dropFleetLease"

#: What separates a holder from its nonce in a **Lease** id. `control/orchestrator/src/lease.ts:leaseId` builds
#: the same string and `parseLeaseId` reads it back; `shared/conformance/lease.json` is what keeps the two
#: one grammar. A **Lease** held under one spelling and dropped under another is a **Lease** that is
#: never dropped, which is **Machines** billing with nothing left that knows about them — and neither
#: side raises, so nothing but a corpus catches it.
LEASE_SEPARATOR = "#"

# The DigitalOcean project the infra dispatch table knows (`control/orchestrator/src/infra/stacks.ts`).
# A stack outside the known projects is refused server-side; naming it here makes the fqn derivable
# without a round trip. The local Docker sibling is {@link DOCKER_FLEET_PROJECT}.
FLEET_PROJECT = "kontra-fleet"

#: The Pulumi project for a local Docker fleet (`docker_fleet`). Sibling of {@link FLEET_PROJECT};
#: same `stackWorkflow`, no cloud credential. Pinned against `stacks.ts` in `tests/test_fleet_client.py`.
DOCKER_FLEET_PROJECT = "kontra-docker-fleet"

#: The converge-level spelling of ONE placement — `FleetArgs`' pre-packing fields, and every key
#: `programs/fleet.ts:placementsOf` folds into a placement when `placements` is absent.
#:
#: `workers` IS DELIBERATELY NOT IN THIS LIST. It exists only inside a placement, because it is
#: per-Artifact, and `coerceFleetArgs` has no converge-level rule for it — so a `workers` sent up
#: here is narrowed away without a word, which is the `--tmux` failure exactly. The list is written
#: out rather than derived from the entry so that adding a per-placement key does not silently
#: acquire a converge-level spelling it has no reader for. `shared/conformance/placement.json` pins it.
LEGACY_PLACEMENT_KEYS = (
    "actorName",
    "actorVersion",
    "actorEngine",
    "bundleUrl",
    "bundleSha",
    "controller",
    "maxSessions",
    "workerImage",
)

# Where the caller SDK's short reads are served — the same queue `catalog` pages Datasets on.
# `activities/fleet.ts` explains why these two are NOT on the infra queue.
CALLER_QUEUE = "kontra-datasets"
RESOLVE_BUNDLE_ACTIVITY = "resolveBundle"
QUEUE_POLLERS_ACTIVITY = "queuePollers"

#: `control/orchestrator/src/infra/programs/fleet.ts:TAG_RE`, character for character. Validated on this
#: side TOO, not instead: the tag becomes a DigitalOcean tag, an inventory group and part of every
#: machine name, and a caller should learn it is malformed from its own workflow rather than from
#: a Pulumi error several minutes into a converge. `tests/test_fleet_client.py` pins the pair.
TAG_RE = re.compile(r"^[a-z][a-z0-9-]{1,15}$")

#: `control/orchestrator/src/secrets/store.ts:SECRET_NAME_RE`, character for character. A credential NAME is
#: bounded to the same small alphabet on both sides so `DO_TOKEN` and `do-token` cannot be two
#: different secrets, and so a caller learns a name is malformed in its own workflow rather than
#: from a refusal a minute later. `tests/test_fleet_client.py` pins the pair.
SECRET_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")

#: Prefixes a DigitalOcean token carries, refused where a credential NAME is expected.
#:
#: THE ALPHABET CANNOT CATCH THIS ONE, and that is why the list exists. A DigitalOcean token is
#: `dop_v1_` followed by lowercase hex — every character of which a secret name allows — so
#: `credential="dop_v1_0123…"` is a perfectly well-formed NAME, and the mistake it represents (the
#: token pasted where the name goes) would sail through validation and into workflow history in the
#: clear, for the namespace's whole retention, where no rotation can take it back.
#:
#: It is a heuristic and it is worth having anyway: no one names a secret `dop_v1_…`, so it cannot
#: false-positive, and it catches the one paste that cannot be undone. It is NOT a claim that a
#: value can always be told from a name — nothing can do that — which is why the field's
#: documentation says NAME everywhere it appears.
CREDENTIAL_LOOKS_LIKE_A_TOKEN = ("dop_v1_", "doo_v1_", "dor_v1_")


#: The name a fleet's cloud credential has when a caller does not choose one.
#:
#: EMPTY, AND THAT IS THE POINT. An empty credential means "whatever THIS control plane calls its
#: cloud credential" — resolved server-side by `control/orchestrator/src/infra/credential.ts:
#: defaultCloudCredential`, which reads `KONTRA_CLOUD_CREDENTIAL` and falls back to `do-token`. A
#: literal default on this side would be a name baked into every caller's history that a controller
#: whose secret is called `do-prod` could not honour. Name it here when you want the guarantee.
DEFAULT_CREDENTIAL = ""


@dataclass(frozen=True)
class DigitalOcean:
    """Where a DigitalOcean fleet lands, and which of your credentials pays for it (ADR 0034 §3).

    THE PROVIDER IS IN THE CLASS NAME AND NOWHERE ELSE. Nothing downstream of `fleet.up()` spells a
    cloud: the inventory is **Machines**, their addresses and their **Roles**, and no **Worker**, no
    **Batch** and no **Dataset** carries provider identity. A caller may NAME a provider when it
    asks — that is this class — and that is the whole of the exception.

    IT IS NOT A UNION WITH A `provider:` FIELD, and the reason is `region`/`vpc`. A DigitalOcean VPC
    is REGIONAL, so the two are one fact. That pairing is what made kontra un-installable outside
    one project: the wire accepted a region and this SDK exposed `region=` with no VPC knob, so the
    one combination a caller could reach was a NEW region with the OLD VPC — which DigitalOcean
    rejects outright. MEASURED on a fresh nyc1 controller: the first fleet run put its Machines in
    sfo3, where they could not reach the controller's Temporal or Redis, and the run hung on
    `f.ready()` until it was cancelled. So this class refuses a `vpc` without the `region` it
    belongs to, which makes that combination unrepresentable rather than merely discouraged; a
    region on its own means that region's DEFAULT VPC, which is what somebody who set only a region
    meant.

    `ssh_key_ids` are DigitalOcean key ids and they are ACCOUNT-SCOPED: another account's fleet gets
    Machines nobody can log into, which is invisible until a Terminal is opened on one. Left unset
    the controller's own default applies.

    ── THE CREDENTIAL IS A NAME ──────────────────────────────────────────────────────────────────

    `credential="do-prod"` names a secret in the operator's store. IT IS NEVER A VALUE, and that is
    not a convention — the field crosses into workflow history, and the payload codec is a
    claim-check rather than encryption: anything under 128 KiB rides inline in the clear for the
    namespace's whole retention, and a cloud token is about seventy bytes. There is no size at which
    a credential is safely a workflow argument. The infra worker resolves the name at the point of
    use and the value exists for the length of one converge.

    Rotating that secret and starting a new fleet uses the new value, with no service restart and no
    file edit. A missing or revoked one fails at the START of `fleet.up()`, naming the secret.
    """

    #: How many Machines. SCALE; `sessions=` on `up()` is density.
    machines: int = 0
    #: The NAME of the operator secret holding the cloud token. Never the token.
    credential: str = DEFAULT_CREDENTIAL
    #: A DigitalOcean region slug — `nyc3`, `sfo3`. Empty leaves the controller's default.
    region: str = ""
    #: The VPC uuid inside `region`. Requires `region`: see the class docstring.
    vpc: str = ""
    #: A Droplet size slug — `s-1vcpu-2gb`. Empty leaves the controller's default.
    size: str = ""
    #: An image slug. Empty leaves the controller's default.
    image: str = ""
    #: Account-scoped SSH key ids. Empty leaves the controller's default.
    ssh_key_ids: tuple[str, ...] = field(default_factory=tuple)

    @property
    def project(self) -> str:
        """Pulumi project this provider converges. The stack fqn is `{project}/{name}`."""
        return FLEET_PROJECT

    def __post_init__(self) -> None:
        if not isinstance(self.machines, int) or isinstance(self.machines, bool) or self.machines < 0:
            raise ValueError(f"machines must be a non-negative integer, got {self.machines!r}")
        if self.vpc and not self.region:
            # The measured outage, made unrepresentable. A VPC is regional, so a uuid with no region
            # beside it is either a request to put Machines in a region that cannot host that VPC,
            # or a fact half-stated. Both are minutes of provision followed by a hung `ready()`.
            raise ValueError(
                "a DigitalOcean VPC is REGIONAL: name the region it belongs to as well "
                f"(vpc={self.vpc!r} with no region). A region on its own means that region's "
                "default VPC, which is almost always what you want."
            )
        if self.credential.startswith(CREDENTIAL_LOOKS_LIKE_A_TOKEN):
            # NEVER QUOTED BACK. An error is a read path — it reaches a log line, an exception
            # message and a workflow's failure event — so the one thing this must not do is repeat
            # the value it is refusing.
            raise ValueError(
                "credential= is the NAME of a secret in the operator's store, and that looks like a "
                "DigitalOcean token. Put the token in the store — the Settings page, or "
                "`PUT /api/secrets/<name>` — and name that here. A credential passed as a workflow "
                "argument rides inline in history in the clear, and no rotation takes it back."
            )
        if self.credential and not SECRET_NAME_RE.match(self.credential):
            raise ValueError(
                f"credential {self.credential!r} is not a secret name — lowercase letters, digits, "
                "`.`, `-` and `_`, starting with a letter or digit, at most 64 characters. "
                "It is the NAME of a secret in the operator's store, never the token itself."
            )
        object.__setattr__(self, "ssh_key_ids", tuple(str(k) for k in self.ssh_key_ids))

    def args(self) -> dict[str, Any]:
        """What crosses to `stackWorkflow` — NAMES AND NUMBERS ONLY.

        Every unset knob is ABSENT rather than blank. Pulumi is declarative, so a converge sends the
        whole desired state, and an empty string is a request to set the field to empty rather than
        a request to leave it alone — an empty `vpcUuid` asks for a VPC called "".
        """
        out: dict[str, Any] = {"machines": self.machines}
        if self.credential:
            out["credential"] = self.credential
        if self.region:
            out["region"] = self.region
        if self.vpc:
            out["vpcUuid"] = self.vpc
        if self.size:
            out["size"] = self.size
        if self.image:
            out["image"] = self.image
        if self.ssh_key_ids:
            out["sshKeyIds"] = list(self.ssh_key_ids)
        return out


def do_fleet(**kwargs: Any) -> DigitalOcean:
    """`do_fleet(region="nyc3", machines=4, credential="do-prod")` — ADR 0034 §3's spelling.

    The ADR writes the per-provider classes as `doFleet` and `awsFleet`; each SDK spells the same
    name in its own convention, and this is Python's. It is `DigitalOcean(...)` with a name that
    reads as a verb phrase at a call site, and it is the same object either way.

    THE BARE WORD `fleet` IS NOT A PROVIDER. It is the door — `fleet.up(...)`. The local Docker
    provider is {@link docker_fleet}. A reader who finds `fleet` meaning "DigitalOcean" is reading
    pre-0034 code.
    """
    return DigitalOcean(**kwargs)


@dataclass(frozen=True)
class Docker:
    """Where a local Docker fleet lands (Pulumi `@pulumi/docker`, project `kontra-docker-fleet`).

    NO CLOUD CREDENTIAL. The program talks to the engine this process can already reach — the
    socket Compose mounted into `orchestrator-infra` — and a name in the secret store would be a
    lie about what pays for these Machines. They cost a container, not a bill.

    THE SOCKET IS HOST AUTHORITY. Each Machine is a Warden container that creates actor Workers as
    siblings by mounting `/var/run/docker.sock`. That is acceptable for a single-operator laptop
    and is not tenant isolation; the Compose file and the security docs say so in those words.

    `image` is the Warden image, not an actor Artifact. What runs on the Machine is decided at
    `place()`, the same way a DigitalOcean fleet does not bake an actor into cloud-init.
    """

    machines: int = 0
    #: Warden image. Empty leaves the control plane's default (`KONTRA_IMAGE` / `kontra:latest`).
    image: str = ""
    #: Compose network the Machines join so they can dial Temporal/Redis/S3 by service name.
    network: str = ""
    #: Host Docker socket, bind-mounted into each Warden. Empty leaves `/var/run/docker.sock`.
    docker_sock: str = ""

    def __post_init__(self) -> None:
        if not isinstance(self.machines, int) or isinstance(self.machines, bool) or self.machines < 0:
            raise ValueError(f"machines must be a non-negative integer, got {self.machines!r}")

    @property
    def project(self) -> str:
        return DOCKER_FLEET_PROJECT

    @property
    def credential(self) -> str:
        """Always empty: a local Docker fleet has no cloud secret to name."""
        return ""

    def args(self) -> dict[str, Any]:
        """What crosses to `stackWorkflow` — NAMES AND NUMBERS ONLY, no credential."""
        out: dict[str, Any] = {"machines": self.machines}
        if self.image:
            out["image"] = self.image
        if self.network:
            out["network"] = self.network
        if self.docker_sock:
            out["dockerSock"] = self.docker_sock
        return out


def docker_fleet(**kwargs: Any) -> Docker:
    """`docker_fleet(machines=1)` — the local Pulumi Docker provider, sibling of {@link do_fleet}.

    Same `hold` / `up` / Lease / `place` surface. Different Pulumi project, no cloud credential.
    """
    return Docker(**kwargs)


def _hold_retry():
    """The retry policy on `holdFleetLease`.

    IMPORTED LAZILY, like every other `temporalio` name in this module: `kontra.fleet` is imported
    by ordinary caller code as well as by workflow code, and a module-scope SDK import would make the
    former depend on the latter.

    TEN ATTEMPTS RATHER THAN THE DEFAULT'S UNBOUNDED. The retry is not only for networks — a hold
    that arrives while the **Lease** workflow is tearing the **Fleet** down is refused on purpose, and retrying
    is what opens a fresh **Lease** workflow once the old one has closed. That resolves in seconds. An unbounded
    policy would turn a **Fleet** that can never be held into a **Run** that hangs at the first line
    of its scope with nothing to read, which is the failure this repo keeps paying for.
    """
    from temporalio.common import RetryPolicy

    return RetryPolicy(
        initial_interval=timedelta(seconds=1),
        backoff_coefficient=2.0,
        maximum_interval=timedelta(seconds=30),
        maximum_attempts=10,
    )


def lease_id(holder: str, nonce: str) -> str:
    """A **Lease** id: the holder, a `#`, and a nonce. `control/orchestrator/src/lease.ts:leaseId`'s peer.

    THE HOLDER IS IN THE ID ON PURPOSE. A **Lease** is the only thing between a shared **Fleet** and
    its teardown, so the first question about a **Fleet** that will not die is who is holding it, and
    an opaque uuid answers that with a second lookup a leaked **Lease** workflow may not survive to serve.

    THE NONCE IS WHAT MAKES IT A LEASE AND NOT A HOLDER. One **Run** may open two scopes on one
    **Fleet** — a retry, a nested `async with` — and if both claimed the bare run id the inner scope's
    exit would drop the outer scope's claim and destroy **Machines** the outer scope is still using.
    """
    return f"{holder}{LEASE_SEPARATOR}{nonce}"


@dataclass
class Placement:
    """One Artifact's DESIRED STATE on a **Fleet** — what `place()` asks for and `place()` again
    changes.

    A dataclass rather than a bag of arguments because it is the thing that gets COMPARED: the
    second `place()` for the same Actor is the scale operation, and what makes it a scale rather
    than a re-place is that the Artifact did not move. See {@link Fleet.place}.
    """

    actor: str
    version: str
    #: Live Sessions per Worker. None leaves the hosts on their own default. THE DENSITY KNOB a
    #: second `place()` changes; `machines=` on `hold()` is the other axis and is not this one.
    sessions: int | None = None
    #: Where these Machines call home. Empty takes the **Fleet**'s, then the Artifact's.
    controller: str = ""
    #: How many of the **Fleet**'s **Machines** this placement lands on, one **Worker** each. None
    #: means every one of them. See {@link Fleet.place}.
    workers: int | None = None
    #: Whether the caller SAID one Worker per Machine. None is "did not say" and places exactly as
    #: True does; the difference is what each one refuses — see {@link Fleet.place}.
    spread: bool | None = None

    @property
    def ref(self) -> str:
        """`<actor>@<version>` — how a placement is named in an error a human has to act on."""
        return f"{self.actor}@{self.version}"


class PlacementFailed(Exception):
    """`place()` did not put the Artifact on the **Machines**, and the **Machines** are already up.

    THIS EXCEPTION IS THE WINDOW ADR 0037 NAMES, MADE VISIBLE. *"`place()` can fail after `hold()`
    succeeded — no room, image will not pull, digest unsigned — so there is a window in which
    **Machines** are held with nothing on them. Today that failure is inside one call and cannot
    happen."*

    It exists because the underlying error does not say the expensive part. A `resolveBundle` that
    cannot find `nscheck@0.1.1` raises the same sentence whether it was reached from `up()` — where
    no **Machine** exists yet and the typo costs one activity — or from `place()`, where four
    Droplets are already running, already billing, and about to be destroyed by this scope's own
    exit with nothing having run on them. Retrying pays for the provision a second time.

    So the count of standing **Machines** is in the message. That number is the cost of the window,
    and it is the number that tells a reader whether to fix the typo and re-run or to hold the
    **Fleet** open (`destroy_on_exit=False`) while they do.
    """

    def __init__(self, fleet: str, ref: str, machines: int, cause: BaseException) -> None:
        self.fleet, self.ref, self.machines = fleet, ref, machines
        super().__init__(
            f"could not place {ref} on {fleet}: {cause}. "
            f"{machines} Machine(s) are already up and holding this Fleet with nothing on them — "
            f"leaving this scope drops the Lease and they are destroyed having run nothing, and a "
            f"retry pays for the provision again. `fleet.up()` cannot reach this state: it resolves "
            f"the Artifact before the first Machine exists."
        )


class FleetNotReady(Exception):
    """`ready()` gave up waiting for Workers to poll.

    Carries what was actually observed, because the two ways to reach this are opposite problems:
    `pollers=0` with no error means the Machines exist and their Workers are not running (a failed
    install, a crash-looping unit); `error` set means Temporal could not be asked at all, and how
    many Workers are polling is simply unknown.
    """

    def __init__(self, fleet: str, queue: str, want: int, got: int, error: str = "") -> None:
        self.fleet, self.queue, self.want, self.got, self.error = fleet, queue, want, got, error
        why = f"last describe failed: {error}" if error else f"{got}/{want} polling"
        super().__init__(
            f"fleet {fleet!r} not ready: {why} on {queue}. "
            f"`kontra workers list` shows the same view; if the Machines are up, the Worker is "
            f"not — check `systemctl status kontra-handler` on one of them."
        )


class Fleet:
    """Held capacity, for the length of the `async with`. Built by `hold()` or `up()`; never
    directly."""

    def __init__(
        self,
        *,
        name: str,
        tag: str,
        provider: DigitalOcean | Docker,
        controller: str,
        destroy_on_exit: bool,
        timeout: timedelta,
        lease_ttl: timedelta | None = None,
    ) -> None:
        #: The **Fleet**'s name — the last segment of {@link fqn}. NOT invented at this level: `up()`
        #: derives it from the Artifact it places and `hold()` from the tag. See {@link fqn}.
        self._name = name
        self.tag = tag
        #: WHERE these Machines land and WHICH credential pays for them, expressed in code. One
        #: object rather than a spread of keyword arguments, because a DigitalOcean region and VPC
        #: are one fact and a bag of optional strings cannot say so (ADR 0034 §3).
        self.provider = provider
        self.controller = controller
        self.destroy_on_exit = destroy_on_exit
        self.timeout = timeout
        #: How long this scope's **Lease** survives without renewal. None takes the control plane's
        #: default (`control/orchestrator/src/lease.ts:LEASE_TTL_MS`, an hour) — the same "empty means whatever
        #: THIS control plane says" arrangement `credential` has, and for the same reason: a number
        #: baked in here is one every caller's history carries and no operator can move.
        self.lease_ttl = lease_ttl
        #: Machine name → {name, host, publicIp, tag}. The one-way handoff Fleet gives Execution.
        self.inventory: dict[str, Any] = {}
        #: Actor name → {@link Placement}. THE DESIRED STATE, and the reason `place()` needs no
        #: `scale()` beside it: the second call overwrites the entry and re-converges the difference.
        #: `up()` puts one in here before the scope opens, which is what makes it sugar rather than a
        #: second implementation.
        self._placements: dict[str, Placement] = {}
        #: `<actor>@<version>` → what `resolveBundle` answered. CACHED, and that is a safety
        #: property rather than a saving: a version is a MUTABLE registry tag, so re-resolving on a
        #: scale would swap the Artifact under a **Fleet** that is already running work, and nothing
        #: would report it. See {@link place}.
        self._bundles: dict[str, dict[str, Any]] = {}
        self._up = False
        #: This scope's **Lease** id while it holds one. Empty before `hold` and after `drop`, which
        #: is what makes a second drop a no-op on this side as well as on the **Lease** workflow's.
        self._lease = ""
        #: How many **Leases** were on this **Fleet** when this scope took its own — 1 means alone.
        self._leases = 0

    @property
    def machines(self) -> int:
        """How many Machines this fleet asked for. Reads off the provider config — the one place
        scale is stated — so the two can never disagree."""
        return self.provider.machines

    @property
    def credential(self) -> str:
        """The NAME of the secret this fleet's cloud calls are made with, or empty for this control
        plane's default. NEVER a value: see {@link DigitalOcean}."""
        return self.provider.credential

    @property
    def fqn(self) -> str:
        """`kontra-fleet/<name>` — the stack, and the id of the workflow that owns it.

        DERIVED, NOT INVENTED, AND THE TWO DOORS DERIVE IT FROM DIFFERENT THINGS because they are
        naming different things:

          • `up()` names a **Fleet** after the ONE Artifact it places — `<actor>-<version>`. Two
            runs both wanting `nscheck@0.1.0` Machines want the SAME Machines, so the collision is
            a real answer. That derivation is unchanged and every stack this repo has ever created
            is still reachable under it, which is why `up()` did not move to the rule below.
          • `hold()` names it after the TAG, because that is what a **Fleet** is once it is capacity
            rather than one Artifact's provisioning (ADR 0037: *"it is not named after what it
            runs"*). The tag is already the DigitalOcean tag `kontra-<tag>`, the inventory group and
            the `kf-<tag>-NN` machine name, so `kontra-fleet/dns` is the stack whose Machines are
            called `kf-dns-01`, and two runs asking for `dns` capacity share it.

        NEITHER IS A CALLER-INVENTED STACK NAME, which was tried and cut. That form named "one
        bounded period of work" — a period nothing in kontra measured — defaulting to the role
        beside it, so it collided when two scopes happened to share a role and did not collide when
        two scopes wanted the same Machines. A tag is not that: it is a label that already appears
        on the infrastructure, and `kf-dns-01` is the operator's own name for the thing.

        It is also what makes a LEAKED fleet recoverable. The id is the only handle a human has,
        and a uuid would name an orphan after something nobody knows.
        """
        return f"{self.provider.project}/{self.name}"

    @property
    def name(self) -> str:
        """This fleet's name — the last segment of {@link fqn}. See there for where it comes from."""
        return self._name

    @property
    def placements(self) -> dict[str, Placement]:
        """What this **Fleet** is asked to run, by actor name. EMPTY IS A STATE WORTH READING: it is
        the hold-without-place window, and it means Machines are up with nothing on them."""
        return dict(self._placements)

    @property
    def _placement(self) -> Placement | None:
        """The single placement, or None while nothing is placed.

        RAISES ON A PACKED **FLEET** RATHER THAN PICKING ONE. Every singular accessor below —
        `actor`, `version`, `sessions`, `queue`, `bundle_sha` — is defined only while a **Fleet**
        places one Artifact, and each of them answered `next(iter(...))` when this could not happen.
        Now that it can, returning the first is a lie a caller cannot see: `f.queue` would name one
        of two queues and a dispatch loop built on it would be watching the wrong one. The plural
        forms are `placements`, `queue_for(p)` and `ready()` with no argument, and the message names
        them.
        """
        if len(self._placements) > 1:
            raise ValueError(
                f"{self.fqn} places "
                f"{', '.join(sorted(p.ref for p in self._placements.values()))} and this reads one "
                f"of them. Use .placements, .queue_for(placement), or ready() with no argument, "
                f"which waits for all of them."
            )
        return next(iter(self._placements.values()), None)

    @property
    def actor(self) -> str:
        """The Actor this **Fleet** places, or empty while nothing is placed on it."""
        p = self._placement
        return p.actor if p else ""

    @property
    def version(self) -> str:
        """That Actor's version, or empty while nothing is placed."""
        p = self._placement
        return p.version if p else ""

    @property
    def sessions(self) -> int | None:
        """Live Sessions per Worker for the current placement, or None for the hosts' default."""
        p = self._placement
        return p.sessions if p else None

    def workers_for(self, placement: Placement) -> int:
        """How many **Machines** one placement lands on — its `workers=`, or all of them.

        THE ONE PLACE THE NUMBER IS DERIVED, so `spread=True`'s meaning and the wire's `workers` key
        cannot disagree. `spread=True` is exactly "every Machine", which is why it refuses a
        `workers=` beside it: two ways to say one number is two ways to get it wrong.
        """
        return self.machines if placement.workers is None else placement.workers

    def queue_for(self, placement: Placement) -> str:
        """The shared queue one placement's Workers poll."""
        from kontra.catalog import shared_queue

        return shared_queue(placement.actor, placement.version)

    @property
    def queue(self) -> str:
        """The shared queue this fleet's Workers poll — what `ready()` watches. Empty while nothing
        is placed, because there is no queue until something is."""
        p = self._placement
        return self.queue_for(p) if p else ""

    @property
    def bundle_sha(self) -> str:
        """The Artifact this scope actually placed.

        Worth reading and worth logging: a version is a MUTABLE registry TAG and this is the
        immutable thing it resolved to, so recording it is what keeps "what did this run place"
        answerable after somebody deploys again.
        """
        p = self._placement
        return str(self._bundles.get(p.ref, {}).get("bundleSha", "")) if p else ""

    @property
    def lease(self) -> str:
        """This scope's **Lease** id while it holds one, `<run>#<nonce>`, else empty.

        Worth logging and worth printing: it is the exact string `kontra fleet leases` shows, and the
        exact string that must be dropped. A **Lease** held under one spelling and dropped under
        another is one that is never dropped.
        """
        return self._lease

    @property
    def shared(self) -> bool:
        """Whether another **Run** was already holding this **Fleet** when this scope claimed it.

        It decides whether this scope's own FAILURE may tear the **Fleet** down — see `_converge`.
        """
        return self._leases > 1

    def hosts(self) -> list[str]:
        """Private VPC addresses, sorted by machine name. How the Controller reaches them."""
        return [self.inventory[k].get("host", "") for k in sorted(self.inventory)]

    def cost_hourly(self) -> float:
        """What this **Fleet** costs per hour, in USD, summed across its **Machines**.

        The per-machine price is DigitalOcean's own list price, read from their sizes endpoint
        when the **Fleet** converged — not a table in this repo, which would be wrong the first
        time a price changed and wrong silently.

        **0.0 means UNKNOWN, never free.** A docker **Fleet** has no meter, and the cloud lookup
        is allowed to fail rather than block a converge. `cost_words` is the thing to render;
        it refuses to invent a figure it does not have.
        """
        return sum(float(m.get("priceHourly") or 0.0) for m in self.inventory.values())

    def cost_words(self, seconds: float | None = None) -> str:
        """One sentence an operator can act on: the rate, and the spend so far if asked.

        A **Fleet** is the only part of a **Run** that bills by wall clock, so a **Run** that
        never says what it is holding is a **Run** whose cost is discovered on an invoice. This is
        the sentence a `workflow.logger` line carries.
        """
        n = len(self.inventory)
        sizes = {str(m.get("size") or "?") for m in self.inventory.values()}
        shape = f"{n} x {sorted(sizes)[0]}" if len(sizes) == 1 else f"{n} machine(s)"
        rate = self.cost_hourly()
        if rate <= 0:
            return f"{shape} — no cloud meter on this Fleet, so nothing to price"
        words = f"{shape} at ${rate:.4f}/hr (${rate * 24:.2f}/day)"
        if seconds is not None and seconds > 0:
            # ROUNDED UP TO THE HOUR, because that is how DigitalOcean bills a Droplet: a Fleet
            # held for 70 minutes is charged two hours, and reporting 1.17 would understate every
            # short Run.
            hours = max(1, math.ceil(seconds / 3600.0))
            words += f"; held {seconds / 60:.0f} min, billed {hours}h = ${rate * hours:.2f}"
        return words

    def __repr__(self) -> str:  # pragma: no cover - debugging affordance
        state = f"{len(self.inventory)} machines" if self._up else "not converged"
        placed = ", ".join(sorted(p.ref for p in self._placements.values())) or "nothing placed"
        return f"<kontra fleet {self.fqn} tag={self.tag} {state}, {placed}>"

    async def __aenter__(self) -> "Fleet":
        # HOLD FIRST, CONVERGE SECOND, and the order is the whole safety.
        #
        # A **Fleet** is destroyed when its last **Lease** drops. Converging before claiming leaves a
        # window in which another **Run**'s exit takes the last **Lease** away and tears down the
        # **Machines** this scope has just paid four minutes for — and the failure would present as a
        # `ready()` that never opens on a fleet that provisioned perfectly.
        await self._hold()
        try:
            if self._placements or not self.shared:
                await self._converge()
            else:
                # A MACHINES-ONLY CONVERGE ON A SHARED FLEET IS A TEARDOWN OF SOMEBODY ELSE'S WORK.
                #
                # Pulumi's desired state is TOTAL. This scope has nothing placed yet, so the args it
                # would send carry no `bundleUrl` — which is not "leave the placement alone", it is
                # "there should be no placement", and the co-tenant's `command.remote.Command` is
                # deleted, running that Worker's teardown and stopping it. Nothing raises on
                # either side: their `ready()` has already passed, and their next Batch waits on a
                # queue nobody polls until ScheduleToStart fires.
                #
                # So a later holder takes the capacity that is there and converges nothing. That is
                # also what makes `hold()` on an existing **Fleet** the cheap path: no Pulumi, no
                # four minutes, one activity. `up()` never reaches this branch — its placement is
                # staged before the scope opens, so its desired state is complete.
                from temporalio import workflow

                workflow.logger.info(
                    "fleet already held by another Run — taking its Machines, converging nothing",
                    extra={"fqn": self.fqn, "leases": self._leases},
                )
        except BaseException:
            # A CONVERGE THAT DID NOT FINISH MUST NOT LEAVE THIS SCOPE'S CLAIM BEHIND. `__aexit__`
            # never runs when `__aenter__` raises, so this is the only place that can let go — and a
            # **Lease** nobody drops is a **Fleet** nothing collects until its clock runs out.
            await self._drop()
            raise
        return self

    async def __aexit__(self, exc_type, exc, tb) -> bool:
        if self._up and not self._placements:
            # THE WINDOW, SAID OUT LOUD ON THE WAY OUT. A scope that converged Machines and never
            # placed anything on them bought capacity and used none of it — which is either a
            # `place()` that failed (see {@link PlacementFailed}) or a `hold()` somebody meant to
            # follow with one. Neither is an error and both are money, so it is a line and not a
            # raise. `up()` cannot reach this: its placement is staged before the scope opens.
            from temporalio import workflow

            workflow.logger.warning(
                "fleet held with nothing placed on it — %d Machine(s) on %s ran nothing this scope",
                len(self.inventory),
                self.fqn,
                extra={"fqn": self.fqn, "machines": len(self.inventory)},
            )
        await self._drop()
        return False  # never swallow the body's failure — a fleet that came up and did no work
        #                is still a failed run, and it must be seen as one

    async def _hold(self) -> None:
        """Claim the **Fleet** for this scope (ADR 0037).

        THE HOLDER IS THIS RUN'S WORKFLOW ID, which is what makes the clock safe: at a **Lease**'s
        deadline the **Lease** workflow asks Temporal whether that workflow is still RUNNING and renews the ones
        that are. So this scope pays NOTHING to stay held — no heartbeat, no renew timer, no events
        at all between `hold` and `drop`, at any duration — and a **Run** that dies mid-flight is
        still noticed. A holder-side renewal would have put a timer loop in every caller's history
        and would have failed in exactly the case it existed for, since a **Run** nobody is running
        cannot renew anything.

        A FLEET THIS SCOPE WILL NOT DROP IS HELD BY NOBODY. `destroy_on_exit=False` adopts the
        **Fleet** for a handoff, so naming this **Run** as its holder would end it the moment this
        **Run** does — the **Lease** workflow would find the holder closed at the first deadline and tear the
        **Fleet** down, which is the opposite of adopting it. An unattributed **Lease** is never
        renewed and never asked about: it lives exactly one TTL, which is the handoff window, and
        `lease_ttl=` is how a longer one is asked for.
        """
        from temporalio import workflow

        holder = workflow.info().workflow_id if self.destroy_on_exit else ""
        # `workflow.uuid4()` is the SDK's DETERMINISTIC uuid — a replay produces the same id, so a
        # workflow task that is re-run does not claim a second **Lease** it will only drop once.
        self._lease = lease_id(holder, workflow.uuid4().hex[:8])

        args: dict[str, Any] = {"stackFqn": self.fqn, "lease": self._lease, "holder": holder}
        if self.lease_ttl is not None:
            args["ttlMs"] = int(self.lease_ttl.total_seconds() * 1000)
        if self.credential:
            # WHICH credential the eventual teardown makes its provider calls with. A NAME, never a
            # value. It rides the hold rather than the destroy because the **Lease** workflow outlives this
            # **Run**: the last holder out is very often not the one that provisioned.
            args["credential"] = self.credential

        out = dict(
            await workflow.execute_activity(
                HOLD_LEASE_ACTIVITY,
                args,
                task_queue=CALLER_QUEUE,
                start_to_close_timeout=timedelta(seconds=30),
                # RETRIED, AND NOT ONLY FOR NETWORKS. A hold that arrives while the **Lease** workflow is
                # tearing the **Fleet** down is refused, on purpose (`control/orchestrator/src/workflows/lease.ts`);
                # the retry is what opens a fresh **Lease** workflow once the old one has closed. Ten attempts
                # rather than the default's unbounded, so a **Fleet** that can never be held fails
                # the run instead of hanging it.
                retry_policy=_hold_retry(),
                summary=f"hold {self.name}",
            )
        )
        self._leases = int(out.get("leases") or 1)
        workflow.logger.info(
            "fleet lease held",
            extra={"fqn": self.fqn, "lease": self._lease, "leases": self._leases},
        )

    async def _resolve(self, placement: Placement) -> dict[str, Any]:
        """WHICH ARTIFACT. Resolved once per `<actor>@<version>` and remembered.

        THE CACHE IS A SAFETY PROPERTY, NOT A SAVING. A version is a MUTABLE registry tag, so
        `resolveBundle` asked twice in one scope may legitimately answer with two different shas —
        somebody deployed in between. A second `place()` that only changes `sessions=` would then
        silently swap the Artifact under **Machines** that are already running work, mid-**Run**,
        and nothing anywhere would report it: the converge succeeds, the install re-runs, and half
        the **Run**'s output came from code the other half did not use. So the resolve happens once
        per Artifact and the SCALE re-converges the sha this scope already placed.

        Asking for a genuinely different version is a different Artifact and therefore a different
        placement, which {@link place} refuses for its own reason.
        """
        from temporalio import workflow

        cached = self._bundles.get(placement.ref)
        if cached is not None:
            return cached
        bundle = dict(
            await workflow.execute_activity(
                RESOLVE_BUNDLE_ACTIVITY,
                {
                    "actor": placement.actor,
                    "version": placement.version,
                    "controller": placement.controller or self.controller,
                },
                task_queue=CALLER_QUEUE,
                start_to_close_timeout=timedelta(minutes=1),
                summary=f"resolve {placement.ref}",
            )
        )
        self._bundles[placement.ref] = bundle
        return bundle

    async def _converge(self) -> None:
        """Send this **Fleet**'s whole desired state to Pulumi, through `stackWorkflow`.

        EVERY CONVERGE SENDS THE WHOLE THING, which is not a style choice — Pulumi is declarative,
        so an omitted field is a request to REMOVE what it describes rather than a request to leave
        it alone. `machines` in particular: absent, the orchestrator coerces it to 0 and the
        converge destroys every Droplet in the **Fleet**. That is why there is one builder here and
        `place()` calls it rather than sending a patch.

        SINCE PACKING, "THE WHOLE THING" IS EVERY PLACEMENT AND NOT JUST THIS ONE, and that sentence
        is what makes two Actors survive one another's converges. `place("subfinder", …)` on a
        **Fleet** already running `nscheck` sends BOTH, because a desired state carrying only
        `subfinder` is a request to delete `nscheck`'s install command — which runs its teardown and
        stops its **Worker**, successfully, with nothing raising on either side. The loop below is
        therefore over `self._placements` rather than over the one being placed, and
        `tests/test_fleet_hold_place.py` holds it by placing a second Actor and reading the first
        one back out of the SECOND converge's arguments.
        """
        from temporalio import workflow

        # SORTED BY ACTOR NAME, matching `programs/fleet.ts:placementsOf`. The order decides each
        # Worker's metrics port on a packed Machine, so two converges that differ only in the order
        # a caller happened to call `place()` in must not re-install anything.
        placements = [self._placements[k] for k in sorted(self._placements)]

        # WHERE, WHAT AND WITH WHICH CREDENTIAL — the provider's half of the desired state, plus
        # the label and the Artifacts. `provider.args()` omits every unset knob for the reason it
        # documents; the credential in it is a NAME, and the only thing about the cloud account
        # that ever crosses this boundary.
        args: dict[str, Any] = {"tag": self.tag, **self.provider.args()}
        if self.controller:
            args["controller"] = self.controller
        if placements:
            entries: list[dict[str, Any]] = []
            for p in placements:
                entry = dict(await self._resolve(p))
                if p.controller:
                    entry["controller"] = p.controller
                elif self.controller:
                    entry["controller"] = self.controller
                if p.sessions:
                    entry["maxSessions"] = p.sessions
                # SENT ONLY WHEN THE CALLER SAID SOMETHING ABOUT IT. Absent means "every Machine",
                # which is what the program defaults to, so an unasked-for placement's wire is
                # byte-identical to what it was before packing existed.
                if p.workers is not None or p.spread is True:
                    entry["workers"] = self.workers_for(p)

                # The Controller reaches the args one of two ways — resolved with the Artifact
                # above, or named by the caller — and if it arrived by NEITHER, say so here. The
                # converge would otherwise fail deep inside the stack program with `controller="" is
                # not safe to place on a Machine`, which is true, unactionable from this side, and
                # arrives a minute later.
                #
                # ONLY WHEN SOMETHING IS BEING PLACED. A Machines-only converge fetches no Artifact
                # and registers no Worker, so it has nothing to call home about; requiring an address
                # there would refuse `hold()` on a control plane that is perfectly able to provide
                # one later.
                if not entry.get("controller"):
                    raise ValueError(
                        f"no Controller address for {p.ref} on this fleet: Machines fetch the "
                        "Artifact from it and register their Worker with it. Set KONTRA_CONTROLLER "
                        "where the orchestrator runs, or pass controller= to fleet.up()"
                    )
                entries.append(entry)
            args["placements"] = entries
            # THE SINGLE-PLACEMENT SPELLING, KEPT FOR EXACTLY ONE PLACEMENT. `coerceFleetArgs` folds
            # these into a one-entry array when `placements` is absent, and every stack this repo has
            # ever converged was created from them — so a Fleet holding one Artifact sends what it has
            # always sent and Pulumi sees no change it did not ask for. A PACKED Fleet sends the array
            # ALONE: there is no honest scalar answer for two Artifacts, and picking one would be a
            # second, disagreeing desired state riding beside the true one.
            #
            # AN EXPLICIT LIST RATHER THAN `args.update(entries[0])`, and the difference is a key the
            # reader drops. `workers` exists ONLY inside a placement — there is no converge-level
            # spelling of it, because it is per-Artifact — so copying the whole entry up put a
            # `workers` at the top level that `coerceFleetArgs` narrows away without a word. Harmless
            # today because the array wins, and exactly the shape of `--tmux`: a key a writer sends,
            # a reader discards, and nothing anywhere reports. Caught by `shared/conformance/placement.json`.
            if len(entries) == 1:
                for legacy in LEGACY_PLACEMENT_KEYS:
                    if legacy in entries[0]:
                        args[legacy] = entries[0][legacy]

        # 2) CONVERGE, as a child workflow whose id is the stack. Starting it is what claims the
        #    stack: a run colliding with a `kontra fleet` command in flight fails HERE, which
        #    is the right answer — two writers on one Pulumi state is the failure the id prevents.
        result = await workflow.execute_child_workflow(
            STACK_WORKFLOW,
            {
                "stackFqn": self.fqn,
                "op": "up",
                "args": args,
                # THE SAGA LEG IS MINE ONLY WHILE THE FLEET IS.
                #
                # `compensateOnCancel` tears the WHOLE STACK down when this converge does not finish.
                # That is right when this **Run** is alone on the **Fleet** — a cancelled provision
                # leaving **Machines** running is the worst outcome available — and it is a way to
                # delete somebody else's **Machines** the moment a **Fleet** is shared: my failed
                # `up` is not a reason to destroy the capacity another **Run** is working on. So it
                # is off when the **Lease** workflow reported a co-tenant, and the **Lease** dropped by
                # `__aenter__`'s except clause is what collects the **Fleet** instead — at zero
                # **Leases**, which is the only condition that has ever been safe.
                "compensateOnCancel": not self.shared,
            },
            id=self.fqn,
            task_queue=INFRA_QUEUE,
            execution_timeout=self.timeout,
            static_summary=f"fleet up {self.fqn} ({self.machines}x {self.tag})",
        )
        outputs = (result or {}).get("outputs") or {}
        self.inventory = dict(outputs.get("inventory") or {})
        self._up = True
        workflow.logger.info(
            "fleet up",
            extra={
                "fqn": self.fqn,
                "machines": len(self.inventory),
                # EVERY PLACEMENT, NOT `self.actor`. That accessor now raises on a packed Fleet
                # (see `_placement`), and a log line that raised would turn a successful converge
                # into a failed one at the last statement.
                "placed": ", ".join(p.ref for p in placements) or "nothing",
            },
        )
        await self._converge_sessions()

    async def _converge_sessions(self) -> None:
        """Give every **Machine** a tmux session, so its pane has something to show.

        WHY THE SCOPE ASKS AND THE PROGRAM DOES NOT. ADR 0020 made session existence a Temporal
        converge rather than an argument on the provision, and `programs/fleet.ts` says so where
        somebody would reach for it: *"There is deliberately no `tmux` arg"*. The consequence went
        unwritten for a release: `kontra fleet up --tmux` could create a session and `fleet.up()`
        structurally could not, so every workflow-raised **Machine** drew "no session — Converge
        session" on the Monitor forever. That reads as an error while being the DESIGNED state,
        which is worse than either, and the operator's only path was a button.

        The converge is still a converge — this starts the same `tmuxSessionWorkflow` the Monitor's
        button starts. What changes is that a **Run** no longer has to be told to press it.

        IT CANNOT FAIL THE RUN, and that is deliberate rather than defensive. A pane is an
        observability affordance and the **Workers** are already polling: a **Fleet** whose sessions
        did not converge still produces every row of its output. Raising here would turn a cosmetic
        miss into a failed provision — and the sessions are converged AFTER `_up` is set, so a
        **Fleet** that reached this line is a **Fleet** that exists and will be torn down by the
        scope regardless of what happens next.
        """
        # Imported HERE, like every other Temporal reference in this file: the module is resolved
        # inside the workflow sandbox at call time, and a top-level import would make `kontra`
        # unimportable outside one.
        from temporalio import workflow
        from temporalio.common import RetryPolicy

        # A HOLD WITH NOTHING PLACED HAS NO SESSION TO CONVERGE. `machinesFromStack` names only
        # Machines carrying a placement, so this would cost an activity round-trip to be told
        # "the stack names no Machines with a placement" — once per bare `hold()`, and again on
        # every `place()` that follows. The sessions are converged by the placement's own converge.
        if not self._placements:
            return

        try:
            out = dict(
                await workflow.execute_activity(
                    CONVERGE_SESSIONS_ACTIVITY,
                    {"stackFqn": self.fqn},
                    task_queue=INFRA_QUEUE,
                    start_to_close_timeout=timedelta(minutes=2),
                    retry_policy=RetryPolicy(maximum_attempts=2),
                )
                or {}
            )
        except Exception as exc:  # noqa: BLE001 - a pane is never worth a failed Run
            workflow.logger.info("sessions not converged", extra={"fqn": self.fqn, "why": str(exc)})
            return
        refused = out.get("refused") or []
        workflow.logger.info(
            "sessions converged",
            extra={
                "fqn": self.fqn,
                "machines": len(out.get("converged") or []),
                # NAMED, not counted. "2 refused" tells an operator nothing they can act on.
                "refused": "; ".join(f"{r.get('machine')}: {r.get('why')}" for r in refused) or "none",
            },
        )

    async def place(
        self,
        actor: str,
        version: str,
        *,
        sessions: int | None = None,
        controller: str = "",
        workers: int | None = None,
        spread: bool | None = None,
    ) -> Placement:
        """Put an Actor on this **Fleet**'s **Machines**, and keep it there. IDEMPOTENT DESIRED
        STATE.

        actor / version   an Artifact already published by `kontra build` / `kontra fleet deploy`
        sessions          live Sessions per **Worker** (`KONTRA_MAX_PARALLEL_SESSIONS`). None leaves
                          the hosts on their own default of 4. DENSITY
        controller        where these Machines call home. Empty takes the **Fleet**'s, then the one
                          resolved alongside the Artifact
        workers           how many of the **Fleet**'s **Machines** this Artifact lands on, one
                          **Worker** each. None means every one of them. COUNT
        spread            True pins `workers` to the **Machine** count — one **Worker** per
                          **Machine**, on every **Machine**. False is the explicit spelling of a
                          `workers=` below that count, and says nothing on its own

        ── CALLING IT AGAIN IS THE SCALE OPERATION, WHICH IS THE POINT (ADR 0037) ─────────────────

        There is no `scale()` and there is not going to be one. `place("nscheck", "0.1.0",
        sessions=8)` followed later by `place("nscheck", "0.1.0", sessions=16)` re-converges the
        **Fleet** with the new density, mid-**Run**, on **Machines** that are already working. That
        is not a special case in the implementation — it is the same call with a different desired
        state, which is exactly what makes mid-run change need no new concept.

        The Artifact does NOT move when you do that: the sha this scope resolved the first time is
        the sha it re-places. See {@link _resolve} for why re-resolving would be a silent
        correctness bug rather than a cost.

        ── CALLING IT FOR A SECOND ARTIFACT IS PACKING, AND THERE IS NO FLAG FOR IT ───────────────

        `place("subfinder", "0.2.0")` on a **Fleet** already running `nscheck` puts a second
        **Worker** on the same **Machines**, and the two share the **Machine**'s egress address.
        That consequence is the whole of ADR 0037's trade and it is not hidden behind a keyword: the
        second call IS the request, and a caller who did not want it did not make it.

        Every converge this scope sends carries EVERY placement, which is what keeps the two alive.
        A converge naming only `subfinder` would be a request to delete `nscheck`'s install command,
        which runs its teardown and stops its **Worker** — successfully, silently. See
        {@link _converge}.

        ── WHAT `spread=True` PROMISES, AND THE ONE THING IT DOES NOT ─────────────────────────────

        It promises that no two **Workers** OF THIS PLACEMENT share a **Machine**, on every
        **Machine** the **Fleet** has. That is `subfinder`'s operating rule — one worker per droplet,
        concurrency 1, and a source address it does not share with another `subfinder` — and it is
        the property that was an invisible invariant before packing and is a request after it.

        IT DOES NOT RESERVE THE **MACHINE**. ADR 0037's own snippet places `nscheck` and then
        `subfinder(spread=True)` on ONE four-**Machine** **Fleet**, so the two are packed together
        and the address IS shared with the co-placed **Worker**. Reading `spread=True` as "nothing
        else runs here" would make that snippet unsatisfiable, and a promise this call cannot keep is
        worse than a narrower one it can. A **Worker** that must have a **Machine** to itself needs a
        **Fleet** of its own — a different `tag`.

        ── WHAT IT REFUSES, AND WHY EACH REFUSAL IS BETTER THAN THE ALTERNATIVE ───────────────────

        `workers=` BESIDE `spread=True`. Two ways to say one number, and the wrong resolution is
        silent: honouring `workers=2` would hand a caller two source addresses where they asked for
        one per **Machine**, and honouring `spread` would ignore an argument they typed.

        `workers=` ABOVE THE **MACHINE** COUNT. A placement puts at most one **Worker** on a
        **Machine**, so eight on four is not a **Fleet** this call can build; truncating to four
        would leave a caller believing in four **Workers** that do not exist. `sessions=` is the
        argument for more concurrency on the **Machines** you have.

        `spread=False` ON ITS OWN. It is the explicit spelling of "this placement does not need every
        **Machine**", which is `workers=`; with no count beside it, it asks for nothing and changes
        nothing, and accepting an argument that does nothing is how `--tmux` rode a whole release.

        A SHARED FLEET. When the **Lease** workflow reported a co-tenant, this scope may not rewrite the
        **Fleet**'s desired state: the converge would carry THIS scope's placements and therefore
        delete theirs, running their **Worker**'s teardown while their **Run** is mid-dispatch. The
        packing above works WITHIN one scope, which is the only place this side can see the whole
        desired state; a second holder uses what is already there — `ready()` and
        `catalog.actor(...)` need no placement of their own.

        OUTSIDE THE SCOPE. Converging **Machines** while holding no **Lease** is capacity nothing
        will collect but the **Lease** workflow's clock.
        """
        from temporalio import workflow

        actor, version = (actor or "").strip(), (version or "").strip()
        if not actor or not version:
            raise ValueError(
                "place(actor, version): a placement names one published Artifact, and both halves "
                "are required — a version is not optional because a Fleet places a sha, not a name"
            )
        if sessions is not None and (not isinstance(sessions, int) or isinstance(sessions, bool) or sessions < 1):
            raise ValueError(f"sessions must be a positive integer or None, got {sessions!r}")
        if workers is not None and (not isinstance(workers, int) or isinstance(workers, bool) or workers < 1):
            raise ValueError(f"workers must be a positive integer or None, got {workers!r}")
        if spread is True and workers is not None:
            raise ValueError(
                f"spread=True already says how many Workers this placement runs — one per Machine, "
                f"and this Fleet has {self.machines} — so workers={workers} beside it is two ways to "
                f"say one number. Drop one: spread=True for every Machine's egress address, or "
                f"workers={workers} for that many of them."
            )
        if spread is False and workers is None:
            raise ValueError(
                "spread=False is the explicit spelling of `workers=` — this placement does not need "
                "every Machine — and on its own it asks for nothing and changes nothing. Say how "
                "many Machines it should land on (workers=N), or drop it: a placement that names "
                "neither takes every Machine, which is what a Fleet has always done."
            )
        if spread is True and self.machines < 1:
            raise ValueError(
                f"spread=True asks for one Worker per Machine and {self.fqn} has {self.machines}. "
                f"There is no Machine to give this placement an egress address of its own."
            )
        if workers is not None and workers > self.machines:
            # A PLACEMENT PUTS AT MOST ONE WORKER ON A MACHINE — see this method's docstring and
            # `cli/warden/driver.go`'s label. Truncating would leave the caller believing in Workers that
            # were never built, which is the shape of failure this repo keeps paying for.
            raise ValueError(
                f"workers={workers} on {self.fqn}, which has {self.machines} Machine(s): a placement "
                f"puts at most ONE Worker on a Machine, because two of {actor}@{version} there would "
                f"share a label, a unit name and a queue and nothing could tell them apart. Raise "
                f"machines= on the hold, or ask for more concurrency per Worker with sessions=."
            )
        if not self._lease:
            raise RuntimeError(
                f"place() outside the fleet scope: {self.fqn} holds no Lease here, so converging "
                f"Machines onto it would leave capacity nothing collects but the **Lease** workflow's clock. "
                f"Place inside `async with fleet.hold(...)`."
            )
        if self.shared:
            raise RuntimeError(
                f"{self.fqn} is held by {self._leases} Runs and a placement is the whole Fleet's "
                f"desired state, so converging this one would delete the co-tenant's placement and "
                f"stop their Workers — with nothing raising on either side. Use the placement that "
                f"is already there (ready() and catalog.actor() need none of your own), or hold a "
                f"Fleet under a tag nobody else is using."
            )
        prior = self._placements.get(actor)

        placement = Placement(
            actor=actor,
            version=version,
            sessions=sessions,
            controller=controller,
            workers=workers,
            spread=spread,
        )
        self._placements[actor] = placement
        try:
            await self._converge()
        except BaseException as e:
            # ROLLED BACK, so `ready()` does not wait for a placement that is not there and the
            # exit's "nothing placed" line still tells the truth. The desired state is what this
            # scope BELIEVES is on the Machines, and a converge that failed did not put it there.
            if prior is None:
                self._placements.pop(actor, None)
            else:
                self._placements[actor] = prior
            if isinstance(e, Exception):
                # THE WINDOW, PRICED. See {@link PlacementFailed} — the underlying error says what
                # went wrong and says nothing about the Machines that are already running.
                raise PlacementFailed(self.fqn, placement.ref, len(self.inventory), e) from e
            raise
        workflow.logger.info(
            "fleet placement converged",
            extra={
                "fqn": self.fqn,
                "actor": placement.ref,
                "sessions": placement.sessions,
                "workers": self.workers_for(placement),
                # `self.bundle_sha` reads the SINGLE placement and raises on a packed Fleet, so this
                # reads the one that was just placed. Same value while a Fleet holds one Artifact.
                "bundle": str(self._bundles.get(placement.ref, {}).get("bundleSha", ""))[:12],
                # PACKING, SAID OUT LOUD AT THE MOMENT IT HAPPENS. Two Workers on a Machine share its
                # egress address (ADR 0037), and the run that finds that out from a rate-limited
                # source three hours later has no line to search for. This is that line.
                "packed_with": ", ".join(
                    sorted(p.ref for p in self._placements.values() if p.actor != actor)
                ),
            },
        )
        if len(self._placements) > 1:
            workflow.logger.info(
                "%s now packs %d Artifacts onto %d Machine(s) — Workers sharing a Machine share its "
                "egress address; spread=True asks for one Worker of a placement per Machine and does "
                "not reserve one",
                self.fqn,
                len(self._placements),
                self.machines,
                extra={"fqn": self.fqn, "placements": len(self._placements)},
            )
        return placement

    async def ready(
        self,
        actor: str = "",
        *,
        at_least: int | None = None,
        timeout: timedelta = timedelta(minutes=10),
        poll: timedelta = timedelta(seconds=10),
    ) -> int:
        """Wait until Workers are actually polling this fleet's queue. Returns how many.

        `actor` names ONE placement to wait for; omitted, every placement has to be polling, and
        the answer is the smallest count seen — the number of Machines that are ready for ALL of
        the work, which is the number a dispatch loop can rely on.

        `at_least` defaults to EACH PLACEMENT'S OWN WORKER COUNT — every **Machine** for a placement
        that named none, and `workers=` for one that did. Lower it to start work on a partial fleet;
        there is no rolling health gate in the provision itself, so this is where "three of four is
        enough" gets expressed. One `at_least` applies to every placement being waited for, because
        it is a statement about how much capacity the caller needs rather than about an Artifact.

        THE DEFAULT USED TO BE THE **FLEET'S** MACHINE COUNT, and `workers=` is what made that wrong:
        a placement on two of six **Machines** has two pollers and always will, so a gate that waited
        for six would burn its whole timeout and then raise `FleetNotReady` about a **Fleet** that was
        working perfectly. It is the same number while a placement takes every **Machine**, which is
        every placement that predates packing.

        RAISES `FleetNotReady` on timeout rather than returning a count, because the alternative is
        a run that dispatches into a queue nobody polls and reports as slow.

        RAISES `ValueError` when nothing is placed, which is the hold-without-place window: there
        is no queue to watch until a `place()` has made one, so a `ready()` here could only return 0
        or wait out its whole timeout on a **Fleet** that will never have a poller.
        """
        from temporalio import workflow

        # NOTHING TO WAIT FOR MEANS NOTHING TO ASK, and it is checked BEFORE the placement below:
        # a caller who asked for zero Machines, or for `at_least=0`, is ready by their own
        # definition and must not be refused for a placement they do not need.
        if at_least is not None and at_least <= 0:
            return 0
        if at_least is None and self.machines <= 0:
            return 0

        targets = self._ready_targets(actor)
        #: How many pollers each placement needs. PER PLACEMENT, because `workers=` is.
        wanted = {p.actor: at_least if at_least is not None else self.workers_for(p) for p in targets}
        # The number reported in an error and returned to the caller. With one placement it is that
        # placement's count; with several it is the strictest, which is the one a dispatch loop that
        # uses all of them has to satisfy.
        want = min(wanted.values())
        deadline = workflow.now() + timeout
        while True:
            errors: dict[str, str] = {}
            #: The placements an answer was actually GOT for, and the ONLY place a poller count is
            #: written down.
            #:
            #: A DESCRIBE THAT FAILED PRODUCES NO ENTRY AT ALL. That is one rule in one place, and
            #: it is deliberately not two: writing an errored placement down as 0 as well would be
            #: a second expression of the same intent, correct only by the arithmetic of `want`
            #: being positive — so removing either half would leave the other silently covering for
            #: it, and a mutation would walk straight through both. "Could not ask" is therefore
            #: structurally not a measurement, rather than a measurement that happens to be small,
            #: which is also the shape `queuePollers` uses on the other side of the wire.
            measured: dict[str, int] = {}
            for p in targets:
                out = await workflow.execute_activity(
                    QUEUE_POLLERS_ACTIVITY,
                    {"actor": p.actor, "version": p.version},
                    task_queue=CALLER_QUEUE,
                    start_to_close_timeout=timedelta(seconds=30),
                    summary=f"pollers on {self.queue_for(p)}",
                )
                errors[p.actor] = str((out or {}).get("error") or "")
                # A COUNT FROM A FAILED DESCRIBE IS NOT A MEASUREMENT EITHER. `panels/pollers.ts`
                # drops a half-fold rather than returning a smaller number, but this side must not
                # depend on that: an error field wins over whatever `pollers` says beside it.
                if not errors[p.actor]:
                    measured[p.actor] = int((out or {}).get("pollers") or 0)
            # EACH PLACEMENT AGAINST ITS OWN NUMBER. A packed **Fleet** may carry one Artifact on
            # every **Machine** beside another on two of them, and comparing both to one target is
            # either a gate that never opens or one that opens early — which is a dispatch into a
            # queue with too few pollers, reported as slow.
            if len(measured) == len(targets) and all(c >= wanted[a] for a, c in measured.items()):
                return min(measured.values())
            if workflow.now() >= deadline:
                # THE PLACEMENT FURTHEST FROM READY is the one worth naming: with several, a message
                # about the healthy one would send somebody to the wrong queue. Measured as the
                # SHORTFALL against that placement's own target rather than as a raw count, because
                # two pollers of a two-Worker placement is ready and two of a six-Worker one is not.
                # An unmeasured placement sorts to the front, which is right — "could not ask" is the
                # diagnosis that changes what you go and do.
                worst = min(targets, key=lambda p: (measured.get(p.actor, 0) - wanted[p.actor], p.actor))
                raise FleetNotReady(
                    self.name,
                    self.queue_for(worst),
                    wanted[worst.actor],
                    measured.get(worst.actor, 0),
                    errors[worst.actor],
                )
            await workflow.sleep(poll)

    def _ready_targets(self, actor: str) -> list[Placement]:
        """Which placements `ready()` waits for, or a refusal naming what is actually here."""
        if not self._placements:
            raise ValueError(
                f"nothing is placed on {self.fqn}: ready() waits for the Workers a place() put "
                f"there, and this Fleet is holding {len(self.inventory)} Machine(s) with nothing on "
                f"them. That is the hold-without-place window — call place(actor, version) first, "
                f"or use fleet.up(), which stages the placement before the Machines exist."
            )
        if not actor:
            return list(self._placements.values())
        p = self._placements.get(actor)
        if p is None:
            raise ValueError(
                f"ready({actor!r}): {self.fqn} places "
                f"{', '.join(sorted(x.ref for x in self._placements.values()))} and not {actor!r}. "
                f"ready() with no argument waits for all of them."
            )
        return [p]

    async def _drop(self) -> None:
        """Let go of this scope's **Lease** (ADR 0037).

        THIS USED TO BE `_destroy`, AND THE CHANGE IS THE SLICE. Scope exit started a
        `stackWorkflow destroy` outright, which is correct only while exactly one **Run** owns a
        **Fleet** — and 0037 makes a **Fleet** capacity several **Runs** may hold at once, at which
        point the first scope to exit takes everybody's **Machines**. What this scope owns is its own
        claim; whether that was the LAST claim is the **Lease** workflow's question and the **Lease** workflow's answer, and
        the teardown happens there.

        SHIELDED, for exactly the reason the destroy was: the one path that would otherwise skip it
        is a CANCELLED run, which is precisely when **Machines** are still running with nobody left
        to notice. It is a cheaper thing to shield than it used to be — one activity rather than a
        child workflow — so the window in which a cancel can outrun it is smaller than before.

        A DROP THAT DOES NOT LAND IS NO LONGER A LEAK, WHICH IS THE OTHER HALF OF THE CHANGE. A
        teardown that failed used to mean **Machines** nobody was tracking; a drop that fails means a
        **Lease** that lapses on its own clock, so the error below is a delay to report rather than a
        bill to chase. It is still logged loudly: an hour of **Machines** is worth a line.
        """
        from temporalio import workflow

        if not self._lease:
            return
        lease, self._lease = self._lease, ""
        self._up = False
        if not self.destroy_on_exit:
            # ADOPTED. The **Lease** stays, unattributed and un-renewable, and the clock is what ends
            # it — see `_hold`. Dropping here would destroy the **Fleet** this scope deliberately
            # left standing for the next **Run**.
            workflow.logger.info(
                "fleet adopted — its Lease is left standing and expires on its own clock",
                extra={"fqn": self.fqn, "lease": lease},
            )
            return
        try:
            await asyncio.shield(
                workflow.execute_activity(
                    DROP_LEASE_ACTIVITY,
                    {"stackFqn": self.fqn, "lease": lease},
                    task_queue=CALLER_QUEUE,
                    start_to_close_timeout=timedelta(seconds=30),
                    summary=f"drop {self.name}",
                )
            )
        except Exception as e:
            # Do not mask the run's own failure with the drop's. Loud and named, because the
            # consequence is a **Fleet** that outlives this **Run** by up to one TTL.
            workflow.logger.error(
                "FLEET LEASE NOT DROPPED — %s holds it until its clock runs out; "
                "`kontra fleet leases %s` shows it and `kontra fleet down %s` ends it now",
                lease,
                self.fqn,
                self.name,
                extra={"fqn": self.fqn, "lease": lease, "error": str(e)},
            )


def hold(
    provider: DigitalOcean | Docker | None = None,
    /,
    *,
    tag: str,
    machines: int | None = None,
    region: str = "",
    size: str = "",
    credential: str = DEFAULT_CREDENTIAL,
    controller: str = "",
    destroy_on_exit: bool = True,
    lease_ttl: timedelta | None = None,
    timeout: timedelta = timedelta(minutes=60),
) -> Fleet:
    """Claim capacity: `async with fleet.hold(tag="dns", machines=4) as f:` (ADR 0037).

    provider     WHERE the Machines land and WHICH credential pays for them, in code —
                 `do_fleet(region="nyc3", machines=4, credential="do-prod")`. Exactly as on `up()`,
                 and the bare `machines=`/`region=`/`size=`/`credential=` knobs are the same
                 shorthand with the same refusal when both are given
    tag          THE FLEET. It is the label these Machines carry — the DigitalOcean tag
                 `kontra-<tag>`, the inventory group, the `kf-<tag>-NN` name — and since ADR 0037
                 it is also the **Fleet**'s NAME, so `kontra-fleet/dns` is the stack whose Machines
                 are `kf-dns-01`. Two Runs asking for `dns` capacity get the same Machines
    machines     how many. SCALE; `sessions=` on `place()` is density
    controller   where the Machines call home. Empty takes the one resolved with the Artifact
    destroy_on_exit / lease_ttl / timeout
                 exactly as on {@link up}

    ── WHAT THIS BUYS OVER `up()`, AND WHAT IT COSTS ────────────────────────────────────────────

    It buys the split ADR 0037 asks for: capacity is one decision and what runs on it is another,
    so `place()` can be called again mid-**Run** with a different `sessions=` and that IS the scale
    operation. `up()` cannot express that, because it has already decided both by the time the
    scope opens.

    IT COSTS A WINDOW. Between this call succeeding and the first `place()` succeeding, the
    **Machines** are up, billing, and running nothing — and a `place()` can fail there for reasons
    `up()` finds before a single Droplet exists: an Artifact that does not resolve, a version that
    was never published, an install that does not come up. ADR 0037 names this as the price of the
    split. {@link PlacementFailed} is where a caller meets it, and it carries the standing Machine
    count because that number is the cost.

    A HOLD ON A **FLEET** SOMEBODY ELSE IS HOLDING CONVERGES NOTHING, which is what makes it the
    cheap path onto existing capacity — one activity, no Pulumi, no four minutes — and is also the
    only safe answer: a Machines-only converge omits the co-tenant's placement, and omitting it in
    a declarative desired state REMOVES it. See {@link Fleet.__aenter__}.

    THAT SCOPE'S `inventory` IS EMPTY, and it is the one thing to know about it: nothing converged,
    so there are no stack outputs to read. `machines` still reports what this scope ASKED for, which
    is what `ready()` gates on — and `ready()` counts POLLERS rather than **Machines**, so it is
    answering the question that matters either way. Reach for `f.hosts()` only on a **Fleet** this
    scope brought up.

    Nothing is provisioned until the scope is entered.
    """
    tag = (tag or "").strip()
    if not TAG_RE.match(tag):
        raise ValueError(
            f"tag {tag!r} invalid: lowercase letters, digits and dashes, 2-16 chars "
            f"(it becomes the DigitalOcean tag `kontra-{tag}`, the inventory group, the "
            f"`kf-{tag}-NN` machine name — and, since ADR 0037, the Fleet's own name)"
        )
    return _fleet(
        provider,
        name=tag,
        tag=tag,
        machines=machines,
        region=region,
        size=size,
        credential=credential,
        controller=controller,
        destroy_on_exit=destroy_on_exit,
        lease_ttl=lease_ttl,
        timeout=timeout,
    )


def up(
    provider: DigitalOcean | Docker | None = None,
    /,
    *,
    actor: str,
    version: str,
    machines: int | None = None,
    tag: str = "",
    sessions: int | None = None,
    region: str = "",
    size: str = "",
    credential: str = DEFAULT_CREDENTIAL,
    controller: str = "",
    destroy_on_exit: bool = True,
    lease_ttl: timedelta | None = None,
    timeout: timedelta = timedelta(minutes=60),
) -> Fleet:
    """Open a fleet scope: `async with fleet.up(do_fleet(machines=4), actor=…, version=…) as f:`.

    provider     WHERE the Machines land and WHICH credential pays for them, in code —
                 `do_fleet(region="nyc3", machines=4, credential="do-prod")`. Per provider, because
                 the configurations are genuinely different shapes: a DigitalOcean VPC is regional,
                 so region and VPC are ONE fact, and `ssh_key_ids` are account-scoped ids that mean
                 nothing on another cloud (ADR 0034 §3)
    actor        an actor already published by `kontra build` / `kontra fleet deploy`
    version      its version, exactly as the manifest declares it
    machines     how many. SCALE; `sessions` is density. The shorthand for
                 `do_fleet(machines=N)` — see the note below
    tag          a LABEL for these Machines — a DigitalOcean tag, an inventory group and the
                 `kf-<tag>-NN` name prefix. Nothing dispatches on it. Defaults to the actor's
                 name, which is what almost every fleet wants
    sessions     live Sessions per Machine (`KONTRA_MAX_PARALLEL_SESSIONS`). None leaves the
                 hosts on their own default of 4
    credential   the NAME of the secret holding the cloud token, never the token. Empty means this
                 control plane's default name. Shorthand for `do_fleet(credential=…)`
    destroy_on_exit
                 False adopts a fleet and leaves it standing, for a run that hands off to the
                 next one. The default is the safe direction, because the failure mode of
                 forgetting is Droplets nobody is tracking. SINCE ADR 0037 "standing" means
                 "for one `lease_ttl`", not "for ever" — see below
    lease_ttl    how long this scope's **Lease** survives without renewal. None takes the control
                 plane's default. It is NOT a limit on how long the run may take: a **Lease** whose
                 holder is still RUNNING is renewed at every deadline, so this bounds a fleet whose
                 holder nobody can account for, and nothing else

    ── THE SCOPE HOLDS A LEASE; IT DOES NOT OWN THE FLEET (ADR 0037) ────────────────────────────────

    Entering the scope takes a **Lease** on `<actor>-<version>` and exiting DROPS it. The
    **Machines** are destroyed when the LAST **Lease** drops, which is the same behaviour as before
    whenever this run is the only holder, and is the difference between capacity and per-run
    provisioning whenever it is not: a second run inside the same fleet no longer has its
    **Machines** deleted by the first one to finish.

    AND EVERY **LEASE** EXPIRES ON A CLOCK, which is strictly stronger than what this scope could do
    on its own. Scope exit is a step in a durable program, so a process dying cannot skip it — but a
    **Run** that is never resumed never exits its scope at all, and nothing in that arrangement ever
    covered the control plane itself dying. The clock does: at a deadline the **Lease** workflow asks Temporal
    whether the holder is still running, and a **Fleet** whose holders are all gone is destroyed with
    nothing left anywhere that knows about it.

    `destroy_on_exit=False` therefore means "held by nobody, for one TTL" rather than "left standing
    for ever". A handoff that needs longer says `lease_ttl=`; a fleet meant to outlive every run is
    `kontra fleet up`, which is an operator's act and takes no **Lease**.

    THE PROVIDER MAY BE SPELLED OUT OR LEFT IMPLICIT, and both are the same call. `machines=`,
    `region=`, `size=` and `credential=` build a {@link DigitalOcean} when no provider object is
    passed — the shape every caller in this repo was written against, kept working — and passing
    both an object and a bare knob is a refusal rather than a merge, because "which one wins" is a
    question no caller should have to answer. Reach for `do_fleet(...)` when you need what the bare
    knobs cannot say: the VPC that belongs with a region, an image, account SSH key ids.

    THE CREDENTIAL IS A NAME AND NEVER A VALUE. It crosses into workflow history, where the payload
    codec is a claim-check rather than encryption — anything under 128 KiB rides inline in the clear
    for the namespace's whole retention. The infra worker resolves the name at the point of use.
    Rotating that secret and starting a new fleet uses the new value with no restart; a missing or
    revoked one fails HERE, naming the secret, before a single Machine exists.

    THE FLEET IS NAMED AFTER WHAT IT PLACES — `<actor>-<version>` — so there is no name to invent
    and none to keep in agreement between the run that creates it and the command that destroys
    it. Two scopes wanting `nscheck@0.1.0` Machines want the SAME Machines, and the second one
    fails to claim the stack rather than converging over the first. That is the correct answer,
    and it is the one the deleted caller-named form got wrong in both directions: it collided when
    two scopes happened to share a role, and did not when they should have.

    `role=` was this argument's name until it was corrected. It described nothing kontra ever
    behaved on, and the name beside it described a period nothing measured; both are gone.

    ── IT IS SUGAR OVER `hold` + `place`, AND IT STILL COSTS ONE CONVERGE (ADR 0037) ─────────────

    `up(actor=A, version=V, sessions=S, …)` is `hold(tag=tag or A, …)` with `Placement(A, V, S)`
    STAGED — put into the **Fleet**'s desired state before the scope opens, so the entry converge
    carries it. That is the whole of the difference from writing the pair out by hand, and it is
    the difference that matters:

      • ONE CONVERGE, NOT TWO. Machines and placement go up together, exactly as before this ADR,
        so an existing workflow's history, its wall time and its Pulumi state are unchanged.
      • NO HOLD-WITHOUT-PLACE WINDOW. The Artifact is resolved before the first Droplet exists, so
        a version that was never published costs one activity here and a whole provision there.
        `up()` cannot raise {@link PlacementFailed}, and that is not an omission — the state it
        describes is unreachable from this door.
      • THE FLEET IS STILL NAMED `<actor>-<version>`, not after the tag. `hold()` names a **Fleet**
        after its tag (ADR 0037: a **Fleet** is capacity), and moving `up()` to that rule would
        orphan every stack this repo has ever created and break `kontra fleet down nscheck-0.1.0`.
        Two ways to start a **Fleet** exist for ever, says 0037; two ways to NAME one is the part
        of that tax that had to be paid here.

    Nothing is provisioned until the scope is entered.
    """
    actor = (actor or "").strip()
    version = (version or "").strip()
    if not actor or not version:
        raise ValueError(
            "actor and version are required: a fleet places one published Artifact, and its "
            "name is `<actor>-<version>`"
        )
    tag = (tag or actor).strip()
    if not TAG_RE.match(tag):
        raise ValueError(
            f"tag {tag!r} invalid: lowercase letters, digits and dashes, 2-16 chars "
            f"(it becomes the DigitalOcean tag `kontra-{tag}`, the inventory group and the "
            f"`kf-{tag}-NN` machine name)"
        )
    if sessions is not None and (not isinstance(sessions, int) or sessions < 1):
        raise ValueError(f"sessions must be a positive integer or None, got {sessions!r}")

    f = _fleet(
        provider,
        # DERIVED FROM THE ARTIFACT, not from the tag — see the docstring above for why this door
        # keeps the pre-0037 rule while `hold()` takes the new one.
        name=f"{actor}-{version}",
        tag=tag,
        machines=machines,
        region=region,
        size=size,
        credential=credential,
        controller=controller,
        destroy_on_exit=destroy_on_exit,
        lease_ttl=lease_ttl,
        timeout=timeout,
    )
    # THE STAGED `place`. Written straight into the desired state rather than by calling `place()`,
    # because `place()` converges and there is nothing to converge yet — the point of staging is
    # that this placement rides the SAME converge the Machines do.
    f._placements[actor] = Placement(actor=actor, version=version, sessions=sessions)
    return f


def _fleet(
    provider: DigitalOcean | Docker | None,
    /,
    *,
    name: str,
    tag: str,
    machines: int | None,
    region: str,
    size: str,
    credential: str,
    controller: str,
    destroy_on_exit: bool,
    lease_ttl: timedelta | None,
    timeout: timedelta,
) -> Fleet:
    """Everything `hold()` and `up()` agree about: WHERE the Machines land, and the **Lease**.

    ONE IMPLEMENTATION, TWO DOORS. The alternative — `up()` calling `hold()` — reads better in a
    changelog and is wrong in one specific way: `up()` derives the **Fleet**'s NAME from the
    Artifact and `hold()` from the tag, so `up()` would have to pass a name through `hold()`, which
    would put a caller-invented stack name back in the public surface. That form was deleted for
    colliding in both directions and it does not come back as a keyword argument.
    """
    bare = {"machines": machines, "region": region, "size": size, "credential": credential}
    named = sorted(k for k, v in bare.items() if v is not None and v != "")
    if provider is None:
        if machines is None:
            # SCALE HAS NO SENSIBLE DEFAULT, AND `None` IS NOT THE BYOC PATH YET. ADR 0037 gives
            # `machines=None` the meaning "take what already exists rather than provisioning", and
            # it cannot be honoured here: what "take what exists" needs is the CURRENT machine
            # count, and there is no read of it a workflow can reach. The naive implementation is
            # worse than missing — Pulumi's desired state is total and the orchestrator coerces an
            # absent `machines` to 0, so a converge that "left the count alone" would DESTROY every
            # Droplet in the Fleet (`control/orchestrator/src/infra/stacks.ts:coerceFleetArgs`). Refusing is the
            # only honest answer until that read exists.
            raise ValueError(
                "machines= is required: say how many Machines this fleet places, or pass a provider "
                "configuration — do_fleet(machines=4, region=…). `machines=None` is ADR 0037's BYOC "
                "path and is not built: taking what already exists needs a read of the current "
                "count, and a converge with no count destroys the Fleet rather than leaving it alone."
            )
        provider = DigitalOcean(
            machines=machines, credential=credential, region=region, size=size
        )
    elif not isinstance(provider, (DigitalOcean, Docker)):
        raise TypeError(
            "a fleet's provider configuration must be a DigitalOcean (do_fleet(...)) or a Docker "
            f"(docker_fleet(...)), got {type(provider).__name__}"
        )
    elif named:
        # A REFUSAL RATHER THAN A MERGE. `fleet.up(do_fleet(region="nyc3"), region="sfo3")` has no
        # right answer, and the wrong one puts Machines in a region that cannot reach the
        # Controller — which does not fail, it HANGS on `ready()`. See DigitalOcean's docstring.
        # The same sentence for docker_fleet: region=/credential= beside it are DigitalOcean knobs
        # that do not apply, and a silent drop would look like a working local fleet.
        kind = "docker_fleet" if isinstance(provider, Docker) else "do_fleet"
        raise ValueError(
            f"pass the fleet's configuration once: {', '.join(named)} was given beside a provider "
            f"object. Put it inside — {kind}({', '.join(f'{k}=…' for k in named)}, …)"
        )

    if lease_ttl is not None and lease_ttl.total_seconds() <= 0:
        # A zero or negative TTL is a **Lease** that has already expired — the fleet would be
        # destroyed at the first deadline check, mid-run, by the mechanism meant to protect it.
        raise ValueError(f"lease_ttl must be positive or None, got {lease_ttl!r}")

    return Fleet(
        name=name,
        tag=tag,
        provider=provider,
        controller=controller,
        destroy_on_exit=destroy_on_exit,
        timeout=timeout,
        lease_ttl=lease_ttl,
    )


class _FleetModule(ModuleType):
    """Makes `fleet["scanners"]` spellable. See {@link __getitem__} for what the subscript NAMES.

    A module has no `__getitem__` and cannot be given one directly, so this module's own object is
    re-classed to this subclass at the bottom of the file — the standard way (PEP 562 gives modules
    `__getattr__`; the `__class__` assignment is what covers everything else). `kontra` is a
    passthrough module in the workflow sandbox (`internals/temporal/wfhost.py:DEFAULT_PASSTHROUGH`),
    so a workflow sees this same object rather than a re-imported copy.
    """

    def __getitem__(self, tag: str) -> "functools.partial[Fleet]":
        """`fleet["scanners"](actor=…, version=…, machines=4)` — the TAG, bound up front.

        WHAT THE SUBSCRIPT NAMES, decided and written down here because there were two candidates:

          • THE TAG. YES. It is the only string a caller supplies about a fleet, and the only part
            of a fleet's identity a caller OWNS: it becomes the DigitalOcean tag `kontra-<tag>`,
            the inventory group, and the `kf-<tag>-NN` machine name prefix. `fleet["scanners"]`
            therefore says the same thing the operator's `kf-scanners-01` says.
          • THE STACK. NO, and this is not a near miss. A fleet's name is `<actor>-<version>` and
            its fqn `kontra-fleet/<actor>-<version>` — DERIVED from what it places, with no
            argument (see `Fleet.fqn`). Letting a caller name it would restore the deleted form, which
            was deleted for colliding in both directions: two scopes sharing a role collided when
            they should not have, and two scopes wanting the same Machines did not when they
            should have. A subscript that named the stack would also have to be PARSED back into
            an actor and a version, which `0.1.0-rc1` makes ambiguous. Neither is worth a spelling.

        SO THE SUBSCRIPT NAMES THE MACHINES AND NOT THE STACK, and `fleet.up` still names the
        Artifact. That split is the same one the module header keeps: what a fleet IS comes from
        what it places; what it is LABELLED is the caller's.

        ONE IMPLEMENTATION, THE OTHER DELEGATING: this is `up` with `tag` bound, so
        `fleet["scanners"](actor="nscheck", version="0.1.0", machines=4)` and
        `fleet.up(actor="nscheck", version="0.1.0", machines=4, tag="scanners")` are the same call
        by construction, and every validation (TAG_RE included) still happens in `up`.
        """
        if not isinstance(tag, str):
            raise TypeError(f"a fleet tag must be a str, got {type(tag).__name__}")
        return functools.partial(up, tag=tag)


def inventory_hosts(inventory: Mapping[str, Any]) -> list[str]:
    """Private addresses out of a stack's inventory output, sorted by machine name.

    Free-standing so a caller holding stack outputs from anywhere — `kontra fleet status`, an
    earlier fleet — reads them the same way `Fleet.hosts()` does.
    """
    return [dict(inventory[k]).get("host", "") for k in sorted(inventory)]


__all__ = [
    "up",
    "hold",
    "Fleet",
    "Placement",
    "FleetNotReady",
    "PlacementFailed",
    "DigitalOcean",
    "do_fleet",
    "Docker",
    "docker_fleet",
    "inventory_hosts",
    "lease_id",
    "FLEET_PROJECT",
    "DOCKER_FLEET_PROJECT",
    "INFRA_QUEUE",
]

# The one line that makes `fleet["scanners"]` legal. Everything else in this module is unchanged,
# and `fleet.up(...)` is untouched — see `_FleetModule.__getitem__` for what the subscript names.
sys.modules[__name__].__class__ = _FleetModule
