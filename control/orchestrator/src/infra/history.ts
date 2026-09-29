/**
 * Pulumi's CONVERGE RECORDS — one per `up`, `preview` or `destroy` this backend has run.
 *
 * ── THE RECORDS WERE ALREADY ON DISK AND NOTHING READ THEM ───────────────────────────────────────
 *
 * `./state.ts` reads `.pulumi/stacks/<project>/<stack>.json` — the CURRENT desired state — and stops
 * there. Beside it the DIY backend keeps `.pulumi/history/<project>/<stack>/*.history.json`, one file
 * per converge, and no process in this repository opened one. MEASURED on the live volume 2026-09-26:
 * 112 records across 20 stacks, 74 `update` and 38 `destroy`, 106 `succeeded` and 6 `failed`. The
 * console's `packages/core/src/infra/history.ts` has parsed this shape — with tests — since the slice
 * that added it, against a route that did not exist.
 *
 * THE CHECKPOINT AND THE RECORDS ANSWER DIFFERENT QUESTIONS, which is why this is a second reader
 * rather than a field on the first. A checkpoint says what EXISTS; it is rewritten in place, so it
 * cannot say that a Fleet was torn down and stood up again this afternoon, or that the teardown
 * failed. These records are the only thing on this host that can — and a teardown that failed and was
 * never noticed is a Droplet still being billed.
 *
 * ── A STACK THAT NEVER CONVERGED ANSWERS AN EMPTY LIST, NOT A 404 ───────────────────────────────
 *
 * `readStack` answers `null` there and the route turns that into a 404, which is right for a document
 * that does not exist. A HISTORY is a set, and the empty set is a real answer: this stack has never
 * converged. The distinction is load-bearing on the other side — the console's loader treats a 404
 * here as "this control plane does not serve that route" and draws NO strip, while an empty list
 * draws a strip with no ticks. Collapsing them would make a console running against an older control
 * plane assert that every stack on the volume has never converged, which is false for all of them.
 *
 * ── MILLISECONDS ON THE WAY OUT ─────────────────────────────────────────────────────────────────
 *
 * Pulumi writes `startTime` and `endTime` as epoch SECONDS. Read as milliseconds the first record on
 * the live volume is 1970-01-21 rather than 2026-09-23, so a reader that trusts the number verbatim
 * draws every converge fifty-six years old — wrong in a way that still renders. The console detects
 * the unit rather than assuming it, and it should keep doing that; this normalises anyway, because a
 * server that hands out one unit is the more correct server and the ambiguity should not travel.
 */

import { readdir, readFile } from 'node:fs/promises';
import * as path from 'node:path';

// `./paths` and not `./workspace`: this module READS files, it never runs an engine, and importing
// the Automation API to learn a directory would put Pulumi in the API's module graph. Same rule as
// `./state.ts`, which is this file's sibling in every sense.
import { stateDir } from './paths';

/**
 * One converge, as this route reports it.
 *
 * NARROWED TO SIX FIELDS, AND `environment` IS DELIBERATELY NOT ONE OF THEM. Each record on disk
 * carries an `environment` map, and while Pulumi redacts it to `PULUMI_CONFIG_PASSPHRASE: 'set'`
 * rather than to a value, a field this type does not name is a field no surface can print by
 * accident. `config` is dropped for the plainer reason that all 112 records measured have `{}`.
 */
export interface ConvergeRecord {
  /** `update` / `destroy` on disk today; `preview`, `refresh`, `import` and `rename` are Pulumi's
   *  other update kinds and pass through unchanged rather than being folded into a known set. */
  kind: string;
  /** `succeeded` / `failed` on disk today; `in-progress` is Pulumi's third. */
  result: string;
  /** Epoch MILLISECONDS — see the header. `0` when the record carried no time at all. */
  startTime: number;
  endTime: number;
  /** `{create, update, delete, replace, same}`, any subset. ABSENT on 2 of the 112 records
   *  measured, and absent here too: a converge whose counts Pulumi did not write must not report
   *  zero changes, which is a different claim from "we do not know". */
  resourceChanges?: Record<string, number>;
  /** The operator's `-m` message. Empty on every record measured; carried because it is the one
   *  field a human wrote. */
  message?: string;
  /** The engine that ran it, from `environment['pulumi.version']`. The one field worth lifting out
   *  of a map this type otherwise refuses to carry: a converge that behaved differently after an
   *  engine upgrade is otherwise unattributable. */
  engine?: string;
}

/**
 * How many records one read returns, newest first.
 *
 * `kontra-docker-fleet/canary` alone held 51 of the 112 on the live volume, and a Fleet that is
 * converged on every run accumulates without bound — the backend never prunes them. A cap keeps one
 * stack's history from being the whole response, and the caller can ask for more.
 */
export const DEFAULT_LIMIT = 50;
export const MAX_LIMIT = 500;

function historyDir(project: string, stack: string): string {
  return path.join(stateDir(), '.pulumi', 'history', project, stack);
}

/**
 * Epoch seconds or milliseconds → milliseconds, DETECTED rather than assumed.
 *
 * Below 1e11 the value can only be seconds (1e11 seconds is the year 5138); at or above it can only
 * be milliseconds (1e11 ms is 1973). Detection rather than a constant multiply because this reader
 * must keep working if a future engine writes the other unit — and because a record that has already
 * been normalised once must not be multiplied again.
 */
export function toMs(v: unknown): number {
  if (typeof v !== 'number' || !Number.isFinite(v) || v <= 0) return 0;
  return v < 1e11 ? Math.round(v * 1000) : Math.round(v);
}

