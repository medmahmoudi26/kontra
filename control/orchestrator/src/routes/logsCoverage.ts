/**
 * `GET /api/logs/coverage` — WHICH WORKERS ARE RUNNING AND NOT LOGGING.
 *
 * ── THE QUESTION NOTHING COULD ANSWER ───────────────────────────────────────────────────────────
 *
 * Every failure in this system's logging path has the same shape, and it is the worst shape a
 * logging failure can take: it is SILENT, and its symptom is indistinguishable from success.
 *
 *   • `logship.sh` shipped four container names, so a fifth actor's lines were simply absent.
 *   • Its marker files survived a restart, after which it followed nothing, forever, while still
 *     printing a healthy startup banner.
 *   • `machine.ts` installs vlagent best-effort — `|| log "vlagent unavailable; continuing without
 *     fleet logs"` — so a Machine can run an entire campaign with no log coverage at all.
 *   • Nothing set `KONTRA_LOG_FORMAT`, so the structured path ADR 0050 §1 built was never on.
 *
 * In every one of those, the console's logs rail draws an empty list, and an empty rail reads as
 * "this Run logged nothing". `routes/logs.ts::unreachable` exists because that confusion is
 * expensive; this route exists because `unreachable` only catches the case where the BACKEND is
 * down. A healthy backend with nothing arriving in it looks perfect.
 *
 * ── IT IS A DIFF, AND BOTH SIDES COME FROM SOMEWHERE AUTHORITATIVE ──────────────────────────────
 *
 * EXPECTED is Temporal's own poller listing: `DescribeTaskQueue` over every queue this control
 * plane knows about, folded to distinct worker identities. That is not a roster kontra maintains —
 * it is the set of processes the SERVER has seen poll, which is the strongest available statement
 * that something is running.
 *
 * SEEN is the distinct `worker` values in VictoriaLogs over the same window.
 *
 * The identity on both sides is the SAME STRING, because `Worker(identity=...)` and
 * `logs.temporal_context` compose it from one function (`internals/workerid.py`,
 * `runtime/go/temporalhost/workerid.go`). That equality is the whole reason this route can be a
 * set difference rather than a heuristic — before the identity work, the two sides had no common
 * key and this check could not have been written at all.
 *
 * ── AN UNKNOWN IS NOT A ZERO ────────────────────────────────────────────────────────────────────
 *
 * If the logs backend cannot be reached, this reports `seen: null` and `ok: null` rather than
 * "every worker is missing". Reporting a backend outage as total log loss would be the same
 * category error the rail's empty state makes, one layer up — and it is the one that would get
 * this check ignored after the second false alarm.
 */

import type { FastifyInstance, FastifyReply, FastifyRequest } from 'fastify';

import { EXPLORE_TOKEN_VARS, checkBearer } from '../auth';
import { describeQueue, type QueueDescriber } from '../panels/pollers';
import type { Repo } from '../db/repo';
import { logsBase, tenant, tenantHeaders } from './logs';

/**
 * How far back to look, in minutes.
 *
 * FIFTEEN, and the number is bounded by Temporal rather than chosen for taste. A poller stays in
 * `DescribeTaskQueue` for about five minutes after it was last seen, so a window shorter than that
 * reports a worker that died four minutes ago as "running and not logging" — a false positive, and
 * false positives are what make a check like this get switched off. Fifteen gives a quiet worker
 * room to say something without letting a genuinely silent one hide.
 */
const DEFAULT_WINDOW_MIN = 15;

/** A wedged logs backend must cost this answer, never the request. Same bound as `logs.ts`. */
const QUERY_TIMEOUT_MS = 10_000;

export interface Coverage {
  windowMinutes: number;
  /** Worker identities Temporal has seen poll. Sorted, so two calls diff cleanly. */
  expected: string[];
  /** Worker identities that wrote a log line. `null` when the logs backend could not be asked. */
  seen: string[] | null;
  /** Running, not logging. THE ANSWER. Empty is the healthy state; `null` means unknown. */
  missing: string[] | null;
  /**
   * Logging, but not polling any queue this control plane knows about.
   *
   * NOT AN ERROR, and worth reporting rather than filtering. It is what a Worker on a queue nobody
   * registered looks like — a stale deploy still running, an actor removed from the catalog while
   * its Machine lives on, or a Worker pointed at the wrong namespace. Each of those is something an
   * operator wants to know about and none of them shows up anywhere else.
   */
  unexpected: string[] | null;
  /** `true` when nothing is missing, `null` when the logs backend could not be reached. */
  ok: boolean | null;
  /** Why `seen` is null, when it is. A sentence, for a human. */
  detail?: string;
}

/** Only queue names Temporal will accept — the same guard `routes/pollers.ts` applies. */
const QUEUE_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

