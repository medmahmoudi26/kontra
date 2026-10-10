/**
 * The workspace routes under per-request workspaces (ADR 0070 §3): a session lists only its own,
 * and only a member of every workspace changes the install's default or makes a new one.
 */

import { mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import Fastify, { type FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../temporalClient', () => ({ clientFor: async () => ({}) }));

import { sessions } from '../auth/session';
import { CONSOLE_USERS_VAR } from '../auth/users';
import { CURRENT_FILE, writeCurrentName } from '../workspaces';
import { registerWorkspaceRoutes } from './workspaces';

let parent: string;
let app: FastifyInstance;
const saved = { ...process.env };

beforeEach(async () => {
  parent = mkdtempSync(path.join(os.tmpdir(), 'ws-routes-'));
  for (const n of ['hello', 'bugbounty']) mkdirSync(path.join(parent, n));
  writeCurrentName(parent, 'hello');
  for (const k of ['KONTRA_STATE_TOKEN', 'KONTRA_EXPLORE_TOKEN', 'KONTRA_RUN_TOKEN', 'KONTRA_SECRETS_TOKEN']) delete process.env[k];
  process.env.KONTRA_WORKSPACES = parent;
  process.env[CONSOLE_USERS_VAR] = Buffer.from(
    JSON.stringify([
      { name: 'admin', password_hash: 'scrypt$2$1$1$c2FsdA$aGFzaA' },
      { name: 'ana', password_hash: 'scrypt$2$1$1$c2FsdA$aGFzaA', workspaces: ['bugbounty'] },
    ]),
    'utf8'
  ).toString('base64');
  app = Fastify();
  registerWorkspaceRoutes(app);
  await app.ready();
});

afterEach(async () => {
  await app.close();
  rmSync(parent, { recursive: true, force: true });
  process.env = { ...saved };
});

const bearer = (token: string) => ({ authorization: `Bearer ${token}` });

describe('listing', () => {
  it('shows a scoped session only its own workspaces, and not the install default when it is another', async () => {
    const s = sessions.mint('ana', ['console'], ['bugbounty']);
    const res = await app.inject({ method: 'GET', url: '/api/workspaces', headers: bearer(s.token) });
    const body = res.json() as { names: string[]; current: string; currentPath: string };
    expect(body.names).toEqual(['bugbounty']);
    expect(body.current).toBe('bugbounty');
    expect(body.currentPath).toBe('');
    sessions.revoke(s.token);
  });

  it('shows the admin everything', async () => {
    const s = sessions.mint('admin');
    const res = await app.inject({ method: 'GET', url: '/api/workspaces', headers: bearer(s.token) });
    expect((res.json() as { names: string[] }).names.sort()).toEqual(['bugbounty', 'hello']);
    sessions.revoke(s.token);
  });

  it('needs a credential once the install has one', async () => {
    expect((await app.inject({ method: 'GET', url: '/api/workspaces' })).statusCode).toBe(401);
  });
});

describe('changing the install', () => {
  it('refuses a scoped session switching the default or creating a workspace', async () => {
    const s = sessions.mint('ana', ['console'], ['bugbounty']);
    const put = await app.inject({ method: 'PUT', url: '/api/workspaces/current', headers: bearer(s.token), payload: { name: 'bugbounty' } });
    expect(put.statusCode).toBe(403);
    expect(readFileSync(path.join(parent, CURRENT_FILE), 'utf8').trim()).toBe('hello');
    const post = await app.inject({ method: 'POST', url: '/api/workspaces', headers: bearer(s.token), payload: { name: 'third' } });
    expect(post.statusCode).toBe(403);
    sessions.revoke(s.token);
  });

  it('lets the admin switch', async () => {
    const s = sessions.mint('admin');
    const put = await app.inject({ method: 'PUT', url: '/api/workspaces/current', headers: bearer(s.token), payload: { name: 'bugbounty' } });
    expect(put.statusCode).toBe(200);
    expect(readFileSync(path.join(parent, CURRENT_FILE), 'utf8').trim()).toBe('bugbounty');
    sessions.revoke(s.token);
  });
});
