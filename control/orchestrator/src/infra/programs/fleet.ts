/**
 * The fleet program — the Pulumi port of `infra/tofu/main.tf` (ADR 0019).
 *
 * This is the ONLY provider-coupled file in the stack, exactly as `main.tf` was. Everything
 * above it talks about **Machines** and **Roles**; moving clouds means editing this file and
 * nothing else.
 *
 * The deleted `main.tf` encoded three production incidents. They are ported deliberately, and
 * one of them had to get STRICTER in translation:
 *
 *   - `ignoreChanges: ['image']` — an image slug moving would otherwise recreate the whole
 *     fleet mid-run. Terraform's `ignore_changes = [image]` covered only that; in Pulumi
 *     `userData` and `sshKeys` are ALSO replacement triggers, so an innocent cloud-init edit
 *     would destroy a live fleet. All three are ignored here.
 *   - cloud-init installs **Docker only** — no actor image and no version. Baking
 *     `cachebuster:0.2.0` into cloud-init is how this fleet silently ran stale code for weeks.
 *     What runs is decided at deploy time against an immutable artifact.
 *   - the inventory is built from resources this program created, so the **Controller is
 *     structurally absent** — it cannot appear the way it did when inventory was queried from
 *     the cloud API. A Controller in the fleet inventory means an actor container on the
 *     control plane.
 */

import * as command from '@pulumi/command';
import * as digitalocean from '@pulumi/digitalocean';
import * as pulumi from '@pulumi/pulumi';
import { readFileSync } from 'node:fs';
import {
  METRICS_PORT_BASE,
  machineInstall,
  machineTeardown,
  type MachineActor,
} from './machine';

/** A machine's workload assignment — a tag and an inventory group (see infra/CONTEXT.md).
 * Bounded so it is safe to interpolate into a tag, a group name and a machine name. */
const TAG_RE = /^[a-z][a-z0-9-]{1,15}$/;

export interface FleetArgs {
  /**
   * Which machines this actor lands on — a label, and nothing dispatches on it.
   *
   * IT WAS CALLED `role`, which claimed more than it does. Nothing in kontra ever behaved
   * differently because a Machine was `dns` rather than `crawl`; the string becomes a
   * DigitalOcean tag, an inventory group and the `kf-<tag>-NN` name prefix, and that is the
   * whole of it. NOT an enum of known workloads — a new actor must not have to edit this file
   * or mislabel its infrastructure.
   */
  tag: string;
  /** How many machines. The real scale knob for the fleet; density is `maxSessions`. */
  machines: number;
  region?: string;
  size?: string;
  image?: string;
  vpcUuid?: string;
  sshKeyIds?: string[];

  // ---- placing an actor on the Machines ---------------------------------------------
  //
  // Optional. With none of these set the stack is Machines only, which is what `fleet up`
  // does. `fleet deploy` re-converges the SAME stack with them set, so placement is a
  // declarative property of the Fleet rather than a side effect somebody has to remember to
  // repeat — the thing that let this fleet run stale code three times.
  //
  // The actor runs NATIVELY. There is no image and no container on a fleet Machine: see
  // ./machine.ts for why the container layer earned nothing here.

  /**
   * EVERY Artifact this Fleet places, since ADR 0037's other half (slice 11).
   *
   * ═══ THE WHOLE LIST, EVERY CONVERGE, OR IT IS A TEARDOWN ═══
   *
   * Pulumi's desired state is TOTAL, and that is not a caution here, it is the type. A placement
   * that stops appearing in this array is a request to DELETE its `command.remote.Command`, which
   * runs `machineTeardown` and stops that Worker. So a writer that sends one placement onto a Fleet
   * carrying two has not "updated one of them" — it has removed the other, successfully, with
   * nothing raising on either side. `shared/conformance/placement.json` is the corpus that keeps the two
   * writers honest about it.
   *
   * The single-placement keys BELOW are the pre-slice-11 spelling and still work: `coerceFleetArgs`
   * folds them into a one-entry array when this is absent, so `kontra fleet deploy` and every stack
   * created before this change converge to exactly what they converged to before.
   */
  placements?: PlacementArgs[];

