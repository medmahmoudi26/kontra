/**
 * The chrome's pure parts: id parsing, the scroll arithmetic, copy extraction, the words on numbers,
 * and the persisted terminal style (slice 7a).
 *
 * WHY THESE AND NOT THE COMPONENTS. Every rule below is one whose failure looks like something else. A
 * wheel boundary that never returns zero looks like a wall that cannot be scrolled; one that always
 * returns zero looks like a tile that cannot be. A copy that keeps trailing blank lines looks like a
 * Machine printing whitespace. A stored style that throws on a future version looks like a Dashboard
 * that will not mount — which is the failure `grid/layout.ts` is emphatic about, and the same rule
 * applies here. The components themselves are proven in the browser suite; these are the parts a
 * screenshot cannot accuse correctly.
 */

import { describe, expect, it } from 'vitest';
import {
  absorbableScroll,
  decayVelocity,
  flickVelocity,
  MAX_VELOCITY_PX_PER_MS,
  MIN_VELOCITY_PX_PER_MS,
  pushSample,
  VELOCITY_SAMPLES,
  wheelPixels,
} from './scroll';
import { visibleText, type BufferLike } from './copy';
import { modeStakes, parseTileRef, tileRefFor } from './tileRef';
import { ageWords, byteWords } from './format';
import {
  clampFontSize,
  DEFAULT_FONT_SIZE,
  DEFAULT_TERMINAL_STYLE,
  MAX_FONT_SIZE,
  MIN_FONT_SIZE,
  paletteBackground,
  paletteIsDark,
  parseTerminalStyle,
  resolvePalette,
  serializeTerminalStyle,
  SIDEBAR_DEFAULT_OPEN,
  TERMINAL_FONT_STACK,
} from '../theme';

// --- ids ------------------------------------------------------------------------------------------

describe('parseTileRef', () => {
  it('takes a well-formed id apart', () => {
    expect(parseTileRef('fleet:kf-crawl-01/kontra-webcrawl/actor')).toEqual({
      mode: 'fleet',
      node: 'kf-crawl-01',
      session: 'kontra-webcrawl',
      window: 'actor',
      wellFormed: true,
    });
  });

  it('never throws, whatever it is handed', () => {
    // The server's parse throws by design — those segments reach `ssh` argv and a `ControlPath`. A
    // browser interpolates them into a React key, so throwing here would only take the tree down.
    for (const id of ['', 'nonsense', ':', 'fleet:', 'fleet:a/b', 'fleet:a/b/c/d', 'fleet:a//c']) {
      expect(() => parseTileRef(id)).not.toThrow();
      expect(parseTileRef(id).wellFormed).toBe(false);
    }
  });

  it('keeps an unknown mode as the word the streamer sent, rather than guessing fleet', () => {
    const ref = parseTileRef('kubernetes:pod-7/sess/actor');
    expect(ref.mode).toBe('kubernetes');
    expect(ref.wellFormed).toBe(true);
  });

  it('never yields an empty label, because an empty tree row is an invisible one', () => {
    const ref = parseTileRef('fleet:a//c');
    expect(ref.session).toBe('?');
  });
});

describe('tileRefFor', () => {
  it('falls back to the wire fields when the id does not parse', () => {
    expect(tileRefFor({ id: 'nonsense', machine: 'kf-crawl-09', window: 'handler' })).toMatchObject({
      node: 'kf-crawl-09',
      window: 'handler',
      wellFormed: false,
    });
  });

  it('prefers the id when it does parse — the id is the only place mode and session exist', () => {
    // `panelsClient.Terminal` has no `mode` and no `session`: the wire kept `machine` for compatibility
    // when slice 6 generalised it, so two of the tree's four levels live only inside the id.
    expect(tileRefFor({ id: 'local:dev-box/kontra-parse/actor', machine: 'ignored', window: 'ignored' })).toEqual({
      mode: 'local',
      node: 'dev-box',
      session: 'kontra-parse',
      window: 'actor',
      wellFormed: true,
    });
  });
});

describe('modeStakes', () => {
  it('says the fleet costs the view and local costs a Worker', () => {
    // ADR 0020 finding (2) plus its Consequences: on the fleet `machine.ts` keeps the actor under systemd
    // and puts only journals in panes; locally the panes hold the real processes.
    expect(modeStakes('fleet').level).toBe('view');
    expect(modeStakes('local').level).toBe('worker');
    expect(modeStakes('docker').level).toBe('worker');
  });

  it('assumes the dangerous answer for a mode it does not know', () => {
    const stakes = modeStakes('kubernetes');
    expect(stakes.level).toBe('unknown');
    expect(stakes.sentence).toContain('assume');
  });
});

