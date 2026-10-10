/**
 * THE WORKSPACE IS PER REQUEST (ADR 0070 §3, plan WP-71).
 *
 * A request names the workspace it is about in `X-Kontra-Workspace` (or `?workspace=` where it
 * cannot set a header: an `EventSource`). The hook checks the caller may name it and runs the rest
 * of the request inside that workspace's namespace scope, so every store the handler reaches —
 * Temporal, the report tables, the lake — addresses that workspace without being told.
 *
 * A request that names none gets `.current`, as before: the CLI and the laptop console still work
 * unchanged. What changes is that `.current` stops being the only answer, so two people on one
 * install can look at two workspaces at once.
 *
 * EITHER WAY THE ANSWER IS READ ONCE, when the request arrives. Reading `.current` per call let a
 * switch in the middle of a request split its Temporal reads and its store writes across two
 * workspaces, and let a stream follow the console into another workspace mid-stream.
 *
 * WHO MAY NAME WHAT
 *   - a console session: the workspaces its user is a member of. A session scoped to some
 *     workspaces is refused on every `/api` route whose effective workspace (named or `.current`)
 *     is not one of them, except the sign-in surface and the workspace routes, which check for
 *     themselves.
 *   - a configured service token: any workspace. It is an install credential.
 *   - nobody (no credential, or one that matches nothing): only the current workspace, unless the
 *     install configures no console user and no service token at all. The `legacy` routes are
 *     reachable anonymously (`apiGate.ts`), and a header must not widen that reach from one
 *     workspace to all of them.
 *
 * THE SCOPE IS ENTERED IN `preHandler`, NOT `onRequest`. A body is parsed on the socket's own
 * events, outside any context `onRequest` set up, so a scope entered there is lost for every POST.
 * The check runs early (`onRequest`) and the scope is entered after parsing, right before the
 * handler.
 */

import type { FastifyInstance, FastifyRequest } from 'fastify';

import { timingSafeEqualStr } from '../auth';
import {
  LEGACY_WORKSPACE,
  WorkspaceRefused,
  activeWorkspace,
  assertWorkspaceName,
  currentNamespace,
  listWorkspaceNames,
  namespaceFor,
  withinNamespace,
  workspacesParent,
} from '../workspaces';
import { bearerOf, isMember, sessions, type Membership } from './session';
import { consoleUsers } from './users';

export const WORKSPACE_HEADER = 'x-kontra-workspace';
export const WORKSPACE_QUERY = 'workspace';

/** Every service-token variable an `/api` route admits on. Holding one is holding the install. */
export const SERVICE_TOKEN_VARS = [
  'KONTRA_STATE_TOKEN',
  'KONTRA_EXPLORE_TOKEN',
  'KONTRA_RUN_TOKEN',
  'KONTRA_SECRETS_TOKEN',
] as const;

/** Routes that do not run in a workspace, or check membership themselves. */
const UNSCOPED: ReadonlySet<string> = new Set([
  'GET /api/health',
  'GET /api/login',
  'POST /api/login',
  'POST /api/logout',
  'GET /api/workspaces',
  'PUT /api/workspaces/current',
  'POST /api/workspaces',
]);

const resolved = new WeakMap<FastifyRequest, string>();

/** The caller's membership: a session's, every workspace for a service token, or `null`. */
export function membershipOf(
  authorization: string | undefined,
  env: NodeJS.ProcessEnv = process.env
): Membership | null {
  const live = sessions.look(bearerOf(authorization));
  if (live) return live.workspaces;
  for (const v of SERVICE_TOKEN_VARS) {
    const token = env[v];
    if (token && timingSafeEqualStr(authorization ?? '', `Bearer ${token}`)) return '*';
  }
  // AN INSTALL WITH NO CREDENTIAL CONFIGURED ANYWHERE is open by construction (`checkOptionalBearer`
  // admits everyone there), so refusing a header would protect nothing and break the console.
  const anyToken = SERVICE_TOKEN_VARS.some((v) => Boolean(env[v]));
  if (!anyToken && consoleUsers(env).length === 0) return '*';
  return null;
}

/** The workspace a request names, or `undefined`. Throws `WorkspaceRefused` on a malformed name. */
function requested(req: FastifyRequest): string | undefined {
  const header = req.headers[WORKSPACE_HEADER];
  const query = (req.query as Record<string, unknown> | undefined)?.[WORKSPACE_QUERY];
  const raw = header ?? query;
  if (raw === undefined) return undefined;
  if (typeof raw !== 'string') throw new WorkspaceRefused(`name one workspace, not several`);
  const name = raw.trim();
  if (name !== LEGACY_WORKSPACE) assertWorkspaceName(name);
  return name;
}

function exists(name: string, env: NodeJS.ProcessEnv): boolean {
  if (name === LEGACY_WORKSPACE) return true;
  const parent = workspacesParent(env);
  return parent !== '' && listWorkspaceNames(parent).includes(name);
}

export function installWorkspaceScope(app: FastifyInstance, env: NodeJS.ProcessEnv = process.env): void {
  app.addHook('onRequest', async (req, reply) => {
    const url = req.routeOptions?.url;
    if (!url || !url.startsWith('/api')) return;
    if (UNSCOPED.has(`${req.method} ${url}`)) return;
    let name: string | undefined;
    try {
      name = requested(req);
    } catch (err) {
      return reply.code(400).send({ error: (err as Error).message });
    }
    const membership = membershipOf(req.headers.authorization, env);
    if (name === undefined) {
      // NO NAME: `.current`, as before — but a scoped session may not fall back into a workspace it
      // is not a member of, which is the one way the old default could leak.
      if (membership !== null && membership !== '*') {
        const current = activeWorkspace(env) || LEGACY_WORKSPACE;
        if (!isMember(membership, current)) {
          return reply.code(403).send({
            error: `forbidden: this session is not a member of workspace ${JSON.stringify(current)}; name one in the ${WORKSPACE_HEADER} header`,
          });
        }
      }
      resolved.set(req, currentNamespace(env));
      return;
    }
    if (!exists(name, env)) return reply.code(404).send({ error: `no workspace ${JSON.stringify(name)}` });
    if (membership === null) {
      // An anonymous caller keeps exactly the reach it had: the current workspace.
      if (name !== (activeWorkspace(env) || LEGACY_WORKSPACE)) {
        return reply.code(401).send({ error: `unauthorized: naming a workspace needs a credential` });
      }
    } else if (!isMember(membership, name)) {
      return reply.code(403).send({ error: `forbidden: not a member of workspace ${JSON.stringify(name)}` });
    }
    resolved.set(req, namespaceFor(name, env));
  });

  app.addHook('preHandler', (req, _reply, done) => {
    const namespace = resolved.get(req);
    if (namespace === undefined) return done();
    withinNamespace(namespace, done);
  });
}
