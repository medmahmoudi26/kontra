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
import {
  WorkspaceRefused,
  createWorkspace,
  describeWorkspaces,
  useWorkspace,
} from '../workspaces';

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
      return useWorkspace(body.name ?? '');
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
      return createWorkspace(body.name ?? '', { seed: Boolean(body.seed), use: body.use !== false });
    } catch (err) {
      if (err instanceof WorkspaceRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not create workspace: ${errMessage(err)}` });
    }
  });
}
