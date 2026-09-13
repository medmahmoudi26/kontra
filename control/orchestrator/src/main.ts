/**
 * THE ORCHESTRATOR PROCESS — three roles, three queues, one PID (ADR 0031 §1).
 *
 *   node dist/src/main.js
 *
 * `orchestrator-api`, `orchestrator-materializer` and `orchestrator-infra` were three `command:`
 * entries over one image, and three containers is three things that can be the wrong version, fail
 * to start, or be missing on a fresh machine. They become three roles here — one process the
 * appliance runs as a single child, in place of three it would otherwise have to supervise.
 *
 * **THE MERGE IS ONLY SAFE BECAUSE PULUMI IS NOT IN IT.** `infra.ts`'s header records the measured
 * reason the infra role was a separate process: Pulumi's Node language host installs PROCESS-GLOBAL
 * `unhandledRejection` / `uncaughtException` handlers for the duration of every inline run, so an
 * unrelated rejected promise anywhere in the process fails the in-flight `up` — and the process
 * survives, reporting healthy. Co-locating an engine with a Fastify server and a DuckDB decode is a
 * correctness problem, not a tuning one. ADR 0031 §4 removes the engine from the appliance, and
 * removing the engine is what removes the hazard. {@link runInfra} therefore registers
 * `workflows/appliance.ts` and never `workflows/infra.ts`.
 *
 * MEASURED, not asserted: no `@pulumi/*` package is reachable from this module, and neither
 * `DIGITALOCEAN_TOKEN` nor `PULUMI_CONFIG_PASSPHRASE` is read anywhere in the graph — the API's
 * `parseFqn` used to drag the whole Automation API in, which is why `infra/paths.ts` exists. The
 * one Pulumi variable still read here is `KONTRA_PULUMI_STATE_DIR`, by the READ side: the `/infra`
 * dashboard lists checkpoint files, and on an appliance that directory does not exist and the page
 * says there are no stacks.
 *
 * Two consequences follow and are worth stating rather than discovering:
 *
 *   - **The Dashboard streamer stops being a forked child.** It was forked for the same measured
 *     reason and inherits the same relief (ADR 0031 §4: "no engine, no fork"). It runs in this
 *     process, so there is no second PID for the binary to orphan.
 *   - **A compose controller that keeps the provisioner must not run the infra role here.** Both
 *     workers would poll `kontra-infra` and one of them refuses every `fleet.up()` it happens to
 *     win. That deployment names `KONTRA_ORCHESTRATOR_ROLES=api,materializer` and keeps
 *     `orchestrator-infra` beside this process (ADR 0034 §1).
 *
 * **TASK QUEUES DO NOT MERGE WHEN PROCESSES DO** — see `roles.ts`, which refuses to boot if two of
 * them were handed the same name.
 */

import { NativeConnection, Worker } from '@temporalio/worker';

import * as panelActivities from './activities/panels';
import { acquireCatalogLock } from './data/catalogLock';
import { runMaterializer } from './materializer';
import { main as runPanels } from './panels';
import { infraQueue } from './queues';
import { armRetentionSchedule } from './retention';
import { assertDistinctQueues, queueAssignments, resolveRoles, ROLES_VAR, type Role } from './roles';
import { runApi } from './server';
import { getClient } from './temporalClient';
import { temporalConnectOptions } from './temporalTls';

function log(line: string): void {
  // eslint-disable-next-line no-console
  console.log(`[orchestrator] ${line}`);
}

/**
 * The INFRA role, minus the provisioner.
 *
 * Everything that made this a separate container is gone from it: no `assertBackend`, no
 * `adoptLegacyCloudToken`, no engine, no `activities/infra` — those are the Pulumi half, and they
 * stay in `infra.ts` for the compose controller that still runs it. What is left is the
 * controller-pinned queue and the two workflows that were never about provisioning.
 *
 * ONE ACTIVITY AT A TIME IS NOT INHERITED. `infra.ts` pins `maxConcurrentActivityTaskExecutions: 1`
 * because the engine forks a `pulumi` CLI child plus one per provider and would OOM a capped
 * container mid-apply. With no engine the only activity left is `convergeTmuxSession`, which is one
 * blocking `ssh` — and serialising those behind each other is a wall of tiles that fills one pane
 * at a time. The Temporal default applies instead, deliberately.
 */
