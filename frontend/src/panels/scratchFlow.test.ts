import { describe, expect, it } from 'vitest';
// The SERVER's own parse and rendering, not a restatement of them. `renderScratch` is the single
// answer to "what did the drawing say" (ADR 0026), so the round trip is only worth testing against
// the real one.
import { parseScratchDocument, renderScratch, resolveScratch } from '@core/scratch';
import type { ScratchDocument } from '../run/api';
import {
  isNoteNode,
  nodeSlot,
  normaliseScratchDocument,
  toFlowEdges,
  toFlowNodes,
  withDerivedDirections,
  withEdge,
  withEdgeLabel,
  withMethod,
  withPositions,
  withoutEdges,
  withoutNode,
  SCRATCH_NODE_TYPES,
  type ScratchFlowEdge,
  type ScratchFlowNode,
} from './scratchFlow';

/**
 * The seam between the stored drawing and the canvas library.
 *
 * TWO PROPERTIES ARE WORTH TESTS AND THE REST IS PLUMBING.
 *
 * The first is that the DOCUMENT DOES NOT MOVE. `renderScratch` on the server turns it into the
 * spec an agent writes code from, and ADR 0026 makes that one rendering the reason the surface
 * exists — so a Scratch that goes through this canvas and back out has to be the same bytes it
 * arrived as. Not "the same information": the same bytes, because a reordered key is a diff on
 * every save that moved nothing, and a renamed field is a spec that silently loses a node.
 *
 * The second is that a BAD POSITION DOES NOT BLANK THE PAGE. React Flow reads `node.position.x`
 * while it lays out, and a throw in there takes the whole surface down rather than the one node —
 * white page, stack in the console, no other clue. The server's parse does not catch it: `NaN` is
 * a `typeof v === 'number'`, so `{x: NaN}` is stored and read back and turned into a transform
 * nothing can be seen through.
 */

/**
 * The Scratch this installation actually has saved, copied out of the store verbatim.
 *
 * A HAND-WRITTEN FIXTURE WOULD NOT HAVE CAUGHT THE THING THIS IS FOR: labelled and unlabelled
 * edges together, a pinned note and a loose one, and the exact key order `parseScratchDocument`
 * writes. That is what makes the byte-for-byte assertion below mean something.
 */
const SAVED = `{"nodes":[{"id":"d1","kind":"dataset","at":{"x":40,"y":120},"name":"domains","direction":"in"},{"id":"a1","kind":"actor","at":{"x":260,"y":120},"actor":"nscheck","version":"0.1.0","method":"delegation"},{"id":"a2","kind":"actor","at":{"x":480,"y":120},"actor":"nscheck","version":"0.1.0","method":"ask"},{"id":"a3","kind":"actor","at":{"x":480,"y":300},"actor":"probe","version":"0.1.0","method":"head"},{"id":"d2","kind":"dataset","at":{"x":700,"y":120},"name":"lame","direction":"out"}],"edges":[{"id":"e1","from":"d1","to":"a1","label":"a page at a time"},{"id":"e2","from":"a1","to":"a2"},{"id":"e3","from":"a2","to":"d2"},{"id":"e4","from":"a2","to":"a3","label":"only the ones that answered"}],"notes":[{"id":"n1","at":{"x":40,"y":420},"text":"page 200 at a time — 400 units through one activity is past the measured guard"},{"id":"n2","at":{"x":260,"y":220},"text":"this fan-out is about 4x: one domain becomes one unit per nameserver","on":"a1"}]}`;

const saved = (): unknown => JSON.parse(SAVED) as unknown;

