# 59. The state store refuses rather than forgetting

Date: 2026-10-06

## Status

**Accepted.** The first slice of the Phase 1 work on the commit path, and the one that stands alone:
it is correct on its own terms whether or not the checkpoint later moves into Temporal's history.

## Context

Redis ran with `--maxmemory-policy volatile-lru`.

`volatile-lru` evicts keys **that have a TTL**, and leaves keys without one alone. That reads like a
careful choice, and in kontra's keyspace it is precisely the wrong one, because of which keys have a
TTL:

| key | TTL | holds |
|---|---|---|
| `kontra-actor:<actorId>` | **24 h** | the engine's per-unit commit markers and resume scratch |
| `kontra-global:<actor>:<key>` | none | dedupe sets and counters |

So under memory pressure Redis deleted **the record of which Units had committed** — the one thing in
the keyspace whose loss is not a slowdown. A retry then reads an absent commit map and re-runs work
that already finished, or reads a partly-evicted one and skips work that did not. Nothing raises; the
run reports success over a Dataset that is missing records or holding duplicates.

The repository already contained the argument against this, in the appliance's own keyspace
(`cli/appliance/kv/keyspace.go`), which exempts `kontra-global:*` and says why:

> `kontra-global:*` has no TTL because it is a dedupe set or a counter, and evicting one does not
> degrade a run, it silently corrupts it. So when nothing volatile is left, this REFUSES the write.

And in the same file, the sentence that makes the hazard explicit without naming it as one:

> Every volatile key in this keyspace is one actor's state hash carrying the same 24 h TTL.

The commit map had exactly the property the exemption was written for, and none of the protection.
`--appendonly yes` with the default `appendfsync everysec` added a second problem of the same kind: a
one-second window in which a commit marker is acknowledged and then lost to a crash.

**Measured on the install where this was found: 1.46 MiB of 512 MiB in use, 79 keys,
`evicted_keys:0`.** The hazard was latent, never fired here, and the fix costs nothing today — which
is the best moment to take it, not a reason to skip it.

## Decision

1. `--maxmemory-policy noeviction`. A write that would exceed the bound is **refused**, which reaches
   the actor as an error it can retry or fail on.
2. `--appendfsync always`. Every write is on disk before it is acknowledged.
3. Both in all three places the store is defined: `docker-compose.yml` (and therefore
   `docker-compose.quickstart.yml`, which is a symlink to it) and `control/pulumi/Pulumi.yaml`.

**Loud and recoverable beats silent and wrong.** That is the whole decision, and it is the same trade
the appliance made when it chose to refuse rather than evict its last protected key.

### Why not simply raise `maxmemory`

It moves the cliff without changing what is at the bottom of it. At 512 MiB with `volatile-lru` the
corruption happens at 512 MiB; at 4 GiB it happens at 4 GiB, on a bigger run, further from anyone
who could connect it to a cause. A bound that is refused is a bound you can reason about.

### Why this is not the whole Phase 1 fix

`noeviction` stops the store from *forgetting* the commit map. It does not make the commit map
durable: the hash still carries a 24 h TTL, and it still lives in a cache rather than in the run's
own history. The rest of Phase 1 moves the checkpoint into the Temporal activity heartbeat, where it
is part of the workflow's history and survives anything the store does. This ADR is the floor under
that work, not a substitute for it.

## Consequences

- **A full store now fails writes instead of evicting.** On this install that is a change with no
  present effect (0.3% of the bound in use, zero evictions ever), and the failure names itself when
  it does happen.
- `appendfsync always` costs a fsync per write. What remains in this store after Phase 1 is
  `global_state` and `object_state` — dedupe sets and counters, not a throughput path — and a lost
  dedupe entry is a duplicate record.
- **Two documentation claims were wrong and are corrected.** `docs/wiki/Writing-Actors-Go.md` said
  "Durable state and per-Unit progress live in Redis" and `docs/wiki/Deployment.md` said a retry
  "replays its per-unit state from Redis, so the run completes" — both unconditional, both true only
  while the key had not been evicted.
- **The README's exactly-once claim is corrected.** It said a Go handler "owns the workflow and the
  exactly-once reload". Dispatch is **at-least-once**, as Temporal activities are; what makes it safe
  is that a Unit's commit marker is written before anything after it can run, and that records are
  keyed by position so a re-push lands in the same place rather than twice. The wiki already had the
  precise form of this — "Execution is at-least-once; result recording is exactly-once"
  (`Durability-and-Failures.md`, `Execution-Model.md`) — so the README was the outlier, not the rule.

## Tests

`control/orchestrator/src/compose.test.ts` — three. The policy is asserted by name, and
`volatile-lru`/`allkeys-lru` are asserted **absent** so a revert reads as a revert rather than merely
as a different string. `appendfsync always` is asserted, and a third assertion checks the helper is
reading a real `redis` service block, because a service lookup that returned nothing would pass both
of the others.
