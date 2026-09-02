/**
 * THE INFRA SURFACE (ADR 0019) — five routes, and the two functions behind the first two of them.
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

import { checkBearer } from './auth';
import { INFRA_DASHBOARD_HTML } from './infra/dashboard';
import { LEASE_QUERY, leaseWorkflowId, type FleetLeaseSet } from './lease';
// From `infra/paths`, NOT `infra/workspace`: the latter imports the Pulumi Automation API at
// its top, and this is a string parse. See that file's header.
import { parseFqn } from './infra/paths';
import { listStacks, readStack } from './infra/state';
import { errMessage } from './routes/errors';
import { getClient } from './temporalClient';
// The QUEUE, from `queues.ts` and not from `infra.ts`. This route holds no credential and runs no
// engine — it starts a workflow on an address — and importing the provisioner to read that address
// put Pulumi in the API's module graph for a string.
import { infraQueue } from './queues';
import type { StackOp } from './workflows/stack';

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
  /** Last heartbeat from the engine — which resource it is on. Absent once terminal. */
  progress?: Record<string, unknown>;
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