describe('the saved sketch survives the canvas', () => {
  it('re-serialises byte for byte after a load', () => {
    // What the server stores is `JSON.stringify` of these objects. Opening `lame nameserver sweep`
    // and saving it again without touching it must produce the same string, or every open is a
    // diff and nobody can tell an edit from a round trip.
    expect(JSON.stringify(normaliseScratchDocument(saved()))).toBe(SAVED);
  });

  it('still re-serialises byte for byte after a load, a projection and a save', () => {
    const doc = normaliseScratchDocument(saved());
    const flow = toFlowNodes(doc);
    const back = withPositions(
      doc,
      flow.map((n) => ({ id: n.id, position: n.position }))
    );
    expect(JSON.stringify(back)).toBe(SAVED);
  });

  it('gives every node and every note a React Flow node', () => {
    const doc = normaliseScratchDocument(saved());
    const flow = toFlowNodes(doc);
    expect(flow.map((n) => n.id)).toEqual(['d1', 'a1', 'a2', 'a3', 'd2', 'n1', 'n2']);
  });

  it('carries an edge label onto the line and leaves the unlabelled ones bare', () => {
    const doc = normaliseScratchDocument(saved());
    expect(toFlowEdges(doc)).toEqual([
      {
        id: 'e1',
        source: 'd1',
        target: 'a1',
        sourceHandle: 'out:dataset',
        targetHandle: 'in:actor',
        label: 'a page at a time',
      },
      { id: 'e2', source: 'a1', target: 'a2', sourceHandle: 'out:actor', targetHandle: 'in:actor' },
      {
        id: 'e3',
        source: 'a2',
        target: 'd2',
        sourceHandle: 'out:actor',
        targetHandle: 'in:dataset',
      },
      {
        id: 'e4',
        source: 'a2',
        target: 'a3',
        sourceHandle: 'out:actor',
        targetHandle: 'in:actor',
        label: 'only the ones that answered',
      },
    ]);
  });

  it('lands an edge that names no field on the WHOLE-NODE handle of its kind', () => {
    // Every edge in this sketch was drawn before typed ports, so every one of them means the whole
    // node — which is what keeps an existing document loading with no migration. The node
    // components draw their handles with the same functions this projects with: left unnamed, React
    // Flow attaches to whichever handle of that type it finds first, which is right today and
    // silent the day it stops being.
    const doc = normaliseScratchDocument(saved());
    const handles = toFlowEdges(doc).map((e) => `${String(e.sourceHandle)} → ${String(e.targetHandle)}`);
    expect(handles).toEqual([
      'out:dataset → in:actor',
      'out:actor → in:actor',
      'out:actor → in:dataset',
      'out:actor → in:actor',
    ]);
  });

  it('lands a ported edge on the handle of the FIELD it names', () => {
    const doc = normaliseScratchDocument({
      ...(saved() as object),
      edges: [{ id: 'e1', from: 'a1', to: 'a2', fromPort: 'body', toPort: 'html' }],
    });
    const [edge] = toFlowEdges(doc);
    expect(edge?.sourceHandle).toBe('out:field:body');
    expect(edge?.targetHandle).toBe('in:field:html');
  });

  it('leaves every edge of a sketch that lines up unmarked', () => {
    const doc = normaliseScratchDocument(saved());
    expect(toFlowEdges(doc).map((e) => e.className)).toEqual([undefined, undefined, undefined, undefined]);
  });
});

describe('a node type per kind', () => {
  it('types each node by what it IS, and a note as a note', () => {
    // The vocabulary the drawing is in. A canvas that drew all four as one rectangle made telling
    // a Dataset from an Actor a matter of reading the text on it.
    const doc = normaliseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at: { x: 0, y: 0 }, actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'w', kind: 'workflow', at: { x: 0, y: 0 }, file: 'sweep.py' },
        { id: 'd', kind: 'dataset', at: { x: 0, y: 0 }, name: 'domains', direction: 'in' },
      ],
      edges: [],
      notes: [{ id: 'n', at: { x: 0, y: 0 }, text: 'page 200 at a time' }],
    });
    expect(toFlowNodes(doc).map((n) => n.type)).toEqual(['actor', 'workflow', 'dataset', 'note']);
  });

  it('emits nothing outside the registered set', () => {
    // `scratchNodeTypes` is keyed by this list. A type it emits that has no component registered
    // falls back to React Flow's default node — an empty rectangle with no label, draggable and
    // savable, saying nothing about what it is.
    const doc = normaliseScratchDocument(saved());
    for (const node of toFlowNodes(doc)) {
      expect(SCRATCH_NODE_TYPES).toContain(node.type);
    }
  });

  it('puts the document node in `data`, not a copy of it', () => {
    // The projection is a view, not a second document. If `data` held a copy, an edit would have
    // two places to land and the canvas would eventually draw something the spec did not say.
    const doc = normaliseScratchDocument(saved());
    const [first] = toFlowNodes(doc);
    expect(first && !isNoteNode(first) && first.data.node).toBe(doc.nodes[0]);
  });
});

