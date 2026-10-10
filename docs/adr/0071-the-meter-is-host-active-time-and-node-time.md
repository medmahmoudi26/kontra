# 71. The meter is workflow-host active time plus node time

Date: 2026-10-10

## Status

**Accepted.** Implements PRD *the simplified platform* D8, "Metering" (plan item WP-77, owner
default A40). **Supersedes 0046** as the billing decision. 0046's diagnostics stay: `historyLength`
read by `describe`, `KontraTenant` populated, and a Runs page that excludes internal types. Uses the
vocabulary of ADR 0070.

## Context

ADR 0046 made the Temporal event the billing unit, and measured why the archive could not carry it.
The PRD then chose a different product: "workflow-host active time (scale-to-zero on empty queues)
plus node time. No per-event pricing."

The event as a unit has two problems the PRD's choice avoids:

- **A customer cannot predict it.** One Method call wrote 34 events when 0046 was measured, and
  Phase 2 (ADR 0067) changes that number by removing the backing workflow. A price per event is a
  price that kontra's own refactors move.
- **It charges for the wrong resource.** Events are Temporal's cost. A tenant's cost to kontra is the
  compute it holds: the nodes its fleet runs, and the workflow host that runs its workflows.

Both of the PRD's units are things kontra already decides and therefore already knows: when a node
pool converges or is destroyed (the fleet pool workflow, ADR 0066), and when a workflow host
Deployment's replica count leaves or returns to zero (KEDA, ADR 0066).

## Decision

1. **Two units, both time.**
   - **Node time**: seconds a fleet's nodes exist, by node size, from the converge that created
     them to the destroy that removed them. A `byo_kubeconfig` fleet records none, because kontra did not
     create its nodes.
   - **Workflow-host active time**: minutes during which a Workspace's workflow host has more than
     zero replicas, rounded up to whole minutes per active interval. A host scaled to zero on empty
     queues costs nothing.

2. **One append-only usage journal is the record.** A Postgres table `usage_journal`, one row per
   start or stop of a unit, unique on `(namespace, workflow_id, activity_id, kind)`, so an activity
   retry cannot count twice. Rows are appended by the activities that already make the change
   (pool converge and destroy; host scale-up and scale-to-zero), never by a listing. A failed append is
   logged and surfaced on `GET /api/usage`, and never fails the run or the converge that caused it.

3. **Attribution is by address, as everywhere else.** Workflow-host time belongs to the Workspace
   whose namespace the host serves. Node time belongs to the Account that owns the fleet: on the
   managed cloud each Account gets its own DigitalOcean project and VPC (PRD D8, WP-79), so a node
   is never shared across Accounts. On the laptop tier the install is the one Account and nothing is
   priced.

4. **Prices are not in kontra.** The journal records quantities and the node size. kontra-cloud
   turns them into money, and owns currency, plans and Credits.

5. **No per-event charge.** `historyLength` stays a diagnostic and a limit: Temporal terminates a
   workflow at 51,200 events, which is why the long-lived kontra workflows continue-as-new (the fleet
   pool does).

## Consequences

- WP-76 builds the journal and `GET /api/usage`. WP-78 builds the per-Workspace workflow host on
  KEDA and journals its active intervals. WP-79 gives each managed Account its own project and VPC.
- A wedged workflow host that keeps a replica up is billed. That is deliberate: it holds a node's
  memory. The fix is the host's own health check, not the meter.
- Clock skew between the orchestrator and Kubernetes cannot move a bill by more than one rounding
  unit, because both ends of an interval are written by the same orchestrator clock.
- 0046's metering pass (a second listing of internal workflow types) is not built.
