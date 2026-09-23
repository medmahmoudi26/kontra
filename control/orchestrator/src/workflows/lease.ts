/**
 * `fleetLeaseWorkflow` — the **Lease** workflow of **Leases** on one **Fleet**, and the thing that destroys its
 * **Machines** when the last one drops (ADR 0037).
 *
 * ═══ WHAT IT REPLACES ═══
 *
 * Until this slice, teardown was the caller's `async with` exit: `kontra.fleet`'s scope started a
 * `stackWorkflow destroy` as a child of the **Run** that provisioned it. That works, and `nscheck`'s
 * header says why — *"a script that provisions ten machines and then dies leaves ten machines; this
 * cannot, because the teardown is a replayable step in a program Temporal finishes whether or not
 * the process that started it still exists"*. It has exactly two gaps, and 0037 widens the first
 * one until it must be closed:
 *
 *   1. IT ASSUMES ONE OWNER. Scope exit destroys unconditionally, which is correct only while
 *      exactly one **Run** owns a **Fleet**. 0037 makes a **Fleet** capacity that several **Runs**
 *      may hold at once, at which point the first scope to exit takes everybody's **Machines**.
 *   2. IT NEVER COVERED THE CONTROL PLANE DYING. A **Run** whose workflow is never resumed never
 *      exits its scope. Temporal will finish a program it can still run; a **Fleet** whose holder
 *      is a workflow nobody will ever poll again has nothing left that would tear it down.
 *
 * So teardown moves OUT of the holder and into this **Lease** workflow, and gets a clock.
 *
 * ═══ THE THREE WAYS A LEASE FAILS TO DROP, AND WHAT ANSWERS EACH ═══
 *
 *   • DROPPED TWICE. `Map.delete` of an absent key. The second drop is a no-op and the teardown is
 *     driven by the SIZE of the **Lease** workflow rather than by a count of drops, so it cannot double-fire.
 *   • NEVER DROPPED because the holder died mid-run. The expiry check below: at a **Lease**'s
 *     deadline the **Lease** workflow asks Temporal whether the holder is still RUNNING, and only renews the
 *     ones that are.
 *   • NEVER DROPPED because the control plane restarted holding nothing. Same mechanism, and this
 *     is the case that needs the durable timer rather than a process: the deadline is a Temporal
 *     timer in this workflow's history, so it survives every process that has ever touched it. The
 *     honest floor is stated in the file's last comment.
 *
 * ═══ COST ═══
 *
 * BLOCKED, LIKE THE WARDEN, and for the same reason (`cli/warden/warden_workflow.go`): a workflow waiting
 * on a condition writes nothing while it waits. There is exactly one timer in flight — the earliest
 * expiry — and it is re-armed only when the **Lease** workflow changes. MEASURED by `lease.test.ts`, which
 * prints every number it asserts:
 *
 *   0 events in 4,000 ms — 0/hour — while a **Fleet** is held and nothing happens to it
 *   6 events per hold, and 6 per drop: WorkflowExecutionSignaled, the workflow task's three,
 *     TimerCanceled, TimerStarted. The pair at the end is a blocked workflow re-arming its one timer
 *   11.5-12.7 events per EXPIRY CHECK — measured three times as 37/3, 38/3 and 46/4 over the same
 *     four-second window; the six above plus the liveness activity's Scheduled/Started/Completed
 *     and its own workflow task. A RANGE because the number of checks that land in a fixed window
 *     is not fixed, which is also why the assertion is on the zero and not on this
 *
 * THE ZERO HAS A CONTROL BESIDE IT, in the same test and over the same wall-clock window: a second
 * **Lease** workflow with a 200 ms TTL, on the same worker and the same server, grew 37-46 events while the
 * quiet one grew none, and the number of expiry checks that produced them is asserted non-zero.
 * "It did not grow" is therefore a claim about this design and not about a counter that never moved.
 *
 * AT THE DEFAULT ONE-HOUR TTL a **Fleet** held for a week costs ~2,100 events, so Temporal's 51,200
 * history limit is roughly five months of continuous holding — which is why there is no
 * continue-as-new here and why a ticking design would have needed one.
 *
 * And in the HOLDER's history, which is the number that matters to a caller: a **Lease** costs one
 * activity at `hold` and one at `drop`, and NOTHING in between at any duration — because the holder
 * never renews. That is strictly cheaper than the child `stackWorkflow` it replaces on the exit
 * path, which was a whole workflow's worth of events in the caller's own history.
 */

