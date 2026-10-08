/**
 * THE OPERATOR AUDIT TRAIL: who did what, when, from where, and whether it was allowed.
 *
 * ── THE GAP THIS CLOSES WAS WRITTEN DOWN AND LEFT OPEN ──────────────────────────────────────────
 *
 * `auth/session.ts` carries this field:
 *
 *     user: string;
 *     // Which console user signed in. Carried so a future audit line can name somebody.
 *
 * Nothing ever named anybody. Until this file, the control plane recorded no trace of a human
 * action: not a sign-in, not a failed sign-in, not a Run started or terminated, not a credential
 * bound to an actor, not a Dataset deleted, not a Fleet provisioned. `secrets/audit.ts` is an
 * excellent ledger and it answers a different question — which ACTOR read which credential. The
 * question "which PERSON did this" had no answer anywhere.
 *
 * ── WHY IT IS A FILE AND NOT ONLY A LOG LINE ────────────────────────────────────────────────────
 *
 * Both, in fact: {@link audit} writes here AND emits a structured record, so "every Run somebody
 * terminated last month" is one LogsQL filter. But the log store cannot be the only home.
 *
 *   • VictoriaLogs retention is ONE setting for everything (`KONTRA_LOGS_RETENTION`). Operational
 *     logs want months; an audit trail wants a year or more, and tying them together means either
 *     paying log volume for audit retention or losing the audit trail to a log policy.
 *   • The store is a container. `docker compose down -v` is a thing operators do.
 *   • An auditor asks for a FILE. A query against a running service is evidence that the service
 *     was running.
 *
 * ── IT ROTATES AND NEVER DROPS ──────────────────────────────────────────────────────────────────
 *
 * `secrets/audit.ts` trims to the newest CAP entries, which is right for a ledger an operator
 * skims and WRONG here: count-trimming means anybody who can generate audit events can
 * evict the evidence of what they did by generating more of them. So this rotates to a dated file
 * at a size bound and deletes a rotated file only when it is older than the retention window.
 * Losing an old record to a stated policy is a decision; losing a recent one to a flood is a
 * vulnerability.
 *
 * ── WHAT IT MUST NEVER CONTAIN ──────────────────────────────────────────────────────────────────
 *
 * No secret values, no digests of them, no session tokens, no passwords — for the reason
 * `secrets/audit.ts` states and one more: this is the file most likely to be handed to somebody
 * outside the company. There is no field here a credential could be put in, and
 * `audit.test.ts` sweeps a written file for sentinels.
 */

import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  statSync,
  unlinkSync,
} from 'node:fs';
import path from 'node:path';

import { bearerOf, sessions } from './auth/session';
import { MACHINE, ROLE } from './workerIdentity';

/**
 * What happened. A closed set, because an audit trail whose vocabulary is free text cannot be
 * queried — and "show me every credential binding last quarter" is the question it exists for.
 *
 * NAMED `<noun>.<verb>` so a LogsQL prefix filter selects a whole family: `action:run.*`.
 */
export type AuditAction =
  /** Console sign-in. Both outcomes are recorded; see {@link AuditEntry.outcome}. */
  | 'login'
  | 'logout'
  /** A Run's lifecycle, as driven by a person rather than by a workflow. */
  | 'run.start'
  | 'run.terminate'
  | 'run.cancel'
  /** The operator's half of the credential story; `secrets/audit.ts` holds the actor's half. */
  | 'secret.bind'
  | 'secret.unbind'
  /** Data that stops existing. */
  | 'dataset.delete'
  /**
   * DATA THAT BECOMES READABLE FROM ANOTHER WORKSPACE (ADR 0053) — the two verbs that move the
   * isolation boundary ADR 0051 draws.
   *
   * They belong here for a reason `dataset.delete` does not have: a deletion announces itself the
   * moment anybody looks for the rows, while a grant is INVISIBLE until somebody uses it. "Who
   * opened this up, and when" has no other answer. The `target` is the full address
   * `<workspace>/<kind>/<name>`, because the workspace is what makes the act cross a boundary.
   */
  | 'dataset.share'
  | 'dataset.unshare';