describe('a document that cannot be laid out still opens', () => {
  it('places a node that has no position at all', () => {
    // This is the input that threw INSIDE React Flow and blanked the whole surface, not just the
    // node: it reads `node.position.x` while laying out.
    const doc = normaliseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'b', kind: 'workflow', file: 'sweep.py' },
      ],
      edges: [],
      notes: [],
    });
    expect(doc.nodes.map((n) => n.at)).toEqual([nodeSlot(0), nodeSlot(1)]);
    expect(toFlowNodes(doc).map((n) => n.position)).toEqual([nodeSlot(0), nodeSlot(1)]);
  });

  it('places a node whose position is NaN, which the server happily stores', () => {
    // `isPoint` in `src/scratch.ts` asks `typeof v === 'number'`, and NaN answers yes. The node
    // travels all the way here and becomes `translate(NaN, 0)` — a box nobody can find, with its
    // edges attached to nothing.
    const doc = normaliseScratchDocument({
      nodes: [{ id: 'a', kind: 'actor', at: { x: NaN, y: 12 }, actor: 'nscheck', version: '0.1.0', method: 'ask' }],
      edges: [],
      notes: [],
    });
    expect(doc.nodes[0]?.at).toEqual(nodeSlot(0));
  });

  it('places a node whose position is a string, and one whose position is null', () => {
    const doc = normaliseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at: { x: '40', y: 120 }, actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'b', kind: 'dataset', at: null, name: 'domains', direction: 'out' },
      ],
      edges: [],
      notes: [],
    });
    expect(doc.nodes.map((n) => n.at)).toEqual([nodeSlot(0), nodeSlot(1)]);
  });

  it('opens a document with no lists in it at all', () => {
    // A saved graph from before the three lists existed has no `nodes` key, and `undefined.map` is
    // the same blank page by a different route.
    expect(normaliseScratchDocument({})).toEqual({ nodes: [], edges: [], notes: [] });
    expect(normaliseScratchDocument(null)).toEqual({ nodes: [], edges: [], notes: [] });
    expect(normaliseScratchDocument('not a document')).toEqual({ nodes: [], edges: [], notes: [] });
    expect(normaliseScratchDocument({ nodes: 'no', edges: 7, notes: null })).toEqual({
      nodes: [],
      edges: [],
      notes: [],
    });
  });

  it('drops a node of a kind this build has no meaning for, and the edges that named it', () => {
    // React Flow's answer to an unregistered type is its default node: an empty rectangle the
    // author can drag and save. The server drops the same node on the way in.
    const doc = normaliseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at: { x: 0, y: 0 }, actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'x', kind: 'incantation', at: { x: 0, y: 0 } },
      ],
      edges: [{ id: 'e', from: 'a', to: 'x' }],
      notes: [],
    });
    expect(doc.nodes.map((n) => n.id)).toEqual(['a']);
    expect(doc.edges).toEqual([]);
  });

  it('keeps a note whose node is gone, and stops it claiming the pin', () => {
    // The note carries the one thing no schema holds. Losing it to a deleted node would lose the
    // paging rule; keeping the pin would leave the canvas and the stored document two different
    // drawings, because the server strips a pin it cannot resolve.
    const doc = normaliseScratchDocument({
      nodes: [],
      edges: [],
      notes: [{ id: 'n', at: { x: 1, y: 2 }, text: 'page 200 at a time', on: 'gone' }],
    });
    expect(doc.notes).toEqual([{ id: 'n', at: { x: 1, y: 2 }, text: 'page 200 at a time' }]);
  });
});

describe('dragging, and what reaches the document', () => {
  const doc = (): ScratchDocument => normaliseScratchDocument(saved());

  it('writes a dragged position back onto the node it belongs to', () => {
    const moved = withPositions(doc(), [{ id: 'a1', position: { x: 11, y: 22 } }]);
    expect(moved.nodes.find((n) => n.id === 'a1')?.at).toEqual({ x: 11, y: 22 });
    expect(moved.nodes.find((n) => n.id === 'a2')?.at).toEqual({ x: 480, y: 120 });
  });

  it('drags a note by the same route as a node', () => {
    const moved = withPositions(doc(), [{ id: 'n1', position: { x: 5, y: 6 } }]);
    expect(moved.notes.find((n) => n.id === 'n1')?.at).toEqual({ x: 5, y: 6 });
  });

  it('changes NOTHING ELSE about a node it moves', () => {
    // The keys keep their order, so what is stored differs in the two numbers that moved and
    // nowhere else. A rebuilt node would be the same drawing and a different document.
    const before = doc();
    const after = withPositions(before, [{ id: 'a1', position: { x: 11, y: 22 } }]);
    expect(JSON.stringify(after)).toBe(SAVED.replace('{"x":260,"y":120}', '{"x":11,"y":22}'));
  });

  it('returns the document ITSELF when the drag ended where it started', () => {
    // A click that selects reports a position too. Treating that as an edit puts a permanent
    // unsaved • beside the name of a sketch nobody changed.
    const before = doc();
    expect(withPositions(before, [{ id: 'a1', position: { x: 260, y: 120 } }])).toBe(before);
    expect(withPositions(before, [])).toBe(before);
  });

  it('ignores a move for something that is not in the document', () => {
    const before = doc();
    expect(withPositions(before, [{ id: 'ghost', position: { x: 1, y: 1 } }])).toBe(before);
  });
});

