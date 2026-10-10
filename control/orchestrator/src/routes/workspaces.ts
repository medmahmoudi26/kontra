/**
 * Named workspaces under the Compose parent mount — list, switch, create.
 *
 * ADMISSION: listing is open. Switching and creating take the same optional run token as
 * registering a source: they change which code the catalog discovers.
 */

import type { FastifyInstance } from 'fastify';

import { checkOptionalBearer } from '../auth';
import { RUN_TOKEN_VARS } from '../workflowControl';
import { errMessage } from './errors';
import { clientFor } from '../temporalClient';
import {
  WorkspaceRefused,
  createWorkspace,
  describeWorkspaces,
  namespaceFor,
  useWorkspace,
} from '../workspaces';

/** How long a create or a switch waits for its namespace before answering without it. */
const PROVISION_WAIT_MS = 15_000;

/**
 * THE WORKSPACE'S NAMESPACE, MADE NOW rather than by the first thing that needs it (ADR 0051).
 *
 * A workspace's runs, workers and reports all live in its own Temporal namespace, and a namespace
 * takes a few seconds to become usable after it is registered. Making it here means the first run
 * started from the console does not pay for that, and a Temporal that cannot make one is reported
 * on the request that caused it instead of on some later run. NEVER A REASON TO REFUSE: the
 * workspace exists on disk either way, and every reader provisions on first use too.
 */
async function provision(workspace: string): Promise<{ namespace: string; state: 'ready' | 'pending' | 'failed'; detail?: string }> {
  const namespace = namespaceFor(workspace);
  let timer: NodeJS.Timeout | undefined;
  const late = new Promise<'pending'>((resolve) => {
    timer = setTimeout(() => resolve('pending'), PROVISION_WAIT_MS);
  });
  try {
    const made = clientFor(namespace).then(() => 'ready' as const);
    // A provision that outlives the wait carries on and is not reported twice.
    made.catch(() => undefined);
    const state = await Promise.race([made, late]);
    return { namespace, state };
  } catch (err) {
    return { namespace, state: 'failed', detail: errMessage(err) };
  } finally {
    clearTimeout(timer);
  }
}

export function registerWorkspaceRoutes(app: FastifyInstance): void {
  app.get('/api/workspaces', async (_req, reply) => {
    try {
      return describeWorkspaces();
    } catch (err) {
      return reply.code(502).send({ error: `could not list workspaces: ${errMessage(err)}` });
    }
  });

  app.put('/api/workspaces/current', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const body = (req.body ?? {}) as { name?: string };
    try {
      const used = useWorkspace(body.name ?? '');
      return { ...used, namespace: await provision(body.name ?? '') };
    } catch (err) {
      if (err instanceof WorkspaceRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not switch workspace: ${errMessage(err)}` });
    }
  });

  app.post('/api/workspaces', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const body = (req.body ?? {}) as { name?: string; seed?: boolean; use?: boolean };
    try {
      const created = createWorkspace(body.name ?? '', { seed: Boolean(body.seed), use: body.use !== false });
      return { ...created, namespace: await provision(body.name ?? '') };
    } catch (err) {
      if (err instanceof WorkspaceRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not create workspace: ${errMessage(err)}` });
    }
  });
}
