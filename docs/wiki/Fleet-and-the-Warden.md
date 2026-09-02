# Fleet and the Warden

How kontra runs code on machines — including machines it did not provision, and code it did not write.

## A Fleet is capacity, not a job's provisioning

A **Fleet** is a set of **Machines**, each running a **Warden**, that **Containers** are scheduled onto. It is tagged, not named after what it runs. It is not owned by one **Run**, and it outlives any one of them.

```python
async with fleet.hold(tag="dns", machines=4) as f:   # capacity, and a Lease on it
    await f.place("nscheck", "0.1.0", containers=3, workers=8)
    await f.place("subfinder", "0.2.0", spread=True) # …and something else, packed alongside
    await f.ready()                                  # Containers actually POLLING
# scope exit drops the Lease
```

`fleet.up(actor=…)` still works and still names its Fleet `<actor>-<version>` — every stack ever created stays reachable — but `hold`/`place` is the shape to learn.

### Lease

**One Run's claim on a Fleet.** Machines are destroyed when the **last** Lease drops, so two Runs can hold the same capacity without either taking it out from under the other.

Every Lease also expires on a clock — and **the clock is a check, not a deadline.** At expiry the Lease workflow asks Temporal whether each holder is still `RUNNING` and renews the ones that are. So the interval bounds *how long a Fleet survives a holder nobody can account for*; it is not a limit on how long a Run may take.

A holder pays nothing to stay held: no heartbeat, no renewal, no events at all between hold and drop. A Run that dies without dropping is collected by the clock — which is strictly stronger than scope-exit teardown, because scope exit never covered the control plane dying.

```bash
kontra fleet leases --fleet nscheck-0.1.0
```

### Placement

**One Artifact's desired state on a Fleet** — which Actor, which version, how many **Workers** per Container, and how many of the Fleet's Machines it lands on.

Calling `place()` again with a different `workers=` or `containers=` **is** the scale operation. There is no `scale()` verb, because a second `place()` with a different desired state is the same declarative call.

Calling it for a second Actor is **packing** — and there is no flag for that either. The second call *is* the request.

> **The desired state is total.** Pulumi carries no patches, so a converge that omits a Placement does not leave it alone — it removes it. Every converge carries every Placement the scope has made.

### spread=

`spread=True` places one Container of *that Placement* per Machine, on every Machine.

It exists because packing takes something away. While a Machine held exactly one Container, a unique egress address was an invisible invariant — the operating rule for `subfinder` is one container per droplet, concurrency 1, unique IP. Packing makes several Containers share the Machine's address, so the property becomes a request.

**It does not reserve a Machine.** A co-placed Container still shares the address. A Container that must have a Machine to itself needs a Fleet of its own — a different tag.

## The Warden

**The one process kontra installs on a Machine.** It reconciles which Containers run there, judges their health, enforces the Machine's egress policy, and streams its panes. **It runs no actor code itself.**

Four things that are not negotiable:

1. **It runs no actor code.** Every Container is started through the four driver verbs and never linked into the Warden. A Container that segfaults is a value the loop logs and continues from.
2. **It reconciles desired state, not commands.** No start message, no stop message. A rebooted Machine asks the same question and gets the same answer.
3. **What is running comes from the runtime, never from a file.** The loop keeps no record of what it started, so a Container left behind by a previous Warden is seen and reconciled rather than duplicated.
4. **It may restart a Container; it may not replace a Machine.** Re-provisioning costs money, and a Machine that judges itself unfit is not the thing to trust with that decision.

### Outbound only

A Machine accepts no inbound connection. Every arrow is dialled by it — which is what makes NAT, a private VPC and a customer's firewall work unchanged, and why the assignment is *fetched* rather than pushed.

### What it costs

The Warden is a **blocked** Temporal Worker — it waits on a condition, not a timer. Measured against a live server: **0 events/hour at rest**, ~6 per real change. Telemetry rides a separate snapshot path and never touches workflow history, because a 200×50 terminal frame through history is 86% of a workflow's events for a value that is stale a second later.

## Running one on your own machine

Enrolment is a token you carry to the machine, not a port the control plane dials back:

```bash
kontra warden ca token --tenant acme           # on the control plane
kontra warden join --controller https://…:8443 --token kw1.…   # on the machine
kontra warden serve --driver podman
```

The private key never leaves the machine; what comes back is a signed certificate carrying the tenant's namespace as a URI SAN.

Full guide: **[Running a Warden](https://github.com/medmahmoudi26/kontra-actors/blob/main/docs/running-a-warden.md)**.

## Bring your own cloud

Because every connection is outbound and enrolment is a carried token, a Machine in someone else's network needs no inbound rule. The strongest arrangement puts the **whole control plane on their side** — their machine runs Temporal, the object store and the orchestrator; their VPSs run Wardens; your workflow worker connects over a VPN.

That keeps execution *and* output in their tenancy, which network-level access alone does not: `KONTRA_S3_ENDPOINT` points at the controller, so wherever the controller is, is where the data lands.

See [[Security-Model]] for what is isolated between tenants today and what is not.

## Reading

- [[Deployment]] — operating a control plane
- [[Dashboard]] — the Monitor, and the panes a Warden streams
- [[Security-Model]] — boundaries, and their limits
- `docs/adr/0036`, `docs/adr/0037` — the decisions and their trade-offs
