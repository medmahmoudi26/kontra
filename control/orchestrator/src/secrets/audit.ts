/**
 * THE RESOLUTION LEDGER: which actor read which of your credentials, when, and for what run.
 *
 * A credential grant that cannot be reviewed is a grant the operator made once and never sees
 * again. This is the surface that answers "which actor read my key, and when" — which is what
 * turns the binding into a decision they can revisit rather than one they have to remember.
 *
 * ── THE RECORD MUST NOT BECOME THE LEAK ───────────────────────────────────────────────────────
 *
 * {@link Resolution} carries the secret's NAME and nothing derived from its bytes. NOT a
 * fingerprint, NOT a last-four, NOT a length. The store already refuses those for the reason
 * `types.ts` states — a hash of a low-entropy secret is a brute-forceable copy of it and a suffix
 * is a fraction of one — and an audit log is a strictly worse place for one than the store is: it
 * is append-only, it is the file most likely to be shipped to a log aggregator, and it is the one
 * an operator will happily paste into a ticket. There is no field here a value could be put in,
 * and `slotStore.test.ts` sweeps a WRITTEN `resolutions.log` for a sentinel — the file on disk,
 * after a real resolution, not the object this module returns.
 *
 * ── REFUSALS ARE RECORDED TOO ─────────────────────────────────────────────────────────────────
 *
 * Every outcome is a line, not only the successful ones. An actor asking for a slot it never
 * declared is the case this whole design exists to refuse, and a refusal nobody can see is a
 * refusal nobody acts on: the interesting security event is precisely the one that did NOT hand
 * over a credential. A log of successes only would be a log that goes quiet exactly when it
 * matters.
 *
 * ── ONE FILE, APPEND-ONLY, BOUNDED ────────────────────────────────────────────────────────────
 *
 * JSON lines beside the store, 0600, in the same directory as the values — an audit trail that
 * rested somewhere the store's own permissions do not cover would be the weakest link about the
 * strongest thing. Appended rather than rewritten, so a crash mid-write costs the line being
 * written and never the history behind it, and BOUNDED at {@link CAP} entries because this runs on
 * an install whose disk is the operator's laptop: the trim keeps the newest, which is the half
 * anybody reads.
 */

import { appendFileSync, existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { secretsDir } from './keyring';

/** What happened when an actor asked for a slot. Every one of these is a line in the log. */
export type ResolutionOutcome =
  /** The value was handed over. */
  | 'resolved'
  /** The actor asked for a slot its declaration does not contain — refused (`slots.ts`). */
  | 'undeclared'
  /** Declared, and the operator has bound nothing to it. */
  | 'unbound'
  /** Bound to a secret whose every version is revoked. */
  | 'revoked'
  /** Bound to a secret that has been destroyed. */
  | 'missing'
  /** The store refused the resolution itself — an owned secret, a bad version pin. */
  | 'forbidden';

/**
 * ONE RESOLUTION, as the operator reads it back. `(actor, version, slot, run)` is the tuple the
 * slice is specified in; `secret` and `secretVersion` are the operator's half of the answer —
 * which of THEIR credentials this reached, and which version of it.
 */
export interface Resolution {
  at: number;
  actor: string;
  /**
   * The actor VERSION, as the worker declared itself.
   *
   * SELF-ASSERTED, and said so here rather than left to be assumed: an identity token names one
   * ACTOR and carries no version (`identity.ts`), so this is the version the worker says it is
   * running. It is exactly as true as the code-serving boundary that minted the identity, which is
   * the same bound `identity.ts` already documents for `sub`. Recording it is still worth it — it
   * is the difference between "probe read this key" and "the probe build that added the slot read
   * this key" — but it is not a claim the orchestrator verified.
   */
  version: string;
  slot: string;
  /** The run the worker was serving, or empty when it did not name one (a load outside a run). */
  run: string;
  /** The operator's secret NAME. Never any part of its value, and never a digest of one. */
  secret: string;
  /** Which version answered. 0 when nothing was resolved. */
  secretVersion: number;
  outcome: ResolutionOutcome;
}

/** How many entries the log keeps. Trimmed to this on the write that exceeds {@link SLACK}. */
const CAP = 5000;

/** Trimming rewrites the whole file, so it happens at CAP + this rather than on every append. */
const SLACK = 1000;

/** What a filtered read narrows on. Every field is optional; all of them AND together. */
export interface ResolutionFilter {
  actor?: string;
  slot?: string;
  run?: string;
  secret?: string;
  /** Newest first, capped. Defaults to 200 — a page, not a file. */
  limit?: number;
}

export class ResolutionLog {
  private readonly file: string;
  private readonly dir: string;

  constructor(opts: { dir?: string } = {}) {
    this.dir = opts.dir ?? secretsDir();
    this.file = path.join(this.dir, 'resolutions.log');
  }

  /** Where the ledger rests. Shown beside the trail, for the reason the store's location is. */
  get location(): string {
    return this.file;
  }

  /**
   * Append one line. NEVER THROWS OUT — a ledger that could fail a resolution would be a new way
   * for a run to die, and an operator whose disk filled would find their actors unable to read
   * credentials they had already been granted. What it must not do is go quiet: a failure prints,
   * because a silent audit log is worse than no audit log.
   */
  record(entry: Resolution): void {
    try {
      if (!existsSync(this.dir)) mkdirSync(this.dir, { recursive: true, mode: 0o700 });
      appendFileSync(this.file, `${JSON.stringify(entry)}\n`, { mode: 0o600 });
      this.trim();
    } catch (err) {
      console.error(
        `[secrets] could not append to the resolution ledger at ${this.file}: ${
          err instanceof Error ? err.message : String(err)
        }`
      );
    }
  }

  /** Newest first. A malformed line is SKIPPED rather than thrown on — one corrupt append must
   *  not take the whole trail out of the page that is the only way to read it. */
  list(filter: ResolutionFilter = {}): Resolution[] {
    const limit = filter.limit && filter.limit > 0 ? Math.min(filter.limit, CAP) : 200;
    const out: Resolution[] = [];
    for (const entry of this.readAll().reverse()) {
      if (filter.actor && entry.actor !== filter.actor) continue;
      if (filter.slot && entry.slot !== filter.slot) continue;
      if (filter.run && entry.run !== filter.run) continue;
      if (filter.secret && entry.secret !== filter.secret) continue;
      out.push(entry);
      if (out.length >= limit) break;
    }
    return out;
  }

  private readAll(): Resolution[] {
    if (!existsSync(this.file)) return [];
    const out: Resolution[] = [];
    for (const line of readFileSync(this.file, 'utf8').split('\n')) {
      if (!line.trim()) continue;
      try {
        out.push(JSON.parse(line) as Resolution);
      } catch {
        // See list(): a torn line is skipped, never thrown.
      }
    }
    return out;
  }

  /** Keep the newest {@link CAP}. Temp file plus rename, so a crash mid-trim leaves the old log. */
  private trim(): void {
    const all = this.readAll();
    if (all.length <= CAP + SLACK) return;
    const kept = all.slice(all.length - CAP);
    const tmp = `${this.file}.${process.pid}.tmp`;
    writeFileSync(tmp, kept.map((e) => `${JSON.stringify(e)}\n`).join(''), { mode: 0o600 });
    renameSync(tmp, this.file);
  }
}
