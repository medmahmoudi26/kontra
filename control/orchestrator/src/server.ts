/**
 * The orchestrator HTTP API (Fastify): the shared state, the module wiring, and the SPA.
 *
 * THERE ARE NO ROUTES IN THIS FILE ANY MORE. `buildServer` used to be one closure of about 1,900
 * lines registering roughly seventy routes in one scope; it now builds what genuinely SPANS
 * surfaces and hands each surface to its own module, the way `registerSecretRoutes`,
 * `registerSlotRoutes` and `infraRoutes.ts` already did. Every module states which surface it
 * serves and what it needs in its own header, and those needs are its parameters — so what a route
 * group can reach is the signature of one function rather than everything a 1,900-line closure
 * happened to have in scope.
 *
 * THE SURFACES, and where each lives:
 *
 *   routes/pulse.ts       liveness: this process (`/api/health`) and the cluster (`/api/pulse`)
 *   routes/catalog.ts     actor registration and the catalog the design surfaces are typed against
 *   routes/scratch.ts     the two opaque-document surfaces: Scratches and `/api/graphs`
 *   routes/runs.ts        a Run's lifecycle: list, start, read, stamp, stop, progress, heartbeats
 *   routes/history.ts     the run's Temporal event log, and the console's only drill-down
 *   routes/hitl.ts        the asks a parked Run is waiting on, and the one verb that answers them
 *   routes/fleet.ts       fleet operation history, and the phase a Fleet child reports
 *   routes/workflows.ts   a caller's own code: on disk, registered, served, paused
 *   routes/pollers.ts     who is POLLING a task queue — the third state a workflow can be in
 *   routes/sources.ts     registered folders: list, register, forget, read, serve, generate
 *   routes/probe.ts       ADR 0033: start one Method call, and read what it answered
 *   routes/datasets.ts    the lake browser: listing, preview, provenance, temp delete, the record
 *   routes/retention.ts   the dry-run half of the untagged-TTL sweep (ADR 0029 §3, §5)
 *   routes/rowStream.ts   the live row tail — the one streaming lifecycle on this API
 *   routes/query.ts       the workbench: operator-typed SQL, its schema, its download
 *   routes/explore.ts     the exact-run manifest and the only presigned URLs this API issues
 *   routes/summaries.ts   the bounded scalars the "is it moving?" plane reads
 *   routes/state.ts       raw state inspection, token-gated (ADR 0019's sibling)
 *   secrets/routes.ts     named, versioned, write-only secrets (issue 19; ADR 0034 §4)
 *   secrets/slotRoutes.ts slot declarations, bindings and the resolution ledger (issue 20)
 *   infraRoutes.ts        the infra control plane (ADR 0019), routes and the two functions behind
 *
 * IT STARTS WORKFLOWS; IT DOES NOT EXECUTE BATCHES. That is the invariant, and it is narrower than
 * the one this header used to state ("IT DOES NOT START RUNS", with `POST /api/runs` named as a
 * deliberate 404). What ADR 0023 §12 removed was the route that took a GRAPH and started the
 * interpreter, along with the server-minted run id, the durable pre-start record and the
 * idempotency replay that existed because the SERVER owned execution. The verb came back on
 * 2026-08-15 in its post-§12 meaning: `POST /api/runs` starts ONE execution of a caller's own
 * workflow, on a queue derived from that caller's folder, refusing outright when no worker polls
 * it — and ADR 0029 §2 depends on that route by name. `POST /api/sources/actor/:id/probe` (ADR
 * 0033) is the second: one execution of a kontra-owned workflow that makes exactly ONE Method call.
 * Neither one runs a Batch here; both hand it to a worker this process does not host.
 *
 * In production it also serves the built SPA (web/dist) with an SPA fallback, so the
 * whole tool is one origin/container. That half stays HERE rather than in `routes/`: it is not an
 * API surface, it is what this process is when it is not answering `/api/`. Temporal is touched
 * lazily — only the run endpoints — so the server boots and serves catalog/designs with no cluster.
 *
 * Env: KONTRA_ORCHESTRATOR_PORT (default 8088 — 8080 is SeaweedFS), KONTRA_ORCHESTRATOR_DB
 * (default ./backend.db, a SQLite file; `:memory:` for none), KONTRA_ORCHESTRATOR_WEB (default ../web/dist),
 * plus KONTRA_ADDRESS / KONTRA_NAMESPACE / KONTRA_S3_*.
 */

import path from 'node:path';
import { existsSync, readFileSync } from 'node:fs';
import Fastify, { type FastifyInstance } from 'fastify';
import type { FastifyReply } from 'fastify';

import fastifyStatic from '@fastify/static';
import fastifyCompress from '@fastify/compress';
import { Repo } from './db/repo';
import { SourceStore } from './sourceStore';
import { RunLifecycle } from './runs';
import type { PulseDeps } from './pulse';
import {
  describeRun as describeRunById,
  describeRunHeartbeats,
  fetchRunClose,
  fetchRunIO,
  getClient,
  listRuns,
  queryRunReport,
  type RunDescription,
} from './temporalClient';
import { HistoryArchive, startHistoryArchiver } from './historyArchive';
import { startInuseReconciler } from './images/inuseReconciler';
import { installApiGate } from './auth/apiGate';
import { contextForRun, startReportRenderer, sweepFinishedRuns } from './report/sweep';
import { reportStore, type ReportStore } from './report/store';
import { render as renderReportInHost } from './report/renderHost';
import { admitReport, registerReportRoutes } from './routes/report';
import { registerReportLiveRoute } from './routes/reportLive';
import { LiveHub, type RenderOnce } from './report/live';
import { IN_FLIGHT, inFlightSummary, progressFromHeartbeats, type InFlightRows } from './report/liveProducers';
import type { RowTailHub } from './rowTail';

