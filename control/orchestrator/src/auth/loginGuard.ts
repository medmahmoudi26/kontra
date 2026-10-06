/**
 * WHAT STOPS `POST /api/login` BEING BOTH AN ORACLE AND A LEVER.
 *
 * The route itself is careful — one answer for every failure, a malformed hash locks, and an unknown
 * user is verified against a decoy so timing says nothing. What it had no answer for is VOLUME.
 *
 * ── THE TWO THINGS AN UNBOUNDED LOGIN ROUTE GIVES AWAY ──────────────────────────────────────────
 *
 * 1. UNLIMITED GUESSES. scrypt makes one attempt expensive; it does nothing about a million of them.
 *    The install's password is generated (`kontra init`), so it is not in a wordlist — but
 *    `kontra user add` takes whatever an operator types, and nothing here bounded the attempts
 *    against it.
 *
 * 2. THE CONTROL PLANE'S I/O, which is the one that surprised. `node:crypto`'s scrypt does not run
 *    on the event loop — it runs on the LIBUV THREADPOOL, which defaults to four threads and is
 *    SHARED with every `fs` and `dns` call the orchestrator makes. Each verification holds a thread
 *    for ~150 ms and 32 MiB. So a few hundred concurrent unauthenticated requests do not merely slow
 *    logins down: they queue ahead of every file read in the process, and the decoy-hash defense
 *    AMPLIFIES it, because an attacker does not need to know a single username to make the work
 *    happen. Reading the served bundle, the audit log and config.yaml all stall behind them.
 *
 * So two controls, and they are different controls for the two different problems:
 *
 *   the gate       bounds CONCURRENT scrypt work, protecting the threadpool. Shed, do not queue.
 *   the bucket     bounds ATTEMPTS PER SOURCE over time, protecting the password.
 *
 * BOTH ARE CHECKED BEFORE ANY SCRYPT RUNS. A limiter that rejects after doing the expensive thing
 * is not a limiter; it is a log line.
 *
 * ── WHY PER-SOURCE AND NOT PER-USERNAME ─────────────────────────────────────────────────────────
 *
 * Keying attempts on the submitted username is the stronger defense against someone grinding one
 * known account, and it is not taken here for two reasons. It hands anybody a LOCKOUT: ten wrong
 * guesses at `admin` and the operator cannot sign in to their own box. And it reintroduces the
 * enumeration oracle the route is built to avoid — a limit that engages per name is observable, so
 * "which names exist" becomes answerable by watching which keys throttle.
 *
 * `req.ip` is Fastify's socket peer and `trustProxy` is off, so it is the kernel's view of who
 * connected and cannot be set by a header. On a loopback appliance it is usually `127.0.0.1` for
 * everything, which makes the bucket effectively global — and that is the correct behaviour for a
 * single-operator install, not a shortcoming: the attacker and the operator are on the same address,
 * so the budget they share is the one that matters.
 *
 * ── STATE LIVES IN MEMORY AND DIES WITH THE PROCESS ─────────────────────────────────────────────
 *
 * Same as `sessions`. A restart forgives every bucket, which is the honest trade: the alternative is
 * a Redis dependency in the sign-in path, and sign-in has to work when Redis does not.
 */

/** Tunables. Exported so tests can build a guard that does not take a real minute to exercise. */
export interface LoginGuardOptions {
  /** How many scrypt verifications may be in flight at once. */
  concurrency: number;
  /** How many may WAIT for a slot. Beyond this the request is shed with 429. */
  queueDepth: number;
  /** Attempts allowed per source per window. */
  attempts: number;
  /** The window, in milliseconds. */
  windowMs: number;
  /** Clock, for tests. */
  now: () => number;
}

/**
 * CONCURRENCY 2 OF FOUR THREADPOOL THREADS, leaving two for the `fs` and `dns` work the rest of the
 * process needs to keep answering. Three would make a login flood a storage outage.
 *
 * QUEUE DEPTH 4, so a real operator's click never loses to a transient burst, and the eleventh
 * simultaneous request is refused in microseconds instead of waiting on a thread.
 *
 * TEN ATTEMPTS A MINUTE is far above human typing — an operator who fumbles a generated password
 * three times is unaffected — and far below useful guessing: it caps an attacker at 14,400 a day
 * against a credential with vastly more entropy than that.
 */
export const DEFAULTS: LoginGuardOptions = {
  concurrency: 2,
  queueDepth: 4,
  attempts: 10,
  windowMs: 60_000,
  now: () => Date.now(),
};

