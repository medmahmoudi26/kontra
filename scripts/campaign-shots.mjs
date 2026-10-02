// campaign-shots.mjs — the screenshots a request-smuggling talk needs, off the live console.
//
//   SHOTS=/tmp/shots RUN=campaign-1790558895 \
//     node --experimental-default-type=module /root/oss/kontra/scripts/campaign-shots.mjs
//
// WHY THIS IS A STANDALONE SCRIPT AND NOT A PLAYWRIGHT SPEC. `pnpm test:e2e` boots an ephemeral
// vite dev server and deliberately points /api at a port nothing listens on, so every spec renders
// against `page.route` stubs — the PNGs it leaves in test-results/ are fixtures (example hosts,
// `sweep-42`), not this install's data. A talk needs the real run, so this drives :8088 directly,
// which is the pattern e2e/canary.mjs already uses.
//
// FOUR THINGS IT WILL NOT DO, each learned the expensive way:
//
//   1. It will not screenshot a historical run id. `/api/runs` discovers runs by their dispatches
//      and Temporal drops the execution on retention, so a deep link to an old run renders the
//      detail with BLANK status chips, "Temporal has dropped this execution" for Input, and
//      "0 of 0 steps done" for Progress. The run must still be in `/api/runs` — so shoot it as
//      soon as it finishes, not next week.
//   2. It will not read the Dataset or Logs regions off a freshly loaded run page. `drawer` starts
//      null and Drawer.svelte renders its children only `{#if open}`, so `run-dataset`, `run-log`
//      and `query-dataset` are NOT IN THE DOM until something clicks `open-dataset`.
//   3. It will not assume a deep-linked query has run. `?q=1` seeds the workbench textarea from
//      `runScopedSql` and stops — there is no auto-execute — so a shot taken on arrival shows the
//      SQL above an empty result pane, which is the opposite of showing the query that produced
//      the numbers. It clicks Run and waits for the row count.
//   4. It will not log in via a cookie or basic auth. The console mints a bearer into
//      localStorage['kontra.session'] from POST /api/login, and localStorage is per-context, so
//      every headless run starts signed out.
import { chromium } from '/root/oss/kontra-console/node_modules/@playwright/test/index.mjs';
import { mkdirSync, writeFileSync } from 'node:fs';

const BASE = process.env.BASE || 'http://localhost:8088';
const SHOTS = process.env.SHOTS || '/tmp/campaign-shots';
const USER = process.env.KONTRA_USER || 'admin';
const PASS = process.env.KONTRA_PASS || '';
const RUN = process.env.RUN || '';
// The datasets this campaign wrote. Named rather than discovered so a shot of "the injection
// points" is a shot of THIS campaign's, not of whatever happens to sort first in the catalog.
const OBS = process.env.OBS || 'observations_c3';
const POINTS = process.env.POINTS || 'injection_points_c3';
const LEADS = process.env.LEADS || 'desync_leads_c3';

if (!PASS) {
  console.error('KONTRA_PASS is required (see /var/lib/kontra/console-password in the api container)');
  process.exit(2);
}
mkdirSync(SHOTS, { recursive: true });

const notes = [];
const note = (name, detail) => {
  notes.push({ name, detail });
  console.log(`  shot  ${name}${detail ? ` — ${detail}` : ''}`);
};

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1680, height: 1150 }, deviceScaleFactor: 2 });
const shot = async (name, detail = '') => {
  await page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: false });
  note(name, detail);
};

// ── SIGN IN ────────────────────────────────────────────────────────────────────────────────────
await page.goto(BASE, { waitUntil: 'domcontentloaded' });
const userBox = page.locator('input[name="user"]');
if (await userBox.count()) {
  await userBox.fill(USER);
  await page.locator('input[name="password"]').fill(PASS);
  await page.locator('button[type="submit"]').click();
}
await page.waitForSelector('[data-testid^="nav-"]', { timeout: 30_000 });