import { registerInfraRoutes } from './infraRoutes';
import { registerSecretRoutes } from './secrets/routes';
import { registerSlotRoutes } from './secrets/slotRoutes';
import { slotStore, type SlotStore } from './secrets/slotStore';
import { secretStore, type SecretStore } from './secrets/store';
import { describeExposure, setRegisteredFolders } from './workflowControl';
import { describeQueue, temporalQueueDescriber, type QueueDescriber } from './pollers';
import { ObjectStore } from './codec/objectStore';
import { DatasetRecordStore, datasetRecordStore } from './data/datasetRecords';
import { RunWorkflowStore, runWorkflowStore } from './data/runWorkflows';
import type { LakeConfig } from './data/parquet';
import { MaterializationStore, materializationStore } from './data/materializationStore';
import { SummaryStore, summaryStore } from './data/summaries';
import { registerCatalogRoutes } from './routes/catalog';
import { registerImageRoutes } from './routes/images';
import { registerDatasetRoutes } from './routes/datasets';
import { errMessage } from './routes/errors';
import { registerExploreRoutes } from './routes/explore';
import { registerFleetRoutes } from './routes/fleet';
import { registerHistoryRoutes } from './routes/history';
import { registerHitlRoutes } from './routes/hitl';
import { registerLogsRoutes } from './routes/logs';
import { registerLogsCoverageRoutes } from './routes/logsCoverage';
import { registerAuditRoutes } from './routes/audit';
import { registerLoginRoutes } from './routes/login';
import { registerPollerRoutes } from './routes/pollers';
import { registerProbeRoutes } from './routes/probe';
import { registerPulseRoutes } from './routes/pulse';
import { registerQueryRoutes } from './routes/query';
import { registerRetentionRoutes } from './routes/retention';
import { registerRowStreamRoute, type RowStreamCaps } from './routes/rowStream';
import { registerRunRoutes } from './routes/runs';
import { registerScratchRoutes } from './routes/scratch';
import { registerSourceRoutes } from './routes/sources';
import { registerWorkspaceRoutes } from './routes/workspaces';
import { registerStateRoutes } from './routes/state';
import { registerUploadRoutes } from './routes/uploads';
import { registerStuckRoutes } from './routes/stuck';
import { registerRunStream } from './routes/runStream';
import { registerSummaryRoutes } from './routes/summaries';
import { registerWorkflowRoutes } from './routes/workflows';

/** How often a watched open run's live report re-renders when nothing else asked it to. Matches the
 *  row tail's poll: a faster tick would render a context that cannot have changed. */
const LIVE_TICK_MS = 2_000;

/**
 * The console's surfaces, by first path segment — the closed set the SPA fallback serves.
 *
 * MUST TRACK the console's own `state/surfaces.ts` (`SPA_SEGMENTS`), which is the authority. It is
 * copied rather than imported because the console is a separate package this one does not depend
 * on; `spaFallback.test.ts` pins the two together so the copy cannot drift silently.
 *
 * Matching the FIRST segment is the point: everything after it is an id whose bytes are not ours to
 * predict — a Terminal id carries a colon and, on tmux, a dot, and a dataset name may carry one too.
 *
 * `runs`, `scratch` AND `monitor` ARE RETIRED SURFACES AND ARE STILL SERVED. A run is reached
 * through the workflow that produced it, Scratch became a workflow's own tab, and the Monitor was
 * deleted outright — but `/runs/<id>` is in somebody's notes and still names a run, and `/monitor`
 * is in somebody's tab. The console REDIRECTS all three. A redirect is code, and code has to load:
 * drop any of these segments here and a cold load of `/runs/sweep-v1.2` 404s on the dot before the
 * shell that would forward it ever runs.
 *
 * ── ONE SET AGAIN (ADR 0048 §7) ─────────────────────────────────────────────────────────────────
 *
 * This was two sets for the length of the migration — `SPA_SURFACES` for React, `SVELTE_SURFACES`
 * for the bundle replacing it — and moving a surface between consoles was moving a string between
 * them. Every surface has moved and React is deleted, so there is one bundle and one list. The
 * disjointness check that guarded the pair is gone with the pair; what replaces it is that there is
 * no second document to serve by mistake.
 */
export const SPA_SURFACES: ReadonlySet<string> = new Set([
  'catalog',
  'workflows',
  // A **Report** is addressed in its own right (ADR 0055): `/reports` is every report this control
  // plane has rendered and `/reports/<runId>` is one. It is here as well as in the console's nav
  // because a COLD LOAD of either address reaches this server first, and a surface the nav offers
  // that this set does not name is a link that 404s on a reload — invisible in-app, where navigation
  // never leaves the document.
  'reports',
  'actors',
  'datasets',
  'logs',
  'secrets',
  'settings',
  // retired, still addressable — see above
  'runs',
  'scratch',
  'monitor',
]);

/**
 * Extensionless routes the console owns that are not Surfaces — today, the IDE embed.
 *
 * `/dev?actor=&method=` is what the VS Code extension opens in a webview: the console's own Method
 * form with the chrome removed. It was served by the fallback's "no file extension" clause and is
 * named here instead, because a route that works by heuristic works until the heuristic changes.
 */
export const SVELTE_ROUTES: ReadonlySet<string> = new Set<string>(['dev']);