  // ---- the single-placement spelling, folded into `placements` on arrival ------------------
  /** The Artifact: a Bundle, fetched from the Controller and verified by this sha. */
  bundleUrl?: string;
  bundleSha?: string;
  actorName?: string;
  // There is deliberately no `tmux` arg (ADR 0020). Session existence is a Temporal converge on the
  // infra queue now, not a property of a placement — which is what lets a Machine deployed without
  // it be given a Terminal on demand, with no re-deploy. It had also never worked as an arg:
  // `coerceFleetArgs` narrows untrusted JSON to strings and numbers, so the boolean the CLI sent
  // was dropped before it ever reached this program.
  actorVersion?: string;
  actorEngine?: string;
  /** Where the Machines call home — Temporal, the catalog, S3, Redis. */
  controller?: string;
  /** Live Sessions per Machine — the density knob named in `machines` above. See MachineActor. */
  maxSessions?: number;
}

/**
 * One Artifact on this **Fleet** — what `f.place(actor, version, …)` asks for.
 *
 * A PLACEMENT PUTS AT MOST ONE WORKER ON ANY ONE MACHINE, and that is structural rather than a
 * policy. Two Workers of one `<actor>@<version>` on one Machine would carry the same
 * `KONTRA_WORKER` label (`cli/driver.go`), write the same two systemd units, and poll the same
 * queue — so `list()` could not tell them apart and `cli/warden.go:reconcile` already refuses the
 * duplicate out loud. More concurrency for ONE Artifact on ONE Machine is `maxSessions`, which is
 * density and is the axis ADR 0037 §6 says packing does not replace.
 */
export interface PlacementArgs {
  actorName: string;
  actorVersion?: string;
  actorEngine?: string;
  /** The Artifact: a Bundle, fetched from the Controller and verified by this sha. */
  bundleUrl: string;
  bundleSha?: string;
  /** Where these Workers call home. Empty takes the Fleet's. */
  controller?: string;
  /** Live Sessions per Worker. See MachineActor.maxSessions. */
  maxSessions?: number;
  /**
   * How many of the Fleet's Machines this placement lands on, one Worker each. Absent means every
   * Machine, which is what every Fleet did before packing existed.
   *
   * THE FIRST `workers` MACHINES, IN NAME ORDER, AND THAT IS A DECISION. The alternative — spreading
   * a short placement across the Fleet, or balancing the packing — moves an existing Worker whenever
   * the Machine count changes, and moving a Worker is a `machineTeardown` on a Machine nobody asked
   * to touch. Taking a prefix means a scale-up ADDS Workers and never relocates one.
   */
  workers?: number;
}

/** The sfo3 VPC this repo was developed against, and the region it belongs to. */
const HOME_REGION = 'sfo3';
const HOME_VPC = '482bd33f-2f05-4541-9b82-1a62022b06a0';

/**
 * Where a fleet is provisioned, and on what.
 *
 * ── THESE WERE CONSTANTS AND THEY ARE CONFIGURATION ───────────────────────────────────────────
 *
 * `region` and `vpcUuid` were hard-coded to one developer's sfo3 project. A control plane
 * installed anywhere else could not put a fleet in its own region: `coerceFleetArgs` accepts a
 * region off the wire, but the Python `fleet.up()` exposes `region=` and NO vpc knob — so the one
 * combination reachable from a caller was a NEW region with the OLD VPC, which DigitalOcean
 * rejects outright because a VPC is regional. MEASURED on a fresh nyc1 controller: the first fleet
 * run put its Machines in sfo3, where they could not reach the controller's Temporal or Redis, and
 * the run hung on `f.ready()` until it was cancelled. Editing the actor's `actor.json` region does
 * nothing — that field never reaches this program.
 *
 * ── WHY AN UNSET VPC IS EMPTY RATHER THAN THE OLD ONE ─────────────────────────────────────────
 *
 * The pairing is the trap, so the default encodes the pairing. Setting the region alone must not
 * silently keep an sfo3 VPC: an empty `vpcUuid` lets DigitalOcean place the Machines in that
 * region's DEFAULT VPC, which is the answer somebody who set only a region meant. The historical
 * uuid is kept for the historical region alone, so this repo's own installation is byte-identical
 * to what it was and every other installation gets something that can work.
 *
 * `sshKeyIds` are DigitalOcean key ids, and they are account-scoped: another account's fleet gets
 * machines nobody can log into, which is invisible until a Terminal is opened on one.
 */
