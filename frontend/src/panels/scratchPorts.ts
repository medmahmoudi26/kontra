/**
 * What a node's FIELDS are, and what it means when an edge joins two of them.
 *
 * THE SCHEMA IS THE EDGES. `fetch → title` is ambiguous about which of three fields carries the
 * page; `fetch.body → title.html` is not, and the agent writing code from the drawing stops having
 * to infer it. So every node connects per field: declared inputs are handles down the left edge,
 * declared outputs down the right, for all three kinds with no exceptions and no second mechanism.
 *
 * ONE ANSWER TO "WHAT ARE THIS NODE'S FIELDS", which is why this reads a `ScratchReading` rather
 * than the catalog. The inspector already resolves a node against the catalog, reports which Method
 * it dispatches and which edges a Method change stranded (`scratchInspect.ts`); ports derived from
 * a second lookup would be a second opinion — the panel saying a Method takes `{url}` while the box
 * beside it draws a handle called `target`, with nothing to say which is right.
 *
 * AND THE FIELDS THEMSELVES COME FROM `MethodContract.tsx` (`schemaFields`, `schemaType`), the same
 * flattening the field tables draw, for the same reason. `readSchema` keeps its three answers apart
 * — `not declared`, `declares no fields`, and a list — because an Actor that declares nothing and
 * one that takes nothing are different facts, and only one of them can be called with anything.
 *
 * TYPED, NOT VALIDATING (ADR 0026). Everything here MARKS; nothing refuses. A type that does not
 * line up, and a field that is not there at all, are both drawn and both said out loud — Scratch is
 * where an operator thinks, and a surface that argues with an unfinished drawing is one they stop
 * opening.
 */

import type { ScratchDocument } from '../run/api';
import { readSchema, sayNothing } from './workflowContract';
import { nodeLabel, readScratchNode, type ScratchCatalogue, type ScratchReading } from './scratchInspect';
import type { JsonSchema } from '../types';

/** One field, on one side of one node. */
export interface ScratchPort {
  /** The field, as its schema or its table column names it. */
  name: string;
  /** Its declared type, in the words the field tables use (`schemaType`) — or the lake's own column
   *  type for a Dataset, which is a DuckDB type and says so. */
  type: string;
  required: boolean;
  /**
   * False when NOTHING declares this field and only an EDGE names it.
   *
   * A METHOD CHANGE STRANDS FIELDS, and React Flow drops an edge whose named handle does not exist —
   * so a port that vanished would take the author's line off the canvas with it, which is deleting
   * the drawing rather than reporting it. The port stays, marked, and the edge stays drawn.
   */
  declared: boolean;
}

/** One side of one node: its fields, and — when it has none — why. */
export interface PortSide {
  ports: ScratchPort[];
  /** Whether anything declared these fields at all. */
  declared: boolean;
  /**
   * Why there are no fields on this side, for the whole-node handle to say. Empty when there are.
   *
   * IT IS NOT AN ERROR, and the wording has to carry that. A Method that declares neither `takes=`
   * nor `emits=`, a workflow annotated `dict`, a node whose Method is not picked yet — all of them
   * are ordinary states somebody is mid-way through drawing, and a node that read as broken because
   * of one would be a node they deleted and drew again.
   */
  why: string;
}

export interface NodePorts {
  in: PortSide;
  out: PortSide;
}

/** A node with nothing declared on either side. Shared so "no catalog entry" and "not asked yet"
 *  differ in their sentence and in nothing else. */
const nothing = (why: string): NodePorts => ({
  in: { ports: [], declared: false, why },
  out: { ports: [], declared: false, why },
});

/** One schema as a side. `readSchema` is `WorkflowContract`'s, which is `schemaFields` with the
 *  undeclared/no-fields distinction kept — the distinction this whole surface turns on. */
function side(schema: JsonSchema | undefined): PortSide {
  const reading = readSchema(schema);
  if (reading.kind !== 'fields') return { ports: [], declared: false, why: sayNothing(reading) };
  // NAMED, NOT SPREAD. `schemaFields` hands back the top level of a field TREE — a nested object
  // carries its whole subtree on it — and a canvas port is a handle with a name and a type. Copying
  // the tree onto every handle would put a schema inside a drawing that only ever compares names.
  return {
    ports: reading.fields.map((f) => ({
      name: f.name,
      type: f.type,
      required: f.required,
      declared: true,
    })),
    declared: true,
    why: '',
  };
}

