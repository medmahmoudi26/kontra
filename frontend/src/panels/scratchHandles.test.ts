import { describe, expect, it } from 'vitest';
import type { ScratchNode } from '../run/api';
import { fieldHandle, handleField, handleKind, inHandle, outHandle } from './scratchHandles';

/**
 * The ids the two ends of an edge are named by.
 *
 * THEY ARE BUILT IN ONE PLACE AND READ IN THREE: the node draws the handle, the projection names the
 * handle a stored edge lands on, and a connection is read back into the two field names the document
 * keeps. React Flow drops an edge whose named handle does not exist — error 008, no line — so a
 * second spelling anywhere is an edge that silently is not there, and these are the assertions that
 * pin the spelling.
 *
 * WHAT IS NOT HERE ANY MORE is `handleIsNatural` and the sentences about a Dataset wired the wrong
 * way round. They judged an author-declared `direction` against the edges — and `direction` is now
 * DERIVED from those same edges (`scratchFlow.ts:withDerivedDirections`), so the rule could only
 * ever fire against a Dataset that was both written and read, which is an ordinary drawing rather
 * than a mistake. What is odd about an edge is a question about its TYPES now, and it is answered in
 * `scratchPorts.test.ts`.
 */

const actor: ScratchNode = {
  id: 'a',
  kind: 'actor',
  at: { x: 0, y: 0 },
  actor: 'nscheck',
  version: '0.1.0',
  method: 'ask',
};
const workflow: ScratchNode = { id: 'w', kind: 'workflow', at: { x: 0, y: 0 }, file: 'sweep.py' };
const dataset: ScratchNode = {
  id: 'd',
  kind: 'dataset',
  at: { x: 0, y: 0 },
  name: 'domains',
  direction: 'in',
};

describe('the whole-node handle', () => {
  it('says which of the three kinds the node is, and a Dataset is no longer two of them', () => {
    // It used to split into `dataset-read` and `dataset-write`, because the author declared a
    // direction and it decided which of the node's handles meant anything. A Dataset one step writes
    // and the next reads is one node with both sides in use.
    expect(handleKind(actor)).toBe('actor');
    expect(handleKind(workflow)).toBe('workflow');
    expect(handleKind(dataset)).toBe('dataset');
    expect(handleKind({ ...dataset, direction: 'out' } as ScratchNode)).toBe('dataset');
  });

  it('encodes the side and the kind into an id both the node and the projection build', () => {
    expect(inHandle(handleKind(dataset))).toBe('in:dataset');
    expect(outHandle(handleKind(actor))).toBe('out:actor');
  });
});

describe('a handle for one field', () => {
  it('carries the field name, on the side it is on', () => {
    expect(fieldHandle('in', 'url')).toBe('in:field:url');
    expect(fieldHandle('out', 'body')).toBe('out:field:body');
  });

  it('reads the field back out of a connection', () => {
    // What `onConnect` is handed is two handle ids; what the document stores is two FIELD names.
    expect(handleField(fieldHandle('out', 'body'))).toBe('body');
    expect(handleField(fieldHandle('in', 'url'))).toBe('url');
  });

  it('answers nothing for a whole-node handle, which is what an unported edge means', () => {
    expect(handleField(inHandle('actor'))).toBeUndefined();
    expect(handleField(outHandle('dataset'))).toBeUndefined();
    // React Flow reports `null` for a connection made without handle ids at all.
    expect(handleField(null)).toBeUndefined();
    expect(handleField(undefined)).toBeUndefined();
  });

  it('keeps a field name that contains a colon whole', () => {
    // A JSON Schema property may be called anything; splitting on `:` would hand back `a` for
    // `a:b`, which is a field the node does not have and an edge that lands nowhere.
    expect(handleField(fieldHandle('in', 'a:b'))).toBe('a:b');
  });
});
