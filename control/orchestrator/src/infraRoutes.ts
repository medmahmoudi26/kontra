/**
 * THE INFRA SURFACE (ADR 0019) — six routes, and the three functions behind the first three of them.
 *
 * The routes only ever START a workflow and read its status. They never touch Pulumi and they
 * never see a credential — the engine lives in `orchestrator-infra`, reached by task queue.
 *
 * The workflow id IS the stack fqn. That is what makes concurrent `up`s on one stack
 * impossible: a second POST does not start a second writer, it collides with the first.
 *
 * TOKEN-GATED FROM THE FIRST COMMIT, WITHOUT EXCEPTION. These routes reach a process holding
 * `DIGITALOCEAN_TOKEN`, on the one machine running the control plane; the rest of this API is
 * unauthenticated (auth.ts records that), so an ungated infra route would hand anyone who can reach
 * `:8088` the ability to spend money and stand up attack infrastructure. `checkBearer`, not the run
 * surface's opt-in `checkOptionalBearer` — with no token configured this surface 503s rather than
 * opening.
 *
 * WHAT IT NEEDS: nothing injected. `getClient` dials Temporal from inside a handler, and the
 * Pulumi checkpoint reads (`infra/state.ts`) parse files on this host's disk. The one route with no
 * gate is `GET /infra`, the dashboard PAGE, which contains no data — only the code that fetches it
 * from the four gated routes above.
 *
 * THIS FILE PREDATES `routes/` AND KEEPS ITS PLACE. It was already the extracted half of this
 * surface when the rest of `buildServer` was one closure; folding the registrar in here — rather
 * than adding a second `infra` file under `routes/` that imports these two functions — keeps the
 * whole surface in the one file its name promises.
 */

import type { FastifyInstance } from 'fastify';
import { WorkflowNotFoundError } from '@temporalio/client';
import { defaultPayloadConverter, type Payload } from '@temporalio/common';

import { dataConverter } from './codec/dataConverter';

import { checkBearer } from './auth';
import { INFRA_DASHBOARD_HTML } from './infra/dashboard';
import { LEASE_QUERY, leaseWorkflowId, type FleetLeaseSet } from './lease';
// From `infra/paths`, NOT `infra/workspace`: the latter imports the Pulumi Automation API at
// its top, and this is a string parse. See that file's header.
import { parseFqn } from './infra/paths';
import { readHistory } from './infra/history';
import { listStacks, readStack } from './infra/state';
import { errMessage } from './routes/errors';
import { NAMESPACE, getClient } from './temporalClient';
// The QUEUE, from `queues.ts` and not from `infra.ts`. This route holds no credential and runs no
// engine — it starts a workflow on an address — and importing the provisioner to read that address
// put Pulumi in the API's module graph for a string.
import { infraQueue } from './queues';
import type { StackOp } from './workflows/stack';
import { tenantAttributes } from './visibility';

/**
 * Infra routes accept the state token — the narrower of the two, not the explore token.
 *
 * Reading a dataset and provisioning cloud machines are not the same privilege, and the
 * explore token is handed out for the workbench. Anything that can spend money sits behind
 * the tighter one.
 */
export const INFRA_ROUTE_TOKEN_VARS = ['KONTRA_STATE_TOKEN'] as const;

export interface StackOpAccepted {
  fqn: string;
  op: StackOp;
  workflowId: string;
  runId: string;
}

/**
 * Start (or attach to) the stack's workflow.
 *
 * `workflowIdConflictPolicy: 'FAIL'` is deliberate: if an operation is already running for this
 * stack, the caller is told so rather than silently queueing a second writer behind it.
 */