export interface ServerOptions {
  repo?: Repo;
  /** Object store the dataset browser reads (default: env-configured). Injectable for tests. */
  store?: ObjectStore;
  /**
   * The three bounds on `/api/datasets/rows/stream`, lowered so a test can prove each one.
   *
   * PRODUCTION NEVER PASSES THIS. The defaults are the real caps; overriding them exists so a test
   * reaches "too many" with two connections rather than sixty-five, which is the difference between
   * a suite that pins the behaviour and one that pins nothing because it is too slow to run. The
   * three are documented where they are enforced — `routes/rowStream.ts`.
   */
  rowStreamCaps?: RowStreamCaps;
  /** Materialization status authority (ADR 0017). Injectable for tests. */
  materialization?: MaterializationStore;
  /**
   * The Dataset record — the durable authority for tags and renames (ADR 0029 §1, §4). Injectable
   * for tests; keyed by `runId` and independent of Temporal, which is what lets a Dataset be tagged
   * after its Run has closed.
   */
  records?: DatasetRecordStore;
  /**
   * A **Run**'s caller-workflow identity, snapshotted at start (ADR 0029 §2, `data/runWorkflows.ts`)
   * — what the derived Dataset name renders. Injectable for tests; a Run with no row here falls back
   * to the producing Actor's identity, so a server built without one still names every Dataset.
   */
  runWorkflows?: RunWorkflowStore;
  /**
   * The EXECUTION authority — Temporal. Injectable so a test can assert a run's status without
   * a live cluster; `RunLifecycle.read` rethrows a Temporal error when nothing else can answer,
   * so a test that does not inject one is asserting against whatever cluster happens to be up.
   */
  describeRun?: (runId: string) => Promise<RunDescription | undefined>;
  /**
   * The two BOUNDED Temporal reads behind `GET /api/pulse` (`pulse.ts`).
   *
   * Injectable for the same reason `describeRun` is, and with one extra edge: the pulse is polled
   * by the chrome on EVERY surface, so a suite that did not inject it would have every server test
   * that happens to build a full app quietly dialling whatever cluster is up.
   */
  pulse?: PulseDeps;
  /**
   * Who is POLLING a task queue — the same seam `cli/workers.go` and `fleet.ready()` take.
   *
   * Injectable for the same reason `describeRun` is: without it a test of the pollers route is
   * asserting against whatever cluster happens to be up, and passes on a developer's machine while
   * timing out in CI.
   */
  queueDescriber?: QueueDescriber;
  /** Operational summary tables the dashboards read. Injectable for tests. */
  summaries?: SummaryStore;
  /**
   * The four report tables (ADR 0055). Injectable so a route test reaches a report without the
   * process-wide store, which on a fresh test process would open the real database file.
   */
  reports?: ReportStore;
  /** DuckLake overrides for the explore manifest (tests point this at a local lake). */
  lake?: Partial<LakeConfig>;
  /**
   * TESTS ONLY. Run the workbench engine WITHOUT its sandbox.
   *
   * The sandbox disables LocalFileSystem, and a test lake is a local directory — so a hardened
   * engine cannot read one, and a route test against it would only ever exercise queries that
   * the catalog can answer without opening a file. Defaults to hardened, and the sandbox itself
   * is asserted directly in data/queryEngine.test.ts; `serves nothing unsandboxed by default`
   * in queryRoutes.test.ts pins that a server built without this flag really is hardened.
   */
  unsafeQueryNoSandbox?: boolean;
  /**
   * The secret store (issue 19). Injectable so a test points it at a temp directory rather than
   * the operator's own `~/.kontra/secrets` — a suite that wrote into the real store would put test
   * credentials beside real ones and mint a key file nobody asked for.
   */
  secrets?: SecretStore;
  /**
   * The slot declarations and bindings (issue 20). Injectable for the same reason the store is:
   * a suite that wrote here would leave grants in the operator's own `~/.kontra/secrets`.
   */
  slots?: SlotStore;
  /** Directory of the built SPA to serve; skipped if it doesn't exist. */
  webRoot?: string;
  logger?: boolean;
}

/**
 * BUILD THE APP: the shared state, then one call per surface.
 *
 * WHAT LIVES HERE IS WHAT MORE THAN ONE MODULE READS, and nothing else. Every value below is
 * annotated with who shares it, because that is the only justification for it being in this scope
 * rather than inside the one module that uses it — a store only the workbench touches belongs to
 * the workbench, and a poller registry only the row tail owns belongs to the row tail (it does, in
 * `routes/rowStream.ts`, along with its own `onClose` teardown).
 *
 * IT ACQUIRES NO PORT, NO BACKGROUND LOOP AND NO TEMPORAL CONNECTION. A test or the CLI calls this;
 * `runApi` below is what boots. The lazily-built queue describer is the sharp edge of that rule and
 * is the reason it is a getter rather than a value.
 */
/** One route this server registers: the method and the path Fastify itself recorded. */
export interface RegisteredRoute {
  method: string;
  url: string;
}

declare module 'fastify' {
  interface FastifyInstance {
    /** Every route registered on this instance, collected by an `onRoute` hook. */
    kontraRoutes: RegisteredRoute[];
  }
}

