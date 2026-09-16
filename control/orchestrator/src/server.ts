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
 *   routes/panels.ts      the same-origin Dashboard proxy, so the SPA holds no panel token
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
import { existsSync } from 'node:fs';
import Fastify, { type FastifyInstance } from 'fastify';
import fastifyStatic from '@fastify/static';
import fastifyCompress from '@fastify/compress';
import { Repo } from './db/repo';
import { SourceStore } from './sourceStore';
import { RunLifecycle } from './runs';
import type { PulseDeps } from './pulse';
import type { RunDescription } from './temporalClient';
import { HistoryArchive, startHistoryArchiver } from './historyArchive';
import { registerInfraRoutes } from './infraRoutes';
import { registerSecretRoutes } from './secrets/routes';
import { registerSlotRoutes } from './secrets/slotRoutes';
import { slotStore, type SlotStore } from './secrets/slotStore';
import { secretStore, type SecretStore } from './secrets/store';
import { describeExposure, setRegisteredFolders } from './workflowControl';
import { describeQueue, temporalQueueDescriber, type QueueDescriber } from './panels/pollers';
import { ObjectStore } from './codec/objectStore';
import { DatasetRecordStore, datasetRecordStore } from './data/datasetRecords';
import { RunWorkflowStore, runWorkflowStore } from './data/runWorkflows';
import type { LakeConfig } from './data/parquet';
import { MaterializationStore, materializationStore } from './data/materializationStore';
import { SummaryStore, summaryStore } from './data/summaries';
import { registerCatalogRoutes } from './routes/catalog';
import { registerDatasetRoutes } from './routes/datasets';
import { errMessage } from './routes/errors';
import { registerExploreRoutes } from './routes/explore';
import { registerFleetRoutes } from './routes/fleet';
import { registerHistoryRoutes } from './routes/history';
import { registerHitlRoutes } from './routes/hitl';
import { registerLoginRoutes } from './routes/login';
import { registerPanelRoutes } from './routes/panels';
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

/**
 * The web app's surfaces, by first path segment — the closed set the SPA fallback serves.
 *
 * MUST TRACK `frontend/src/state/surfaces.ts`'s `SPA_SEGMENTS`, which is the authority. It
 * is copied rather than imported because `frontend` is a separate package this one does not
 * depend on; `spaFallback.test.ts` pins the two together so the copy cannot drift silently.
 *
 * Matching the FIRST segment is the point: everything after it is an id whose bytes are not ours to
 * predict — a Terminal id carries a colon and, on tmux, a dot, and a dataset name may carry one too.
 *
 * `runs` AND `scratch` ARE RETIRED SURFACES AND ARE STILL SERVED. A run is reached through the
 * workflow that produced it and Scratch became a workflow's own tab, but `/runs/<id>` is in
 * somebody's notes and still names a run — the console REDIRECTS it.
 * A redirect is code, and code has to load: drop either segment here and a cold load of
 * `/runs/sweep-v1.2` 404s on the dot before the shell that would forward it ever runs.
 */
export const SPA_SURFACES: ReadonlySet<string> = new Set([
  'catalog',
  'workflows',
  'actors',
  'datasets',
  'monitor',
  'secrets',
  'settings',
  // retired, still addressable — see above
  'runs',
  'scratch',
]);

/**
 * The surfaces the SVELTE bundle serves (ADR 0048 §1).
 *
 * ── TWO SETS, ONE RULE ──────────────────────────────────────────────────────────────────────────
 *
 * The console is migrating one Surface at a time, and the seam is this: a first segment in here
 * gets `svelte.html`, a first segment in {@link SPA_SURFACES} gets `index.html`. No interop, no
 * shared shell — the only thing the two bundles share is an origin and a session.
 *
 * MUST BE DISJOINT FROM `SPA_SURFACES`, and {@link assertBundlesAreDisjoint} enforces it at boot.
 * A segment in both is not a merge conflict that fails loudly; it is a surface that silently serves
 * whichever bundle this file checks first, which is decided by the order of two `if`s.
 *
 * MOVING A SURFACE IS MOVING A STRING between these two sets. That is the whole migration
 * mechanism, and it is reversible in one line — which is what makes the checkpoint in slice 10 a
 * real decision rather than a direction of travel.
 *
 * Empty until slice 04. `/dev` is not here and does not need to be: it has no extension, so the
 * fallback's second clause already serves it — which is also why the DEV_ROUTES set below exists,
 * because "extensionless" is not a bundle.
 */
export const SVELTE_SURFACES: ReadonlySet<string> = new Set<string>([]);

