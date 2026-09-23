/**
 * `GET /api/audit` — reading the operator trail (`../audit.ts`).
 *
 * ── A TRAIL NOBODY CAN READ IS A TRAIL NOBODY MAINTAINS ─────────────────────────────────────────
 *
 * `secrets/audit.ts` has a listing route beside it for exactly this reason, and the reasoning
 * transfers: a ledger whose only reader is `cat` on a container volume gets stale, gets wrong, and
 * nobody notices until the quarter it is asked for. Making it a route means the console can draw
 * it, which means somebody looks at it, which is what keeps it true.
 *
 * ── GATED ON THE SECRETS TOKEN, AND FAIL-CLOSED ─────────────────────────────────────────────────
 *
 * The trail names every person who signed in, every Run anybody started, and every credential
 * binding anybody made. That is a reconnaissance document — not because any single line is a
 * secret, but because together they describe who has authority here and when they use it.
 *
 * `checkBearer` AND NOT `checkOptionalBearer`, which is where this differs from the secrets
 * surface it borrows the token from. The optional form is "require the token if one is
 * configured", so on an install where nobody set `KONTRA_SECRETS_TOKEN` it is OPEN — and the
 * generated spec said so out loud: `"Open: no credential is checked"`, on the audit trail. That is
 * the right trade for a Settings page, which must work on a fresh appliance, and the wrong one
 * here: a world-readable audit log is worse than an audit surface that answers 503 until somebody
 * configures a token. Same posture as `routes/logs.ts` and `explore.ts`, for the same reason.
 *
 * READING IT IS NOT ITSELF AUDITED, and that is a deliberate omission rather than an oversight.
 * A read that writes a line to the thing it read is how a trail fills with its own reflection:
 * one console page left open on a poll turns an audit log into a log of that page. What would
 * make it worth adding is a separate retention for read events, which is its own decision.
 */

import type { FastifyInstance } from 'fastify';

import { auditLog, retentionDays, type AuditAction, type AuditOutcome } from '../audit';
import { checkBearer } from '../auth';
import { SECRETS_TOKEN_VARS } from '../secrets/routes';

export function registerAuditRoutes(app: FastifyInstance): void {
  app.get('/api/audit', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, SECRETS_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);

    const q = req.query as Partial<{
      who: string;
      action: string;
      target: string;
      outcome: string;
      sinceMs: string;
      limit: string;
    }>;
    const limit = q.limit ? Number.parseInt(q.limit, 10) : undefined;
    const since = q.sinceMs ? Number.parseInt(q.sinceMs, 10) : undefined;

    return {
      // WHERE IT RESTS, returned rather than documented. An auditor asks for a file, and the
      // answer to "which file" should not be a paragraph somebody has to find.
      location: auditLog.location,
      retentionDays: retentionDays(),
      entries: auditLog.list({
        ...(q.who ? { who: q.who } : {}),
        ...(q.action ? { action: q.action as AuditAction } : {}),
        ...(q.target ? { target: q.target } : {}),
        ...(q.outcome ? { outcome: q.outcome as AuditOutcome } : {}),
        ...(Number.isFinite(since) ? { since: since as number } : {}),
        ...(Number.isFinite(limit) ? { limit: limit as number } : {}),
      }),
    };
  });
}
