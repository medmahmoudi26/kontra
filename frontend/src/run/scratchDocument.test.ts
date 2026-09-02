import { describe, expect, it } from 'vitest';
import type { ScratchDocument, ScratchEdge, ScratchNode, ScratchNote } from './api';

/**
 * The Scratch document, restated for the browser — pinned against what the server keeps.
 *
 * THE SHAPE IS DEFINED ONCE, on the server (`backend/src/scratch.ts`), and this is the second
 * spelling of it. The repo's convention is that every cross-boundary literal is written
 * independently and held to one answer by a test rather than shared through a package, and the
 * reason bites harder here than usual: the whole feature rests on the editor NOT being the only
 * thing that knows what a drawing meant. If these two drifted, the canvas would keep drawing a node
 * the server had already dropped — and the agent would be handed a spec missing the piece the
 * author was looking at.
 *
 * These are type-level assertions written as values: a field renamed on either side stops this file
 * compiling, which is the point. They also pin the two distinctions that are easy to flatten and
 * expensive to get wrong.
 */

describe('a node is a REAL thing', () => {
  it('names an Actor by catalog key AND the Method it dispatches', () => {
    // Not a rectangle with a label. `nscheck@0.1.0` is the catalog's own key and `delegation` is a
    // Method it declares, so an agent reading this has nothing to guess — which actor, which
    // version, whether the second word is a Method or a note.
    const node: ScratchNode = {
      id: 'a1',
      kind: 'actor',
      at: { x: 0, y: 0 },
      actor: 'nscheck',
      version: '0.1.0',
      method: 'delegation',
    };
    expect(node.kind).toBe('actor');
  });

  it('gives a Dataset a DIRECTION, which the canvas derives and the spec reads', () => {
    // Reading and writing are the same call up to `.writer()`, and counting both as output once
    // announced a thousand committed rows one second after a run started — most of them an input
    // list it had not read yet. The field stays for that reason; what changed is that the drawing
    // answers it (`scratchFlow.ts:withDerivedDirections`) rather than a button in the palette.
    const read: ScratchNode = { id: 'd1', kind: 'dataset', at: { x: 0, y: 0 }, name: 'domains', direction: 'in' };
    const written: ScratchNode = { id: 'd2', kind: 'dataset', at: { x: 0, y: 0 }, name: 'lame', direction: 'out' };
    expect([read, written].map((n) => (n.kind === 'dataset' ? n.direction : ''))).toEqual(['in', 'out']);
  });

  it('names a Workflow by the file that defines it', () => {
    const node: ScratchNode = { id: 'w1', kind: 'workflow', at: { x: 0, y: 0 }, file: 'nscheck.py' };
    expect(node.kind).toBe('workflow');
  });
});

describe('what carries the parts a schema cannot', () => {
  it('lets an edge be labelled, and lets it not be', () => {
    // "only the ones that answered" is a fact about the flow that no type on either end holds.
    const labelled: ScratchEdge = { id: 'e1', from: 'a', to: 'b', label: 'only the ones that answered' };
    const plain: ScratchEdge = { id: 'e2', from: 'b', to: 'c' };
    expect(labelled.label).toBeTruthy();
    expect(plain.label).toBeUndefined();
  });

  it('lets an edge name the FIELD at each end, and lets it name neither', () => {
    // `fetch.body → title.html` says which value travels; `fetch → title` leaves an agent to work
    // it out from two schemas with three candidate fields between them. Both are legitimate: no
    // ports is what every edge drawn before this meant, and what a Method declaring neither
    // `takes=` nor `emits=` can offer.
    const typed: ScratchEdge = { id: 'e1', from: 'a', to: 'b', fromPort: 'body', toPort: 'html' };
    const whole: ScratchEdge = { id: 'e2', from: 'a', to: 'b' };
    expect([typed.fromPort, typed.toPort]).toEqual(['body', 'html']);
    expect([whole.fromPort, whole.toPort]).toEqual([undefined, undefined]);
  });

  it('lets a note float or be pinned to a node', () => {
    // These are what a person knows and a schema does not — "page 200 at a time", "isolate, do not
    // fail the run" — and they are what make generated code right rather than merely valid.
    const loose: ScratchNote = { id: 'n1', at: { x: 0, y: 0 }, text: 'page 200 at a time' };
    const pinned: ScratchNote = { id: 'n2', at: { x: 0, y: 0 }, text: 'about a 4x fan-out', on: 'a1' };
    expect(loose.on).toBeUndefined();
    expect(pinned.on).toBe('a1');
  });
});

describe('a document is three lists and nothing else', () => {
  it('has no geometry beyond a point per node', () => {
    // Position is DECORATION: the graph means the same thing at any coordinates, which is why the
    // spec an agent reads drops it entirely. A document that carried sizes, z-order or a viewport
    // would be a document where moving a box could change what the drawing said.
    const doc: ScratchDocument = { nodes: [], edges: [], notes: [] };
    expect(Object.keys(doc).sort()).toEqual(['edges', 'nodes', 'notes']);
  });
});