// --- the wheel boundary ---------------------------------------------------------------------------

describe('absorbableScroll', () => {
  const box = { scrollTop: 100, scrollHeight: 1000, clientHeight: 200 };

  it('absorbs a delta the scrollback has room for', () => {
    expect(absorbableScroll(box, 60)).toBe(60);
    expect(absorbableScroll(box, -60)).toBe(-60);
  });

  it('clamps a delta to the room that is left rather than choosing one scroller for the gesture', () => {
    // 300 px of momentum against 40 px of scrollback: consume the 40, then chain.
    expect(absorbableScroll({ ...box, scrollTop: 760 }, 300)).toBe(40);
    expect(absorbableScroll({ ...box, scrollTop: 40 }, -300)).toBe(-40);
  });

  it('returns 0 at the boundary, which is what hands the wheel to the wall', () => {
    expect(absorbableScroll({ ...box, scrollTop: 0 }, -50)).toBe(0);
    expect(absorbableScroll({ ...box, scrollTop: 800 }, 50)).toBe(0);
  });

  it('returns 0 for a tile with no scrollback — every snapshot tile, every repaint', () => {
    // `applyFrame` calls `reset()` on each repaint, so a snapshot tile has exactly one screen and nothing
    // behind it. The wheel over it scrolls the wall, with no special case anywhere.
    expect(absorbableScroll({ scrollTop: 0, scrollHeight: 200, clientHeight: 200 }, 60)).toBe(0);
  });

  it('treats a sub-pixel remainder as nothing, so a trackpad cannot deadlock the handoff', () => {
    expect(absorbableScroll({ ...box, scrollTop: 799.6 }, 5)).toBe(0);
  });
});

describe('wheelPixels', () => {
  it('passes pixel deltas through', () => {
    expect(wheelPixels({ deltaY: 53, deltaMode: 0 }, { lineHeight: 16, pageHeight: 200 })).toBe(53);
  });

  it('converts line and page deltas, so a Firefox notch is not three pixels', () => {
    expect(wheelPixels({ deltaY: 3, deltaMode: 1 }, { lineHeight: 16, pageHeight: 200 })).toBe(48);
    expect(wheelPixels({ deltaY: 1, deltaMode: 2 }, { lineHeight: 16, pageHeight: 200 })).toBe(200);
  });

  it('never multiplies by zero, whatever geometry it is handed', () => {
    expect(wheelPixels({ deltaY: 3, deltaMode: 1 }, { lineHeight: 0, pageHeight: 0 })).toBe(3);
  });
});

// --- touch momentum -------------------------------------------------------------------------------

describe('flickVelocity', () => {
  it('is zero without two samples, and for two samples at the same instant', () => {
    expect(flickVelocity([])).toBe(0);
    expect(flickVelocity([{ t: 1, y: 1 }])).toBe(0);
    expect(flickVelocity([{ t: 5, y: 0 }, { t: 5, y: 100 }])).toBe(0);
  });

  it('averages over the window rather than trusting the last pair', () => {
    expect(flickVelocity([{ t: 0, y: 0 }, { t: 10, y: 10 }, { t: 20, y: 20 }])).toBe(1);
  });

  it('clamps, because a fast trackpad flick reports a velocity that would scroll thousands of lines', () => {
    expect(flickVelocity([{ t: 0, y: 0 }, { t: 1, y: 9999 }])).toBe(MAX_VELOCITY_PX_PER_MS);
    expect(flickVelocity([{ t: 0, y: 0 }, { t: 1, y: -9999 }])).toBe(-MAX_VELOCITY_PX_PER_MS);
  });
});

describe('decayVelocity', () => {
  it('decays and then stops at the floor rather than animating a tenth of a pixel forever', () => {
    expect(decayVelocity(1, 0)).toBe(1);
    expect(decayVelocity(1, 100)).toBeLessThan(1);
    expect(decayVelocity(MIN_VELOCITY_PX_PER_MS / 2, 16)).toBe(0);
  });

  it('is exponential in ELAPSED time, so a dropped frame does not lengthen the glide', () => {
    // Two 8 ms frames and one 16 ms frame must land in the same place, or momentum feels different on a
    // loaded box than an idle one.
    const twoFrames = decayVelocity(decayVelocity(2, 8), 8);
    const oneLongFrame = decayVelocity(2, 16);
    expect(Math.abs(twoFrames - oneLongFrame)).toBeLessThan(1e-9);
  });
});

