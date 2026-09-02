/**
 * A run is handed off with its id — in a browser, against a stubbed orchestrator.
 *
 * WHY THE API IS STUBBED HERE AND THE STREAMER IS NOT (`dashboard.spec.ts`). That suite exists
 * because a terminal is invisible to a typecheck: a socket, a hand-rolled 101 and an xterm mounted
 * at 0×0 all pass `tsc` and show a human nothing. This one is about ROUTING AND WORDING — which
 * surface a click lands on, which id the header is showing, and what is printed in the gap before a
 * new run can say anything about itself. The orchestrator API is `page.route`d rather than booted
 * so the run under test can be shaped exactly, including the two-second window in which a brand new
 * run has no detail yet. That window is not reliably producible against a live cluster.
 *
 * FOUR TESTS WERE REMOVED HERE, not lost. They drove the RETIRED Runs page — they clicked
 * `nav-runs` and asserted on `run-row-*`, `open-run-id`, `watching-run`, `run-state` and
 * `run-error`. Every one of those testids is now absent from `frontend/src` (checked: zero
 * occurrences), the page they belonged to went with `RunsPage.tsx`, and two tests added with it —
 * in `sideNav.render.test.ts` and `shell.render.test.ts` — now assert `nav-runs` is NOT
 * rendered. A spec whose first action is a click on an element the suite proves does not exist is
 * not covering a surface; it is describing one. What those four were FOR survives in the vitest
 * suite, which tests the same wording against the live surfaces with a DOM.
 *
 * It imports `test` from `./fixtures` rather than from `@playwright/test` — the projects set a
 * custom `streamerKind` option and a plain `test` treats an option it does not know as an ERROR,
 * not a no-op. The `view` fixture is never requested, so no streamer is started for these tests.
 */

import { expect, test } from './fixtures';

/** A run that finished, and whose output the ledger says failed. Both dimensions, disagreeing —
 *  which is the pairing the whole run surface exists to keep visible (ADR 0017). */
const CRAWL = {
  runId: 'crawl-2026-08-14T09-00-00',
  type: 'Crawl',
  status: 'completed',
  tenant: 'acme',
  startedAt: Date.parse('2026-08-14T09:00:00Z'),
  closedAt: Date.parse('2026-08-14T09:04:54Z'),
  dispatches: 4,
  materialization: { total: 2, pending: 0, running: 0, complete: 1, failed: 1, rows: 623, bytes: 4096 },
  lifecycle: 'output_failed',
};

/** The newest run, still open. */
const SWEEP = {
  runId: 'nscheck-2026-08-14T11-30-00',
  type: 'NsCheck',
  status: 'running',
  tenant: 'acme',
  startedAt: Date.parse('2026-08-14T11:30:00Z'),
  closedAt: 0,
  dispatches: 2,
  materialization: { total: 1, pending: 0, running: 1, complete: 0, failed: 0, rows: 0, bytes: 0 },
  lifecycle: 'executing',
};

const SOURCE = '@workflow.defn\nclass NsCheck:\n    async def run(self):\n        pass\n';

/** `/api/runs/:id` shape — the detail names the execution dimension `execution`, and carries no
 *  `status`: that is the list row's name for the same authority. */
function detailOf(row: typeof CRAWL, over: Record<string, unknown> = {}) {
  return {
    runId: row.runId,
    type: row.type,
    tenant: row.tenant,
    startedAt: row.startedAt,
    closedAt: row.closedAt,
    execution: row.status,
    materialization: row.materialization,
    materializationRecords: [],
    lifecycle: row.lifecycle,
    settled: row.lifecycle === 'completed' || row.lifecycle === 'output_failed',
    ...over,
  };
}

interface Stubs {
  /** Rows `/api/runs` answers with. */
  runs?: unknown[];
  /** Per-run detail, by id. A function may delay — the two-second gap before the first answer is a
   *  case with its own bug. */
  detail?: Record<string, () => Promise<unknown> | unknown>;
  /** What `POST /api/runs` answers, if the test presses Run. */
  started?: { runId: string; type: string; queue: string };
}

/**
 * One handler for the whole API, switched on the path.
 *
 * A single route rather than a dozen globs, because ORDER is the trap: Playwright matches handlers
 * in reverse registration order, so `**​/api/runs` registered after `**​/api/runs/*` silently wins
 * for both — and a run detail served the run LIST reads as a page that renders nothing.
 */