import * as wf from '@temporalio/workflow';
import { ApplicationFailure } from '@temporalio/common';

import {
  LEASE_DROP_SIGNAL,
  LEASE_HOLD_SIGNAL,
  LEASE_QUERY,
  LEASE_TTL_MS,
  LEASE_UNKNOWN_LIMIT,
  type FleetLeaseSet,
  type LeaseView,
} from '../lease';
import type { HoldersAliveInput, HoldersAliveOutput } from '../activities/lease';
import type { StackWorkflowInput } from './stack';

/** What a hold says. Every field but `lease` may be absent; see each one. */
export interface HoldSignal {
  /** The **Lease** id — `<holder>#<nonce>`, built by the caller. The only required field. */
  lease: string;
  /**
   * The **Run** holding it, as a Temporal workflow id. Empty means UNATTRIBUTED: nothing can be
   * asked about it, so its deadline is final. `destroy_on_exit=False` takes one of these.
   */
  holder?: string;
  /** How long this claim survives without renewal. Absent takes the control plane's default. */
  ttlMs?: number;
  /**
   * The NAME of the cloud credential the eventual teardown makes its provider calls with. NEVER a
   * value — see `workflows/stack.ts`. Carried on the HOLD rather than on the workflow's input
   * because the **Lease** workflow outlives the **Run** that created it: the last holder out is very often not
   * the one that provisioned, and a teardown against the wrong credential leaves **Machines**
   * billing and reaching hostile infrastructure. The most recent non-empty one wins.
   */
  credential?: string;
}

/** What a drop says. Idempotent by construction: dropping what is not held is a no-op. */
export interface DropSignal {
  lease: string;
}

export interface FleetLeaseInput {
  /** The stack these **Leases** are on. This workflow's own id is `kontra-lease/` + this. */
  stackFqn: string;
  /**
   * Where the liveness check runs. INPUT, NEVER A CONSTANT READ IN HERE — `workflows/retention.ts`
   * records what the alternative cost (a sweep that could not be routed reached the live
   * materializer and deleted 223,378 rows), and the mechanical reason: workflow code runs in a `vm`
   * context created empty, so `process` is not merely non-deterministic in here, it is undefined.
   *
   * Empty disables the check, which means every expiry is final. That is the correct degradation
   * rather than a default: a **Lease** workflow that cannot ask whether a holder is alive must not silently
   * pick either answer, and of the two, "the **Fleet** dies at its deadline" is the one whose
   * failure a **Run** can retry.
   */
  livenessQueue?: string;
  /**
   * The **Leases** held at the moment of a continue-as-new handover (ADR 0050 is not this; see
   * kontra#10). Absent on a first start, which is every start a caller makes.
   *
   * WHY THIS WORKFLOW CONTINUES AT ALL. The header below measures 0 events/hour while a **Fleet**
   * is held and nothing happens to it, and then says a one-hour TTL costs ~2,100 events a week —
   * "roughly five months of continuous holding, which is why there is no continue-as-new here".
   * That reasoning is right about the RATE and silent about the TOTAL. Temporal TERMINATES an
   * execution at 51,200 events, so a **Fleet** held across a quarter walks into a hard stop, and
   * what dies is the thing that knows the **Fleet** must be destroyed. The backoff made the bleed
   * slow; it did not make it stop.
   *
   * CARRIED AS INPUT AND NOT REBUILT. A handover that re-derived the ledger would be a second
   * source of truth for who holds what, and the one moment it could disagree is the moment a
   * **Fleet** is torn down under a live holder.
   */
  carried?: CarriedLeases;
}