describe('pushSample', () => {
  it('keeps the window bounded', () => {
    const samples: { t: number; y: number }[] = [];
    for (let i = 0; i < 20; i += 1) pushSample(samples, { t: i, y: i });
    expect(samples).toHaveLength(VELOCITY_SAMPLES + 1);
    expect(samples[samples.length - 1]).toEqual({ t: 19, y: 19 });
  });
});

// --- copy -----------------------------------------------------------------------------------------

function buffer(lines: string[], viewportY = 0): BufferLike {
  return {
    viewportY,
    length: lines.length,
    getLine: (i) => (lines[i] === undefined ? undefined : { translateToString: () => lines[i] ?? '' }),
  };
}

describe('visibleText', () => {
  it('takes the rows on screen and no others', () => {
    // Not the scrollback: a Terminal is not a record, and "copy everything" would hand someone 2000 lines
    // that look like a log and are not one.
    const text = visibleText(buffer(['old', 'a', 'b', 'c'], 1), 2);
    expect(text).toBe('a\nb');
  });

  it('drops trailing blank lines but keeps the ones in the middle', () => {
    // `capture-pane` pads a screen to its full height, so a four-line screen in a 50-row pane would
    // otherwise copy as four lines and forty-six empty ones. A gap a Worker printed stays a gap.
    expect(visibleText(buffer(['a', '', 'b', '', '', '']), 6)).toBe('a\n\nb');
  });

  it('is empty for an empty screen rather than a pile of newlines', () => {
    expect(visibleText(buffer(['', '', '']), 3)).toBe('');
  });

  it('stops at the end of the buffer when rows overruns it', () => {
    expect(visibleText(buffer(['a', 'b']), 50)).toBe('a\nb');
  });

  it('renders a missing line as empty rather than throwing', () => {
    const holed: BufferLike = { viewportY: 0, length: 3, getLine: () => undefined };
    expect(visibleText(holed, 3)).toBe('');
  });
});

// --- words ----------------------------------------------------------------------------------------

describe('ageWords', () => {
  it('never says 0s, on a page whose whole subject is staleness', () => {
    expect(ageWords(0)).toBe('just now');
    expect(ageWords(999)).toBe('just now');
  });

  it('coarsens upward', () => {
    expect(ageWords(1000)).toBe('1s ago');
    expect(ageWords(59_000)).toBe('59s ago');
    expect(ageWords(60_000)).toBe('1m ago');
    expect(ageWords(3_600_000)).toBe('1h ago');
    expect(ageWords(86_400_000 * 2)).toBe('2d ago');
  });

  it('distinguishes never from now — the rule `heartbeat.ts` states', () => {
    expect(ageWords(null)).toBe('never');
  });
});

describe('byteWords', () => {
  it('uses binary units, because the cap it reports is written in KiB', () => {
    expect(byteWords(512)).toBe('512 B');
    expect(byteWords(2048)).toBe('2.0 KiB');
    expect(byteWords(65_536)).toBe('64 KiB');
    expect(byteWords(3_145_728)).toBe('3.0 MiB');
  });
});

// --- the persisted style --------------------------------------------------------------------------