// ── 1 · THE RUN RESULT PAGE ────────────────────────────────────────────────────────────────────
// `/runs/<id>` is the run's own record. The two status chips come from the LIST row, so a run that
// has aged out of /api/runs renders them empty — which is why this checks the list first.
if (RUN) {
  const listed = await page.request.get(`${BASE}/api/runs?limit=100`).then((r) => r.json()).catch(() => []);
  const present = Array.isArray(listed) && listed.some((r) => (r.runId || r.id) === RUN);
  if (!present) {
    console.error(`WARNING: ${RUN} is not in /api/runs — its status chips and Input/Progress will be blank.`);
  }
  // WAIT FOR THE PAYLOAD, NOT FOR THE PAGE. The Input card is painted from
  // `/api/runs/<id>/io`, and that route deserialises the WHOLE argument — for this campaign a
  // 462-program list — so it answers in TENS OF SECONDS while the rest of the run page is up in
  // two. Every field renders "not sent" until it lands, and "not sent" is not a neutral blank: it
  // is the console stating the workflow was started WITHOUT that argument. A shot taken on a fixed
  // settle delay therefore says this campaign ran with no scope, no programs and no fleet size —
  // the exact opposite of what the slide is being shown to prove.
  //
  // Registered BEFORE the navigation that triggers it, or the response is missed on a fast answer.
  const io = page
    .waitForResponse((r) => r.url().includes('/io') && r.status() === 200, { timeout: 180_000 })
    .catch(() => null);
  await page.goto(`${BASE}/runs/${RUN}`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('[data-testid="run-detail"]', { timeout: 60_000 });
  const got = await io;
  if (!got) console.error('  WARNING: /io never answered — the Input card will read "not sent"');
  await page.waitForTimeout(2500); // let the card repaint off the payload

  // AND THEN CHECK, because the wait above is necessary and not sufficient: a 200 that decoded to
  // nothing still paints "not sent". This asserts the rendered result rather than the transport.
  const unset = await page.locator('.fval.unset').count().catch(() => 0);
  const total = await page.locator('.fval').count().catch(() => 0);
  if (total > 0 && unset === total) {
    console.error(`  WARNING: all ${total} Input field(s) still read "not sent" — do not present this shot`);
  } else {
    console.log(`  input  ${total - unset}/${total} field(s) carry a value`);
  }
  await shot('01-run-result', 'the campaign run: input as started, progress, and the output cards');

  // A SECOND FRAME, BECAUSE FIXING THE INPUT CARD MADE IT ENORMOUS. Rendering `programs` in full is
  // the right call for a RECORD — it is the list this campaign actually covered, and truncating it
  // would be the console editing the evidence — but 462 names is a wall of text that pushes the
  // status chips and the entire Progress region below the fold. So the frame that proves the run
  // did something ends up being a picture of an argument. This shoots the phases on their own.
  const prog = page.getByTestId('run-progress');
  if (await prog.count()) {
    await prog.scrollIntoViewIfNeeded().catch(() => {});
    await page.waitForTimeout(900);
    await shot('01b-run-progress', 'the phases the campaign moved through, and what was in flight');
  }

  // ── THE MACHINES, AS MACHINES ──────────────────────────────────────────────────────────────
  //
  // The Infrastructure region: one chassis per Droplet, the Actors placed on each, and what each
  // Worker is doing. It is the only frame in this deck that shows WHAT THE CAMPAIGN RAN ON rather
  // than what it found, and the numbers beside it — machines, regions, $/h — are the ones the run
  // has to be able to defend.
  //
  // IT IS SHOT SEPARATELY AND IT MAY LEGITIMATELY BE ABSENT: the region renders nothing for a run
  // that started no Fleet, and a campaign whose Fleets have been released shows them torn down
  // rather than up. Reported either way instead of failing, because "no rack" is a true statement
  // about some runs and this script must not invent one.
  const rack = page.getByTestId('run-rack');
  if (await rack.count()) {
    await rack.scrollIntoViewIfNeeded().catch(() => {});
    await page.waitForTimeout(900);
    const boxes = await rack.getByTestId('rack-machine').count().catch(() => 0);
    const fleets = await rack.getByTestId('rack-fleet').count().catch(() => 0);
    await rack.screenshot({ path: `${SHOTS}/01c-infrastructure.png` });
    note('01c-infrastructure', `the Machines this run provisioned: ${fleets} Fleet(s), ${boxes} Machine(s), with the Actors placed on each`);
    console.log(`  rack   ${fleets} fleet(s), ${boxes} machine(s)`);
  } else {
    console.log('  rack   absent — this run started no Fleet child');
  }

  // The four numbers, read back off the rendered cards rather than off the API — a slide claims
  // what the page SHOWS, so this records exactly that.
  const fields = {};
  for (const key of ['websites_enumerated', 'requests_sent', 'machines_used', 'fleet_cost_usd',
                     'programs', 'crawl_requests', 'probe_requests', 'injection_points',
                     'exchanges', 'leads_promoted', 'egress_addresses_crlf', 'egress_addresses_cl0']) {
    const el = page.getByTestId(`output-field-${key}`);
    if (await el.count()) fields[key] = (await el.textContent())?.trim().replace(/\s+/g, ' ');
  }
  writeFileSync(`${SHOTS}/run-output.json`, JSON.stringify({ run: RUN, fields }, null, 2));
  console.log(`  read  ${Object.keys(fields).length} output card(s) into run-output.json`);

  // The OUTPUT region alone, which is where the four numbers live.
  const out = page.getByTestId('run-output');
  if (await out.count()) {
    await out.scrollIntoViewIfNeeded();
    await page.waitForTimeout(600);
    await out.screenshot({ path: `${SHOTS}/02-run-output.png` });
    note('02-run-output', 'the four campaign numbers as the run reported them');
  }

  // ── 2 · THE DRAWER: this run's rows, and its log rail ───────────────────────────────────────
  // Not in the DOM until clicked — see the header.
  if (await page.getByTestId('open-dataset').count()) {
    await page.getByTestId('open-dataset').click();
    await page.waitForSelector('[data-testid="run-dataset"]', { timeout: 30_000 }).catch(() => {});
    await page.waitForTimeout(1500);
    await shot('03-run-datasets', "the run's own output rows, in the drawer");
  }
  if (await page.getByTestId('drawer-tab-logs').count()) {
    await page.getByTestId('drawer-tab-logs').click();
    await page.waitForTimeout(1500);
    await shot('04-run-logs', 'the log rail: fleet cost, phases, and anything marked incomplete');
  }
  await page.keyboard.press('Escape').catch(() => {});
}

// ── 3 · THE DATASETS, EACH WITH THE QUERY THAT PRODUCED IT ─────────────────────────────────────
//
// One shot per question a talk actually asks. The SQL is typed rather than deep-linked because
// `?q=1` composes only a `SELECT * … WHERE run_id = …` and these are the reductions that mean
// something to an audience.
const queries = [
  ['05-injection-points', POINTS,
   `SELECT host, point_kind, point_id, point_rationale, seen_count\n` +
   `FROM ${POINTS}\nWHERE is_injection_point\nORDER BY seen_count DESC\nLIMIT 40`,
   'where a payload can go, and why each one counts'],
  ['06-point-kinds', POINTS,
   `SELECT point_kind, count(*) AS points, count(DISTINCT host) AS hosts\n` +
   `FROM ${POINTS}\nWHERE is_injection_point\nGROUP BY 1 ORDER BY points DESC`,
   'the injection surface, by kind of point'],
  ['07-vectors', 'techniques',
   `SELECT class, family, count(*) AS rows\nFROM techniques\nGROUP BY 1, 2 ORDER BY 1, 2`,
   'the payload corpus this campaign sent, by technique family'],
  ['08-requests-by-axis', OBS,
   `SELECT axis, class, count(*) AS observations, sum(requests) AS requests_sent,\n` +
   `       count(DISTINCT host) AS hosts, count(DISTINCT egress) AS source_ips\n` +
   `FROM ${OBS}\nGROUP BY 1, 2 ORDER BY requests_sent DESC`,
   'both techniques, with the traffic and the source addresses each one used'],
  ['09-egress', OBS,
   `SELECT egress AS source_ip, count(*) AS observations, sum(requests) AS requests_sent\n` +
   `FROM ${OBS}\nWHERE egress <> ''\nGROUP BY 1 ORDER BY requests_sent DESC`,
   'the fleet, seen from the traffic rather than from the invoice'],
  ['10-leads', LEADS,
   `SELECT host, endpoint, axis, class, technique, signal_count, is_proof\n` +
   `FROM ${LEADS}\nORDER BY is_proof DESC, signal_count DESC\nLIMIT 30`,
   'what earned a human’s attention'],
];

// HIDE THE WORKBENCH'S DATASET RAIL FOR THESE SHOTS.
//
// `aside.schema` in `Query.svelte` lists everything queryable in the INSTALL — a navigation aid,
// not a result. It ignores the name filter above it, so it keeps listing `http_events_nba-public`
// and `exchanges_nba-public`: a program dropped from scope, whose rows survive because a durable
// Dataset is deliberately not deletable by name and ages out by retention instead. Left visible,
// every query slide carries an excluded program in its margin, and a reader cannot tell that those
// rows predate the exclusion rather than coming from this run.
//
// Hiding a nav rail is framing, not editing: no number, no row and no query result changes, and
// the lake still holds exactly what it held. The alternative — forcing a delete past the guard
// that refuses one — would change the record to make a slide tidier, which is the wrong way round.
// `visibility`, NOT `display`. The rail is a GRID COLUMN: removing it from layout collapses the
// workbench onto the left track, which narrows the SQL box and clips the result table down to its
// first column — a slide showing one column of a five-column answer is a worse lie than the rail
// was. Keeping the box and blanking its contents leaves every other measurement untouched.
const HIDE_RAIL = 'aside.schema ul, aside.schema p { visibility: hidden !important; }';

for (const [name, dataset, sql, detail] of queries) {
  await page.goto(`${BASE}/datasets/${dataset}?q=1`, { waitUntil: 'domcontentloaded' });
  await page.addStyleTag({ content: HIDE_RAIL }).catch(() => {});
  const box = page.locator('[data-testid="dataset-query"] textarea').first();
  // WAIT FOR IT, DO NOT COUNT IT. `domcontentloaded` fires when the document is parsed, which on a
  // client-routed SPA is BEFORE the route's component has mounted — so an immediate `count()` is a
  // race, and it is a race only the FIRST navigation here loses: every later one arrives with the
  // dataset view already warm. The symptom is precisely one skipped shot, always the first query in
  // the list, on a dataset the very next query then reads successfully.
  await box.waitFor({ state: 'visible', timeout: 30_000 }).catch(() => {});
  if (!(await box.count())) {
    console.error(`  skip  ${name} — no query box for ${dataset} (does it exist?)`);
    continue;
  }

  // SCOPE THE LIST ABOVE THE QUERY TO THIS DATASET.
  //
  // The catalog is shared and long-lived: this install holds 46 Datasets, including
  // `http_events_nba-public` left by a campaign that ran BEFORE nba was dropped from scope. The
  // rows are real history and are deliberately NOT deletable by name — a durable Dataset ages out
  // by retention — but a slide is not a catalog listing, and a reader who sees an excluded
  // program in the background of a result reasonably concludes it was scanned in THIS run.
  // Filtering is the honest fix: it narrows the view to what the query below is actually about,
  // rather than editing what the lake holds.
  const filter = page.getByPlaceholder('filter by name or owner').first();
  if (await filter.count()) {
    await filter.fill(dataset);
    await page.waitForTimeout(400);
  }
  await box.click();
  await box.fill(sql);
  // Run it. The deep link seeds the textarea and never executes — see the header.
  await page.keyboard.press('Control+Enter');

  // WAIT FOR THE RESULT, NOT FOR A DURATION. The Run button reads "running…" while the query is in
  // flight, and these queries do not take a predictable time: `injection_points_h1` answered in
  // 56ms at 5,355 rows and was still running past three and a half seconds at 9,159, because the
  // campaign keeps writing to the datasets being queried. A fixed settle therefore degrades as the
  // run grows, and it fails in the worst possible way — an empty result pane under the SQL that
  // was supposed to produce the numbers, which reads as "the query returned nothing".
  await page
    .locator('button', { hasText: /^running/i })
    .waitFor({ state: 'detached', timeout: 120_000 })
    .catch(() => {});
  await page.waitForTimeout(600); // let the rows paint once the button flips back

  // Say what landed, so an empty pane is visible HERE and not discovered on a slide.
  const cells = await page.locator('table tbody tr').count().catch(() => 0);
  if (cells === 0) console.error(`  WARNING: ${name} rendered no result rows`);
  await shot(name, detail);
}

writeFileSync(`${SHOTS}/index.json`, JSON.stringify({ base: BASE, run: RUN, shots: notes }, null, 2));
console.log(`\n${notes.length} screenshot(s) in ${SHOTS}`);
await browser.close();
