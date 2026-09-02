/**
 * The one check that presses the buttons: Serve, Run, and watch a real run fill the surface.
 *
 * SEPARATE FROM `live.mjs` ON PURPOSE, and the separation is a safety property rather than tidiness.
 * That script reads: it drags a tile, runs SELECTs, and never mutates anything, so it is safe to run
 * at any time against any stack. This one starts a caller workflow, and the workflow it is pointed
 * at provisions Droplets — so it refuses to run without `KONTRA_RUN_E2E=yes` in the environment. A
 * verification script that can spend money by being run out of habit is a footgun with a green tick
 * on it.
 *
 *   KONTRA_RUN_E2E=yes node e2e/run.mjs
 *   KONTRA_RUN_E2E=yes WORKFLOW=nscheck.py QUEUE=recon INPUT='{"machines":2}' node e2e/run.mjs
 *
 * WHAT IT IS ACTUALLY CHECKING is the thing no unit test can: that pressing Run on the CATALOG hands
 * you that run's detail on the RUNS surface, and that the four readings of it there — the stats, the
 * rails, the output and Temporal's event log — all come alive against a run that is really
 * happening, and agree with each other at the end.
 */

import { mkdirSync } from 'node:fs';
import { chromium } from '@playwright/test';

if (process.env.KONTRA_RUN_E2E !== 'yes') {
  console.error(
    'refusing to start a workflow: set KONTRA_RUN_E2E=yes.\n' +
      'This script presses Run, and the workflow it drives provisions Machines.'
  );
  process.exit(2);
}

const BASE = process.env.KONTRA_UI ?? 'http://localhost:8088';
const WORKFLOW = process.env.WORKFLOW ?? 'nscheck.py';
const QUEUE = process.env.QUEUE ?? 'recon';
const INPUT = process.env.INPUT ?? '{"dataset": "domains", "into": "lame", "machines": 4}';
const SHOTS = process.env.SHOTS ?? '';
/** How long to give the whole run. A four-Machine nscheck sweep measured ~7 minutes of real
 *  work on top of a provisioning phase of two to three; twenty-five is generous without being a
 *  script that hangs a terminal all afternoon when something is wrong. */
const DEADLINE_MS = Number(process.env.DEADLINE_MS ?? 25 * 60 * 1000);

if (SHOTS) mkdirSync(SHOTS, { recursive: true });
const shot = async (page, name) => {
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: false });
};

let total = 0;
let failures = 0;
function check(name, ok, detail = '') {
  total += 1;
  if (!ok) failures += 1;
  console.log(`${ok ? '  ok  ' : ' FAIL '} ${name}${detail ? ` — ${detail}` : ''}`);
}
const stamp = () => new Date().toISOString().slice(11, 19);
const say = (line) => console.log(`  ··   ${stamp()} ${line}`);

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1600, height: 950 } });
const pageErrors = [];
page.on('pageerror', (e) => pageErrors.push(String(e)));
page.on('console', (m) => {
  if (m.type() === 'error') pageErrors.push(`console: ${m.text()}`);
});

await page.goto(BASE, { waitUntil: 'domcontentloaded' });
await page.getByTestId('nav-catalog').click();
await page.getByTestId(`workflow-file-${WORKFLOW}`).click();
await page.waitForTimeout(1200);

// --- the options, then Serve --------------------------------------------------------------------
await page.getByTestId('run-options').click();
// The Type field is the one with the datalist; Queue is the other. Selected that way rather than by
// order, because the two are one tab apart and filling the wrong one starts a workflow type nothing
// has registered — which does not fail, it HANGS.
await page.locator('input:not([list])').first().fill(QUEUE);
await page.locator('textarea').first().fill(INPUT);
await page.waitForTimeout(200);

say(`serving ${WORKFLOW} on ${QUEUE}`);
await page.getByTestId('serve-button').click();
await Promise.race([
  page.getByTestId('serve-result').waitFor({ timeout: 60_000 }),
  page.getByTestId('workflow-error').waitFor({ timeout: 60_000 }),
]);

