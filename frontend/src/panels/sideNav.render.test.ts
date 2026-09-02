/**
 * The nav rail, drawn in both of its widths and with each of the five surfaces marked.
 *
 * `renderToStaticMarkup`, still, and now by choice rather than by necessity. Every assertion here
 * is about text and attributes, both of which are in the markup, and a static render of a props-in
 * component is the cheapest way to draw one rail per state — thirty-odd of them in this file.
 *
 * IT DRAWS `NavRail`, NOT `SideNav`, AND THAT IS STILL THE RIGHT SPLIT, though the reason changed.
 * It used to be forced: zustand hands `renderToStaticMarkup` the SERVER snapshot
 * (`getInitialState()`), so a `setState` in a test moved nothing the container read — an earlier
 * version of this file seeded `view` and asserted the active item, which only appeared to pass
 * because `workflows` is also the initial value. With a DOM that is no longer true and the
 * container's own wiring is reachable; what keeps the split is that props-in / markup-out is how
 * every state gets drawn cheaply, including the ones the store cannot produce on demand.
 *
 * WHAT COLLAPSING IS ALLOWED TO COST. Width, the wordmark, the theme toggle and the throughput
 * spark — all of which are recoverable by one click or by visiting a surface. What it must NOT cost
 * is knowing where you are, being able to get anywhere, the one number that is about right now
 * (`live` counts Terminals holding a real PTY attach, and a leaking attach is exactly what you do
 * not find out by navigating), or the pulse — whether anything is happening at all, anywhere.
 *
 * THE PULSE'S FOUR READINGS ARE PINNED HERE AS MARKUP AND IN `chrome/pulse.test.ts` AS VALUES, and
 * both are wanted. That module decides which of idle, running, parked and not-known this is; this
 * file is about whether the rail actually draws it, at both widths, and whether an idle appliance
 * reads as idle rather than as an empty page.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { NavRail, type NavRailProps } from './SideNav';
import { readPulse, type PulseReading } from './chrome/pulse';
import { NAV_COLLAPSED_KEY, readFlag, writeFlag } from './chrome/persistedFlag';
import { SURFACES, type View } from '../state/surfaces';

/** `localStorage` in node, for the flag the container reads. */
function fakeStorage(seed: Record<string, string> = {}): Storage {
  const map = new Map(Object.entries(seed));
  return {
    get length() {
      return map.size;
    },
    clear: () => map.clear(),
    getItem: (k: string) => map.get(k) ?? null,
    key: (i: number) => [...map.keys()][i] ?? null,
    removeItem: (k: string) => void map.delete(k),
    setItem: (k: string, v: string) => void map.set(k, v),
  } as Storage;
}

const IDS: View[] = SURFACES.map((s) => s.id);

const NOW = 1_800_000_000_000;

/** A reading, built the way the container builds one, so these fixtures cannot drift from the
 *  shape `readPulse` actually produces. */
function reading(
  over: Partial<{ running: number; parked: number; named: PulsePark[]; scanned: number; capped: boolean }> = {},
  error: string | null = null
): PulseReading {
  const named = over.named ?? [];
  return readPulse({
    pulse:
      error !== null && over.running === undefined
        ? null
        : {
            running: over.running ?? 0,
            parked: over.parked ?? named.length,
            named,
            scanned: over.scanned ?? over.running ?? 0,
            capped: over.capped ?? false,
            at: NOW,
          },
    error,
    known: [],
    now: NOW,
  });
}

interface PulsePark {
  runId: string;
  workflow: string;
  pending: number;
  since: number;
}

/** The pulse before the first answer has arrived — what every page load starts as. */
const UNREAD: PulseReading = readPulse({ pulse: null, error: null, known: [], now: NOW });
/** Temporal answered, and nothing is running. */
const IDLE: PulseReading = reading();
const PARK: PulsePark = { runId: 'dnssweep-1787', workflow: 'DnsSweep', pending: 2, since: NOW - 240_000 };

/** Nothing counted, nothing measured, and the pulse not yet read — a session that has just loaded. */
const EMPTY: NavRailProps = {
  view: 'workflows',
  counts: { workflows: null, actors: null, datasets: null, monitor: null, settings: null },
  pulse: UNREAD,
  collapsed: false,
  theme: 'dark',
  wall: { panes: 0, live: 0 },
  fleetSeries: [],
  unitsPerSec: 0,
  onView: () => {},
  onOpenRun: () => {},
  onCollapsed: () => {},
  onTheme: () => {},
};

/** Every surface has reported its inventory, two Machines are attached and two runs are going. */
const LOADED: NavRailProps = {
  ...EMPTY,
  counts: { workflows: 3, actors: 2, datasets: 11, monitor: 4, settings: null },
  pulse: reading({ running: 2 }),
  wall: { panes: 4, live: 2 },
  fleetSeries: [0, 41, 118],
  unitsPerSec: 118,
};

