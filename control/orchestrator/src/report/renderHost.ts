/**
 * RUNNING A RENDER WITHOUT LETTING IT HOLD THE PROCESS.
 *
 * ── THE PROBLEM, MEASURED ──────────────────────────────────────────────────────────────────────
 *
 * Liquid's render does not yield to the event loop, and `renderLimit` is checked PERIODICALLY: a
 * 300 ms budget overshot to 5,022 ms on a tight loop on the pinned version. A render in the process
 * that serves the API is therefore seconds in which no route answers, and acceptance test 5 asks for
 * the opposite in those words.
 *
 * ── THE ANSWER, AND ITS ONE HONEST GAP ─────────────────────────────────────────────────────────
 *
 * The render goes to a worker thread, and the host holds a wall-clock deadline it enforces with
 * `terminate()` — a bound the library cannot overshoot because the library is not consulted.
 *
 * THE WORKER ENTRY IS A `.js` FILE, which exists in a built image (`dist/src/report/renderWorker.js`)
 * and does NOT exist when this module is loaded from TypeScript sources, as vitest does. Rather than
 * invent a loader for the test environment, the host FALLS BACK to an in-process render and says so
 * through `onNote`. The consequence is stated rather than hidden: the worker path is exercised by the
 * built artifact and by nothing in vitest, so acceptance test 5's "another route answers in under
 * 100 ms" is a property of the image, not of the unit suite. The in-process path is still bounded by
 * `renderLimit`, so the fallback is slower to protect, never unprotected.
 */

import { existsSync } from 'node:fs';
import * as path from 'node:path';
import { Worker } from 'node:worker_threads';

import { runRender, type RenderRequest, type RenderResponse } from './renderWorker';

/** How long the host waits before killing the thread. 1.5x the in-band limit — see below. */
export function hardDeadlineMs(renderLimitMs: number): number {
  // 1.5x SO THE IN-BAND LIMIT NORMALLY WINS. `renderLimit` produces a message an author can act on
  // ("template render limit exceeded"); a terminate produces only the fact that it was killed. The
  // multiplier gives the library room for its periodic check — measured overshooting by 2.5x on a
  // pathological loop, which this will cut short, which is the point.
  return Math.max(1_000, Math.ceil(renderLimitMs * 1.5));
}

export interface RenderHostDeps {
  /** Something an operator should know that is not a failure — see historyArchive's split. */
  onNote?: (note: string) => void;
  /** Force the in-process path. Tests use it to assert the fallback rather than the environment. */
  inProcess?: boolean;
}

/** Where the worker entry would be, beside this module. */
function workerEntry(): string {
  return path.join(__dirname, 'renderWorker.js');
}

/**
 * Render once, in a worker when there is one.
 *
 * NEVER REJECTS. Every outcome — a template error, a dead worker, a deadline — comes back as
 * `{ ok: false, error }`, because §4.5 says a render failure stores a version and does not fail the
 * run. A rejection here would make the sweep's error path decide what the report says, which is the
 * wrong place for that sentence to be written.
 */
export async function render(request: RenderRequest, deps: RenderHostDeps = {}): Promise<RenderResponse> {
  const entry = workerEntry();
  if (deps.inProcess || !existsSync(entry)) {
    if (!deps.inProcess) {
      deps.onNote?.(
        `report render: running IN-PROCESS — ${entry} is not present, which is normal when the ` +
          'orchestrator runs from TypeScript sources. A pathological template is still bounded by ' +
          'renderLimit, but it will hold this process for the duration.'
      );
    }
    return runRender(request);
  }

  const deadline = hardDeadlineMs(request.limits?.renderLimitMs ?? 10_000);
  return new Promise<RenderResponse>((resolve) => {
    let settled = false;
    const finish = (response: RenderResponse): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      void worker.terminate();
      resolve(response);
    };
    const worker = new Worker(entry, { workerData: request });
    const timer = setTimeout(() => {
      finish({
        ok: false,
        error:
          `the render did not finish within ${deadline}ms and was stopped. This is the deadline the ` +
          'orchestrator enforces itself, above the template render limit — the template is doing ' +
          'something unbounded, or an object-store read is hanging.',
      });
    }, deadline);
    timer.unref?.();
    worker.on('message', (response: RenderResponse) => finish(response));
    worker.on('error', (err: Error) => finish({ ok: false, error: `the render thread failed: ${err.message}` }));
    worker.on('exit', (code: number) => {
      // An exit before a message is a crash — an OOM kill, a terminate from elsewhere. Named, because
      // "the thread went away" and "the template was wrong" need different fixes.
      if (code !== 0) finish({ ok: false, error: `the render thread exited with code ${code}` });
      else finish({ ok: false, error: 'the render thread exited without producing a report' });
    });
  });
}

export type { RenderRequest, RenderResponse } from './renderWorker';
