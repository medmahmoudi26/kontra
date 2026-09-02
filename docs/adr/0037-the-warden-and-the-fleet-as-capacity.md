# 37. The Warden, and the Fleet becomes capacity held by Leases

## Status

**Accepted.** Depends on **0036**, which decides that actor code runs in a container everywhere.
This ADR decides what puts it there, and what a **Fleet** is once a **Machine** can run anything.

Retires **Watchdog** from `infra/CONTEXT.md` and redefines **Fleet**; introduces **Warden** and
**Lease**. Reopens the "Not yet settled" note on the repair model, and settles half of it: the
**Warden** may restart a **Worker**, and may not replace a **Machine**.

Narrows **0034** without contradicting it. That ADR says *"cloud provisioning belongs to the compose
controller"* and gives the appliance no Pulumi, no provider plugin and no cloud token. All of that
stands. What changes is that a **Machine** can now arrive without kontra provisioning it, so the
appliance gains a **Fleet** without gaining any of the four things 0034 withheld.

## Context

1. **A Fleet is named after the one Artifact it places.** `infra/CONTEXT.md`: a **Fleet**'s name is
   *"`<actor>-<version>`, **DERIVED** from the **Artifact** it places, so that two runs wanting the
   same **Machines** collide and two runs wanting different ones do not — the property `campaign=`
   got wrong in both directions before it was deleted."* A **Fleet** that carries several actors
   cannot be named this way, and the record is explicit that a caller-given name was tried and cut.

2. **Teardown is the fleet's central safety claim.** `nscheck`'s own header: *"A script that
   provisions ten machines and then dies leaves ten machines; this cannot, because the teardown is a
   replayable step in a program Temporal finishes whether or not the process that started it still
   exists."* Observed on this checkout: a run that failed on a `TypeError` **after** the fleet was up
   still destroyed both Droplets. Scope-exit teardown works, and it works because exactly one **Run**
   owns the **Fleet**.

3. **A long-lived watcher workflow is not free.** Measured previously in this repo: a ticking
   watcher costs ~20 events per tick and makes continue-as-new mandatory. At a 5-second tick that is
   ~14,400 events/hour per **Machine**. A workflow that BLOCKS on a condition costs nothing while it
   waits.

4. **A pane snapshot path already exists.** `/api/panels/terminals` carries `lastSnapshotAt`,
   `paneCols`/`paneRows` and a health block (`reachable`, `session`, `process`, `poller`, `loads`)
   per pane, and the Monitor renders fleet panes from it today.

5. **One Worker per Machine is a functional requirement, not tidiness.** `infra/README.md` gives the
   reason as *"its unique egress address"*, and the operating rule for `subfinder` is one worker per
   droplet, concurrency 1, unique IP. Packing several **Workers** onto a **Machine** makes them share
   a source address.

6. **`sessions` is already the concurrency knob**, and it is per-**Worker** density rather than
   count — so packing adds a second, different axis rather than replacing the existing one.

## Decision

### The Warden

- **Every Machine runs exactly one Warden, and the Warden runs everything else.** It reconciles the
  **Workers** on its **Machine** against what the control plane asked for, judges their health,
  restarts them when sick, enforces egress policy, and streams the **Machine's** panes. It executes
  no actor code itself.

- **The Warden is a Temporal Worker, and it BLOCKS.** One per **Machine**, waiting on a condition
  rather than a timer, so history records decisions — assignment changed, **Worker** started,
  **Worker** exited non-zero, **Machine** unreachable — at roughly five events per real change and
  nothing at rest. This buys durable fleet lifecycle and puts it in the Transcript and the Event log
  for free.

- **Telemetry does not ride workflow history.** CPU, memory, load ratios and tmux frames go over the
  existing pane-snapshot path. A 200×50 terminal frame through Temporal history is the shape this
  repo already measured as 86% of a workflow's events, for a value that is stale a second later.

- **The Watchdog retires into the Warden.** The thing that decides a **Worker** is sick should be the
  thing that can restart it; two processes disagreeing about that was a bug waiting to be written.

- **The Warden may restart a Worker. It may not replace a Machine.** That is the settled half of the
  repair model. Destroying and re-provisioning is the control plane's act, because it costs money and
  because a **Machine** that judges itself unfit is not the thing to trust with the decision.

### The Fleet

- **A Fleet is tagged capacity, not per-Campaign provisioning.** It is a set of **Machines**, each
  running a **Warden**, that **Workers** are scheduled onto. It is not named after what it runs, it
  is not owned by one **Run**, and it outlives any one of them.

- **Several Workers may share a Machine, and they share its egress address.** This is the whole
  reason the container **Target** exists (0036) and it is a real loss: the unique-source-address
  property that `subfinder` depends on stops being automatic. It becomes a request — `spread=True`
  places one **Worker** per **Machine** — so what was an invariant nobody could see is now a
  parameter somebody has to choose. That is the trade, stated plainly.

  **MEASURED when this was built (slice 11), because the claim is about a network and was not
  self-evident.** Under the `podman` driver each **Worker** is a pod with its OWN address on the
  **Machine's** bridge, so two of them do have two addresses and a reader could conclude that packing
  costs nothing. What is shared is the address their traffic wears once it has left the bridge. On
  this Controller, podman 4.3.1: two **Workers** with distinct container addresses arrived at a
  destination off their bridge as ONE address — the **Machine's** — while the same two arrived at a
  destination on their own bridge as two. `cli/warden_packing_test.go` is that measurement and its
  control.

  **`spread=True` does NOT reserve a Machine, and this entry's own snippet is why.** It places
  `nscheck` and then `subfinder(spread=True)` on ONE four-**Machine** **Fleet**, so the two are packed
  together and `subfinder`'s address IS shared with the co-placed **Worker**. Read as "nothing else
  runs here" the snippet would be unsatisfiable, so what the argument promises is the narrower thing
  it can keep: no two **Workers** OF THAT **PLACEMENT** share a **Machine**, which is `subfinder`'s
  rate-limit rule. A **Worker** that must have a **Machine** to itself needs a **Fleet** of its own.