export async function startStackOp(
  fqn: string,
  op: StackOp,
  body: Record<string, unknown>
): Promise<StackOpAccepted> {
  const client = await getClient();
  const handle = await client.workflow.start('stackWorkflow', {
    taskQueue: infraQueue(),
    workflowId: fqn,
    workflowIdConflictPolicy: 'FAIL',
    // Whose Fleet operation this is, stamped at start — see `tenantAttributes`. This one is the
    // reason the gap mattered: a stack up/down is what spends money, and it was the least
    // attributable execution on the cluster.
    typedSearchAttributes: tenantAttributes(NAMESPACE),
    args: [
      {
        stackFqn: fqn,
        op,
        args: (body.args ?? {}) as Record<string, unknown>,
        compensateOnCancel: body.compensateOnCancel !== false,
      },
    ],
  });
  return { fqn, op, workflowId: handle.workflowId, runId: handle.firstExecutionRunId };
}

export interface StackOpStatus {
  fqn: string;
  status: string;
  /**
   * The WORKFLOW's own progress — `{phase, op}`, and `{result, changes}` once it is done.
   *
   * THIS FIELD'S COMMENT USED TO SAY "last heartbeat from the engine — which resource it is on",
   * AND IT NEVER CARRIED ONE. `getProgress` is a query handler over a variable `stackWorkflow`
   * assigns four times (`starting`, `running`, `done`, `compensating`); the per-resource detail
   * `stackUp` emits — `last = {op, urn}` on every `resourcePreEvent` — is an ACTIVITY heartbeat,
   * which Temporal stores on the pending activity and never delivers to the workflow. So the one
   * fact the comment promised was in the other half of the response all along, undecoded. See
   * {@link cursor}.
   */
  progress?: Record<string, unknown>;
  /**
   * WHICH RESOURCE THE ENGINE IS ON, RIGHT NOW — the activity heartbeat, decoded.
   *
   * `{op, urn}`, e.g. `{op: 'create', urn: 'urn:pulumi:recon-s4::kontra-fleet::…::kf-recon-03'}`.
   * A converge is minutes of silence punctuated by these, and until now nothing outside the Pulumi
   * process could see one: the run page said `Bring the Fleet up · 214s` and could not say that
   * three Droplets existed and the fourth was being made.
   *
   * ABSENT WHENEVER IT WOULD BE A GUESS — no activity pending (between attempts, or terminal), no
   * details yet (the first thirty seconds, before the first `resourcePreEvent`), or a payload this
   * process could not decode. Never an empty object: "the engine is on nothing" and "we have not
   * been told" are different, and only one of them is ever true while a converge runs.
   */
  cursor?: Record<string, unknown>;
  result?: unknown;
}

/** Status + live progress for one stack's operation. */
export async function readStackOp(fqn: string): Promise<StackOpStatus> {
  const client = await getClient();
  const handle = client.workflow.getHandle(fqn);
  const desc = await handle.describe();
  const out: StackOpStatus = { fqn, status: String(desc.status.name) };

  // Progress is best-effort by design: the query is rejected on some terminal states, and a
  // status surface that fails because its *decoration* is unavailable is one operators stop
  // trusting.
  try {
    out.progress = await handle.query<Record<string, unknown>, []>('getProgress');
  } catch {
    /* terminal or not yet queryable */
  }
  // Same posture, and more so: this reads a proto bag off `describe`'s raw response, so anything
  // missing or shaped differently yields no cursor rather than a throw on a status route.
  const cursor = await heartbeatCursor(desc.raw);
  if (cursor) out.cursor = cursor;

  if (desc.status.name === 'COMPLETED') {
    try {
      out.result = await handle.result();
    } catch {
      /* surfaced via status */
    }
  }
  return out;
}

