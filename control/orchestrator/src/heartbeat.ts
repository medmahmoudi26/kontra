/**
 * Heartbeat shapes and the pure wire -> row mapping, kept apart from `temporalClient.ts` so it
 * can be unit-tested. Importing `temporalClient` under vitest fails to resolve `@temporalio/proto`
 * (via `visibility.ts`), which is why the mapping that silently dropped `isolated` had no test
 * covering it in the first place. Same reasoning as `codec/shard.ts`: the logic worth pinning
 * does not need the transport.
 */

/** Live per-node heartbeat progress (roadmap platform-x100 #06). */
export interface NodeHeartbeat {
  node: string;
  done: number;
  total: number;
  /** Units this node PERMANENTLY DROPPED — see heartbeatRow for why this is not optional. */
  isolated: number;
  attempt: number;
  lastBeat: number; // epoch ms; 0 when never beaten
  /**
   * The author's `@actor.healthcheck` map, VERBATIM — `{program, at, found, hosts, probes, …}`
   * for the desync scanner.
   *
   * IT USED TO BE DROPPED HERE, and that was the whole reason the console had nothing worth
   * rendering. The actor beats this every two seconds (`engine.go::progressBeat`), Temporal
   * stores and serves it on the pending activity, and this mapper then built a row out of
   * `done/total/isolated` and discarded the rest — so the only fields that reached an API were
   * the ones that describe the machine, never the ones that say which PROGRAM, which HOST and
   * which PATH the worker is on, or whether it has found anything.
   *
   * Deliberately `unknown` and passed through untouched: the shape is the actor author's, this
   * file has no business knowing desync's keys, and a typed subset here would silently drop the
   * next actor's fields exactly as it dropped these.
   */
  progress?: Record<string, unknown>;
}

/**
 * The decoded `RunBatch` heartbeat payload.
 *
 * The emitter is the ACTOR, not the handler (ADR 0018): `_beat` in
 * runtime/python/internals/engine.py. It used to be a Go struct in runtime/handler/activity.go, back
 * when the handler drove the actor through a sidecar and beat on its behalf.
 *
 * Every field is optional because the wire is another process's build, which may predate any
 * given field — which also means a RENAMED field does not error here, it defaults to 0 and the
 * monitor shows 0/0 forever. That has happened; tests/test_actor_engine.py pins the names on the
 * emitting side for exactly that reason.
 */
export interface HeartbeatDetail {
  node?: string;
  done?: number;
  total?: number;
  isolated?: number;
  /** The author's healthcheck map. Optional like everything else here — an actor that defines no
   * `@actor.healthcheck` beats liveness only, and that is a normal actor, not a broken one. */
  progress?: Record<string, unknown>;
}

/**
 * Build the API row for one decoded heartbeat.
 *
 * `isolated` is deliberately a plain number rather than `number | undefined`. The discriminator
 * for "we know nothing about this node" is the absence of an entry in the heartbeat map, which
 * is what cli/monitor.go's `isolatedCol(hb, ok)` keys its "-" off; a row that exists always
 * carries a count. Widening it to undefined here would add a second, redundant unknown that
 * every consumer would have to re-collapse — and the whole point of this column is that
 * "unknown" and "zero" stay distinguishable at exactly one layer.
 */
export function heartbeatRow(
  detail: HeartbeatDetail,
  fallbackNode: string,
  attempt: number,
  lastBeat: number
): NodeHeartbeat {
  return {
    node: detail.node || fallbackNode,
    done: detail.done ?? 0,
    total: detail.total ?? 0,
    isolated: detail.isolated ?? 0,
    attempt,
    lastBeat,
    // ABSENT, NOT EMPTY, when the actor beat none. `{}` would read as "healthcheck ran and had
    // nothing to say", which is a different fact from "this actor defines no healthcheck" — and
    // a panel that cannot tell them apart shows an empty progress row for a healthy worker.
    ...(detail.progress ? { progress: detail.progress } : {}),
  };
}
