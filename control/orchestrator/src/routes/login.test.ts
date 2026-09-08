/**
 * Signing in, and what the token it hands out is good for.
 *
 * The console's `Authorization` used to be BAKED INTO ITS BUNDLE at build time. This is what
 * replaced it, so the properties worth pinning are the ones that make handing a browser a token
 * safe: one answer for every failure, a malformed hash that locks, and a session that is not a
 * service token.
 */

import Fastify, { type FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { hashPassword } from '../auth/password';
import { SessionBook, bearerOf } from '../auth/session';
import { consoleUsers, CONSOLE_USERS_VAR } from '../auth/users';
import { registerLoginRoutes } from './login';

const PASSWORD = 'correct-horse-battery-staple';

function encodeUsers(users: Array<{ name: string; password_hash: string }>): string {
  return Buffer.from(JSON.stringify(users), 'utf8').toString('base64');
}

let app: FastifyInstance;

beforeEach(async () => {
  app = Fastify();
  registerLoginRoutes(app);
  await app.ready();
});
afterEach(async () => {
  await app.close();
  delete process.env[CONSOLE_USERS_VAR];
});

async function withUser(name: string, password: string): Promise<void> {
  const password_hash = await hashPassword(password);
  process.env[CONSOLE_USERS_VAR] = encodeUsers([{ name, password_hash }]);
}

describe('signing in', () => {
  it('hands out a token for the right password', async () => {
    await withUser('admin', PASSWORD);
    const res = await app.inject({ method: 'POST', url: '/api/login', payload: { user: 'admin', password: PASSWORD } });
    expect(res.statusCode).toBe(200);
    const body = res.json() as { token: string; user: string; expiresAt: number };
    expect(body.user).toBe('admin');
    expect(body.token.length).toBeGreaterThan(30);
    expect(body.expiresAt).toBeGreaterThan(Date.now());
  });

  it('gives one answer to a wrong password and to a user who does not exist', async () => {
    // A different message for each is a user-enumeration oracle, and the operator gains nothing
    // from the distinction — they know which of the two they typed.
    await withUser('admin', PASSWORD);
    const wrongPassword = await app.inject({ method: 'POST', url: '/api/login', payload: { user: 'admin', password: 'no' } });
    const noSuchUser = await app.inject({ method: 'POST', url: '/api/login', payload: { user: 'nobody', password: PASSWORD } });
    expect(wrongPassword.statusCode).toBe(401);
    expect(noSuchUser.statusCode).toBe(401);
    expect(wrongPassword.json()).toEqual(noSuchUser.json());
  });

  it('refuses a stored hash that will not parse, rather than 500ing or passing', async () => {
    // A config somebody hand-edited into nonsense must LOCK the console. ADR 0039's rule: a check
    // that cannot run is a check that failed.
    process.env[CONSOLE_USERS_VAR] = encodeUsers([{ name: 'admin', password_hash: 'garbage' }]);
    const res = await app.inject({ method: 'POST', url: '/api/login', payload: { user: 'admin', password: PASSWORD } });
    expect(res.statusCode).toBe(401);
  });

  it('says which side is unconfigured when there is no console user', async () => {
    delete process.env[CONSOLE_USERS_VAR];
    const res = await app.inject({ method: 'POST', url: '/api/login', payload: { user: 'admin', password: PASSWORD } });
    expect(res.statusCode).toBe(503);
    expect(res.json().error).toMatch(/kontra init|kontra user add/);
  });

  it('reports whether signing in is possible, without naming anyone', async () => {
    delete process.env[CONSOLE_USERS_VAR];
    expect((await app.inject({ method: 'GET', url: '/api/login' })).json()).toEqual({ enabled: false });
    await withUser('admin', PASSWORD);
    const res = await app.inject({ method: 'GET', url: '/api/login' });
    expect(res.json()).toEqual({ enabled: true });
    expect(res.body).not.toContain('admin'); // the enumeration oracle the login itself refuses to be
  });

  it('rejects a request that is not a login attempt', async () => {
    await withUser('admin', PASSWORD);
    const res = await app.inject({ method: 'POST', url: '/api/login', payload: { user: 'admin' } });
    expect(res.statusCode).toBe(400);
  });
});

describe('the session a login hands out', () => {
  it('expires, and using it puts the clock back', async () => {
    let now = 1_000_000;
    const book = new SessionBook(() => now, 1000);
    const { token } = book.mint('admin');
    expect(book.verify(token)).toBe('admin');
    now += 900;
    expect(book.verify(token), 'using it should extend it').toBe('admin');
    now += 1001;
    expect(book.verify(token), 'idle past the TTL is gone').toBeNull();
  });

  it('does not admit a token it never minted', async () => {
    const book = new SessionBook();
    book.mint('admin');
    expect(book.verify('not-a-real-token')).toBeNull();
    expect(book.verify('')).toBeNull();
    expect(book.verify(undefined)).toBeNull();
  });

  it('is revoked by signing out, idempotently', async () => {
    const book = new SessionBook();
    const { token } = book.mint('admin');
    book.revoke(token);
    expect(book.verify(token)).toBeNull();
    book.revoke(token); // twice is not an error
  });

  it('is bounded, dropping the oldest rather than locking an operator out', async () => {
    const book = new SessionBook();
    for (let i = 0; i < 600; i += 1) book.mint('admin');
    expect(book.outstanding).toBeLessThanOrEqual(512);
  });
});

describe('parsing what the CLI exports', () => {
  it('reads base64 JSON', () => {
    process.env[CONSOLE_USERS_VAR] = encodeUsers([{ name: 'a', password_hash: 'scrypt$1$1$1$AA$BB' }]);
    expect(consoleUsers()).toEqual([{ name: 'a', passwordHash: 'scrypt$1$1$1$AA$BB' }]);
  });

  it('collapses every unreadable form to no users, never to a user with no hash', () => {
    // An entry with a name and no hash would be an account any password opens — the one outcome
    // worse than no account at all.
    for (const bad of [
      'not base64 at all!!',
      Buffer.from('not json', 'utf8').toString('base64'),
      Buffer.from('{"not":"an array"}', 'utf8').toString('base64'),
      encodeUsers([{ name: 'a', password_hash: '' }] as never),
      encodeUsers([{ name: '', password_hash: 'x' }] as never),
    ]) {
      process.env[CONSOLE_USERS_VAR] = bad;
      expect(consoleUsers(), bad.slice(0, 20)).toEqual([]);
    }
  });

  it('bearerOf parses the header and nothing else', () => {
    expect(bearerOf('Bearer abc')).toBe('abc');
    expect(bearerOf('bearer abc')).toBe('abc');
    expect(bearerOf('Basic abc')).toBeUndefined();
    expect(bearerOf(undefined)).toBeUndefined();
  });
});