export function fleetDefaults(env: NodeJS.ProcessEnv = process.env) {
  const region = (env.KONTRA_FLEET_REGION ?? '').trim() || HOME_REGION;
  const vpc = (env.KONTRA_FLEET_VPC ?? '').trim();
  const keys = (env.KONTRA_FLEET_SSH_KEY_IDS ?? '')
    .split(',')
    .map((k) => k.trim())
    .filter(Boolean);
  return {
    region,
    size: (env.KONTRA_FLEET_SIZE ?? '').trim() || 's-1vcpu-2gb',
    image: (env.KONTRA_FLEET_IMAGE ?? '').trim() || 'ubuntu-22-04-x64',
    vpcUuid: vpc || (region === HOME_REGION ? HOME_VPC : ''),
    sshKeyIds: keys.length > 0 ? keys : ['44333901', '39835074'],
  };
}

/** The values this process was started with. Read once — a provision must not change region
 *  half-way through because somebody edited the environment of a running worker. */
export const FLEET_DEFAULTS = fleetDefaults();

/**
 * Deliberately almost empty, and deliberately carrying NO actor, no version and no Bundle.
 *
 * It used to install Docker. Nothing on a fleet Machine runs in a container any more, so that
 * install was pure latency on every provision and a package set we never used. What a Machine
 * needs to receive a Worker is python3 and curl, and the Ubuntu image already has python3.
 *
 * Baking `cachebuster:0.2.0` into cloud-init is how this fleet silently ran stale code for
 * weeks; what runs is decided at deploy time against a content-pinned Artifact, never here.
 */
const CLOUD_INIT = `#cloud-config
package_update: true
packages:
  - curl
  - ca-certificates
  - python3-pip
`;

export function validateTag(tag: string): void {
  if (!TAG_RE.test(tag)) {
    throw new Error(
      `tag ${JSON.stringify(tag)} invalid: lowercase letters, digits and dashes, 2-16 chars ` +
        `(it becomes the DigitalOcean tag \`kontra-${tag}\`, the inventory group and the \`kf-${tag}-NN\` machine name)`
    );
  }
}

/**
 * Every placement this converge carries, in ONE representation.
 *
 * THE LEGACY KEYS ARE AN INPUT, NEVER A SECOND OUTPUT. `kontra fleet deploy` and every stack
 * created before slice 11 send `actorName`/`bundleUrl`/… at the top level; they are folded here into
 * a one-entry array, and everything downstream reads only the array. A program that read both would
 * be two sources of truth for one desired state, which is the drift `--tmux` already cost a release.
 *
 * SORTED BY ACTOR NAME, because the order decides a Worker's metrics port (see `fleetProgram`) and
 * a port that moved because a caller listed its placements differently would re-install a Worker
 * nobody asked to touch. Object key order is not a contract; a sort is.
 */
