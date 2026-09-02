/**
 * The Scratch document as React Flow wants to see it, and the positions back again.
 *
 * TWO SHAPES, CONVERTED AT THE BOUNDARY. The document is the load-bearing one: `renderScratch` on
 * the server turns it into the spec an agent writes code from, and ADR 0026 makes that ONE
 * rendering the reason the surface is worth having. So `id`, `kind`, `at` and the notes keep their
 * names and meanings whatever the canvas library of the day would prefer to call them. React Flow
 * wants `position`, `type` and `data`; it gets a projection, and the document gets the positions
 * back. Nothing here reshapes the document to suit the library.
 *
 * A TYPE PER KIND, because that is the vocabulary the drawing is in. An Actor's Method, a caller
 * workflow and a Dataset are three different things to whoever reads the sketch back, and one
 * rectangle with a label made the reader work out which was which from the text.
 */

import type { CSSProperties } from 'react';
import type { Edge, Node } from '@xyflow/react';
import type {
  ScratchDocument,
  ScratchEdge,
  ScratchNode,
  ScratchNote,
  ScratchPoint,
} from '../run/api';
import { fieldHandle, handleKind, inHandle, outHandle } from './scratchHandles';

/** A document with nothing in it. Shared so "new sketch" and "nothing loaded" are the same value. */
export const EMPTY_SCRATCH: ScratchDocument = { nodes: [], edges: [], notes: [] };

/**
 * Every node type the canvas registers.
 *
 * `scratchNodeTypes` in `ScratchNodes.tsx` is typed by this, so a kind added here without a
 * component stops the build. Without that, React Flow falls back to its default node for an
 * unregistered type — an empty rectangle with no label, which the author can still drag and save,
 * and which says nothing about what it is.
 */
export const SCRATCH_NODE_TYPES = ['actor', 'workflow', 'dataset', 'note'] as const;
export type ScratchNodeType = (typeof SCRATCH_NODE_TYPES)[number];

/** A catalogued node — an Actor's Method, a caller workflow, or a Dataset. */
export type ScratchNodeData = { node: ScratchNode };
/** A note. Free text, deliberately not catalogued: it carries the paging and the failure policy no
 *  schema holds. */
export type ScratchNoteData = { note: ScratchNote };

export type ScratchFlowNode =
  | Node<ScratchNodeData, 'actor' | 'workflow' | 'dataset'>
  | Node<ScratchNoteData, 'note'>;

/**
 * `type` is optional on React Flow's `Node`, so `n.type === 'note'` does not narrow the union on
 * its own — TypeScript still allows a note node whose type is undefined. A guard states the
 * invariant this module actually maintains: everything it emits is typed.
 */
export function isNoteNode(n: ScratchFlowNode): n is Node<ScratchNoteData, 'note'> {
  return n.type === 'note';
}

/**
 * Node box geometry.
 *
 * THE WIDTH IS FIXED and the height is a FLOOR. A box that resized sideways to its content would
 * move every time a Method was renamed, and the drawing's shape is the thing being authored — but a
 * node now carries a handle per declared field down each edge, and a fixed height would either
 * clip the fields of a Method that declares six or leave a Dataset node mostly empty. 216 is two
 * readable columns of `name type`; 176 was one line of text and left an output field's type with
 * about four characters.
 */
export const NODE_W = 216;
export const NODE_H = 54;

/** Where the next placed node lands. Staggered so a run of clicks does not stack every node on one
 *  spot, which is what a fixed origin produces and is indistinguishable from nothing happening. */
export function nodeSlot(index: number): ScratchPoint {
  return { x: 60 + (index % 4) * (NODE_W + 48), y: 48 + Math.floor(index / 4) * (NODE_H + 64) };
}

/** Where the next note lands: BELOW the node grid, in a grid of its own. A note that lands on top
 *  of something hides it, and both mistakes were made getting here — the first note covered a node
 *  (nodes start at y=48, the note started at y=60), then a 26px cascade buried each note's text
 *  under the next one. A note is 188 wide, so four across at 200 and down at 100 leaves every one
 *  of them readable. */
