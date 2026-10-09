import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

/**
 * THE FIRST RUN ON A FRESH INSTALL ENDS IN A REPORT, AND THIS IS WHAT SAYS SO.
 *
 * A person who has just installed kontra opens the console, picks the canary, presses Run without
 * typing anything, and reads what came back. Every link of that is asserted here, in a real browser,
 * against an install brought up from this commit's images by CI's `docker` job:
 *
 *   sign in        the console's own form, with the password first boot wrote
 *   run            Workflows → canary → Run, with the form exactly as it is prefilled
 *   complete       the Run closes `completed` inside the budget
 *   report         the run page links to the report, and the report is the CANARY'S, rendered from
 *                  `workspaces/default/workflows/canary/report.md` — not the default one every Run
 *                  falls back to — with the summary, both targets and the query for this run's rows
 *
 * …and CI then checks the Fleet the run held is gone, from the Docker host (see the end of the test).
 *
 * WHY THE TEMPLATE HASH IS THE CENTRAL ASSERTION. A Run whose `report.md` could not be pinned still
 * gets a report — the default one, with a warning. That page is not empty and not red, so a check
 * that only looked for "a report rendered" would pass on exactly the regression it exists to catch.
 * The stored version's `templateHash` is `sha256:` of the template text the Run was pinned to, so
 * comparing it with this checkout's file proves the canary's own template produced the page.
 *
 * WHY THE CONSOLE AND NOT `kontra workflow start`. Only a Run started through `POST /api/runs` has its
 * template pinned at start; the CLI dials Temporal directly and its Runs get the default report (see
 * the header of `control/orchestrator/src/report/sweep.ts`). The console is also what a person uses.
 *
 * The field-level contract between the workflow's return value and the template is held without a
 * cluster by `tests/test_canary_report.py` and `control/orchestrator/src/report/canaryTemplate.test.ts`.
 *
 *     KONTRA_CONSOLE_PASS=… pnpm --filter @kontra/e2e test      # KONTRA_UI defaults to 127.0.0.1:8088
 */

const USER = process.env.KONTRA_CONSOLE_USER ?? 'admin';
const PASS = process.env.KONTRA_CONSOLE_PASS ?? '';

/** A docker-provider canary measured 69–89s on a live install, Fleet included. The ceiling. */
const RUN_BUDGET_MS = Number(process.env.CANARY_BUDGET_MS ?? 5 * 60_000);
/** The report sweep runs every 60s after the Run closes; two passes and some slack. */
const REPORT_BUDGET_MS = 3 * 60_000;

const TEMPLATE_PATH = resolve(__dirname, '../workspaces/default/workflows/canary/report.md');
const TEMPLATE_HASH = `sha256:${createHash('sha256').update(readFileSync(TEMPLATE_PATH, 'utf8'), 'utf8').digest('hex')}`;

interface RunRow {
  runId: string;
  status: string;
  type: string;
  startedAt: number;
  closedAt?: number;
}

interface StoredReport {
  runId: string;
  version: number;
  status: string;
  templateHash: string;
  error?: string;
  snapshot?: { warnings?: string[] };
}

/** One API session for the checks the page does not show. The page signs in separately, through the form. */
async function apiToken(request: APIRequestContext): Promise<string> {
  const res = await request.post('/api/login', { data: { user: USER, password: PASS } });
  expect(res.status(), 'the console password signs in over the API').toBe(200);
  const body = (await res.json()) as { token?: string };
  expect(body.token, 'the login answer carries a session token').toBeTruthy();
  return body.token as string;
}

async function getJson<T>(request: APIRequestContext, token: string, path: string): Promise<{ status: number; body: T }> {
  const res = await request.get(path, { headers: { authorization: `Bearer ${token}` } });
  const text = await res.text();
  let body: unknown = text;
  try {
    body = JSON.parse(text);
  } catch {
    // Not JSON (an export, or an error page); the caller decides whether that matters.
  }
  return { status: res.status(), body: body as T };
}

/** Poll until `probe` returns something, or fail with the last thing it saw. */
async function waitFor<T>(what: string, budgetMs: number, probe: () => Promise<{ done?: T; seen: unknown }>): Promise<T> {
  const deadline = Date.now() + budgetMs;
  let seen: unknown;
  for (;;) {
    const r = await probe();
    if (r.done !== undefined) return r.done;
    seen = r.seen;
    if (Date.now() > deadline) {
      throw new Error(`${what}: not within ${Math.round(budgetMs / 1000)}s; last seen ${JSON.stringify(seen).slice(0, 600)}`);
    }
    await new Promise((r2) => setTimeout(r2, 3000));
  }
}

async function signIn(page: Page): Promise<void> {
  await page.goto('/', { waitUntil: 'domcontentloaded' });
  // WAIT FOR THE FORM rather than probing for it: the Login wrapper mounts after
  // `domcontentloaded`, and a probe that ran first would count zero, skip the sign-in, and turn every
  // later read into a 401 that looks like a broken surface.
  const user = page.locator('input[name="user"]');
  await user.waitFor({ state: 'visible', timeout: 60_000 });
  await user.fill(USER);
  await page.locator('input[name="password"]').fill(PASS);
  await page.locator('button[type="submit"]').click();
  await expect(page.locator('[data-testid^="nav-"]').first()).toBeVisible({ timeout: 30_000 });
}