export function placementsOf(args: FleetArgs): PlacementArgs[] {
  // AN EMPTY ARRAY IS AN ANSWER AND IT BEATS THE SCALARS. `[]` says "this Fleet places nothing",
  // which is what `fleet.hold()` converges; ABSENT says "read the single-placement keys instead".
  // The two agree on every input but one — a converge carrying `placements: []` BESIDE the scalars,
  // which is a writer un-placing what those scalars describe. Falling back there would keep the
  // Worker it was asked to remove, and report success. (`args.placements` is only ever set by
  // `coerceFleetArgs` when an array actually arrived, so `undefined` and `[]` are distinguishable
  // here; `[]` is truthy, which is exactly why the length check was wrong.)
  const list =
    args.placements !== undefined
      ? args.placements
      : args.bundleUrl
        ? [
            {
              actorName: args.actorName ?? args.tag,
              actorVersion: args.actorVersion,
              actorEngine: args.actorEngine,
              bundleUrl: args.bundleUrl,
              bundleSha: args.bundleSha,
              controller: args.controller,
              maxSessions: args.maxSessions,
            },
          ]
        : [];
  return [...list].sort((x, y) => (x.actorName < y.actorName ? -1 : x.actorName > y.actorName ? 1 : 0));
}

/**
 * Which Machines one placement lands on: the first `workers` of them, all of them when unset.
 *
 * A COUNT LARGER THAN THE FLEET IS A REFUSAL, not a silent truncation. `workers: 8` on a
 * four-Machine Fleet is a caller who believes eight Workers of this Artifact are running; quietly
 * placing four would make the belief permanent and the shortfall invisible, and the honest reading
 * of "more concurrency on the Machines I have" is `maxSessions`, which is a different argument.
 */
export function machinesFor(p: PlacementArgs, machineCount: number): number {
  if (p.workers === undefined) return machineCount;
  if (!Number.isInteger(p.workers) || p.workers < 1) {
    throw new Error(
      `placement ${JSON.stringify(p.actorName)} asks for workers=${p.workers}; it is how many of ` +
        `this Fleet's Machines the Artifact lands on, so it is a whole number of at least 1`
    );
  }
  if (p.workers > machineCount) {
    throw new Error(
      `placement ${JSON.stringify(p.actorName)} asks for ${p.workers} Workers on a Fleet of ` +
        `${machineCount} Machine(s), and a placement puts at most ONE Worker on a Machine — two of ` +
        `one actor@version there would share a label, a unit name and a queue. Raise the Fleet's ` +
        `machine count, or ask for more concurrency per Worker with maxSessions (ADR 0037 §6)`
    );
  }
  return p.workers;
}

/** One machine's entry in the inventory Fleet publishes to the rest of the system. */
export interface MachineEntry {
  name: string;
  /** Private VPC address — how the Controller reaches it. */
  host: string;
  /** Public address. Unique per machine, which is the whole reason for one Worker per
   * Machine: passive recon sources rate-limit by source IP. */
  publicIp: string;
  tag: string;
}

/**
 * Build the fleet. Returns the stack outputs — an inventory keyed by machine name, which is
 * the narrow one-way handoff Fleet gives Execution.
 */
