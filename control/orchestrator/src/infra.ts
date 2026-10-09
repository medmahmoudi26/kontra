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
 * IT USED TO CARRY TWO MORE JOBS AND BOTH WERE THE MONITOR'S. It ran the session converge
 * (`tmuxSessionWorkflow`) because session existence on a Machine was Fleet authority, and it forked
 * and supervised the Dashboard streamer so that ADR 0019's process-global Pulumi hazard could not
 * blame panel code for a failed `fleet deploy`. The Monitor is gone — a read-only terminal over a
 * tmux socket was machine-local, unsearchable and not the record — so both jobs went with it, and
 * with them the shared tmux socket that let this container and the API's reach each other's
 * processes.
 *
 *   node dist/src/infra.js
 */

import { Client, Connection } from '@temporalio/client';
import { NativeConnection, Worker } from '@temporalio/worker';
import * as infraActivities from './activities/infra';
import * as serveDevActivities from './activities/serveDev';
import * as buildActorActivities from './activities/buildActor';
import { dataConverter } from './codec/dataConverter';
import { adoptLegacyCloudToken } from './infra/credential';
import { assertBackend, backendUrl } from './infra/workspace';
import { infraQueue } from './queues';
import { configureServeDev } from './activities/serveDev';
import { configureBuildActor } from './activities/buildActor';
import { SourceStore } from './sourceStore';
import { kontraBin, serveEnv } from './workflowControl';
import { armRetentionSchedule } from './retention';
import { temporalConnectOptions } from './temporalTls';
import { identityFor } from './workerIdentity';
import { runPerNamespace } from './namespacePool';
import { bindNamespace } from './workspaces';
import { LEGACY_NAMESPACE, clientFor } from './temporalClient';

/**
 * The queue this process serves. It is `queues.ts`'s now — a queue NAME is a routing fact and not
 * a provisioner's, and `infraRoutes.ts` had to load this whole module, Pulumi included, to read a
 * string. Resolved once at load, exactly as the `const` it replaces was.
 */
const INFRA_QUEUE = infraQueue();

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

  /*
   * WIRE serve-dev's ONE SEAM — how a source id becomes a folder.
   *
   * HERE AND NOT INSIDE THE ACTIVITY, because this is the boundary that makes the id-only rule a
   * property rather than a promise. The activity is handed a `resolve` that can only answer from
   * THIS control plane's registration store; there is no spelling of `ServeDevInput` that reaches a
   * path the operator never registered, and no way for a caller to substitute a different resolver.
   *
   * `kind` IS CHECKED AGAINST THE STORE, not taken from the request. A caller naming a workflow id
   * with `kind: 'actor'` would otherwise pick the actor verb for a workflow folder — not a privilege
   * escape, but a confusing failure, and the store already knows which it is.
   */
  const sources = new SourceStore();
  configureServeDev({
    resolve: async (sourceId) => {
      for (const kind of ['workflow', 'actor'] as const) {
        const got = sources.get(kind, sourceId);
        if (got !== undefined) return { name: got.name, path: got.path, kind: got.kind };
      }
      return null;
    },
    kontraBin,
    serveEnv,
  });

  // The BUILD half, wired off the same store for the same reason. `resolve` looks only at the
  // actor kind here — `buildActor` refuses anything else anyway, and asking the workflow store
  // first would let a workflow folder's id resolve to a row the activity then rejects, which
  // reports "not an actor" about a lookup that should simply have missed.
  configureBuildActor({
    resolve: async (sourceId) => {
      const got = sources.get('actor', sourceId);
      return got === undefined
        ? null
        : { name: got.name, path: got.path, kind: got.kind, version: got.version };
    },
    kontraBin,
    serveEnv,
  });

  // NO SIGNAL HANDLERS, WHICH IS WHERE THIS STARTED. They existed for exactly one reason — a
  // forked streamer that Node's default SIGTERM would have orphaned, still holding port 8090 and
  // the read-only fleet key. There is no child now, so the default is right again, and this process
  // ends the way every other worker here does. Making a `pulumi up` shut down gracefully is a
  // separate change with consequences for the lock (see activities/infra.ts).
  await runWorker();
}

async function runWorker(): Promise<void> {
  const address = process.env.KONTRA_ADDRESS ?? 'localhost:7233';

  const connection = await NativeConnection.connect(temporalConnectOptions({ address }));

  console.log(
    `[infra] queue=${INFRA_QUEUE} temporal=${address} ` +
      `backend=${backendUrl()} as=${identityFor(INFRA_QUEUE)}`
  );

  // THE RETENTION SCHEDULE STAYS IN THE LEGACY NAMESPACE, ONCE. It sweeps the Dataset lake, which
  // still follows the install's current workspace rather than a run's namespace, so arming it in
  // every namespace would sweep the same lake once per workspace.
  await armRetention(address, LEGACY_NAMESPACE);

  // PER WORKSPACE NAMESPACE (ADR 0051): a run's Fleet is a CHILD workflow on this queue, and a child
  // runs in its parent's namespace. See `namespacePool.ts`.
  await runPerNamespace({
    label: 'infra',
    ensure: async (namespace) => {
      await clientFor(namespace);
    },
    make: (namespace) =>
      Worker.create({
        workflowsPath: require.resolve('./workflows/infra'),
        activities: bindNamespace(namespace, { ...infraActivities, ...serveDevActivities, ...buildActorActivities }),
        taskQueue: INFRA_QUEUE,
        namespace,
        connection,
        maxConcurrentActivityTaskExecutions: 1,
        maxHeartbeatThrottleInterval: '2s',
        defaultHeartbeatThrottleInterval: '2s',
        identity: identityFor(INFRA_QUEUE),
      }),
  });
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
    connection = await Connection.connect(temporalConnectOptions({ address }));
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
