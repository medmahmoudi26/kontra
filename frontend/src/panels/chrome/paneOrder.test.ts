/**
 * What makes a persisted stack order survive the stack changing.
 *
 * THE ORDER IS BY KEY, NEVER BY INDEX, and the reconcile test below is the whole reason. A stored
 * `[2,0,1]` permutes whatever three panes happen to be there next release — which is a layout that
 * silently rearranges panes an operator never touched, and worse than storing nothing.
 */

import { describe, expect, it } from 'vitest';
import {
  EMPTY_ORDER,
  moveBefore,
  nudge,
  paneOrderKey,
  parsePaneOrder,
  reconcileOrder,
  toggleFolded,
} from './paneOrder';

describe('reconciling a stored order against the panes that exist', () => {
  it('keeps the operator’s order', () => {
    expect(reconcileOrder(['log', 'editor', 'worker'], ['editor', 'worker', 'log'])).toEqual([
      'log',
      'editor',
      'worker',
    ]);
  });

  it('drops a pane that no longer exists', () => {
    expect(reconcileOrder(['gone', 'editor'], ['editor', 'worker'])).toEqual(['editor', 'worker']);
  });

  it('appends a pane that is new, at the bottom', () => {
    // Where a reader looks for something they have not seen before — and never silently first,
    // which would move the pane they were using.
    expect(reconcileOrder(['editor'], ['editor', 'worker'])).toEqual(['editor', 'worker']);
  });

  it('survives a stored order with a key twice', () => {
    expect(reconcileOrder(['editor', 'editor'], ['editor', 'worker'])).toEqual(['editor', 'worker']);
  });

  it('is the source order when nothing is stored', () => {
    expect(reconcileOrder([], ['editor', 'worker', 'log'])).toEqual(['editor', 'worker', 'log']);
  });
});

describe('moving a pane', () => {
  const ORDER = ['editor', 'worker', 'log'];

  it('drops it above the pane it was dropped on', () => {
    expect(moveBefore(ORDER, 'log', 'editor')).toEqual(['log', 'editor', 'worker']);
    expect(moveBefore(ORDER, 'editor', 'log')).toEqual(['worker', 'editor', 'log']);
  });

  it('is unchanged for a drag that ended nowhere', () => {
    // An ordinary gesture, not an error: a pane dropped on itself, or on something that is not in
    // this stack at all.
    expect(moveBefore(ORDER, 'log', 'log')).toEqual(ORDER);
    expect(moveBefore(ORDER, 'log', 'elsewhere')).toEqual(ORDER);
    expect(moveBefore(ORDER, 'ghost', 'log')).toEqual(ORDER);
  });

  it('never mutates the list it was given', () => {
    const before = [...ORDER];
    moveBefore(ORDER, 'log', 'editor');
    expect(ORDER).toEqual(before);
  });
});

describe('nudging with the keyboard', () => {
  const ORDER = ['editor', 'worker', 'log'];

  it('moves one place, in the direction pressed', () => {
    expect(nudge(ORDER, 'worker', -1)).toEqual(['worker', 'editor', 'log']);
    expect(nudge(ORDER, 'worker', 1)).toEqual(['editor', 'log', 'worker']);
  });

  it('stops at the ends rather than wrapping', () => {
    // A pane that jumped from the top to the bottom on one more press is a stack nobody can aim.
    expect(nudge(ORDER, 'editor', -1)).toEqual(ORDER);
    expect(nudge(ORDER, 'log', 1)).toEqual(ORDER);
  });
});

describe('folding', () => {
  it('is a toggle, and is not a move', () => {
    expect(toggleFolded([], 'worker')).toEqual(['worker']);
    expect(toggleFolded(['worker'], 'worker')).toEqual([]);
  });
});

describe('reading a stored stack', () => {
  it('is empty when nothing is stored', () => {
    expect(parsePaneOrder(null)).toEqual(EMPTY_ORDER);
  });

  it('reads order and folds back', () => {
    const raw = JSON.stringify({ order: ['log', 'editor'], folded: ['editor'] });
    expect(parsePaneOrder(raw)).toEqual({ order: ['log', 'editor'], folded: ['editor'] });
  });

  it('costs the source order and not the page on anything unreadable', () => {
    expect(parsePaneOrder('{oh no')).toEqual(EMPTY_ORDER);
    expect(parsePaneOrder('null')).toEqual(EMPTY_ORDER);
    expect(parsePaneOrder(JSON.stringify({ order: 'editor' }))).toEqual(EMPTY_ORDER);
  });

  it('drops non-string entries rather than putting them in the order', () => {
    // A stored index from an older build is exactly the value this rejects — reconcile would drop
    // it anyway, but a number in a list of keys is a corrupt layout and should not survive parsing.
    expect(parsePaneOrder(JSON.stringify({ order: ['editor', 2, null] }))).toEqual({
      order: ['editor'],
      folded: [],
    });
  });
});

describe('paneOrderKey', () => {
  it('namespaces per stack', () => {
    expect(paneOrderKey('workflows')).not.toBe(paneOrderKey('actors'));
  });
});
