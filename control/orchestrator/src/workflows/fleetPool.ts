/**
 * THE FLEET POOL — one per fleet profile, the single writer of everything on its cluster
 * (ADR 0066 decision 7).
 *
 * WHAT IT HOLDS, all of it workflow state and none of it a secret:
 *   • the holds — `<holder>#<nonce>`, which run, in which tenant namespace, until when;
 *   • the placements — one Deployment per (tenant, actor@version), the holds that asked for it, and
 *     the replica count each asked for (the Deployment runs the largest);
 *   • which tenant namespaces have been bootstrapped, and whether the cluster exists.
 * The kubeconfig and the profile's cloud token never enter it: the activities read them inside the
 * infra container (ADR 0034's rule, kept).
 *
 * ONE WRITER, SERIALISED. Every cluster mutation — converge, tenant bootstrap, apply, delete, destroy
 * — goes through `serial`, one at a time in arrival order, so two runs placing and dropping at once
 * are two messages to this workflow and never two read-modify-writes of a Kubernetes object.
 *
 * HOLDS EXPIRE ON A CLOCK, as the Lease's always did: at a hold's deadline the pool asks Temporal
 * whether its holder still runs, renews it if so and drops it if not. A run that never exits its
 * scope (a control plane that died holding a fleet) cannot pin nodes for ever.
 *
 * THE NODES GO AFTER AN IDLE GRACE. When the last hold drops, the pool waits the profile's
 * `idle_minutes` for another; a hold in that window cancels the teardown and finds the cluster warm.
 */
import * as wf from '@temporalio/workflow';
import { ApplicationFailure } from '@temporalio/common';

import {
  POOL_DROP_SIGNAL,
  POOL_HOLD_UPDATE,
  POOL_LEASE_TTL_MS,
  POOL_PLACE_UPDATE,
  POOL_QUERY,
  POOL_TOUCH_SIGNAL,
  placementKey,
  type PlacementView,
  type PoolDrop,
  type PoolHold,
  type PoolPlace,
  type PoolState,
} from '../fleetPool';
import type * as activities from '../activities/fleetPool';

interface Lease {
  holder: string;
  namespace: string;
  ttlMs: number;
  expiresAt: number;
}

interface Placement {
  namespace: string;
  actor: string;
  version: string;
  deployment: string;
  image: string;
  /** replicas asked for, per holding lease. */
  asks: Record<string, number>;
  applied: number;
}

export interface PoolCarried {
  leases: [string, Lease][];
  placements: [string, Placement][];
  tenants: string[];
  converged: boolean;
  nodes: number;
  idleMinutes: number;
}

export interface PoolInput {
  profile: string;
  carried?: PoolCarried;
}

export const touchPool = wf.defineSignal<[]>(POOL_TOUCH_SIGNAL);
export const dropFromPool = wf.defineSignal<[PoolDrop]>(POOL_DROP_SIGNAL);
export const holdPool = wf.defineUpdate<{ lease: string; holds: number; expiresAt: number; nodes: number }, [PoolHold]>(
  POOL_HOLD_UPDATE
);
export const placeOnPool = wf.defineUpdate<{ deployment: string; image: string; replicas: number }, [PoolPlace]>(
  POOL_PLACE_UPDATE
);
export const poolState = wf.defineQuery<PoolState>(POOL_QUERY);

const act = wf.proxyActivities<typeof activities>({
  // A converge makes VMs or droplets and installs k3s: minutes, heartbeating.
  startToCloseTimeout: '45 minutes',
  heartbeatTimeout: '5 minutes',
  retry: { maximumAttempts: 3, initialInterval: '10 seconds' },
});

