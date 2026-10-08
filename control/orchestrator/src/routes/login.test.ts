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

import { DEFAULTS } from '../auth/loginGuard';
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

/**
 * A user whose hash is CHEAP to verify, for the tests that exercise the attempt counter.
 *
 * Those tests spend the whole budget, so at the real N=32768 they run scrypt dozens of times and
 * the file becomes the slowest in the suite — measured at 20s. What they are asserting is the
 * guard's bookkeeping and the shape of its refusal, neither of which depends on the work factor.
 * The cost parameters are what `auth/password.ts` reads out of the hash string, so a cheap one
 * exercises exactly the same code path.
 *
 * The tests above, which are about the credential itself, keep the real cost.
 */
async function withCheapUser(name: string, password: string): Promise<void> {
  const password_hash = await hashPassword(password, { N: 2, r: 1, p: 1 });
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
    expect(bearerOf('BEARER abc')).toBe('abc');
    expect(bearerOf('  Bearer   abc  ')).toBe('abc');
    expect(bearerOf('Bearer\tabc')).toBe('abc');
    expect(bearerOf('Basic abc')).toBeUndefined();
    expect(bearerOf(undefined)).toBeUndefined();
    // AT LEAST ONE SPACE. `Bearerabc` is a different scheme with a long name, not a token.
    expect(bearerOf('Bearerabc')).toBeUndefined();
    expect(bearerOf('Bearer')).toBeUndefined();
    expect(bearerOf('Bearer   ')).toBeUndefined();
    // A LINE TERMINATOR IN THE TOKEN WAS NEVER ACCEPTED, because the old pattern's `.` could not
    // cross one. The regex-free version has to refuse it deliberately.
    expect(bearerOf('Bearer a\nb')).toBeUndefined();
    expect(bearerOf('Bearer a\rb')).toBeUndefined();
  });

  /**
   * THE REASON IT IS NOT A REGEX. `/^Bearer\s+(.+)$/i` is polynomial — `\s+` and `.+` both match a
   * space and `.` cannot cross a line terminator, so this input backtracked once per space and
   * rescanned from each. Header parsing runs before any credential is checked, so the cost was
   * unauthenticated.
   *
   * A BOUND RATHER THAN A COMPARISON, because a ratio against the old implementation would be a
   * test of this runner's mood. 100k spaces is ~1 ms linear and seconds quadratic; 250 ms is far
   * enough from both to never flake and still fail loudly on a reintroduced quantifier.
   */
  it('bearerOf does not backtrack on a long whitespace run', () => {
    const hostile = `bearer ${' '.repeat(100_000)}x\ny`;
    const started = performance.now();
    expect(bearerOf(hostile)).toBeUndefined();
    expect(performance.now() - started).toBeLessThan(250);
  });
});

/**
 * VOLUME, which the properties above say nothing about.
 *
 * Every test here uses its own `remoteAddress`. The guard is a module singleton with a per-source
 * budget, so sharing an address across tests would let one spend another's — and the symptom would
 * be an unrelated test 429ing, which is a bad afternoon.
 */
describe('too many sign-ins', () => {
  it('429s past the attempt budget, and says when to come back', async () => {
    await withCheapUser('admin', PASSWORD);
    const ip = '10.9.0.1';
    const attempt = () =>
      app.inject({
        method: 'POST',
        url: '/api/login',
        remoteAddress: ip,
        payload: { user: 'admin', password: 'wrong' },
      });

    for (let i = 0; i < DEFAULTS.attempts; i++) {
      expect((await attempt()).statusCode, `attempt ${i + 1} of the budget`).toBe(401);
    }
    const over = await attempt();
    expect(over.statusCode).toBe(429);
    expect(Number(over.headers['retry-after'])).toBeGreaterThan(0);
  });

  /**
   * THE LIMIT MUST NOT BE AN ORACLE. If a throttled response differed from a refused one, an
   * attacker would learn where the limit is — and worse, a per-name limit would make "does this
   * account exist" answerable by watching which keys throttle. The body is identical on purpose.
   */
  it('answers a throttled attempt with the same body as a refused one', async () => {
    await withCheapUser('admin', PASSWORD);
    const ip = '10.9.0.2';
    const attempt = () =>
      app.inject({
        method: 'POST',
        url: '/api/login',
        remoteAddress: ip,
        payload: { user: 'admin', password: 'wrong' },
      });

    let refusedBody: unknown;
    for (let i = 0; i < DEFAULTS.attempts; i++) refusedBody = (await attempt()).json();
    const throttled = await attempt();
    expect(throttled.statusCode).toBe(429);
    expect(throttled.json()).toEqual(refusedBody);
  });

  /**
   * 429 AND NOT 401. `packages/core/src/run/session.ts` clears the console's token on a 401, so
   * answering a rate limit with one would sign the operator out of the whole console and read to
   * them as "the orchestrator restarted".
   */
  it('does not sign the console out when it throttles', async () => {
    await withCheapUser('admin', PASSWORD);
    const ip = '10.9.0.3';
    for (let i = 0; i < DEFAULTS.attempts; i++) {
      await app.inject({
        method: 'POST',
        url: '/api/login',
        remoteAddress: ip,
        payload: { user: 'admin', password: 'wrong' },
      });
    }
    const throttled = await app.inject({
      method: 'POST',
      url: '/api/login',
      remoteAddress: ip,
      payload: { user: 'admin', password: 'wrong' },
    });
    expect(throttled.statusCode).not.toBe(401);
    expect(throttled.statusCode).toBe(429);
  });

  /** One address cannot spend another's budget — the operator keeps working while someone grinds. */
  it('budgets each address separately', async () => {
    await withCheapUser('admin', PASSWORD);
    for (let i = 0; i < DEFAULTS.attempts + 1; i++) {
      await app.inject({
        method: 'POST',
        url: '/api/login',
        remoteAddress: '10.9.0.4',
        payload: { user: 'admin', password: 'wrong' },
      });
    }
    const elsewhere = await app.inject({
      method: 'POST',
      url: '/api/login',
      remoteAddress: '10.9.0.5',
      payload: { user: 'admin', password: PASSWORD },
    });
    expect(elsewhere.statusCode).toBe(200);
  });

  /**
   * FORGIVEN ON SUCCESS. An operator who fumbles a generated password up to the budget and then
   * gets it right must not spend the rest of the window locked out of their own box — the password
   * is generated and long, so fumbling it is the expected case, not the suspicious one.
   */
  it('forgives an address that signs in successfully', async () => {
    await withCheapUser('admin', PASSWORD);
    const ip = '10.9.0.6';
    // One short of the budget, then the real password.
    for (let i = 0; i < DEFAULTS.attempts - 1; i++) {
      await app.inject({
        method: 'POST',
        url: '/api/login',
        remoteAddress: ip,
        payload: { user: 'admin', password: 'wrong' },
      });
    }
    const good = await app.inject({
      method: 'POST',
      url: '/api/login',
      remoteAddress: ip,
      payload: { user: 'admin', password: PASSWORD },
    });
    expect(good.statusCode).toBe(200);

    // The budget is back: a fresh wrong attempt is a 401, not a 429.
    const after = await app.inject({
      method: 'POST',
      url: '/api/login',
      remoteAddress: ip,
      payload: { user: 'admin', password: 'wrong' },
    });
    expect(after.statusCode).toBe(401);
  });
});
