/**
 * Queue names and poller freshness — the pure half of `panels/pollers.ts`.
 *
 * NOT `control/orchestrator/src/queues.ts`, WHICH IS A DIFFERENT FILE WITH THE SAME NAME. That one holds the
 * orchestrator's INTERNAL routing constants — which of its own processes serves which queue. This
 * one derives the queue an ACTOR's Worker polls, from the actor's name and version, and it is a
 * cross-language contract. Adding an actor queue to the other file, or a role queue to this one,
 * would look right in both places.
 *
 * IT IS HERE BECAUSE THE CONSOLE NEEDS IT AND `pollers.ts` CANNOT MOVE. That module reaches
 * `panels/discovery`, which reaches `infra/stacks` and `infra/state` — Pulumi, a filesystem and a
 * server. None of that belongs in a browser bundle. These three exports have no dependencies at all.
 *
 * `sharedQueue` IS ONE OF FOUR ARMS OF `shared/conformance/queues.json`, and it stays inside the kontra
 * repository for that reason: the corpus is what keeps this derivation equal to the Go, Python and
 * CLI ones, and a copy in another repo would be a fifth implementation of a rule that has four. The
 * corpus names the failure — "the actor registers, polls a queue nobody schedules onto, and reports
 * as a healthy idle Worker while every run hangs to StartToClose."
 */

/**
 * The shared queue an actor's handler binds: `<actor>-<version>`, or `<actor>-shared` with no
 * version.
 *
 * ONE OF THE CROSS-LANGUAGE DERIVATIONS of this string, held to one answer by
 * `shared/conformance/queues.json` §shared, which every language executes — `queues.conformance.test.ts`
 * is this package's arm. The comment that stood here called this "a fourth derivation" and told
 * the reader to keep it in step with three named files by hand; two other files carried the same
 * instruction with different counts, and one named a file that had been renamed out of the tree.
 *
 * NOT the sessions queue (`<shared>-sessions`) the actor process itself polls. The shared queue is
 * what `cli/workers.go` describes and what `machine.ts` means by "the handler is what
 * `kontra workers list` counts".
 */
export function sharedQueue(actor: string, version: string): string {
  if (version !== '') return `${actor}-${version}`;
  return `${actor}-shared`;
}

/**
 * How stale a poll may be and still count as serving.
 *
 * MEASURED, not guessed. Temporal keeps a poller in `DescribeTaskQueue` for about five minutes
 * after it was last seen, so a worker killed thirty seconds ago is STILL LISTED — the identity
 * alone would report a dead worker as serving for five minutes. A long poll is 60 s, so a live
 * worker's `lastPoll` is never much older than that; two minutes is one comfortable long-poll of
 * headroom without inheriting Temporal's whole cache window.
 *
 * IT LIVES HERE, not in the SPA, and it moved rather than being copied. The Actors page reads it
 * through `run/workflowState.ts` (which re-exports this) and so does `panels/actorWorkers.ts`; the
 * SERVER needs the same window to refuse a probe aimed at a queue whose only pollers are dead
 * (`probe.ts`), and a second `now - t <= 120_000` written on this side is exactly how one surface
 * starts offering a worker the other calls stale.
 */
export const POLL_FRESH_MS = 120_000;

/**
 * Is ONE poll recent enough to mean "polling now"?
 *
 * `0` is NOT a fresh poll and not a stale one either — it is Temporal listing an identity without
 * dating it, which is not evidence of anything current. Callers that can say so must.
 */
export function pollIsFresh(lastPoll: number, now: number): boolean {
  if (lastPoll <= 0) return false;
  return now - lastPoll <= POLL_FRESH_MS;
}

/**
 * The host a poller identity names, or undefined when the identity is not in a shape we understand.
 *
 * Both SDK defaults put the hostname second: Go `<pid>@<hostname>@<queue>` (an empty queue on a
 * client, hence the trailing `@`), Python `<pid>@<hostname>`. Undefined is a real answer here and
 * the caller must not read it as "not this Machine" — a custom `Identity` is legal, and a Worker we
 * cannot attribute is `unknown`, not `none`.
 */
export function identityHost(identity: string): string | undefined {
  const parts = identity.split('@');
  if (parts.length < 2) return undefined;
  const host = (parts[1] ?? '').trim();
  return host === '' ? undefined : host;
}

/**
 * The producing half of {@link identityHost}: `<pid>@<host>@<queue>`, which every Worker in this
 * repository now passes to Temporal as its `identity`.
 *
 * IT SITS BESIDE THE PARSER ON PURPOSE. These two functions are one contract, and the failure mode
 * of splitting them is silent: a producer that drifts does not throw, it produces identities the
 * Monitor cannot attribute, and the column goes to `unknown` for a Machine that is perfectly
 * healthy. Written here, `identityHost(workerIdentity(...))` is a property a test can state.
 *
 * NOT A KONTRA SCHEME. This is the shape the GO SDK writes by default; Python's default drops the
 * queue and is widened to match, so one parser reads both halves of a Worker. That is the test for
 * whether a field belongs here at all — anything needing a new parser is a kontra label and belongs
 * on the record, not inside Temporal's identity.
 *
 * PURE, AND THEREFORE `pid` AND `host` ARE ARGUMENTS. `@kontra/core` is in the console's bundle;
 * `process.pid` and `os.hostname()` are not things a browser has. The caller that has them passes
 * them, exactly as every other derivation in this file takes its inputs rather than reading them.
 */
export function workerIdentity(pid: number | string, host: string, queue: string): string {
  return `${pid}@${host}@${queue}`;
}