export function buildServer(opts: ServerOptions = {}): FastifyInstance {
  // BEFORE ANYTHING IS REGISTERED. A segment claiming both bundles makes one console unreachable,
  // and the failure is a user finding the wrong page rather than a process that refused to start.
  const repo = opts.repo ?? new Repo(process.env.KONTRA_ORCHESTRATOR_DB ?? 'orchestrator.db');
  // The object store: SHARED by the dataset browser, the workbench, the explore manifest, the row
  // tail and the history archive. One store, so a count on one surface cannot disagree with a
  // count on another.
  const store = opts.store ?? new ObjectStore();
  // Where an operator's own code lives: the workspace, read from disk on every listing. SHARED by
  // the sources and probe surfaces, because a probe runs the FOLDER's Actor and version — never a
  // body's — and both must resolve a folder the same way.
  const sources = new SourceStore();
  /* A REGISTERED WORKFLOW FOLDER IS OPENABLE WHEREVER IT LIVES (issue #4). Registration accepts any
     absolute path and the Workflows list draws what it accepted, but `resolveWorkflowFile` was
     confined to `~/.kontra/workflows` alone — so a folder registered from a checkout listed, and
     then failed to open with "no such workflow in …". This is the wiring that closes it, and it is
     a FUNCTION rather than a snapshot because a folder registered a second ago has to be openable a
     second later.

     THE RESOLVER IS STILL THE BOUNDARY. This hands it names and paths; it confines inside each one
     and resolves symlinks exactly as before, and the default root still wins on a collision. */
  setRegisteredFolders(
    () =>
      new Map(
        sources
          .list('workflow')
          .filter((s) => s.absent !== true)
          .map((s) => [s.name, s.path] as const)
      )
  );
  // The materialization ledger (ADR 0017): SHARED by the dataset listing, the retention preview and
  // `RunLifecycle`, which is the second of a Run's two status authorities.
  const materialization = opts.materialization ?? materializationStore();
  // Tags and renames (ADR 0029 §1, §4): SHARED by the dataset record writes and the sweep that
  // reads a tag as the KEEP/COLLECT authority.
  const records = opts.records ?? datasetRecordStore();
  // The caller identity a derived Dataset name renders (ADR 0029 §2): SHARED by the run surface,
  // which stamps it, the dataset listing, which renders it, and the sweep, which reports it.
  const runWorkflows = opts.runWorkflows ?? runWorkflowStore();
  // Operational scalars: SHARED by the summaries surface and the sweep, which purges a collected
  // Run's row.
  const summaries = opts.summaries ?? summaryStore();
  // DuckLake overrides: SHARED by every module that opens the lake.
  const lake = opts.lake ?? {};
  // The reduced event logs of runs Temporal no longer has (ADR 0025). Built here so it reads the
  // SAME store the dataset browser does — an archive in a different bucket from the run's units is
  // an archive nobody finds. Reading it is free until the history route needs it. SHARED by the
  // history and hitl surfaces, which both fall back to it for the same reason.
  const archive = new HistoryArchive(store);
  // A Run's two-dimensional status (ADR 0017): SHARED by the run surface, the asks (which read the
  // live memo off the same describe) and the explore manifest (which carries the status).
  const runs = new RunLifecycle(materialization, opts.describeRun);
  /**
   * ONE describer for the process, built lazily. SHARED by `POST /api/runs`, which refuses a start
   * into a queue nobody polls, and by the pollers route, which reports the same read.
   *
   * It holds a Temporal connection. Building one per request would open a connection per poll of
   * every workflow on the page — and the page polls. It is passed as a GETTER so that registering
   * either module opens nothing.
   */
  let describer: QueueDescriber | null = opts.queueDescriber ?? null;
  const queueDescriber = (): QueueDescriber => (describer ??= temporalQueueDescriber());

  // 32 MiB body limit (Fastify defaults to 1 MiB): a saved design document carries the whole
  // editor canvas, and the default rejected the larger ones with a broken pipe.
  const app = Fastify({ bodyLimit: 33_554_432, logger: opts.logger ?? false });

  // DENY BY DEFAULT ON `/api/*`. Admission is a call each route makes for itself across 30 files,
  // so a route that forgets is OPEN and nothing says so. This refuses any `/api` route that has not
  // declared a posture in `auth/apiGate.ts`, which closes the hole for code nobody has written yet;
  // `auth/apiSurface.test.ts` holds that table to what the server actually does.
  installApiGate(app);

  // THE ROUTE INVENTORY, DERIVED RATHER THAN MAINTAINED.
  //
  // `onRoute` fires for every route as it is registered, so this list is the routes that EXIST —
  // not a second list somebody has to remember to update. `docs/openapi.json` is generated from it
  // and `openapi.test.ts` fails when the two disagree, which is what makes a route that quietly
  // moved a red build rather than a 404 nobody sees until a surface is empty.
  //
  // Registered FIRST, before any route is added, because the hook only sees what comes after it.
  const routes: RegisteredRoute[] = [];
  app.addHook('onRoute', (r) => {
    for (const method of Array.isArray(r.method) ? r.method : [r.method]) {
      // HEAD is Fastify's own doubling of every GET and describes nothing a caller chooses.
      if (method === 'HEAD' || method === 'OPTIONS') continue;
      routes.push({ method, url: r.url });
    }
  });
  app.decorate('kontraRoutes', routes);

  // Compress everything worth compressing. The SPA bundle and the JSON of a dataset preview are
  // both highly repetitive text; served raw they were several times larger than they need to be.
  // `global: true` covers the static assets and the API alike, and the plugin only encodes when
  // the client sent an Accept-Encoding that permits it.
  app.register(fastifyCompress, { global: true, threshold: 1024 });

  // --- the surfaces ---------------------------------------------------------------
  //
  // Order is presentation, not behaviour: Fastify's router prefers a static segment over a
  // parameterised one regardless of which module registered which first, and a duplicate path
  // throws at registration rather than shadowing silently. So these read top-down as the API does.
  registerPulseRoutes(app, opts.pulse);
  // THE LOGIN, FIRST, because everything else can be reached with what it hands out. It is where
  // the console's `Authorization` token comes from — a browser cannot read `~/.kontra/config.yaml`
  // the way the CLI does, and the alternative was a bearer baked into the bundle at build time.
  registerLoginRoutes(app);
  // The operator trail, beside the sign-in that is its first entry (`audit.ts`).
  registerAuditRoutes(app);
  registerCatalogRoutes(app, repo);
  // The image store (ADR 0061). It needs the catalog to say what is IN USE and the registry to say
  // what exists, and it is a separate module from `catalog.ts` because that one's four routes are
  // open by an argued decision and these eight are not.
  registerImageRoutes(app, { repo });
  registerScratchRoutes(app, repo);
  registerRunRoutes(app, { runs, runWorkflows, queueDescriber });
  registerHistoryRoutes(app, archive);
  registerHitlRoutes(app, { runs, archive });

  /* A RUN'S REPORT, ITS EXPORTS, ITS BYTES AND THE THREAD UNDER IT (ADR 0055).
     The context builder is the sweep's own, shared rather than reimplemented: a preview that assembled
     its context differently would be previewing a change to a document it is not showing. */
  const reports = opts.reports ?? reportStore();
  registerReportRoutes(app, {
    reports,
    identities: async (runIds) =>
      (await runWorkflowStore().list(runIds)).map((r) => ({ runId: r.runId, workflow: r.workflow })),
    render: (request) => renderReportInHost(request, { onNote: (note) => app.log.info(note) }),
    context: async (runId) => {
      const described = await describeRunById(runId);
      if (!described) return undefined;
      const io = await fetchRunIO(runId);
      if (!io) return undefined;
      const built = await contextForRun(
        {
          runId,
          status: described.status,
          startedAt: described.startedAt,
          closedAt: described.closedAt,
          type: described.type,
        },
        io,
        {
          store: reports,
          now: Date.now,
          close: (id) => fetchRunClose(id),
          identity: async (id) => {
            const found = await runWorkflowStore().get(id);
            return found ? { workflow: found.workflow, version: found.version } : undefined;
          },
        }
      );
      return built.context;
    },
  });
  /* LIVE REPORT MODE (ADR 0062). Registered beside the report surface because it is the same renderer
     and the same gate — `admitReport` is shared rather than copied, so the documented posture and the
     enforced one cannot drift. The render goes to the same worker thread the sweep uses, so a live
     tick costs the API's event loop no more than a stored render does. */
  /* WHAT THE LIVE RENDER READS THAT A STORED ONE NEVER DOES (ADR 0062 §2, and its correction).
     `liveRows` is the row tail's last look at each watched run's pushed records, kept between its
     polls; `rowTailHub` is the run page's own poller, assigned where its route registers below, so a
     live report watches the same rows through the same caps rather than a second poller. */
  const liveRows = new Map<string, InFlightRows>();
  let rowTailHub: RowTailHub | undefined;
  const renderLive: RenderOnce = async (key) => {
    const described = await describeRunById(key.runId);
    if (!described) return { error: `run ${key.runId} is no longer readable` };
    const io = (await fetchRunIO(key.runId)) ?? {};
    // ONLY FOR AN OPEN RUN. A closed run's live render is the bridge to its stored version, and
    // must say what the stored one will: no progress, no in-flight rows, `result` from its return.
    let live: Parameters<typeof contextForRun>[3];
    if (described.status === 'running') {
      const [beats, partial] = await Promise.all([
        describeRunHeartbeats(key.runId).catch(() => ({})),
        queryRunReport(key.runId),
      ]);
      const progress = progressFromHeartbeats(beats);
      const seen = liveRows.get(key.runId);
      live = {
        ...(progress ? { progress } : {}),
        ...(seen && seen.rows > 0 ? { datasets: { [IN_FLIGHT]: inFlightSummary(seen) } } : {}),
        ...(partial !== undefined ? { partial } : {}),
      };
    }
    const built = await contextForRun(
      {
        runId: key.runId,
        status: described.status,
        startedAt: described.startedAt,
        closedAt: described.closedAt,
        type: described.type,
      },
      io,
      {
        store: reports,
        now: Date.now,
        close: (id) => fetchRunClose(id),
        identity: async (id) => {
          const found = await runWorkflowStore().get(id);
          return found ? { workflow: found.workflow, version: found.version } : undefined;
        },
      },
      live
    );
    const result = await renderReportInHost(
      { template: built.template, context: built.context as unknown as Record<string, unknown> },
      { onNote: (note) => app.log.info(note) }
    );
    if (!result.ok) return { error: result.error };
    return { snapshot: result.snapshot };
  };
  const liveHub = new LiveHub({
    renderOnce: renderLive,
    onError: (err) => app.log.warn(`report live: ${errMessage(err)}`),
    tickMs: LIVE_TICK_MS,
    // THE ROW TAIL, PER WATCHED RUN, for as long as somebody watches. A new snapshot is new context,
    // so it re-renders through the session's debounce; the subscription ends with the session.
    onOpen: (key, session) => {
      if (!rowTailHub) return undefined;
      let cancel: () => void;
      try {
        cancel = rowTailHub.subscribe(key.runId, (e) => {
          if (e.kind !== 'snapshot') return;
          const before = liveRows.get(key.runId);
          liveRows.set(key.runId, {
            rows: e.snapshot.rows,
            lastChunkAt: e.snapshot.lastChunkAt,
            // A snapshot carries a window only when freshly minted; keep the last one otherwise.
            recent: e.snapshot.window ? e.snapshot.window.recent.map((c) => c.row) : (before?.recent ?? []),
          });
          session.touch();
        });
      } catch (err) {
        // At the row tail's caps. The report still updates on its other triggers.
        app.log.warn(`report live: run ${key.runId}: no row tail: ${errMessage(err)}`);
        return undefined;
      }
      return () => {
        cancel();
        liveRows.delete(key.runId);
      };
    },
  });
  registerReportLiveRoute(app, {
    hub: liveHub,
    admit: admitReport,
    onError: (err, runId) =>
      app.log.warn(`report live: ${runId ? `run ${runId}: ` : ''}${errMessage(err)}`),
    resolveRun: async (runId) => {
      const described = await describeRunById(runId);
      if (!described) return undefined;
      return {
        runStartedAt: described.startedAt,
        status: described.status,
        closedAt: described.closedAt,
      };
    },
    renderOnce: renderLive,
    /* THE TERMINAL EVENT, NOT A TIMER (ADR 0062). `handle.result()` is a long poll on history under
       the hood, so this resolves within seconds of the run closing rather than within the
       reconciliation pass's minute. It REJECTS for a failed, cancelled, terminated or timed-out run,
       and the rejection is the signal — such a run still gets a report, saying what happened.
       PARKED PER WATCHED RUN, not per open run, which is what bounds it: at most one poll per live
       session, and sessions are already capped. A poll per open run would risk Temporal's
       per-namespace long-poll limiter, whose RESOURCE_EXHAUSTED the SDK's default retry interceptor
       would then retry on the shared channel that `/api/runs` uses.
       The STORED version is minted by the ordinary sweep path over this one run, so the render that
       is persisted is built from the FINAL context and carries the key a later convergence pass will
       compute — which is what stops that pass from inserting a second version beside it. */
    watchTerminal: async (key) => {
      const client = await getClient();
      try {
        await client.workflow.getHandle(key.runId).result();
      } catch {
        // Terminal and not a completion. Still reportable, and the report says which.
      }
      const described = await describeRunById(key.runId);
      if (!described || described.closedAt <= 0) return undefined;
      await sweepFinishedRuns({
        store: reports,
        list: async () => [
          {
            runId: key.runId,
            status: described.status,
            startedAt: described.startedAt,
            closedAt: described.closedAt,
            type: described.type,
          },
        ],
        io: (id) => fetchRunIO(id),
        close: (id) => fetchRunClose(id),
        identity: async (id) => {
          const found = await runWorkflowStore().get(id);
          return found ? { workflow: found.workflow, version: found.version } : undefined;
        },
        onError: (err, runId) =>
          app.log.warn(`report live finalize: ${runId ? `run ${runId}: ` : ''}${errMessage(err)}`),
      });
      const latest = await reports.version(key.runId);
      return latest ? { version: latest.version } : undefined;
    },
  });
  registerLogsRoutes(app);
  // Which Workers are running and NOT logging — the check every silent shipper failure needed.
  registerLogsCoverageRoutes(app, queueDescriber, repo);
  registerFleetRoutes(app);
  registerWorkflowRoutes(app, repo);
  registerPollerRoutes(app, queueDescriber, repo);
  registerSourceRoutes(app, sources);
  registerWorkspaceRoutes(app);
  registerProbeRoutes(app, sources);
  registerDatasetRoutes(app, { store, lake, materialization, records, runWorkflows });
  registerRetentionRoutes(app, { store, lake, materialization, records, runWorkflows, summaries });
  rowTailHub = registerRowStreamRoute(app, store, opts.rowStreamCaps ?? {});
  // The workbench sandbox. On unless a test explicitly asks otherwise — see ServerOptions.
  registerQueryRoutes(app, { store, lake, harden: !opts.unsafeQueryNoSandbox });
  registerExploreRoutes(app, { store, lake, runs });
  registerSummaryRoutes(app, summaries);
  registerStateRoutes(app);
  // The bytes behind a `File` or `Folder` field on a Method or workflow form. Content-addressed
  // into the SAME CAS the claim-check codec writes and `kontra.fetch_blob` reads, so an upload is
  // dereferenceable by every actor in every language with nothing new to configure.
  registerUploadRoutes(app, store);
  /* WHAT WILL NEVER MOVE (issue F7). An audit found nine open executions wedged on a
     `WorkflowTaskScheduled` nobody polls — up to 38 days old, every one an INTERNAL workflow type,
     and therefore invisible to every surface kontra has. This reports them; `kontra doctor` prints
     it. It deliberately does not reap: what to do with a two-week-old Warden is an operator's call,
     not a health check's. */
  registerStuckRoutes(app, { describeQueue: (q) => describeQueue(queueDescriber(), q) });
  /* THE LIVE RUN STREAM, WHICH WAS WRITTEN AND TESTED AND NEVER CALLED.
   *
   * `runStream.ts` and its suite have been in the tree since the slice that added them; nothing
   * invoked `registerRunStream`, so `/api/runs/:id/stream` 404'd while eleven tests passed over the
   * loop behind it. That is the same shape as a surface added to a union and left out of the array
   * that orders it — a finished thing with no way in — and it is why `registeredWorkflows.test.ts`
   * exists for the other one.
   *
   * ONE SOURCE, WHICH IS THE ROUTE'S OWN INVARIANT. `read` is `runs.read` — the function
   * `GET /api/runs/:runId` returns — so the stream cannot become a fourth reading of a Run that
   * disagrees with the three kontra already keeps deliberately distinct. `runStream.test.ts` pins
   * the bytes; this is what makes the production path use the same function.
   *
   * TERMINAL IS `closedAt > 0`, the same test `isArchivable` applies: a run Temporal still calls
   * `running` through a continue-as-new chain has not closed, and ending the stream on it would cut
   * a client off mid-run. */
  registerRunStream(app, {
    read: async (runId) => {
      const view = await runs.read(runId);
      if (!view) return { ok: false, body: { error: 'run not found' } };
      return { ok: true, terminal: view.closedAt > 0, body: view };
    },
  });

  // --- secrets (issue 19; ADR 0034 §4) ---------------------------------------------
  //
  // Named, versioned, write-only. The routes live in their own file so the one place a value can
  // leave this process is one file to review rather than a needle in this one; see
  // `secrets/routes.ts` for the three admission postures and why they differ.
  //
  // Built lazily: `secretStore()` computes paths and touches nothing, so an install with no
  // secrets has no store directory and no key file until somebody writes the first one.
  const secrets = opts.secrets ?? secretStore();
  registerSecretRoutes(app, secrets);

  // --- slots and bindings (issue 20) ------------------------------------------------
  //
  // The half that makes a THIRD-PARTY actor usable without its author knowing your secret names:
  // the actor declares `api_key`, the operator binds `api_key -> stripe-prod`, and every resolution
  // is a line in a ledger this API can read back. Its own file for `secrets/routes.ts`'s reason —
  // exactly one route in it answers with a value.
  registerSlotRoutes(app, secrets, opts.slots ?? slotStore(secrets));

  // --- infra (ADR 0019) -----------------------------------------------------------
  //
  // Token-gated from the FIRST commit, without exception: these routes reach a process holding
  // DIGITALOCEAN_TOKEN. Registrar and the two functions it calls live together in `infraRoutes.ts`.
  registerInfraRoutes(app);

  // --- static SPA (production) ---
  //
  // Vite content-hashes every file under /assets, so those names are immutable by construction:
  // a changed build produces a NEW name. The default here was `cache-control: public, max-age=0`
  // on everything, which made the browser re-validate the whole bundle on every visit.
  //
  // `cacheControl: false` is load-bearing. fastify-static calls `setHeaders` and THEN applies
  // send's own computed headers with `reply.headers(...)`, so a Cache-Control set in the hook is
  // overwritten by send's `max-age=<maxage>` unless send is told not to emit one at all.
  //
  // index.html is deliberately NOT immutable: it is the document that names the current hashes,
  // so caching it is how a deploy becomes invisible.
  //
  // IT STAYS IN THIS FILE, not in `routes/`. Nothing here is an API surface: it is what this
  // process serves when a request is NOT for `/api/`, and the fallback below has to know about
  // every one of those requests — which is exactly the thing `routes/` modules must not know.
  const webRoot = opts.webRoot ?? defaultWebRoot();
  if (webRoot && existsSync(webRoot)) {
    // THE SHELL'S VALIDATOR IS ITS CONTENT, and it has to be, because its mtime is a lie.
    //
    // `no-cache` above means "revalidate", not "do not cache" — so the browser asks with the
    // validator it holds and keeps its copy on a 304. send derives that validator from
    // `(size, mtime)`, and BOTH are constant across builds here: the image extracts the SPA from
    // a tarball written with a FIXED mtime (deliberately — it is what makes the bundle's sha
    // reproducible, see cli/bundle.go), so `last-modified` is the epoch and the ETag is
    // `W/"<size>-0"`. Vite's asset hashes are fixed-length, so index.html is the SAME SIZE on
    // every build too.
    //
    // The result: `W/"184-0"` for every build this image has ever served. A browser that loaded
    // the console once revalidates, gets 304, and keeps a shell naming last week's chunks — the
    // deploy is invisible, which is precisely what the comment above says must not happen.
    // MEASURED: a rebuilt console with a new pane did not appear until a hard reload.
    //
    // Hashed once at boot rather than per request: this process serves exactly one build and the
    // file cannot change under it.
    // THE SHELL IS SERVED FROM MEMORY, and not by send, because send 304s on a request we
    // cannot stop it answering.
    //
    // `lastModified: false` stops the header going OUT; it does not stop send honouring an
    // incoming `If-Modified-Since`. MEASURED after that change: a request carrying the epoch —
    // which is exactly what a browser holding a previous shell sends, since the image extracts
    // the SPA from a tarball with a fixed mtime — still came back 304, so the deploy stayed
    // invisible. Serving the bytes ourselves is the only version with no conditional path at all.
    //
    // `no-store`, not `no-cache`: the second means "revalidate", which is what put us here. The
    // shell is ~400 bytes and names the hashes of everything else, so it is the one file that must
    // never be answered from anybody's cache.
    const shell = (() => {
      try {
        return readFileSync(path.join(webRoot, 'index.html'));
      } catch {
        return null; // no SPA in this image (CI's `--no-spa` build); `/` then 404s, and /api works
      }
    })();
    const sendShell = (reply: FastifyReply) =>
      shell
        ? reply.code(200).header('content-type', 'text/html; charset=utf-8')
            .header('cache-control', 'no-store').send(shell)
        : reply.code(404).send({ error: 'no console bundle in this build' });

    app.get('/', (_req, reply) => sendShell(reply));

    app.register(fastifyStatic, {
      root: webRoot,
      cacheControl: false,
      // BOTH VALIDATORS OFF, so the shell has none and a browser must actually fetch it.
      //
      // Sending a content ETag instead was the first attempt and it is worse than it looks:
      // `etag: false` also turns off send's If-None-Match HANDLING, so the header would go out
      // and never produce a 304 — a validator the server refuses to honour. An asset needs no
      // validator either: its NAME is its content hash, and `immutable` already means "never
      // ask again".
      //
      // The cost of always serving the shell is ~400 bytes per navigation. The cost of getting
      // it wrong is a deploy nobody can see.
      etag: false,
      lastModified: false,
      setHeaders(res, filePath) {
        const immutable = filePath.includes(`${path.sep}assets${path.sep}`);
        res.setHeader(
          'cache-control',
          immutable ? 'public, max-age=31536000, immutable' : 'no-cache'
        );
      },
    });
    // SPA fallback: any non-API GET that isn't a real file serves index.html.
    /**
     * THE SPA FALLBACK, and the two things it must NOT swallow.
     *
     * A path-addressed surface (`/runs/<id>`, `/datasets/<name>`) is not a file, so a cold load or
     * an F5 on one has to come back as `index.html` — that is what this exists for.
     *
     * IT MUST NOT ANSWER FOR A MISSING ASSET. It did, and the failure is nasty: `index.html` is
     * `no-cache` but the chunks it names are `immutable`, so a browser holding a previous shell asks
     * for a chunk a redeploy has deleted, gets 200 text/html back, and the module loader tries to
     * parse HTML as JavaScript. The page dies with no useful error and a plain reload does not
     * always fix it. Observed on this box after a redeploy: `/assets/DatasetPage-PvDWLHct.js` —
     * deleted minutes earlier — answered `200 text/html`. A missing asset must 404, so the browser
     * reports what actually happened.
     *
     * IT MUST NOT 404 A SURFACE, which the extension heuristic alone did. That rule was first
     * written as "does the last path segment look like a file", on the stated assumption that "a
     * surface id never carries a file extension". **That assumption is false, and the same change
     * that introduced it falsified it.** `formatAddress` mints `/runs/<id>`, `/datasets/<name>` and
     * `/monitor/<terminalId>` with `encodeURIComponent`, which does NOT escape `.`, and dots are
     * expected in all three: `address.ts` says in its own comment that a Terminal id carries "a
     * colon (`kontra-recon:0.1`) and, on tmux, a dot"; `safeName` admits `.` in a dataset name; and
     * a `--id` may carry one (`sweep-v1.2`). So `/datasets/acme.com` answered
     * `404 {"error":"not found"}` on a cold load — breaking exactly the deep links the URL work
     * shipped, while in-app navigation hid it because it never leaves the SPA.
     *
     * SO ASK THE QUESTION THAT HAS AN ANSWER: is this one of the app's surfaces? Those are a closed
     * set the frontend already enumerates, and matching on the FIRST segment is independent of
     * whatever bytes an id happens to contain. The extension heuristic is kept for everything else,
     * which is what still 404s a deleted chunk, a `favicon.ico` or a stray source map.
     *
     * The list is restated here rather than imported because `frontend` is a separate
     * package the server does not depend on. `address.ts`'s `PATHS` is a `Record<View, string>` so
     * that a new surface fails to compile there; the cost of the copy is that a seventh surface must
     * be added in both places, and `spaFallback.test.ts` pins the pair.
     */
    app.setNotFoundHandler((req, reply) => {
      if (req.method === 'GET' && !req.url.startsWith('/api/')) {
        const path = req.url.split('?')[0] ?? '';
        const first = path.split('/')[1] ?? '';
        if (SPA_SURFACES.has(first) || SVELTE_ROUTES.has(first)) return sendShell(reply);
        const last = path.slice(path.lastIndexOf('/') + 1);
        if (!/\.[A-Za-z0-9]+$/.test(last)) return sendShell(reply);
      }
      return reply.code(404).send({ error: 'not found' });
    });
  }

  return app;
}