/*
 * WHAT IS NOT IN THAT UNION YET, STATED RATHER THAN IMPLIED.
 *
 * A vocabulary listing actions nothing emits reads as coverage, which is the one thing an audit
 * trail must never fake. Every member above has a call site; these do not, and each has a reason:
 *
 *   • `fleet.up` / `fleet.down` — provisioning is NOT an HTTP route. A Run asks for Machines
 *     through `kontra.fleet.hold()`, which starts Fleet's own `stackWorkflow` as a child workflow
 *     (CONTEXT-MAP.md). The person accountable is the one who started the Run, which `run.start`
 *     records, and Temporal's history carries the infra Worker's identity for the converge itself.
 *     A line here would duplicate both without adding a fact.
 *   • `secret.create` / `secret.revoke` / `secret.destroy` — these go through `secrets/routes.ts`
 *     and belong here; they are the next call sites to wire, not a decision against them.
 *   • `actor.register` — `kontra actor register` reaches the catalog, and a Worker self-registers
 *     on boot. Auditing the second as a human action would be wrong, and separating the two needs
 *     the caller distinction this file's `callerOf` only just made possible.
 */

/** Was the action performed, or refused? BOTH ARE RECORDED — see {@link AuditLog.record}. */
export type AuditOutcome = 'allowed' | 'refused';

/** How the caller proved who they were. Not a permission level — a provenance. */
export type AuditVia =
  /** A console session minted by `POST /api/login`. `who` is the console user. */
  | 'session'
  /** A service token (`KONTRA_STATE_TOKEN` and friends). `who` names the token's role, never it. */
  | 'token'
  /** An actor's own identity (`secrets/identity.ts`). */
  | 'actor'
  /** No credential was presented. Recorded rather than dropped: a refused anonymous attempt on a
   *  privileged route is exactly the event this trail exists to surface. */
  | 'anonymous';

export interface AuditEntry {
  /** Epoch ms, from the orchestrator's clock — the process that made the decision. */
  at: number;
  action: AuditAction;
  outcome: AuditOutcome;
  /** WHO. A console user name, a token's role, or an actor's name. Never a credential. */
  who: string;
  via: AuditVia;
  /** WHAT it was done to: a run id, a dataset name, a secret NAME, a fleet tag. */
  target: string;
  /** Where the request came from. `req.ip`, which behind a proxy is the proxy — stated, not fixed:
   *  trusting `X-Forwarded-For` without a configured trust boundary is worse than recording the hop
   *  that is actually true. */
  ip: string;
  /** Which orchestrator recorded it. A tenant may run more than one, and "which box decided this"
   *  is a question an incident asks. */
  machine: string;
  role: string;
  /** One sentence, for a human. Never a value, never a token, never an exception with a body. */
  detail?: string;
}

/** Rotate when the live file passes this. 8 MiB is ~40k entries — weeks on a busy install. */
const ROTATE_BYTES = 8 * 1024 * 1024;

/** How long a rotated file is kept. A year, which is what an annual audit period asks for. */
export function retentionDays(): number {
  const raw = Number(process.env.KONTRA_AUDIT_RETENTION_DAYS);
  return Number.isFinite(raw) && raw > 0 ? raw : 365;
}

/**
 * Where the trail rests.
 *
 * ON A VOLUME, and this is not a detail. `server.ts` learned it the hard way with the orchestrator
 * DB: a default relative to the process's WORKDIR is an overlay layer inside the image, destroyed
 * by every `up -d` that recreates the container. An audit trail that disappears on an image upgrade
 * is worse than none, because its absence looks like a period in which nothing happened.
 *
 * Defaults beside the orchestrator DB, which compose already puts on `orchestrator-db`.
 */
export function auditDir(): string {
  if (process.env.KONTRA_AUDIT_DIR) return process.env.KONTRA_AUDIT_DIR;
  const db = process.env.KONTRA_ORCHESTRATOR_DB;
  return db ? path.dirname(db) : path.join(process.cwd(), '.kontra');
}

