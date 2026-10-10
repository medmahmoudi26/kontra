/**
 * COMPLETION AWAITERS — a run the console started gets its stored report within seconds of closing,
 * not at the next sweep (PRD D6: "the completion poll is replaced by completion awaiters").
 *
 * The sweep renders every closed run on a clock, which is right for a run nobody is waiting on and
 * for healing a template edit, and slow for the run somebody just started from the console and is
 * about to open: its report appears up to a minute after the run ends. A live session already
 * finalises the run it watches the moment it closes; this does the same for every run started
 * through `POST /api/runs`, watched or not.
 *
 * BOUNDED, BECAUSE EACH ONE IS A LONG POLL. An awaiter holds one history long-poll open for the
 * life of its run; past {@link DEFAULT_CAP} of them a new run is left to the sweep, which still
 * renders it — later, not never. The count is what the cap is for, so nothing is queued.
 */

export const DEFAULT_CAP = 64;

export class CompletionAwaiters {
  private readonly active = new Set<string>();

  constructor(
    /** Wait for the run to close, then render and store its final report. */
    private readonly finalize: (runId: string, namespace: string) => Promise<void>,
    private readonly cap = DEFAULT_CAP,
    private readonly onError: (err: unknown, runId: string) => void = () => undefined
  ) {}

  /** Await this run's completion. False when it was left to the sweep (at the cap, or already awaited). */
  track(runId: string, namespace: string): boolean {
    const key = `${namespace}/${runId}`;
    if (this.active.has(key) || this.active.size >= this.cap) return false;
    this.active.add(key);
    void this.finalize(runId, namespace)
      .catch((err: unknown) => this.onError(err, runId))
      .finally(() => this.active.delete(key));
    return true;
  }

  get size(): number {
    return this.active.size;
  }
}
