/**
 * The Warden report path: what it carries, what it refuses, and the stale pane it ends.
 *
 * ═══ THE CONTROLS COME FIRST ═══
 *
 * `cli/driver_podman_test.go`'s socket test is this program's model — prove the hazard exists before
 * asserting it was handled — and this file follows it because two of its assertions are negatives
 * that pass trivially against a module that does nothing:
 *
 *   - "telemetry does not touch workflow history" is worthless unless the sweep FINDS Temporal
 *     somewhere. It asserts that first.
 *   - "a destroyed Machine's panes disappear" is worthless unless a Machine's panes appear at all.
 *     The same test asserts the pane on the wall before asserting it leaves.
 */

import { readFileSync, readdirSync } from 'node:fs';
import * as path from 'node:path';
import { describe, expect, it } from 'vitest';

import type { MachineTarget } from './discovery';
import { interpretProbe } from './probe';
import { HOST_MARKER, parseProbeHost } from './tmux';
import {
  loadsFromReport,
  probeFromReport,
  targetFromReport,
  telemetrySentence,
  WardenReports,
  REPORT_TTL_MS,
  MAX_REPORTING_MACHINES,
  type WardenReport,
} from './warden';

/** A report shaped exactly as `cli/panereport.go` marshals one. */
function report(over: Partial<WardenReport> = {}): WardenReport {
  return {
    warden: 'w-abc',
    machine: 'kf-nscheck-01',
    driver: 'podman',
    at: '2026-08-30T12:00:00Z',
    telemetry: { cpu: 0.42, memory: 0.31, load1: 1.25 },
    workers: [
      {
        id: 'nscheck@0.1.0',
        name: 'nscheck',
        version: '0.1.0',
        whole: true,
        halves: ['actor', 'handler'],
        health: { verdict: 'healthy', reason: 'ratio', ratio: 0.02, loads: 82, failures: 2, spanSeconds: 300 },
        panes: [
          { window: 'actor', cols: 120, rows: 40, frame: 'actor is up and polling' },
          { window: 'handler', cols: 120, rows: 40, frame: 'handler is up and polling' },
        ],
      },
    ],
    ...over,
  };
}

describe('a report is evidence of existence, and the inventory is not', () => {
  it('is believed for its TTL and no longer, on the CONTROLLER’s clock', () => {
    // THE MACHINE'S OWN CLOCK IS NOT USED, and the reason is the failure it would cause: a Machine
    // an hour behind would have every report arrive already expired, and one an hour ahead would be
    // believed for an hour after it died — which is the stale pane this module exists to end,
    // arriving through the field meant to prevent it. So `at` here is deliberately absurd.
    let now = 1_000_000;
    const reports = new WardenReports(() => now);
    reports.accept(report({ at: '1999-01-01T00:00:00Z' }));
    expect(reports.has('kf-nscheck-01')).toBe(true);

    now += REPORT_TTL_MS - 1;
    expect(reports.has('kf-nscheck-01')).toBe(true);
    now += 2;
    expect(reports.has('kf-nscheck-01')).toBe(false);

    // …and the Machine is still remembered as one that HAD a Warden, which is the whole basis of
    // "gone" as distinct from "unreachable".
    expect(reports.seen('kf-nscheck-01')).toBe(true);
  });

  it('refuses a new Machine at the cap and still accepts an existing one’s update', () => {
    // The map is filled by a POST, so it is bounded. Refusing the UPDATE instead would freeze the
    // wall at whatever it held when the cap was hit — a Fleet that looks like it stopped changing.
    const reports = new WardenReports(() => 1);
    // Names that satisfy `ids.ts:SAFE.machine` (`kf-<tag>-NN`), because the ingest refuses anything
    // else — see the test below. A fixture that used `kf-fill-0` filled nothing and made this whole
    // assertion vacuous, which is how it was found.
    const filler = (i: number): string => `kf-f${String(i).padStart(3, '0')}-01`;
    for (let i = 0; i < MAX_REPORTING_MACHINES; i++) {
      expect(reports.accept(report({ machine: filler(i) })), filler(i)).toBe(true);
    }
    expect(reports.size()).toBe(MAX_REPORTING_MACHINES);
    expect(reports.accept(report({ machine: 'kf-toomany-01' }))).toBe(false);
    expect(reports.has('kf-toomany-01')).toBe(false);

    expect(reports.accept(report({ machine: filler(0), driver: 'process' }))).toBe(true);
    expect(reports.report(filler(0))?.driver).toBe('process');
  });

  it('drops a report that names no Machine, or one no Terminal id could carry', () => {
    // A name outside `ids.ts:SAFE.machine` reaches `terminalId`, throws, and is skipped one layer
    // down — so a Machine reporting under any other name would be accepted, held, counted, and
    // invisible, which from the Machine is indistinguishable from a Controller with nobody watching.
    const reports = new WardenReports(() => 1);
    for (const machine of ['   ', 'byoc-01', 'kf-nscheck-1', 'kf-a-01; rm -rf /', '']) {
      expect(reports.accept({ ...report(), machine }), machine).toBe(false);
    }
    expect(reports.size()).toBe(0);
    // THE CONTROL: a well-formed name is taken, or the loop above proves only that `accept` refuses
    // everything.
    expect(reports.accept(report())).toBe(true);
  });
});

