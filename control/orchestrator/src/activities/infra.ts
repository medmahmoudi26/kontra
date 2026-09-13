/**
 * Pulumi operations as Temporal activities (ADR 0019).
 *
 * The Automation API is a normal library call that does blocking I/O, so it belongs in an
 * activity and never in workflow code. Running it here is what makes provisioning durable,
 * retriable, cancellable and observable — all of which are Temporal's properties, not
 * Pulumi's.
 *
 * Two behaviours below were measured against a real backend, not assumed:
 *
 *   - **A killed operation leaves a lock with no TTL.** `kill -9` mid-`destroy` leaves
 *     `.pulumi/locks/<project>/<stack>/<uuid>.json`, and every later op fails with
 *     `the stack is currently locked by 1 lock(s)` — permanently. `pulumi cancel` clears it
 *     and the following `up` finds state intact.
 *   - **Therefore `cancel()` runs only on retry, never on a timer.** It is safe here for one
 *     reason: the caller is an entity workflow whose id IS the stack fqn, so Temporal's
 *     workflow-id uniqueness makes a second concurrent writer structurally impossible. On a
 *     timer, or from a bare activity call, the same line would happily cancel a healthy
 *     concurrent update.
 *
 * THIS FILE IS THE LAST HOP FOR THE CLOUD CREDENTIAL (ADR 0034 §4). The workflow above carries a
 * NAME; {@link workspaceFor} turns it into a value here, in the process about to make the call it
 * is for, and puts it in one Pulumi workspace's `envVars`. Nothing that leaves an activity in this
 * file carries it — not a result, not a heartbeat, not an error message — because an activity's
 * result and its arguments are written into workflow history identically, and the payload codec is
 * a claim-check rather than encryption: a seventy-byte token is two thousand times under its
 * threshold and would ride inline, in the clear, for the namespace's whole retention.
 */

import { Context } from '@temporalio/activity';
import { ApplicationFailure } from '@temporalio/common';
import { fqn, parseFqn, selectStack, type StackRef } from '../infra/workspace';
import { planFor, type StackInput } from '../infra/stacks';
import {
  CloudCredentialUnavailable,
  checkCredential,
  credentialLabel,
  resolveProviderEnv,
} from '../infra/credential';

export interface StackOpInput extends StackInput {
  /** `project/stack`. Also the workflow id — see the header. */
  stackFqn: string;
  /**
   * The Run this converge serves — the CALLER's workflow id, taken from the parent by
   * `stackWorkflow`, or empty for a `kontra fleet` command.
   *
   * It exists for the resolution **Lease** workflow (`secrets/audit.ts`), so "which of my credentials was read
   * and for what" has an answer that names a run rather than a stack. A workflow id is already in
   * history in the clear on both sides; it is not a secret and it is not authority.
   */
  run?: string;
}

/**
 * Everything the last hop needs, in one place, from one parse.
 *
 * WHAT THIS FUNCTION IS. It is the boundary ADR 0034 §4 draws: above it a credential is a NAME
 * carried in an activity argument, below it a value that lives in one Pulumi workspace's `envVars`
 * for the length of one converge. Nothing between the two exists — there is no variable in this
 * module holding a token, and no return value that carries one.
 */
async function workspaceFor(input: StackOpInput) {
  const plan = planFor(input);
  const args = input.args ?? {};
  const providerEnv = await resolveProviderEnv({
    credential: plan.credential,
    providerEnvVar: plan.providerEnvVar,
    // The Actor this fleet places, for the **Lease** workflow. A Machines-only converge places none, and the
    // stack's own name is the honest answer there — it is `<actor>-<version>` by construction.
    actor: typeof args.actorName === 'string' && args.actorName ? args.actorName : ref(input).stack,
    version: typeof args.actorVersion === 'string' ? args.actorVersion : '',
    run: typeof input.run === 'string' ? input.run : '',
  }).catch(rethrowCredential);
  return selectStack(ref(input), plan.program, providerEnv);
}

/**
 * A credential that cannot be resolved is not a transient failure, so it is not retried.
 *
 * Three attempts at a secret that does not exist is three identical messages and a slower answer,
 * and — worse on the `destroy` path — three minutes before an operator learns why their Machines
 * are still billing.
 */
function rethrowCredential(err: unknown): never {
  if (err instanceof CloudCredentialUnavailable) {
    throw ApplicationFailure.nonRetryable(err.message, 'CloudCredentialUnavailable');
  }
  throw err;
}

