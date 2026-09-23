# 51. The workspace is the isolation boundary

Date: 2026-09-23

## Status

Proposed.

Extends [ADR 0049](0049-the-workspace-is-the-registration.md), which made a workspace the thing that
decides *which code exists*. This makes it the thing that decides *where everything that code
produces goes*. It does not change ADR 0049's rule; it says that rule was only the first half.

It also settles the tenancy question that every surface has been quietly guessing at, and removes
`tenant` from the console.

## Context

### One user, one tenant. The install is the tenant.

kontra's model is one user per tenant and many workspaces per tenant. That has been true in the
operator's head for a while and has never been true in the code, which carries a `tenant` field
through the run record, the log records, the dataset rows and the console — always holding the
string `default`.

The console's tenant chip is the clearest case. It printed `tenant default` in the nav bar, and it
never had a source: `Shell.svelte` declared a `tenant?: string` prop, nothing ever passed it, and
the literal `'default'` in its own default value was what every install on earth read. A label that
is the same string on every screen it will ever appear on is not information. Two more surfaces
printed `health.namespace` under the word "tenant", which is not a tenant — it is the Temporal
namespace.

**If there is exactly one tenant, naming it on screen is like printing the hostname of the machine
you are already sitting at.** What varies, and what should decide what every surface shows, is the
workspace.

### A workspace is a filesystem pointer. Every store is a process constant.

ADR 0049 made a folder an Actor or a Workflow because of where it sits. The machinery is a parent
directory (`KONTRA_WORKSPACES`), one child per workspace, and a `.current` file naming the active
one. `workspaceRoot()` reads it; discovery and watch re-read it on every pass.

`workspaces.ts` states the limit of that in its own header, in one sentence:

> A workspace is a code folder (actors/ + workflows/). **Datasets, runs and login stay
> cluster-wide.** Switching rewrites `.current`; discovery and watch re-read it. Nothing remounts.

Measured, on this checkout. Three files in the orchestrator read the workspace at all:

    control/orchestrator/src/sources.ts           code discovery
    control/orchestrator/src/workflowControl.ts   resolving a folder to serve or start
    control/orchestrator/src/workspaces.ts        the machinery itself

and the count of the string `workspace` in every module that owns a store is zero:

    routes/datasets.ts              0
    data/parquet.ts                 0     the lake
    data/materializationStore.ts    0     the ledger
    codec/objectStore.ts            0     the object store
    temporalClient.ts               0     the namespace
    routes/logs.ts                  0     VictoriaLogs

Each of those resolves its target **once, from the environment, at process boot**:

```ts
export const NAMESPACE = process.env.KONTRA_NAMESPACE ?? 'default';   // temporalClient.ts
this.bucket = opts.bucket ?? env.KONTRA_S3_BUCKET ?? 'kontra';        // objectStore.ts
```

`NAMESPACE` is a module-level `const`. **A workspace switch cannot change any of these without
restarting the process**, and today it does not try to.

### What that costs, concretely

Switch from `default` to `bugbounty` and open Datasets: `canary_signals` is still there, with the
`default` workspace's ten rows, because the lake has one catalog and one schema and nothing in the
listing path has ever heard of a workspace. The same is true of the run list, the log rail, the
secrets, the fleet and the Pulumi state. The workspace picker changes which code you can *serve*
and nothing about what you *see*.

For a single operator flipping between `default` and `scraping`, that is untidy. For the thing this
is about to become — a public release where a bug-bounty engagement and a client scrape sit in two
workspaces on one install — it is a **data-separation failure**, not a cosmetic one. A dataset from
one engagement listing under another is exactly the outcome the boundary exists to prevent, and the
current design has no mechanism that could prevent it.

## Decision

**1. The workspace is the isolation boundary.** Every per-workspace store is addressed by the
workspace, and there is no store that is "cluster-wide" except the ones that are genuinely about the
install itself (the console login, the user table, the audit log).

| what | today | per workspace |
|---|---|---|
| code (actors/, workflows/) | already per workspace (ADR 0049) | unchanged |
| Temporal | one namespace, `KONTRA_NAMESPACE` | one namespace per workspace |
| datasets | one DuckLake catalog + schema | one catalog per workspace |
| materialization ledger | one table | one table, or one row-scope, per workspace |
| object store | one bucket | one prefix per workspace, one bucket |
| logs | one VictoriaLogs stream space | one VictoriaLogs tenant per workspace |
| metrics | one VictoriaMetrics | one tenant per workspace |
| secrets | one store | one namespace per workspace |
| Pulumi | one project | one project (or stack) per workspace |
| fleet / leases | cluster-wide | per workspace, because they follow Temporal |
| login, users, audit | cluster-wide | **stays cluster-wide** — it is the install's |

