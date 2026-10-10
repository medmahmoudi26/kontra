/**
 * RENDERING A REPORT WHEN A RUN ENDS.
 *
 * ── THE SPECIFICATION NAMES THE WRONG TWO FILES ────────────────────────────────────────────────
 *
 * §4.5 says to hook in "where the orchestrator already detects a run has finished for
 * materialization (start at `data/sealFinishedDatasets.ts` and the materializer role)". Neither is
 * that place. `sealFinishedDatasets.ts` is a pure decision function whose only importer is its own
 * test, and `materializer.ts` is a Temporal Worker registering activities on `kontra-datasets` — it
 * hosts no workflows and notices nothing.
 *
 * What actually notices is `startHistoryArchiver` in `historyArchive.ts`: a `setInterval` in the API
 * role running one Temporal visibility query and acting on the closed runs it finds. This module is
 * that pattern, copied deliberately — the counters, the `onNote`/`onError` split, the single-flight
 * guard, the capped-page warning — because it was built for the same problem ("a Dataset outlives the
 * Run; the Run's story does not") and a second shape for the same job would be a second thing to
 * learn and a second thing to get wrong.
 *
 * ── IT IS NOT IN A REQUEST PATH AND NOT IN A WORKFLOW, WHICH IS WHAT §4.5 ACTUALLY ASKS ────────
 *
 * The role is `api`, because that is where the detection loop lives; the RENDER is in a worker thread
 * (`renderHost.ts`), so the API's event loop is not held by it. Putting the sweep in the materializer
 * role instead would mean inventing an interval loop in a Temporal Worker that has none, and the
 * detection primitive — the visibility query — is not there either.
 *
 * ── §4.6'S FALLBACK CANNOT BE IMPLEMENTED AS WRITTEN ───────────────────────────────────────────
 *
 * It says that if the start path cannot pin the template, the renderer should "fall back to capturing
 * at run end". Capturing at run end means reading `report.md` out of the Run's workflow folder, and
 * NOTHING MAPS A RUN ID TO ITS FOLDER: `runWorkflows` records a manifest name and version, Temporal
 * holds a workflow type, and the run id is `<type>-<unixseconds>`. There is no path from any of those
 * to a directory.
 *
 * So the fallback is the DEFAULT report plus a warning in the snapshot naming what happened. That is
 * the honest form of §4.6's intent — never a silently empty page — and it is also the better one:
 * reading today's `report.md` for a Run that started a week ago would produce a report nobody can
 * reproduce, which is the exact thing pinning exists to prevent. A Run started through
 * `POST /api/runs` is pinned and unaffected; a Run started by `kontra workflow start`, which dials
 * Temporal directly, is the case this paragraph is about.
 */

import { createHash } from 'node:crypto';

import { DEFAULT_TEMPLATE, defaultContext, defaultTemplateId } from './defaultTemplate';
import {
  buildContext,
  renderKey,
  statusWord,
  type DatasetSummary,
  type ProgressContext,
  type TemplateContext,
} from './context';
import { render as renderInHost, type RenderResponse } from './renderHost';
import type { ReportSnapshot } from './render';
import { reportStore, type ReportStore } from './store';
import { folderTemplateFor, type FolderTemplate } from './folderTemplate';
import { activeNamespace } from '../workspaces';

/** Every 15 minutes, matching the history archiver — the same runs, the same visibility query. */
/**
 * ADR 0062 CHANGED WHAT THIS LOOP IS FOR, AND THEREFORE ITS CADENCE — 15 minutes to 60 seconds.
 *
 * It used to be the only way a finished Run got a report, so the interval WAS the latency: up to
 * fifteen minutes of a completed run showing nothing. Live mode parks a long poll per watched run and
 * finalises on the terminal event, so that latency is gone and this pass is no longer on the happy
 * path at all.
 *
 * WHAT IT STILL DOES, AND WHY DELETING IT WOULD HAVE BEEN A REGRESSION DRESSED AS A CLEANUP:
 * `renderKey` covers the TEMPLATE HASH, so fixing a typo in `report.md` changes the key and the next
 * pass re-renders every affected finished run — the back catalogue heals itself. A terminal-event
 * watcher is structurally blind to that: the run is closed, its history is final, and no event will
 * ever arrive for an edit to a file. This is the convergence loop for (template × run); completion
 * was only its most visible input.
 *
 * It is also the reconciliation arm for runs that ended while no watcher was parked — a restart, or a
 * run nobody was watching. In steady state it should find NOTHING, and `startReportRenderer` says so
 * out loud when it finds something, because a safety net doing real work every minute means the
 * mechanism it is backing up is broken.
 */