export function noteSlot(index: number): ScratchPoint {
  return { x: 60 + (index % 4) * 200, y: 420 + Math.floor(index / 4) * 100 };
}

/** A usable coordinate, or nothing. `NaN` and `Infinity` are `typeof v === 'number'` and neither
 *  can be laid out — a NaN transform draws the node nowhere and takes its edges with it. */
function point(v: unknown): ScratchPoint | null {
  if (!v || typeof v !== 'object') return null;
  const p = v as Partial<ScratchPoint>;
  if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) return null;
  return p as ScratchPoint;
}

const isKind = (v: unknown): v is 'actor' | 'workflow' | 'dataset' =>
  v === 'actor' || v === 'workflow' || v === 'dataset';

/**
 * Read a stored document into one React Flow can be handed.
 *
 * POSITION IS THE TRAP, and it has taken the whole surface down before. React Flow reads
 * `node.position.x` while it lays out, so a saved document with no positions threw INSIDE the
 * library — and a throw in there blanks the page, not the one node: the surface went white and the
 * only trace was a stack in the console.
 *
 * THE SERVER'S PARSE DOES NOT CLOSE THIS. `isPoint` in `src/scratch.ts` accepts any two numbers,
 * and `NaN` is a number — so `{x: NaN, y: 0}` is stored, read back, and turned into
 * `translate(NaN, 0)`, which is a node nobody can find and edges attached to nothing. Missing
 * arrays are the other half: a document from before the three lists existed has no `nodes` at all,
 * and `undefined.map` is the same blank page by a different route.
 *
 * IT PLACES, IT DOES NOT INVENT. Everything that survives is the same object it arrived as, so a
 * document that was already well-formed re-serialises byte for byte — which is what keeps
 * `renderScratch` reading exactly what it read before somebody opened the page.
 */
export function normaliseScratchDocument(raw: unknown): ScratchDocument {
  const doc = (raw ?? {}) as Partial<ScratchDocument>;

  const nodes: ScratchNode[] = [];
  for (const candidate of Array.isArray(doc.nodes) ? doc.nodes : []) {
    const n = candidate as Partial<ScratchNode> & { at?: unknown };
    if (!n || typeof n.id !== 'string' || !n.id || !isKind(n.kind)) continue;
    const at = point(n.at);
    nodes.push(at ? (n as ScratchNode) : ({ ...n, at: nodeSlot(nodes.length) } as ScratchNode));
  }

  // An edge to a node that is not here cannot be drawn and cannot be followed. The server drops it
  // on the way in for that reason; dropping it here as well keeps the canvas and the stored
  // document the same drawing, rather than two that converge on the next save.
  const known = new Set(nodes.map((n) => n.id));
  const edges: ScratchEdge[] = [];
  for (const candidate of Array.isArray(doc.edges) ? doc.edges : []) {
    const e = candidate as Partial<ScratchEdge>;
    if (!e || typeof e.id !== 'string' || !e.id) continue;
    if (typeof e.from !== 'string' || typeof e.to !== 'string') continue;
    if (!known.has(e.from) || !known.has(e.to)) continue;
    edges.push(e as ScratchEdge);
  }

  const notes: ScratchNote[] = [];
  for (const candidate of Array.isArray(doc.notes) ? doc.notes : []) {
    const n = candidate as Partial<ScratchNote> & { at?: unknown };
    if (!n || typeof n.id !== 'string' || !n.id || typeof n.text !== 'string') continue;
    const at = point(n.at);
    // A note pinned to a node that is gone KEEPS ITS TEXT — losing it would lose the one thing in
    // the document a schema cannot carry — but it stops claiming a pin, because the server strips
    // that pin on the way in and the two would otherwise be different drawings until the next save.
    const pinned = typeof n.on === 'string' && known.has(n.on);
    if (at && (pinned || n.on === undefined)) {
      notes.push(n as ScratchNote);
      continue;
    }
    // Rebuilt in the server's own key order (`id, at, text, on`), so a note this had to repair
    // still serialises the way `parseScratchDocument` would have written it.
    const note: ScratchNote = { id: n.id, at: at ?? noteSlot(notes.length), text: n.text };
    notes.push(pinned ? { ...note, on: n.on as string } : note);
  }

  return { nodes, edges, notes };
}

