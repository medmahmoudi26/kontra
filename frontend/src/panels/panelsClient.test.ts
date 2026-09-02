import { describe, expect, it } from 'vitest';
import {
  decodeTagged,
  healthEntries,
  healthLines,
  inapplicableReason,
  isHealthy,
  PANEL_PORT,
  panelSocketUrl,
  resolvePanelBase,
  signalApplies,
  type TerminalHealth,
} from './panelsClient';

const UNKNOWN: TerminalHealth = {
  reachable: 'unknown',
  session: 'unknown',
  poller: 'unknown',
  loads: 'unknown',
};

describe('where the streamer is', () => {
  it('takes VITE_KONTRA_PANEL_BASE when it is set', () => {
    // Injected, never inferred: the SPA and the streamer are two origins on purpose, so a
    // deployment can put the streamer anywhere.
    expect(resolvePanelBase({ VITE_KONTRA_PANEL_BASE: 'https://panels.example:9443/' })).toBe(
      'https://panels.example:9443'
    );
  });

  it('otherwise follows the page’s own host, not localhost', () => {
    // A Controller reached over its VPC address must not have its Dashboard point at the
    // operator's laptop.
    expect(resolvePanelBase({}, { protocol: 'http:', hostname: '10.124.0.2' })).toBe(
      `http://10.124.0.2:${PANEL_PORT}`
    );
    expect(resolvePanelBase({}, { protocol: 'https:', hostname: 'ctrl.example' })).toBe(
      `https://ctrl.example:${PANEL_PORT}`
    );
    expect(resolvePanelBase({ VITE_KONTRA_PANEL_BASE: '  ' }, { protocol: 'http:', hostname: 'h' })).toBe(
      `http://h:${PANEL_PORT}`
    );
  });

  it('carries the ticket — never a token — into the socket URL, and follows the scheme', () => {
    const url = panelSocketUrl('http://localhost:8090', 'tk/+ 1=');
    expect(url).toBe('ws://localhost:8090/api/panels/ws?ticket=tk%2F%2B%201%3D');
    expect(panelSocketUrl('https://panels.example', 'abc')).toBe(
      'wss://panels.example/api/panels/ws?ticket=abc'
    );
    // A ticket is single-use and ~30s precisely because a URL lands in access logs and proxy
    // buffers; a token there would be a durable credential in every one of them.
    expect(url).not.toMatch(/token/i);
  });
});

describe('the tagged binary frame', () => {
  it('decodes [idLen][id][payload]', () => {
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    const idBytes = new TextEncoder().encode(id);
    const payload = new TextEncoder().encode('\x1b[H\x1b[2Jhello');
    const buf = new Uint8Array(1 + idBytes.length + payload.length);
    buf[0] = idBytes.length;
    buf.set(idBytes, 1);
    buf.set(payload, 1 + idBytes.length);

    const decoded = decodeTagged(buf.buffer);
    expect(decoded.id).toBe(id);
    // The clear-home prefix is what makes a repaint replace the tile instead of appending to it.
    expect(new TextDecoder().decode(decoded.payload)).toBe('\x1b[H\x1b[2Jhello');
  });

  it('refuses a malformed frame rather than writing garbage to a tile', () => {
    expect(() => decodeTagged(new Uint8Array([0]).buffer)).toThrow(/malformed/);
    expect(() => decodeTagged(new Uint8Array([9, 1, 2]).buffer)).toThrow(/malformed/);
  });
});