const DEFAULT_INTERVAL_MS = 60 * 1000;

/** How many ids a counter names before it stops naming them. A sweep report is a log line. */
export const SKIPPED_IDS_CAP = 50;

/** Just enough of a run row for this module. Satisfied by `RunRow` from `temporalClient`. */
export interface SweepRun {
  runId: string;
  status: string;
  startedAt: number;
  closedAt: number;
  type?: string;
}

/**
 * One pass's outcome, one counter per distinguishable thing that can happen.
 *
 * NO ROLLED-UP TOTAL. `gone` is the one that means the sweep is too slow and `failed` is the one that
 * means something is broken; adding them would hide whichever is smaller. `noTemplate` is not a
 * failure at all — it is the common case — and counting it separately is what stops somebody reading
 * a healthy install's log as an outage.
 */
export interface ReportSweepResult {
  scanned: number;
  closed: number;
  rendered: number;
  /** Already had a version for this exact render — the idempotent path, and the steady state. */
  present: number;
  /** Rendered through the default template because nothing pinned one. */
  noTemplate: number;
  /** Rendered, but the template failed: a version with `status: 'error'` was stored. */
  errored: number;
  /** Temporal described it and then could not serve its metadata — retention took it in between. */
  gone: number;
  goneIds: string[];
  failed: number;
  capped: boolean;
}

export interface SweepDeps {
  list?: () => Promise<SweepRun[]>;
  io?: (runId: string) => Promise<{ input?: unknown; output?: unknown } | undefined>;
  close?: (runId: string) => Promise<{ type: string; message: string } | undefined>;
  identity?: (runId: string) => Promise<{ workflow: string; version: string } | undefined>;
  store?: ReportStore;
  render?: (request: { template: string; context: Record<string, unknown> }) => Promise<RenderResponse>;
  /** Name the default template by this release instead of by its own digest. See `defaultTemplateId`. */
  version?: string;
  now?: () => number;
  onNote?: (note: string) => void;
  onError?: (err: unknown, runId?: string) => void;
  /** A page this size may be a prefix of what exists — the archiver's own caveat. */
  listLimit?: number;  /**
   * Run one run's whole step — its reads and its store writes — inside a scope, which the server
   * uses to address the run's own namespace (ADR 0051). Identity when absent.
   */
  within?: <T>(runId: string, fn: () => Promise<T>) => Promise<T>;
  /**
   * The template of a run nothing pinned, from its workflow type (`report/folderTemplate.ts`).
   * Defaults to a lookup in the active namespace's workspace folder.
   */
  folderTemplate?: (type: string) => FolderTemplate | undefined;
  /**
   * Whether a template found that way is PINNED. True for the sweep and the live render; false for
   * a preview, which §6.2 says stores nothing.
   */
  pinFound?: boolean;
}

/** A closed run is reportable. The same predicate as `isArchivable`, and for the same reason. */
export function isReportable(run: Pick<SweepRun, 'status' | 'closedAt'>): boolean {
  return run.closedAt > 0 && run.status !== 'running' && run.status !== 'pending';
}

/**
 * Render every closed run that has no report yet.
 *
 * ONE RUN'S FAILURE NEVER STOPS THE PASS. Each run is wrapped, counted and reported through
 * `onError`; the alternative is one unreadable run blocking every later one forever, which is the
 * shape that makes a sweep look like it is working while it has stopped.
 */
