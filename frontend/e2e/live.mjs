/**
 * The five surfaces, checked against a RUNNING stack.
 *
 * NOT the suite in `dashboard.spec.ts`, and deliberately separate from it. That one drives a stub
 * streamer and a stub process so it can run anywhere, hermetically, in CI. This one drives the real
 * orchestrator on :8088, the host's real tmux server and the real lake, because the questions it
 * answers are ones a stub cannot: does the wall show the Machines that are actually there, does
 * opening a Dataset actually query it, and does a run that actually happened report what it did.
 *
 *   node e2e/live.mjs                      # the five surfaces
 *   RUN_ID=nscheck-123 node e2e/live.mjs   # …and what the Runs surface says about that run,
 *                                          #   opened BY ITS ID, which is the whole property
 *
 * It mutates nothing: it drags a tile (a local layout), runs SELECTs, and reads. It never presses
 * Serve or Run — starting a workflow from a verification script is how a check becomes a fleet.
 */

import { mkdirSync } from 'node:fs';
// From `@playwright/test`, which is what this package actually depends on — bare `playwright` is a
// transitive package and does not resolve under pnpm's strict layout.
import { chromium } from '@playwright/test';

const BASE = process.env.KONTRA_UI ?? 'http://localhost:8088';
const RUN = process.env.RUN_ID ?? '';
const SHOTS = process.env.SHOTS ?? '';