function draw(props: Partial<NavRailProps> = {}): string {
  return renderToStaticMarkup(createElement(NavRail, { ...EMPTY, ...props }));
}

describe('the nav rail', () => {
  it('draws every surface at both widths — collapsing never removes a destination', () => {
    for (const collapsed of [false, true]) {
      const html = draw({ ...LOADED, collapsed });
      for (const id of IDS) expect(html, `${id} at collapsed=${collapsed}`).toContain(`data-testid="nav-${id}"`);
    }
  });

  it('draws no retired surface — Runs and Scratch are not destinations any more', () => {
    // Their ADDRESSES still work (they redirect), which is a different thing from being somewhere
    // an operator can click to.
    const html = draw(LOADED);
    expect(html).not.toContain('data-testid="nav-runs"');
    expect(html).not.toContain('data-testid="nav-scratch"');
  });

  it('marks whichever surface is open, one at a time', () => {
    for (const id of IDS) {
      const html = draw({ view: id });
      expect(html, id).toContain(`data-testid="nav-${id}" data-active="true"`);
      // Exactly one. Two marked items is a rail that has stopped answering "where am I".
      expect(html.match(/data-active="true"/g), id).toHaveLength(1);
    }
  });

  it('is narrower collapsed, and says so in the DOM rather than only in CSS', () => {
    expect(draw({ collapsed: true })).toContain('data-collapsed="true"');
    expect(draw({ collapsed: false })).not.toContain('data-collapsed="true"');
  });

  it('drops the labels and the wordmark, which is what the width was being spent on', () => {
    const wide = draw({ collapsed: false });
    const thin = draw({ collapsed: true });
    expect(wide).toContain('kontra');
    expect(wide).toContain('Workflows');
    expect(thin).not.toContain('>kontra<');
    // The label is gone from the button body but survives in the tooltip — see below.
    expect(thin).not.toContain('>Workflows<');
  });

  it('drops the standalone count cells when collapsed — at 48px there is nowhere to put them', () => {
    expect(draw({ collapsed: true })).not.toContain('data-testid="nav-inventory-workflows"');
    expect(draw({ collapsed: false })).toContain('data-testid="nav-inventory-workflows"');
  });

  it('names the surface in the tooltip once the label is gone', () => {
    expect(draw({ collapsed: true })).toContain('Workflows —');
    expect(draw({ collapsed: false })).not.toContain('Workflows —');
  });

  it('keeps `live` when collapsed and drops the spark, because one is about now and one is not', () => {
    const thin = draw({ ...LOADED, collapsed: true });
    expect(thin).toContain('data-testid="nav-count-livepanes"');
    expect(thin).not.toContain('data-testid="fleet-spark"');
    expect(draw({ ...LOADED, collapsed: false })).toContain('data-testid="fleet-spark"');
  });

  it('offers the toggle at both widths — a rail you cannot expand is a rail you cannot use', () => {
    expect(draw({ collapsed: true })).toContain('data-testid="nav-collapse"');
    expect(draw({ collapsed: false })).toContain('data-testid="nav-collapse"');
    expect(draw({ collapsed: true })).toContain('aria-expanded="false"');
    expect(draw({ collapsed: false })).toContain('aria-expanded="true"');
  });
});

describe('an inventory that has not been taken', () => {
  it('prints nothing rather than a zero it has not earned', () => {
    // `null` is NOT COUNTED YET and it is not zero. A surface publishes its own inventory when it
    // first loads; `0 workflows` about a directory nobody has read is false rather than cautious.
    const html = draw(EMPTY);
    expect(html).toContain('data-testid="nav-inventory-workflows"></span>');
  });

  it('prints a zero once somebody has looked, because that is a measurement', () => {
    const html = draw({ ...EMPTY, counts: { ...EMPTY.counts, datasets: 0 } });
    expect(html).toContain('data-testid="nav-inventory-datasets">0</span>');
  });

  it('never counts Settings, because a count of secrets would be about a store that is not there', () => {
    expect(LOADED.counts.settings).toBeNull();
    expect(draw(LOADED)).toContain('data-testid="nav-inventory-settings"></span>');
  });
});