export async function sweepFinishedRuns(deps: SweepDeps = {}): Promise<ReportSweepResult> {
  const store = deps.store ?? reportStore();
  const now = deps.now ?? Date.now;
  const renderOne = deps.render ?? ((request) => renderInHost(request));
  const out: ReportSweepResult = {
    scanned: 0,
    closed: 0,
    rendered: 0,
    present: 0,
    noTemplate: 0,
    errored: 0,
    gone: 0,
    goneIds: [],
    failed: 0,
    capped: false,
  };

  const runs = await (deps.list ?? (async () => []))();
  out.scanned = runs.length;
  if (deps.listLimit !== undefined) out.capped = runs.length >= deps.listLimit;

  const within = deps.within ?? (<T>(_runId: string, fn: () => Promise<T>) => fn());
  for (const run of runs) {
    if (!isReportable(run)) continue;
    out.closed += 1;
    try {
      await within(run.runId, async () => {
        const io = deps.io ? await deps.io(run.runId) : undefined;
        if (!io) {
          // Temporal described it a moment ago and cannot serve its metadata now. Counted and NAMED,
          // never inferred — the archiver's rule, and the reason is the same: the loss is unrecoverable
          // and the only moment it can be reconciled is now.
          out.gone += 1;
          if (out.goneIds.length < SKIPPED_IDS_CAP) out.goneIds.push(run.runId);
          return;
        }
        const built = await contextForRun(run, io, {
          ...deps,
          store,
          now,
        });
        const { template, templateHash, context } = built;

        /* THE KEY IS CHECKED BEFORE THE RENDER AND THE ROW IS WRITTEN AFTER IT, in ONE insert.
           The first version of this claimed the key first and inserted the snapshot second, which found
           its own row by that key and answered `created: false` — so every snapshot was discarded and
           every report stored empty. One insert, carrying the snapshot, is the fix.
           The cheap read here is what keeps a steady-state pass over a full retention window from
           rendering anything; two orchestrators racing can both render, and the loser's insert is
           absorbed by the unique index, which costs one wasted render rather than a lost report. */
        const key = renderKey(templateHash, context);
        if ((await store.versionByKey(run.runId, key)) !== undefined) {
          out.present += 1;
          return;
        }

        if (!built.pinned) out.noTemplate += 1;

        const result = await renderOne({ template, context });
        if (!result.ok) {
          out.errored += 1;
          await store.declareVersion({
            runId: run.runId,
            status: 'error',
            templateHash,
            renderKey: key,
            errorText: result.error,
            renderedBy: 'sweep',
            at: now(),
          });
          return;
        }

        const snapshot: ReportSnapshot = !built.pinned
          ? {
              ...result.snapshot,
              warnings: [
                'No report.md was pinned when this run started, and no single workflow folder in its ' +
                  'workspace declares its type, so this is the default report.',
              ],
            }
          : built.pinnedLate
            ? {
                ...result.snapshot,
                warnings: [
                  'This run was started outside the console, which pins nothing, so this report uses ' +
                    `the report.md in its workspace's workflow folder as it was when first rendered — the ` +
                    'copy on disk there, which is not necessarily the committed one or the one the run started with.',
                ],
              }
            : result.snapshot;
        const stored = await store.declareVersion({
          runId: run.runId,
          status: 'ok',
          templateHash,
          renderKey: key,
          snapshotJson: JSON.stringify(snapshot),
          renderedBy: 'sweep',
          at: now(),
        });
        if (!stored.created) {
          // Another orchestrator got there between the check and the insert. Its version is the one
          // that counts, and this render is discarded rather than stored beside it.
          out.present += 1;
          return;
        }
        await store.putSecrets(run.runId, stored.version, result.secrets);
        out.rendered += 1;
      });
    } catch (err) {
      out.failed += 1;
      deps.onError?.(err, run.runId);
    }
  }
  return out;
}

/**
 * Start the sweep. Returns a stop function.
 *
 * SINGLE-FLIGHT, because a first pass over a full retention window can outlast the interval and two
 * of them would render every run twice to store the same version once — the archiver's reasoning, and
 * here the wasted work is CPU rather than an S3 read.
 *
 * NOTHING THROWN HERE MAY ESCAPE. A rejection inside `setInterval` takes the API process down, and a
 * report is the least important thing this process does.
 */