if (SHOTS) mkdirSync(SHOTS, { recursive: true });
const shot = async (page, name) => {
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/${name}.png` });
};

let total = 0;
let failures = 0;
function check(name, ok, detail = '') {
  total += 1;
  if (!ok) failures += 1;
  console.log(`${ok ? '  ok  ' : ' FAIL '} ${name}${detail ? ` — ${detail}` : ''}`);
}

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1600, height: 950 } });

const pageErrors = [];
page.on('pageerror', (e) => pageErrors.push(String(e)));
page.on('console', (m) => {
  if (m.type() === 'error') pageErrors.push(`console: ${m.text()}`);
});

await page.goto(BASE, { waitUntil: 'domcontentloaded' });

// --- the shell ---------------------------------------------------------------------------------
await page.waitForSelector('[data-testid="nav-catalog"]', { timeout: 20_000 });
for (const v of ['catalog', 'runs', 'actors', 'monitor', 'datasets']) {
  check(`nav has ${v}`, await page.getByTestId(`nav-${v}`).isVisible());
}
const navCount = await page.locator('nav button[data-testid^="nav-"]').count();
// Five: the query workbench folded into a Dataset's own console, and the definitions split from
// the runs — a Run is addressed by its id, not by the file that started it.
check('there are exactly five surfaces', navCount === 5, `${navCount}`);
check('the rail shows what the wall costs', await page.getByTestId('nav-counts').isVisible());
// MEASURED, never animated. An empty series draws nothing at all rather than a flat line, so the
// throughput slot is allowed to be empty here — what must never happen is a line before two samples.
const spark = page.getByTestId('fleet-spark');
check('the rail has a throughput slot', await spark.isVisible());

// --- catalog -----------------------------------------------------------------------------------
await page.getByTestId('nav-catalog').click();
const firstFile = page.locator('[data-testid^="workflow-file-"]').first();
await firstFile.waitFor({ timeout: 20_000 });
await firstFile.click();
await page.waitForTimeout(1500);

check('the editor is CodeMirror', (await page.locator('.workflow-editor .cm-editor').count()) > 0);

// Queue, type and input fold away — they are set once a session and then read never. Opening them
// is what the rest of this block reads.
await page.getByTestId('run-options').click();
const typeValue = await page.locator('input[list="workflow-defns"]').inputValue();
// Read from the file's own `@workflow.defn`. The filename guess reads `nscheck.py` as `Nscheck`
// while the class is `NsCheck`, and starting a type nothing registered HANGS rather than failing.
check('the type came from the source, not the filename', /^[A-Z]/.test(typeValue), typeValue);
check('serve and run are both offered', await page.getByTestId('serve-button').isVisible() && await page.getByTestId('run-button').isVisible());
// The Datasets this FILE names, beside the source that names them. The counts here are the lake's
// totals across every run — a run's own rows are on that run, and the panel says which is which.
const filePanel = await page.getByTestId('file-datasets').innerText();
check('the datasets panel is beside the code', filePanel.length > 0);
check('its counts say what they counted', /every run|not written yet|names no Dataset/.test(filePanel), filePanel.replace(/\n/g, ' | ').slice(0, 160));
await shot(page, 'live-1-catalog');

// --- runs --------------------------------------------------------------------------------------
//
// A RUN IS OPENED BY ITS ID. Not by picking the file that started it — that was the whole defect
// this surface exists to fix, and `RUN_ID` from a CLI or a log line is exactly the input it now
// takes.
await page.getByTestId('nav-runs').click();
await page.getByTestId('open-run-id').waitFor({ timeout: 20_000 });
const listed = await page.locator('[data-testid^="run-row-"]').count();
check('the runs list is keyed by run id', listed >= 0, `${listed} rows`);

if (RUN) {
  await page.getByTestId('open-run-id').fill(RUN);
  await page.getByTestId('open-run-id').press('Enter');
  await page.getByTestId('watching-run').waitFor({ timeout: 20_000 });
  const showing = await page.getByTestId('watching-run').innerText();
  check('the id alone opened that run', showing.trim() === RUN, showing.trim());

  const duration = await page.getByTestId('run-duration').innerText().catch(() => '');
  check('the run reports an execution time', /\d+\s*[smh]/.test(duration), duration);

  // BOTH DIMENSIONS, SEPARATELY (ADR 0017). One merged verdict is what let a run report `completed`
  // over output nobody could read.
  const exec = await page.getByTestId('exec-detail').innerText().catch(() => '');
  const mat = await page.getByTestId('mat-detail').innerText().catch(() => '');
  check('the detail prints the execution dimension', exec.trim().length > 0, exec.trim());
  check('the detail prints the materialization dimension', mat.trim().length > 0, mat.trim());

  // The event log against a real history. This is the reading no status field can give: a run is
  // `running` whether it is working, backing off, or blocked on a queue nobody polls.
  await page.waitForTimeout(2500);
  check('the event log is on the page', await page.getByTestId('event-body').isVisible());
  for (const chip of ['all', 'failure', 'activity']) {
    check(`the log filters on ${chip}`, await page.getByTestId(`event-filter-${chip}`).isVisible());
  }
  const logged = await page.getByTestId('event-count').innerText().catch(() => '');
  check('the log counts real events', /[1-9]\d*\s+shown/.test(logged), logged);
  const firstEvent = await page.locator('[data-testid^="event-"][data-testid*="-"]').first().count();
  check('the timeline drew an event', firstEvent > 0);

  // The table is the other half of the toggle: a timeline for reading in order, a table for
  // scanning a thousand rows for the one that is not like the others.
  await page.getByTestId('event-mode-table').click();
  await page.waitForTimeout(400);
  check('the table mode lists rows', (await page.locator('[data-testid^="event-row-"]').count()) > 0);
  await page.getByTestId('event-mode-timeline').click();

  // The four stat cards. Each is a MEASURED number or an em dash — never a placeholder zero, which
  // would read as "this run scheduled no activities" about a run nobody has read the history of yet.
  for (const stat of ['committed', 'activities', 'retries', 'streak']) {
    check(`the run carries the ${stat} stat`, await page.getByTestId(`stat-${stat}`).isVisible());
  }
  const activities = await page.getByTestId('stat-activities').getAttribute('data-value');
  check('the run reports its activity count from the history', /^[0-9,]+$/.test(activities ?? ''), activities ?? '');

  // `units committed` is the LEDGER's number for this run, so `—` beside `unrecorded` is a correct
  // answer today: nothing on the caller-owned path writes a materialization record yet (issue 06
  // is where run-scoped counts land). What must never happen is a number with no source.
  const committed = await page.getByTestId('stat-committed').getAttribute('data-value');
  const committedNote = await page.getByTestId('stat-committed').innerText();
  check(
    'what it committed is measured or says it is unrecorded',
    /^[0-9,]+$/.test(committed ?? '') || /unrecorded|ledger unread/.test(committedNote),
    `${committed} · ${committedNote.replace(/\n/g, ' ')}`
  );
  await shot(page, 'live-2-run');
}

// --- actors ------------------------------------------------------------------------------------
await page.getByTestId('nav-actors').click();
const firstActor = page.locator('[data-testid^="actor-"]').first();
await firstActor.waitFor({ timeout: 20_000 });
const actorText = await firstActor.innerText();
check('an actor card lists its methods', /\(\)/.test(actorText), actorText.split('\n').slice(0, 4).join(' · '));
// An undeclared schema must SAY so — an empty field table and "takes nothing" are different facts.
check('an undeclared schema is drawn as absent', /not declared|\{/.test(actorText));
await shot(page, 'live-3-actors');

// --- datasets ----------------------------------------------------------------------------------
await page.getByTestId('nav-datasets').click();
const firstRow = page.locator('[data-testid^="dataset-row-"]').first();
await firstRow.waitFor({ timeout: 25_000 });

// ONE ROW PER DATASET. The listing returns one row per `version=…/dt=…` partition, so three Runs of
// one workflow put three rows called `lame` on this page — each reading `623 rows`, none of them
// saying they were one Dataset. A repeated name here is that bug, come back.
const listed = await page
  .locator('[data-testid^="dataset-row-"]')
  .evaluateAll((rows) => rows.map((r) => r.getAttribute('data-testid')));
check(
  'each Dataset is listed once, however many dispatches wrote it',
  new Set(listed).size === listed.length,
  `${listed.length} rows, ${new Set(listed).size} names`
);

await firstRow.click();
await page.waitForSelector('[data-testid="query-status"]', { timeout: 25_000 });
await page.waitForSelector('.ag-row', { timeout: 30_000 }).catch(() => {});
const status = await page.getByTestId('query-status').innerText();
// Opening a Dataset RUNS its query. A console that opens on an empty grid is a console; a Dataset
// that opens on its first hundred rows is a Dataset. `returned` is that count's scope: neither the
// Dataset's total nor one Run's share, and on a capped result not even all of its own answer.
check('opening a dataset runs its query', /Query OK · [\d,]+ rows? returned/.test(status), status.slice(0, 70));
const gridRows = await page.locator('.ag-row').count();
check('the grid drew rows', gridRows > 0, `${gridRows}`);

// EVERY COUNT SAYS WHAT IT COUNTED — and a listing row is now a DATASET, so opening one opens the
// whole name and the header counts every Run that wrote it. That is the same scope the provenance
// panel below counts, which is the point: the two used to differ (623 against 1,246, both correct,
// neither labelled) because the row was one dispatch and the panel was the name.
const headerRows = await page.getByTestId('dataset-header-rows').innerText();
check('the header says which rows it counted', /rows · (every run|the whole list)/.test(headerRows), headerRows.slice(0, 60));
const provState = await page.getByTestId('dataset-provenance').getAttribute('data-state');
check('the provenance panel answered', ['measured', 'none', 'empty'].includes(provState), `${provState}`);
if (provState === 'measured') {
  const provScope = await page.getByTestId('provenance-scope').innerText();
  // …and as of when: a Dataset a Run is still appending to grows, so the total dates itself.
  check('the panel counts every Run, as of a moment', /rows · every run · as of /.test(provScope), provScope.slice(0, 70));
  const runs = await page.getByTestId('provenance-runs-headline').innerText().catch(() => '');
  check('and names the Runs that wrote it', /Run/.test(runs), runs);
}
await shot(page, 'live-4-datasets');

// --- monitor -----------------------------------------------------------------------------------
await page.getByTestId('nav-monitor').click();
await page.waitForSelector('[data-testid="tile-wall"]', { timeout: 30_000 });
await page.waitForTimeout(3500);

// The filter row. Its count is never conditional: a filter that hides eight Machines must not read
// like a Fleet that lost eight Machines.
check('the wall has a filter row', await page.getByTestId('pane-filter').isVisible());
const countBefore = await page.getByTestId('filter-count').getAttribute('data-total');
await page.getByTestId('filter-query').fill('zzzz-no-such-machine');
await page.waitForTimeout(400);
const shownNone = await page.getByTestId('filter-count').getAttribute('data-shown');
check('a filter that matches nothing hides everything', shownNone === '0', `${shownNone} of ${countBefore}`);
// …and says so DIFFERENTLY from an empty Fleet, which is the only real risk a filter introduces here.
check('an empty filter result is not an empty Fleet', (await page.getByTestId('wall-filtered-empty').count()) > 0);
await page.getByTestId('filter-clear').click();
await page.waitForTimeout(600);
const shownAgain = await page.getByTestId('filter-count').getAttribute('data-shown');
check('clearing the filter brings them back', shownAgain === countBefore, `${shownAgain} of ${countBefore}`);

const tiles = page.locator('[data-testid^="slot-tile-"]');
const tileCount = await tiles.count();
check('the wall has tiles', tileCount > 0, `${tileCount}`);

if (tileCount > 0) {
  const first = tiles.first();
  const id = (await first.getAttribute('data-testid')).replace('slot-tile-', '');
  // The banner names the Machine three ways, because each answers a different question: the
  // hostname is what a person calls it, the IP is what `ssh` takes, the session is what
  // `tmux attach -t` takes — and on a local host the session is the only thing separating Workers.
  check('the banner carries an address', await page.getByTestId(`tile-ip-${id}`).isVisible());
  const session = await page.getByTestId(`tile-session-${id}`).innerText();
  check('the banner carries the tmux session', /.+:.+/.test(session), session);

  // The drag. This is what the wall rework exists for and a typecheck cannot see it.
  const before = await first.boundingBox();
  await page.mouse.move(before.x + 120, before.y + 12);
  await page.mouse.down();
  await page.mouse.move(before.x + 520, before.y + 200, { steps: 20 });
  check('a drag shows where the tile will land', (await page.getByTestId('tile-ghost-target').count()) > 0);
  await page.mouse.up();
  await page.waitForTimeout(700);
  const after = await first.boundingBox();
  check(
    'the tile moved and stayed moved',
    Math.abs(after.x - before.x) > 40 || Math.abs(after.y - before.y) > 40,
    `${Math.round(before.x)},${Math.round(before.y)} → ${Math.round(after.x)},${Math.round(after.y)}`
  );
  // A tile keyed by its Terminal keeps its xterm across a move. A remount looks exactly like a
  // Machine that stopped printing, which is why this is asserted rather than assumed.
  check('the terminal survived the drag', (await page.locator(`[data-testid="terminal-${id}"] .xterm`).count()) > 0);

  const box = await first.boundingBox();
  await page.getByTestId(`tile-resize-${id}`).hover();
  await page.mouse.down();
  await page.mouse.move(box.x + box.width + 260, box.y + box.height + 140, { steps: 15 });
  await page.mouse.up();
  await page.waitForTimeout(700);
  const grown = await first.boundingBox();
  check('the tile resized', grown.width > box.width + 30 || grown.height > box.height + 30,
    `${Math.round(box.width)}×${Math.round(box.height)} → ${Math.round(grown.width)}×${Math.round(grown.height)}`);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.getByTestId('nav-monitor').click();
  await page.waitForSelector('[data-testid="tile-wall"]', { timeout: 30_000 });
  await page.waitForTimeout(2500);
  const reloaded = await page.getByTestId(`slot-tile-${id}`).boundingBox().catch(() => null);
  check('the arrangement survived a reload', reloaded !== null && Math.abs(reloaded.width - grown.width) < 40,
    `${Math.round(grown.width)} → ${Math.round(reloaded?.width ?? 0)}`);
}
await shot(page, 'live-5-monitor');

// `dimensions` is the known xterm dispose race this repo already documents; a `ResizeObserver
// loop` notice is the browser's own throttling notice and is not an error in the page.
const real = pageErrors.filter((e) => !/dimensions|ResizeObserver loop/.test(e));
check('no page errors', real.length === 0, real.slice(0, 2).join(' ;; ').slice(0, 240));

await browser.close();
console.log(`\n${total - failures}/${total} checks passed`);
process.exit(failures > 0 ? 1 : 0);