export interface AuditFilter {
  who?: string;
  action?: AuditAction;
  target?: string;
  outcome?: AuditOutcome;
  /** Only entries at or after this epoch ms. */
  since?: number;
  /** Newest first, capped. Defaults to 200 — a page, not a file. */
  limit?: number;
}

export class AuditLog {
  private readonly dir: string;
  private readonly file: string;

  constructor(opts: { dir?: string } = {}) {
    this.dir = opts.dir ?? auditDir();
    this.file = path.join(this.dir, 'audit.log');
  }

  /** Where it rests, so an operator can hand it over without being told where to look. */
  get location(): string {
    return this.file;
  }

  /**
   * Append one entry.
   *
   * NEVER THROWS OUT. A trail that could fail the action it records would be a new way for an
   * operator's work to die — and the pressure would be to remove the trail. What it must not do is
   * go quiet: a failure prints, because a silent audit log is worse than no audit log at all.
   *
   * REFUSALS ARE RECORDED TOO, which is the half most systems omit. An operator trying to
   * terminate a Run they may not touch, an expired session on a privileged route, an anonymous
   * request at `/api/fleet` — those are precisely the interesting events, and a trail of successes
   * only is a trail that goes quiet exactly when something is happening.
   */
  record(entry: AuditEntry): void {
    try {
      if (!existsSync(this.dir)) mkdirSync(this.dir, { recursive: true, mode: 0o700 });
      this.rotateIfLarge();
      appendFileSync(this.file, `${JSON.stringify(entry)}\n`, { mode: 0o600 });
      this.sweepRotated();
    } catch (err) {
      // eslint-disable-next-line no-console
      console.error(
        `[audit] could not append to ${this.file}: ${err instanceof Error ? err.message : String(err)}`
      );
    }
  }

  /** Newest first, across the live file and any rotated ones still inside the window. */
  list(filter: AuditFilter = {}): AuditEntry[] {
    const limit = filter.limit && filter.limit > 0 ? Math.min(filter.limit, 5000) : 200;
    const out: AuditEntry[] = [];
    for (const file of this.filesNewestFirst()) {
      for (const entry of this.readFile(file).reverse()) {
        if (filter.who && entry.who !== filter.who) continue;
        if (filter.action && entry.action !== filter.action) continue;
        if (filter.target && entry.target !== filter.target) continue;
        if (filter.outcome && entry.outcome !== filter.outcome) continue;
        if (filter.since !== undefined && entry.at < filter.since) continue;
        out.push(entry);
        if (out.length >= limit) return out;
      }
    }
    return out;
  }

  /** The live file first, then rotated ones newest-first by their name, which sorts by date. */
  private filesNewestFirst(): string[] {
    const files = [this.file];
    if (!existsSync(this.dir)) return files;
    const rotated = readdirSync(this.dir)
      .filter((n) => /^audit-.+\.log$/.test(n))
      .sort()
      .reverse()
      .map((n) => path.join(this.dir, n));
    return files.concat(rotated);
  }

  private readFile(file: string): AuditEntry[] {
    if (!existsSync(file)) return [];
    const out: AuditEntry[] = [];
    for (const line of readFileSync(file, 'utf8').split('\n')) {
      if (!line.trim()) continue;
      try {
        out.push(JSON.parse(line) as AuditEntry);
      } catch {
        // A torn line is SKIPPED, never thrown on: one bad append must not take the whole trail
        // out of the only page that reads it.
      }
    }
    return out;
  }

  /**
   * ROTATE, DO NOT TRIM. The distinction is the whole reason this class is not `ResolutionLog`.
   *
   * Trimming to the newest N means anybody who can generate audit events can evict the record of
   * what they did by generating more of them — a self-erasing log, and the erasure looks exactly
   * like normal operation. Rotation moves old entries aside; only {@link sweepRotated} deletes
   * anything, and only on age, which is a stated policy rather than a side effect of volume.
   */
  private rotateIfLarge(): void {
    if (!existsSync(this.file)) return;
    if (statSync(this.file).size < ROTATE_BYTES) return;
    // Seconds in the name, so two rotations in one minute do not collide. `renameSync` is atomic
    // within a filesystem, so a reader either sees the old file or the new one, never a half.
    const stamp = new Date().toISOString().replace(/[:.]/g, '-');
    renameSync(this.file, path.join(this.dir, `audit-${stamp}.log`));
  }

