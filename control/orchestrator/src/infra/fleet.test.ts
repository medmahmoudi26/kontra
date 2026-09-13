/**
 * The fleet program's guards (ADR 0019).
 *
 * These pin the things `infra/tofu/main.tf` learned the hard way and that a rewrite is most
 * likely to drop quietly: the replacement triggers, the role bound, the machine naming that
 * every log and inventory refers to, and the absence of any actor version in cloud-init.
 *
 * The program is not executed here — that needs the Pulumi engine. What is asserted is the
 * shape it builds and the input validation in front of it, which is where the regressions
 * would land.
 */

import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  FLEET_DEFAULTS,
  fleetDefaults,
  machinesFor,
  placementsOf,
  validateTag,
} from './programs/fleet';
import { coerceFleetArgs, programFor, FLEET_PROJECT } from './stacks';
import { fqn, parseFqn } from './workspace';

const SOURCE = readFileSync(join(__dirname, 'programs', 'fleet.ts'), 'utf8');

describe('tag', () => {
  it('accepts an arbitrary lowercase role, not an enum of known workloads', () => {
    // A hardcoded crawl|bust list meant every new actor had to edit this file or mislabel its
    // infrastructure. `leaks` was provisioned into exactly that gap once.
    for (const ok of ['crawl', 'bust', 'leaks', 'desync', 'a1', 'x-y-z']) {
      expect(() => validateTag(ok)).not.toThrow();
    }
  });

  it('rejects anything unsafe to interpolate into a tag, group or machine name', () => {
    for (const bad of ['', 'A', '1abc', 'has space', 'has_underscore', 'x', 'toolongrolename-x', '../etc']) {
      expect(() => validateTag(bad), bad).toThrow(/invalid/);
    }
  });
});

describe('replacement triggers', () => {
  it('ignores image, userData AND sshKeys', () => {
    // main.tf only needed `ignore_changes = [image]`. In Pulumi all three force replacement,
    // so an innocent cloud-init edit would destroy a live fleet mid-run. If this
    // assertion is ever "fixed" by narrowing the list, read the ADR first.
    expect(SOURCE).toMatch(/ignoreChanges:\s*\['image',\s*'userData',\s*'sshKeys'\]/);
  });
});

describe('cloud-init', () => {
  it('pins NO actor, NO version and NO Bundle', () => {
    const cloudInit = SOURCE.slice(SOURCE.indexOf('#cloud-config'), SOURCE.indexOf('`;'));
    // Baking `cachebuster:0.2.0` into cloud-init is how this fleet silently ran stale code
    // for weeks. What runs is decided at deploy time, never at provision time.
    expect(cloudInit).not.toMatch(/\d+\.\d+\.\d+/);
    expect(cloudInit).not.toMatch(/KONTRA_ACTOR|bundle|registry|:5000/i);
  });

  it('does not install Docker', () => {
    // Nothing on a fleet Machine runs in a container. Leaving the install in would be minutes
    // of provision latency per Machine for a runtime nothing uses.
    expect(SOURCE).not.toMatch(/get\.docker\.com|docker run|docker pull/);
  });
});

describe('machine naming', () => {
  it('names machines kf-<role>-NN with an explicit name, never autonamed', () => {
    // Pulumi autonames by default (`temporal-a7f3c91`). An autonamed machine breaks every
    // inventory entry and log correlation that refers to kf-<role>-NN.
    expect(SOURCE).toMatch(/const name = `kf-\$\{args\.tag\}-\$\{String\(i \+ 1\)\.padStart\(2, '0'\)\}`/);
    expect(SOURCE).toMatch(/^\s+name,$/m);
  });
});

describe('stack dispatch', () => {
  it('refuses a project it does not know', () => {
    // The route picks WHICH stack; it must never pick WHAT the stack contains.
    expect(() => programFor({ stackFqn: 'evil-project/x' })).toThrow(/unknown infra project/);
  });

  it('builds the fleet program for the fleet project', () => {
    expect(programFor({ stackFqn: `${FLEET_PROJECT}/run-1`, args: { tag: 'crawl', machines: 2 } }))
      .toBeTypeOf('function');
  });
});

