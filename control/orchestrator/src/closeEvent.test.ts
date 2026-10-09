import { describe, expect, it, vi } from 'vitest';

import { closeEventOf, type CloseEventService } from './temporalClient';

/**
 * `closeEventOf` NEVER ASKS AN OPEN RUN FOR ITS CLOSE EVENT.
 *
 * Temporal long-polls that request until its expiry, and `waitNewEvent: false` does not stop it:
 * 20,006 ms against an open run on a live install, against 30 ms for a first-event read. A live
 * report's first render made it twice before writing a byte, so the browser showed "No report yet"
 * for 41 s of a 54 s run. The fake below hangs exactly the way Temporal does, so a regression is a
 * timeout here rather than a slow page somewhere else.
 */

const RUNNING = 1;
const COMPLETED = 2;
const FAILED = 3;

const completedEvent = { eventType: 2, workflowExecutionCompletedEventAttributes: { result: {} } };

function fake(status: number | 'throws'): CloseEventService & {
  describeWorkflowExecution: ReturnType<typeof vi.fn>;
  getWorkflowExecutionHistory: ReturnType<typeof vi.fn>;
} {
  return {
    describeWorkflowExecution: vi.fn(async () => {
      if (status === 'throws') throw new Error('UNAVAILABLE');
      return { workflowExecutionInfo: { status } };
    }),
    getWorkflowExecutionHistory: vi.fn(
      async (req: { historyEventFilterType: number }) => {
        // What Temporal does with the close-event filter on an open run: hold the request.
        if (req.historyEventFilterType === 2 && status === RUNNING) return new Promise(() => {});
        return { history: { events: [{ eventType: 1 }, completedEvent] } };
      }
    ),
  } as never;
}

const execution = { workflowId: 'canary-1791565531' };

describe('closeEventOf', () => {
  it('answers undefined for a RUNNING run at once, and never sends the close-event read', async () => {
    const svc = fake(RUNNING);
    await expect(closeEventOf(svc, 'default', execution)).resolves.toBeUndefined();
    expect(svc.getWorkflowExecutionHistory).not.toHaveBeenCalled();
  });

  it('reads the close event of a finished run, with the close-event filter and no wait', async () => {
    const svc = fake(COMPLETED);
    await expect(closeEventOf(svc, 'default', execution)).resolves.toEqual(completedEvent);
    expect(svc.getWorkflowExecutionHistory).toHaveBeenCalledWith({
      namespace: 'default',
      execution,
      historyEventFilterType: 2,
      waitNewEvent: false,
    });
  });

  it('reads it for a run that ended any other way too', async () => {
    const svc = fake(FAILED);
    await closeEventOf(svc, 'default', { ...execution, runId: 'exec-1' });
    expect(svc.getWorkflowExecutionHistory).toHaveBeenCalledOnce();
    expect(svc.describeWorkflowExecution).toHaveBeenCalledWith({
      namespace: 'default',
      execution: { ...execution, runId: 'exec-1' },
    });
  });

  it('falls back to the plain read when the describe fails, rather than guessing either way', async () => {
    const svc = fake('throws');
    await expect(closeEventOf(svc, 'default', execution)).resolves.toEqual(completedEvent);
    expect(svc.getWorkflowExecutionHistory).toHaveBeenCalledOnce();
  });
});