describe('a report becomes the same ProbeResult an SSH probe would have produced', () => {
  it('carries the panes, their geometry, and the Worker’s halves', () => {
    const probe = probeFromReport(report());
    expect(probe.reachable).toBe('ok');
    expect(probe.session).toBe('present');
    expect(probe.windows).toEqual(['actor', 'handler']);
    expect(probe.panes?.get('actor')?.cols).toBe(120);
    expect(probe.panes?.get('actor')?.rows).toBe(40);
    expect(probe.panes?.get('actor')?.process).toBe('running');
  });

  it('says a Machine with a Warden and no Workers has no session, rather than being unreachable', () => {
    // The two are different facts and the actions differ: an unreachable Machine is a network or a
    // key problem, and a Machine with nothing placed on it is waiting for a `place()`.
    const probe = probeFromReport(report({ workers: [] }));
    expect(probe.reachable).toBe('ok');
    expect(probe.session).toBe('absent');
    expect(probe.windows).toEqual([]);
  });

  it('carries a sick Worker’s sentence, and a cannot-tell’s', () => {
    const sick = probeFromReport(
      report({
        workers: [
          {
            ...report().workers[0]!,
            health: {
              verdict: 'sick',
              reason: 'ratio',
              detail: '81 of its 82 resource loads failed in 5m0s (99%)',
              ratio: 0.988,
              loads: 82,
              failures: 81,
              spanSeconds: 300,
            },
          },
        ],
      })
    );
    expect(sick.detail).toContain('81 of its 82 resource loads failed');

    const blind = probeFromReport(
      report({
        workers: [
          {
            ...report().workers[0]!,
            health: {
              verdict: 'cannot-tell',
              reason: 'scrape',
              detail: 'could not read its counters at 10.88.0.7:9110',
              ratio: 0,
              loads: 0,
              failures: 0,
              spanSeconds: 0,
            },
          },
        ],
      })
    );
    // `cannot tell` HAS to reach the operator. A health check that degrades to silence is the same
    // failure as one that degrades to always-pass, one step further along.
    expect(blind.detail).toContain('could not read its counters');
  });
});

describe('the loads chip', () => {
  it('is `failing` for a sick Worker and `ok` for a healthy one', () => {
    expect(loadsFromReport(report())).toBe('ok');
    const sick = report();
    sick.workers[0]!.health.verdict = 'sick';
    expect(loadsFromReport(sick)).toBe('failing');
  });

  it('is `unknown` — never `ok` — for every cannot-tell', () => {
    // THE ONE-WAY RULE. `metrics.ts` argues it at length for the VictoriaMetrics path: "a green chip
    // derived from a counter nothing increments would be worse than no chip". Every reason the
    // Warden has for not knowing collapses to `unknown`, and none of them to `ok`.
    for (const reason of ['address', 'scrape', 'samples', 'span', 'loads', 'reset', 'disabled']) {
      const r = report();
      r.workers[0]!.health = {
        verdict: 'cannot-tell',
        reason,
        ratio: 0,
        loads: 0,
        failures: 0,
        spanSeconds: 0,
      };
      expect(loadsFromReport(r), reason).toBe('unknown');
    }
  });

  it('is `failing` when ANY Worker on the Machine is sick', () => {
    const r = report();
    r.workers = [
      { ...r.workers[0]!, id: 'a@1', name: 'a' },
      {
        ...r.workers[0]!,
        id: 'b@1',
        name: 'b',
        health: { verdict: 'sick', reason: 'ratio', ratio: 1, loads: 82, failures: 81, spanSeconds: 300 },
      },
    ];
    expect(loadsFromReport(r)).toBe('failing');
  });
});