/**
 * The nanosecond stamp the backend puts in a record's FILENAME — `<stack>-<nanos>.history.json`.
 *
 * SORTED ON THIS RATHER THAN ON mtime OR ON `startTime`, and each alternative is wrong in its own
 * way. An mtime is rewritten by anything that touches the volume — a backup restore reorders the
 * whole history. `startTime` is the field two of the records measured do not reliably carry, and a
 * record with no time would sort to the beginning of time and claim to be the oldest converge this
 * stack ever ran. The filename stamp is written once, by the engine, and is monotonic.
 *
 * IT IS RETURNED AS DIGITS AND NEVER AS A `number`, which is not fastidiousness. A real stamp is
 * `1790632599584975927` — about 1.79e18, while a double carries 53 bits of integer precision and
 * runs out at 9.007e15. `Number()` on one silently rounds the last three digits away, so two
 * converges inside the same ~256 ns compare EQUAL and the sort puts them in whatever order the
 * directory listing happened to have. Converges are seconds apart in practice, which is exactly why
 * this would never have been noticed; {@link cmpStamp} compares the digits instead, exactly.
 */
export function stampOf(file: string): string {
  const m = /-(\d+)\.history\.json$/.exec(file);
  // Leading zeros stripped so `007` and `7` are one stamp — `cmpStamp` compares by length first.
  return m ? m[1]!.replace(/^0+(?=\d)/, '') : '';
}

/** Two digit strings as integers, exactly, with no float in the path: longer is larger, and equal
 *  lengths compare lexicographically. Ascending, so `sort((a, b) => cmpStamp(b, a))` is newest-first. */
export function cmpStamp(a: string, b: string): number {
  if (a.length !== b.length) return a.length - b.length;
  return a < b ? -1 : a > b ? 1 : 0;
}

export interface ReadHistoryOptions {
  /** Newest-first cap. Clamped to {@link MAX_LIMIT}; anything unparseable takes the default. */
  limit?: number;
}

/**
 * Every converge this backend recorded for one stack, newest first.
 *
 * NEVER THROWS FOR AN ABSENT STACK, and never throws for ONE unreadable record either. A history
 * directory that is not there is a stack that has never converged — the empty set, see the header.
 * A single file that is truncated or half-written is a record this reader skips, because the
 * alternative is that one bad file on a volume makes the other fifty unreadable. A converge in
 * flight is exactly when a half-written file exists, which is exactly when somebody is looking.
 */
export async function readHistory(
  fqn: string,
  opts: ReadHistoryOptions = {}
): Promise<ConvergeRecord[]> {
  const [project, stack] = fqn.split('/');
  if (!project || !stack) return [];

  const n = Number(opts.limit);
  const limit = Number.isFinite(n) && n > 0 ? Math.min(Math.trunc(n), MAX_LIMIT) : DEFAULT_LIMIT;

  const dir = historyDir(project, stack);
  let names: string[];
  try {
    names = await readdir(dir);
  } catch {
    return []; // never converged, or this host holds no state at all
  }

  // `.checkpoint.json` sits beside every record and is a whole second copy of the stack — the
  // largest one on the live volume is 283 KB. Reading those to throw them away would make this
  // route the most expensive read on the API. `.attrs` is the backend's own bookkeeping.
  const files = names
    .filter((f) => f.endsWith('.history.json'))
    .sort((a, b) => cmpStamp(stampOf(b), stampOf(a)))
    .slice(0, limit);

  const out: ConvergeRecord[] = [];
  for (const f of files) {
    let raw: unknown;
    try {
      raw = JSON.parse(await readFile(path.join(dir, f), 'utf8'));
    } catch {
      continue; // truncated, half-written, or not JSON — see the header
    }
    const rec = narrowRecord(raw);
    if (rec) out.push(rec);
  }
  return out;
}

/** One record off disk, narrowed to {@link ConvergeRecord}. `undefined` when it is not an object at
 *  all — which is what a proxy's error page looks like if one ever lands in this directory. */
export function narrowRecord(raw: unknown): ConvergeRecord | undefined {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return undefined;
  const r = raw as Record<string, unknown>;

  // COUNTS ARE COPIED KEY BY KEY, and only the numeric ones. The whole object is Pulumi's and this
  // type promises a `Record<string, number>`; passing it through verbatim would let a string reach a
  // caller that is about to add it up.
  let changes: Record<string, number> | undefined;
  const rc = r['resourceChanges'];
  if (typeof rc === 'object' && rc !== null && !Array.isArray(rc)) {
    changes = {};
    for (const [k, v] of Object.entries(rc as Record<string, unknown>)) {
      if (typeof v === 'number' && Number.isFinite(v)) changes[k] = v;
    }
  }

  const env = r['environment'];
  const engine =
    typeof env === 'object' && env !== null
      ? (env as Record<string, unknown>)['pulumi.version']
      : undefined;

  return {
    kind: typeof r['kind'] === 'string' ? (r['kind'] as string) : '',
    result: typeof r['result'] === 'string' ? (r['result'] as string) : '',
    startTime: toMs(r['startTime']),
    endTime: toMs(r['endTime']),
    ...(changes === undefined ? {} : { resourceChanges: changes }),
    ...(typeof r['message'] === 'string' && r['message'] !== ''
      ? { message: r['message'] as string }
      : {}),
    ...(typeof engine === 'string' && engine !== '' ? { engine } : {}),
  };
}
