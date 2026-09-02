/**
 * The Dashboard in a real browser — run TWICE, against two streamers (ADR 0020, CONTRACT.md 14).
 *
 * WHAT THIS SUITE IS FOR. `pnpm typecheck` and both vitest suites pass on a view that throws on
 * mount, on a WebSocket that never connects, and on a terminal that mounts at 0×0 and paints nothing.
 * Every assertion below is a failure mode only a browser can see.
 *
 * Every spec here runs in both projects, because each answers a question the other cannot:
 *
 *   contract  the stub (`stubStreamer.mjs`) implements RFC 6455 and the tagged framing independently,
 *             so a pass means the BROWSER can read what the wire SPECIFIES — not what one author's
 *             encoder happens to emit.
 *   real      boots `PanelServer` with only the SSH/tmux seam faked, so a pass means slice 1's
 *             hand-rolled `Sec-WebSocket-Accept`, frame encoder, mask handling and close path survive
 *             the strictest client they will ever meet. Nothing in the stub executes that code.
 *
 * A DISAGREEMENT between the two projects is the most valuable result this file can produce: either
 * the stub has drifted from the contract, or the real streamer has a bug its own unit tests share an
 * author with. Neither should be smoothed over.
 *
 * WHAT NEITHER PROVES. No Machine in this environment can be SSHed to, so nothing here exercises
 * `ssh`, `tmux capture-pane`, the `ControlMaster`, or a live attach. The screens are synthetic.
 *
 * Selectors are `data-testid` / `data-*` only. A Tailwind class is a styling decision, and a spec
 * that selects on one turns a restyle into a broken stream.
 */

import { CLEAR_HOME } from '../../backend/src/panels/tmux';
import { PANEL_TOKEN } from './env';
import {
  FIRST_TERMINAL,
  MACHINE_WITH_SESSION,
  MACHINE_WITHOUT_SESSION,
  SCREEN_MARKER,
  terminalIdFor,
} from './fakeFleet';
import { expect, test } from './fixtures';

/**
 * How many times the stub screen's two SHORT lines are on the tile.
 *
 * NOT A LINE COUNT, AND THAT IS A CORRECTION. This used to count non-empty ROWS and compare against
 * `SCREEN_LINES`, which measures the tile's WIDTH as much as the repaint: an xterm wraps, so a tile
 * a few columns narrower turns four lines into six with one screen on it. It went to six the day
 * the nav rail grew from 196 px to 240 px — a change that has nothing to do with whether a snapshot
 * repaints, which is the only thing this test is about.
 *
 * TWO OF THE FOUR, and which two is the load-bearing part. `innerText` joins rows with newlines, so
 * a needle that straddles a wrap boundary is simply not in the string: the stub's two journal lines
 * are ~60 characters and wrap at any realistic tile width, which is exactly how the first attempt at
 * this fix still read `retrying once` as absent. The service line (~37 chars) and the marker line
 * (~36) fit on one row at any width a tile can have, so they are countable — and counting them is a
 * complete statement of the invariant anyway: an appended screen brings its own copy of both.
 */
function screenSignature(text: string): string {
  return ['.service on', SCREEN_MARKER].map((needle) => count(text, needle)).join('/');
}

function count(text: string, needle: string): number {
  return text.split(needle).length - 1;
}

test('mounts the Dashboard without an uncaught exception of its own', async ({ view }, testInfo) => {
  await view.open(); // asserts there was no page error other than the documented one
  await expect(view.tile()).toBeVisible();

  // The exemption is reported, not swallowed: when this stops firing, delete `KNOWN_MOUNT_RACE`.
  for (const err of view.pageErrors) {
    const line = `KNOWN dev-only mount race (StrictMode + xterm dispose): ${err.split('\n')[0]}`;
    testInfo.annotations.push({ type: 'known-issue', description: line });
    // eslint-disable-next-line no-console
    console.log(`  ! ${line}`);
  }
});

test('mounts a Terminal at a real geometry, and reports that geometry to the streamer', async ({
  view,
}) => {
  await view.open();
  const tile = view.tile();
  await expect(tile).toBeVisible();

  // 0×0 is what a tile mounted before layout, or inside a `display:none` parent, measures. It
  // typechecks, renders a rectangle, and shows nothing forever.
  await expect
    .poll(() => tile.getAttribute('data-cols').then(Number), { message: 'cols addon-fit measured' })
    .toBeGreaterThan(20);
  const rows = Number(await tile.getAttribute('data-rows'));
  expect(rows, 'rows addon-fit measured').toBeGreaterThan(5);

  // …and that attribute is not just a claim the page makes about itself: the DOM renderer emits one
  // element per row, so xterm's own geometry has to agree with it. Counted UNDER this tile's rows, not
  // page-wide: slice 4's wall puts N terminals on the page and a page-wide count would be N × rows.
  expect(await view.screen().locator('> div').count()).toBe(rows);

  // The measured size is what a live attach would `stty` before attaching, so the streamer has to
  // receive it — a tile that subscribes 0×0 asks a Machine for a zero-sized screen.
  const subscribe = view.subscribes()[0];
  expect(subscribe?.id).toBe(FIRST_TERMINAL);
  expect(Number(subscribe?.cols), 'cols on the wire').toBeGreaterThan(20);
  expect(Number(subscribe?.rows), 'rows on the wire').toBeGreaterThan(5);
});

