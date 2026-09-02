/**
 * `orchestrator-infra` — the fourth orchestrator role (ADR 0019).
 *
 * A separate PROCESS, not a module inside orchestrator-api, and the reason is measured:
 * Pulumi's Node language host installs process-global `unhandledRejection` /
 * `uncaughtException` handlers for the duration of every inline run. An unrelated rejected
 * promise anywhere in the process fails the in-flight `up`, and Node's own crash handling is
 * suppressed while it runs. orchestrator-api serves the SPA, runs DuckDB queries and talks to
 * S3 — co-locating the engine with it is a correctness problem, not a tuning one.
 *
 * It is also the only process that can resolve the CLOUD CREDENTIAL, and it is pinned to the
 * Controller by its task queue rather than by convention. That used to mean "the only process
 * holding DIGITALOCEAN_TOKEN"; since ADR 0034 §4 the value comes out of the secret store at the
 * last hop, inside the activity about to converge, and what crosses every boundary above it is a
 * NAME. {@link adoptLegacyCloudToken} below is the one-time bridge for an installation whose token
 * is still in a `.env`.
 *
 * SINCE ADR 0020 it carries two more jobs, both for that same reason.
 *
 * It runs the SESSION CONVERGE (`tmuxSessionWorkflow`): session existence on a Machine is Fleet
 * authority and this is the only process holding `KONTRA_SSH_KEY`.
 *
 * And it FORKS AND SUPERVISES the Dashboard streamer (`dist/src/panels.js`) instead of hosting it.
 * ADR 0019's process-global hazard was re-measured on @pulumi/pulumi 3.256.0 and it fails silently:
 * an unhandled rejection injected two seconds into an inline `up()` made `up()` throw, reporting
 * the foreign error as `error: [runtime] Unhandled exception`, and **the process survived every
 * time**. So a co-located streamer would not crash this container — it would fail
 * `kontra fleet deploy`, blame panel code, and leave this process reporting healthy. The handlers
 * are process-global and not container-global, so a fork is the whole fix: same container, same
 * read-only key mount, same Fleet authority, different PID.
 *
 * Which is why nothing below AWAITS anything the child produces. {@link superviseChild} has no
 * async surface at all.
 *
 *   node dist/src/infra.js
 */

import { fork } from 'node:child_process';
import { Client, Connection } from '@temporalio/client';
import { NativeConnection, Worker } from '@temporalio/worker';
import * as infraActivities from './activities/infra';
import * as panelActivities from './activities/panels';
import { dataConverter } from './codec/dataConverter';
import { adoptLegacyCloudToken } from './infra/credential';
import { assertBackend, backendUrl } from './infra/workspace';
import { superviseChild, type Supervisor } from './panels/supervisor';
import { infraQueue } from './queues';
import { armRetentionSchedule } from './retention';

/**
 * The queue this process serves. It is `queues.ts`'s now — a queue NAME is a routing fact and not
 * a provisioner's, and `infraRoutes.ts` had to load this whole module, Pulumi included, to read a
 * string. Resolved once at load, exactly as the `const` it replaces was.
 */
const INFRA_QUEUE = infraQueue();

/**
 * Fork the streamer and keep it forked.
 *
 * `require.resolve` rather than a path literal so a missing build fails here, loudly, instead of
 * producing a child that exits instantly forever. Returns synchronously: the supervisor is a
 * handle, never a promise, so there is nothing on this path for the Pulumi engine's handlers to
 * catch on.
 */
export function supervisePanels(): Supervisor {
  const modulePath = require.resolve('./panels');
  return superviseChild({
    spawn: () =>
      fork(modulePath, [], {
        // The child inherits the key mount, the state directory and the Temporal address from this
        // environment — it is the same container by design. It does NOT get a Pulumi engine.
        env: process.env,
        stdio: ['ignore', 'inherit', 'inherit', 'ipc'],
      }),
    log: (line, extra) => {
      // eslint-disable-next-line no-console
      console.log(`[infra] panels: ${line}${extra ? ` ${JSON.stringify(extra)}` : ''}`);
    },
  });
}

