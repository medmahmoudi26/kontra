/**
 * RESOLVING A `{ref}` IN A `{% code %}` BLOCK — from the object store, never through an actor.
 *
 * ── THE MEASURED REASON THIS FILE EXISTS ───────────────────────────────────────────────────────
 *
 * There are two ways to turn a claim check into bytes, and only one of them can be used here.
 *
 *   • FROM A WORKFLOW, a dereference dispatches the `kontra.fetch_blob` activity to
 *     `<actor>-<version>` — a task queue only an actor host registers. With the Fleet gone there is
 *     nothing polling it and the read HANGS: measured on a live Run that finished its work, sealed
 *     every partition, then sat in `running` with the activity reading `stalled` for one hour and
 *     forty minutes before somebody cancelled it, at the cost of two hours of idle Droplet billing.
 *   • FROM THE ORCHESTRATOR, which holds the object-store credentials, it is a GET by digest.
 *     `ClaimCheckCodec` already does exactly this (`codec/claimCheck.ts:128`:
 *     `this.store.getVerified(ref.sha256, 'claim-check')`). No activity, no queue, no actor.
 *
 * A report renders AFTER every Fleet scope has exited, so the first path is not an edge case here —
 * it is the normal case. This resolver takes the second. The hang is then structurally impossible
 * rather than bounded, which is a better property than any timeout.
 *
 * ── AND IT STILL HAS A DEADLINE ────────────────────────────────────────────────────────────────
 *
 * Not for the actor — there is none — but because the S3 GET underneath has NO configured timeout
 * anywhere in this codebase. A store that accepts a connection and never answers would wedge a render
 * exactly as the missing actor did, by a different route. The deadline is the defence against that
 * one, and it is the whole defence.
 *
 * ── AN UNRESOLVED REF IS AN OUTPUT, NOT AN ERROR ───────────────────────────────────────────────
 *
 * Every failure here answers `{ unresolved }` and the render continues: the block gets a marker
 * naming what was not read, the version stores as `ok`, and the report says what it could not see. A
 * report that states its own gap is worth more than no report — and an empty block would be
 * indistinguishable from a ref that legitimately held nothing, which is the confusion that cost this
 * project a live Run in a different place.
 */

import type { RefResolution } from './codeTag';

/** How long a single ref read may take. Short, because a report is presentation work. */
export const DEFAULT_REF_DEADLINE_MS = 5_000;

export function refDeadlineMs(): number {
  const raw = Number(process.env.KONTRA_REPORT_REF_DEADLINE_MS);
  return Number.isFinite(raw) && raw > 0 ? Math.floor(raw) : DEFAULT_REF_DEADLINE_MS;
}

/**
 * The one store capability a resolver needs.
 *
 * MATCHES `ObjectStore.getVerified` EXACTLY, including that it THROWS rather than answering
 * `undefined`: a missing object and a sha256 that does not match the bytes both raise
 * (`codec/objectStore.ts:225`). That is the right shape for a codec — a claim check whose bytes
 * changed is corruption, not absence — and this resolver turns both into a NAMED marker, so the
 * distinction survives into the report instead of being flattened to an empty block.
 */
export interface VerifiedReader {
  getVerified(sha256: string, noun: string): Promise<Uint8Array>;
}

/**
 * The digest a `{ref}` names.
 *
 * TWO SPELLINGS ACCEPTED, because the specification and this codebase disagree. §4.3 writes
 * `{"ref": "<claim-check ref>"}` — a string — while a real claim check in `codec/claimCheck.ts:39` is
 * `{sha256, size, meta}`. A workflow author will return whichever they have in hand, so both are
 * read: a bare 64-hex string, or an object carrying `sha256`.
 */
export function digestOf(ref: string): string | undefined {
  const trimmed = ref.trim();
  if (/^[0-9a-f]{64}$/i.test(trimmed)) return trimmed.toLowerCase();
  // `sha256:<hex>`, which is how a digest is spelled everywhere else in this repo.
  const prefixed = /^sha256:([0-9a-f]{64})$/i.exec(trimmed);
  if (prefixed) return prefixed[1]!.toLowerCase();
  try {
    const parsed = JSON.parse(trimmed) as { sha256?: unknown };
    if (typeof parsed.sha256 === 'string' && /^[0-9a-f]{64}$/i.test(parsed.sha256)) {
      return parsed.sha256.toLowerCase();
    }
  } catch {
    // Not JSON. Falls through to undefined, which the caller renders as a marker.
  }
  return undefined;
}

/**
 * A resolver over one object store.
 *
 * `limitBytes` is honoured by TRUNCATING AFTER THE READ rather than by a ranged GET, and that is a
 * deliberate limitation with a reason: `getVerified` recomputes the sha256 of what it fetched and
 * refuses a mismatch (`codec/claimCheck.ts:19`), which a partial read would fail by construction. So
 * integrity wins over bandwidth here, and `fullBytes` reports the true size so the block can say
 * "showing 1.0 MiB of 4.3 MiB" honestly.
 */
export function objectStoreRefResolver(
  store: VerifiedReader,
  opts: { deadlineMs?: number; now?: () => number } = {}
): (ref: string, limitBytes: number) => Promise<RefResolution> {
  const deadline = opts.deadlineMs ?? refDeadlineMs();
  return async (ref: string, limitBytes: number): Promise<RefResolution> => {
    const sha = digestOf(ref);
    if (!sha) {
      return {
        unresolved: `not a claim-check reference (expected a sha256, got ${ref.length} characters)`,
      };
    }
    let timer: NodeJS.Timeout | undefined;
    try {
      const read = await Promise.race([
        store.getVerified(sha, 'report block'),
        new Promise<'timeout'>((resolve) => {
          timer = setTimeout(() => resolve('timeout'), deadline);
          timer.unref?.();
        }),
      ]);
      if (read === 'timeout') {
        return { unresolved: `the object store did not answer within ${deadline}ms` };
      }
      // Copied into a Buffer rather than cast: `getVerified` answers a Uint8Array, and the block
      // record's API is Buffer because every other byte path in this feature is.
      const bytes = Buffer.from(read);
      const fullBytes = bytes.length;
      return { bytes: fullBytes > limitBytes ? bytes.subarray(0, limitBytes) : bytes, fullBytes };
    } catch (err) {
      // Named, not thrown. A store that is down, misconfigured or refusing credentials is an
      // environment fact; the report records it and renders.
      return { unresolved: `could not read the object store: ${err instanceof Error ? err.message : String(err)}` };
    } finally {
      if (timer) clearTimeout(timer);
    }
  };
}

/** A resolver for an install with no object store: every ref is a marker, and says so. */
export function unconfiguredRefResolver(): (ref: string, limitBytes: number) => Promise<RefResolution> {
  return async () => ({
    unresolved: 'no object store is configured (KONTRA_S3_ENDPOINT), so claim-checked bytes cannot be read',
  });
}