describe('the projection keeps what React Flow measured', () => {
  it('hands back the same array when nothing changed', () => {
    // The page syncs the document into the canvas with `setFlowNodes(cur => toFlowNodes(doc, cur))`
    // — returning the same array is what makes React bail out instead of re-rendering the canvas
    // for a change that touched only an edge label.
    const doc = normaliseScratchDocument(saved());
    const first = toFlowNodes(doc);
    expect(toFlowNodes(doc, first)).toBe(first);
  });

  it('carries `measured` forward across a move', () => {
    // React Flow keeps a node's measured size and its handle bounds on the object it was given.
    // Hand it a fresh one and the handle bounds are reset, and every edge touching that node has
    // nowhere to land until the next measurement — which during a drag is every frame.
    const before = normaliseScratchDocument(saved());
    const first: ScratchFlowNode[] = toFlowNodes(before).map((n) =>
      n.id === 'a1' ? { ...n, measured: { width: 176, height: 54 } } : n
    );
    const after = toFlowNodes(withPositions(before, [{ id: 'a1', position: { x: 11, y: 22 } }]), first);
    const moved = after.find((n) => n.id === 'a1');
    expect(moved?.position).toEqual({ x: 11, y: 22 });
    expect(moved?.measured).toEqual({ width: 176, height: 54 });
  });

  it('keeps the untouched nodes as the very same objects', () => {
    const before = normaliseScratchDocument(saved());
    const first = toFlowNodes(before);
    const after = toFlowNodes(withPositions(before, [{ id: 'a1', position: { x: 11, y: 22 } }]), first);
    expect(after.find((n) => n.id === 'a2')).toBe(first.find((n) => n.id === 'a2'));
    expect(after.find((n) => n.id === 'a1')).not.toBe(first.find((n) => n.id === 'a1'));
  });

  it('replaces a flow node whose kind changed rather than mixing the two datas', () => {
    // Ids are only unique within a document and are minted from a counter; nothing stops a new
    // sketch reusing `n1` for a node where the last one had a note. Reusing the previous object
    // would leave a note's data under an actor's type.
    const first = toFlowNodes(
      normaliseScratchDocument({ nodes: [], edges: [], notes: [{ id: 'n1', at: { x: 0, y: 0 }, text: 'a note' }] })
    );
    const after = toFlowNodes(
      normaliseScratchDocument({
        nodes: [{ id: 'n1', kind: 'actor', at: { x: 0, y: 0 }, actor: 'nscheck', version: '0.1.0', method: 'ask' }],
        edges: [],
        notes: [],
      }),
      first
    );
    expect(after[0]?.type).toBe('actor');
    expect(after[0] && !isNoteNode(after[0]) && after[0].data.node.kind).toBe('actor');
  });
});

describe('the projection keeps what React Flow decided about an edge', () => {
  it('hands back the same array when nothing changed', () => {
    const doc = normaliseScratchDocument(saved());
    const first = toFlowEdges(doc);
    expect(toFlowEdges(doc, new Map(), first)).toBe(first);
  });

  it('carries `selected` forward across an unrelated document change', () => {
    // React Flow owns which edge is selected. Rebuilding the array deselects whatever the author
    // just clicked the moment anything else moves — and a deselected edge is one Delete does
    // nothing to, which is the whole way an edge drawn by mistake gets removed.
    const before = normaliseScratchDocument(saved());
    const first: ScratchFlowEdge[] = toFlowEdges(before).map((e) =>
      e.id === 'e2' ? { ...e, selected: true } : e
    );
    const after = toFlowEdges(
      withPositions(before, [{ id: 'a1', position: { x: 11, y: 22 } }]),
      new Map(),
      first
    );
    expect(after.find((e) => e.id === 'e2')?.selected).toBe(true);
  });

  it('rebuilds the edge whose FIELD changed, so its handles follow', () => {
    // Re-drawing an edge onto another field changes which handle it lands on. Reusing the previous
    // object would leave the line attached to a handle it no longer names — React Flow's error 008,
    // and no edge.
    const doc: ScratchDocument = normaliseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at: { x: 0, y: 0 }, actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'b', kind: 'actor', at: { x: 0, y: 0 }, actor: 'nscheck', version: '0.1.0', method: 'ask' },
      ],
      edges: [{ id: 'e', from: 'a', to: 'b', fromPort: 'body' }],
      notes: [],
    });
    const first = toFlowEdges(doc);
    const moved = normaliseScratchDocument({
      ...doc,
      edges: [{ id: 'e', from: 'a', to: 'b', fromPort: 'status' }],
    });
    const after = toFlowEdges(moved, new Map(), first);
    expect(after[0]?.sourceHandle).toBe('out:field:status');
    expect(after[0]).not.toBe(first[0]);
  });

  it('rebuilds the edge whose COMPLAINT changed, not only the marked-ness of it', () => {
    // Two different faults both mark the edge amber, so comparing the className alone would keep an
    // edge explaining a problem it no longer has — beside its label, and out loud to a screen
    // reader.
    const doc = normaliseScratchDocument(saved());
    const first = toFlowEdges(doc, new Map([['e2', 'body is not declared here']]));
    const after = toFlowEdges(doc, new Map([['e2', 'body is a string and count takes integer']]), first);
    const edge = after.find((e) => e.id === 'e2');
    expect(edge?.ariaLabel).toContain('count takes integer');
    expect(edge).not.toBe(first.find((e) => e.id === 'e2'));
  });
});