// A QUEUE THAT IS ALREADY SERVED IS NOT A FAILURE OF THIS CHECK. `kontra workflow serve` refuses to
// start a second worker into an existing tmux session, by name, and that refusal is correct — but a
// verification script that treated it as a failure would be red for the ordinary case of running
// twice in one session. What matters afterwards is only whether the queue has a poller.
const served = (await page.getByTestId('serve-result').count()) > 0;
const serveErr = served ? '' : await page.getByTestId('workflow-error').innerText();
const already = /already exists/.test(serveErr);
check(
  'Serve either starts a worker or says the session is already there',
  served || already,
  served ? (await page.getByTestId('serve-result').innerText()).trim() : serveErr.slice(0, 160)
);

if (served) {
  // The link back to the wall. Serving is what makes a workflow watchable, and the button hands the
  // link over — the streamer discovers `kontra-*` on its own cadence, so this is a wait, not a read.
  await page
    .getByTestId('watch-on-wall')
    .waitFor({ timeout: 90_000 })
    .catch(() => {});
  check(
    'Serve hands back a link to the pane on the wall',
    (await page.getByTestId('watch-on-wall').count()) > 0
  );
}
await shot(page, 'run-1-served');

// A worker has to be POLLING before Run means anything: a start against a queue nobody serves does
// not fail, it waits — which is the exact failure this surface exists to make visible.
say('waiting for the worker to poll the queue');
await page.waitForTimeout(12_000);

// --- Run ----------------------------------------------------------------------------------------
//
// Pressing Run hands over to the RUNS surface, on that run's own detail: `startRun` answers with the
// caller's workflow id, which IS the run id, and the id is the whole of what the detail needs. The
// header printing it is therefore both the id and the proof that the hand-off happened.
await page.getByTestId('run-button').click();
await page.getByTestId('watching-run').waitFor({ timeout: 60_000 });
const runId = (await page.getByTestId('watching-run').innerText()).trim();
check('Run hands over to that run detail, by id', runId.length > 0, runId);
check(
  'the hand-off changed surface as well as selection',
  (await page.getByTestId('nav-runs').getAttribute('data-active')) === 'true'
);
say(`started ${runId}`);

// --- watch ----------------------------------------------------------------------------------------
//
// The provisioning window is the interesting one, and it is where this surface used to say "no run
// yet" for minutes about a run the operator had just started: `/api/runs` discovers runs by their
// DISPATCHES, so a run that has not dispatched is not in that list. The event log is the only
// reading that is alive in that window, which is why it is the first thing asserted.
const started = Date.now();
let sawEvents = false;
let sawRails = false;
let sawRows = 0;
let settledState = '';

while (Date.now() - started < DEADLINE_MS) {
  // Belt and braces on top of the header fix: only honour a settle for the run we started. A page
  // showing a different run's state is exactly the class of mistake that made this loop exit before
  // it had watched anything.
  const showing = await page.getByTestId('watching-run').innerText().catch(() => '');
  const [count, rails, state] = await Promise.all([
    page.getByTestId('event-count').innerText().catch(() => ''),
    page.getByTestId('rails').count().catch(() => 0),
    // The DETAIL's pill, which is this run's. Every list row carries its own `exec-<runId>` — a
    // shared hook here once read a neighbouring workflow's last status and broke the watch loop on
    // its first iteration, before any telemetry had arrived.
    page.getByTestId('run-state').innerText().catch(() => ''),
  ]);
  const events = Number((count.match(/([\d,]+)\s+shown/) ?? [])[1]?.replace(/,/g, '') ?? 0);
  if (!sawEvents && events > 0) {
    sawEvents = true;
    say(`the event log is live (${events} events) — this is the provisioning window`);
    await shot(page, 'run-2-provisioning');
  }
  if (!sawRails && rails > 0) {
    sawRails = true;
    say('per-Machine rails are beating — Batches are in flight');
    await shot(page, 'run-3-inflight');
  }

  // `units committed` is the LEDGER's number for this run, and `—` is what it says while nothing
  // has recorded one — which is every caller-published Dataset today. Read as 0 for progress
  // logging; it is asserted properly below, where "unrecorded" is an allowed answer and a
  // fabricated number is not.
  const committed =
    Number(
      ((await page.getByTestId('stat-committed').getAttribute('data-value')) ?? '0').replace(/,/g, '')
    ) || 0;
  if (committed > sawRows) {
    sawRows = committed;
    say(`${committed.toLocaleString()} units committed · ${events} events`);
  }

  if (showing.trim() === runId && /completed|failed|output failed|cancelled/i.test(state)) {
    settledState = state.trim();
    break;
  }
  await page.waitForTimeout(5000);
}