async function stub(page: import('@playwright/test').Page, s: Stubs): Promise<void> {
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    const json = (body: unknown, status = 200) =>
      route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });

    // The streamer lives on its own origin and is not part of this test. Refusing is what the app
    // already handles: the rail keeps its last inventory rather than claiming zero Machines.
    if (path.startsWith('/api/panels')) return route.abort();

    if (path === '/api/runs' && route.request().method() === 'POST') {
      return json(s.started ?? { runId: 'unexpected', type: '', queue: '' }, 201);
    }
    if (path === '/api/runs') return json(s.runs ?? []);

    const run = /^\/api\/runs\/([^/]+)$/.exec(path);
    if (run) {
      const make = s.detail?.[decodeURIComponent(run[1]!)];
      if (!make) return json({ error: 'run not found' }, 404);
      return json(await make());
    }
    if (/^\/api\/runs\/[^/]+\/history$/.test(path)) {
      return json({ events: [], scanned: 0, elided: 0, truncated: false });
    }
    if (/^\/api\/runs\/[^/]+\/heartbeats$/.test(path)) return json({ nodes: {} });

    if (path === '/api/workflows') {
      return json({
        dir: '/tmp/.kontra/workflows',
        workflows: [{ name: 'nscheck.py', bytes: SOURCE.length, modifiedAt: Date.now() }],
        // THE DESCRIPTOR, without which Run is permanently disabled.
        //
        // `registered` is what a serving worker publishes about the type it serves, and the queue on
        // it is the ONLY writer of the Run form's queue field — the queue is derived from the
        // folder's content digest (`wf-<name>-<digest12>`, GitHub #15), not typed, so there is no
        // other way for it to become non-empty and `disabled={... || !queue.trim() || ...}` holds
        // forever. The stub answered `/api/workflows` without this key, so the button this spec
        // exists to press could never be pressed.
        //
        // `name` is the workflow TYPE (`NsCheck`, from SOURCE), not the file name: the page joins
        // the descriptor on the type the open source declares.
        registered: [{ name: 'NsCheck', queue: 'wf-nscheck-0a4be1b6cdea', description: '' }],
      });
    }
    // THE FOLDER LISTING, without which the Workflows page has no rows at all.
    //
    // A workflow became a FOLDER holding `workflow.py` rather than a bare file, and the page now
    // renders `mergeWorkflowFolders(files, sources)` — so a stub that answers `/api/workflows` and
    // leaves this to the catch-all's `json({})` produces `sources: undefined` and an empty list.
    // The row this spec presses Run on simply never existed, and the failure read as a missing
    // element rather than as a page rendering from half its inputs.
    if (path === '/api/sources/workflow') {
      return json({
        defaultRoot: '/tmp/.kontra/workflows',
        sources: [
          {
            id: 'wf-nscheck',
            kind: 'workflow',
            name: 'nscheck.py',
            path: '/tmp/.kontra/workflows/nscheck',
            version: '',
            description: '',
            registeredAt: 0,
          },
        ],
      });
    }
    if (path.startsWith('/api/workflows/file/')) return json({ name: 'nscheck.py', source: SOURCE });
    if (path === '/api/workflows/exposure') return json({ open: false, detail: '' });
    // Fleet history. Answered explicitly because the catch-all below returns `{}`, and a
    // non-array here is a bad shape rather than an empty list — the Runs page reports that as an
    // error in its Fleet panel, which is correct behaviour and pure noise in these tests.
    if (path === '/api/fleet/operations') return json([]);
    if (path === '/api/datasets') return json([]);
    if (path === '/api/actors') return json([]);
    return json({});
  });
}

/** Uncaught exceptions, collected. A surface that throws on mount must be reported as itself
 *  rather than as a locator that timed out thirty seconds later. */
function watchErrors(page: import('@playwright/test').Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (err) => errors.push(err.stack ?? String(err)));
  return errors;
}

test('Run on the Workflows surface lands on the new run, and never shows the previous run verdict', async ({
  page,
}) => {
  const errors = watchErrors(page);
  const NEW = 'nscheck-2026-08-14T12-30-00';
  await stub(page, {
    // The newest KNOWN run is `completed`. Reading its status while the new run has no detail yet
    // is the measured bug: `completed` printed under an id one second old.
    runs: [CRAWL],
    started: { runId: NEW, type: 'NsCheck', queue: 'recon' },
    detail: {
      [CRAWL.runId]: () => detailOf(CRAWL),
      [NEW]: async () => {
        // The two-second gap, made deterministic. Nothing may report on this run until it answers.
        await new Promise((done) => setTimeout(done, 3000));
        return detailOf({ ...SWEEP, runId: NEW }, { execution: 'running' });
      },
    },
  });
  await page.goto('/');

  // WORKFLOWS, not Catalog. Run moved when the two surfaces split: Catalog became an inventory of
  // what exists and Workflows became where you work on one. The hand-off being tested is unchanged
  // and is the whole point — whichever surface starts a run, the id comes with it.
  await expect(async () => {
    await page.getByTestId('nav-workflows').click();
    await expect(page.getByTestId('run-button')).toBeVisible({ timeout: 2000 });
  }).toPass({ timeout: 20_000 });
  await expect(page.getByTestId('workflow-file-nscheck.py')).toBeVisible();

  await page.getByTestId('run-button').click();

  // THE HAND-OFF, AT ITS NEW ADDRESS. This asserted `watching-run` and then that `nav-runs` had
  // become active — and the second half is now wrong ON PURPOSE. Pressing Run used to call
  // `openRun`, which switched the view to Runs; that took the author's code, their input, the
  // worker's pane and the run off screen at the exact moment they wanted all four. The run is
  // watched HERE now, in `RunTail`, and the Runs surface is one deliberate click away. So the
  // invariant is unchanged — the id comes with the hand-off — and what carries it moved.
  await expect(page.getByTestId('run-tail-id')).toHaveText(NEW);
  await expect(page.getByTestId('nav-workflows')).toHaveAttribute('data-active', 'true');

  // THE TRAP, which is what this test is really for and which has not moved at all. In the gap
  // there is no detail for the new run, and whatever is shown then must not be the verdict of the
  // run above it in the list — `completed`, printed under an id one second old, is the measured
  // bug. The honest placeholder used to be the word `starting`; `RunTail` spells it `…`. Either is
  // fine and the assertion below is the one that matters: never a verdict it has not been told.
  await expect(page.getByTestId('run-tail-execution')).not.toHaveText('completed');
  await expect(page.getByTestId('run-tail-execution')).toHaveText('…');

  // …and once the run answers for itself, it says what IT is doing.
  await expect(page.getByTestId('run-tail-execution')).toHaveText('running', { timeout: 15_000 });
  expect(errors).toEqual([]);
});