/**
 * The newest pending activity's heartbeat details, decoded.
 *
 * ── WHY THIS IS A PROTO WALK AND NOT AN SDK CALL ────────────────────────────────────────────────
 *
 * `@temporalio/client` gives no typed accessor for heartbeat details on a describe — they arrive as
 * `pendingActivities[].heartbeatDetails`, a `Payloads` message on the raw response. `runActivity.ts`
 * already walks the same bag for the same reason and its header says why the reading is structural
 * and defensive: the collections are optional, and `pendingNexusOperations` only exists on servers
 * new enough to have it. Anything absent here yields `undefined`.
 *
 * ── THE CODEC RUNS BEFORE THE CONVERTER ─────────────────────────────────────────────────────────
 *
 * The payload is claim-checked like every other payload this cluster writes. `{op, urn}` is a couple
 * of hundred bytes, so it rides inline and the codec is a passthrough for it — but calling `decode`
 * is what makes that a property of the DATA rather than an assumption about its size, and a heartbeat
 * that ever did get claim-checked would otherwise decode to a pointer this route printed as the
 * resource being built.
 *
 * ── ONE ACTIVITY, THE LAST ONE TO HEARTBEAT ────────────────────────────────────────────────────
 *
 * `orchestrator-infra` runs at `maxConcurrentActivityTaskExecutions: 1`, so a converging stack has
 * exactly one pending activity in practice. Picking the freshest `lastHeartbeatTime` rather than the
 * first entry keeps that an observation instead of a dependency: a retry that left a stale entry
 * behind must not be reported as the resource being worked on now.
 */
async function heartbeatCursor(raw: unknown): Promise<Record<string, unknown> | undefined> {
  const bag = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
  const pending = Array.isArray(bag.pendingActivities) ? bag.pendingActivities : [];

  let best: { at: number; payloads: unknown } | undefined;
  for (const item of pending) {
    const act = (item && typeof item === 'object' ? item : {}) as Record<string, unknown>;
    const payloads = (act.heartbeatDetails as { payloads?: unknown } | undefined)?.payloads;
    if (!Array.isArray(payloads) || payloads.length === 0) continue;
    const at = heartbeatMs(act.lastHeartbeatTime);
    if (best === undefined || at >= best.at) best = { at, payloads };
  }
  if (best === undefined) return undefined;

  try {
    const decoded = await decodePayloads(best.payloads as Payload[]);
    const value = decoded.length === 0 ? undefined : defaultPayloadConverter.fromPayload(decoded[0]!);
    // A heartbeat that is not an object is not a cursor. `stackUp` sends `{op, urn}` or `{changes}`,
    // and the second of those is a real frame with no resource in it — the caller decides what to do
    // with a cursor that names none, and inventing one here would be worse than saying nothing.
    return value !== null && typeof value === 'object' && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : undefined;
  } catch {
    return undefined;
  }
}

/** Proto Timestamp → epoch ms. `seconds` arrives as a number or a protobufjs Long, which is the
 *  same reading `runActivity.ts:tsToMs` does — written again rather than imported, because that
 *  module's copy is private to its own reduction and this one has a single caller. */
function heartbeatMs(ts: unknown): number {
  if (!ts || typeof ts !== 'object') return 0;
  const t = ts as { seconds?: unknown; nanos?: unknown };
  const secs = Number((t.seconds ?? 0).toString());
  const nanos = Number((t.nanos ?? 0).toString());
  const ms = (Number.isFinite(secs) ? secs : 0) * 1000 + Math.floor((Number.isFinite(nanos) ? nanos : 0) / 1e6);
  return ms > 0 ? ms : 0;
}

/** Run the cluster's payload codecs over raw payloads, in order. One codec today (the claim check);
 *  the loop is what keeps that a fact about the configuration rather than about this function. */
async function decodePayloads(payloads: Payload[]): Promise<Payload[]> {
  let out = payloads;
  for (const codec of dataConverter.payloadCodecs) out = await codec.decode(out);
  return out;
}

/**
 * Who is holding this **Fleet** (ADR 0037).
 *
 * A **Fleet** WITH NO LEDGER IS A FLEET WITH NOTHING HELD, and that is an answer rather than a 404.
 * The **Lease** workflow is created by the first hold and completes when the last **Lease** drops, so "no such
 * workflow" is the ordinary state of every **Fleet** an operator brought up by hand — and it is also
 * the exact question `kontra fleet down` asks before it destroys anything. A 404 there would have to
 * be read as "unknown", and an operator who cannot tell "nobody holds it" from "I could not find
 * out" either stops asking or forces every time.
 */