check('the event log came alive during the run', sawEvents);
check('the rails showed per-Machine progress', sawRails);
check('the run settled inside the deadline', settledState !== '', settledState || 'still running');
check('the run completed', /completed/i.test(settledState), settledState);

await page.waitForTimeout(3000);
await shot(page, 'run-4-settled');

// --- what it says afterwards ----------------------------------------------------------------------
const duration = await page.getByTestId('run-duration').innerText().catch(() => '');
check('the surface reports an execution time', /\d+\s*[smh]/.test(duration), duration);

// WHAT IT COMMITTED, OR THAT NOTHING RECORDED IT — and never a number with no source. The card
// reads the run's own materialization ledger, which is the only run-scoped account of output there
// is; nothing on the caller-owned path writes a record to it yet, so `unrecorded` is the honest
// answer today and the one this check accepts. Issue 06 is where a run-scoped count lands, and this
// is the line that tightens when it does.
const committed = (await page.getByTestId('stat-committed').getAttribute('data-value')) ?? '';
const committedNote = await page.getByTestId('stat-committed').innerText();
check(
  'it says what it committed, or says nothing recorded it',
  Number(committed.replace(/,/g, '')) > 0 || /unrecorded|ledger unread/.test(committedNote),
  `${committed} · ${committedNote.replace(/\n/g, ' ')}`
);

const activities = (await page.getByTestId('stat-activities').getAttribute('data-value')) ?? '';
check('the history counted the activities it scheduled', Number(activities.replace(/,/g, '')) > 0, activities);

// A run that finished having committed nothing is the failure that is invisible everywhere else —
// Temporal says `completed`, the duration looks normal, and the Dataset is simply absent. It is
// SAID on this surface, so its absence here is itself a check. Gated on the ledger having records:
// with none, the panel says the ledger is empty rather than accusing the run.
check(
  'it did not finish empty',
  (await page.getByTestId('wrote-nothing').count()) === 0,
  (await page.getByTestId('wrote-nothing').count()) > 0 ? 'the run committed no rows' : ''
);

const outputs = await page.getByTestId('run-output').innerText();
check(
  'the output panel says what this run recorded',
  /rows|no record/.test(outputs),
  outputs.replace(/\n/g, ' | ').slice(0, 160)
);

// The run is on the RUNS list now, by its id — it has dispatched, which is how that list discovers
// one. Its row carries both dimensions, separately.
check('the runs list records this run', (await page.getByTestId(`run-row-${runId}`).count()) > 0, runId);
const rowExec = await page.getByTestId(`exec-${runId}`).innerText().catch(() => '');
const rowMat = await page.getByTestId(`mat-${runId}`).innerText().catch(() => '');
check('its row prints both dimensions, not one verdict', rowExec.length > 0 && rowMat.length > 0, `${rowExec} · ${rowMat}`);

// …and the definition's own record still names it, on the Catalog, beside the source.
await page.getByTestId('nav-catalog').click();
await page.waitForTimeout(1200);
const history = await page.getByTestId('run-history').innerText().catch(() => '');
check('the definition run history records this run', history.includes(runId), runId);

// The 400 from a queue that was already served is this script's own doing and is asserted above.
const real = pageErrors.filter(
  (e) => !/dimensions|ResizeObserver loop/.test(e) && !(already && /400/.test(e))
);
check('no page errors', real.length === 0, real.slice(0, 2).join(' ;; ').slice(0, 240));

await browser.close();
console.log(`\nrun ${runId} — ${settledState || 'unsettled'}`);
console.log(`${total - failures}/${total} checks passed`);
process.exit(failures > 0 ? 1 : 0);