export async function runInfra(): Promise<void> {
  const address = process.env.KONTRA_ADDRESS ?? 'localhost:7233';
  const namespace = process.env.KONTRA_NAMESPACE ?? 'default';
  const queue = infraQueue();

  // THE DASHBOARD STREAMER FIRST, AND NEVER AWAITED PAST ITS LISTEN. `infra.ts` forks it before
  // touching Temporal for a reason that survives the merge: the Monitor's read path is SSH and a
  // tmux socket, so a Temporal outage should cost the converge button and not the whole wall.
  // `runPanels` resolves once its port is bound and leaves the server alive.
  //
  // NOT A FORKED CHILD ANY MORE (ADR 0031 §4). It was forked because Pulumi's process-global
  // handlers would have turned an unhandled rejection in panel code into a failed `up` — "no
  // engine, no fork". One less PID is also one less thing for the binary to orphan.
  await runPanels().catch((err) => {
    // A streamer that cannot bind must not take the control plane with it — the Monitor goes
    // blank, everything else keeps working, and the reason is in the log rather than in an exit
    // code nobody sees.
    log(`infra role: the Dashboard streamer did not start: ${message(err)}`);
  });

  // An explicit connection, not the default. `connection: undefined` silently means
  // localhost:7233 — right on a laptop and always wrong in a container, where the failure reads as
  // an unrelated tonic transport error against ::1.
  const connection = await NativeConnection.connect(temporalConnectOptions({ address }));

  const worker = await Worker.create({
    // THE APPLIANCE BUNDLE. `stackWorkflow` is in it and refuses; leaving the TYPE out would not
    // fail a `fleet.up()`, it would hang one — the task is taken, no such type is found, the task
    // fails, and Temporal retries it forever. See `workflows/appliance.ts`.
    workflowsPath: require.resolve('./workflows/appliance'),
    // The panel activities only. `activities/infra.ts` is the Pulumi half and is not imported by
    // this file at all, so nothing here can resolve a cloud credential even by mistake.
    activities: { ...panelActivities },
    taskQueue: queue,
    namespace,
    connection,
  });

  log(`infra role: queue=${queue} (no provisioner — stackWorkflow is registered and refuses)`);

  // ARM THE RETENTION SWEEP (ADR 0029 §5) — after the Worker exists, before it polls, and HERE
  // because this is the process hosting `sweepDatasetsWorkflow`. The Schedule targets this queue,
  // so arming it anywhere else could register an hourly firing onto a queue with no poller: a
  // workflow per hour that never starts, with `SKIP` skipping every firing behind the first, which
  // looks armed and sweeps nothing.
  //
  // It cannot fail this boot (see `armRetentionSchedule`), and it deletes nothing by default — a
  // firing resolves its mode on the worker holding the lake, where unset means dry run.
  //
  // ON THE API'S CLIENT, not a second connection of its own. `infra.ts` opens one and closes it
  // again because a `NativeConnection` is the Rust core's and the Schedule API lives on
  // `@temporalio/client`'s; in a merged process that client already exists and is already built
  // with the repo's `dataConverter`, which is the property that mattered.
  try {
    const client = await getClient();
    await armRetentionSchedule(client.schedule, {
      taskQueue: queue,
      log: (line) => log(`infra role: ${line}`),
    });
  } catch (err) {
    // Reaching Temporal at all is the worker's problem, not this one's — it is about to try the
    // same address and will fail loudly if it cannot. Housekeeping never takes the process down.
    log(`infra role: could not arm the retention schedule: ${message(err)}`);
  }

  await worker.run();
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Start one role and never resolve — the API is the exception and says so. */
function start(role: Role): Promise<unknown> {
  switch (role) {
    case 'api':
      return runApi();
    case 'materializer':
      return runMaterializer();
    case 'infra':
      return runInfra();
  }
}

export async function main(): Promise<void> {
  const roles = resolveRoles();

  // BEFORE A CONNECTION AND BEFORE A POLL. Two roles on one queue is a routing contract that
  // cannot be honoured, and it is invisible once the workers are running: each task goes to
  // whichever poller wins it, and the one that cannot run it fails it forever while every surface
  // reports a healthy process.
  assertDistinctQueues(roles);

  // THE CATALOG IS SINGLE-WRITER AND THIS IS WHERE THAT BECOMES VISIBLE (ADR 0031 §1b). The lake's
  // catalog is a file in the data directory now, not a Postgres a second process could share, so a
  // second control plane on the same directory is a corruption waiting for the first
  // materialization. `acquireCatalogLock` answers null — no lock, no refusal — when the catalog is
  // a connstring, because more than one process is exactly what that setting is for.
  //
  // ONLY THE ROLES THAT ATTACH IT. The API lists and previews Datasets and the materializer writes
  // them; the infra role touches no lake at all (the sweep's ACTIVITIES are registered on the
  // materializer — `materializer.ts` — and only its workflow is hosted there). A provisioner
  // sidecar started as `KONTRA_ORCHESTRATOR_ROLES=infra` beside a control plane must not be
  // refused for holding nothing.
  const lock = roles.some((r) => r === 'api' || r === 'materializer') ? acquireCatalogLock() : null;
  if (lock) {
    log(`catalog=${lock.catalog} (single-writer; lock ${lock.path})`);
    // Released on the way out so a clean restart does not have to reason about a stale record.
    // A `kill -9` skips this, which is what `holderAlive` is for.
    //
    // THE SIGNAL HANDLERS ARE NOT DECORATION. Node's DEFAULT for SIGTERM and SIGINT terminates the
    // process without running `exit` listeners, so `docker stop` — the ordinary way this container
    // ends — would leave the record behind every single time, and every restart would be a
    // stale-lock recovery. Nothing else here handles either signal today, so this changes no
    // shutdown behaviour beyond the release; the conventional 128+signal exit code is kept so a
    // supervisor still sees a signalled stop rather than a clean one.
    process.on('exit', () => lock.release());
    for (const [sig, code] of [['SIGINT', 130], ['SIGTERM', 143]] as const) {
      process.on(sig, () => {
        lock.release();
        process.exit(code);
      });
    }
  }

  log(`roles=${roles.join(',')} (${ROLES_VAR})`);
  for (const q of queueAssignments(roles)) log(`  ${q.queue} — ${q.purpose} [${q.role}]`);

  // `Promise.all` and not `allSettled`: the first REJECTION ends the process. A process still
  // running two of its three roles is the half-dead control plane this merge exists to stop
  // shipping — and it is worse than a crash, because the container is up and the surface that
  // would have told you is the one that died.
  //
  // The worker roles never resolve; the API's promise resolves once its port is bound, and the
  // listener is what keeps the event loop alive after that. So this settles only when every role
  // is either serving or has thrown.
  await Promise.all(roles.map(start));
}

if (require.main === module) {
  main().catch((err) => {
    // eslint-disable-next-line no-console
    console.error('[orchestrator] fatal:', err);
    process.exit(1);
  });
}