describe('telemetry', () => {
  it('reads as one sentence a human can hold', () => {
    expect(telemetrySentence({ cpu: 0.42, memory: 0.31, load1: 1.25 })).toBe(
      'cpu 42%, memory 31%, load 1.25'
    );
  });

  it('omits a field that was not measured, rather than rendering it as zero', () => {
    // `types.ts`: "`unknown` is a value, never a shrug." A Machine whose /proc could not be read is
    // not a Machine that is idle.
    expect(telemetrySentence({ load1: 0.5 })).toBe('load 0.50');
    expect(telemetrySentence({})).toBeUndefined();
    expect(telemetrySentence(undefined)).toBeUndefined();
    // …and zero is a real reading that must still appear.
    expect(telemetrySentence({ cpu: 0 })).toBe('cpu 0%');
  });
});

describe('a self-enrolled Machine is on the wall without ever being in a checkpoint', () => {
  it('becomes a fleet MachineTarget from its report alone', () => {
    // ADR 0037's `machines=None` "takes what already exists rather than provisioning", so a
    // self-enrolled Machine is in no Pulumi state. A wall built only from `discovery.ts` would be
    // blind to exactly the Machines this program is being built for.
    const m: MachineTarget = targetFromReport(report());
    expect(m.mode).toBe('fleet');
    expect(m.machine).toBe('kf-nscheck-01');
    expect(m.actor).toBe('nscheck');
    expect(m.version).toBe('0.1.0');
    expect(m.session).toBe('nscheck-0_1_0'); // sessionNameFor, tmux-sanitised
    expect(m.windows).toEqual(['actor', 'handler']);
  });
});