test('a fresh install runs the canary from the console and renders the canary\'s own report', async ({ page, request }) => {
  test.setTimeout(RUN_BUDGET_MS + REPORT_BUDGET_MS + 3 * 60_000);
  expect(PASS, 'set KONTRA_CONSOLE_PASS — first boot writes it to /var/lib/kontra/console-password').not.toBe('');

  const pageErrors: string[] = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));

  const token = await apiToken(request);

  // ── THE CANARY IS SERVED ──────────────────────────────────────────────────────────────────────
  //
  // A workflow registers itself in the catalog when a worker serves it. `POST /api/runs` refuses a
  // workflow nothing serves, so pressing Run before this would test the refusal, not the canary.
  await waitFor('the canary workflow is registered by a serving worker', 3 * 60_000, async () => {
    const { body } = await getJson<{ registered?: Array<{ name?: string }> }>(request, token, '/api/workflows');
    const names = (body?.registered ?? []).map((w) => String(w.name ?? ''));
    return { done: names.some((n) => n.toLowerCase() === 'canary') ? true : undefined, seen: names };
  });

  // ── RUN IT, AS A PERSON WOULD ─────────────────────────────────────────────────────────────────
  await signIn(page);
  await page.getByTestId('nav-workflows').click();
  await page.getByText('canary', { exact: true }).first().click();
  const runButton = page.getByTestId('run-button');
  await expect(runButton).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('workflow-about')).toContainText('Provisions a Fleet');
  // NOTHING TYPED. Every field has a declared default; a canary that needs input is not a canary.
  await expect(runButton).toBeEnabled();

  const startedAfter = Date.now() - 5_000; // the runner's clock and the API's are the same host's
  await runButton.click();

  const run = await waitFor('the canary run closes', RUN_BUDGET_MS, async () => {
    const { body } = await getJson<RunRow[]>(request, token, '/api/runs?limit=20');
    // BY START TIME, NOT ROW ORDER: `/api/runs` puts open runs first.
    const mine = (Array.isArray(body) ? body : [])
      .filter((r) => r.type === 'Canary' && r.startedAt >= startedAfter)
      .sort((a, b) => b.startedAt - a.startedAt)[0];
    return { done: mine?.closedAt ? mine : undefined, seen: mine ?? body };
  });
  console.log(`canary run ${run.runId}: ${run.status} in ${((run.closedAt! - run.startedAt) / 1000).toFixed(1)}s`);
  expect(run.status, `${run.runId} ended ${run.status}`).toBe('completed');

  // ── THE REPORT EXISTS, AND IT IS THE CANARY'S ─────────────────────────────────────────────────
  const stored = await waitFor('the report is rendered', REPORT_BUDGET_MS, async () => {
    const r = await getJson<StoredReport>(request, token, `/api/runs/${encodeURIComponent(run.runId)}/report`);
    return { done: r.status === 200 ? r.body : undefined, seen: r };
  });
  expect(stored.status, `the report failed to render: ${stored.error ?? ''}`).toBe('ok');
  expect(
    stored.templateHash,
    'the report was rendered from the canary\'s report.md; `default@…` means the template was never pinned'
  ).toBe(TEMPLATE_HASH);
  expect(stored.snapshot?.warnings ?? [], 'the stored report carries no warnings').toEqual([]);

  // ── …AND A PERSON CAN READ IT ─────────────────────────────────────────────────────────────────
  //
  // From the run page, through the link a person clicks, not by typing the address.
  await page.goto(`/runs/${encodeURIComponent(run.runId)}`, { waitUntil: 'domcontentloaded' });
  const toReport = page.getByTestId('run-to-report');
  await expect(toReport).toBeVisible({ timeout: 60_000 });
  await toReport.click();
  await expect(page).toHaveURL(new RegExp(`/reports/${run.runId}`));

  await expect(page.getByTestId('report-status')).toHaveText('rendered');
  const body = page.getByTestId('report-body');
  await expect(body.getByRole('heading', { name: 'canary · completed' })).toBeVisible();
  await expect(body).toContainText('All 2 targets swept: 10 of 10 records landed in canary_signals');
  for (const target of ['alpha', 'beta']) {
    await expect(body.getByRole('row', { name: new RegExp(`^${target}\\s+5\\s+swept$`) })).toBeVisible();
  }
  await expect(body.getByRole('row', { name: /10 of 10/ })).toBeVisible();
  // The query reads back exactly this run's rows: the run id is a value the workflow returned.
  await expect(page.getByTestId('report-block-b1')).toContainText(`where run_id = '${run.runId}'`);
  await page.screenshot({ path: test.info().outputPath('canary-report.png'), fullPage: true });

  // The export is what leaves the console (pasted into a ticket, attached to a mail). Same content.
  const md = await getJson<string>(request, token, `/api/runs/${encodeURIComponent(run.runId)}/report/export?format=md`);
  expect(md.status).toBe(200);
  expect(String(md.body)).toContain('All 2 targets swept');

  // THE FLEET'S TEARDOWN IS ASSERTED BY CI, NOT HERE. No API route lists Machines (`/api/fleet` is
  // a 404, so a check against it passes on an empty error body), and the honest evidence is the
  // Warden's `kf-*` containers on the Docker host — which is where `.github/workflows/ci.yml` looks,
  // in the step after this one.

  // `ResizeObserver loop` is the browser's own throttling notice, not an error in the page.
  expect(pageErrors.filter((e) => !/ResizeObserver loop/.test(e)), 'no uncaught errors in the page').toEqual([]);
});