**2. The Temporal namespace is the anchor.** Runs, queues, visibility, schedules, leases and fleet
operations are all Temporal objects, so isolating the namespace isolates all of them at once and
without a single new filter. It is also the only one of these that Temporal will enforce for us: a
client bound to namespace `ws-bugbounty` cannot read `ws-default` by constructing a different query.
Every other store on the list needs code to honour its boundary; this one needs configuration.

**3. Isolation is by ADDRESS, never by FILTER.** A `WHERE workspace = ?` on a shared table is a
boundary that is one forgotten clause away from not existing, and the forgotten clause reads as an
ordinary result. A separate namespace, catalog, prefix or tenant fails the other way: get the
address wrong and you see nothing, loudly. Given the sentence "everything needs to be neat and tidy,
fast and SECURE", the failure mode that shows up as an empty page is the correct one to choose.

**4. A store handle is resolved per request, not per process.** The boot-time `const` is what makes
this change structural rather than a patch. `NAMESPACE`, the lake config, the bucket prefix and the
logs tenant become functions of a workspace name, and the request carries the workspace. A process
serves every workspace; it does not restart to switch.

**5. `tenant` leaves the frontend, and the word stops being used for three different things.** The
install is the tenant, so nothing on screen says so. Where a surface was showing the Temporal
namespace under the label "tenant", it says "Temporal namespace". The `tenant` field stays on the
wire and in the log records for now — removing it from the record schemas is a separate migration —
but no surface renders it as though the reader had a choice about it.

**6. Migration is additive, and the existing install keeps working.** The `default` workspace maps
to exactly what exists today: namespace `default`, the current catalog, no object-store prefix, the
current logs tenant. So nothing moves, no data is rewritten, and an install that never creates a
second workspace sees no change at all. A NEW workspace gets a new namespace, a new catalog and a
new prefix on creation.

## Consequences

**Creating a workspace stops being free.** It is `mkdir` today. It becomes: register a Temporal
namespace, create a DuckLake catalog, create a logs tenant. That is real work with real failure
modes, and `POST /api/workspaces` has to report them rather than returning 200 over a half-made
workspace. It should be a workflow, for the reason every other provisioning step in kontra is one:
a script that dies halfway leaves the mess standing.

**Deleting a workspace becomes meaningful and dangerous.** Today it is a directory nobody deletes.
Afterwards it names a namespace, a catalog and a bucket prefix full of somebody's engagement data.
Deletion is out of scope here and must not be added casually.

**Cross-workspace reads become impossible rather than merely absent.** A console that wanted "every
run on this install" would have to fan out across namespaces. That is the point, and the cost is
real: `/api/runs` with no workspace is not a question with an answer any more.

**The console must carry the workspace on every request.** It has a picker already; what it does not
have is the workspace travelling with each call. Until it does, a switch that changes the code root
and not the data root is exactly the half-isolation this ADR is about.

**The three boot-time constants are load-bearing and each is a separate slice.** Namespace, lake and
object store can be done independently and in that order, because each is isolated on its own and
Temporal carries the most per unit of work.

## What this does not decide

- **How a Temporal namespace is provisioned** on the appliance versus on a cluster, or what its
  retention is. `kontra up` runs Temporal in-process; namespace registration there is not the same
  operation as against a real cluster.
- **Whether the ledger is one table or many.** It is keyed by `run_id`, and run ids are unique across
  namespaces, so a shared table with a workspace column is defensible here in a way it is not for
  the lake. Decided when the slice is written, not before.
- **Whether `tenant` is removed from the record schemas.** It is on the wire, in the log records and
  in the dataset provenance columns. Removing it is a migration with a compatibility story; this ADR
  only stops rendering it.
- **What happens to a Fleet mid-flight when a workspace is switched.** Leases follow Temporal, so a
  Fleet belongs to a workspace — but the operator switching workspaces in the console while a Fleet
  is up is a state nobody has designed yet.