/** What crosses a continue-as-new boundary. Counters and the ledger — never a result list. */
export interface CarriedLeases {
  /** `lease id -> entry`, as pairs because a Map is not JSON. */
  leases: [string, Entry][];
  /** The credential NAME the eventual teardown uses. Never a value. */
  credential: string;
  /**
   * Whether this chain has ever actually held something.
   *
   * IT MUST CROSS THE BOUNDARY OR THE TEARDOWN IS LOST. `everHeld` gates the destroy, and a
   * continued execution starts with an empty one — so a **Fleet** that had been held for months
   * would hand over, drop its last **Lease**, and then decline to destroy the Machines because the
   * new leg had never seen a hold. That is a running bill nobody is watching.
   */
  everHeld: boolean;
}

export const holdLease = wf.defineSignal<[HoldSignal]>(LEASE_HOLD_SIGNAL);
export const dropLease = wf.defineSignal<[DropSignal]>(LEASE_DROP_SIGNAL);
export const getLeases = wf.defineQuery<FleetLeaseSet>(LEASE_QUERY);

/**
 * How many times the **Lease** workflow will try to tear a **Fleet** down before it gives up and FAILS.
 *
 * It gives up loudly rather than quietly: a failed workflow is red in the Workflows page and names
 * the `kontra fleet down` that fixes it, whereas a **Lease** workflow that retried for ever would sit RUNNING
 * and look like a **Fleet** somebody is still using.
 */
const DESTROY_ATTEMPTS = 5;

/** Backoff between teardown attempts. The thing being waited out is another writer on the stack —
 *  a `kontra fleet up` in flight, or this **Fleet**'s own compensating destroy. */
const DESTROY_BACKOFF_MS = 30_000;

/**
 * The workflow clock, and it is `Date.now()` on purpose.
 *
 * `@temporalio/worker` REPLACES the global `Date` inside the sandbox with one that reads the
 * activator's notion of now (`global-overrides.js`), so this is the deterministic clock: a replay
 * gets the same milliseconds it got the first time, and a time-skipping test environment moves it
 * without moving the wall. Named here rather than spelled inline five times so that nobody
 * "corrects" one of the five to a wall-clock read and gets a **Lease** workflow whose replays disagree about
 * which **Leases** had expired.
 */
function now(): number {
  return Date.now();
}

interface Entry {
  holder: string;
  ttlMs: number;
  expiresAt: number;
  /** Consecutive expiries at which the holder's liveness could not be established. */
  unknownChecks: number;
}

/**
 * The **Lease** workflow.
 *
 * IT IS STARTED BY ITS FIRST HOLD, and never any other way. `holdFleetLease` uses signal-with-start,
 * so the opening `hold` is already in history before the first workflow task runs and the loop below
 * never sees an empty **Lease** workflow it should not act on. A `drop` cannot start one: dropping a **Lease**
 * on a **Fleet** with no **Lease** workflow is a **Fleet** with nothing held, which is exactly what the caller
 * wanted to be true.
 */