describe('argument coercion', () => {
  it('coerces machines to a number so a string cannot reach the provider', () => {
    expect(coerceFleetArgs({ tag: 'crawl', machines: '3' }).machines).toBe(3);
  });

  it('drops unknown keys rather than forwarding them', () => {
    const out = coerceFleetArgs({ tag: 'crawl', machines: 1, evil: 'rm -rf' } as Record<string, unknown>);
    expect(out).not.toHaveProperty('evil');
  });

  it('keeps the DO defaults out of the caller\'s hands unless explicitly set', () => {
    const out = coerceFleetArgs({ tag: 'crawl', machines: 1 });
    expect(out.region).toBeUndefined();
    expect(FLEET_DEFAULTS.region).toBe('sfo3');
  });
});

/**
 * WHERE A FLEET LANDS, and the pairing that made kontra un-installable outside one project.
 *
 * `region` and `vpcUuid` were constants pointing at one developer's sfo3 VPC. A control plane in
 * any other region could only provision Machines in sfo3 — where they cannot reach its Temporal,
 * Redis or object store — and the symptom is not an error: the run HANGS on `fleet.ready()`,
 * because Machines that never register look exactly like Machines still booting. MEASURED on a
 * fresh nyc1 controller, which put its first fleet in sfo3.
 *
 * The trap is the PAIRING. A VPC is regional, so "a new region with the old VPC" is the one
 * combination DigitalOcean refuses outright — and it was also the only one a caller could produce,
 * since the Python `fleet.up()` exposes `region=` and no vpc knob at all.
 */
describe('fleet placement', () => {
  it("is unchanged when nothing is configured — this repo's own installation", () => {
    const d = fleetDefaults({});
    expect(d.region).toBe('sfo3');
    expect(d.vpcUuid).toBe('482bd33f-2f05-4541-9b82-1a62022b06a0');
    expect(d.size).toBe('s-1vcpu-2gb');
    expect(d.image).toBe('ubuntu-22-04-x64');
  });

  it('DROPS the sfo3 VPC when the region moves, rather than pairing two regions', () => {
    // The whole point. Inheriting the old uuid is the request DO rejects; an empty vpc lets it use
    // that region's default, which is what setting only a region meant.
    const d = fleetDefaults({ KONTRA_FLEET_REGION: 'nyc1' });
    expect(d.region).toBe('nyc1');
    expect(d.vpcUuid).toBe('');
  });

  it('keeps an explicit VPC, because somebody who names one means it', () => {
    const d = fleetDefaults({ KONTRA_FLEET_REGION: 'nyc1', KONTRA_FLEET_VPC: 'abc-123' });
    expect(d).toMatchObject({ region: 'nyc1', vpcUuid: 'abc-123' });
  });

  it('keeps the home VPC when the region is explicitly the home region', () => {
    expect(fleetDefaults({ KONTRA_FLEET_REGION: 'sfo3' }).vpcUuid).toBe(
      '482bd33f-2f05-4541-9b82-1a62022b06a0'
    );
  });

  it('treats blank and whitespace as unset, not as a value', () => {
    // A compose file passes "" for every variable an operator did not set, so empty MUST mean
    // "keep the default" — a region of '' would be sent to DigitalOcean verbatim.
    expect(fleetDefaults({ KONTRA_FLEET_REGION: '   ' }).region).toBe('sfo3');
    expect(fleetDefaults({ KONTRA_FLEET_SIZE: '' }).size).toBe('s-1vcpu-2gb');
  });

  it('takes size, image and ssh key ids when they are given', () => {
    const d = fleetDefaults({
      KONTRA_FLEET_SIZE: 's-2vcpu-4gb',
      KONTRA_FLEET_IMAGE: 'ubuntu-24-04-x64',
      KONTRA_FLEET_SSH_KEY_IDS: ' 111 , 222 ',
    });
    expect(d).toMatchObject({ size: 's-2vcpu-4gb', image: 'ubuntu-24-04-x64' });
    // Trimmed and split: an id with a stray space is not a key DigitalOcean knows.
    expect(d.sshKeyIds).toEqual(['111', '222']);
  });

  it('falls back to the known key ids rather than making machines nobody can log into', () => {
    expect(fleetDefaults({ KONTRA_FLEET_SSH_KEY_IDS: ' , ' }).sshKeyIds).toEqual([
      '44333901',
      '39835074',
    ]);
  });
});