describe('an edge that does not line up is marked, not refused', () => {
  const doc = (): ScratchDocument => normaliseScratchDocument(saved());

  it('still draws every edge, whatever is said about it', () => {
    // REFUSING IS THE FAILURE. Scratch is where somebody thinks, and the actor that reads the
    // dataset may not be placed yet — a canvas that would not let the line be drawn would be
    // arguing with a drawing that is not finished.
    const marked = toFlowEdges(doc(), new Map([['e2', 'these two fields are different types']]));
    expect(marked.map((e) => e.id)).toEqual(['e1', 'e2', 'e3', 'e4']);
  });

  it('marks the edge and carries the sentence on the line itself', () => {
    // ON THE EDGE, NOT IN THE LABEL: the label is the author's own word for the edge ("only the ones
    // that answered") and overwriting it with a complaint would lose the one thing on the line that
    // came from a person.
    const [, e2] = toFlowEdges(
      doc(),
      new Map([['e2', 'body leaves as string and n takes integer — different types.']])
    );
    expect(e2?.className).toBe('scratch-edge-odd');
    expect(e2?.ariaLabel).toContain('different types');
    expect(e2?.label).toBeUndefined();
  });

  it('leaves an unmarked drawing entirely unmarked', () => {
    expect(toFlowEdges(doc()).map((e) => e.className)).toEqual([
      undefined,
      undefined,
      undefined,
      undefined,
    ]);
  });
});

describe('a Dataset’s direction is read off the drawing', () => {
  const lake = (direction: 'in' | 'out', edges: ScratchDocument['edges']): ScratchDocument =>
    normaliseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at: { x: 0, y: 0 }, actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'd', kind: 'dataset', at: { x: 0, y: 0 }, name: 'lame', direction },
      ],
      edges,
      notes: [],
    });

  const directionOf = (doc: ScratchDocument): string => {
    const node = withDerivedDirections(doc).nodes.find((n) => n.id === 'd');
    return node && node.kind === 'dataset' ? node.direction : '';
  };

  it('is WRITTEN TO when an edge lands on it', () => {
    // The palette used to ask, before anything was drawn. The drawing answers: an edge onto the
    // node's input is a write.
    expect(directionOf(lake('in', [{ id: 'e', from: 'a', to: 'd' }]))).toBe('out');
  });

  it('is READ FROM when an edge leaves it', () => {
    expect(directionOf(lake('out', [{ id: 'e', from: 'd', to: 'a' }]))).toBe('in');
  });

  it('is WRITTEN TO when it is both, because that is the claim that is true of it', () => {
    // A Dataset one step writes and the next reads is an ordinary drawing, and the document has one
    // field to say it in. Calling a read-only Dataset `out` is the overstatement that once
    // announced a thousand committed rows a second after a run started; calling this one `out` is
    // simply true — something here does produce it.
    const both = lake('in', [
      { id: 'in', from: 'a', to: 'd' },
      { id: 'out', from: 'd', to: 'a' },
    ]);
    expect(directionOf(both)).toBe('out');
  });

  it('keeps what a Dataset with no edges already has', () => {
    // There is no drawing to read it off, so nothing is derived and nothing is invented — which is
    // also what makes a freshly dropped Dataset legible before the first line is drawn to it.
    const alone = lake('out', []);
    expect(withDerivedDirections(alone)).toBe(alone);
  });

  it('returns the document ITSELF when the drawing already agrees', () => {
    // It runs on every edit, including a drag — so a document it changes nothing about must come
    // back unchanged, or every pointer move would mark the sketch unsaved.
    const agreed = lake('out', [{ id: 'e', from: 'a', to: 'd' }]);
    expect(withDerivedDirections(agreed)).toBe(agreed);
  });

  it('leaves the sketch this installation has saved exactly as it is', () => {
    // THE COMPATIBILITY CLAIM, against the real document rather than a fixture written to pass:
    // `domains` is read from and has an edge leaving it, `lame` is written to and has one landing on
    // it, so the drawing already says what the palette's buttons used to. Opening it must not
    // rewrite a byte — every open would otherwise be a diff, and nobody could tell one from an edit.
    const opened = normaliseScratchDocument(saved());
    expect(withDerivedDirections(opened)).toBe(opened);
    expect(JSON.stringify(withDerivedDirections(opened))).toBe(SAVED);
  });

  it('rewrites the one field, in the key order the server writes', () => {
    // Same rule as `withPositions` and `withMethod`: what is stored is `JSON.stringify` of these
    // objects, so a rebuilt node is a diff on a sketch whose one changed field is a single word.
    const before = lake('in', [{ id: 'e', from: 'a', to: 'd' }]);
    expect(JSON.stringify(withDerivedDirections(before))).toBe(
      JSON.stringify(before).replace('"direction":"in"', '"direction":"out"')
    );
  });
});