export async function fleetLeaseWorkflow(input: FleetLeaseInput): Promise<FleetLeaseSet> {
  /**
   * PROXIED ONCE, PER EXECUTION, from a value that came in over the wire — the shape
   * `workflows/retention.ts` documents. It is deterministic in the way the sandbox demands: the
   * queue is in history, so a replay builds the identical proxy, which a `process.env` read in here
   * could not do because there is no `process` in here at all.
   *
   * NOT ON THE INFRA QUEUE, which is where this workflow itself runs. That queue serves one activity
   * at a time on purpose (a Pulumi engine forks a CLI child plus one per provider on a memory-capped
   * container), so a liveness check posted there would sit behind a sixty-minute converge — and the
   * **Lease** it was meant to renew would lapse while the answer queued. `activities/fleet.ts`
   * records the same trap from the caller's side.
   */
  const { runningHolders } = wf.proxyActivities<{
    runningHolders(i: HoldersAliveInput): Promise<HoldersAliveOutput>;
  }>({
    taskQueue: input.livenessQueue || undefined,
    // A describe per holder against the same Temporal this workflow is running on: seconds.
    startToCloseTimeout: '1 minute',
    retry: { maximumAttempts: 3 },
  });

  // SEEDED FROM THE HANDOVER, empty on a first start. See `FleetLeaseInput.carried`.
  const leases = new Map<string, Entry>(input.carried?.leases ?? []);
  let credential = input.carried?.credential ?? '';
  let destroying = false;
  let destroyed = false;

  /**
   * Whether this **Lease** workflow has ever actually HELD something.
   *
   * THE TEARDOWN IS GATED ON THIS AND NOT MERELY ON BEING EMPTY. A **Lease** workflow is created by a hold, so
   * "zero **Leases**" normally means "the last one dropped" — but a hold this workflow could not
   * use, a malformed signal or an empty id, reaches the same branch having claimed nothing, and
   * destroying a **Fleet** nobody ever claimed is the most expensive possible response to a typo.
   * Signal-with-start is the only way in, so the input that gets here is not always one this repo
   * wrote.
   */
  let everHeld = input.carried?.everHeld ?? false;

  /** Bumped by every signal; the loop waits for it to move. A counter rather than a boolean so two
   *  signals arriving inside one workflow task cannot cancel each other out. */
  let generation = 0;

  const view = (): FleetLeaseSet => ({
    fleet: input.stackFqn,
    leases: [...leases.entries()]
      .map(([lease, e]): LeaseView => ({ lease, holder: e.holder, expiresAt: e.expiresAt }))
      // Sorted, because a query answer that reorders between two reads reads as churn. By id, which
      // is the only field guaranteed unique.
      .sort((a, b) => (a.lease < b.lease ? -1 : a.lease > b.lease ? 1 : 0)),
    destroyed,
  });

  wf.setHandler(holdLease, (h: HoldSignal) => {
    generation += 1;
    if (!h?.lease) return;
    /**
     * A HOLD ARRIVING AFTER THE LEDGER HAS COMMITTED TO TEARDOWN IS REFUSED BY SILENCE, and this
     * is the one race in the design.
     *
     * Run A drops its last **Lease**; the **Lease** workflow begins destroying; Run B holds a microsecond
     * later. Accepting B's hold here would leave B converged onto **Machines** this workflow is in
     * the middle of deleting. So the hold is DROPPED ON THE FLOOR, which is not a silent failure —
     * `holdFleetLease` confirms its own hold by querying this **Lease** workflow, finds it absent, and retries;
     * once this workflow closes, the retry's signal-with-start opens a NEW **Lease** workflow on a **Fleet**
     * B's own converge will bring back. The protocol is "a hold is not held until the **Lease** workflow says
     * it is", and it is the only reason that activity does a read after a write.
     */
    if (destroying) return;
    const ttlMs = typeof h.ttlMs === 'number' && h.ttlMs > 0 ? h.ttlMs : LEASE_TTL_MS;
    if (h.credential) credential = h.credential;
    everHeld = true;
    // Holding an id that is already held REFRESHES it. That makes `hold` the renew verb as well,
    // which is what a long handoff needs and what keeps the signal count at two.
    leases.set(h.lease, {
      holder: h.holder ?? '',
      ttlMs,
      expiresAt: now() + ttlMs,
      unknownChecks: 0,
    });
  });

  wf.setHandler(dropLease, (d: DropSignal) => {
    generation += 1;
    if (d?.lease) leases.delete(d.lease);
  });

  wf.setHandler(getLeases, view);

  let seen = generation;
  for (;;) {
    if (leases.size === 0) {
      // LAST ONE OUT. Committed here and not inside the teardown, so that a hold racing it is
      // refused by the handler above rather than accepted into a **Lease** workflow that is already leaving.
      destroying = true;
      break;
    }
    const earliest = Math.min(...[...leases.values()].map((e) => e.expiresAt));
    // At least 1 ms: `condition(fn, 0)` is a timeout the SDK is entitled to read as "no timeout",
    // and a deadline already in the past must still wake this loop.
    const wait = Math.max(earliest - now(), 1);
    const signalled = await wf.condition(() => generation !== seen, wait);
    seen = generation;
    if (!signalled) await reap();

    /*
     * HAND OVER WHEN THE SERVER SAYS SO (kontra#10).
     *
     * `continueAsNewSuggested` is set by the server on EVERY workflow task once history passes
     * 4,096 events, and until now nothing in this repository read it. It is free — it is already on
     * the info object — and it is the only warning this workflow gets before Temporal terminates it
     * at 51,200. A held **Fleet** trips the suggestion at ~13.7 days and the ceiling at ~5.6 months.
     *
     * ONLY ON THE QUIET PATH — `!signalled`. A continue-as-new abandons the current execution, and a
     * signal that arrived but has not been folded into `leases` yet would be lost with it: a hold
     * dropped on the floor is a **Fleet** destroyed under a live holder, and a drop dropped on the
     * floor is a **Fleet** that outlives its last holder and bills. Waking on the timer means the
     * handler ran to completion and the ledger is settled, so the boundary is safe exactly here.
     *
     * AND NOT WHILE LEAVING. `destroying` is committed before the loop breaks, so a handover cannot
     * race the teardown; this point is only reached with at least one **Lease** still held.
     */
    if (wf.workflowInfo().continueAsNewSuggested) {
      await wf.continueAsNew<typeof fleetLeaseWorkflow>({
        ...input,
        carried: { leases: [...leases.entries()], credential, everHeld },
      });
    }
  }

  if (everHeld) {
    await destroy();
    destroyed = true;
  }
  return view();

  /**
   * An expiry fired. Decide, for each lapsed **Lease**, whether its holder is still there.
   *
   * THE CHECK IS WHAT MAKES THE TTL SAFE. Without it the clock would be a hard limit on how long a
   * **Run** may use a **Fleet**, and enforcing it would destroy **Machines** under live work — a
   * new failure introduced by the mechanism meant to prevent one. With it, the deadline means "how
   * long a **Fleet** survives a holder nobody can account for", which is what ADR 0037 asks for.
   */
  async function reap(): Promise<void> {
    // SNAPSHOT BEFORE THE AWAIT. Signals are delivered while the activity runs, so an entry may be
    // dropped, re-held or refreshed underneath this; the deadline captured here is what says
    // whether the entry decided on below is still the entry that lapsed.
    const lapsed = [...leases.entries()]
      .filter(([, e]) => e.expiresAt <= now())
      .map(([lease, e]) => ({ lease, holder: e.holder, expiresAt: e.expiresAt }));
    if (lapsed.length === 0) return;

    const holders = [...new Set(lapsed.map((l) => l.holder).filter((h) => h !== ''))];
    let alive = new Set<string>();
    let unknown = new Set<string>();
    if (holders.length > 0 && input.livenessQueue) {
      try {
        const out = await runningHolders({ holders });
        alive = new Set(out.alive ?? []);
        unknown = new Set(out.unknown ?? []);
      } catch {
        // THE CHECK FAILING IS NOT AN ANSWER. Every holder it was asked about becomes unknown,
        // which buys a bounded number of extensions and then stops. Rethrowing would kill the one
        // workflow that can still tear this **Fleet** down.
        unknown = new Set(holders);
      }
    }

    for (const l of lapsed) {
      const cur = leases.get(l.lease);
      // Dropped, or re-held with a later deadline, while the check ran. Either way this is no
      // longer the **Lease** that lapsed and nothing here applies to it.
      if (!cur || cur.expiresAt > l.expiresAt) continue;
      if (l.holder && alive.has(l.holder)) {
        cur.expiresAt = now() + cur.ttlMs;
        cur.unknownChecks = 0;
        continue;
      }
      if (l.holder && unknown.has(l.holder) && cur.unknownChecks + 1 < LEASE_UNKNOWN_LIMIT) {
        cur.unknownChecks += 1;
        cur.expiresAt = now() + cur.ttlMs;
        continue;
      }
      leases.delete(l.lease);
    }
  }

  /**
   * Tear the **Fleet** down, through the same `stackWorkflow` every other teardown goes through.
   *
   * THE SAME WORKFLOW AND THE SAME ID, not the `stackDestroy` activity underneath it, because the
   * id IS the stack (ADR 0019) and that is what makes a second writer structurally impossible. It
   * also means this teardown carries the credential preflight, appears in the Workflows page beside
   * the `up` that created the **Fleet**, and refuses with a sentence on an appliance rather than
   * looping against a provisioner that does not exist.
   *
   * ABANDON, not the default TERMINATE: if this **Lease** workflow is terminated while the destroy is in
   * flight, TERMINATE would kill the teardown and leave the **Machines** billing — which is
   * `kontra.fleet`'s own reason for the same flag, and it is the failure this slice exists to
   * prevent, so it may not be reintroduced one indirection lower.
   */
  async function destroy(): Promise<void> {
    let last: unknown;
    for (let attempt = 1; attempt <= DESTROY_ATTEMPTS; attempt += 1) {
      try {
        await wf.executeChild<(i: StackWorkflowInput) => Promise<unknown>>('stackWorkflow', {
          workflowId: input.stackFqn,
          args: [
            {
              stackFqn: input.stackFqn,
              op: 'destroy',
              // `destroy` carries no desired state — sending args is the one way to destroy the
              // wrong thing. The credential NAME is not desired state; it is which of the
              // operator's credentials to make the calls with.
              args: credential ? { credential } : {},
            },
          ],
          parentClosePolicy: wf.ParentClosePolicy.ABANDON,
        });
        return;
      } catch (err) {
        last = err;
        // An appliance's `stackWorkflow` refuses non-retryably and will refuse identically in
        // thirty seconds. Waiting five times to say so turns one clear sentence into two and a
        // half minutes of a **Fleet** looking like it might yet be collected.
        if (isPermanent(err)) break;
        if (attempt < DESTROY_ATTEMPTS) await wf.sleep(DESTROY_BACKOFF_MS);
      }
    }
    throw ApplicationFailure.nonRetryable(
      `the last Lease on ${input.stackFqn} dropped and its Machines could not be destroyed ` +
        `after ${DESTROY_ATTEMPTS} attempts — they are still running and still billing. ` +
        `Tear them down by hand: kontra fleet down (the stack is ${input.stackFqn}). ` +
        `Last error: ${describe(last)}`,
      'FleetTeardownFailed'
    );
  }
}