/** Why a request was refused, or `null` when it may proceed. */
export type Refusal =
  | { reason: 'busy'; retryAfterSeconds: number }
  | { reason: 'too-many-attempts'; retryAfterSeconds: number };

/** One source's attempt record: a fixed window, reset when it expires. */
interface Bucket {
  count: number;
  windowStart: number;
}

export class LoginGuard {
  private readonly opts: LoginGuardOptions;
  private readonly buckets = new Map<string, Bucket>();
  private inFlight = 0;
  private waiting = 0;

  constructor(opts: Partial<LoginGuardOptions> = {}) {
    this.opts = { ...DEFAULTS, ...opts };
  }

  /**
   * Count an attempt, reserve a slot, and run `work` in it — or refuse, having done no scrypt.
   *
   * ONE METHOD AND NOT TWO, AND THAT IS A CORRECTNESS CHOICE. The obvious shape is a synchronous
   * `admit()` the route checks, then a separate `run()`. It has a hole: the reservation would
   * happen inside `run`, so if an `await` ever appeared between the two calls — a log line, an
   * audit write, a refactor — every request in a flood would pass `admit` while `waiting` was still
   * zero, and the gate would admit all of them. The bound would read as enforced and hold nothing.
   *
   * Reserving and running in one call cannot be sequenced wrongly by a caller, so the hazard is not
   * a rule to remember.
   *
   * THE ATTEMPT IS COUNTED ON THE WAY IN, not on failure. Counting only failures would let an
   * attacker run the threadpool flat for free, since every guess in a guessing attack is wrong.
   */
  async attempt<T>(source: string, work: () => Promise<T>): Promise<Refusal | { value: T }> {
    const now = this.opts.now();
    const bucket = this.buckets.get(source);
    if (!bucket || now - bucket.windowStart >= this.opts.windowMs) {
      this.buckets.set(source, { count: 1, windowStart: now });
    } else if (bucket.count >= this.opts.attempts) {
      const remaining = this.opts.windowMs - (now - bucket.windowStart);
      return { reason: 'too-many-attempts', retryAfterSeconds: Math.max(1, Math.ceil(remaining / 1000)) };
    } else {
      bucket.count += 1;
    }

    // Bounded without a timer. One entry per source address, so a loopback install holds exactly
    // one; this is for the install that is not loopback, where an orchestrator up for a month would
    // otherwise keep a bucket for every address that ever tried.
    if (this.buckets.size > 1024) this.sweep();

    // The gate is second: a source within its budget can still be shed when the box is saturated.
    // Shedding does not refund the attempt — the work of deciding is not free either, and a refund
    // would make saturation a way to attempt without being counted.
    if (this.waiting >= this.opts.queueDepth) {
      return { reason: 'busy', retryAfterSeconds: 1 };
    }

    // RESERVED SYNCHRONOUSLY, before the first await below, which is what makes the check above
    // mean anything.
    this.waiting += 1;
    try {
      while (this.inFlight >= this.opts.concurrency) {
        await new Promise<void>((resolve) => setTimeout(resolve, 5));
      }
      this.inFlight += 1;
    } finally {
      this.waiting -= 1;
    }
    try {
      return { value: await work() };
    } finally {
      // Load-bearing: a verification that throws must still return its slot, or the gate closes
      // permanently after `concurrency` malformed hashes.
      this.inFlight -= 1;
    }
  }

  /**
   * Forget a source's attempts — called on a SUCCESSFUL sign-in.
   *
   * Without this an operator who mistypes nine times and then gets it right spends the rest of the
   * window locked out of the box they just proved they own.
   */
  forgive(source: string): void {
    this.buckets.delete(source);
  }

  /** For tests and for the shape of a future `/api/health` line. */
  snapshot(): { inFlight: number; waiting: number; sources: number } {
    return { inFlight: this.inFlight, waiting: this.waiting, sources: this.buckets.size };
  }

  /** Drop expired buckets. Called from `attempt` when the map grows, never on a timer. */
  sweep(): number {
    const now = this.opts.now();
    let dropped = 0;
    for (const [key, bucket] of this.buckets) {
      if (now - bucket.windowStart >= this.opts.windowMs) {
        this.buckets.delete(key);
        dropped += 1;
      }
    }
    return dropped;
  }
}

/** The process-wide guard the login route uses. One per orchestrator, like `sessions`. */
export const loginGuard = new LoginGuard();