describe('drawing an edge between two fields', () => {
  const two = (edges: ScratchDocument['edges'] = []): ScratchDocument =>
    normaliseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at: { x: 0, y: 0 }, actor: 'probe', version: '0.1.0', method: 'fetch' },
        { id: 'b', kind: 'actor', at: { x: 0, y: 0 }, actor: 'probe', version: '0.1.0', method: 'title' },
      ],
      edges,
      notes: [],
    });

  it('stores the two field names, in the key order the server writes them', () => {
    const after = withEdge(two(), { id: 'e1', from: 'a', to: 'b', fromPort: 'body', toPort: 'html' });
    expect(JSON.stringify(after.edges)).toBe(
      '[{"id":"e1","from":"a","to":"b","fromPort":"body","toPort":"html"}]'
    );
  });

  it('stores no ports at all for an edge drawn between two whole nodes', () => {
    // Which is what every edge in an existing document is, and what a node with nothing declared
    // can only offer.
    const after = withEdge(two(), { id: 'e1', from: 'a', to: 'b' });
    expect(JSON.stringify(after.edges)).toBe('[{"id":"e1","from":"a","to":"b"}]');
  });

  it('refuses to draw the same edge twice and returns the document itself', () => {
    const once = withEdge(two(), { id: 'e1', from: 'a', to: 'b', fromPort: 'body', toPort: 'html' });
    expect(withEdge(once, { id: 'e2', from: 'a', to: 'b', fromPort: 'body', toPort: 'html' })).toBe(once);
  });

  it('lets two DIFFERENT fields run between the same pair of nodes', () => {
    // Two lines, two lines in the spec: `fetch.body → title.html` and `fetch.status → title.code`
    // carry different values, and collapsing them would lose one of them.
    const once = withEdge(two(), { id: 'e1', from: 'a', to: 'b', fromPort: 'body', toPort: 'html' });
    const twice = withEdge(once, { id: 'e2', from: 'a', to: 'b', fromPort: 'status', toPort: 'code' });
    expect(twice.edges.map((e) => e.id)).toEqual(['e1', 'e2']);
  });
});

describe('an edge’s label', () => {
  const doc = (): ScratchDocument =>
    normaliseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at: { x: 0, y: 0 }, actor: 'probe', version: '0.1.0', method: 'fetch' },
        { id: 'b', kind: 'actor', at: { x: 0, y: 0 }, actor: 'probe', version: '0.1.0', method: 'title' },
      ],
      edges: [{ id: 'e1', from: 'a', to: 'b', fromPort: 'body', toPort: 'html' }],
      notes: [],
    });

  it('goes on and comes off without taking the fields with it', () => {
    // THE BUG THIS PINS: clearing a label rebuilt the edge as `{id, from, to}`, which silently threw
    // away the two fields it attached to — the edge stayed on the canvas and jumped to the
    // whole-node handles, and the spec stopped saying which value travelled.
    const labelled = withEdgeLabel(doc(), 'e1', 'only the ones that answered');
    expect(labelled.edges[0]?.label).toBe('only the ones that answered');
    const cleared = withEdgeLabel(labelled, 'e1', '');
    expect(JSON.stringify(cleared.edges)).toBe(
      '[{"id":"e1","from":"a","to":"b","fromPort":"body","toPort":"html"}]'
    );
  });

  it('returns the document ITSELF when the label did not change', () => {
    const before = doc();
    expect(withEdgeLabel(before, 'e1', '')).toBe(before);
    expect(withEdgeLabel(before, 'ghost', 'x')).toBe(before);
  });
});