describe('what is running right now, across everything', () => {
  /* THE DEBT RETIRING THE GLOBAL RUNS VIEW LEFT. That surface was the one place that answered "is
     anything happening, anywhere" — a question asked from whichever page an operator is on, which
     is exactly when the answer has to be in the chrome rather than on a surface. Four readings,
     because collapsing any two of them is a lie somebody acts on. */

  it('marks Workflows while anything is in flight, and says how many', () => {
    const html = draw(LOADED);
    expect(html).toContain('data-testid="nav-running"');
    expect(html).toContain('data-value="2"');
  });

  it('reads an IDLE appliance as idle — the word, never a zero', () => {
    // `in flight 0` beside `live panes 0` beside `units/s —` reads as a page that failed to load.
    // An appliance at rest has to say so.
    const html = draw({ pulse: IDLE });
    expect(html).toContain('data-testid="nav-pulse" data-tone="idle"');
    expect(html).toContain('>idle<');
    expect(html).not.toContain('data-testid="nav-running"');
    expect(html).not.toContain('data-testid="nav-parked"');
  });

  it('reads a RUNNING appliance as running, on both marks', () => {
    const html = draw({ pulse: reading({ running: 4 }) });
    expect(html).toContain('data-testid="nav-pulse" data-tone="running"');
    expect(html).toContain('>4 running<');
    expect(html).toContain('data-testid="nav-running"');
    expect(html).not.toContain('data-testid="nav-parked"');
  });

  it('reads a PARKED run as parked, which is not the same as making progress', () => {
    const html = draw({ pulse: reading({ running: 1, named: [PARK] }) });
    expect(html).toContain('data-testid="nav-pulse" data-tone="parked"');
    expect(html).toContain('>1 waiting on you<');
    expect(html).toContain('data-testid="nav-parked"');
    // The tooltip names the run, because the mark has to have somewhere to send you.
    expect(html).toContain('dnssweep-1787');
  });

  it('reads a MIXED fleet as BOTH — a busy fleet must not disappear when one question is asked', () => {
    const html = draw({ pulse: reading({ running: 4, named: [PARK] }) });
    expect(html).toContain('>4 running<');
    expect(html).toContain('>1 waiting on you<');
    // And both dots, below/beside each other rather than one replacing the other.
    expect(html).toContain('data-testid="nav-running"');
    expect(html).toContain('data-testid="nav-parked"');
  });

  it('reads an UNREACHABLE appliance as not known, and never as idle', () => {
    // The worst sentence this rail can produce is a confident all-clear over a cluster nobody can
    // see into.
    const html = draw({ pulse: reading({}, 'read the pulse failed: 502') });
    expect(html).toContain('data-testid="nav-pulse" data-tone="unknown"');
    expect(html).toContain('>not known<');
    expect(html).not.toContain('>idle<');
    // And no green dot over a cluster that could not be asked.
    expect(html).not.toContain('data-testid="nav-running"');
  });

  it('draws nothing at all before the first answer arrives', () => {
    // A rail that flashed a word it was about to correct, on every page load, is a rail nobody
    // reads. `unread` is not `idle`.
    expect(draw(EMPTY)).not.toContain('data-testid="nav-pulse"');
    expect(draw(EMPTY)).not.toContain('>idle<');
  });

  it('survives collapsing, unlike every other number on an item', () => {
    // 48 pixels is precisely the state an operator is in when they have forgotten a sweep is going.
    const thin = draw({ ...LOADED, collapsed: true });
    expect(thin).toContain('data-testid="nav-running"');
    expect(thin).toContain('data-testid="nav-pulse"');
    expect(thin).toContain('data-testid="nav-pulse-running"');
  });

  it('keeps the "waiting on you" mark when collapsed, which is when you forget you are the bottleneck', () => {
    const thin = draw({ pulse: reading({ running: 3, named: [PARK] }), collapsed: true });
    expect(thin).toContain('data-testid="nav-parked"');
    expect(thin).toContain('data-testid="nav-pulse-parked"');
  });

  it('marks only Workflows, whichever surface is open', () => {
    for (const id of IDS) {
      const html = draw({ ...LOADED, view: id });
      expect(html.match(/data-testid="nav-running"/g), id).toHaveLength(1);
    }
  });

  it('is a mark beside the count and not the count itself', () => {
    // `3` workflows, of which some are running, is two facts and has to read as two.
    const html = draw(LOADED);
    expect(html).toContain('data-testid="nav-inventory-workflows">3</span>');
    expect(html).toContain('data-value="2"');
  });
});