describe('placing an actor', () => {
  it('names each Worker resource after its Machine AND its Actor', () => {
    // IT WAS `${m.name}-actor`, ONE PER MACHINE, and a name with no room for a second Artifact is
    // exactly why `place()` had to refuse one until slice 11. Two placements need two URNs, or the
    // second converge replaces the first's resource instead of joining it — which runs its teardown
    // and stops its Worker. `packing.test.ts` proves the resources themselves; this pins the name.
    expect(SOURCE).toMatch(/`\$\{m\.name\}-actor-\$\{actor\.name\}`/);
    expect(SOURCE).toMatch(/machines\.slice\(0, machinesFor\(p, args\.machines\)\)/);
  });

  it('re-runs the placement when the Artifact changes, and only then', () => {
    // Without `triggers` an unchanged command is skipped — right for everything else, exactly
    // wrong for a re-deploy, which would report success and leave the old Bundle running.
    expect(SOURCE).toMatch(/triggers: \[\s*p\.bundleSha/);
  });

  it('stops the Worker before its Machine goes, and only that Worker', () => {
    // A process killed with its host never runs @actor.close. A CONSTANT teardown would stop every
    // Worker on the Machine, which on a packed one takes the co-tenant down (machine.ts).
    expect(SOURCE).toMatch(/delete: teardown/);
    expect(SOURCE).toMatch(/machineTeardown\(actor\.name\)/);
  });

  it('reads the SSH key from disk instead of taking it as config', () => {
    // Config lands in Pulumi state. A private key in state is a private key in every backup
    // of that state.
    expect(SOURCE).toMatch(/readFileSync\(path, 'utf8'\)/);
    expect(SOURCE).not.toMatch(/privateKey\?:/);
  });

  it('echoes the placement back as a stack output', () => {
    // `fleet up --count 2` sends the whole desired state; without this the CLI cannot inherit
    // the placement it is not being asked to change, and a scale-up silently un-deploys. Since
    // packing it echoes the whole LIST, because inheriting one of two is that same un-deploy.
    expect(SOURCE).toMatch(/bundleSha: first\?\.bundleSha \?\? ''/);
    expect(SOURCE).toMatch(/^\s+placements,$/m);
  });

  it('runs the same command on update as on create', () => {
    // A `remote.Command` with only `create` is never re-run on an existing Machine, so a
    // re-deploy would converge clean and change nothing on the box.
    expect(SOURCE).toMatch(/create: install,\s*\n\s*update: install,/);
  });
});

describe('stack fqn', () => {
  it('round-trips', () => {
    expect(fqn(parseFqn('kontra-fleet/run-apex-119'))).toBe('kontra-fleet/run-apex-119');
  });

  it('rejects anything that could escape into a path', () => {
    // The fqn becomes a workflow id AND a directory under the state backend.
    for (const bad of ['../../etc/passwd', 'a/b/c', 'only-one-part', '/leading', 'a/', '/']) {
      expect(() => parseFqn(bad), bad).toThrow(/bad stack fqn/);
    }
  });
});

describe('sessions are not a placement property (ADR 0020)', () => {
  /**
   * This used to pin the opposite: that `--tmux` reached the Machine install and changed the
   * converge trigger. It does not any more, and the flag never actually worked — `coerceFleetArgs`
   * narrows untrusted JSON to strings and numbers, so the boolean the CLI sent was dropped before
   * this program saw it. Session existence is `tmuxSessionWorkflow` now, which is what lets a
   * Machine deployed without `--tmux` be given a Terminal with no re-deploy.
   */
  it('carries no tmux argument at all', () => {
    expect(SOURCE).not.toContain('args.tmux');
    expect(SOURCE).not.toMatch(/tmux\?:/);
  });

  it('still re-runs the placement when the Artifact or the Controller changes', () => {
    // Dropping the tmux element from `triggers` must not weaken what the triggers exist for: an
    // unchanged command is skipped, which for a re-deploy would report success and leave the old
    // Bundle running.
    expect(SOURCE).toContain("p.bundleSha ?? ''");
    expect(SOURCE).toContain('actor.controller,');
  });
});

describe('density (maxSessions)', () => {
  it('is coerced as a NUMBER, which the string loop would have dropped', () => {
    // Exactly how `--tmux` was lost: `coerceFleetArgs` narrows untrusted JSON field by field, and
    // a field absent from the list is silently gone before the program sees it.
    expect(coerceFleetArgs({ tag: 'crawl', machines: 1, maxSessions: 12 }).maxSessions).toBe(12);
    expect(coerceFleetArgs({ tag: 'crawl', machines: 1, maxSessions: '12' }).maxSessions).toBe(12);
  });

  it('treats absent, zero and junk alike as "leave the host default alone"', () => {
    for (const raw of [undefined, null, 0, '', 'lots', Number.NaN]) {
      expect(coerceFleetArgs({ tag: 'crawl', machines: 1, maxSessions: raw }).maxSessions)
        .toBeUndefined();
    }
  });

  it('re-runs the placement when ONLY the density changed', () => {
    // The one that is easy to miss. The cap is written into worker-<actor>.env, which only this
    // command writes — so without it in `triggers`, a converge changing nothing else is skipped,
    // reports success, and leaves the fleet on its old cap with nothing saying so.
    expect(SOURCE).toContain("String(p.maxSessions ?? '')");
  });

  it('echoes the density back so a scale-up does not silently reset it', () => {
    // `fleet up` inherits the placement it is not changing. A number that came back as '' would
    // coerce to NaN and fail the next converge, so the unset case is 0.
    expect(SOURCE).toContain('maxSessions: first?.maxSessions ?? 0');
  });

  it('carries the density INSIDE each placement, not once for the Fleet', () => {
    // Two Artifacts on one Fleet want two different caps — `nscheck` at 8 Sessions beside
    // `subfinder` at 1 is the ADR's own example — so a converge-level `maxSessions` would be one
    // answer to a question that now has N. It survives at the converge level only as the
    // single-placement spelling `coerceFleetArgs` folds in.
    const packed = coerceFleetArgs({
      tag: 'crawl',
      machines: 2,
      placements: [
        { actorName: 'a', bundleUrl: 'http://c/a', maxSessions: 8 },
        { actorName: 'b', bundleUrl: 'http://c/b', maxSessions: 1 },
      ],
    });
    expect(packed.placements?.map((p) => p.maxSessions)).toEqual([8, 1]);
    expect(packed.maxSessions).toBeUndefined();
  });
});

/**
 * ═══ `workers=`: HOW MANY MACHINES ONE PLACEMENT LANDS ON (ADR 0037, slice 11) ═══
 *
 * The distribution is a pure function so it can be asked directly. What it decides is which Machines
 * a Fleet's Workers are on, and every wrong answer is expensive in the same way: a Worker that moves
 * is a `machineTeardown` on a Machine nobody asked to touch.
 */
describe('workers (how many Machines a placement lands on)', () => {
  const p = (workers?: number) => ({ actorName: 'a', bundleUrl: 'http://c/a', workers });

  it('takes every Machine when nobody said otherwise', () => {
    // The pre-packing behaviour, kept: every existing Fleet places on all of its Machines and none
    // of them says so. An implementation that defaulted to 1 would silently shrink every Fleet in
    // this repo to one Worker on the next converge.
    expect(machinesFor(p(), 4)).toBe(4);
    expect(machinesFor(p(), 0)).toBe(0);
  });

  it('takes a prefix, so a scale-up adds Workers and never moves one', () => {
    expect(machinesFor(p(2), 4)).toBe(2);
    // The same placement on a Fleet grown to six is still the same two Machines PLUS whatever the
    // count now says — the first two never change hands, which is what makes a scale-up cheap.
    expect(machinesFor(p(2), 6)).toBe(2);
  });

  it('refuses more Workers than Machines rather than truncating', () => {
    // A placement puts at most ONE Worker on a Machine: two of one actor@version there would share
    // a KONTRA_WORKER label, a unit name and a queue. Truncating would leave the caller believing
    // in four Workers that were never built — and believing it permanently, since the converge
    // succeeds.
    expect(() => machinesFor(p(8), 4)).toThrow(/8 Workers on a Fleet of 4/);
    expect(() => machinesFor(p(8), 4)).toThrow(/maxSessions/);
  });

  it('refuses a count that is not a whole positive number', () => {
    for (const bad of [0, -1, 1.5, Number.NaN]) {
      expect(() => machinesFor(p(bad), 4), String(bad)).toThrow(/workers=/);
    }
  });

  it('is a number on the wire and never a boolean', () => {
    // `spread=` is a boolean at the Python call site and resolves to this NUMBER before anything
    // crosses, because `coerceFleetArgs` narrows to strings and numbers — a boolean would be
    // dropped and one Worker per Machine would quietly become whatever the default was, which is
    // how `--tmux` rode a whole release.
    expect(coerceFleetArgs({
      tag: 'crawl',
      machines: 2,
      placements: [{ actorName: 'a', bundleUrl: 'http://c/a', workers: '2' }],
    }).placements?.[0].workers).toBe(2);
    for (const raw of [undefined, null, 0, '', 'lots', Number.NaN]) {
      expect(coerceFleetArgs({
        tag: 'crawl',
        machines: 2,
        placements: [{ actorName: 'a', bundleUrl: 'http://c/a', workers: raw }],
      }).placements?.[0].workers, String(raw)).toBeUndefined();
    }
  });
});

/**
 * The reader's half of packing. `coerceFleetArgs` is the ONLY thing between untrusted JSON and the
 * one provider-coupled file in the stack, and an array is the one shape it had no rule for.
 */
describe('coercing placements', () => {
  const base = { tag: 'crawl', machines: 2 };

  it('folds the single-placement spelling into a one-entry list', () => {
    // `kontra fleet deploy` and every stack created before packing send the scalars. `placementsOf`
    // is where they become a placement, and nothing downstream reads them again — two sources of
    // truth for one desired state is the drift `--tmux` already cost a release.
    const args = coerceFleetArgs({
      ...base,
      actorName: 'nscheck',
      actorVersion: '0.1.0',
      bundleUrl: 'http://c/n',
      bundleSha: 'n'.repeat(64),
      controller: '10.0.0.1',
      maxSessions: 8,
    });
    expect(args.placements).toBeUndefined(); // the fold happens in the program, not the reader
    expect(placementsOf(args)).toEqual([
      {
        actorName: 'nscheck',
        actorVersion: '0.1.0',
        actorEngine: undefined,
        bundleUrl: 'http://c/n',
        bundleSha: 'n'.repeat(64),
        controller: '10.0.0.1',
        maxSessions: 8,
      },
    ]);
  });

  it('prefers the list when both spellings arrive', () => {
    // A writer that sends both sends them DERIVED from one another (kontra.fleet does, for one
    // placement). A reader that merged them would have to decide which wins, and the wrong answer
    // is a Fleet running an Artifact nobody named.
    expect(
      placementsOf(
        coerceFleetArgs({
          ...base,
          actorName: 'stale',
          bundleUrl: 'http://c/stale',
          placements: [{ actorName: 'fresh', bundleUrl: 'http://c/fresh' }],
        })
      ).map((p) => p.actorName)
    ).toEqual(['fresh']);
  });

  it('sorts by actor name, so the caller cannot move a Worker by reordering its arguments', () => {
    // The order decides each Worker's metrics port on a packed Machine, which is in the remote
    // command's triggers. Object key order is not a contract; a sort is.
    expect(
      placementsOf(
        coerceFleetArgs({
          ...base,
          placements: [
            { actorName: 'subfinder', bundleUrl: 'http://c/s' },
            { actorName: 'nscheck', bundleUrl: 'http://c/n' },
          ],
        })
      ).map((p) => p.actorName)
    ).toEqual(['nscheck', 'subfinder']);
  });

  it('drops an entry with nothing placeable in it, and keeps the rest', () => {
    // A placement with no bundleUrl would build a Worker that curls the empty string; one with no
    // actorName would write a co-tenant's paths, since every path on the Machine is named after it.
    const args = coerceFleetArgs({
      ...base,
      placements: [
        { actorName: 'a', bundleUrl: 'http://c/a' },
        { actorName: 'b' },
        { bundleUrl: 'http://c/c' },
        'not an object',
        null,
        { actorName: 'd', bundleUrl: 'http://c/d' },
      ],
    });
    expect(args.placements?.map((p) => p.actorName)).toEqual(['a', 'd']);
  });

  it('drops a key inside an entry that the program was not told about', () => {
    // The same wordless narrowing `evil: 'rm -rf'` gets one level up. A caller cannot reach the
    // program through an unknown key, nested or not.
    const args = coerceFleetArgs({
      ...base,
      placements: [
        { actorName: 'a', bundleUrl: 'http://c/a', evil: 'rm -rf /', region: 'nyc3', credential: 'do-prod' },
      ],
    });
    expect(Object.keys(args.placements![0]).sort()).toEqual(['actorName', 'bundleUrl']);
  });

  it('tells an EMPTY list apart from an absent one', () => {
    // `placements: []` says this Fleet places nothing, which is a real desired state — it is what
    // `fleet.hold()` converges. Absent says "read the scalars instead". A reader that treated the
    // two alike would either place nothing on a Fleet that asked for one Artifact, or fold a
    // Machines-only converge's empty scalars into a placement with no Bundle.
    expect(placementsOf(coerceFleetArgs({ ...base, placements: [] }))).toEqual([]);
    expect(placementsOf(coerceFleetArgs({ ...base, actorName: 'a', bundleUrl: 'http://c/a' }))
      .length).toBe(1);
    expect(placementsOf(coerceFleetArgs({ ...base }))).toEqual([]);
  });

  it('lets an empty list BEAT the scalars, rather than falling back to them', () => {
    // THE CASE THAT SEPARATES THE TWO SPELLINGS, and the one a truthiness check gets wrong: `[]` is
    // truthy in JavaScript, so `args.placements ? … : …` and `args.placements.length > 0 ? … : …`
    // agree on every input EXCEPT this one. A writer that sends both is saying "un-place what these
    // scalars describe", and falling back would keep the Worker it was asked to remove — a converge
    // that reports success and changes nothing, which is the whole failure mode of this file.
    expect(
      placementsOf(
        coerceFleetArgs({
          ...base,
          actorName: 'stale',
          bundleUrl: 'http://c/stale',
          bundleSha: 's'.repeat(64),
          placements: [],
        })
      )
    ).toEqual([]);
  });

  it('drops an entry whose required keys are EMPTY, not just missing', () => {
    // `""` is not the same as absent to a `typeof` check, and it is what a writer produces from an
    // unset variable rather than from a forgotten key — `bundleUrl: ''` would place a Worker that
    // curls the empty string, and `actorName: ''` would write `/etc/kontra/worker-.env` and
    // `kontra-actor-.service`, which are the paths of no Worker and the paths every OTHER Worker
    // with an empty name would also write.
    const args = coerceFleetArgs({
      ...base,
      placements: [
        { actorName: '', bundleUrl: 'http://c/a' },
        { actorName: 'b', bundleUrl: '' },
        { actorName: 'good', bundleUrl: 'http://c/good' },
      ],
    });
    expect(args.placements?.map((p) => p.actorName)).toEqual(['good']);
  });
});