async function main(): Promise<void> {
  // Fail closed at boot rather than at the first provision: a misconfigured backend does not
  // error, it silently deploys to Pulumi's SaaS.
  assertBackend();

  // The one-time move off the environment variable (ADR 0034 §4). It NEVER throws and it never
  // overwrites: an installation with the token in a `.env` gets it written into the store under
  // this control plane's credential name, once, loudly; one that has already rotated in the store
  // gets nothing. After it, `DIGITALOCEAN_TOKEN` is read nowhere — a fleet operation resolves the
  // NAME it was given, so a rotation takes effect on the next converge with no restart.
  await adoptLegacyCloudToken();

  // Fork the streamer FIRST, and never await it.
  //
  // Before Temporal, deliberately: the Dashboard's read path is SSH and a Pulumi checkpoint, so a
  // Temporal outage should cost the converge button, not the whole wall. `assertBackend()` above is
  // the one thing that must pass first — a process refusing to start should not leave a child
  // holding a published port.
  const panels = supervisePanels();

  // MEASURED, not assumed: without these handlers, `timeout 20 node dist/src/infra.js` left
  // `dist/src/panels.js` running. Node's default for SIGTERM/SIGINT with no listener is to terminate
  // immediately, so the `finally` below never runs and the child is ORPHANED — still holding port
  // 8090 and the read-only fleet key. In a container that is masked by the runtime reaping the
  // namespace; on a laptop it is a stale streamer nobody can see.
  //
  // The worker's own shutdown is deliberately unchanged by this: it had no signal handling before
  // this slice either, and making a `pulumi up` shut down gracefully is a separate change with
  // consequences for the lock (see activities/infra.ts).
  const onSignal = (signal: NodeJS.Signals): void => {
    // eslint-disable-next-line no-console
    console.log(`[infra] ${signal} — stopping the panels child`);
    // Exit only once the child is actually gone. `stop` escalates SIGTERM to SIGKILL and calls back
    // either way, so this cannot hang — and it cannot exit early and orphan a child that is still
    // shutting down, which is the whole point of handling the signal at all.
    panels.stop({ done: () => process.exit(0) });
  };
  process.once('SIGTERM', () => onSignal('SIGTERM'));
  process.once('SIGINT', () => onSignal('SIGINT'));

  try {
    await runWorker();
  } finally {
    // And whatever else ends the worker ends the child too.
    panels.stop();
  }
}

async function runWorker(): Promise<void> {
  const address = process.env.KONTRA_ADDRESS ?? 'localhost:7233';
  const namespace = process.env.KONTRA_NAMESPACE ?? 'default';

  // An explicit connection, not the default. `connection: undefined` silently means
  // localhost:7233 — which is right on a laptop and always wrong in a container, where the
  // failure reads as an unrelated tonic transport error against ::1.
  const connection = await NativeConnection.connect({ address });

  const worker = await Worker.create({
    // A BUNDLE, not a single file: `stackWorkflow` and `tmuxSessionWorkflow` share this queue
    // because both need the cloud credential or the fleet key, and neither may run anywhere else.
    workflowsPath: require.resolve('./workflows/infra'),
    activities: { ...infraActivities, ...panelActivities },
    taskQueue: INFRA_QUEUE,
    namespace,
    connection,
    // One provision at a time per process. The engine forks a `pulumi` CLI child plus a child
    // per provider (~164 MB per concurrent update measured), and this container is memory
    // capped; queueing is preferable to an OOM mid-apply, which is how locks get wedged.
    //
    // The session converge shares this limit. That is deliberate: it is one `ssh` and a few
    // hundred milliseconds, and a converge queued behind a provision is a wall that fills a
    // moment later — whereas a converge racing an `up` on the same Machine is a Machine being
    // rebuilt under a session that was just created on it.
    maxConcurrentActivityTaskExecutions: 1,
  });

  // eslint-disable-next-line no-console
  console.log(
    `[infra] queue=${INFRA_QUEUE} temporal=${address} ns=${namespace} backend=${backendUrl()}`
  );

  // ARM THE RETENTION SWEEP (ADR 0029 §5) — after the Worker exists, before it polls.
  //
  // HERE because this is the process that HOSTS `sweepDatasetsWorkflow` (`workflows/infra.ts`): the
  // Schedule targets INFRA_QUEUE, so arming it anywhere else could register an hourly firing onto a
  // queue with no poller — a workflow per hour that never starts, and `SKIP` skipping every firing
  // behind the first one, which looks armed and sweeps nothing.
  //
  // It cannot fail this boot: see `armRetentionSchedule`. And it deletes nothing by default — a
  // firing resolves its mode on the worker holding the lake, where unset means dry run.
  await armRetention(address, namespace);

  await worker.run();
}

/**
 * Create the retention Schedule, on a connection of its own that is closed again immediately.
 *
 * A SECOND CONNECTION, briefly, and not the worker's: `NativeConnection` is the Rust core's, and the
 * Schedule API lives on `@temporalio/client`'s gRPC-JS `Connection` — there is no way to share one.
 * It is closed as soon as the Schedule is registered, so the process holds exactly what it held
 * before: the worker's connection and nothing else.
 *
 * The repo's `dataConverter`, so the Schedule's stored workflow arguments are encoded by the same
 * codec the worker will decode them with.
 */
async function armRetention(address: string, namespace: string): Promise<void> {
  let connection: Connection | undefined;
  try {
    connection = await Connection.connect({ address });
    const client = new Client({ connection, namespace, dataConverter });
    await armRetentionSchedule(client.schedule, {
      taskQueue: INFRA_QUEUE,
      // eslint-disable-next-line no-console
      log: (line) => console.log(`[infra] ${line}`),
    });
  } catch (err) {
    // Reaching Temporal at all is the worker's problem, not this one's — it is about to try the same
    // address and will fail loudly if it cannot. Housekeeping never takes the process down.
    // eslint-disable-next-line no-console
    console.log(`[infra] could not arm the retention schedule: ${err instanceof Error ? err.message : String(err)}`);
  } finally {
    await connection?.close().catch(() => undefined);
  }
}

if (require.main === module) {
  main().catch((err) => {
    // eslint-disable-next-line no-console
    console.error('[infra] fatal:', err);
    process.exit(1);
  });
}