/**
 * Distinct `worker` values in the window, asked of VictoriaLogs directly.
 *
 * THE QUERY IS BUILT HERE AND TAKES NO CALLER INPUT, which is why it does not go through
 * `scopedQuery`. That helper wraps a caller's expression in parentheses and ANDs the tenant on —
 * correct for arbitrary LogsQL, and it cannot carry a PIPE, because `(* | uniq by (worker)) AND …`
 * is not a query. The tenant scope is applied here instead, in the same shape, and the only
 * variable in the string is a number this module clamps.
 */
async function seenWorkers(windowMinutes: number): Promise<{ workers: string[] } | { error: string }> {
  const query =
    `_time:${windowMinutes}m AND _stream:{tenant=${JSON.stringify(tenant())}} AND worker:* ` +
    `| uniq by (worker) | limit 1000`;
  try {
    const res = await fetch(`${logsBase()}/select/logsql/query`, {
      method: 'POST',
      headers: { ...tenantHeaders(), 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ query }),
      signal: AbortSignal.timeout(QUERY_TIMEOUT_MS),
    });
    if (!res.ok) {
      return { error: `the logs backend answered ${res.status} to the coverage query` };
    }
    const workers: string[] = [];
    for (const line of (await res.text()).split('\n')) {
      if (!line.trim()) continue;
      try {
        const row = JSON.parse(line) as { worker?: unknown };
        if (typeof row.worker === 'string' && row.worker) workers.push(row.worker);
      } catch {
        // A torn line costs one identity, never the answer. See `routes/logs.ts` for the same rule.
      }
    }
    return { workers };
  } catch (err) {
    return { error: String((err as Error)?.message ?? err) };
  }
}

/** Worker identities Temporal has seen poll, across every queue this control plane knows about. */
async function expectedWorkers(describer: QueueDescriber, repo: Repo): Promise<string[]> {
  const queues = new Set<string>();
  for (const a of repo.listActors()) queues.add(`${a.name}-${a.version}`);
  for (const w of repo.listWorkflows()) if (w.queue) queues.add(w.queue);

  const found = new Set<string>();
  await Promise.all(
    [...queues]
      .filter((q) => QUEUE_RE.test(q))
      .map(async (queue) => {
        try {
          const state = await describeQueue(describer, queue);
          for (const id of state.identities) found.add(id);
        } catch {
          // A QUEUE THAT CANNOT BE DESCRIBED CONTRIBUTES NOTHING, and deliberately does not fail
          // the whole answer. The alternative is that one unreachable queue reports every other
          // Worker as unexpected, which is a false alarm about the wrong thing.
        }
      })
  );
  return [...found].sort();
}

export async function coverage(
  describer: QueueDescriber,
  repo: Repo,
  windowMinutes = DEFAULT_WINDOW_MIN
): Promise<Coverage> {
  const [expected, seenResult] = await Promise.all([
    expectedWorkers(describer, repo),
    seenWorkers(windowMinutes),
  ]);

  if ('error' in seenResult) {
    return {
      windowMinutes,
      expected,
      seen: null,
      missing: null,
      unexpected: null,
      ok: null,
      detail:
        `could not ask the logs backend at ${logsBase()}, so coverage is UNKNOWN rather than ` +
        `zero — a backend outage is not the same fact as every Worker having gone silent. ` +
        `(${seenResult.error})`,
    };
  }

  const seen = [...new Set(seenResult.workers)].sort();
  const seenSet = new Set(seen);
  const expectedSet = new Set(expected);
  const missing = expected.filter((w) => !seenSet.has(w));
  const unexpected = seen.filter((w) => !expectedSet.has(w));

  return {
    windowMinutes,
    expected,
    seen,
    missing,
    unexpected,
    ok: missing.length === 0,
  };
}

export function registerLogsCoverageRoutes(
  app: FastifyInstance,
  queueDescriber: () => QueueDescriber,
  repo: Repo
): void {
  /**
   * Gated exactly like the rest of the logs surface, and fail-closed for the same reason: a worker
   * identity names a Machine and a queue, which together describe this deployment's shape.
   */
  const admit = (req: FastifyRequest, reply: FastifyReply): boolean => {
    const fail = checkBearer(req.headers.authorization, EXPLORE_TOKEN_VARS);
    if (fail) {
      reply.code(fail.code).send(fail.body);
      return false;
    }
    return true;
  };

  app.get('/api/logs/coverage', async (req, reply) => {
    if (!admit(req, reply)) return;
    const raw = Number((req.query as { windowMinutes?: string })?.windowMinutes);
    // Clamped, not validated-and-refused: this is a diagnostic, and a 400 over a query parameter
    // is a worse answer than a sensible window.
    const windowMinutes = Number.isFinite(raw) && raw > 0 ? Math.min(Math.max(raw, 1), 1440) : DEFAULT_WINDOW_MIN;
    return coverage(queueDescriber(), repo, windowMinutes);
  });
}
