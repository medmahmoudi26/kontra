/**
 * Who may sign in to the console — read from the environment, written by the CLI.
 *
 * THE ACCOUNTS LIVE IN `~/.kontra/config.yaml` and reach this process the way every other config
 * value does: `config.ApplyConfig` exports them, "environment first, file second". This side does
 * not read YAML, deliberately — adding a parser to the control plane to read two fields is a
 * dependency it does not need, and it is the same reasoning that chose scrypt over bcrypt for the
 * hash: the cheapest thing that is also correct.
 *
 * BASE64 OF JSON, and the encoding is load-bearing rather than tidy. A stored hash is
 * `scrypt$N$r$p$salt$hash` — it contains `$`, and compose substitutes `$` in values it passes
 * through. The raw form would arrive corrupted in exactly the deployment this exists for, and
 * corrupted into something that still LOOKS like a hash, so every login would fail as "wrong
 * password" rather than as the configuration error it is.
 *
 * READ PER CALL, NOT CACHED. An operator who adds a user and restarts the process gets them; one
 * who exports the variable into a running dev server gets them too. There is no reload to forget,
 * and a base64 decode plus a JSON parse of two entries is not a cost worth caching against a login
 * that spends 150 ms in a KDF by design.
 */

/** The variable the CLI exports. Named here so a test can assert nothing else reads it. */
export const CONSOLE_USERS_VAR = 'KONTRA_CONSOLE_USERS';

export interface ConsoleUser {
  name: string;
  /** `scrypt$N$r$p$salt$hash`. Never a password — see `cli/internal/creds`. */
  passwordHash: string;
}

/**
 * The configured console accounts, or an empty list.
 *
 * EMPTY IS A LEGITIMATE ANSWER, and every failure to read collapses into it: unset, undecodable,
 * not JSON, not an array, entries missing a field. The login route turns that into a 503 that names
 * the command which creates an account — which is a better outcome than a 500, and a far better one
 * than any reading in which a broken list admits somebody.
 */
export function consoleUsers(env: NodeJS.ProcessEnv = process.env): ConsoleUser[] {
  const raw = env[CONSOLE_USERS_VAR];
  if (!raw) return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(Buffer.from(raw, 'base64').toString('utf8'));
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const out: ConsoleUser[] = [];
  for (const entry of parsed) {
    if (typeof entry !== 'object' || entry === null) continue;
    const { name, password_hash: hash } = entry as { name?: unknown; password_hash?: unknown };
    // A half-written entry is not an account. Admitting a `name` with no hash would be an account
    // that any password opens, which is the one outcome worse than no account at all.
    if (typeof name !== 'string' || !name) continue;
    if (typeof hash !== 'string' || !hash) continue;
    out.push({ name, passwordHash: hash });
  }
  return out;
}
