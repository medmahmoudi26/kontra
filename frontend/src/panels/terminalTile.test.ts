import { describe, expect, it, vi } from 'vitest';

// @xterm/addon-fit ships a UMD bundle whose wrapper reads `self`, and this suite runs in
// `environment: 'node'` (vite.config.ts owns that, and it is not this slice's to change). Importing
// the tile pulls the addon in, so the global it looks for has to exist BEFORE the imports below are
// evaluated — which is what `vi.hoisted` is for.
vi.hoisted(() => {
  (globalThis as unknown as { self?: unknown }).self ??= globalThis;
});


import {
  applyFrame,
  CLEAR_HOME,
  holdsRepaint,
  isRepaint,
  processExited,
  screenHash,
  sessionGone,
  testid,
  type TerminalSink,
} from './TerminalTile';
import { CHIP_SIGNALS } from './HealthChips';
import type { TerminalHealth } from './panelsClient';

/**
 * The one rule a tile cannot get wrong: a snapshot REPAINTS and a live frame APPENDS.
 *
 * The wall paints a full screen every few seconds. Get this wrong and a tile grows a new copy of the
 * screen every tick — which typechecks, renders, and looks like a Worker printing the same thing
 * forever. The distinction is carried by the `\x1b[H\x1b[2J` prefix and nothing else, so it is
 * pinned here rather than asserted in a comment.
 *
 * These are the pure halves of the component. The mount, the WebSocket and the measured geometry are
 * proven in a real browser by the Playwright suite — a green typecheck passes on a terminal that
 * mounts at 0×0.
 */

function bytes(s: string): Uint8Array {
  return Uint8Array.from(s, (c) => c.charCodeAt(0));
}

class FakeTerminal implements TerminalSink {
  readonly ops: string[] = [];
  private screen = '';

  reset(): void {
    this.ops.push('reset');
    this.screen = '';
  }

  write(data: Uint8Array): void {
    this.ops.push('write');
    this.screen += String.fromCharCode(...data);
  }

  /** What the tile is showing, with the control prefixes taken back off. */
  get text(): string {
    return this.screen.split(CLEAR_HOME).join('');
  }

  get resets(): number {
    return this.ops.filter((o) => o === 'reset').length;
  }
}

describe('a snapshot repaints, a live frame appends', () => {
  it('does not accumulate screens across repaints', () => {
    const term = new FakeTerminal();
    // Three snapshot rounds of the same tile, which is twelve seconds of a wall at the default
    // cadence.
    for (let i = 0; i < 3; i += 1) {
      expect(applyFrame(term, bytes(`${CLEAR_HOME}screen ${i}\n`))).toBe('repaint');
    }
    expect(term.resets).toBe(3);
    // One screen, the newest — not three stacked.
    expect(term.text).toBe('screen 2\n');
    expect(term.text).not.toContain('screen 0');
  });

  it('appends a live frame, and never resets on one', () => {
    const term = new FakeTerminal();
    applyFrame(term, bytes(`${CLEAR_HOME}the backlog\n`)); // the seed a focus arrives with
    expect(applyFrame(term, bytes('line one\n'))).toBe('append');
    expect(applyFrame(term, bytes('line two\n'))).toBe('append');
    expect(term.resets).toBe(1);
    expect(term.text).toBe('the backlog\nline one\nline two\n');
  });

  it('goes back to one screen when a live tile is demoted to snapshots', () => {
    // The transition that would otherwise leave a tile in the alternate buffer with a leftover
    // scroll region: a live attach's first bytes were measured to be `\x1b[?1049h` … `\x1b[1;50r`.
    const term = new FakeTerminal();
    applyFrame(term, bytes('\x1b[?1049h\x1b[1;50rlive output\n'));
    applyFrame(term, bytes(`${CLEAR_HOME}the snapshot after the blur\n`));
    expect(term.text).toBe('the snapshot after the blur\n');
    expect(term.resets).toBe(1);
  });

  it('only treats the prefix as a repaint at the very start of a frame', () => {
    expect(isRepaint(bytes(CLEAR_HOME))).toBe(true);
    expect(isRepaint(bytes(`${CLEAR_HOME}anything`))).toBe(true);
    // A journal line that happens to carry the sequence in the middle is not a new screen.
    expect(isRepaint(bytes(`some output ${CLEAR_HOME}`))).toBe(false);
    expect(isRepaint(bytes('\x1b[H'))).toBe(false); // truncated, and shorter than the prefix
    expect(isRepaint(bytes('\x1b[H\x1b[3J'))).toBe(false);
    expect(isRepaint(new Uint8Array())).toBe(false);
    expect(isRepaint(bytes('plain text'))).toBe(false);
  });

  it('writes the frame through unmodified, prefix included', () => {
    // The payload is handed to xterm exactly as the Machine sent it: slicing escape bytes off a
    // frame is how a parser ends up half a sequence behind.
    const term = new FakeTerminal();
    const frame = bytes(`${CLEAR_HOME}\x1b[32mgreen\x1b[m\n`);
    applyFrame(term, frame);
    expect(term.ops).toEqual(['reset', 'write']);
    expect(String.fromCharCode(...frame).startsWith(CLEAR_HOME)).toBe(true);
  });
});