/**
 * Extensionless routes the Svelte bundle owns that are not Surfaces — today, the IDE embed.
 *
 * `/dev` never appeared in `SPA_SURFACES` and was served anyway, by the fallback's "no file
 * extension" clause. That worked while there was one document. With two it is ambiguous, and an
 * ambiguity resolved by which branch runs first is the kind that is discovered by a user.
 */
export const SVELTE_ROUTES: ReadonlySet<string> = new Set<string>([
  // THE WALKING SKELETON, and it is temporary on purpose. `/_svelte` is what slice 02 ships: a
  // plain page that proves the second bundle is built, served, routed and sharing a session,
  // before any surface depends on that being true. It goes when `/dev` moves in slice 04 — at
  // which point this set has a real member and this one is noise.
  //
  // It is also what stops `spaFallback.test.ts` asserting over an empty set: a loop over nothing
  // passes every expectation inside it, so a skeleton with no route is a test suite that proves
  // the split works without ever having served the second document.
  '_svelte',
]);

/**
 * Refuse to boot if a segment claims both bundles.
 *
 * Called from {@link buildServer}. It throws rather than warns: a duplicated segment means one of
 * two consoles is unreachable, and the process that would have told you is the one now serving the
 * wrong one.
 */
export function assertBundlesAreDisjoint(
  react: ReadonlySet<string> = SPA_SURFACES,
  svelte: ReadonlySet<string> = SVELTE_SURFACES
): void {
  const both = [...svelte].filter((s) => react.has(s));
  if (both.length > 0) {
    throw new Error(
      `SPA surfaces claimed by both bundles: ${both.join(', ')}. ` +
        'A surface belongs to exactly one bundle — moving it means DELETING it from the other set, ' +
        'not adding it here.'
    );
  }
}

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
  assertBundlesAreDisjoint();
  const repo = opts.repo ?? new Repo(process.env.KONTRA_ORCHESTRATOR_DB ?? 'orchestrator.db');
  // The object store: SHARED by the dataset browser, the workbench, the explore manifest, the row
  // tail and the history archive. One store, so a count on one surface cannot disagree with a
  // count on another.
  const store = opts.store ?? new ObjectStore();
  // Where an operator's own code lives. Reads its list through the same Repo, so a registration
  // survives a restart the way an actor's catalog entry does. SHARED by the sources and probe
  // surfaces: a probe runs the FOLDER's Actor and version, never a body's.
  const sources = new SourceStore(repo);
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

  // THE ROUTE INVENTORY, DERIVED RATHER THAN MAINTAINED.
  //
  // `onRoute` fires for every route as it is registered, so this list is the routes that EXIST —
  // not a second list somebody has to remember to update. `docs/openapi.json` is generated from it
  // and `openapi.test.ts` fails when the two disagree, which is what makes a route that quietly
  // moved a red build rather than a 404 nobody sees until a panel is empty.
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
  registerPanelRoutes(app);
  registerCatalogRoutes(app, repo);
  registerScratchRoutes(app, repo);
  registerRunRoutes(app, { runs, runWorkflows, queueDescriber });
  registerHistoryRoutes(app, archive);
  registerHitlRoutes(app, { runs, archive });
  registerFleetRoutes(app);
  registerWorkflowRoutes(app, repo);
  registerPollerRoutes(app, queueDescriber);
  registerSourceRoutes(app, sources);
  registerWorkspaceRoutes(app);
  registerProbeRoutes(app, sources);
  registerDatasetRoutes(app, { store, lake, materialization, records, runWorkflows });
  registerRetentionRoutes(app, { store, lake, materialization, records, runWorkflows, summaries });
  registerRowStreamRoute(app, store, opts.rowStreamCaps ?? {});
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
  // Built lazily: `secretStore()` computes paths and touches nothing, so an appliance with no
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
    app.register(fastifyStatic, {
      root: webRoot,
      cacheControl: false,
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
        // THE SVELTE BUNDLE IS CHECKED FIRST, and the order would matter if the two sets could
        // overlap — `assertBundlesAreDisjoint` at boot is what makes it not matter.
        if (SVELTE_SURFACES.has(first) || SVELTE_ROUTES.has(first)) return reply.sendFile('svelte.html');
        if (SPA_SURFACES.has(first)) return reply.sendFile('index.html');
        const last = path.slice(path.lastIndexOf('/') + 1);
        if (!/\.[A-Za-z0-9]+$/.test(last)) return reply.sendFile('index.html');
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
 *   `web/dist`              THE BUNDLE. Inside a hydrated appliance bundle the SPA is a CHILD of
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
  // embedded services onto one address; this listener ignored it, so an appliance told to bind
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