/**
 * The ports one node declares, from what it IS.
 *
 * PER KIND, and each one from the thing that actually holds the signature: an Actor's ports are the
 * input and output schemas of the Method THIS NODE SELECTED — change the Method in the inspector and
 * the handles change, because `fetch` takes a URL and emits a body while `title` takes a body — a
 * workflow's are its registered `WorkflowDescriptor`, and a Dataset's are its columns.
 */
export function nodePorts(reading: ScratchReading): NodePorts {
  if (reading.kind === 'actor') {
    const { standing } = reading;
    if (standing.state === 'unchosen') return nothing('no Method chosen yet');
    if (standing.state === 'unknown-actor') return nothing('the catalog has no such Actor');
    if (standing.state === 'unknown-method') return nothing('the Actor declares no such Method');
    return { in: side(standing.op.input), out: side(standing.op.output) };
  }

  if (reading.kind === 'workflow') {
    const { standing } = reading;
    if (standing.state === 'unregistered') return nothing('no worker has registered its contract');
    if (standing.state === 'ambiguous') return nothing('several registered types share this name');
    return { in: side(standing.descriptor.input), out: side(standing.descriptor.output) };
  }

  // A DATASET'S TWO SIDES ARE THE SAME COLUMNS, because they are the same table: writing into
  // `url` and reading out of `url` are one column seen from two ends, and giving each side its own
  // list would invite them to differ.
  const { columns } = reading;
  if (columns.state === 'unread') return nothing('the lake has not answered with its columns yet');
  if (columns.state === 'absent') return nothing('nothing in the lake has this name yet');
  const ports = columns.columns.map((c) => ({
    name: c.name,
    type: c.type,
    required: false,
    declared: true,
  }));
  return {
    in: { ports: [...ports], declared: true, why: '' },
    out: { ports: [...ports], declared: true, why: '' },
  };
}

/**
 * Every node's ports, with the ones only an EDGE knows about added back.
 *
 * THE STRANDED PORT IS THE POINT OF DOING THIS OVER THE WHOLE DOCUMENT. Change a node's Method and
 * the field an edge was drawn to is suddenly not declared anywhere — and React Flow does not draw an
 * edge whose named handle is missing, so the author's line would simply disappear off the canvas
 * while staying in the document. Nothing is deleted here and nothing is deleted there
 * (`scratchInspect.ts:methodFallout` reports the same fact in sentences): the port comes back marked
 * as undeclared, the edge stays drawn, and {@link portFaults} says what is wrong with it.
 */
export function documentPorts(doc: ScratchDocument, cat: ScratchCatalogue): Map<string, NodePorts> {
  const ports = new Map<string, NodePorts>();
  for (const node of doc.nodes) ports.set(node.id, nodePorts(readScratchNode(node, cat)));

  for (const e of doc.edges) {
    if (e.fromPort) strand(ports.get(e.from)?.out, e.fromPort);
    if (e.toPort) strand(ports.get(e.to)?.in, e.toPort);
  }
  return ports;
}

/** Put back a field an edge names and the node does not declare. Once — two edges leaving the same
 *  stranded field are two lines from one handle, not two handles. */
function strand(into: PortSide | undefined, field: string): void {
  if (!into || into.ports.some((p) => p.name === field)) return;
  into.ports.push({ name: field, type: '', required: false, declared: false });
}

/**
 * What is odd about every edge in the drawing, by edge id.
 *
 * MARKED, NEVER REFUSED, and the sentence is the whole of the marking's value: the canvas draws an
 * odd edge dashed and amber, which is enough to notice and not enough to act on. The wording follows
 * `ScratchProblem.detail` on the server — the same job (the drawing claims something this build does
 * not believe) done for the types rather than for the catalog.
 *
 * A PORTLESS EDGE IS NEVER ODD. It means the whole node, which is how "these two are connected, I
 * have not said how yet" stays sayable — and how every edge in every document drawn before typed
 * ports still reads.
 */