test('mints a ticket and reaches OPEN through the 101 handshake', async ({ view, control }) => {
  await view.open();

  // The page's own report of `onopen`: `phase` becomes `streaming`, which the tile publishes.
  await expect(view.tile()).toHaveAttribute('data-mode', 'snapshot');
  await expect(view.page.getByText('snapshots', { exact: true })).toBeVisible();

  const url = view.socketUrls[0] ?? '';
  expect(url, 'the page opened the streamer socket').toContain('/api/panels/ws?ticket=');
  // A URL ends up in access logs, `Referer` headers and proxy buffers. The single-use ticket is what
  // may appear there; the token that mints tickets may not.
  expect(url).not.toContain(PANEL_TOKEN);

  // Chromium surfaces frames only after a 101 whose `Sec-WebSocket-Accept` it verified, so a
  // received `hello` IS the accept-key computation passing a real client.
  await expect
    .poll(() => view.received.filter((frame) => frame.includes('"hello"')).length, {
      message: 'a hello frame per socket',
    })
    .toBeGreaterThanOrEqual(1);
  expect(view.unexpectedSocketErrors()).toEqual([]);

  // In dev the page connects TWICE — `main.tsx` renders under `React.StrictMode`, so the connect
  // effect is mount → cleanup (which closes the socket) → mount. That is not a bug, but it is only
  // sound because a ticket is single-use: every socket had to mint and redeem its own.
  expect(new Set(view.socketUrls).size, 'two sockets sharing one ticket').toBe(
    view.socketUrls.length
  );

  // Single-use: the ticket the page redeemed is gone from the book, and nothing the browser sent
  // tripped the frame decoder. (The stub has no log seam, so that second check is real streamer only —
  // and it is the one that matters there, because the decoder is slice 1's.)
  expect((await control.stats()).ticketsOutstanding).toBe(0);
  expect(control.logs().join('\n')).not.toContain('protocol error');
});

test('repaints a snapshot rather than appending it', async ({ view, control }, testInfo) => {
  await view.open();
  // Scoped to the terminal's rows, not the tile: the tile also carries chrome (a size badge, a "Go
  // live" control), and counting lines on the wrapper measures that chrome as terminal output.
  const screen = view.screen();
  await expect(screen).toContainText(`${SCREEN_MARKER} SCREEN-ONE`);
  // The ANSI a `capture-pane -e` carries was PARSED, not printed: a broken binary path shows the
  // escape bytes as text, which no assertion on the wire alone would catch.
  await expect(screen).not.toContainText('[1;32m');

  // A snapshot frame is a full screen preceded by home+clear, so two different screens must leave
  // only the second one on the tile.
  await control.setLabel('SCREEN-TWO');
  await expect(screen).toContainText(`${SCREEN_MARKER} SCREEN-TWO`);
  await expect(screen).not.toContainText('SCREEN-ONE');

  // Several repaints later, the terminal must still be exactly one screen: one marker, four lines.
  await expect
    .poll(async () => (await control.stats()).paints, { message: 'a few more paints' })
    .toBeGreaterThan(6);

  // POLLED, not read once. A one-shot read of the DOM lands mid-repaint often enough to matter: the
  // rows are briefly EMPTY between `\x1b[H\x1b[2J` and the new screen being rendered, and that read
  // reported "0 markers" — a flake that accuses the repaint of the opposite of its actual bug. The
  // invariant is safe to poll because appending is monotone: once two screens are on the tile the
  // count only grows, so it can never come back to one.
  await expect
    .poll(async () => screenSignature(await screen.innerText()), {
      message: 'exactly one screen on the tile (any count above one ⇒ appending screens)',
    })
    // One service line and one marker: exactly one screen. Two of either is an appended screen.
    .toBe('1/1');

  // And the mechanism itself, on the wire: the payload starts with the clear-home prefix.
  expect(view.payload(0).subarray(0, CLEAR_HOME.length).toString()).toBe(CLEAR_HOME);

  // Through `outputPath`, not a path of my own: Playwright clears `outputDir` between projects, so a
  // hand-written `test-results/dashboard-<project>.png` loses whichever project ran first. This lands
  // in the per-test directory the runner keeps and prints.
  const screenshot = testInfo.outputPath('dashboard.png');
  await view.page.screenshot({ path: screenshot });
  await testInfo.attach('dashboard', { path: screenshot, contentType: 'image/png' });
});

