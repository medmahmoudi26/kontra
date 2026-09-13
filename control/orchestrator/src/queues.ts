/**
 * Temporal task-queue names — the routing contract between the orchestrator's processes.
 *
 * A queue name is the ONLY thing that decides which host runs which work, so the constant lives
 * in one place shared by the worker that serves the queue and anything that routes to it.
 *
 * There is no orchestrator EXECUTION queue any more. It carried the graph interpreter and its
 * data-routing activities; a Run is now one execution of a caller's workflow, on the caller's own
 * task queue (ADR 0023 §12), so the work that queue existed for happens somewhere this process
 * does not own. What is left is the one workload that must be placed rather than merely run.
 *
 * SINCE ADR 0031 §1 THE PROCESSES CAN MERGE AND THE QUEUES DO NOT. `orchestrator-api`,
 * `orchestrator-materializer` and `orchestrator-infra` are three roles that may share one PID
 * (`main.ts`), and every constant below is unchanged by that: a queue is HOW WORK IS ROUTED and a
 * process is only WHERE IT RUNS. `roles.ts` turns that sentence into a boot-time assertion — two
 * roles handed one queue is a routing contract that cannot be honoured, whatever the topology.
 *
 * NO WORKFLOW IMPORTS THIS MODULE ANY MORE, and the env reads stay inside functions anyway.
 *
 * `workflows/retention.ts` was the last one that did. It took `DATASET_QUEUE` and pinned its sweep
 * activity to the CONSTANT, so `KONTRA_DATASET_QUEUE` could not route a sweep at all — which is how
 * a smoke test with three isolated queue names reached the live materializer and deleted 223,378
 * rows (`.scratch/post-merge-review/INCIDENT-2026-08-26.md`). The sweep's queue now travels as
 * workflow INPUT, resolved by `src/retention.ts` out here where an environment exists.
 *
 * THE RULE STANDS FOR WHATEVER IMPORTS THIS NEXT, and it is stronger than "determinism". Workflow
 * code runs in a `vm` context created EMPTY, so `process` in there is not a hazard, it is UNDEFINED:
 * a module-scope `process.env` read reached from a workflow bundle throws `ReferenceError: process
 * is not defined` while the bundle is being imported, which fails the workflow task for EVERY type
 * in that bundle, retries forever, and surfaces to the caller as a run that simply never progresses.
 * Inside a function the import is free and only a CALL is fatal — so a default is a literal, an
 * override is a call, and a workflow may import a name from here but must never call one.
 */

/**
 * Typed-output materialization. Served ONLY by the isolated materializer worker, which runs
 * off-controller with its own cgroup, its own DuckDB memory budget and one slot. Embedded DuckDB
 * is the largest memory consumer in this system and the controller is a 4 GB host that also runs
 * Temporal, SeaweedFS and Postgres — so the placement is enforced by routing rather than by
 * convention.
 */
export const MATERIALIZER_QUEUE = 'kontra-materializer';

/** The materialization queue in effect. See {@link MATERIALIZER_QUEUE}. */
export function materializerQueue(): string {
  return process.env.KONTRA_MATERIALIZER_QUEUE || MATERIALIZER_QUEUE;
}

/**
 * Dataset PAGING — `workflows.dataset(name).batches(...)` on the caller's side.
 *
 * Served by the same worker as materialization (it already holds the DuckLake attach and an
 * ObjectStore), but on its OWN queue, because the two workloads want opposite tuning. A
 * materialization is one bounded, memory-hungry decode at a time; paging is a short read that a
 * caller's loop blocks on, once per page, and a caller stuck behind a 40-minute decode would
 * look like a hung workflow. Separate queues is what lets the slot counts diverge without
 * either side being renamed.
 */
export const DATASET_QUEUE = 'kontra-datasets';

/** The paging queue in effect. See {@link DATASET_QUEUE}. */
export function datasetQueue(): string {
  return process.env.KONTRA_DATASET_QUEUE || DATASET_QUEUE;
}

/**
 * THE CONTROLLER-PINNED QUEUE — and it is not "the Pulumi queue" (ADR 0031 §4, ADR 0034 §2).
 *
 * It moved here from `infra.ts` when the roles merged, because that file also carries the Pulumi
 * engine and the cloud credential, and a queue NAME is neither: `infraRoutes.ts` needs the address
 * to start a stack op, and `roles.ts` needs it to prove no two roles collide, and neither should
 * have to load a provisioner to learn a string.
 *
 * Three workflows share it and only one of them provisions. `tmuxSessionWorkflow` is session
 * existence on a Machine — Fleet authority, and what the Monitor is built on — and
 * `sweepDatasetsWorkflow` is the retention sweep, hosted here because this is the controller-pinned
 * workflow host. So an appliance that has no provisioner still serves this queue, with
 * `stackWorkflow` registered as a REFUSAL (`workflows/appliance.ts`) rather than absent: a type
 * nobody registered does not fail a `fleet.up()`, it hangs it.
 */
export const INFRA_QUEUE = 'kontra-infra';

/** The controller-pinned queue in effect. See {@link INFRA_QUEUE}. */
export function infraQueue(): string {
  return process.env.KONTRA_INFRA_QUEUE || INFRA_QUEUE;
}

/**
 * THE ACTOR PROBE (ADR 0033) — a kontra-owned queue that serves exactly ONE workflow type.
 *
 * That constraint is part of the line, not an implementation note. A kontra queue serving
 * ARBITRARY caller workflows is the execution queue this file's header says was deleted, rebuilt
 * under a different name; one type, and a second type on this queue is a reviewable event.
 *
 * It is not served by any process in this repo's Node images. A Nexus operation can only be
 * scheduled by a WORKFLOW command, in every SDK we have, and TypeScript has no caller half to
 * reuse (ADR 0023 §22) — so the probe workflow is actorkit's, in Python
 * (`sdk/python/kontra/probe.py`), and this orchestrator only STARTS it. What lives here is the
 * routing contract: the queue to start it on and the type to start.
 *
 * THE SECOND INDEPENDENT DERIVATION of both strings; the peer is `kontra.probe`, and
 * `tests/test_actor_probe.py` pins the pair the way `test_queue_congruence.py` pins the others.
 * `KONTRA_PROBE_QUEUE` moves both halves at once, for a second control plane on one cluster.
 */
export const PROBE_QUEUE = 'kontra-probe';

/** The `@workflow.defn` name the probe worker serves. See {@link PROBE_QUEUE}. */
export const PROBE_WORKFLOW = 'ActorProbe';

/** The queue in effect, honouring the same override the Python side reads. */
export function probeQueue(): string {
  return process.env.KONTRA_PROBE_QUEUE || PROBE_QUEUE;
}