export function portFaults(
  doc: ScratchDocument,
  ports: ReadonlyMap<string, NodePorts>
): Map<string, string> {
  const byId = new Map(doc.nodes.map((n) => [n.id, n]));
  const faults = new Map<string, string>();

  for (const e of doc.edges) {
    const from = byId.get(e.from);
    const to = byId.get(e.to);
    if (!from || !to) continue;
    const source = e.fromPort ? find(ports.get(e.from)?.out, e.fromPort) : undefined;
    const target = e.toPort ? find(ports.get(e.to)?.in, e.toPort) : undefined;

    if (e.fromPort && !source?.declared) {
      faults.set(
        e.id,
        `${nodeLabel(from)} declares no output field ${e.fromPort} — this edge leaves from one, so ` +
          'either what the node is changed under it or the field was renamed.'
      );
      continue;
    }
    if (e.toPort && !target?.declared) {
      faults.set(
        e.id,
        `${nodeLabel(to)} declares no input field ${e.toPort} — this edge lands on one, so either ` +
          'what the node is changed under it or the field was renamed.'
      );
      continue;
    }
    // BOTH ENDS OR NEITHER. A field named on one side only is half a thought somebody is still
    // having — the other end's Method may not be picked yet — and there is nothing to compare.
    if (source && target && !typesLineUp(source.type, target.type)) {
      faults.set(
        e.id,
        `${source.name} leaves as ${source.type} and ${target.name} takes ${target.type} — ` +
          'this edge joins two fields of different types.'
      );
    }
  }
  return faults;
}

const find = (s: PortSide | undefined, name: string): ScratchPort | undefined =>
  s?.ports.find((p) => p.name === name);

/**
 * Do two declared types line up?
 *
 * COARSE ON PURPOSE, AND SILENT WHEN IT DOES NOT KNOW. The two ends of an edge are not written in
 * one language: a Method's field is a JSON Schema type (`string`, `integer`) and a Dataset's column
 * is a DuckDB one (`VARCHAR`, `HUGEINT`, `STRUCT(...)`), so a string comparison would mark every
 * edge between an Actor and a Dataset in the lake — which is most of the edges anybody draws, and a
 * marking that is always on is one nobody reads. Anything unrecognised answers "lines up", because
 * the alternative is this surface asserting a mismatch it cannot actually see.
 */
export function typesLineUp(a: string, b: string): boolean {
  const left = typeFamilies(a);
  const right = typeFamilies(b);
  if (left.size === 0 || right.size === 0) return true;
  for (const f of left) if (right.has(f)) return true;
  return false;
}

/** The families one declared type could be. A union — `string | null` from a derived optional, or an
 *  `anyOf` — is several, and matching any one of them is a match. */
export function typeFamilies(type: string): Set<string> {
  const out = new Set<string>();
  for (const part of type.split('|')) {
    const family = familyOf(part);
    if (family) out.add(family);
  }
  return out;
}

const NUMBERS = new Set([
  'NUMBER',
  'INTEGER',
  'INT',
  'TINYINT',
  'SMALLINT',
  'BIGINT',
  'HUGEINT',
  'UTINYINT',
  'USMALLINT',
  'UINTEGER',
  'UBIGINT',
  'UHUGEINT',
  'FLOAT',
  'REAL',
  'DOUBLE',
  'DECIMAL',
  'NUMERIC',
]);

const STRINGS = new Set(['STRING', 'VARCHAR', 'TEXT', 'CHAR', 'BPCHAR', 'UUID', 'DATE', 'TIME']);

function familyOf(raw: string): string {
  const t = raw.trim().toUpperCase();
  // `body[]` and `VARCHAR[]` are both a list of something. The element type is deliberately not
  // compared: this marks a value going somewhere it plainly cannot, not a schema check.
  if (t.endsWith('[]')) return 'array';
  // `DECIMAL(18,3)` and `STRUCT(a VARCHAR, …)` carry their shape in the parens; the family is the
  // word in front of it.
  const base = (t.split('(')[0] ?? '').trim();
  if (!base) return '';
  if (base === 'STRUCT' || base === 'MAP' || base === 'JSON' || base === 'OBJECT') return 'object';
  if (base === 'ARRAY' || base === 'LIST') return 'array';
  if (base === 'BOOLEAN' || base === 'BOOL') return 'boolean';
  if (base === 'NULL') return 'null';
  if (NUMBERS.has(base)) return 'number';
  if (STRINGS.has(base) || base.startsWith('TIMESTAMP')) return 'string';
  // `any`, a `$ref`'d model name, `BLOB`, an interval — nothing this can compare, and saying so is
  // how it stays silent rather than wrong.
  return '';
}

/**
 * A type short enough for a handle's label. The full one is in its `title`.
 *
 * A lake column can be `STRUCT(body_len BIGINT, body_preview VARCHAR, …)` — two hundred characters
 * of it — and a node is 216 pixels wide.
 */
export function shortType(type: string): string {
  const paren = type.indexOf('(');
  return paren === -1 ? type : `${type.slice(0, paren)}…`;
}

