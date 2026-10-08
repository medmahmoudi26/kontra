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

/*
 * `MATERIALIZER_QUEUE` / `materializerQueue()` WERE HERE AND ARE GONE (2026-09-26).
 *
 * `kontra-materializer` carried `declareMaterialization`, `materializeNode` and
 * `recordMaterializationFailure`, whose own file header recorded that nothing in this repository
 * called them: the v1 graph interpreter was their only caller and ADR 0023 §1 took materialization
 * off the interpreter. Measured before removal — a live poller on the queue, an add rate and a
 * dispatch rate of zero, and every activity a real run schedules landing on `kontra-datasets`.
 *
 * `KONTRA_MATERIALIZER_QUEUE` is therefore no longer read. Typed output still goes into DuckLake —
 * `publishBatch` -> `writeDatasetParquet`, on {@link DATASET_QUEUE} — so the ROLE keeps its name;
 * what went was one queue and three activities, not the job.
 */

/**
 * THE DATASET QUEUE — and since the removal above, the only queue the materializer role serves.
 *
 * It began as dataset PAGING alone (`workflows.dataset(name).batches(...)`), split off from the
 * decode queue because the two wanted opposite tuning. The decode queue is gone and the split
 * outlived it: everything that needs the DuckLake attach and an ObjectStore is here now —
 * `publishBatch` (the typed-output WRITE), the page reads, the two fleet reads, the three Lease
 * calls, and the retention sweep with its reclamation chain.
 *
 * WHICH MAKES ITS SLOT COUNT LOAD-BEARING IN A WAY THE NAME DOES NOT SUGGEST. `KONTRA_DATASET_SLOTS`
 * governs all of it, and at one slot a single stuck activity stops publishes, page reads, lease
 * holds and retention together — measured on 2026-09-25, when a closed shared DuckDB handle held
 * the only slot and the next run's `publishBatch` sat at `ACTIVITY_TASK_SCHEDULED` against a
 * poller Temporal reported as healthy.
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
 * workflow host. So an install that has no provisioner still serves this queue, with
 * `stackWorkflow` registered as a REFUSAL (`workflows/noProvisioner.ts`) rather than absent: a type
 * nobody registered does not fail a `fleet.up()`, it hangs it.
 */
export const INFRA_QUEUE = 'kontra-infra';

/** The controller-pinned queue in effect. See {@link INFRA_QUEUE}. */
export function infraQueue(): string {
  return process.env.KONTRA_INFRA_QUEUE || INFRA_QUEUE;
}

/**
 * SERVE-DEV — the second workload on {@link INFRA_QUEUE}, and the two strings its halves agree on.
 *
 * WHY A TYPE NAME AND AN ID LIVE IN THE QUEUE FILE. The same reasoning that put `PROBE_WORKFLOW`
 * below: what is being recorded is a ROUTING CONTRACT — the queue to start it on, the type to
 * start, and the id to start it under — and those three are one fact with three spellings. Here the
 * fact has FOUR readers, which is what made a shared home necessary rather than merely tidy:
 *
 *   - `actorControl.serveActor`    starts it       (`serve-dev/at:<folder>`)
 *   - `workflowControl.serveWorkflow` starts it    (the same id, derived from the workflow's dir)
 *   - `visibility.ts`              HIDES it from the Runs page, by type
 *   - `temporalClient.listServes`  SHOWS it on the Actors page, by type AND id
 *
 * The last two are the halves of one feature and they fail in opposite directions. A type name
 * that drifts from the start site makes the exclusion stop excluding — a wall of `serveDevWorkflow`
 * rows on the Runs page, wrong but loud. The SAME drift makes the history stop finding anything —
 * an actor that has been served forty times reporting "nothing has served this yet", which is wrong
 * and SILENT, and is the failure this const exists to make impossible.
 *
 * NO ENVIRONMENT OVERRIDE, unlike every queue above it. A queue name is overridable so a second
 * control plane can share a cluster; a workflow TYPE is what the worker registered
 * (`workflows/infra.ts`), and an override would only ever route a start to a type nobody serves.
 */
export const SERVE_DEV_WORKFLOW = 'serveDevWorkflow';

/**
 * The workflow id one folder's serves run under: `serve-dev/` + the Source id, which is
 * `at:<absolute folder path>` (`sourceStore.ts`).
 *
 * DERIVED, LIKE THE LEASE'S (`lease.ts:leaseWorkflowId`). Nothing allocates it, so the read half
 * can address a folder's serve history with nothing to look up — and, more to the point, cannot
 * address a DIFFERENT one than the write half wrote.
 *
 * THE ID IS ALSO THE CONCURRENCY RULE. Both start sites pass `workflowIdConflictPolicy: 'FAIL'`, so
 * one folder can have one serve in flight and Temporal is what refuses the second — a property of
 * this string rather than of a check somebody remembered to write. It follows that the id is REUSED
 * across every serve of a folder, which is why the history below is a list of EXECUTIONS under one
 * id and never a list of ids.
 */
export function serveDevWorkflowId(sourceId: string): string {
  return `serve-dev/${sourceId}`;
}

/**
 * BUILDING AN ACTOR IMAGE — the second caller of the API→infra channel, for the same reason as the
 * first. `kontra deploy` drives `docker build` and `docker push`, which needs the Docker socket,
 * and `kontra-api` is the container with the published port and the untrusted HTTP input. It has
 * no socket and must not get one, so the build moves to the authority rather than the authority
 * moving to the build. See `activities/serveDev.ts` for the whole argument; nothing here is new
 * privilege, it is one existing privilege gaining one more caller.
 */
export const BUILD_ACTOR_WORKFLOW = 'buildActorWorkflow';

/**
 * The workflow id one folder's builds run under.
 *
 * SEPARATE FROM `serveDevWorkflowId`, so a build and a serve of the same folder do not refuse each
 * other. They are different operations on one directory and there is no reason pressing Build
 * should be blocked by a serve that is already running — but two concurrent BUILDS of one folder
 * write the same image tag, so those must still collide. Same-prefix-different-verb gives exactly
 * that: `FAIL` on this id refuses a second build of this folder and nothing else.
 */
export function buildActorWorkflowId(sourceId: string): string {
  return `build-actor/${sourceId}`;
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