export async function kontraFleetPoolWorkflow(input: PoolInput): Promise<PoolState> {
  const { profile } = input;
  const leases = new Map<string, Lease>(input.carried?.leases ?? []);
  const placements = new Map<string, Placement>(input.carried?.placements ?? []);
  const tenants = new Set<string>(input.carried?.tenants ?? []);
  let converged = input.carried?.converged ?? false;
  let nodes = input.carried?.nodes ?? 0;
  let idleMinutes = input.carried?.idleMinutes ?? 15;
  let everHeld = leases.size > 0 || converged;
  let destroying = false;
  let destroyed = false;
  let generation = 0;

  // ONE CLUSTER MUTATION AT A TIME, in arrival order. A failed step does not poison the chain.
  let chain: Promise<unknown> = Promise.resolve();
  const serial = <T>(fn: () => Promise<T>): Promise<T> => {
    const next = chain.then(fn, fn);
    chain = next.catch(() => undefined);
    return next;
  };

  const ensureConverged = (): Promise<void> =>
    serial(async () => {
      if (converged) return;
      const out = await act.convergePool({ profile });
      converged = true;
      nodes = out.nodes;
      idleMinutes = out.idleMinutes;
    });

  const ensureTenant = (namespace: string): Promise<void> =>
    serial(async () => {
      if (tenants.has(namespace)) return;
      await act.bootstrapPoolTenant({ profile, namespace });
      tenants.add(namespace);
    });

  const view = (): PoolState => ({
    profile,
    converged,
    destroyed,
    nodes,
    leases: [...leases.entries()]
      .map(([lease, l]) => ({ lease, holder: l.holder, namespace: l.namespace, expiresAt: l.expiresAt }))
      .sort((a, b) => (a.lease < b.lease ? -1 : a.lease > b.lease ? 1 : 0)),
    placements: [...placements.values()]
      .map(
        (p): PlacementView => ({
          namespace: p.namespace,
          actor: p.actor,
          version: p.version,
          replicas: p.applied,
          deployment: p.deployment,
          image: p.image,
          holders: Object.keys(p.asks).sort(),
        })
      )
      .sort((a, b) => (a.deployment < b.deployment ? -1 : 1)),
  });

  wf.setHandler(touchPool, () => undefined);
  wf.setHandler(poolState, view);

  wf.setHandler(
    holdPool,
    async (h) => {
      if (destroying) throw ApplicationFailure.nonRetryable('this pool is tearing down; hold again', 'PoolEnding');
      generation += 1;
      everHeld = true;
      const ttlMs = h.ttlMs && h.ttlMs > 0 ? h.ttlMs : POOL_LEASE_TTL_MS;
      leases.set(h.lease, { holder: h.holder, namespace: h.holderNamespace, ttlMs, expiresAt: Date.now() + ttlMs });
      try {
        await ensureConverged();
        await ensureTenant(h.holderNamespace);
      } catch (err) {
        // A hold whose cluster could not be made is not a hold.
        leases.delete(h.lease);
        generation += 1;
        throw err;
      }
      return { lease: h.lease, holds: leases.size, expiresAt: leases.get(h.lease)?.expiresAt ?? 0, nodes };
    },
    {
      validator: (h) => {
        if (!h?.lease || !h.holderNamespace) throw new Error('a hold needs a lease id and the holder\'s namespace');
      },
    }
  );

  wf.setHandler(
    placeOnPool,
    async (p) => {
      if (!leases.has(p.lease)) {
        throw ApplicationFailure.nonRetryable(`lease ${p.lease} does not hold ${profile}; hold before placing`, 'NotHeld');
      }
      await ensureConverged();
      await ensureTenant(p.namespace);
      const key = placementKey(p.namespace, p.actor, p.version);
      const existing = placements.get(key);
      const placement: Placement = existing ?? {
        namespace: p.namespace,
        actor: p.actor,
        version: p.version,
        deployment: '',
        image: '',
        asks: {},
        applied: 0,
      };
      placement.asks[p.lease] = Math.max(1, Math.floor(p.replicas));
      placements.set(key, placement);
      const want = Math.max(...Object.values(placement.asks));
      if (want !== placement.applied || !placement.deployment) {
        const out = await serial(() =>
          act.applyPlacement({ profile, namespace: p.namespace, actor: p.actor, version: p.version, replicas: want })
        );
        placement.deployment = out.deployment;
        placement.image = out.image;
        placement.applied = want;
      }
      generation += 1;
      return { deployment: placement.deployment, image: placement.image, replicas: placement.applied };
    },
    {
      validator: (p) => {
        if (!p?.lease || !p.namespace || !p.actor || !p.version) throw new Error('a placement needs lease, namespace, actor and version');
        if (!Number.isInteger(p.replicas) || p.replicas < 1) throw new Error('replicas must be a whole number of at least 1');
      },
    }
  );

  wf.setHandler(dropFromPool, (d) => {
    if (!d?.lease) return;
    leases.delete(d.lease);
    generation += 1;
  });

  /** Placements nobody holds any more go, and a shrunk ask is applied. */
  const reconcilePlacements = async (): Promise<void> => {
    for (const [key, p] of [...placements.entries()]) {
      for (const lease of Object.keys(p.asks)) if (!leases.has(lease)) delete p.asks[lease];
      if (Object.keys(p.asks).length === 0) {
        if (p.deployment) {
          await serial(() => act.deletePlacement({ profile, namespace: p.namespace, actor: p.actor, version: p.version }));
        }
        placements.delete(key);
        continue;
      }
      const want = Math.max(...Object.values(p.asks));
      if (want !== p.applied) {
        await serial(() => act.applyPlacement({ profile, namespace: p.namespace, actor: p.actor, version: p.version, replicas: want }));
        p.applied = want;
      }
    }
  };

  /** Holds past their deadline: renewed if their run still runs, dropped if not. */
  const reap = async (): Promise<void> => {
    const due = [...leases.entries()].filter(([, l]) => l.expiresAt <= Date.now());
    if (due.length === 0) return;
    const { alive } = await act.poolHoldersAlive({
      holders: due.map(([, l]) => ({ workflowId: l.holder, namespace: l.namespace })),
    });
    const living = new Set(alive.map((h) => `${h.namespace}/${h.workflowId}`));
    for (const [lease, l] of due) {
      if (l.holder && living.has(`${l.namespace}/${l.holder}`)) l.expiresAt = Date.now() + l.ttlMs;
      else leases.delete(lease);
    }
  };

  let seen = generation;
  for (;;) {
    await reconcilePlacements();
    if (leases.size === 0) {
      if (!everHeld) break;
      // THE IDLE GRACE: a hold in this window cancels the teardown and finds the cluster warm.
      const resumed = await wf.condition(() => leases.size > 0, idleMinutes * 60_000);
      if (resumed) continue;
      break;
    }
    const earliest = Math.min(...[...leases.values()].map((l) => l.expiresAt));
    const signalled = await wf.condition(() => generation !== seen, Math.max(earliest - Date.now(), 1));
    seen = generation;
    if (!signalled) await reap();
    if (wf.workflowInfo().continueAsNewSuggested) {
      await wf.condition(wf.allHandlersFinished);
      await wf.continueAsNew<typeof kontraFleetPoolWorkflow>({
        profile,
        carried: {
          leases: [...leases.entries()],
          placements: [...placements.entries()],
          tenants: [...tenants],
          converged,
          nodes,
          idleMinutes,
        },
      });
    }
  }

  destroying = true;
  await wf.condition(wf.allHandlersFinished);
  if (converged) {
    await serial(() => act.destroyPool({ profile }));
    destroyed = true;
    converged = false;
  }
  return view();
}