/**
 * The document's nodes and notes, projected onto React Flow.
 *
 * `previous` IS NOT AN OPTIMISATION. React Flow keeps a node's measured size and its handle bounds
 * on the node object it was given; hand it a freshly built object and `measured` is gone, the handle
 * bounds are reset with it, and every edge attached to that node has nowhere to land until it is
 * measured again. During a drag that is every frame — the edges flicker off the node being moved.
 * Carrying the previous object forward keeps the measurement that the library, not the document, is
 * the owner of.
 *
 * Returning `previous` itself when nothing changed is what lets the caller's `setState` bail out
 * instead of re-rendering the canvas on every unrelated document edit.
 */
export function toFlowNodes(
  doc: ScratchDocument,
  previous: ScratchFlowNode[] = []
): ScratchFlowNode[] {
  const before = new Map(previous.map((n) => [n.id, n]));
  const next: ScratchFlowNode[] = [];

  for (const node of doc.nodes) {
    const prev = before.get(node.id);
    if (prev && !isNoteNode(prev) && prev.type === node.kind) {
      const moved = prev.position.x !== node.at.x || prev.position.y !== node.at.y;
      next.push(
        moved || prev.data.node !== node
          ? { ...prev, position: { x: node.at.x, y: node.at.y }, data: { node } }
          : prev
      );
    } else {
      next.push({
        id: node.id,
        type: node.kind,
        position: { x: node.at.x, y: node.at.y },
        data: { node },
      });
    }
  }

  for (const note of doc.notes) {
    const prev = before.get(note.id);
    if (prev && isNoteNode(prev)) {
      const moved = prev.position.x !== note.at.x || prev.position.y !== note.at.y;
      next.push(
        moved || prev.data.note !== note
          ? { ...prev, position: { x: note.at.x, y: note.at.y }, data: { note } }
          : prev
      );
    } else {
      next.push({
        id: note.id,
        type: 'note',
        position: { x: note.at.x, y: note.at.y },
        data: { note },
      });
    }
  }

  const same = next.length === previous.length && next.every((n, i) => n === previous[i]);
  return same ? previous : next;
}

/** One React Flow edge per document edge. The document's `from`/`to` are React Flow's
 *  `source`/`target`; the author's word for the edge is its label, drawn on the line. */
export type ScratchFlowEdge = Edge;

/** An edge that does not line up. Dashed and amber — the drawing is still drawn, because refusing
 *  it would be the canvas arguing with a sketch that is not finished (`scratchHandles.ts`). */
const ODD_EDGE: CSSProperties = { stroke: '#f59e0b', strokeDasharray: '5 4' };

/**
 * One document edge, projected.
 *
 * IT NAMES THE HANDLES IT LANDS ON, built by the same functions the node components draw with. Left
 * unnamed React Flow attaches to whichever handle of that type it finds first, which is right today
 * and silent when it stops being — an edge that quietly moves to another handle looks like a drawing
 * that changed itself. Named, a handle that is not there is React Flow's own error 008 with the edge
 * id in it.
 *
 * A PORT NAMES A FIELD'S HANDLE; NO PORT NAMES THE WHOLE NODE'S. That is the whole compatibility
 * story for documents drawn before typed ports: every edge in them has no ports, so every one of
 * them lands where it always did.
 */