test('a session that goes away becomes words and a converge, not a quiet tile', async ({
  view,
  control,
}) => {
  await view.open();
  const tile = view.tile();
  await expect(view.screen()).toContainText(SCREEN_MARKER);
  await expect(view.emptyTile()).toHaveCount(0);

  // The read path IS the drift detector: the session goes away and the streamer announces it to the
  // subscribed tab. No reload — this is the live `{t:'state'}` path a real `tmux kill-session` takes.
  await control.setSession('absent');

  const empty = view.emptyTile();
  await expect(empty).toBeVisible();
  await expect(empty).toContainText(`no session on ${MACHINE_WITH_SESSION}`);
  await expect(empty).toContainText('converge to create it');
  // The SESSION signal has gone bad, and the tile says so where the tile still speaks: the foot
  // status line leads with the failing signal rather than staying green over an empty pane. (The
  // chip row that used to carry this was removed; `renders unknown health as its own state` covers
  // the full four-signal reading in the drawer.)
  await expect(view.statusLine()).not.toHaveAttribute('data-tone', 'ok');
  await expect(view.statusLine()).toHaveAttribute('data-finding', 'session');
  // The Machine is still a tile. A Machine that is up with no session must never be an absence.
  await expect(tile).toBeVisible();

  // And the affordance works: a masked client frame, decoded by the streamer's own decoder, starts
  // the converge for the Machine the id names — and for no other.
  //
  // The tile's own `converge-<id>` hook when it exists, the page header's button until then. Slice 4
  // moves `onConverge` per-tile and drops the header button (slice 2 amendment 10), and once a wall has
  // several Machines without a session, a query by ACCESSIBLE NAME matches more than one converge — so
  // the id-bearing hook is both the durable selector and the stronger claim.
  const tileConverge = view.page.getByTestId(`converge-${FIRST_TERMINAL}`);
  if (await tileConverge.count()) await tileConverge.click();
  else await view.page.getByRole('button', { name: 'Converge session' }).click();
  await expect
    .poll(async () => (await control.stats()).converges, { message: 'the converge the tile offered' })
    .toEqual([MACHINE_WITH_SESSION]);
});

test('a Machine that never had a session is a tile of its own, not an absence', async ({ view }) => {
  await view.open();
  // The fleet has always had two Machines and the streamer has always served four Terminals; the PAGE
  // pins one until slice 4's grid lands. Skipped rather than deleted, and skipped rather than asserted
  // against a wall that does not exist yet: the day the wall arrives, this starts proving itself.
  //
  // WAIT FOR THE FIRST TILE BEFORE COUNTING. `count()` does not retry, so counting straight after
  // `open()` measured how fast the fleet fetch happened to be: this skipped itself once the suite
  // grew from 18 specs to 23 and the workers got busier, then passed alone. A self-skip that fires
  // on a race reads as a pass and takes a real assertion with it — the one failure mode this file
  // exists to prevent. The guard's INTENT is unchanged: a page that genuinely pins one Terminal
  // shows that one, counts 1, and still skips.
  const wall = view.page.locator('[data-testid^="terminal-fleet:"]');
  await expect(wall.first()).toBeVisible();
  const tiles = await wall.count();
  test.skip(
    tiles < 2,
    `the page pins ONE Terminal today (${tiles} tile); the wall is slice 4 — the streamer already serves all four`
  );

  const id = terminalIdFor(MACHINE_WITHOUT_SESSION, 'actor');
  await expect(view.tile(id)).toBeVisible();
  const empty = view.page.getByTestId(`tile-empty-${id}`);
  await expect(empty).toBeVisible();
  await expect(empty).toContainText(`no session on ${MACHINE_WITHOUT_SESSION}`);
  // Its own converge, carrying its own id: on a wall, a converge that could only be reached by
  // accessible name would be ambiguous between Machines.
  await expect(view.page.getByTestId(`converge-${id}`)).toBeVisible();
});