describe('parseTerminalStyle', () => {
  it('round-trips', () => {
    const style = { palette: 'night' as const, fontSize: 15, sidebar: true };
    expect(parseTerminalStyle(serializeTerminalStyle(style))).toEqual(style);
  });

  it('starts with the tree CLOSED, because an open one narrows every tile on the wall', () => {
    // Measured, not chosen: the browser suite caught a 248-px sidebar taking a two-column tile to ~66
    // columns and wrapping a 66-character journal line. `theme.ts`'s `SIDEBAR_DEFAULT_OPEN` has the sums.
    expect(SIDEBAR_DEFAULT_OPEN).toBe(false);
    expect(parseTerminalStyle(null).sidebar).toBe(false);
  });

  it('opens the tree only for a stored `true`, never for a truthy value', () => {
    expect(parseTerminalStyle('{"version":1,"palette":"night","fontSize":12,"sidebar":"yes"}').sidebar).toBe(
      false
    );
    expect(parseTerminalStyle('{"version":1,"palette":"night","fontSize":12,"sidebar":1}').sidebar).toBe(false);
    expect(parseTerminalStyle('{"version":1,"palette":"night","fontSize":12,"sidebar":true}').sidebar).toBe(
      true
    );
  });

  it('never throws, for any input at all', () => {
    for (const raw of [null, undefined, '', '   ', 'not json', '[]', '3', 'null', '{"palette":7}']) {
      expect(() => parseTerminalStyle(raw)).not.toThrow();
    }
  });

  it('drops a document from a future version rather than reading it field by field', () => {
    // The rule `grid/layout.ts` states: a build that renamed `fontSize` would otherwise lay the wall out
    // at 9 px and look broken instead of looking reset.
    expect(parseTerminalStyle('{"version":99,"palette":"night","fontSize":20}')).toEqual(
      DEFAULT_TERMINAL_STYLE
    );
  });

  it('falls back per FIELD, so one bad value does not discard a good one', () => {
    expect(parseTerminalStyle('{"version":1,"palette":"nonsense","fontSize":16,"sidebar":true}')).toEqual({
      palette: 'follow',
      fontSize: 16,
      sidebar: true,
    });
  });

  it('clamps a font size hand-edited out of range', () => {
    expect(parseTerminalStyle('{"version":1,"palette":"night","fontSize":400}').fontSize).toBe(MAX_FONT_SIZE);
    expect(parseTerminalStyle('{"version":1,"palette":"night","fontSize":-3}').fontSize).toBe(MIN_FONT_SIZE);
  });
});

describe('clampFontSize', () => {
  it('bounds, rounds, and refuses nonsense', () => {
    expect(clampFontSize(12.4)).toBe(12);
    expect(clampFontSize(1)).toBe(MIN_FONT_SIZE);
    expect(clampFontSize(99)).toBe(MAX_FONT_SIZE);
    expect(clampFontSize(Number.NaN)).toBe(DEFAULT_FONT_SIZE);
    expect(clampFontSize('nonsense')).toBe(DEFAULT_FONT_SIZE);
  });

  it('defaults to the size the density ladder was measured against', () => {
    // `grid/layout.ts`'s row heights assume a 12 px cell; raising the DEFAULT would silently shrink the
    // row count of every saved layout.
    expect(DEFAULT_FONT_SIZE).toBe(12);
  });
});

describe('resolvePalette', () => {
  it('follows the app when the palette is `follow`', () => {
    expect(resolvePalette('follow', true).background).toBe(resolvePalette('night', true).background);
    expect(resolvePalette('follow', false).background).toBe(resolvePalette('paper', true).background);
  });

  it('ignores the app when a palette was chosen explicitly', () => {
    expect(resolvePalette('night', false).background).toBe('#0b0b0e');
    expect(paletteIsDark('night', false)).toBe(true);
    expect(paletteIsDark('paper', true)).toBe(false);
  });

  it('keeps slice 2’s terminal background, so a re-theme is not a re-design', () => {
    expect(paletteBackground('night', true)).toBe('#0b0b0e');
  });

  it('defines all sixteen ANSI colours, not just a background', () => {
    // The old code passed `{ background: '#0b0b0e' }` and let xterm default the sixteen — defaults tuned
    // against a WHITE background. `capture-pane -e` carries the SGR a Worker emitted, so those sixteen are
    // the colours the Fleet actually prints in.
    const theme = resolvePalette('night', true);
    for (const key of [
      'black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
      'brightBlack', 'brightRed', 'brightGreen', 'brightYellow',
      'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite',
    ] as const) {
      expect(theme[key], key).toMatch(/^#[0-9a-f]{6}$/);
    }
  });

  it('does not paint bright white as the paper it sits on', () => {
    // "Bright" on a light background has to mean "more emphatic", not "closer to the paper", or a Machine
    // printing bright white text disappears.
    const paper = resolvePalette('paper', false);
    expect(paper.brightWhite).not.toBe(paper.background);
  });
});

describe('TERMINAL_FONT_STACK', () => {
  it('names the Nerd Font first and ends at the generic keyword', () => {
    // A stack ending at a named family renders PROPORTIONAL text in a terminal if that family is missing,
    // which is unreadable rather than merely plain.
    expect(TERMINAL_FONT_STACK.startsWith("'FiraCode Nerd Font'")).toBe(true);
    expect(TERMINAL_FONT_STACK.trimEnd().endsWith('monospace')).toBe(true);
  });
});