export function startReportRenderer(deps: SweepDeps = {}): () => void {
  if (process.env.KONTRA_REPORT_RENDER === 'off') {
    deps.onNote?.(
      'report renderer: OFF (KONTRA_REPORT_RENDER=off). Closed runs will have no report, and this ' +
        'is the only warning of it.'
    );
    return () => undefined;
  }
  const every = Number(process.env.KONTRA_REPORT_RENDER_MS ?? DEFAULT_INTERVAL_MS);
  const interval = Number.isFinite(every) && every > 0 ? every : DEFAULT_INTERVAL_MS;

  let running = false;
  /**
   * ONCE PER PROCESS, BEFORE THE FIRST PASS, and idempotent — after it has run there are no such rows
   * and it costs one query per boot. See {@link ReportStore.dropEmptyDefaultVersions} for why these
   * rows cannot heal through the ordinary convergence loop: the fix moves no `renderKey`, so the key
   * check skips them and `POST /report/render` confirms them, both correctly.
   *
   * It runs HERE rather than as a migration because the sweep immediately after it is what produces
   * the replacements. A migration would leave the rows gone and the reports absent until something
   * else happened to run; this way the repair and the re-render are one boot apart at most.
   */
  let repaired = false;
  const repair = async (): Promise<void> => {
    if (repaired) return;
    repaired = true;
    const store = deps.store ?? reportStore();
    const removed = await store.dropEmptyDefaultVersions();
    if (removed.length === 0) return;
    // NAMED, NOT COUNTED, on SKIPPED_IDS_CAP's grounds: a line saying a number of reports were
    // deleted is not something an operator can check, and this is a deletion.
    const named = removed.slice(0, SKIPPED_IDS_CAP).map((r) => `${r.runId}@v${r.version}`);
    deps.onNote?.(
      `report renderer: removed ${removed.length} empty report version(s) that named the default ` +
        'template but held the render of an empty one — ' +
        `${named.join(', ')}${removed.length > named.length ? ', …' : ''}. ` +
        'This pass re-renders them.'
    );
  };

  const pass = async (): Promise<void> => {
    if (running) return;
    running = true;
    try {
      await repair();
      const report = await sweepFinishedRuns(deps);
      // A SAFETY NET THAT IS DOING REAL WORK IS A BROKEN PRIMARY. Since ADR 0062 the watcher
      // finalises a watched run on its terminal event, so a render here is either a template edit
      // healing the back catalogue (expected, occasional) or a run the watcher missed (not expected).
      // Silence in steady state is the signal; this line is how the difference becomes visible.
      if (report.rendered > 0) {
        deps.onNote?.(
          `report renderer: the reconciliation pass rendered ${report.rendered} run(s). Expected ` +
            'after a template edit; otherwise the terminal-event watcher missed them.'
        );
      }
      if (report.gone > 0) {
        deps.onNote?.(
          `report renderer: ${report.gone} run(s) lost their metadata before a report could be ` +
            `rendered — ${report.goneIds.join(', ')}${report.gone > report.goneIds.length ? ', …' : ''}`
        );
      }
      if (report.errored > 0) {
        deps.onNote?.(
          `report renderer: ${report.errored} template(s) failed to render. Each stored a version ` +
            'saying why; the runs themselves are unaffected.'
        );
      }
      if (report.capped) {
        deps.onNote?.(
          `report renderer: the run listing came back FULL (${report.scanned}) — there may be more ` +
            'than this pass considered.'
        );
      }
    } catch (err) {
      deps.onError?.(err);
    } finally {
      running = false;
    }
  };

  void pass();
  const timer = setInterval(() => void pass(), interval);
  timer.unref?.();
  return () => clearInterval(timer);
}

/** A short digest of a template's text — what `report_template.template_hash` holds. */
export function templateHash(text: string): string {
  return `sha256:${createHash('sha256').update(text, 'utf8').digest('hex')}`;
}

/**
 * The template and the context for one Run — the one place either is assembled.
 *
 * SHARED WITH THE PREVIEW ROUTE, which is why it is exported and why it takes a Run row rather than
 * reaching for one. §6.2 says a preview "uses the stored run context"; the snapshot holds the rendered
 * TREE and not the inputs that produced it, and storing those would be a second copy of every Run's
 * input and result — exactly what the claim-check codec exists to avoid. So a preview rebuilds the
 * context the same way the sweep does, from the same reads, through this function. Two assemblers would
 * mean a preview that renders differently from the version it is previewing a change to, which is the
 * one thing a preview must not do.
 */