describe('removing a piece of the drawing', () => {
  it('takes the edges that pointed at it', () => {
    // A dangling edge cannot be drawn and cannot be followed. The server drops it on the way in,
    // so leaving one here would only mean the canvas and the stored document were two drawings
    // until the next save reconciled them.
    const after = withoutNode(normaliseScratchDocument(saved()), 'a2');
    // `a2` was the hub: e2 fed it, e3 and e4 ran out of it. Only `d1 → a1` is left.
    expect(after.nodes.map((n) => n.id)).toEqual(['d1', 'a1', 'a3', 'd2']);
    expect(after.edges.map((e) => e.id)).toEqual(['e1']);
  });

  it('removes a note by the same call', () => {
    const after = withoutNode(normaliseScratchDocument(saved()), 'n1');
    expect(after.notes.map((n) => n.id)).toEqual(['n2']);
  });

  it('removes an edge on its own, and returns the document itself when there is none to remove', () => {
    const before = normaliseScratchDocument(saved());
    expect(withoutEdges(before, ['e2']).edges.map((e) => e.id)).toEqual(['e1', 'e3', 'e4']);
    // A `remove` change for something already gone must not mark the sketch unsaved — React Flow
    // reports one per selected element, and deleting a node reports its edges too.
    expect(withoutEdges(before, ['ghost'])).toBe(before);
    expect(withoutEdges(before, [])).toBe(before);
  });
});

describe('picking the Method a node dispatches', () => {
  it('rewrites `method` and nothing else, in the key order the server writes', () => {
    // The palette lists one row per Method, so placing a node used to be the only moment it was
    // ever chosen. What must NOT come with the fix is a rebuilt node: what is stored is
    // `JSON.stringify` of these objects, and a node rebuilt in another key order is a diff on a
    // sketch whose one changed field is a single word.
    const before = normaliseScratchDocument(saved());
    const after = withMethod(before, 'a1', 'ask');
    expect(JSON.stringify(after)).toBe(SAVED.replace('"method":"delegation"', '"method":"ask"'));
  });

  it('leaves every edge the author drew exactly where it was', () => {
    // Reported, never deleted (`scratchInspect.ts`): changing the Method changes what the node
    // takes and emits, and a canvas that removed a line to keep itself consistent would be editing
    // the drawing rather than describing it.
    const before = normaliseScratchDocument(saved());
    const after = withMethod(before, 'a1', 'ask');
    expect(after.edges).toBe(before.edges);
    expect(after.notes).toBe(before.notes);
  });

  it('touches no other node, including one naming the same Actor and version', () => {
    // `a2` is the same `nscheck@0.1.0` at a different Method. A change keyed by anything but the
    // node id would move both.
    const before = normaliseScratchDocument(saved());
    const after = withMethod(before, 'a1', 'ask');
    expect(after.nodes[2]).toBe(before.nodes[2]);
    expect(after.nodes.filter((n) => n.kind === 'actor' && n.method === 'ask')).toHaveLength(2);
  });

  it('returns the document ITSELF for a Method that is already the one', () => {
    // Same rule as a drag that ended where it started: a change that changed nothing must not put a
    // permanent • beside the sketch's name.
    const before = normaliseScratchDocument(saved());
    expect(withMethod(before, 'a1', 'delegation')).toBe(before);
    expect(withMethod(before, 'ghost', 'ask')).toBe(before);
    // and a Dataset has no Method to pick — `d1` is left alone rather than growing one
    expect(withMethod(before, 'd1', 'ask')).toBe(before);
  });
});