export function fleetProgram(args: FleetArgs) {
  return async (): Promise<Record<string, unknown>> => {
    validateTag(args.tag);
    if (!Number.isInteger(args.machines) || args.machines < 0) {
      throw new Error(`machines must be a non-negative integer, got ${args.machines}`);
    }

    const cfg = { ...FLEET_DEFAULTS, ...args };
    // `kontra-fleet` finds every Machine kontra owns; `kontra-<tag>` finds one fleet's.
    //
    // THERE IS NO THIRD TAG ANY MORE. A `run-<name>` one used to group Machines by "the
    // bounded period of work this fleet serves" — a period nothing in kontra ever measured, named
    // by a string the caller invented, defaulting to the tag it sat beside. The stack is now named
    // after what it PLACES (`<actor>-<version>`), so the fleet's identity is derivable and the
    // extra tag has nothing left to say.
    const tags = ['kontra-fleet', `kontra-${args.tag}`];

    const machines = Array.from({ length: args.machines }, (_, i) => {
      const name = `kf-${args.tag}-${String(i + 1).padStart(2, '0')}`;
      const droplet = new digitalocean.Droplet(
        name,
        {
          // Explicit name: an autonamed machine breaks every inventory and log correlation
          // that refers to `kf-<tag>-NN`.
          name,
          region: cfg.region,
          size: cfg.size,
          image: cfg.image,
          // OMITTED WHEN EMPTY, not sent as "". A VPC is regional, so the provider must be free to
          // place the Machine in the region's DEFAULT VPC when nobody named one — passing an empty
          // string asks for a VPC called "", and passing another region's uuid is rejected. See
          // `fleetDefaults` for why an unset vpc is empty rather than inherited.
          ...(cfg.vpcUuid ? { vpcUuid: cfg.vpcUuid } : {}),
          sshKeys: [...cfg.sshKeyIds],
          tags,
          userData: CLOUD_INIT,
        },
        {
          // See the header. All three of these force replacement in Pulumi; Terraform only
          // needed `image` guarded, so this is stricter than the file it replaces.
          ignoreChanges: ['image', 'userData', 'sshKeys'],
        }
      );
      // The name travels with the resource: a Pulumi Output cannot name a resource, and the
      // placement below needs a stable, synchronous logical name for its own URN.
      return { name, droplet };
    });

    // ═══ PLACE EVERY ARTIFACT THIS CONVERGE CARRIES ═══
    //
    // ONE RESOURCE PER (MACHINE, PLACEMENT), AND ITS NAME CARRIES THE ACTOR. That is what makes two
    // Actors survive one another's converges: their URNs are different, so a converge that mentions
    // both keeps both, and a converge that mentions one deletes ONLY that one. It used to be
    // `${m.name}-actor`, one per Machine — a name with no room for a second Artifact, which is
    // precisely why `place()` had to refuse one until now.
    //
    // A FLEET PLACED BEFORE THIS CHANGE IS RENAMED ONCE, and that is the cost, stated. Pulumi sees
    // `kf-dns-01-actor` leave and `kf-dns-01-actor-nscheck` arrive, so the Worker is re-installed and
    // the old resource's teardown runs. It is safe in EITHER order — the old teardown names the
    // pre-packing singleton units and the new install writes namespaced ones, and `machineInstall`
    // disarms the singletons itself (machine.ts step 8) — but it IS one restart per Machine.
    const placements = placementsOf(args);
    if (placements.length > 0) {
      const key = privateKey();
      placements.forEach((p, i) => {
        const actor: MachineActor = {
          name: p.actorName || args.tag,
          version: p.actorVersion ?? '0',
          engine: p.actorEngine === 'go' ? 'go' : 'py',
          bundleUrl: p.bundleUrl,
          bundleSha: p.bundleSha ?? '',
          controller: p.controller || args.controller || '',
          tag: args.tag,
          maxSessions: p.maxSessions,
          // ONE WORKER ON A MACHINE KEEPS THE HOSTS' OWN :9110 DEFAULT; A PACKED ONE MUST NOT.
          // Both actor hosts default to that port and the loser of the race logs one line and
          // carries on serving nothing (machine.ts:metricsPort), so a Fleet that packs hands each
          // placement its own port — by position in the sorted list, which is stable for a given
          // set of placements and therefore does not move a port under a Worker that did not change.
          metricsPort: placements.length > 1 ? METRICS_PORT_BASE + i : undefined,
        };
        const install = machineInstall(actor);
        const teardown = machineTeardown(actor.name);
        for (const m of machines.slice(0, machinesFor(p, args.machines))) {
          new command.remote.Command(
            `${m.name}-actor-${actor.name}`,
            {
              connection: {
                host: m.droplet.ipv4Address,
                user: 'root',
                privateKey: key,
                // cloud-init is still running when the API reports the Machine ready; SSH comes
                // up somewhere in the next few minutes. Historically this took up to 7.5.
                dialErrorLimit: 60,
                perDialTimeout: 15,
              },
              create: install,
              update: install,
              // Teardown is part of the resource, so `fleet down` stops the Worker before the
              // Machine goes — a process killed with its host never runs @actor.close. It names
              // THIS Worker's units and nothing else: see machine.ts:machineTeardown for why a
              // constant here would stop a co-tenant.
              delete: teardown,
              // Re-run when the ARTIFACT or the Controller changes, and only then. Without this
              // an unchanged command is skipped, which is right for everything else and exactly
              // wrong for a re-deploy: it would report success and leave the old Bundle running.
              //
              // `maxSessions` is in here for the same reason and it is easy to miss: it is written
              // into `worker-<actor>.env`, which only this command writes, so a converge that
              // changed ONLY the density would otherwise be a no-op reported as success — the fleet
              // keeps its old cap and the caller has no way to tell. `metricsPort` is in the list
              // for exactly that reason too: it moves when a Fleet starts or stops packing, and a
              // Worker left on the old port is one nothing scrapes.
              triggers: [
                p.bundleSha ?? '',
                actor.controller,
                String(p.maxSessions ?? ''),
                String(actor.metricsPort ?? ''),
              ],
            },
            { dependsOn: [m.droplet] }
          );
        }
      });
    }

    const inventory = pulumi
      .all(
        machines.map((m) =>
          pulumi.all([m.droplet.name, m.droplet.ipv4AddressPrivate, m.droplet.ipv4Address])
        )
      )
      .apply((rows) =>
        rows.reduce<Record<string, MachineEntry>>((acc, [name, host, publicIp]) => {
          acc[name] = { name, host, publicIp, tag: args.tag };
          return acc;
        }, {})
      );

    const first = placements[0];
    return {
      inventory,
      tag: args.tag,
      machines: args.machines,
      // ═══ EVERY PLACEMENT, ECHOED, BECAUSE INHERITING ONE OF TWO IS A TEARDOWN ═══
      //
      // `kontra fleet up --count 6` re-converges a Fleet it is not being asked to re-place, and it
      // does that by reading its own placement back out of these outputs (`cli/fleet.go:fleetUp`).
      // While a Fleet held one Artifact the scalars below were enough. On a packed Fleet they are a
      // trap: inheriting only the first one sends a desired state with the second MISSING, which
      // deletes it and stops that Worker — a scale-up that silently un-deploys half the Fleet, which
      // is the very failure the echo was added to prevent, one Artifact later.
      placements,
      // The scalars stay for a reader that predates packing, and they describe the FIRST placement
      // in name order. A packed Fleet cannot be summarised by them and `cli/fleet.go` no longer
      // tries — it prefers `placements` whenever it is there.
      bundleUrl: first?.bundleUrl ?? '',
      bundleSha: first?.bundleSha ?? '',
      actorName: first?.actorName ?? '',
      actorVersion: first?.actorVersion ?? '',
      actorEngine: first?.actorEngine ?? '',
      controller: first?.controller ?? args.controller ?? '',
      // 0 rather than '' for "unset": every other echoed output is a string, and this one is
      // read back by `fleet up` to inherit the placement it is not changing. A `maxSessions` that
      // came back as '' would coerce to NaN and fail validation on the next converge.
      maxSessions: first?.maxSessions ?? 0,
    };
  };
}

/**
 * The SSH key the Controller uses to reach its Machines.
 *
 * Read from disk at converge time, not baked into config: a private key in Pulumi state is a
 * private key in every backup of that state. The Controller is the only host that has it,
 * which is the same reason it is the only host with the cloud credential.
 */
function privateKey(): string {
  const path = process.env.KONTRA_SSH_KEY ?? '/root/.ssh/id_rsa';
  try {
    return readFileSync(path, 'utf8');
  } catch (err) {
    throw new Error(
      `cannot read the fleet SSH key at ${path} (set KONTRA_SSH_KEY): ${(err as Error).message}`
    );
  }
}