/** A failure that will not become a success on the next attempt — an appliance's refusal, a
 *  credential nobody can read. Matched by the SDK's own flag rather than by message text. */
function isPermanent(err: unknown): boolean {
  for (let cur: unknown = err, depth = 0; cur && depth < 8; depth += 1) {
    if (cur instanceof ApplicationFailure && cur.nonRetryable) return true;
    cur = (cur as { cause?: unknown }).cause;
  }
  return false;
}

/** The innermost thing that actually went wrong, flattened for one error string. */
function describe(err: unknown): string {
  const parts: string[] = [];
  for (let cur: unknown = err, depth = 0; cur && depth < 8; depth += 1) {
    const msg = String((cur as Error).message ?? cur);
    if (msg && !parts.includes(msg)) parts.push(msg);
    cur = (cur as { cause?: unknown }).cause;
  }
  return parts.join(': ') || 'unknown';
}

/**
 * ═══ WHAT THIS DOES NOT SURVIVE, STATED PLAINLY ═══
 *
 * A **Fleet** whose control plane vanishes is destroyed WHEN A WORKER NEXT POLLS THE INFRA QUEUE,
 * not at the instant the deadline passes. The timer is durable — it is in this workflow's history
 * on the Temporal server — so no process restart, redeploy or crash loses it, and that is the
 * property ADR 0037 asks for and the one scope-exit teardown never had. What it is NOT is
 * independent of Temporal: if the Temporal cluster itself is gone for ever, nothing here fires, and
 * no design that keeps the destroy authority on the Controller can do better (`infra/CONTEXT.md`:
 * *"Cloud authority — creating and destroying **Machines** — belongs to the **Controller** alone"*).
 * The **Warden** cannot close this gap either: 0037 says it may restart a **Worker** and may not
 * replace a **Machine**, so a **Machine** cannot delete itself on a lapsed **Lease**. Stopping its
 * **Workers** when its claim expires would bound the blast radius without bounding the bill; it is
 * not built, and it is the obvious next thing if this floor is ever judged too low.
 */