  /** Delete rotated files past the retention window. The LIVE file is never touched here. */
  private sweepRotated(): void {
    const cutoff = Date.now() - retentionDays() * 24 * 60 * 60 * 1000;
    for (const name of readdirSync(this.dir)) {
      if (!/^audit-.+\.log$/.test(name)) continue;
      const full = path.join(this.dir, name);
      try {
        if (statSync(full).mtimeMs < cutoff) unlinkSync(full);
      } catch {
        // A file that vanished under us, or one we may not stat. Neither is worth failing a write.
      }
    }
  }
}

/** The process's trail. One per orchestrator, which is one per Tenant. */
export const auditLog = new AuditLog();

/** What a caller supplies; the rest is stamped here so no call site can forget it. */
export type AuditInput = Omit<AuditEntry, 'at' | 'machine' | 'role'> & {
  at?: number;
};

/**
 * Record one operator action — to the trail AND to the log stream.
 *
 * TWO HOMES, TWO QUESTIONS. The file is the evidence an auditor is handed and is retained on its
 * own policy. The log line is what makes "every Run anybody terminated last month" one LogsQL
 * filter, across every orchestrator, beside the Run's own lines — which the file cannot answer
 * because it is a file on one box.
 *
 * The log line is at WARN for a refusal and INFO for an allowed action, following ADR 0050 §2's
 * rule: raise the level when a reader would be wrong about the result if they missed it.
 */
/**
 * WHO IS MAKING THIS REQUEST, as far as the orchestrator can honestly say.
 *
 * THREE ANSWERS AND THEY ARE NOT RANKED. A console session names a PERSON, which is the answer an
 * audit wants. A service token names a capability and not a human — `KONTRA_STATE_TOKEN` is shared
 * by the CLI, CI and anything an operator scripted, so calling it a user would be a lie the trail
 * cannot distinguish from a real one. Anonymous is recorded rather than dropped, because a refused
 * anonymous request on a privileged route is exactly the event worth seeing.
 *
 * THE TOKEN'S VALUE NEVER LEAVES THIS FUNCTION. Not the token, not a prefix of it, not a hash: a
 * hash of a token is a brute-forceable copy of one, which is the rule `secrets/audit.ts` states
 * for credentials and which applies with more force here — this is the file most likely to be sent
 * to somebody outside the company.
 */
export function callerOf(req: {
  headers: { authorization?: string | undefined };
  ip?: string;
}): { who: string; via: AuditVia; ip: string } {
  const ip = req.ip ?? '';
  const bearer = bearerOf(req.headers.authorization);
  if (!bearer) return { who: 'anonymous', via: 'anonymous', ip };
  const user = sessions.verify(bearer);
  if (user) return { who: user, via: 'session', ip };
  return { who: 'service-token', via: 'token', ip };
}

/** Just enough of a Fastify/pino logger to write a line, so a caller can pass `req.log`. */
export interface AuditLogger {
  info?: (obj: object, msg: string) => void;
  warn?: (obj: object, msg: string) => void;
}

export function audit(input: AuditInput, log?: AuditLogger): AuditEntry {
  const entry: AuditEntry = {
    at: input.at ?? Date.now(),
    action: input.action,
    outcome: input.outcome,
    who: input.who,
    via: input.via,
    target: input.target,
    ip: input.ip,
    machine: MACHINE,
    role: ROLE,
    ...(input.detail ? { detail: input.detail } : {}),
  };
  auditLog.record(entry);
  const line = { audit: true, ...entry };
  const message = `audit: ${entry.who} ${entry.action} ${entry.target} — ${entry.outcome}`;
  if (entry.outcome === 'refused') log?.warn?.(line, message);
  else log?.info?.(line, message);
  return entry;
}