describe('the address is not the identity', () => {
  const machine: MachineTarget = {
    mode: 'fleet',
    machine: 'kf-nscheck-01',
    host: '10.124.0.5',
    publicIp: '203.0.113.9',
    tag: 'nscheck',
    fleet: 'nscheck-0.1.0',
    actor: 'nscheck',
    version: '0.1.0',
    session: 'nscheck-0_1_0',
    windows: ['actor', 'handler'],
  };
  const panes =
    'nscheck-0_1_0 actor %1 120x40 0 c:zsh x: \n' + 'nscheck-0_1_0 handler %2 120x40 0 c:zsh x: \n';

  it('THE CONTROL: a box that says it IS this Machine is read exactly as before', () => {
    const ok = interpretProbe(machine, 0, `${HOST_MARKER} kf-nscheck-01\n${panes}`, '');
    expect(ok.gone).toBeUndefined();
    expect(ok.session).toBe('present');
    expect(ok.windows).toEqual(['actor', 'handler']);
  });

  it('refuses a box that says it is a DIFFERENT Machine, however consistent everything else is', () => {
    // THE OBSERVED BUG. DigitalOcean reuses `10.124.0.3`-`.12` for every Fleet in a VPC, and a
    // Machine's session is named `<actor>-<version>` — so a destroyed Machine's address, dialled
    // again, reaches a LIVE Machine of another Fleet running the same Artifact, with the session the
    // probe was looking for. Every field was consistent; the pane was somebody else's screen.
    const gone = interpretProbe(machine, 0, `${HOST_MARKER} kf-nscheck-04\n${panes}`, '');
    expect(gone.gone).toBeDefined();
    expect(gone.gone).toContain('kf-nscheck-04');
    expect(gone.gone).toContain('kf-nscheck-01');
    // NOT a session state: `TerminalTile.tsx:sessionGone` offers "Converge session" for anything
    // that is neither `present` nor `unknown`, so a new enum member would have appeared as the exact
    // remedy this is withdrawing.
    expect(gone.session).toBe('unknown');
    expect(gone.windows).toEqual([]);
  });

  it('does not check a box that did not say — an older streamer, or an image with no `hostname`', () => {
    const unchecked = interpretProbe(machine, 0, panes, '');
    expect(unchecked.gone).toBeUndefined();
    expect(unchecked.session).toBe('present');
  });

  it('ignores a domain suffix and case, which are not a difference between two Machines', () => {
    const ok = interpretProbe(machine, 0, `${HOST_MARKER} KF-NSCheck-01.internal\n${panes}`, '');
    expect(ok.gone).toBeUndefined();
  });

  it('never checks a local or docker node, whose name is not its hostname by design', () => {
    // `health.ts:selfHostName` records the live incident from the other direction: inside a
    // container `os.hostname()` is the container id, and every local pane on the wall went red.
    for (const mode of ['local', 'docker'] as const) {
      const other = { ...machine, mode, machine: 'localhost' };
      expect(interpretProbe(other, 0, `${HOST_MARKER} 242b5de62fa4\n${panes}`, '').gone).toBeUndefined();
    }
  });

  it('parses the marker out of a chatty shell, taking the last one', () => {
    expect(parseProbeHost('MOTD: welcome\nKONTRA_HOST kf-a-01\n')).toBe('kf-a-01');
    expect(parseProbeHost(`${HOST_MARKER} first\n${HOST_MARKER} last\n`)).toBe('last');
    expect(parseProbeHost(panes)).toBeUndefined();
  });

  it('the marker cannot be mistaken for a pane, and a pane cannot be mistaken for the marker', () => {
    // The two parsers run over the same stdout, so each must ignore the other's lines.
    const both = `${HOST_MARKER} kf-nscheck-01\n${panes}`;
    const probe = interpretProbe(machine, 0, both, '');
    expect(probe.windows).toEqual(['actor', 'handler']);
    expect([...(probe.panes?.keys() ?? [])]).toEqual(['actor', 'handler']);
  });
});

describe('none of this rides workflow history', () => {
  /**
   * ADR 0037: "Telemetry does not ride workflow history… A 200×50 terminal frame through Temporal
   * history is the shape this repo already measured as 86% of a workflow's events, for a value that
   * is stale a second later."
   *
   * A source sweep, and honest about being one: it proves the telemetry path cannot REACH Temporal,
   * which is the structural form of the claim. THE CONTROL IS WHAT MAKES IT WORTH RUNNING — a sweep
   * that matched nothing anywhere would pass on a typo, which is the first vacuous guard this
   * program found (a walk that visited zero files).
   */
  const dir = __dirname;
  const TEMPORAL = /@temporalio\//;

  it('THE CONTROL: the sweep finds Temporal where Temporal legitimately is', () => {
    const backend = path.resolve(dir, '..');
    const importers = readdirSync(backend)
      .filter((f) => f.endsWith('.ts'))
      .filter((f) => TEMPORAL.test(readFileSync(path.join(backend, f), 'utf8')));
    expect(importers.length, 'no file in control/orchestrator/src imports @temporalio — fix the pattern').toBeGreaterThan(0);
  });

  it('the telemetry path imports no Temporal client, workflow or activity', () => {
    for (const file of ['warden.ts', 'server.ts', 'metrics.ts']) {
      const src = readFileSync(path.join(dir, file), 'utf8');
      expect(TEMPORAL.test(src), `${file} reaches Temporal`).toBe(false);
    }
  });

  it('a report is REPLACED, never appended — a pane is a screen, not a log', () => {
    // The other half of the same property: even off workflow history, a path that kept every frame
    // would be the growth ADR 0020 measured on a Machine, reproduced on the Controller.
    let now = 1;
    const reports = new WardenReports(() => now++);
    for (let i = 0; i < 500; i++) reports.accept(report({ at: `2026-08-30T12:00:${i % 60}Z` }));
    expect(reports.size()).toBe(1);
    expect(reports.reporting()).toEqual(['kf-nscheck-01']);
  });
});