describe('the way in', () => {
  /* A COUNT AND A WAY IN, NOT A TABLE. The rail may not become the retired Runs list, so the pulse
     carries a destination rather than rows — and the destination is reached through the Workflows
     surface, which is how every route to a run works now. */

  it('opens THE run when exactly one is waiting on you — there is nothing to choose between', () => {
    const opened: string[] = [];
    const html = renderToStaticMarkup(
      createElement(NavRail, {
        ...EMPTY,
        pulse: reading({ running: 2, named: [PARK] }),
        onOpenRun: (id: string) => opened.push(id),
      })
    );
    expect(html).toContain('data-entry="run"');
    // The reading is what decides; the markup only has to prove the rail asked it.
    expect(reading({ running: 2, named: [PARK] }).entry).toEqual({ kind: 'run', runId: PARK.runId });
    expect(opened).toEqual([]);
  });

  it('sends you to the surface when several are, rather than silently picking one', () => {
    const two = reading({
      running: 3,
      named: [PARK, { runId: 'recon-9', workflow: 'Recon', pending: 1, since: NOW - 10_000 }],
    });
    expect(draw({ pulse: two })).toContain('data-entry="surface"');
  });

  it('offers nowhere to go when there is nothing to open', () => {
    // An idle appliance has nothing to open, and a control that navigates nowhere is worse than a
    // line of text that never claimed it would — so it is not a button at all.
    const html = draw({ pulse: IDLE });
    expect(html).toContain('data-entry="none"');
    expect(html).toMatch(/<div[^>]*data-testid="nav-pulse"/);
  });

  it('never draws a list of runs — the whole point of retiring the surface', () => {
    const busy = reading({ running: 40 });
    const html = draw({ pulse: busy });
    // One number and one destination. No row, no id, nothing to scroll.
    expect(html).toContain('>40 running<');
    expect(html.match(/data-testid="nav-pulse"/g)).toHaveLength(1);
  });
});

describe('the footer', () => {
  it('draws a dash rather than a zero rate before two samples exist', () => {
    // A flat line and a `0` before the first measurement would read as a fleet that is running and
    // writing nothing — the most alarming thing this rail can say, about a page that just loaded.
    // `nav-count-unitss` — the testid strips every non-letter from the label, and `units/s` keeps
    // both of its `s`. Ugly, and asserted as it is rather than as it should be.
    expect(draw(EMPTY)).toContain('data-testid="nav-count-unitss"');
    expect(draw(EMPTY)).toContain('data-value="—"');
  });

  it('draws the measured rate once there is one', () => {
    expect(draw(LOADED)).toContain('data-value="118"');
  });

  it('puts the pulse ABOVE the throughput it explains', () => {
    // `units/s` says how fast rows are landing; the pulse says whether anything is putting them
    // there at all, and a rate over a fleet you have not been told is idle stands on nothing.
    const html = draw(LOADED);
    expect(html.indexOf('data-testid="nav-pulse"')).toBeLessThan(html.indexOf('Fleet throughput'));
    expect(html).toContain('data-testid="nav-count-livepanes"');
  });
});

describe('what the tooltip says', () => {
  /*
   * THESE CAME OFF `chrome/navTitle.ts`, which was a three-line function under nine lines
   * explaining that the suite could not reach it any other way. The rule it holds is real —
   * collapsing costs width, not knowledge — and it is asserted here on the ATTRIBUTE an operator
   * actually meets, which is strictly more than the old tests said: they pinned a return value and
   * never once checked that the rail put it on the button.
   */
  const hint = SURFACES[0]!.hint;

  it('is just the hint when expanded — the label and count are already on screen', () => {
    expect(draw({ ...LOADED, collapsed: false })).toContain(`title="${hint}"`);
  });

  it('carries the label and the count when collapsed', () => {
    expect(draw({ ...LOADED, collapsed: true })).toContain(`title="Workflows (3) — ${hint}"`);
  });

  it('says nothing about a count that has not been taken', () => {
    expect(draw({ ...EMPTY, collapsed: true })).toContain(`title="Workflows — ${hint}"`);
  });

  it('says a zero, which is a measurement rather than a silence', () => {
    const counts = { ...EMPTY.counts, workflows: 0 };
    expect(draw({ ...EMPTY, counts, collapsed: true })).toContain(`title="Workflows (0) — ${hint}"`);
  });
});

describe('remembering the width', () => {
  /* The flag itself is shared with `RunTail`'s fold — the storage guards are subtler than they look
     (access can throw, not only the value), and one copy of them is the point. */

  it('round-trips through storage', () => {
    const store = fakeStorage();
    expect(readFlag(NAV_COLLAPSED_KEY, store)).toBe(false);
    expect(writeFlag(NAV_COLLAPSED_KEY, true, store)).toBe(true);
    expect(readFlag(NAV_COLLAPSED_KEY, store)).toBe(true);
  });

  it('treats a storage that throws as "not collapsed" rather than propagating', () => {
    // A browser with site data blocked throws on ACCESS, not just on the value — which is why the
    // read is guarded too, and why this hook is shared rather than copied per panel.
    const hostile = {
      getItem() {
        throw new Error('blocked');
      },
      setItem() {
        throw new Error('blocked');
      },
    } as unknown as Storage;
    expect(readFlag(NAV_COLLAPSED_KEY, hostile)).toBe(false);
    expect(writeFlag(NAV_COLLAPSED_KEY, true, hostile)).toBe(false);
  });
});
