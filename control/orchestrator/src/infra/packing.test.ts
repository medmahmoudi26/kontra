/**
 * PACKING, PROVEN AGAINST THE REAL PROGRAM: two Actors on one Fleet, and what a converge that
 * mentions one does to the other (ADR 0037, slice 11).
 *
 * ═══ WHY THIS FILE RUNS THE PROGRAM INSTEAD OF READING IT ═══
 *
 * `fleet.test.ts` pins the program's SOURCE TEXT, which is the right instrument for "the triggers
 * still carry the sha" and the wrong one for the claim this slice has to make. The claim is about
 * IDENTITY: two placements must produce two `command.remote.Command` resources with two different
 * URNs, because a URN is the only thing Pulumi uses to decide whether a resource in the program is
 * the one already in the state. A source-text assertion cannot see that — the old code read
 * `` `${m.name}-actor` ``, which looks fine on the page and gives both Artifacts the SAME name, so
 * the second placement would REPLACE the first rather than join it. That is the whole hazard slice
 * 10 measured and refused rather than risk: Pulumi's desired state is TOTAL, and a resource that
 * leaves the program is DELETED, which runs its teardown and stops a Worker with nothing raising on
 * either side.
 *
 * So: `pulumi.runtime.setMocks` constructs every resource for real and intercepts it here. Nothing
 * is provisioned, nothing is dialled, and what is asserted is the set of resources the engine would
 * have been asked for — which is exactly the thing a converge diffs.
 *
 * ═══ THE SETTLE LOOP IS LOAD-BEARING ═══
 *
 * The placement commands `dependsOn` a Droplet, so their registration awaits that resource's URN and
 * lands on a later turn of the event loop — AFTER the program's promise resolves. A test that
 * asserted straight after `await program()` would see two Droplets and no install commands, and
 * would pass every assertion about what is NOT there. `machineSecret.test.ts` found that first; this
 * file inherits the fix and adds a positive control, because half of what follows is a negative.
 */

import { beforeAll, describe, expect, it } from 'vitest';
import * as pulumi from '@pulumi/pulumi';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { fleetProgram, type FleetArgs } from './programs/fleet';

/** One resource the program asked the engine for. */
interface Seen {
  type: string;
  name: string;
  inputs: Record<string, unknown>;
}

const captured: Seen[] = [];

const SHA_A = 'a'.repeat(64);
const SHA_B = 'b'.repeat(64);

const NSCHECK = {
  actorName: 'nscheck',
  actorVersion: '0.1.0',
  actorEngine: 'py',
  bundleUrl: `http://10.124.0.2:5000/v2/bundles/nscheck/blobs/sha256:${SHA_A}`,
  bundleSha: SHA_A,
  controller: '10.124.0.2',
};

const SUBFINDER = {
  actorName: 'subfinder',
  actorVersion: '0.2.0',
  actorEngine: 'py',
  bundleUrl: `http://10.124.0.2:5000/v2/bundles/subfinder/blobs/sha256:${SHA_B}`,
  bundleSha: SHA_B,
  controller: '10.124.0.2',
};

/** Run the real fleet program under the mock engine and return the resources it asked for.
 *
 *  EACH SCENARIO GETS ITS OWN TAG, so its Machines are `kf-<tag>-NN` and its resource names cannot
 *  collide with another scenario's inside one Pulumi runtime. That is a property of running several
 *  converges in one process, not of the program. */