describe('health wording', () => {
  it('never presents unknown as healthy', () => {
    // The rule `heartbeat.ts` establishes: "unknown" and "zero" have to stay distinguishable.
    expect(isHealthy(UNKNOWN)).toBe(false);
    for (const line of healthLines(UNKNOWN)) expect(line).toMatch(/unknown/);
  });

  it('gives each failing signal its own words and its own action', () => {
    const lines = healthLines({
      reachable: 'ok',
      session: 'absent',
      process: 'exited',
      poller: 'none',
      loads: 'failing',
      detail: 'kf-crawl-01 is up but has no session kontra-webcrawl — converge to create it',
    });
    // Five independent signals, never collapsed into one light — the arrangement that catches the
    // round-3 failure (81 of 82 resource loads failing while the run reported completed), plus the
    // pane's own process, which a present session hides.
    expect(lines).toHaveLength(6);
    expect(lines.join('\n')).toContain('session: ABSENT — converge');
    expect(lines.join('\n')).toContain('process: EXITED');
    expect(lines.join('\n')).toContain('poller: NONE');
    expect(lines.join('\n')).toContain('loads: FAILING');
    expect(lines[5]).toContain('kf-crawl-01');
  });

  /**
   * `process` IS NOT IN `isHealthy`, and that is deliberate rather than an oversight.
   *
   * It is `unknown` for a perfectly healthy local Worker by construction — tmux reports kontra's
   * hold shell whether the Worker is running or finished — so folding it into the rollup would make
   * the Actors page count a serving fleet as zero serving. The signal is shown per tile, where
   * "unknown" can carry its reason, and never rolled into a number.
   */
  it('does not let an unmeasurable pane process drag every Machine out of "serving"', () => {
    const serving = { reachable: 'ok', session: 'present', process: 'unknown', loads: 'ok' } as const;
    expect(isHealthy({ ...serving, poller: 'live' })).toBe(true);
  });

  it('is healthy only when all four are measured and good', () => {
    expect(isHealthy({ reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' })).toBe(true);
    expect(isHealthy({ reachable: 'ok', session: 'present', poller: 'unknown', loads: 'ok' })).toBe(
      false
    );
    expect(isHealthy({ reachable: 'ok', session: 'no-tmux', poller: 'live', loads: 'ok' })).toBe(false);
  });
});

/**
 * A LOCAL ACTOR IS NOT "0 SERVING".
 *
 * `isHealthy` is what `ActorsPage` and `ActorCard` count "serving" with, and it required
 * `loads === 'ok'` — a reading that comes from a vmagent only `infra/programs/machine.ts` installs,
 * on FLEET Machines and nowhere else. No compose service, no `kontra serve` and no worker image
 * scrapes :9110, so a `local` or `docker` node could never satisfy the condition on any installation
 * that has ever existed, and a perfectly healthy local actor was reported as **0 serving** forever.
 *
 * Same defect as the chip row's, one surface later: an axis that cannot apply to a mode, treated as a
 * failure. Same fix, and — this is the part that matters — the same `signalApplies`, because two
 * predicates disagreeing about what "healthy" means is worse than the bug.
 */
describe('serving, read in the pane’s own mode', () => {
  /** A local Worker that is up and being polled: everything measurable about it is good. */
  const LOCAL_SERVING: TerminalHealth = {
    reachable: 'ok',
    session: 'present',
    process: 'unknown',
    poller: 'live',
    loads: 'unknown',
  };

  it('counts a local Worker that is up and polled', () => {
    // Was `false`, on every local and docker node, permanently — `ActorCard`'s dot was rose and its
    // count read `0 serving` beside a Worker answering calls.
    expect(isHealthy(LOCAL_SERVING, 'local')).toBe(true);
    expect(isHealthy(LOCAL_SERVING, 'docker')).toBe(true);
    // …and the fleet reading is unchanged: there `loads` is real and `unknown` is still not healthy.
    expect(isHealthy(LOCAL_SERVING, 'fleet')).toBe(false);
  });

  it('DEFAULTS to the strict reading, so a caller that forgets the mode cannot loosen a count', () => {
    expect(isHealthy(LOCAL_SERVING)).toBe(false);
    // An unrecognised mode — a streamer from a later build — is treated as fleet for the same
    // reason the chips are: omission must never be what this build does when it does not know.
    expect(isHealthy(LOCAL_SERVING, 'kubernetes')).toBe(false);
  });

  it('never lets `unknown` be healthy on an axis that COULD have been measured', () => {
    // The guard that keeps this from being the collapse ADR 0020 forbids. `poller` is answered by
    // Temporal, which can be asked about a Worker on any node, so an unmeasured poller is still not
    // serving — on a local pane exactly as on a fleet one.
    expect(isHealthy({ ...LOCAL_SERVING, poller: 'unknown' }, 'local')).toBe(false);
    expect(isHealthy({ ...LOCAL_SERVING, session: 'unknown' }, 'local')).toBe(false);
  });

  it('still fails on an inapplicable axis that actually FAILS — omission is one-directional', () => {
    expect(isHealthy({ ...LOCAL_SERVING, loads: 'failing' }, 'local')).toBe(false);
    expect(isHealthy({ ...LOCAL_SERVING, reachable: 'fail' }, 'local')).toBe(false);
  });

  it('is the same predicate the tile reads, applied to the same pane', () => {
    // The whole point of routing both through `signalApplies`: what the chips omit as inapplicable
    // and what this rollup skips are the same list, so a tile showing `n/a: reachable, loads` and an
    // Actors page counting that Machine as serving are two renderings of one reading.
    for (const signal of ['reachable', 'loads'] as const) {
      expect(signalApplies(signal, 'local')).toBe(false);
      expect(signalApplies(signal, 'fleet')).toBe(true);
    }
    expect(signalApplies('poller', 'local')).toBe(true);
    expect(signalApplies('session', 'local')).toBe(true);
  });
});

/**
 * WHICH AXES A PANE'S MODE CAN ANSWER AT ALL.
 *
 * The three modes do not share machinery, and pretending they do is what put two chips on every
 * local tile that could never say anything: nothing DIALS a local or a docker node (the probe is
 * `sh -c` / `docker exec` on the streamer's own host), and nothing SCRAPES one — vmagent is
 * installed by the fleet Machine program alone. "unknown" there is not "not yet".
 */
describe('an axis that cannot apply to this mode', () => {
  it('drops exactly reachable and loads for local and docker, and nothing for fleet', () => {
    for (const mode of ['local', 'docker'] as const) {
      expect(signalApplies('reachable', mode)).toBe(false);
      expect(signalApplies('loads', mode)).toBe(false);
      expect(signalApplies('session', mode)).toBe(true);
      expect(signalApplies('process', mode)).toBe(true);
      // Temporal can be asked about any Worker's queue from anywhere, so this one stays — and on
      // this box it is the only signal on a local tile that has ever reported a real finding.
      expect(signalApplies('poller', mode)).toBe(true);
    }
    for (const signal of ['reachable', 'session', 'process', 'poller', 'loads'] as const) {
      expect(signalApplies(signal, 'fleet')).toBe(true);
      // A mode from a later streamer gets everything. Omission must never be a default for a word
      // this build does not recognise.
      expect(signalApplies(signal, 'kubernetes')).toBe(true);
    }
  });

  it('gives a reason naming the machinery that is missing, not a shrug', () => {
    expect(inapplicableReason('reachable', 'local')).toContain('`sh -c`');
    expect(inapplicableReason('reachable', 'docker')).toContain('`docker exec`');
    expect(inapplicableReason('loads', 'local')).toContain('vmagent');
    expect(inapplicableReason('reachable', 'fleet')).toBeUndefined();
    expect(inapplicableReason('loads', 'fleet')).toBeUndefined();
  });

  it('calls the transport what actually carried the probe', () => {
    // `transport.ts:reachWord` has said this on the server since slice 6; the browser was the last
    // place calling `docker exec` and `sh -c` "ssh".
    const by = (mode: string) =>
      healthEntries({ ...UNKNOWN, reachable: 'fail' }, mode).find((e) => e.signal === 'reachable');
    expect(by('fleet')?.label).toBe('ssh: UNREACHABLE');
    expect(by('docker')?.label).toBe('exec: UNREACHABLE');
    expect(by('local')?.label).toBe('shell: UNREACHABLE');
  });
});

/**
 * THE TWO SETS OF WORDS, AND WHY THERE ARE TWO.
 *
 * `label` goes on a chip beside four others on a 200-pixel tile; `text` goes in the drawer and the
 * tooltip and carries the clause that says what to do. One field could not be both, which is how the
 * chip row got to 91 characters and wrapped a lone chip onto a second line.
 */
describe('chip words and sentence words', () => {
  it('keeps the clause out of the label and in the sentence', () => {
    const entries = healthEntries({
      reachable: 'ok',
      session: 'absent',
      process: 'exited',
      poller: 'none',
      loads: 'failing',
    });
    const label = (s: string) => entries.find((e) => e.signal === s)?.label;
    const text = (s: string) => entries.find((e) => e.signal === s)?.text;
    expect(label('session')).toBe('session: ABSENT');
    expect(text('session')).toBe('session: ABSENT — converge to create it');
    expect(label('process')).toBe('process: EXITED');
    expect(text('process')).toContain('the screen is its last output');
    expect(label('poller')).toBe('poller: NONE');
    expect(text('poller')).toContain('registered but nothing polling');
    // No label may run past its state word: a chip's whole job is to be readable in one glance
    // beside four others.
    for (const e of entries) {
      if (e.signal === 'detail') continue;
      expect(e.label.length).toBeLessThanOrEqual(20);
    }
  });

  it('stops promising a `loads` reading that this installation can never deliver', () => {
    // `docker-compose.yml` dropped VictoriaMetrics; ADR 0020 already records that `loads` reports
    // `unknown` and never `ok` until the missing counters are wired. "not measured yet" reads as a
    // wait for something that is coming. Nothing is coming, and the sentence now says what is absent.
    const loads = healthEntries(UNKNOWN).find((e) => e.signal === 'loads');
    expect(loads?.text).toBe('loads: unknown — no metrics backend has answered for this node');
    expect(loads?.text).not.toContain('not measured yet');
    // Still the word `unknown`, in the words as well as the attribute.
    expect(loads?.label).toBe('loads: unknown');
  });
});
