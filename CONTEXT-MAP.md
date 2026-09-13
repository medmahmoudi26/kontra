# Context Map

kontra has two contexts. They meet at exactly one concept — the machine that runs actor code —
and the split exists because making machines exist and dispatching work to them have different
lifecycles, different failure modes, and different trust boundaries.

## Contexts

- [Execution](./CONTEXT.md) — a caller's workflow, the Actors it calls, and the Datasets that come
  back. The repo's core context: the orchestrator, the handler and kontra all speak this language.
  (It used to read "turns a saved graph into work that runs"; ADR 0023 §12 deleted the graph
  interpreter and made a **Run** one execution of a caller's workflow.)
- [Fleet](./control/orchestrator/src/infra/CONTEXT.md) — brings into existence, repairs, and destroys the machines that
  Execution dispatches to.

## Relationships

- **Fleet → Execution**: Fleet supplies the machines on which Execution's **Workers** run. The
  handoff is one-way and narrow: Fleet publishes which machines are reachable and what **Role**
  each carries; Execution never learns which cloud it is on.
- **Execution → Fleet**: a **Run** *may* ask Fleet for machines, and only through one door —
  `kontra.fleet.hold()` / `f.place()`, which start Fleet's own `stackWorkflow` as a **child
  workflow** on the infra queue; `kontra.fleet.up()` is sugar for the pair. The scope's exit drops
  the **Lease** and the **Machines** die when the last one goes. This reverses the older "a **Run**
  cannot cause a machine to exist": it can, because a caller's workflow is *durable*, and that is
  precisely what makes it safe. A script that provisions ten Droplets and dies leaves ten Droplets;
  a workflow cannot, because letting go is a replayable step Temporal will finish even if the
  starting process is gone.
- **The two verbs stay separate.** Neither `up()` nor `hold()` returns anything you dispatch to:
  which Machines exist is declarative state, and which **Batches** go through them is a loop you
  write. A combined `f.run(actor(...))` would fuse the one boundary this system keeps deliberately
  open. `place()` is the third verb and it stays on the declarative side — it says what a **Machine**
  should be running, never what to send it.
- **The credential never crosses, and since ADR 0034 §4 that is a property of the TYPE rather than
  of which process holds an environment variable.** `fleet.up()` names a stack and NAMES a
  credential; the infra worker resolves that name to a value at the last hop, inside the activity
  about to converge, and the value exists for the length of one converge. The invariant is stated
  as five negatives because that is how it is tested — the value is never a workflow argument,
  never an activity argument, never part of a **Batch**, never in Pulumi stack config or state, and
  never on a **Machine**. Only the name crosses, and a name is not a secret. That buys two things
  the old `DIGITALOCEAN_TOKEN` could not: rotation with no service restart and no file edit, and a
  missing or revoked credential that fails at the START of `fleet.up()` naming the secret rather
  than as a provider error minutes into a converge. A **Run** can ask for machines and still cannot
  learn which cloud it is on, which is what keeps "authority to destroy a machine stays with the
  controller" true below. In the **appliance** (ADR 0031) the same guarantee holds by absence
  rather than by design: Fleet is out of the binary, so there is no cloud credential in it at all,
  and `fleet.up()` fails there — legibly — instead of provisioning. The compose controller keeps
  that job.
- **Fleet does not read a Run.** Fleet judges a machine's health from signals on the machine
  itself, never from Execution's view of a **Run** — a **Run** reporting `completed` is
  precisely the condition under which a machine has been found silently sick.

## This is a language boundary, not a process boundary

Both contexts' code lives in the orchestrator, which owns the provisioner as a library and
exposes it over the same HTTP API that serves the catalog, runs and datasets (ADR 0019). The
provisioner runs as its own role **on its own queue**, and that is what survived `fleet.up()`: a
**Run** reaches Fleet only by starting Fleet's own workflow, under Fleet's own vocabulary, on
Fleet's own worker. It never provisions *itself* — it asks, and the asking is a child workflow with
a stack fqn for an id, not a library call into Execution's process.

That id is load-bearing rather than cosmetic: Pulumi's DIY lock has no compare-and-swap and no TTL,
so Temporal's workflow-id uniqueness is what actually serialises writers. A **Run** that tries to
claim a fleet somebody is already converging fails at the start instead of corrupting it.

That is why the test is the vocabulary and not the process: when a sentence needs both a
**Unit** and a **Machine** to make sense, a boundary is being crossed and one of the two words
is wrong.

## Why repair lives in Fleet, not Execution

Fleet's repair loop must survive the total loss of the Fleet. Modelling it in Execution would
put repair on the very machines being repaired: a repair **Actor** needs a **Worker**, and a
**Worker** is what has failed. The loop that repairs a system cannot live inside it.

The same boundary keeps cloud credentials off machines that reach hostile third-party
infrastructure — authority to destroy a machine stays with the controller.