describe('the round trip through save and load', () => {
  it('survives a drag, a save, a reload and a second load unchanged', () => {
    // What the browser sends is what the server stores is what the browser reads back. The one
    // thing that may differ is the position that was dragged.
    const opened = normaliseScratchDocument(saved());
    const dragged = withPositions(opened, [{ id: 'a3', position: { x: 512, y: 344 } }]);

    // `saveScratch` posts `JSON.stringify(document)`; the store keeps that string and hands it
    // back on the next `fetchScratch`.
    const stored = JSON.parse(JSON.stringify(dragged)) as unknown;
    const reopened = normaliseScratchDocument(stored);

    expect(JSON.stringify(reopened)).toBe(JSON.stringify(dragged));
    expect(reopened.nodes.find((n) => n.id === 'a3')?.at).toEqual({ x: 512, y: 344 });
    expect(toFlowNodes(reopened).find((n) => n.id === 'a3')?.position).toEqual({ x: 512, y: 344 });
  });

  /**
   * An edge drawn between two handles, taken all the way through the SERVER'S OWN parse and
   * rendering rather than a restatement of them.
   *
   * This is the criterion the whole slice rests on: connecting must not have invented an edge shape.
   * React Flow's vocabulary for a connection is `source`/`target`/`sourceHandle`/`targetHandle`, and
   * storing any of it would put the canvas library into the one document `renderScratch` reads —
   * `parseScratchDocument` would drop the handle ids on the way in and the spec would be built from
   * something the browser did not think it saved.
   */
  it('is the same edge the server already parsed, and it renders in the spec', () => {
    const before = normaliseScratchDocument(saved());
    // Exactly what the page appends on `onConnect` between two whole nodes: an id, and the two node
    // ids. Nothing else.
    const drawn: ScratchDocument = withEdge(before, { id: 'e5', from: 'a3', to: 'd2' });

    const stored = parseScratchDocument(JSON.parse(JSON.stringify(drawn)) as unknown);
    expect(JSON.stringify(stored)).toBe(JSON.stringify(drawn));
    expect(JSON.stringify(normaliseScratchDocument(stored))).toBe(JSON.stringify(drawn));

    const record = { id: 's1', name: 'lame nameserver sweep', document: stored, updatedAt: 0 };
    const spec = renderScratch(resolveScratch(record, []), []);
    expect(spec).toContain('- `probe@0.1.0.head` → `lame` (dataset)');
    // And the four it already handled are still there, labels and all.
    expect(spec).toContain('- `domains` (dataset) → `nscheck@0.1.0.delegation`  _(a page at a time)_');
  });

  /**
   * The same trip for an edge that names FIELDS, which is the criterion the ports rest on: an edge
   * attaches one output field to one input field and survives a reload. Every hop is the real one —
   * the page's own `withEdge`, the server's `parseScratchDocument`, the browser's own normalise —
   * because a port the server dropped on the way in would be a drawing that reads correct until
   * somebody presses reload.
   */
  it('carries the two fields through the server’s parse and into the spec', () => {
    const before = normaliseScratchDocument(saved());
    const drawn = withEdge(before, {
      id: 'e5',
      from: 'a2',
      to: 'a3',
      fromPort: 'body',
      toPort: 'url',
    });

    const stored = parseScratchDocument(JSON.parse(JSON.stringify(drawn)) as unknown);
    expect(JSON.stringify(stored)).toBe(JSON.stringify(drawn));
    expect(JSON.stringify(normaliseScratchDocument(stored))).toBe(JSON.stringify(drawn));
    expect(stored.edges.at(-1)).toEqual({
      id: 'e5',
      from: 'a2',
      to: 'a3',
      fromPort: 'body',
      toPort: 'url',
    });

    const record = { id: 's1', name: 'lame nameserver sweep', document: stored, updatedAt: 0 };
    const spec = renderScratch(resolveScratch(record, []), []);
    expect(spec).toContain('- `nscheck@0.1.0.ask.body` → `probe@0.1.0.head.url`');
    // and the agent is told how to read the dots, since one line in this drawing now has them
    expect(spec).toContain('An edge written `a.field → b.field` names the FIELDS it carries');
  });

  it('projects a reloaded ported edge back onto the same handles it was drawn between', () => {
    // The canvas after a reload has to be the canvas before it. A stored edge carries no handle ids
    // — the document holds the two FIELD names — so this is where they become handles again.
    const drawn = withEdge(normaliseScratchDocument(saved()), {
      id: 'e5',
      from: 'a2',
      to: 'a3',
      fromPort: 'body',
      toPort: 'url',
    });
    const reopened = normaliseScratchDocument(
      parseScratchDocument(JSON.parse(JSON.stringify(drawn)) as unknown)
    );
    const edge = toFlowEdges(reopened).find((e) => e.id === 'e5');
    expect(edge?.sourceHandle).toBe('out:field:body');
    expect(edge?.targetHandle).toBe('in:field:url');
  });
});