async function converge(args: FleetArgs): Promise<Seen[]> {
  const before = captured.length;
  await fleetProgram(args)();
  // Settle: see this file's header. Stop when the capture has been quiet for five polls.
  for (let quiet = 0, seen = -1; quiet < 5; quiet += captured.length === seen ? 1 : 0) {
    seen = captured.length;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  return captured.slice(before);
}

const commands = (seen: Seen[]) =>
  seen.filter((r) => r.type.startsWith('command:remote')).map((r) => r.name).sort();

/**
 * Unwrap Pulumi's secret sentinel.
 *
 * `command.remote.Command.connection` is declared SECRET — it carries the fleet SSH key — so it
 * reaches a mock as `{ 4dabf…: 1b47…, value: {…} }` rather than as the object the program passed.
 * Reading `.host` off the wrapper gives `undefined`, which is what this file's first draft asserted
 * against, twice, and it agreed with itself perfectly.
 */
function unsecret(v: unknown): unknown {
  const SIG = '4dabf18193072939515e22adb298388d';
  if (v && typeof v === 'object' && SIG in (v as Record<string, unknown>)) {
    return (v as { value?: unknown }).value;
  }
  return v;
}

beforeAll(() => {
  const dir = mkdtempSync(path.join(tmpdir(), 'kontra-packing-'));
  const keyFile = path.join(dir, 'fleet_key');
  writeFileSync(keyFile, '-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END-----\n');
  process.env.KONTRA_SSH_KEY = keyFile;

  pulumi.runtime.setMocks(
    {
      newResource: (args: pulumi.runtime.MockResourceArgs) => {
        captured.push({ type: args.type, name: args.name, inputs: args.inputs });
        return {
          id: `${args.name}-id`,
          state: {
            ...args.inputs,
            ipv4Address: '203.0.113.10',
            ipv4AddressPrivate: '10.124.0.11',
            name: args.inputs.name ?? args.name,
          },
        };
      },
      call: (args: pulumi.runtime.MockCallArgs) => args.inputs,
    },
    'kontra-fleet',
    'packing',
    false
  );
});

describe('two Actors on one Fleet', () => {
  /**
   * ═══ THE ASSERTION THIS SLICE EXISTS TO MAKE ═══
   *
   * A converge mentioning BOTH placements asks for both Workers on every Machine. Four resources on
   * two Machines, with four distinct names — so a converge that sent this against a state holding
   * only `nscheck` would ADD `subfinder` and touch nothing else.
   */
  it('a converge that mentions both asks for both, on every Machine', async () => {
    const seen = await converge({
      tag: 'both',
      machines: 2,
      placements: [NSCHECK, SUBFINDER],
    });
    expect(commands(seen)).toEqual([
      'kf-both-01-actor-nscheck',
      'kf-both-01-actor-subfinder',
      'kf-both-02-actor-nscheck',
      'kf-both-02-actor-subfinder',
    ]);
    // …and the two Droplets, so the sweep above is reading a real converge rather than an empty one.
    expect(seen.filter((r) => r.type.includes('Droplet'))).toHaveLength(2);
  });

  /**
   * ═══ AND THE HAZARD, MEASURED RATHER THAN ASSERTED ═══
   *
   * The same Fleet converged with only ONE placement. Pulumi's desired state is total, so the
   * resources ABSENT from this list are the ones it would delete — and deleting a
   * `command.remote.Command` runs its `delete` script, which stops that Worker. This test is what
   * turns "a converge that mentions one must not delete the other" from a sentence into a diff:
   * the two names present are `subfinder`'s, so `nscheck`'s two are the ones that would go.
   *
   * IT IS A REAL FAILURE MODE AND NOT A THEORETICAL ONE. `kontra.fleet` is what must never send
   * this shape, and `tests/test_fleet_hold_place.py` holds it there; `cli/fleet.go` is the other
   * writer and `placementsOutput` is what stops IT sending this shape on a plain `--count` change.
   */
  it('a converge that mentions one asks for only that one — which is why the SDK never sends it', async () => {
    const seen = await converge({ tag: 'alone', machines: 2, placements: [SUBFINDER] });
    expect(commands(seen)).toEqual(['kf-alone-01-actor-subfinder', 'kf-alone-02-actor-subfinder']);
  });

  /**
   * THE NAME IS WHAT KEEPS THEM APART, so the failure worth pinning is the one where it does not.
   * Under the pre-slice-11 name — `${m.name}-actor` — both of these would be `kf-both-01-actor` and
   * Pulumi would see ONE resource whose inputs changed: it would run `subfinder`'s install over
   * `nscheck`'s Machine and report success. The assertion is that every name is distinct, which is
   * the property, rather than that some particular string appears in it.
   */
  it('gives every Worker on a Machine a resource name of its own', async () => {
    const seen = await converge({ tag: 'names', machines: 3, placements: [NSCHECK, SUBFINDER] });
    const names = commands(seen);
    expect(names).toHaveLength(6);
    expect(new Set(names).size).toBe(6);
  });

  /**
   * THE INSTALL SCRIPTS ARE THE SECOND HALF, AND THEY ARE A DIFFERENT KIND OF COLLISION. Two
   * distinct resources whose scripts wrote the same unit file would still leave one Worker: the
   * scheduling would be right and the Machine would hold one process. `machine.test.ts` proves the
   * paths in isolation; this proves the program actually hands each placement its own.
   */
  it('sends each Worker an install that writes only its own paths', async () => {
    const seen = await converge({ tag: 'paths', machines: 1, placements: [NSCHECK, SUBFINDER] });
    const script = (actor: string) =>
      String(seen.find((r) => r.name === `kf-paths-01-actor-${actor}`)?.inputs.create ?? '');
    expect(script('nscheck')).toContain('ROOT=/opt/kontra/w/nscheck');
    expect(script('subfinder')).toContain('ROOT=/opt/kontra/w/subfinder');
    expect(script('nscheck')).not.toContain('/opt/kontra/w/subfinder');
    // And each teardown names only its own Worker, so removing one placement cannot stop the other.
    const teardown = (actor: string) =>
      String(seen.find((r) => r.name === `kf-paths-01-actor-${actor}`)?.inputs.delete ?? '');
    expect(teardown('nscheck')).toContain('kontra-actor-nscheck.service');
    expect(teardown('nscheck')).not.toContain('subfinder');
  });

  /**
   * PACKED WORKERS SHARE THEIR MACHINE'S EGRESS ADDRESS, and the program is where that becomes
   * true: both commands connect to the SAME `ipv4Address`, which is the address their traffic
   * leaves by. Stated here because it is the trade ADR 0037 names and the reason `spread=` exists —
   * `cli/warden_packing_test.go` measures the same fact on a live runtime.
   */
  it('puts both Workers behind one Machine, and therefore one address', async () => {
    const seen = await converge({ tag: 'egress', machines: 1, placements: [NSCHECK, SUBFINDER] });
    const hosts = seen
      .filter((r) => r.type.startsWith('command:remote'))
      .map((r) => (unsecret(r.inputs.connection) as { host?: string } | undefined)?.host);
    expect(hosts).toHaveLength(2);
    expect(new Set(hosts).size).toBe(1);
    expect(hosts[0]).toBe('203.0.113.10');
    // ONE Machine, so one source address for both — asserted alongside the hosts because a
    // `connection` that failed to unwrap would give two `undefined`s, which is also a set of one.
    expect(seen.filter((r) => r.type.includes('Droplet'))).toHaveLength(1);
  });

  /**
   * THE PORT THE SECOND WORKER WOULD OTHERWISE LOSE. Both actor hosts default to :9110 and the
   * Python one CATCHES the bind failure and carries on serving nothing, so a packed Machine on a
   * shared port has a Worker the Warden can never judge. The program is what knows a Fleet is
   * packing, so the program is what assigns the ports.
   */
  it('gives each packed Worker its own metrics port, and a lone one the host default', async () => {
    const packed = await converge({ tag: 'ports', machines: 1, placements: [NSCHECK, SUBFINDER] });
    const script = (seen: Seen[], name: string) =>
      String(seen.find((r) => r.name === name)?.inputs.create ?? '');
    expect(script(packed, 'kf-ports-01-actor-nscheck')).toContain(
      'KONTRA_METRICS_ADDR=127.0.0.1:9110'
    );
    expect(script(packed, 'kf-ports-01-actor-subfinder')).toContain(
      'KONTRA_METRICS_ADDR=127.0.0.1:9111'
    );
    // THE CONTROL: one Artifact on the Fleet leaves the variable unset, so every Machine in every
    // existing Fleet keeps the environment it has.
    const lone = await converge({ tag: 'oneport', machines: 1, placements: [NSCHECK] });
    expect(script(lone, 'kf-oneport-01-actor-nscheck')).not.toContain('KONTRA_METRICS_ADDR');
  });

  /**
   * ═══ AND THE PORT IS IN THE TRIGGERS, WHICH IS THE HALF THAT IS EASY TO MISS ═══
   *
   * A `command.remote.Command` whose inputs are unchanged is SKIPPED. The port moves when a Fleet
   * starts or stops packing — `nscheck` alone is on the host default and `nscheck` beside
   * `subfinder` is on 9110 explicitly — and if that change did not re-run the install, the converge
   * would report success while the Worker kept serving on the old port. Nothing would then scrape
   * it, and the Warden would judge it `cannot tell` for ever. This is the same argument
   * `maxSessions` already has in this list, and it is the same silence.
   */
  it('puts the metrics port in the remote command triggers', async () => {
    const seen = await converge({ tag: 'trig', machines: 1, placements: [NSCHECK, SUBFINDER] });
    const triggers = (name: string) =>
      (seen.find((r) => r.name === name)?.inputs.triggers as unknown[] | undefined)?.map(String) ??
      [];
    expect(triggers('kf-trig-01-actor-nscheck')).toContain('9110');
    expect(triggers('kf-trig-01-actor-subfinder')).toContain('9111');
    // …and the sha and the Controller are still in there, so this did not replace them.
    expect(triggers('kf-trig-01-actor-nscheck')).toContain(SHA_A);
    expect(triggers('kf-trig-01-actor-nscheck')).toContain('10.124.0.2');
  });
});

describe('workers and spread, on the Machines themselves', () => {
  /**
   * `spread=True` in the caller SDK is `workers == machines` on the wire — there is no boolean on
   * this wire (`shared/conformance/placement.json:never_boolean`). What it buys is what this asserts: one
   * Worker of this placement on EVERY Machine, which is `subfinder`'s operating rule.
   */
  it('spread puts one Worker of the placement on every Machine', async () => {
    const seen = await converge({
      tag: 'spread',
      machines: 3,
      placements: [{ ...SUBFINDER, workers: 3 }],
    });
    expect(commands(seen)).toEqual([
      'kf-spread-01-actor-subfinder',
      'kf-spread-02-actor-subfinder',
      'kf-spread-03-actor-subfinder',
    ]);
  });

  /**
   * AND A SHORT PLACEMENT TAKES A PREFIX, which is what lets a Fleet be scaled without moving a
   * Worker. The Machines it is NOT on are the ones a co-tenant can have to itself — which is the
   * only way, today, to give a placement a Machine nobody else is on.
   */
  it('a short placement takes the first Machines and leaves the rest', async () => {
    const seen = await converge({
      tag: 'short',
      machines: 3,
      placements: [{ ...NSCHECK, workers: 1 }, { ...SUBFINDER, workers: 2 }],
    });
    expect(commands(seen)).toEqual([
      'kf-short-01-actor-nscheck',
      'kf-short-01-actor-subfinder',
      'kf-short-02-actor-subfinder',
    ]);
    // kf-short-03 holds nothing, and that is the point: it is capacity a third placement can have.
    expect(commands(seen).filter((n) => n.startsWith('kf-short-03'))).toEqual([]);
  });

  it('refuses more Workers than Machines instead of building fewer', async () => {
    await expect(
      fleetProgram({ tag: 'toomany', machines: 2, placements: [{ ...NSCHECK, workers: 8 }] })()
    ).rejects.toThrow(/8 Workers on a Fleet of 2/);
  });

  /**
   * THE PRE-PACKING SPELLING STILL CONVERGES, and it must, because `kontra fleet deploy` sends it
   * and every stack this repo has ever created was created from it. One placement, every Machine.
   */
  it('the single-placement spelling still places on every Machine', async () => {
    const seen = await converge({ tag: 'legacy', machines: 2, ...NSCHECK });
    expect(commands(seen)).toEqual(['kf-legacy-01-actor-nscheck', 'kf-legacy-02-actor-nscheck']);
  });
});
