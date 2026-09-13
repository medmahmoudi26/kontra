/**
 * `POST /api/login` — where the console's token comes from.
 *
 * WHY THIS EXISTS. The CLI authenticates by reading `~/.kontra/config.yaml`. A browser cannot, so
 * the console got in by having a bearer BAKED INTO ITS BUNDLE at build time
 * (`VITE_KONTRA_EXPLORE_TOKEN`): a credential inside a build artifact, invalidated by every
 * rotation, and one that a build run for an unrelated reason silently replaced with an empty
 * string — after which every query answered `query: unauthorized` and nothing said why.
 *
 * So the credential stays on the filesystem where the CLI already keeps it, the operator signs in
 * against it, and what comes back is a SESSION token used as `Authorization: Bearer …` for
 * everything the console does. One token, one header, no build-time secret.
 *
 * THE USERS COME FROM THE SAME FILE THE CLI READS. `kontra init` generates the first account at
 * install, prints the password once, and stores only an scrypt hash — so a config that leaks
 * yields nothing to sign in WITH, only something to attack offline, which is what a KDF is for.
 *
 * ── THE THREE THINGS THIS ROUTE IS CAREFUL ABOUT ────────────────────────────────────────────────
 *
 * ONE ANSWER FOR EVERY FAILURE. A wrong password, an unknown user and a malformed stored hash all
 * return the same 401 with the same body. Distinguishing them is a user-enumeration oracle, and the
 * operator gains nothing from the difference — they know which of the two they got wrong.
 *
 * A MALFORMED HASH LOCKS. `verifyPassword` throws on a hash somebody hand-edited into nonsense, and
 * that is caught as a REFUSAL rather than allowed to become a 500 — or worse, a pass. Same posture
 * as ADR 0039's cosign rule: a check that cannot run is a check that failed.
 *
 * THE WORK IS DONE EVEN WHEN THE USER DOES NOT EXIST. scrypt takes ~150 ms; returning early for an
 * unknown name would make "does this account exist" measurable with a stopwatch. So an unknown user
 * is verified against a throwaway hash and takes the same time as a real one.
 */

import type { FastifyInstance } from 'fastify';

import { verifyPassword } from '../auth/password';
import { bearerOf, sessions } from '../auth/session';
import { consoleUsers, type ConsoleUser } from '../auth/users';

/**
 * A hash no password matches, used to spend the same time on an unknown user as on a real one.
 *
 * Its cost parameters must match what `kontra init` writes, or the timing this exists to equalise
 * would differ by the thing it is hiding. Salt and digest are fixed and meaningless — nothing
 * verifies against it, which is the point.
 */
const DECOY_HASH =
  'scrypt$32768$8$1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';

/** The one body every failure returns. */
const REFUSED = { error: 'invalid user or password' } as const;

async function authenticate(users: readonly ConsoleUser[], name: string, password: string): Promise<string | null> {
  const found = users.find((u) => u.name === name);
  const hash = found?.passwordHash ?? DECOY_HASH;
  let ok = false;
  try {
    ok = await verifyPassword(password, hash);
  } catch {
    // A stored hash that will not parse is a refusal, never a pass and never a 500.
    ok = false;
  }
  return ok && found ? found.name : null;
}

export function registerLoginRoutes(app: FastifyInstance): void {
  /**
   * Who may sign in, and whether anyone can.
   *
   * The console asks this BEFORE showing a form, so an install with no console user says so instead
   * of presenting a login nobody can pass. It reports only whether sign-in is possible — never the
   * names, which would be the enumeration oracle the login itself refuses to be.
   */
  app.get('/api/login', async () => ({ enabled: consoleUsers().length > 0 }));

  app.post('/api/login', async (req, reply) => {
    const { user, password } = (req.body ?? {}) as Partial<{ user: string; password: string }>;
    if (typeof user !== 'string' || typeof password !== 'string' || !user) {
      return reply.code(400).send({ error: 'user and password are required' });
    }
    const users = consoleUsers();
    if (users.length === 0) {
      // FAIL CLOSED AND SAY WHICH SIDE IS UNCONFIGURED — the same posture `routes/panels.ts` takes
      // for a missing panel token. An operator chasing a login that cannot work needs to be sent to
      // the config rather than to their own typing.
      return reply.code(503).send({
        error:
          'disabled: no console user is configured. `kontra init` creates one at install; ' +
          '`kontra user add <name>` adds another.',
      });
    }
    const name = await authenticate(users, user, password);
    if (!name) {
      req.log?.warn?.({ ip: req.ip, user }, 'login: refused');
      return reply.code(401).send(REFUSED);
    }
    const session = sessions.mint(name);
    req.log?.info?.({ ip: req.ip, user: name }, 'login: admitted');
    return reply.send({ token: session.token, user: name, expiresAt: session.expiresAt });
  });

  /** Sign out. Idempotent: a token that is already gone is not an error worth reporting. */
  app.post('/api/logout', async (req, reply) => {
    sessions.revoke(bearerOf(req.headers.authorization));
    return reply.send({ ok: true });
  });
}