/**
 * START-TIME REFUSAL, called by `stackWorkflow` BEFORE the saga leg is armed.
 *
 * IT READS METADATA AND NEVER A VALUE (`infra/credential.ts:checkCredential`), which is what makes
 * it safe to be an activity at all: an activity's RESULT is written into history exactly as its
 * arguments are, so a resolver activity would put the token there with a bow on it. What comes
 * back is a name and a version number, both of which are already in history.
 *
 * It is a separate activity rather than the first line of `stackUp` so that a missing credential
 * fails with NOTHING BUILT and NOTHING TO COMPENSATE — a `destroy` triggered by a failure to read
 * the credential would fail on the same credential and bury the real message under a cleanup error.
 */
export async function checkCloudCredential(
  input: StackOpInput
): Promise<{ credential: string; version: number }> {
  const plan = planFor(input);
  try {
    return await checkCredential(plan.credential);
  } catch (err) {
    if (err instanceof CloudCredentialUnavailable) {
      throw ApplicationFailure.nonRetryable(
        `${input.stackFqn}: ${err.message}`,
        'CloudCredentialUnavailable',
        { credential: credentialLabel(plan.credential) }
      );
    }
    throw err;
  }
}

export interface StackOpResult {
  fqn: string;
  /** Pulumi's own summary: 'succeeded' | 'failed' | 'in-progress'. */
  result: string;
  /** Resource counts by operation — `{create: 10}` etc. Empty when nothing changed. */
  changes: Record<string, number>;
  /** Stack outputs, with secrets already stripped by the caller's plainValue access. */
  outputs: Record<string, unknown>;
}

function ref(input: StackOpInput): StackRef {
  return parseFqn(input.stackFqn);
}

/**
 * Clear a lock left by a previously killed attempt.
 *
 * `pulumi cancel` is documented as dangerous — it can leave a stack inconsistent if a resource
 * operation was genuinely in flight. It is correct here ONLY because the entity workflow proves
 * we are the sole writer for this stack, and only on a retry, where the prior attempt is known
 * to be dead.
 */
async function clearStaleLock(input: StackOpInput): Promise<void> {
  const stack = await workspaceFor(input);
  await stack.cancel().catch(() => {
    /* no lock to clear is the normal case */
  });
}

/** `pulumi up`. Heartbeats per resource so a long provision is observable and a wedged one
 * trips HeartbeatTimeout rather than sitting until StartToClose. */
/** How often the converge re-asserts liveness while one long resource op is in flight.
 * Comfortably inside the 2-minute heartbeatTimeout in workflows/stack.ts; the two numbers are a
 * pair and moving one without the other is what re-opens the bug above. */
const KEEPALIVE_MS = 30_000;

export async function stackUp(input: StackOpInput): Promise<StackOpResult> {
  const ctx = Context.current();
  if (ctx.info.attempt > 1) await clearStaleLock(input);

  const stack = await workspaceFor(input);

  // THE HEARTBEAT IS A LIVENESS CHECK, SO IT CANNOT BE EVENT-DRIVEN ALONE.
  //
  // Pulumi emits a resourcePreEvent when a resource op STARTS and nothing again until it ends. A
  // `command:remote:Command` — which is how every Machine gets its Worker installed — legitimately
  // runs for many minutes: the install script waits on `cloud-init status --wait` before it may
  // touch apt at all, and a fresh DO image is still running unattended-upgrades for the first
  // several. Against a 2-minute heartbeatTimeout that is one pre-event and then silence, so
  // Temporal kills the activity mid-install and retries — restarting the very install that was
  // making progress, into the same wall, until maximumAttempts is spent.
  //
  // Measured on this checkout, fleet `desync`, 4 Machines in sfo3: attempt 1 timed out at
  // `kf-desync-02-actor-desync`, attempt 2 timed out on the SAME resource, and the converge only
  // completed once cloud-init was allowed to finish out of band.
  //
  // So the timer reports what the timeout actually asks about — is the engine still there — while
  // `last` keeps the DETAIL event-driven, so `/api/infra`'s progress line still names the resource
  // being worked on rather than degrading to a tick.
  let last: Record<string, unknown> = { phase: 'up' };
  const keepalive = setInterval(() => ctx.heartbeat(last), KEEPALIVE_MS);

  let res;
  try {
    res = await stack.up({
      color: 'never',
      // Cancellation is a straight wire: Temporal's AbortSignal becomes SIGINT to the engine,
      // which unwinds, checkpoints, and releases the lock.
      signal: ctx.cancellationSignal,
      onEvent: (e) => {
        const m = e.resourcePreEvent?.metadata;
        if (m) last = { op: m.op, urn: m.urn };
        else if (e.summaryEvent) last = { changes: e.summaryEvent.resourceChanges };
        else return;
        ctx.heartbeat(last);
      },
    });
  } finally {
    // In a finally because a converge that THREW must not leave a timer heartbeating for an
    // activity that has already failed — the next attempt would inherit a liveness signal from a
    // dead run.
    clearInterval(keepalive);
  }

  const outputs: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(res.outputs)) outputs[k] = v.secret ? '[secret]' : v.value;

  return {
    fqn: fqn(ref(input)),
    result: res.summary.result,
    changes: res.summary.resourceChanges ?? {},
    outputs,
  };
}

