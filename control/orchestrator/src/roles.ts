/**
 * WHICH ROLES THIS PROCESS SERVES — and the one invariant that survives merging them (ADR 0031 §1).
 *
 * `orchestrator-api`, `orchestrator-materializer` and `orchestrator-infra` were three `command:`
 * entries over one image. They are three ROLES now, and `main.ts` runs any subset of them in one
 * PID. Nothing about the work changed: the API still serves the SPA and the HTTP surface, the
 * materializer still holds the only DuckLake writer, and the infra role still hosts the
 * controller-pinned workflows.
 *
 * **TASK QUEUES DO NOT MERGE WHEN PROCESSES DO.** A queue is HOW WORK IS ROUTED; a process is only
 * WHERE IT RUNS (ADR 0023 §6 — the queue name is the address). `queues.ts` already argues the
 * materializer/paging split on tuning grounds — a caller's page read must not queue behind a
 * forty-minute decode — and that argument does not care how many processes exist.
 *
 * WHICH IS WHY {@link assertDistinctQueues} EXISTS. Three queue names come from three environment
 * variables, and once they are polled by one process nothing else notices when two of them are the
 * same string. The failure is silent and total: one worker's poller wins each task, so a
 * materialization lands on a worker with no `materializeNode` and fails the task, forever, while
 * every surface reports a healthy process. This is the invisible-failure mode this product exists
 * to remove, so it is a boot refusal instead — before a connection, before a poll.
 *
 * The PROBE queue is checked too even though nothing here serves it. `orchestrator-probe` is a
 * Python process on `kontra-probe` (ADR 0033), and a role handed that name would quietly steal
 * `ActorProbe` tasks it cannot run — the same failure, one container further away.
 */

import { datasetQueue, infraQueue, materializerQueue, probeQueue } from './queues';

/** The three roles, in the order `main.ts` starts them. */
export const ROLES = ['api', 'materializer', 'infra'] as const;

export type Role = (typeof ROLES)[number];

/** The variable that names the roles. Unset means all three — the appliance's whole point. */
export const ROLES_VAR = 'KONTRA_ORCHESTRATOR_ROLES';

/**
 * Parse `KONTRA_ORCHESTRATOR_ROLES`.
 *
 * DEFAULTS TO ALL THREE, because the appliance is one process and an operator who names nothing
 * should get a whole control plane. A compose controller that keeps a separate provisioner names
 * its own subset (ADR 0034 §1), and naming a subset is the only way to get one — there is no
 * "everything except" spelling, which is how a role gets dropped by a typo.
 *
 * AN UNKNOWN NAME IS FATAL, not ignored. `KONTRA_ORCHESTRATOR_ROLES=api,materialiser` must not
 * start an API with no materializer and no complaint; the whole class of "the process came up and
 * silently does less than you asked" is what this slice is trying not to create.
 */
export function resolveRoles(raw: string | undefined = process.env[ROLES_VAR]): Role[] {
  const named = (raw ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
  if (named.length === 0) return [...ROLES];

  const unknown = named.filter((n) => !(ROLES as readonly string[]).includes(n));
  if (unknown.length > 0) {
    throw new Error(
      `${ROLES_VAR}=${raw}: no such role ${unknown.map((u) => `"${u}"`).join(', ')} ` +
        `(the roles are ${ROLES.join(', ')}). Leave it unset for all three.`
    );
  }
  // Deduplicated and put back in ROLES order, so the boot log reads the same however it was typed.
  return ROLES.filter((r) => named.includes(r));
}

/** One queue this process will poll, and what to edit if it is wrong. */
export interface QueueAssignment {
  role: Role;
  /** What the queue carries, in the vocabulary the surfaces use. */
  purpose: string;
  /** The name that will actually be polled. */
  queue: string;
  /** The variable that set it — the only actionable half of a collision message. */
  variable: string;
}

/**
 * The queues the named roles will poll, resolved exactly the way the workers resolve them.
 *
 * THROUGH `queues.ts`, NEVER RE-DERIVED. A guard that reads `KONTRA_INFRA_QUEUE` itself would
 * agree with the worker until one of them changed, and then it would pass while the process it was
 * guarding polled something else.
 *
 * The API role appears in no row: it is a Temporal CLIENT, it starts workflows onto queues other
 * processes serve, and it polls nothing.
 */
export function queueAssignments(roles: readonly Role[]): QueueAssignment[] {
  const rows: QueueAssignment[] = [];
  if (roles.includes('materializer')) {
    rows.push({
      role: 'materializer',
      purpose: 'typed-output materialization',
      queue: materializerQueue(),
      variable: 'KONTRA_MATERIALIZER_QUEUE',
    });
    rows.push({
      role: 'materializer',
      purpose: 'dataset paging',
      queue: datasetQueue(),
      variable: 'KONTRA_DATASET_QUEUE',
    });
  }
  if (roles.includes('infra')) {
    rows.push({
      role: 'infra',
      purpose: 'the controller-pinned workflows',
      queue: infraQueue(),
      variable: 'KONTRA_INFRA_QUEUE',
    });
  }
  return rows;
}

/**
 * Refuse to boot when two of this process's queues are the same string, or when one of them is the
 * probe's.
 *
 * LOUD, AND WITH BOTH HALVES NAMED. The message carries the two purposes and the two variables,
 * because "duplicate task queue" on its own leaves an operator diffing a compose file — and the
 * only fix is to edit one of the two variables it names.
 */
export function assertDistinctQueues(roles: readonly Role[]): void {
  const rows = queueAssignments(roles);
  const seen = new Map<string, QueueAssignment>();
  for (const row of rows) {
    const clash = seen.get(row.queue);
    if (clash) {
      throw new Error(
        `two of this process's task queues resolved to the same name "${row.queue}": ` +
          `${clash.purpose} (${clash.variable}) and ${row.purpose} (${row.variable}). ` +
          'Merging the processes does not merge the queues (ADR 0031 §1) — ' +
          'give each of them its own name, or unset one to take its default.'
      );
    }
    seen.set(row.queue, row);
  }

  const probe = probeQueue();
  const stolen = seen.get(probe);
  if (stolen) {
    throw new Error(
      `${stolen.variable} points ${stolen.purpose} at "${probe}", which is the actor probe's own ` +
        'queue (KONTRA_PROBE_QUEUE). This process would take `ActorProbe` tasks it cannot run and ' +
        'fail them forever, while the Actors page reports nothing.'
    );
  }
}