function flowEdge(
  e: ScratchEdge,
  from: ScratchNode | undefined,
  to: ScratchNode | undefined,
  fault: string | undefined
): ScratchFlowEdge {
  const edge: ScratchFlowEdge = {
    id: e.id,
    source: e.from,
    target: e.to,
    sourceHandle: e.fromPort
      ? fieldHandle('out', e.fromPort)
      : from
        ? outHandle(handleKind(from))
        : null,
    targetHandle: e.toPort ? fieldHandle('in', e.toPort) : to ? inHandle(handleKind(to)) : null,
  };
  if (e.label) edge.label = e.label;
  if (fault) {
    edge.className = 'scratch-edge-odd';
    edge.style = ODD_EDGE;
    // ON THE EDGE ITSELF, not in the label: the label is the author's own word for the edge ("only
    // the ones that answered") and overwriting it with a complaint would lose the one thing on the
    // line that came from a person. `ariaLabel` is React Flow's own slot for what the edge means.
    edge.ariaLabel = fault;
    edge.data = { fault };
  }
  return edge;
}

/** Everything about a projected edge that the DOCUMENT decides. `ariaLabel` is compared and not only
 *  the className: two different faults both mark the edge amber, so an edge whose complaint changed
 *  from "no such field" to "different types" would otherwise keep the old sentence — which is read
 *  out to a screen reader and shown beside the edge's label. `selected` is deliberately not here:
 *  that one React Flow owns, and reusing the previous object is how it survives a re-sync. */
function sameEdge(a: ScratchFlowEdge, b: ScratchFlowEdge): boolean {
  return (
    a.source === b.source &&
    a.target === b.target &&
    a.sourceHandle === b.sourceHandle &&
    a.targetHandle === b.targetHandle &&
    a.label === b.label &&
    a.className === b.className &&
    a.ariaLabel === b.ariaLabel
  );
}

/**
 * The document's edges, projected — folded into the previous array for the same reason the nodes
 * are: React Flow owns whether an edge is SELECTED, and a freshly built edge object arrives
 * unselected. Without the fold, the edge you clicked deselects itself the moment anything else in
 * the document changes, and Delete then does nothing.
 */
export function toFlowEdges(
  doc: ScratchDocument,
  /** What is odd about each edge, by edge id (`scratchPorts.ts:portFaults`). Passed in rather than
   *  computed here because the same map is drawn beside the edge's label, and a canvas that derived
   *  it twice would be a canvas that could mark a line and then explain a different complaint. */
  faults: ReadonlyMap<string, string> = new Map(),
  previous: ScratchFlowEdge[] = []
): ScratchFlowEdge[] {
  const before = new Map(previous.map((e) => [e.id, e]));
  const byId = new Map(doc.nodes.map((n) => [n.id, n]));

  const next = doc.edges.map((e) => {
    const fresh = flowEdge(e, byId.get(e.from), byId.get(e.to), faults.get(e.id));
    const prev = before.get(e.id);
    return prev && sameEdge(prev, fresh) ? prev : fresh;
  });

  const same = next.length === previous.length && next.every((e, i) => e === previous[i]);
  return same ? previous : next;
}

/**
 * Draw an edge, from one field to another or from one node to another.
 *
 * KEYS IN THE SERVER'S OWN ORDER (`id, from, to, fromPort, toPort, label`), which is not fussiness:
 * what is stored is `JSON.stringify` of these objects and `parseScratchDocument` rebuilds them in
 * that order, so an edge built any other way here differs from the same edge after one
 * save-and-reload in nothing but key order — a diff on a sketch nobody edited.
 *
 * A DUPLICATE IS A NO-OP, not an error: a second identical edge draws exactly on top of the first,
 * so the author sees nothing happen and the document grows an edge that renders twice in the spec.
 * The ports are part of what makes two edges the same — `fetch.body → title.html` and
 * `fetch.status → title.code` are two fields carried between the same pair of nodes, which is two
 * lines and two lines in the spec.
 */