/**
 * Locate the built SPA by walking up until one of the two shapes it can sit in appears.
 *
 * A marker search rather than a fixed relative path, because this file runs at two DEPTHS —
 * `src/` under vitest and `dist/src/` compiled — and now in two SHAPES as well:
 *
 *   `web/dist`              THE BUNDLE. Inside a hydrated install bundle the SPA is a CHILD of
 *                           the server: `orchestrator/dist/src/main.js` beside
 *                           `orchestrator/web/dist`. That layout is an artifact contract
 *                           `runtime/handler/internal/hydrate` writes and reads, and it did not move.
 *   `../kontra-console/dist` THE CHECKOUT. The console is a separate REPOSITORY since ADR 0038, so
 *                           the checkout shape is a sibling directory rather than a sibling
 *                           package. This was `../frontend/dist`; `frontend/` no longer exists.
 *
 * BOTH, AT EVERY LEVEL, because the walk cannot know which shape it is in. Checking only the first
 * would serve no SPA from a local checkout — a console that boots, answers its API and renders
 * nothing, which is exactly the failure `kontra up --orchestrator=local` exists to avoid.
 *
 * `KONTRA_CONSOLE_DIST` OVERRIDES BOTH, and is the answer for a console checked out somewhere the
 * walk will never look. A path that is set and wrong returns undefined rather than falling through
 * to a search — an operator who named a directory wants to hear that it had no `index.html`, not
 * to be quietly served a different SPA.
 *
 * Returns undefined when the SPA is not built, which is not an error: `defaultWebRoot` returning
 * nothing degrades the API to serving no static files rather than failing to boot.
 */
