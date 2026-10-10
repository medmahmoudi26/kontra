/**
 * The workspace a request is about, named per request and checked against who is asking (ADR 0070).
 */

import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import Fastify, { type FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { activeNamespace, writeCurrentName } from '../workspaces';
import { SessionBook, sessions } from './session';
import { CONSOLE_USERS_VAR } from './users';
import { installWorkspaceScope, WORKSPACE_HEADER } from './workspaceScope';

let parent: string;
let app: FastifyInstance;
let env: NodeJS.ProcessEnv;
let entered: () => void = () => undefined;
let release: () => void = () => undefined;
const saved = { ...process.env };

beforeEach(async () => {
  parent = mkdtempSync(path.join(os.tmpdir(), 'ws-scope-'));
  for (const n of ['hello', 'bugbounty', 'scraping']) mkdirSync(path.join(parent, n));
  writeCurrentName(parent, 'hello');
  // The handler reads `activeNamespace()`, which falls back to `process.env`; the hook gets the
  // same object so both sides read one configuration.
  for (const k of ['KONTRA_STATE_TOKEN', 'KONTRA_EXPLORE_TOKEN', 'KONTRA_RUN_TOKEN', 'KONTRA_SECRETS_TOKEN', CONSOLE_USERS_VAR, 'KONTRA_NAMESPACE']) {
    delete process.env[k];
  }
  process.env.KONTRA_WORKSPACES = parent;
  env = process.env;
  app = Fastify();
  installWorkspaceScope(app, env);
  const echo = async () => {
    // AFTER AN AWAIT, because a scope that only holds for the synchronous part of a handler is the
    // bug this exists to rule out.
    await new Promise((r) => setTimeout(r, 5));
    return { namespace: activeNamespace() };
  };
  app.get('/api/probe', echo);
  app.post('/api/probe', echo);
  app.get('/api/workspaces', async () => ({ unscoped: activeNamespace() }));
  app.get('/api/held', async () => {
    entered();
    await new Promise<void>((r) => (release = r));
    return { namespace: activeNamespace() };
  });
  await app.ready();
});

afterEach(async () => {
  await app.close();
  rmSync(parent, { recursive: true, force: true });
  process.env = { ...saved };
});

function users(list: Array<{ name: string; workspaces?: string[] }>): void {
  process.env[CONSOLE_USERS_VAR] = Buffer.from(
    JSON.stringify(list.map((u) => ({ ...u, password_hash: 'scrypt$2$1$1$c2FsdA$aGFzaA' }))),
    'utf8'
  ).toString('base64');
}

async function probe(headers: Record<string, string> = {}, method: 'GET' | 'POST' = 'GET', url = '/api/probe') {
  const res = await app.inject({
    method,
    url,
    headers: method === 'POST' ? { ...headers, 'content-type': 'application/json' } : headers,
    payload: method === 'POST' ? JSON.stringify({ some: 'body' }) : undefined,
  });
  return { status: res.statusCode, body: res.json() as { namespace?: string; error?: string } };
}

describe('a request names its workspace', () => {
  it('reads .current when it names none, as before', async () => {
    expect(await probe()).toEqual({ status: 200, body: { namespace: 'ws-hello' } });
  });

  it('runs the handler in the named workspace, across awaits and after a body is parsed', async () => {
    expect((await probe({ [WORKSPACE_HEADER]: 'bugbounty' })).body.namespace).toBe('ws-bugbounty');
    expect((await probe({ [WORKSPACE_HEADER]: 'bugbounty' }, 'POST')).body.namespace).toBe('ws-bugbounty');
  });

  it('takes ?workspace= for a client that cannot set a header', async () => {
    const res = await app.inject({ method: 'GET', url: '/api/probe?workspace=scraping' });
    expect(res.json()).toEqual({ namespace: 'ws-scraping' });
  });

  it('keeps two concurrent requests in their own workspaces', async () => {
    const [a, b] = await Promise.all([probe({ [WORKSPACE_HEADER]: 'bugbounty' }), probe({ [WORKSPACE_HEADER]: 'scraping' })]);
    expect([a.body.namespace, b.body.namespace]).toEqual(['ws-bugbounty', 'ws-scraping']);
  });

  it('reads .current once, when the request arrives', async () => {
    const inHandler = new Promise<void>((r) => (entered = r));
    const pending = probe({}, 'GET', '/api/held');
    await inHandler;
    writeCurrentName(parent, 'scraping');
    release();
    expect((await pending).body.namespace).toBe('ws-hello');
  });

  it('maps `default` to the legacy namespace', async () => {
    process.env.KONTRA_NAMESPACE = 'kontra';
    expect((await probe({ [WORKSPACE_HEADER]: 'default' })).body.namespace).toBe('kontra');
  });

  it('refuses a malformed name, two names, and one that does not exist', async () => {
    expect((await probe({ [WORKSPACE_HEADER]: '../hello' })).status).toBe(400);
    expect((await probe({ [WORKSPACE_HEADER]: 'hello, scraping' })).status).toBe(400);
    expect((await probe({ [WORKSPACE_HEADER]: 'nope' })).status).toBe(404);
  });

  it('leaves the workspace routes unscoped: they check for themselves', async () => {
    const res = await app.inject({ method: 'GET', url: '/api/workspaces', headers: { [WORKSPACE_HEADER]: 'nope' } });
    expect(res.statusCode).toBe(200);
  });
});

describe('who may name which workspace', () => {
  it('admits a session to its own workspaces and refuses the rest', async () => {
    users([{ name: 'ana', workspaces: ['bugbounty'] }]);
    const s = sessions.mint('ana', ['console'], ['bugbounty']);
    const auth = { authorization: `Bearer ${s.token}` };
    expect((await probe({ ...auth, [WORKSPACE_HEADER]: 'bugbounty' })).body.namespace).toBe('ws-bugbounty');
    expect((await probe({ ...auth, [WORKSPACE_HEADER]: 'hello' })).status).toBe(403);
    sessions.revoke(s.token);
  });

  it('does not let a scoped session fall back into a .current it is not a member of', async () => {
    users([{ name: 'ana', workspaces: ['bugbounty'] }]);
    const s = sessions.mint('ana', ['console'], ['bugbounty']);
    expect((await probe({ authorization: `Bearer ${s.token}` })).status).toBe(403);
    writeCurrentName(parent, 'bugbounty');
    expect((await probe({ authorization: `Bearer ${s.token}` })).body.namespace).toBe('ws-bugbounty');
    sessions.revoke(s.token);
  });

  it('admits a service token to any workspace', async () => {
    process.env.KONTRA_RUN_TOKEN = 'run-token-for-tests';
    const res = await probe({ authorization: 'Bearer run-token-for-tests', [WORKSPACE_HEADER]: 'scraping' });
    expect(res.body.namespace).toBe('ws-scraping');
  });

  it('keeps an anonymous caller to the current workspace once anything is configured', async () => {
    users([{ name: 'admin' }]);
    expect((await probe({ [WORKSPACE_HEADER]: 'hello' })).body.namespace).toBe('ws-hello');
    expect((await probe({ [WORKSPACE_HEADER]: 'bugbounty' })).status).toBe(401);
    expect((await probe({ authorization: 'Bearer not-a-token', [WORKSPACE_HEADER]: 'bugbounty' })).status).toBe(401);
  });

  it('lets anyone name any workspace on an install that configures no credential', async () => {
    expect((await probe({ [WORKSPACE_HEADER]: 'bugbounty' })).body.namespace).toBe('ws-bugbounty');
  });
});

describe('a session remembers its membership', () => {
  it('defaults to every workspace, the laptop tier admin', () => {
    const book = new SessionBook();
    const s = book.mint('admin');
    expect(book.look(s.token)?.workspaces).toBe('*');
  });

  it('carries the list it was minted with', () => {
    const book = new SessionBook();
    const s = book.mint('ana', ['console'], ['bugbounty']);
    expect(book.look(s.token)?.workspaces).toEqual(['bugbounty']);
  });
});