export async function readLeases(fqn: string): Promise<FleetLeaseSet> {
  const client = await getClient();
  try {
    return await client.workflow.getHandle(leaseWorkflowId(fqn)).query<FleetLeaseSet, []>(LEASE_QUERY);
  } catch (err) {
    if (isWorkflowNotFound(err)) return { fleet: fqn, leases: [] };
    throw err;
  }
}

/**
 * Is this "that workflow does not exist" rather than "Temporal is unwell"?
 *
 * The distinction is the difference between a 404 and a 502, and it is not cosmetic: a 502
 * says the upstream is broken and belongs in an alert, while asking for an infra op that was
 * never started is an ordinary client error. Matched by TYPE, not by message text, so a
 * reworded SDK error cannot silently turn 404s back into 502s.
 *
 * The RUN routes no longer need this: `runs.read` answers `undefined` for a run neither
 * Temporal nor the lake has heard of, which is a fact about the run rather than an error
 * shape to classify. This is the last caller, which is why it lives here rather than in
 * `server.ts`.
 */
function isWorkflowNotFound(err: unknown): boolean {
  return err instanceof WorkflowNotFoundError;
}

export function registerInfraRoutes(app: FastifyInstance): void {
  // --- infra (ADR 0019) -----------------------------------------------------------
  //
  // Token-gated from the FIRST commit, without exception. These routes reach a process
  // holding DIGITALOCEAN_TOKEN, on the one machine running the control plane; the rest of
  // this API is unauthenticated (auth.ts records that), so an ungated infra route would hand
  // anyone who can reach :8088 the ability to spend money and stand up attack infrastructure.
  //
  // The route only ever STARTS a workflow. A ten-machine `up` takes minutes — far longer than
  // any proxy will hold a connection — so this returns an operation id and the caller polls,
  // exactly as the run surface already does.
  app.post('/api/infra/stacks/:fqn/:op', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, INFRA_ROUTE_TOKEN_VARS);
    if (denied) {
      if (denied.code === 401) req.log?.warn?.({ path: req.url, ip: req.ip }, 'infra: rejected');
      return reply.code(denied.code).send(denied.body);
    }
    const { fqn, op } = req.params as { fqn: string; op: string };
    if (op !== 'up' && op !== 'preview' && op !== 'destroy') {
      return reply.code(400).send({ error: `unknown infra op ${op}` });
    }
    try {
      parseFqn(decodeURIComponent(fqn));
    } catch {
      return reply.code(400).send({ error: 'stack must be <project>/<stack>' });
    }
    try {
      return await startStackOp(decodeURIComponent(fqn), op, (req.body ?? {}) as Record<string, unknown>);
    } catch (err) {
      return reply.code(502).send({ error: `could not start infra op: ${errMessage(err)}` });
    }
  });

  app.get('/api/infra/ops/:fqn', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, INFRA_ROUTE_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { fqn } = req.params as { fqn: string };
    try {
      return await readStackOp(decodeURIComponent(fqn));
    } catch (err) {
      if (isWorkflowNotFound(err)) return reply.code(404).send({ error: 'no such infra op' });
      return reply.code(502).send({ error: `could not read infra op: ${errMessage(err)}` });
    }
  });

  // The Pulumi state surface: READ-ONLY, and a different thing from the two routes above.
  // Those START work; these describe what exists. Same token, because an inventory of live
  // machines and their addresses is exactly what an attacker would want first.
  app.get('/api/infra/stacks', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, INFRA_ROUTE_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    try {
      return { stacks: await listStacks() };
    } catch (err) {
      return reply.code(502).send({ error: `could not list stacks: ${errMessage(err)}` });
    }
  });

  app.get('/api/infra/stacks/:fqn/state', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, INFRA_ROUTE_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { fqn } = req.params as { fqn: string };
    let parsed: string;
    try {
      const ref = parseFqn(decodeURIComponent(fqn));
      parsed = `${ref.project}/${ref.stack}`;
    } catch {
      return reply.code(400).send({ error: 'stack must be <project>/<stack>' });
    }
    try {
      const state = await readStack(parsed);
      if (!state) return reply.code(404).send({ error: 'no such stack' });
      return state;
    } catch (err) {
      return reply.code(502).send({ error: `could not read stack: ${errMessage(err)}` });
    }
  });

  /**
   * WHAT THIS STACK HAS CONVERGED, one record per `up`, `preview` or `destroy` (issue 13).
   *
   * A SIBLING OF `/state`, NOT A FIELD ON IT, because the two answer different questions: a
   * checkpoint says what EXISTS right now and is rewritten in place, so it cannot say that a Fleet
   * was torn down and stood up again this afternoon — or that the teardown FAILED, which is a
   * Droplet still being billed. `infra/history.ts` says the rest.
   *
   * SAME TOKEN as its two siblings, for the reason stated above them: the answer is a record of what
   * this control plane has provisioned and when, which is an operational map.
   *
   * IT DOES NOT 404 FOR A STACK THAT NEVER CONVERGED. `/state` does, correctly — a document that is
   * not there. A history is a SET and the empty set is a real answer, and the console keys off that
   * difference: a 404 here can only mean the route is not served, and it draws no strip at all
   * rather than asserting that a stack has never converged.
   */
  app.get('/api/infra/stacks/:fqn/history', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, INFRA_ROUTE_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { fqn } = req.params as { fqn: string };
    const { limit } = (req.query ?? {}) as { limit?: string };
    let parsed: string;
    try {
      // `parseFqn` BEFORE ANY PATH IS BUILT. It is the same guard the two routes above use and here
      // it is doing more work: this handler joins the project and the stack into a DIRECTORY name,
      // so a `..` that reached it would list one. The parse rejects every character that could.
      const ref = parseFqn(decodeURIComponent(fqn));
      parsed = `${ref.project}/${ref.stack}`;
    } catch {
      return reply.code(400).send({ error: 'stack must be <project>/<stack>' });
    }
    try {
      const n = limit === undefined ? undefined : Number.parseInt(limit, 10);
      return { records: await readHistory(parsed, Number.isFinite(n) ? { limit: n } : {}) };
    } catch (err) {
      return reply.code(502).send({ error: `could not read converge records: ${errMessage(err)}` });
    }
  });

  // WHO IS HOLDING THIS FLEET (ADR 0037). Read-only, same token: the answer names the Runs using a
  // Fleet and the deadline each of their claims expires on, which is an operational map of what is
  // running where. It is also what `kontra fleet down` reads before it destroys anything — several
  // Runs may hold one Fleet now, so a hand that tears one down is taking somebody else's Machines
  // unless it looked first.
  app.get('/api/infra/stacks/:fqn/leases', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, INFRA_ROUTE_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { fqn } = req.params as { fqn: string };
    let parsed: string;
    try {
      const ref = parseFqn(decodeURIComponent(fqn));
      parsed = `${ref.project}/${ref.stack}`;
    } catch {
      return reply.code(400).send({ error: 'stack must be <project>/<stack>' });
    }
    try {
      return await readLeases(parsed);
    } catch (err) {
      return reply.code(502).send({ error: `could not read leases: ${errMessage(err)}` });
    }
  });

  // The page itself is NOT token-gated — it contains no data, only the code that fetches it.
  // Everything it displays comes from the two routes above, which are.
  app.get('/infra', async (_req, reply) => {
    return reply.type('text/html; charset=utf-8').send(INFRA_DASHBOARD_HTML);
  });
}