export async function contextForRun(
  run: SweepRun,
  io: { input?: unknown; output?: unknown },
  deps: SweepDeps & { store: ReportStore; now: () => number },
  /**
   * What only an OPEN run has: `run.progress`, the in-flight `datasets` and the workflow's `report`
   * query as `result` (ADR 0062 §2). Passed by the live renderer alone. The stored render never
   * passes it — it is built from the final context — so nothing here can reach a `renderKey`.
   */
  live?: { progress?: ProgressContext; datasets?: Record<string, DatasetSummary>; partial?: unknown }
): Promise<{
  template: string;
  templateHash: string;
  context: TemplateContext;
  pinned: boolean;
  /** Pinned from the folder at a render after the start, not by the start: see {@link LATE_PIN_MS}. */
  pinnedLate: boolean;
}> {
  let pinned = await deps.store.template(run.runId);
  if (pinned === undefined && run.type) {
    // NOTHING PINNED IT — a run started outside the console. Find its folder by type in its own
    // workspace and pin what is there now, so the next render of this run reads the same template.
    const found = (deps.folderTemplate ?? ((type: string) => folderTemplateFor(type, activeNamespace())))(run.type);
    if (found) {
      const pin = {
        runId: run.runId,
        templateHash: found.text === undefined ? defaultTemplateId(deps.version) : templateHash(found.text),
        templateText: found.text ?? '',
        source: (found.text === undefined ? 'default' : 'workspace') as 'default' | 'workspace',
        workspace: found.workspace,
        capturedAt: deps.now(),
      };
      if (deps.pinFound === false) {
        pinned = pin;
      } else {
        await deps.store.pinTemplate({ ...pin, at: pin.capturedAt });
        pinned = await deps.store.template(run.runId);
      }
    }
  }
  const status = statusWord(run.status);
  const close = status === 'completed' ? undefined : await deps.close?.(run.runId);
  const identity = await deps.identity?.(run.runId);
  const context = buildContext({
    runId: run.runId,
    status: run.status,
    startedAt: run.startedAt,
    closedAt: run.closedAt,
    ...(run.type === undefined ? {} : { type: run.type }),
    identity,
    pinned,
    input: io.input,
    output: io.output,
    error: close,
    // The version number is not known until the insert allocates it, and it is in the context because
    // §2.4 promises `report.version`. Rendered as the number this render WILL be, which the idempotency
    // key deliberately excludes — see `renderKey`.
    version: await deps.store.nextVersion(run.runId),
    now: deps.now(),
    ...(live?.progress ? { progress: live.progress } : {}),
    ...(live?.datasets ? { datasets: live.datasets } : {}),
    ...(live?.partial !== undefined ? { partial: live.partial } : {}),
  });
  /*
   * PINNED TO THE DEFAULT IS STILL THE DEFAULT, and reading `pinned` as a BOOLEAN missed that in two
   * places at once. `pinTemplate` stores `templateText: ''` for `source: 'default'` deliberately —
   * the text ships with the orchestrator and a copy per Run would be megabytes of identical rows —
   * but `'' ?? DEFAULT_TEMPLATE` is `''`, so such a Run rendered an EMPTY DOCUMENT. That is every Run
   * started without a `report.md` beside its workflow, which is most of them.
   *
   * Supplying the text alone would only have traded the empty document for an `UndefinedVariableError`
   * on `default.tables`, because `default` was withheld from the context whenever `pinned` was truthy.
   * BOTH ARMS HAVE TO KEY ON THE SOURCE, which is why this is one change and not two.
   *
   * `templateHash` is deliberately untouched: `workflowControl.ts` already pins `defaultTemplateId()`
   * for these Runs, so the hash always named the right template even while the render did not. That is
   * what makes this fix safe — it moves no `renderKey` and orphans no stored version — and it is also
   * why the versions already stored cannot heal on their own. See
   * {@link ReportStore.dropEmptyDefaultVersions}, which removes them so this can replace them.
   *
   * Found by the live-report session against this branch.
   */
  const usesDefault = pinned === undefined || pinned.source === 'default' || pinned.templateText === '';
  const withDefault: TemplateContext = usesDefault
    ? {
        ...context,
        default: defaultContext(context.run as unknown as Record<string, unknown>, context.result),
      }
    : context;
  return {
    template: usesDefault ? DEFAULT_TEMPLATE : pinned!.templateText,
    templateHash: pinned?.templateHash ?? defaultTemplateId(deps.version),
    context: withDefault,
    pinned: pinned !== undefined,
    pinnedLate:
      pinned !== undefined && pinned.source === 'workspace' && pinned.capturedAt - run.startedAt > LATE_PIN_MS,
  };
}

/**
 * How long after its start a pin can be and still be the START's pin. `POST /api/runs` pins within
 * a second of starting; a pin later than this was made by a render from the folder as it was then,
 * and the report says so — the workspace copy is not necessarily what was committed, nor what ran.
 */
export const LATE_PIN_MS = 60_000;