/** `pulumi preview`. Read-only, so it neither takes a lock nor needs the retry dance. */
export async function stackPreview(input: StackOpInput): Promise<StackOpResult> {
  const ctx = Context.current();
  const stack = await workspaceFor(input);
  const res = await stack.preview({
    color: 'never',
    signal: ctx.cancellationSignal,
    onEvent: () => ctx.heartbeat({ phase: 'preview' }),
  });
  return {
    fqn: fqn(ref(input)),
    result: 'preview',
    changes: res.changeSummary as unknown as Record<string, number>,
    outputs: {},
  };
}

/** `pulumi destroy` — the compensating half of a saga. The workflow runs it on cancellation
 * through a disconnected context, so a cancelled fleet-up still tears down what it built
 * rather than leaving half a fleet billing. */
export async function stackDestroy(input: StackOpInput): Promise<StackOpResult> {
  const ctx = Context.current();
  if (ctx.info.attempt > 1) await clearStaleLock(input);

  const stack = await workspaceFor(input);
  const res = await stack.destroy({
    color: 'never',
    onEvent: (e) => {
      const m = e.resourcePreEvent?.metadata;
      if (m) ctx.heartbeat({ op: m.op, urn: m.urn });
    },
  });
  return {
    fqn: fqn(ref(input)),
    result: res.summary.result,
    changes: res.summary.resourceChanges ?? {},
    outputs: {},
  };
}

/**
 * convergeFleetSessions — make every **Machine** in a **Fleet** have a tmux session to look at.
 *
 * WHY THIS EXISTS, AND WHY IT IS NOT AN ARGUMENT ON `fleet.up()`. ADR 0020 made session existence a
 * Temporal converge rather than a flag on the provision, and `programs/fleet.ts` says so at the one
 * place somebody would reach for it: *"There is deliberately no `tmux` arg"*. The consequence was
 * never written down, and it is this: a **Fleet** raised by a WORKFLOW could not create a session at
 * all. `kontra fleet up --tmux` could; `fleet.up()` structurally could not, so every workflow-raised
 * **Machine** rendered "no session — Converge session" on the Monitor forever, and the operator's
 * only path was a button. That reads as an error and is in fact the designed state, which is worse
 * than either.
 *
 * So the converge stays a converge — this starts the SAME `tmuxSessionWorkflow` the Monitor's button
 * starts, through the same `temporalConverger` — and the **Fleet** scope simply asks for it once its
 * **Machines** exist.
 *
 * THE SESSION NAME IS NOT DERIVED HERE, and that is the whole reason this is an activity rather than
 * three lines in the SDK. `shared/conformance/queues.json` pins the tmux session name across four languages
 * and warns in its own words that a drift draws "a Machine whose Worker is running perfectly as one
 * with NO SESSION". Python has no arm of that corpus, so deriving it there would have been a FIFTH
 * implementation of a rule that already has four. `machinesFromStack` is the existing one.
 *
 * IT NEVER FAILS THE RUN. A pane is an observability affordance, not the work: a **Fleet** whose
 * sessions did not converge still has **Workers** polling and still produces every row. So this
 * returns what it managed and names what it did not, and the caller logs it.
 */
export interface ConvergeSessionsInput {
  stackFqn: string;
}

export interface ConvergeSessionsResult {
  /** Machines a session converge was started for. */
  converged: string[];
  /** Machines that have no session and why — never thrown, always reported. */
  refused: { machine: string; why: string }[];
}

export async function convergeFleetSessions(
  input: ConvergeSessionsInput
): Promise<ConvergeSessionsResult> {
  const { readStack } = await import('../infra/state');
  const { machinesFromStack } = await import('../panels/discovery');
  const { temporalConverger } = await import('../panels/converger');

  const out: ConvergeSessionsResult = { converged: [], refused: [] };
  const state = await readStack(input.stackFqn);
  if (!state) {
    out.refused.push({ machine: '*', why: `no stack state for ${input.stackFqn}` });
    return out;
  }

  const machines = machinesFromStack(input.stackFqn, state.outputs);
  if (machines.length === 0) {
    // NOT an error. A Fleet with no placement has no Worker to watch, so it has no session to
    // converge and nothing is wrong — see `queues.json`'s `fleet` fallback.
    out.refused.push({ machine: '*', why: 'the stack names no Machines with a placement' });
    return out;
  }

  const converger = temporalConverger();
  for (const m of machines) {
    try {
      await converger.converge(m);
      out.converged.push(m.machine);
    } catch (err) {
      out.refused.push({ machine: m.machine, why: err instanceof Error ? err.message : String(err) });
    }
  }
  return out;
}