- **A Placement puts at most one Worker on any one Machine**, so `workers=` counts **Machines** and
  not processes, and the count is capped by the **Fleet's**. Two **Workers** of one
  `<actor>@<version>` on one **Machine** are not expressible: they carry the same `KONTRA_WORKER`
  label, write the same units and poll the same queue, so `list()` cannot tell them apart and the
  **Warden's** reconcile refuses the duplicate. That is the structural reason packing adds a
  different axis from `sessions` rather than a bigger one.

- **A Lease is one Run's claim on a Fleet.** **Machines** are destroyed when the last **Lease**
  drops. Every **Lease** also expires on a clock, so a **Fleet** outlives neither its holders nor a
  control plane that died holding it — which is strictly stronger than scope-exit teardown, because
  scope exit never covered the control plane dying.

### What a caller writes

```python
async with fleet.hold(tag="dns", machines=4) as f:      # capacity + Lease
    await f.place("nscheck", "0.1.0", sessions=8)       # what runs on it
    await f.place("subfinder", "0.2.0", spread=True)    # …one per Machine
    await f.ready()
# scope exit drops the LEASE. Machines die only if it was the last one.
```

- **`place()` is idempotent desired state, so it is also the scale verb.** Calling it again with a
  different `workers=` or `sessions=` is how a **Run** scales mid-flight; the **Warden** reconciles
  the difference. There is no separate `scale()`, and mid-run change needs no new concept — it is
  the same call.

- **`workers=` is how many of the Fleet's Machines a Placement lands on**, one **Worker** each; unset
  is every **Machine**, which is what a **Fleet** did before packing. It takes a PREFIX of the
  **Machines** rather than spreading or balancing, so growing a **Fleet** adds **Workers** and never
  relocates one — and relocating one is a teardown on a **Machine** nobody asked to touch.
  `workers=` above the **Machine** count is a refusal, not a truncation.

- **Packing is requested by placing a second Artifact, and there is no flag for it.** `place()` is
  the request; a caller who did not want two **Workers** on a **Machine** did not make the second
  call. A flag would be a second way to say what the verb already says, and the interesting argument
  is the opposite one — `spread=`, which asks for the property packing takes away.

- **`fleet.up()` survives as sugar** for the single-actor case, defined as `hold` + `place`, so every
  existing workflow and example keeps working unchanged.

- **`machines=None` takes what already exists** rather than provisioning. That one argument is the
  whole BYOC path: whether capacity is created or found is the provider's business, not the caller's.

  **CORRECTED 2026-08-31, and it is REFUSED rather than implemented.** Written as stated, this
  argument does not take what exists — it destroys it. Measured while building slice 10:
  `coerceFleetArgs` reads an absent `machines` as `Number(undefined ?? 0)` = **0**, `fleetProgram`
  builds zero Droplets, and Pulumi's desired state being total, it DELETES the Machines that are
  there and reports success. The sentence above described the ergonomics correctly and the mechanism
  not at all.

  "Take what exists" needs a read of the current count, and no workflow can reach one: `readStack` is
  a file read in the infra worker's state directory, behind a queue that serves one activity at a
  time behind 60-minute converges. So `fleet.hold(machines=None)` refuses, with an error naming that,
  and `conformance/placement.json`'s first reader case pins the refusal. BYOC is still the right
  direction and is now an open design question rather than a settled one-liner.

## Consequences

- **Campaign retires.** Left open when this ADR was drafted and settled the same day: the term is
  gone, not narrowed. It meant *"one bounded period of work a **Fleet** is created for and destroyed
  after"* — a definition that names an infrastructure this ADR removes, since a **Fleet** is created
  for nobody and destroyed when the last **Lease** drops. Narrowing it to "one **Run**'s claim" was
  the alternative, and it fails for the plainest reason: that is the definition of **Lease**, three
  entries above it in the same glossary. Both halves of the word already had owners — the work is a
  **Run**, the claim is a **Lease** — so it named nothing of its own.

  The word survives in exactly one place, the `_Avoid_` line under **Lease**, because a glossary
  retires a term by rejecting it rather than by falling silent about it. Everywhere else — comments,
  UI strings, test fixtures, `docs/wiki` — it goes. Accepted ADRs are NOT rewritten: **0020**,
  **0025**, **0026**, **0031**, **0032**, **0034** and **0035** all use the word in prose and keep
  it, because they record what was decided when it still meant something. This entry is the
  authority that supersedes them.

- **A Fleet can now be starved or poisoned by a co-tenant Run.** Shared capacity means one **Run**
  can fill a **Fleet**. Quotas per **Lease** are not decided in this ADR and are the first thing to
  design after it.

- **`place()` can fail after `hold()` succeeded** — no room, image will not pull, digest unsigned —
  so there is a window in which **Machines** are held with nothing on them. Today that failure is
  inside one call and cannot happen.

- **Two ways to start a Fleet exist forever.** `fleet.up()` as sugar is what keeps every current
  example true, and it is also a permanent documentation tax. Accepted knowingly.

- **The appliance gains a Fleet without gaining a cloud credential.** Self-enrolled **Machines** are
  not provisioned by kontra, so **0034**'s four withheld things stay withheld. Nobody adds `--cloud`
  to `kontra up` on the strength of this ADR.

- **The Warden is a protocol across an organisational boundary**, and version skew with customers who
  upgrade slowly is now a permanent cost. Its own upgrade path is a day-one design problem, not a
  later one.