export function defaultWebRoot(from: string = __dirname): string | undefined {
  const override = process.env.KONTRA_CONSOLE_DIST;
  if (override) {
    return existsSync(path.join(override, 'index.html')) ? path.resolve(override) : undefined;
  }
  let dir = from;
  for (let i = 0; i < 8; i += 1) {
    for (const candidate of [
      path.join(dir, 'web', 'dist'),
      path.join(dir, '..', 'kontra-console', 'dist'),
    ]) {
      if (existsSync(path.join(candidate, 'index.html'))) return path.resolve(candidate);
    }
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return undefined;
}

/**
 * START THE API ROLE — build the server, say what it admits, and listen.
 *
 * A FUNCTION because this is one of three roles that may share a PID now (ADR 0031 §1,
 * `main.ts`), and everything in it is boot rather than construction: `buildServer` is what a test
 * or a CLI calls, and it deliberately acquires no port, no background loop and no Temporal
 * connection. That line has not moved — it is the same body, one indent in.
 *
 * It RESOLVES when the port is bound, and the process stays alive on the listener rather than on
 * this promise. A caller that wants to run the other roles beside it therefore gets control back.
 */
export async function runApi(): Promise<FastifyInstance> {
  const app = buildServer({ logger: true });
  const port = Number(process.env.KONTRA_ORCHESTRATOR_PORT ?? process.env.PORT ?? 8088);

  // Say what the control surface admits, at boot, every time. `serve` and `start` are open by
  // default by the operator's choice — but a choice nobody is reminded of is one that gets made
  // once and inherited forever, and this one ends in a machine that can be provisioned by
  // anything that can reach this port. `warn`, not `info`: it should not read like a banner.
  const exposure = describeExposure();
  if (exposure.open) app.log.warn(exposure.detail);
  else app.log.info(exposure.detail);

  // Keep the reduced event log of every closed run (ADR 0025). Started HERE and not in
  // `buildServer` on purpose: a test or a CLI that builds a server must not acquire a background
  // loop that talks to Temporal. Failures are logged and never thrown — the archive going quiet
  // must not take the API down with it, and an operator chasing a missing log needs to see which
  // of the two authorities refused.
  startHistoryArchiver(new ObjectStore(), {
    onError: (err, runId) =>
      app.log.warn(`history archive: ${runId ? `run ${runId}: ` : ''}${errMessage(err)}`),
    // NOT A FAILURE, AND THEREFORE NOT `warn`-BY-DEFAULT: an unconfigured store, a deliberate
    // `off`, a run that aged out and a page that came back full are ordinary states of a healthy
    // system. They go to `info` so they are READ — routing them through the error channel is how
    // an operator learns to ignore the error channel (issue F5).
    onNote: (note) => app.log.info(note),
  });

  // `inuse-` tags, which are what make registry retention safe to ENFORCE: zot keeps the five most
  // recently pushed tags per repository, and without these a sixth-oldest version a Fleet is still
  // running is deleted on schedule. Armed here for the same reason the archive above is — the
  // materializer role has no periodic loop to join, and this is the only in-process reconciler idiom
  // the codebase has. Failures are noted, never thrown: a registry blip must not take the API down,
  // and the worst case of this loop going quiet is retention keeping more than it must.
  //
  // Its own Repo handle, the way the archiver above takes its own ObjectStore: `buildServer` keeps
  // the one it built private, and a background loop that outlives a request has no business reaching
  // into a request-scoped graph. `Repo`'s constructor migration is PRAGMA-guarded and idempotent, so
  // a second reader of the same file is not a second schema.
  const inuseRepo = new Repo(process.env.KONTRA_ORCHESTRATOR_DB ?? 'orchestrator.db');
  startInuseReconciler({
    listActors: () => inuseRepo.listActors(),
    onError: (err, where) => app.log.warn(`inuse tags: ${where ? `${where}: ` : ''}${errMessage(err)}`),
    onNote: (note) => app.log.info(note),
  });

  /* RENDER A REPORT FOR EVERY CLOSED RUN (ADR 0055). Started beside the history archiver, and for the
     same reasons stated there: not in `buildServer`, so a test or a CLI that builds a server acquires
     no background loop; failures logged and never thrown, so a report going quiet cannot take the API
     down with it.
     IT IS THE SAME DETECTION as the archiver's — one visibility query over closed runs — which is why
     it lives here rather than in the materializer role the specification names. The materializer is a
     Temporal Worker with no interval loop and no visibility query; putting a second detection
     mechanism there would be a second thing to learn and a second thing to get wrong. The RENDER
     itself goes to a worker thread (`report/renderHost.ts`), so this loop costs the API's event loop
     nothing but the store reads. */
  startReportRenderer({
    list: async () => listRuns(),
    io: (runId) => fetchRunIO(runId),
    close: (runId) => fetchRunClose(runId),
    identity: async (runId) => {
      const found = await runWorkflowStore().get(runId);
      return found ? { workflow: found.workflow, version: found.version } : undefined;
    },
    onError: (err, runId) =>
      app.log.warn(`report renderer: ${runId ? `run ${runId}: ` : ''}${errMessage(err)}`),
    onNote: (note) => app.log.info(note),
  });

  // AWAITED, where it used to be fire-and-forget. A merged process starts three roles and the
  // caller has to know this one is actually up — an API whose port never bound, beside a
  // materializer that is happily polling, is a control plane reporting healthy with no control
  // surface. The catch is unchanged: a control plane that cannot bind its port must die loudly
  // rather than run without one.
  // THE ADDRESS, WHICH USED TO BE HARD-CODED `0.0.0.0` (issue 18).
  //
  // That was right when this was a container: the bind was inside a network namespace and the
  // control was the compose `ports:` entry, eleven of which each named an address deliberately.
  // It is not right now that the process runs on the host. `kontra up --bind` closes five
  // embedded services onto one address; this listener ignored it, so an install told to bind
  // the docker bridge still answered — unauthenticated — on every interface the machine has,
  // including a public one. MEASURED on a droplet, and it is precisely the exposure ADR 0031 §3
  // says loopback removes.
  //
  // THE DEFAULT IS UNCHANGED, on purpose. `KONTRA_ORCHESTRATOR_BIND` unset still means `0.0.0.0`,
  // because a copy of this image running the `api` role on a worker host (docker-compose.yml
  // describes exactly that relocation) binds inside its own namespace and would be unreachable
  // through its published port otherwise. `kontra up` sets the variable; a container does not.
  const host = process.env.KONTRA_ORCHESTRATOR_BIND?.trim() || '0.0.0.0';
  await app.listen({ port, host }).catch((err) => {
    app.log.error(err);
    process.exit(1);
  });
  return app;
}

if (require.main === module) {
  // `node dist/src/server.js` — the API and NOTHING ELSE. Still supported, and it is what
  // `KONTRA_ORCHESTRATOR_ROLES=api` gets you through `main.js`; a deployment that wants the other
  // roles somewhere else should not have to name them to leave them out.
  void runApi().catch((err) => {
    // eslint-disable-next-line no-console
    console.error('[api] fatal:', err);
    process.exit(1);
  });
}