export function withEdge(
  doc: ScratchDocument,
  edge: { id: string; from: string; to: string; fromPort?: string; toPort?: string }
): ScratchDocument {
  const { id, from, to, fromPort, toPort } = edge;
  const already = doc.edges.some(
    (e) => e.from === from && e.to === to && e.fromPort === fromPort && e.toPort === toPort
  );
  if (already) return doc;
  const next: ScratchEdge = { id, from, to };
  if (fromPort) next.fromPort = fromPort;
  if (toPort) next.toPort = toPort;
  return { ...doc, edges: [...doc.edges, next] };
}

/**
 * Put the author's own word on one edge, or take it off.
 *
 * REBUILT WHEN IT IS CLEARED, and that is what makes this a function rather than three lines inside
 * the input's `onChange`. An empty label must not be stored as `"label":""` — the server drops an
 * empty one on the way in, so the next reload would differ from what the browser thought it saved —
 * so the edge is written out again without it. The version this replaced wrote `{id, from, to}`,
 * which was the whole edge while an edge was three keys, and threw the FIELDS away the moment
 * somebody cleared a label they had typed.
 */
export function withEdgeLabel(doc: ScratchDocument, id: string, label: string): ScratchDocument {
  let touched = false;
  const edges = doc.edges.map((e) => {
    if (e.id !== id || (e.label ?? '') === label) return e;
    touched = true;
    if (label) return { ...e, label };
    const bare: ScratchEdge = { id: e.id, from: e.from, to: e.to };
    if (e.fromPort) bare.fromPort = e.fromPort;
    if (e.toPort) bare.toPort = e.toPort;
    return bare;
  });
  return touched ? { ...doc, edges } : doc;
}

/**
 * Write each Dataset node's `direction` from the side its edges land on.
 *
 * THE DRAWING ALREADY ANSWERS IT. `direction` used to be declared — the palette had `IN` and `OUT`
 * buttons and the author picked one before drawing anything — and with typed ports that is asking to
 * be told the same thing twice, by two people who can disagree: an edge INTO a Dataset is a write and
 * an edge OUT of it is a read, whatever a button said last week.
 *
 * IT STILL HAS TO BE WRITTEN DOWN, because `renderScratch` reads it (ADR 0026) and the distinction is
 * real: reading and writing are the same call up to `.writer()`, and counting both as output once
 * announced a thousand committed rows a second after a run started. What changed is who answers,
 * not whether it is asked.
 *
 * WRITTEN-TO WINS WHEN IT IS BOTH, and that is the direction of the measured bug rather than a
 * toss-up. A Dataset one step writes and the next reads is an ordinary drawing and the document has
 * one field to say it in; calling it `out` claims something in the sketch produces it, which is TRUE
 * of that node — where calling a read-only Dataset `out` is exactly the overstatement that put a
 * thousand rows on the screen that nothing had written.
 *
 * A NODE WITH NO EDGES KEEPS WHAT IT HAS. There is no drawing to read it off, so nothing is derived
 * and nothing is invented — which is also what makes a freshly dropped Dataset legible before the
 * first line is drawn to it.
 */
export function withDerivedDirections(doc: ScratchDocument): ScratchDocument {
  const written = new Set<string>();
  const read = new Set<string>();
  for (const e of doc.edges) {
    written.add(e.to);
    read.add(e.from);
  }

  let touched = false;
  const nodes = doc.nodes.map((n) => {
    if (n.kind !== 'dataset') return n;
    const direction = written.has(n.id) ? 'out' : read.has(n.id) ? 'in' : n.direction;
    if (direction === n.direction) return n;
    touched = true;
    // SPREAD, NEVER REBUILT — same rule as `withPositions`: `direction` is already a key on a
    // dataset node, so assigning it keeps the server's own key order and a sketch whose direction
    // followed an edge diffs in one field.
    return { ...n, direction };
  });

  return touched ? { ...doc, nodes } : doc;
}