describe('a tile somebody is reading back through', () => {
  /**
   * The complaint this fixes: "when I scroll I am automatically taken to the bottom of the tmux".
   *
   * A snapshot carries 50 lines above the visible screen, so a snapshot tile HAS scrollback and can
   * be scrolled — and then the next snapshot, three seconds later, calls `reset()` and puts the
   * viewport back at the tail. The wall pulled a reader to the bottom faster than they could read a
   * line, which made the scrollback the snapshot goes to the trouble of carrying unreachable.
   */
  const snapshot = bytes(`${CLEAR_HOME}the newest screen\n`);
  const liveFrame = bytes('a line from the attach\n');

  it('holds a repaint while the reader is scrolled back', () => {
    expect(holdsRepaint(snapshot, 12)).toBe(true);
  });

  it('repaints as usual at the tail — which is every tile on a wall nobody is touching', () => {
    expect(holdsRepaint(snapshot, 0)).toBe(false);
  });

  it('never holds a live frame, at any scroll position', () => {
    // An append does not reset, and xterm already leaves a scrolled-up viewport alone when output
    // arrives. Holding these would stall a live attach to fix a problem it does not have.
    expect(holdsRepaint(liveFrame, 12)).toBe(false);
    expect(holdsRepaint(liveFrame, 0)).toBe(false);
  });

  it('leaves the screen the reader is on untouched for as long as they hold it', () => {
    const term = new FakeTerminal();
    applyFrame(term, bytes(`${CLEAR_HOME}the screen being read\n`));
    // Four cadences of the wall while somebody reads.
    for (let i = 0; i < 4; i += 1) {
      if (!holdsRepaint(snapshot, 9)) applyFrame(term, snapshot);
    }
    expect(term.text).toBe('the screen being read\n');
    expect(term.resets).toBe(1);

    // And catches up in ONE cadence when they return, because a snapshot is a whole screen —
    // nothing had to be buffered while it waited.
    if (!holdsRepaint(snapshot, 0)) applyFrame(term, snapshot);
    expect(term.text).toBe('the newest screen\n');
  });
});

describe('the hooks a browser test selects on', () => {
  it('names a tile, its empty state and its live control', () => {
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    expect(testid.tile(id)).toBe('terminal-fleet:kf-crawl-01/kontra-webcrawl/actor');
    expect(testid.empty(id)).toBe('tile-empty-fleet:kf-crawl-01/kontra-webcrawl/actor');
    expect(testid.focus(id)).toBe('focus-fleet:kf-crawl-01/kontra-webcrawl/actor');
  });

  it('does not own a second chip vocabulary', () => {
    // The chips are slice 3's component, hosted by the tile. Two implementations of
    // `chip-<signal>` in one DOM is how a Playwright strict-mode selector starts matching two
    // elements, and how the tri-state quietly forks into two definitions.
    //
    // `exited` is a TILE STATE, not a chip: the session is present, so `empty`'s scrim would be
    // wrong, and the last screen is the Worker's final output rather than something to cover up.
    expect(Object.keys(testid)).toEqual(['tile', 'empty', 'exited', 'focus', 'converge']);
    expect(CHIP_SIGNALS).toEqual(['reachable', 'session', 'process', 'poller', 'loads']);
  });
});

/**
 * The four states a tile can be in, kept apart.
 *
 * `.scratch/instrument-panel/issues/05` asks that a pane which has gone away say so rather than
 * freeze on its last frame, and only a missing SESSION was ever detected. The case that bit us is
 * the opposite shape: the session is present, the tile paints, and the Worker inside it finished —
 * `cli/tmux.go` holds the window open on purpose so the exit status stays readable. Session gone,
 * process exited, running-but-silent and stream-disconnected are four facts with four actions, and
 * this file's job is that no two of them collapse into one.
 */
describe('a pane that has gone away, and a pane whose process has', () => {
  const base: TerminalHealth = {
    reachable: 'ok',
    session: 'present',
    process: 'running',
    poller: 'live',
    loads: 'ok',
  };

  it('does not treat an exited process as a missing session', () => {
    const exited: TerminalHealth = { ...base, process: 'exited' };
    expect(processExited(exited)).toBe(true);
    // NOT gone: the session is there, so the scrim and its "Converge session" button would be the
    // wrong offer — there is nothing to converge, and the last screen is the evidence.
    expect(sessionGone(exited)).toBe(false);
  });

  it('does not treat a missing session as an exited process', () => {
    const absent: TerminalHealth = { ...base, session: 'absent', process: 'unknown' };
    expect(sessionGone(absent)).toBe(true);
    expect(processExited(absent)).toBe(false);
  });

  it('never reads an unmeasured process as either', () => {
    // The common case on a local host: tmux reports the hold shell whether the Worker is running or
    // finished, so the streamer refuses to guess and the tile must not guess on its behalf.
    expect(processExited({ ...base, process: 'unknown' })).toBe(false);
    expect(processExited({ ...base, process: undefined })).toBe(false);
  });
});

/**
 * "Running but silent" is a fact the tile has to derive itself, and this is the derivation.
 *
 * A snapshot arrives every few seconds whether or not the pane printed anything, so frame ARRIVAL
 * cannot answer "is this Worker doing anything". Hashing the screen can: an identical repaint is a
 * pane that printed nothing.
 */
describe('the screen fingerprint', () => {
  it('is equal for identical screens and different for changed ones', () => {
    const a = bytes(`${CLEAR_HOME}line one\nline two\n`);
    const b = bytes(`${CLEAR_HOME}line one\nline two\n`);
    const c = bytes(`${CLEAR_HOME}line one\nline three\n`);
    expect(screenHash(a)).toBe(screenHash(b));
    expect(screenHash(a)).not.toBe(screenHash(c));
    // A one-byte difference has to move it — a length-only check would call a changed screen quiet.
    expect(screenHash(bytes('aaaa'))).not.toBe(screenHash(bytes('aaab')));
  });

  it('is an unsigned 32-bit number, so two runs over the same bytes agree', () => {
    const h = screenHash(bytes('x'.repeat(5000)));
    expect(Number.isInteger(h)).toBe(true);
    expect(h).toBeGreaterThanOrEqual(0);
    expect(h).toBeLessThanOrEqual(0xffffffff);
  });
});
