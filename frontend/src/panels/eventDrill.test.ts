import { describe, expect, it } from 'vitest';

import type { EventLink, RunEvent } from '../run/api';
import {
  drillInto,
  historyClosed,
  isNavigable,
  linkLabel,
  popTo,
  rootLevel,
  shortId,
  spanMs,
} from './eventDrill';

function ev(over: Partial<RunEvent> = {}): RunEvent {
  return {
    id: 1,
    type: 'ActivityTaskScheduled',
    cat: 'activity',
    t: 0,
    at: 0,
    detail: '',
    attempt: 1,
    dur: 0,
    ...over,
  };
}

/** The two links off the recorded run `nscheck-1786831339` — see `backend/src/history.test.ts`
 *  for the raw events they are read from. */
const FLEET_UP: EventLink = {
  workflowId: 'kontra-fleet/dns',
  execId: '01a00772-8296-7c0e-ae18-7aaca6721266',
  type: 'stackWorkflow',
  via: 'child',
};
const FLEET_DOWN: EventLink = { ...FLEET_UP, execId: '01a00776-89dc-75da-a0a2-97d86a09a2a3' };
const DISPATCH: EventLink = {
  workflowId: 'actor-nscheck-nscheck-1786831339-nscheck-60c3bfac',
  via: 'nexus',
};

describe('what is navigable', () => {
  // The criterion, exactly: rows the server linked, and no others. Nothing here re-derives the rule
  // from a type name — the server read an id off the attributes or it did not.
  it('is a row that names a workflow, and never one that does not', () => {
    expect(isNavigable(ev({ link: FLEET_UP }))).toBe(true);
    expect(isNavigable(ev({ type: 'ActivityTaskCompleted' }))).toBe(false);
  });
});

describe('descending', () => {
  const root = [rootLevel('nscheck-1786831339')];

  it('pushes the workflow the row names, pinned to the execution the row was about', () => {
    const next = drillInto(root, ev({ id: 12, link: FLEET_UP }));
    expect(next).toHaveLength(2);
    expect(next[1]).toEqual({
      workflowId: 'kontra-fleet/dns',
      execId: '01a00772-8296-7c0e-ae18-7aaca6721266',
      type: 'stackWorkflow',
      from: 12,
    });
  });

  it('leaves the stack alone for a row that names nothing', () => {
    expect(drillInto(root, ev({ id: 7 }))).toEqual(root);
  });

  it('goes two deep — a dispatch made inside a fleet-scoped child', () => {
    const deep = drillInto(drillInto(root, ev({ id: 12, link: FLEET_UP })), ev({ id: 38, link: DISPATCH }));
    expect(deep.map((l) => l.workflowId)).toEqual([
      'nscheck-1786831339',
      'kontra-fleet/dns',
      'actor-nscheck-nscheck-1786831339-nscheck-60c3bfac',
    ]);
  });

  // The child's start and its completion are two rows naming ONE destination. Clicking the second
  // while already inside it must not stack a second identical crumb.
  it('returns to a level already open rather than opening it twice', () => {
    const inside = drillInto(root, ev({ id: 12, link: FLEET_UP }));
    const again = drillInto(drillInto(inside, ev({ id: 38, link: DISPATCH })), ev({ id: 16, link: FLEET_UP }));
    expect(again).toEqual(inside);
  });

  // Both fleet children of that run are `kontra-fleet/dns`. They are two different executions and
  // must be two different destinations, or drilling into the bring-up shows the teardown.
  it('treats two executions of one workflow id as different places', () => {
    const up = drillInto(root, ev({ id: 12, link: FLEET_UP }));
    const down = drillInto(up, ev({ id: 297, link: FLEET_DOWN }));
    expect(down).toHaveLength(3);
    expect(down[2]!.execId).toBe('01a00776-89dc-75da-a0a2-97d86a09a2a3');
  });
});

describe('the breadcrumb', () => {
  const stack = [
    rootLevel('nscheck-1786831339'),
    { workflowId: 'kontra-fleet/dns', execId: 'a', from: 12 },
    { workflowId: 'actor-nscheck-1', from: 38 },
  ];

  // A dispatch inside a fleet-scoped child is two levels down and must be escapable to EITHER.
  it('returns to any level of the chain, dropping only what is below it', () => {
    expect(popTo(stack, 1).map((l) => l.workflowId)).toEqual([
      'nscheck-1786831339',
      'kontra-fleet/dns',
    ]);
    expect(popTo(stack, 0)).toEqual([stack[0]]);
    expect(popTo(stack, 2)).toEqual(stack);
  });

  it('clamps a stale click instead of throwing — the stack may have shortened under it', () => {
    expect(popTo(stack, 9)).toEqual(stack);
    expect(popTo(stack, -3)).toEqual([stack[0]]);
  });

  it('does not alias the stack it was given', () => {
    const out = popTo(stack, 2);
    out.pop();
    expect(stack).toHaveLength(3);
  });
});

describe('labels', () => {
  // Twelve dispatches of one run differ only in their last eight characters; a head-truncated id
  // would print one string twelve times.
  it('keeps the discriminating TAIL of a long id, cut on a separator', () => {
    expect(shortId('actor-nscheck-nscheck-1786831339-nscheck-60c3bfac')).toBe('…nscheck-60c3bfac');
    expect(shortId('kontra-fleet/dns')).toBe('kontra-fleet/dns');
  });

  it('keeps the whole tail when there is no separator to cut on', () => {
    expect(shortId(`x${'9'.repeat(40)}`)).toBe(`…${'9'.repeat(26)}`);
  });

  it('names the workflow type when the event gave one, and the id tail when it did not', () => {
    expect(linkLabel(FLEET_UP)).toBe('stackWorkflow');
    expect(linkLabel(DISPATCH)).toBe('…nscheck-60c3bfac');
  });
});

describe('when a level stops', () => {
  const closed = [ev({ id: 1, type: 'WorkflowExecutionStarted', at: 1_000 }), ev({ id: 11, type: 'WorkflowExecutionCompleted', at: 157_310 })];

  it('is closed once the history ends in a workflow close', () => {
    expect(historyClosed(closed)).toBe(true);
    expect(historyClosed([ev({ type: 'ActivityTaskStarted' })])).toBe(false);
    expect(historyClosed([])).toBe(false);
  });

  it('counts a failure and a termination as closed too — they are ends, not faults to wait on', () => {
    expect(historyClosed([ev({ type: 'WorkflowExecutionFailed' })])).toBe(true);
    expect(historyClosed([ev({ type: 'WorkflowExecutionTerminated' })])).toBe(true);
  });

  // The workflow id is still going under a new execution, and the same route answers with it.
  it('does not call a continue-as-new closed', () => {
    expect(historyClosed([ev({ type: 'WorkflowExecutionContinuedAsNew' })])).toBe(false);
  });

  it('measures a level from its first event to its last', () => {
    expect(spanMs(closed)).toBe(156_310); // the fleet child's 156 seconds
    expect(spanMs([])).toBe(0);
    expect(spanMs([ev({ at: 0 })])).toBe(0);
  });
});
