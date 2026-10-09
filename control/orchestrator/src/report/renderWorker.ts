/**
 * THE RENDER, IN A THREAD OF ITS OWN.
 *
 * ── WHY A WORKER AND NOT A FUNCTION CALL ───────────────────────────────────────────────────────
 *
 * Liquid's render is generator-based and does not yield to the event loop between iterations, so a
 * render holds the thread for its whole duration. Measured on the pinned version: `renderLimit` is
 * checked PERIODICALLY, and a 300 ms budget overshot to 5,022 ms on a tight loop. In the process that
 * serves the API, that is seconds in which no route answers — and acceptance test 5 asks for the
 * opposite in those words ("another route answers in under 100 ms during the render").
 *
 * So the render runs here, and the host can `terminate()` this thread on a wall-clock deadline that
 * does not depend on the library noticing. `renderLimit` remains the in-band limit and produces the
 * good error message; the terminate is the one that cannot be overshot.
 *
 * ── IT BUILDS ITS OWN DEPENDENCIES FROM THE ENVIRONMENT ────────────────────────────────────────
 *
 * Not from the message. A worker that was handed a resolver would need the host to serialise a
 * function; a worker that reads `KONTRA_S3_*` itself needs nothing but the env it already inherits.
 * The message therefore carries data only — template, context, limits — which is also what makes it
 * structured-cloneable without a thought.
 */

import { parentPort, workerData } from 'node:worker_threads';

// THE SUBPATH, NOT THE ROOT. Every src file here imports `@kontra/core/<module>`, and the reason is
// in `shared/core`'s own header: `moduleResolution: "Node"` is node10, which does not read `exports`
// at all, so the bare specifier resolves for vitest and fails `tsc`. The package's `typesVersions` map
// is what makes the subpath work.
import { redactHttp, redactSentence } from '@kontra/core/redaction';

import { ObjectStore } from '../codec/objectStore';
import { buildSnapshot, type ReportSnapshot } from './render';
import { objectStoreRefResolver, unconfiguredRefResolver } from './refs';
import { renderReport, type EngineLimits } from './engine';

/** What the host sends. Data only — see the header. */
export interface RenderRequest {
  template: string;
  context: Record<string, unknown>;
  limits?: EngineLimits;
  snapshotLimitBytes?: number;
}

/** What the host gets back. `ok: false` carries the message a version with `status: 'error'` stores. */
export type RenderResponse =
  | {
      ok: true;
      snapshot: ReportSnapshot;
      bytes: number;
      /** The blocks whose originals must go to `report_secret`, base64. Only the redacted ones. */
      secrets: Array<{ blockId: string; rawB64: string }>;
    }
  | { ok: false; error: string };

export async function runRender(request: RenderRequest): Promise<RenderResponse> {
  try {
    const store = new ObjectStore();
    const { markdown, blocks } = await renderReport(
      request.template,
      request.context,
      {
        resolveRef: store.enabled ? objectStoreRefResolver(store) : unconfiguredRefResolver(),
        redactLatin1: redactHttp,
        redactText: redactSentence,
        ...(request.limits ? { limits: request.limits } : {}),
      }
    );
    const built = buildSnapshot(markdown, blocks, {
      ...(request.snapshotLimitBytes !== undefined ? { limitBytes: request.snapshotLimitBytes } : {}),
    });
    return {
      ok: true,
      snapshot: built.snapshot,
      bytes: built.bytes,
      // ONLY the blocks redaction actually changed carry a `raw`, so this list is the blocks that need
      // an audited reveal path and no others — an unredacted copy of bytes nobody redacted would be a
      // second home for the same data with no second reader.
      secrets: blocks
        .filter((b) => b.raw !== undefined)
        .map((b) => ({ blockId: b.id, rawB64: b.raw!.toString('base64') })),
    };
  } catch (err) {
    // EVERY failure becomes a message, never a rejection that crosses the thread boundary as an
    // unhandled error: §4.5 says a render failure does not fail the run, and the version that records
    // it needs the sentence.
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

// The worker entry. Absent when this module is imported directly (the host's in-process fallback, and
// every test), which is why the body is guarded rather than top-level.
if (parentPort) {
  const request = workerData as RenderRequest;
  void runRender(request).then((response) => {
    parentPort!.postMessage(response);
  });
}
