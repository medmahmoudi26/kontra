/**
 * The MATERIALIZER worker — a separate process, on a separate host, with a separate
 * memory budget (ADR 0017, plan §1).
 *
 * It hosts exactly one thing: the activities that decode a node's output into typed
 * DuckLake rows. It hosts NO workflows, so nothing schedules work here and nothing here can
 * schedule work elsewhere. That is the whole design:
 * embedded DuckDB is the largest memory consumer in this system, and the controller is a
 * 4 GB host that also runs Temporal, SeaweedFS and Postgres. Production materialization does
 * not run there.
 *
 * SINCE ADR 0031 §1 IT IS ALSO A ROLE, not only a process. `main.ts` can run {@link
 * runMaterializer} beside the API and the infra role in one PID, and the appliance does. What that
 * costs is stated where the cost lands: the two QUEUES below are unchanged, and so are the DuckDB
 * memory limit and the slot count — but boundary 1, the cgroup, is a property of a container and
 * does not survive the merge. One process means one heap. On a laptop that is the right trade; on
 * the 4 GB controller it is not, which is why compose still runs this role with `mem_limit`
 * around it.
 *
 * The boundaries, in order of how hard they bite:
 *
 *   1. THE CGROUP is the hard RSS boundary (`mem_limit` on the container). DuckDB's own
 *      `memory_limit` bounds its buffer manager, not the process — a setting alone has
 *      never stopped an OOM kill.
 *   2. DuckDB `memory_limit` (default 256MB) sits UNDER that, so DuckDB spills to disk
 *      before the kernel reaches for the OOM killer.
 *   3. The temp directory is quota-limited and `max_temp_directory_size` caps the spill,
 *      so a runaway sort fills its own volume instead of the host's disk.
 *   4. ONE activity slot by default. Concurrency here multiplies peak RSS directly; raise
 *      it only against measured headroom, which is what the canary gate exists to produce.
 *
 * Env: KONTRA_ADDRESS, KONTRA_NAMESPACE, KONTRA_MATERIALIZER_QUEUE,
 * KONTRA_MATERIALIZER_SLOTS, KONTRA_DUCKDB_MEMORY_LIMIT, KONTRA_DUCKDB_THREADS,
 * KONTRA_DUCKDB_TEMP_DIR, KONTRA_DUCKDB_MAX_TEMP_SIZE, KONTRA_MATERIALIZE_BATCH,
 * KONTRA_MATERIALIZATION_DB, plus the KONTRA_S3_* / KONTRA_DUCKLAKE_* the writer reads.
 */

import { NativeConnection, Worker } from '@temporalio/worker';
import { OpenTelemetryActivityInboundInterceptor } from '@temporalio/interceptors-opentelemetry';

import { createDatasetActivities } from './activities/datasets';
import * as fleetActivities from './activities/fleet';
import * as leaseActivities from './activities/lease';
import { createMaterializerActivities } from './activities/materialize';
import { createRetentionActivities } from './activities/retention';
import { ObjectStore } from './codec/objectStore';
import { materializationStore } from './data/materializationStore';
import { startTracing, tracingEnabled } from './otel';
import { datasetQueue, materializerQueue } from './queues';
import { temporalConnectOptions } from './temporalTls';

/** One decode at a time by default — concurrency here multiplies peak RSS directly. */
const DEFAULT_SLOTS = 1;

function slots(): number {
  const v = process.env.KONTRA_MATERIALIZER_SLOTS;
  const n = v ? Number.parseInt(v, 10) : NaN;
  return Number.isFinite(n) && n > 0 ? n : DEFAULT_SLOTS;
}

/**
 * Pager slots. Higher than the decode default because a page read is short and bounded by its
 * LIMIT, and every concurrent caller's loop needs one — a single slot would serialize every
 * workflow in the fleet behind one another's paging.
 */
function pagerSlots(): number {
  const v = process.env.KONTRA_DATASET_SLOTS;
  const n = v ? Number.parseInt(v, 10) : NaN;
  return Number.isFinite(n) && n > 0 ? n : 4;
}

export async function runMaterializer(): Promise<void> {
  startTracing();
  const status = materializationStore();
  // Fail at BOOT on a misconfigured status store rather than at the first node of a
  // run. A materializer that cannot record status is worse than one that is down:
  // it would decode successfully and leave every run stuck in `finalizing`.
  await status.ensureSchema();

  const connection = await NativeConnection.connect(temporalConnectOptions());
  try {
    const worker = await Worker.create({
      connection,
      namespace: process.env.KONTRA_NAMESPACE ?? 'default',
      taskQueue: materializerQueue(),
      // Activities only, and deliberately no `workflowsPath`: this host is sized for one
      // bounded decode at a time, so it must not be able to pick up a workflow — its own
      // or anyone's — even by accident.
      activities: createMaterializerActivities({ store: new ObjectStore(), status }),
      maxConcurrentActivityTaskExecutions: slots(),
      // The materializer reads payloads from the object store by ref; it is never handed
      // one through Temporal, so it needs no claim-check data converter.
      ...(tracingEnabled
        ? {
            interceptors: {
              activity: [(ctx) => ({ inbound: new OpenTelemetryActivityInboundInterceptor(ctx) })],
            },
          }
        : {}),
    });

    // The dataset PAGER shares this process — it wants the same DuckLake attach, the same
    // ObjectStore and the same "no claim-check converter" posture — but its OWN queue and its
    // own slot count. A caller's `batches()` loop blocks on one page read at a time, and
    // queueing that behind a 40-minute decode would present as a hung workflow.
    const pager = await Worker.create({
      connection,
      namespace: process.env.KONTRA_NAMESPACE ?? 'default',
      taskQueue: datasetQueue(),
      // Plus the caller SDK's two fleet reads (`activities/fleet.ts` says why they are here and
      // not on the infra queue: that worker is deliberately serialised to one Pulumi update at a
      // time, and a readiness poll behind a sixty-minute provision reads as a fleet that never
      // came up).
      //
      // And the retention SWEEP (ADR 0029 §5): it reads the lake and the record/**Lease** workflow/summary
      // stores, which this process already holds, and it heartbeats, so it belongs on the pager
      // queue beside the reads rather than behind a decode. `sweepDatasetsWorkflow` proxies it
      // here from wherever the controller hosts the workflow.
      activities: {
        ...createDatasetActivities({ store: new ObjectStore() }),
        ...createRetentionActivities({ store: new ObjectStore() }),
        ...fleetActivities,
        // And the three **Lease** calls (ADR 0037), here for the same reason and one more: a hold
        // is what a `fleet.up()` does FIRST, so a hold queued behind a sixty-minute converge would
        // block every Run at the top of its scope. `activities/lease.ts` says the rest.
        ...leaseActivities,
      },
      maxConcurrentActivityTaskExecutions: pagerSlots(),
      ...(tracingEnabled
        ? {
            interceptors: {
              activity: [(ctx) => ({ inbound: new OpenTelemetryActivityInboundInterceptor(ctx) })],
            },
          }
        : {}),
    });

    await Promise.all([worker.run(), pager.run()]);
  } finally {
    await connection.close();
  }
}

if (require.main === module) {
  runMaterializer().catch((err) => {
    // eslint-disable-next-line no-console
    console.error(err);
    process.exit(1);
  });
}