test('renders unknown health as its own state, never as ok', async ({ view }) => {
  await view.open();

  // ASSERTED IN THE DRAWER, because that is where the signals now live. The tile's chip row was
  // removed — it spent a row of every Worker's output restating `session: present`,
  // `process: unknown` and `n/a: reachable, loads` — and ADR 0020 permits that only on the condition
  // this test checks: the four signals stay INDEPENDENT and separately readable, and `unknown` never
  // renders as healthy. The rule survived the redesign; only its address changed.
  const health = await view.health();

  const lines = (await health.innerText()).split('\n');

  /**
   * The one line that REPORTS a signal — `session: present`, `poller: unknown`.
   *
   * The colon is load-bearing, and this is not fussiness. The drawer opens with an identity table
   * whose rows include `session⇥kontra-webcrawl`, the tmux session's NAME, and a bare word search
   * for `session` finds that first and then asserts against a completely different fact. A health
   * sentence is always `<label>: <value>`; a table row is tab-separated. That tells them apart
   * without depending on where a heading falls in the rendered markdown.
   */
  const reports = (signal: string): string =>
    lines.find((l) => new RegExp(`(^|\\s)${signal}\\s*:`, 'i').test(l)) ?? '';

  // Four independent signals, one sentence each, never collapsed into one light — the round-3
  // incident (81 of 82 loads failing while the run reported `completed`) is what that rule is for.
  // `reachable` is spelled by its transport (`ssh: reachable`, `n/a: …`), so it is found by its
  // value; the other three are named directly.
  for (const signal of ['session', 'poller', 'loads']) {
    expect(reports(signal), `no line reports ${signal}`).not.toBe('');
  }
  // Reachability is labelled by its TRANSPORT (`ssh: reachable`, or `n/a: …` on a local pane), so
  // it is the one signal found by its value rather than its name.
  const reachability = lines.find((l) => /:\s*reachable\b/i.test(l)) ?? '';
  expect(reachability, 'no line reports reachability').not.toBe('');

  // `poller` and `loads` are unmeasured here, and unmeasured must not look healthy. The drawer says
  // so in words rather than by a colour a screenshot cannot assert on.
  expect(reports('poller')).toMatch(/unknown|not measured/i);
  expect(reports('loads')).toMatch(/unknown|not measured/i);

  // ...while a signal that WAS measured reports what it found, and does not borrow the word. This is
  // the collapse `heartbeat.ts` forbids, checked in the direction that actually bit: a healthy-
  // looking line that is really an unmeasured one.
  expect(reachability).not.toMatch(/unknown/i);
  expect(reports('session')).toMatch(/present/i);

  // And on the wall itself, the one health thing still drawn must not read as healthy while an
  // unmeasured signal is the leading finding.
  const tone = await view.statusLine().getAttribute('data-tone');
  expect(tone).not.toBe('ok');
});

test('surfaces the streamer fail-closed 503 as a readable message, not a blank page', async ({
  view,
  control,
}) => {
  // No token configured on the streamer: `auth.ts` fails closed, and every panel route 503s and
  // serves nothing.
  await control.disableToken();
  await view.open();

  const error = view.page.getByTestId('dashboard-error');
  await expect(error).toBeVisible();
  await expect(error).toContainText('503');
  // The reply names the var an operator has to set — the difference between "the Dashboard is
  // switched off" and "the streamer is down", which are two different machines to go and look at.
  await expect(error).toContainText('disabled');
  await expect(error).toContainText('KONTRA_PANEL_TOKEN');

  // Still a page: the chrome, the read-only warning and the placeholder tile are all there.
  await expect(view.page.getByRole('heading', { name: 'Monitor' })).toBeVisible();
  await expect(view.page.getByText('a Terminal is a screen, not a log')).toBeVisible();
  expect(view.unexpectedPageErrors()).toEqual([]);
  // Nothing streamed, and nothing pretended to: no socket was opened and no Terminal tile exists.
  expect(view.socketUrls).toEqual([]);
  await expect(view.page.getByTestId('terminal-pending')).toBeVisible();
});

test('you cannot type into a Terminal: keystrokes put no bytes on the socket', async ({ view }) => {
  await view.open();
  const screen = view.screen();
  await expect(screen).toContainText(SCREEN_MARKER);

  // ADR 0020 finding (3): through a READ-ONLY tmux client, both `run-shell` and `send-keys` executed
  // as root and tmux reported no error. So read-only can never be a property of tmux — it is a
  // property of our surface, and the surface is this socket. The assertion is therefore on the WIRE:
  // xterm always creates its helper textarea (`disableStdin` makes it ignore input rather than
  // removing the element), so counting DOM nodes would prove nothing.
  const beforeTyping = view.sent.length;
  await screen.click();
  await view.page.keyboard.type('rm -rf /opt/kontra\n');
  await view.page.keyboard.press('Control+C');
  await view.page.waitForTimeout(300);

  expect(view.sent.length, 'a keystroke reached the socket').toBe(beforeTyping);
  await expect(screen).not.toContainText('rm -rf');
  // Everything the page ever sent is a control message from the contract, and not one of them can
  // carry bytes bound for a session.
  expect([...new Set(view.sent.map((message) => message.t))]).toEqual(['subscribe']);
  // …and the socket is still healthy, so the emptiness above is not just a dead connection.
  await expect(screen).toContainText(SCREEN_MARKER);
});