/** Remove edges by id — what React Flow reports when one is selected and deleted. */
export function withoutEdges(doc: ScratchDocument, ids: readonly string[]): ScratchDocument {
  const gone = new Set(ids);
  const edges = doc.edges.filter((e) => !gone.has(e.id));
  return edges.length === doc.edges.length ? doc : { ...doc, edges };
}

/** What React Flow reports when something was dragged: the thing and where it ended up. */
export interface ScratchMove {
  id: string;
  position: ScratchPoint;
}

/**
 * Write dragged positions back into the document.
 *
 * SPREAD, NEVER REBUILT. What is stored is `JSON.stringify` of these objects, and a rebuilt node
 * would put its keys back in whatever order the rebuild used — the same drawing, a different
 * document, and a diff on every save that moved nothing. Spreading keeps `at` in the slot it
 * already had and leaves everything else the object it was.
 *
 * A move to the position something is already at returns the document itself, so a click that
 * selects without dragging does not mark the sketch unsaved.
 */
export function withPositions(doc: ScratchDocument, moved: readonly ScratchMove[]): ScratchDocument {
  if (moved.length === 0) return doc;
  const to = new Map(moved.map((m) => [m.id, m.position]));
  let touched = false;

  const nodes = doc.nodes.map((n) => {
    const p = to.get(n.id);
    if (!p || (p.x === n.at.x && p.y === n.at.y)) return n;
    touched = true;
    return { ...n, at: { x: p.x, y: p.y } };
  });
  const notes = doc.notes.map((n) => {
    const p = to.get(n.id);
    if (!p || (p.x === n.at.x && p.y === n.at.y)) return n;
    touched = true;
    return { ...n, at: { x: p.x, y: p.y } };
  });

  return touched ? { ...doc, nodes, notes } : doc;
}

/**
 * Pick the Method one Actor node dispatches.
 *
 * THE ONLY MOMENT THE METHOD IS EVER CHOSEN USED TO BE THE PALETTE. Every palette row is one
 * Method, so placing a node decided it — and an author who placed the Actor before deciding, or
 * picked the wrong row, had a node that could only be deleted and drawn again, taking its edges
 * with it. `ScratchActorNode.method` was already allowed to be empty for exactly this reason
 * (`src/scratch.ts`); nothing could write it back.
 *
 * IT TOUCHES `method` AND NOTHING ELSE, by spreading rather than rebuilding — same rule as
 * {@link withPositions}. What is stored is `JSON.stringify` of these objects, and `method` is
 * already a key on an actor node, so assigning it keeps the server's own key order and a sketch
 * whose Method changed diffs in one field. Picking the Method already chosen returns the document
 * itself, so it does not mark the sketch unsaved.
 *
 * THE EDGES ARE LEFT ALONE, deliberately. Changing the Method changes what the node takes and emits,
 * so an edge drawn against the old signature may no longer line up — but the author drew it, and a
 * canvas that quietly deleted a line to keep itself consistent would be editing the drawing. The
 * fallout is REPORTED instead (`scratchInspect.ts`).
 */
export function withMethod(doc: ScratchDocument, id: string, method: string): ScratchDocument {
  let touched = false;
  const nodes = doc.nodes.map((n) => {
    if (n.id !== id || n.kind !== 'actor' || n.method === method) return n;
    touched = true;
    return { ...n, method };
  });
  return touched ? { ...doc, nodes } : doc;
}

/**
 * Remove a node or a note, and every edge that pointed at it.
 *
 * The server drops a dangling edge on the way in, so leaving one here would not corrupt anything —
 * it would just mean the canvas and the stored document were two different drawings until the next
 * save reconciled them, which is the sort of difference nobody can see and everybody argues about.
 */
export function withoutNode(doc: ScratchDocument, id: string): ScratchDocument {
  return {
    nodes: doc.nodes.filter((n) => n.id !== id),
    edges: doc.edges.filter((e) => e.from !== id && e.to !== id),
    notes: doc.notes.filter((n) => n.id !== id),
  };
}
