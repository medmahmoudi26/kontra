/**
 * Which of this process's Workers is this — the orchestrator's half of `internals/workerid.py`.
 *
 * ── THE PROBLEM THIS SOLVES IS SPECIFIC TO A MERGED PROCESS ─────────────────────────────────────
 *
 * `docker-compose.yml` runs the API and the materializer in ONE container ("queues stay distinct"),
 * and that container runs four Workers: the materializer's decoder, the dataset pager, the infra
 * role's install worker and whatever a `serve` starts beside them. Left at the SDK default they
 * all identify as the same `<pid>@<hostname>` — one string for four Workers with four different
 * jobs and four different failure modes. A poller listing then shows the same identity on four
 * queues, and "which Worker stopped polling" is not answerable from it.
 *
 * The queue is what separates them, and Temporal's identity has a field for it.
 *
 * ── THE HOSTNAME PIN IN COMPOSE IS THE WORKAROUND THIS REPLACES ─────────────────────────────────
 *
 * `docker-compose.yml` pins `container_name` with the comment "A STABLE HOSTNAME, BECAUSE A
 * WORKER'S TEMPORAL IDENTITY IS `<pid>@<hostname>`" — a deployment-level fix for an unset SDK
 * argument, and one that only works for containers this repo's compose file names. It is still
 * correct and still wanted (a stable host portion is a good thing) but it is no longer load-bearing:
 * the identity is stated here, so a Worker started outside compose is attributable too.
 *
 * ── ROLE, BECAUSE A QUEUE NAME IS NOT ALWAYS A JOB ──────────────────────────────────────────────
 *
 * `KONTRA_WORKER_ROLE` labels the process when the queue does not already say what it is for. It is
 * a LABEL and never an address — nothing routes on it — which is why an unset value is fine and
 * produces an identity one field shorter rather than an error.
 */

import os from 'node:os';

import { workerIdentity as compose } from '@kontra/core/queues';

/**
 * This host's name, snapshotted once.
 *
 * A hostname does not change under a live process, and this is read on every Worker construction
 * and on every audit line. In compose it is `container_name`; on a Fleet Machine it is the
 * Droplet's name, because a Worker there is a pair of systemd units and not a container.
 */
export const MACHINE = os.hostname();

/** What this process is for, when its queue does not say. A label; nothing routes on it. */
export const ROLE = process.env.KONTRA_WORKER_ROLE ?? '';

/**
 * `<pid>@<hostname>@<queue>` — what every `Worker.create` in this process passes as `identity`.
 *
 * The derivation itself is in `@kontra/core/queues` beside `identityHost`, which parses it. Keeping
 * the two together is what makes `identityHost(workerIdentity(...))` a property a test can assert,
 * rather than a convention two files agree on until one of them is edited.
 */
export function identityFor(queue: string): string {
  return compose(process.pid, MACHINE, queue);
}

/**
 * The identity for calls this process MAKES rather than tasks it takes — the client half.
 *
 * A client polls nothing, so field three cannot be a queue; the ROLE goes there instead, and an
 * unset role leaves it empty — which is exactly what the Go SDK writes for a client, and therefore
 * already a shape `identityHost` reads. Keeping the trailing `@` rather than dropping the field is
 * deliberate: a parser forced to special-case the shorter string grows a branch only one SDK ever
 * exercises.
 *
 * WITHOUT THIS, ONE PROCESS WEARS TWO NAMES IN ONE RUN'S HISTORY. Temporal records the client
 * identity on the calls this process makes — starting a workflow, signalling a stream — and the
 * worker identity on the tasks it takes. Setting only the second attributes half of what the
 * orchestrator did to a string nothing else in the system uses.
 */
export function clientIdentity(): string {
  return compose(process.pid, MACHINE, ROLE);
}
