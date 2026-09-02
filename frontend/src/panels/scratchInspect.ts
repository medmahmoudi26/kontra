/**
 * What one node of a Scratch actually IS, read out of the same catalog the palette was built from.
 *
 * THE CANVAS CAN ONLY EVER SHOW A NAME AND A SHAPE. A description is a paragraph and a signature is
 * a schema, and neither fits on a box somebody is dragging around — so until this existed, an
 * author composing two Actors had to leave Scratch for the Actors page to find out what either one
 * does, which is the reading that decides whether the edge they are drawing makes sense.
 *
 * NOTHING HERE FETCHES. The catalog, the workflow listing, the lake listing and the column schema
 * are already in the page — the palette and the ports are drawn from them — so the inspector reads
 * the SAME arrays. A second fetch would be a second opinion about what this installation has,
 * arriving at a different moment.
 *
 * AND THE PORTS ON THE CANVAS ARE THESE READINGS. `scratchPorts.ts` turns what is resolved here into
 * the handles down each node's edges, rather than looking the node up again — two answers to "what
 * are this node's fields" is the failure that would let the panel say a Method takes `{url}` while
 * the box beside it draws a handle called something else.
 *
 * THE UNRESOLVED SENTENCE IS THE SERVER'S OWN. `resolveScratch` (`src/scratch.ts`) already decides
 * what a node claims that the catalog does not have, and that sentence goes to the agent in the
 * spec. Restating it here in different words would give the author and the agent two accounts of
 * the same sketch; one is what makes the difference actionable — the author reads exactly what the
 * agent will read, against the node itself rather than in a list at the bottom of the spec.
 *
 * AND "NOT LOADED" IS NEVER SAID AS "NOT THERE". The lake listing polls (`store.loadDatasets`) and
 * the workflow listing is fetched once on mount; before either answers, an empty array is what the
 * page holds — and reading that as "no Dataset by this name exists" is a claim about the lake made
 * without having looked. Both carry a way to say "nobody has asked yet".
 */

import { resolveScratch } from '@core/scratch';
import type {
  DatasetInfo,
  ScratchDocument,
  ScratchNode,
  WorkflowDescriptor,
  WorkflowFile,
} from '../run/api';
import type { ActorOperation, CatalogActor, JsonSchema } from '../types';
import { groupDatasets, type DatasetGroup } from '../datasets/grouped';
import { schemaFields } from './MethodContract';

/**
 * Everything the inspector reads, as the page already holds it.
 *
 * `workflows` and `datasetsAt` carry the "has it answered yet" question because their absence and
 * their emptiness look identical in an array — see the header.
 */
export interface ScratchCatalogue {
  /** The Actor catalog, from the store — the same array the palette groups by Actor. */
  actors: readonly CatalogActor[];
  /** `.kontra/workflows/` as the page last listed it; `null` before the listing answered. */
  workflows: readonly WorkflowFile[] | null;
  /** What a worker described when it SERVED a workflow — keyed by the `@workflow.defn` type. */
  registered: readonly WorkflowDescriptor[];
  /** The lake listing, one row per `(name, version, dt)`. */
  datasets: readonly DatasetInfo[];
  /** When that listing was measured; `0` means it has not answered yet. */
  datasetsAt: number;
  /**
   * Every Dataset's COLUMNS, from `/api/datasets/schema` — one entry per name, and `null` until
   * that call answers.
   *
   * A SECOND CALL BECAUSE IT IS A SECOND QUESTION. The listing (`/api/datasets`) says what exists
   * and how many rows it has; this says what is IN it, and it is catalog metadata rather than a
   * scan — `querySchema` runs `DESCRIBE`, which opens no data file. A Dataset node's ports are
   * these columns, so a node drawn against a name the lake has never heard of has no fields to
   * offer and says so, rather than showing an empty table that reads as a Dataset with no columns.
   */
  columns: readonly DatasetColumns[] | null;
}

/** One Dataset's columns as `/api/datasets/schema` reports them. Structural, so the page hands over
 *  the `SchemaEntry`s the Datasets console already fetches rather than a copy of them. */
export interface DatasetColumns {
  name: string;
  columns: ReadonlyArray<{ name: string; type: string }>;
}

/** How an Actor node stands against the catalog. */
export type ActorStanding =
  /** The catalog has the Actor and the Method it names. */
  | { state: 'resolved'; op: ActorOperation }
  /**
   * The catalog has the Actor and the author has not picked a Method.
   *
   * A NORMAL STATE, NOT A FAULT. `ScratchActorNode.method` is allowed to be empty because a
   * half-drawn sketch is a legitimate thing to save, and the inspector is where it gets resolved —
   * so this is drawn as a prompt to pick, not as a problem to fix by deleting the node.
   */
  | { state: 'unchosen' }
  /** The catalog has the Actor and not this Method — `resolveScratch`'s sentence. */
  | { state: 'unknown-method'; detail: string }
  /** Nothing is registered under `actor@version` — `resolveScratch`'s sentence. */
  | { state: 'unknown-actor'; detail: string };

export interface ActorReading {
  kind: 'actor';
  node: Extract<ScratchNode, { kind: 'actor' }>;
  /** Every Method the catalog entry declares — what the inspector offers to pick from. Empty when
   *  no entry was found, and legitimately empty for a load-only Actor. */
  methods: ActorOperation[];
  standing: ActorStanding;
}

/** How a Workflow node stands against what workers have registered. */
export type WorkflowStanding =
  | { state: 'described'; descriptor: WorkflowDescriptor }
  /** Nothing has registered a contract under this file's name. */
  | { state: 'unregistered' }
  /** Several registered types share this file's name — a file may declare more than one
   *  `@workflow.defn`, and picking one of them here would be a guess. */
  | { state: 'ambiguous'; types: string[] };

export interface WorkflowReading {
  kind: 'workflow';
  node: Extract<ScratchNode, { kind: 'workflow' }>;
  /** The file in `.kontra/workflows/`, when the listing has one by that name. `null` when the
   *  listing has answered and does not — which is a thing to say, naming the file. */
  file: WorkflowFile | null;
  /** Whether the listing has answered at all. A missing file is only a claim once it has. */
  listed: boolean;
  standing: WorkflowStanding;
}

/** How a Dataset node stands against the lake listing. */
export type DatasetStanding =
  /** One group per kind that carries this name — an operator's list and an Actor's output can
   *  share one, and they are different tables. */
  | { state: 'listed'; groups: DatasetGroup[] }
  | { state: 'absent' }
  /** The listing has not answered yet, so nothing is being claimed about this name. */
  | { state: 'unread' };

/**
 * What the lake says this Dataset's COLUMNS are.
 *
 * THREE ANSWERS, LIKE EVERY OTHER SCHEMA READING ON THIS SURFACE. `unread` is before the call
 * answered — nothing is being claimed — and `absent` is a name `/api/datasets/schema` did not
 * mention, which is a Dataset nothing has written yet rather than one with no columns.
 */
export type DatasetColumnsReading =
  | { state: 'listed'; columns: ReadonlyArray<{ name: string; type: string }> }
  | { state: 'absent' }
  | { state: 'unread' };

export interface DatasetReading {
  kind: 'dataset';
  node: Extract<ScratchNode, { kind: 'dataset' }>;
  standing: DatasetStanding;
  /** The columns, which are also this node's ports (`scratchPorts.ts`). */
  columns: DatasetColumnsReading;
  /** When the listing was measured — every count drawn from it is a fact about that moment. */
  measuredAt: number;
}

export type ScratchReading = ActorReading | WorkflowReading | DatasetReading;

/** One node in the words the canvas puts on it, and the spec after it. */
export function nodeLabel(node: ScratchNode): string {
  if (node.kind === 'actor') return `${node.actor}@${node.version}.${node.method || '?'}()`;
  if (node.kind === 'workflow') return node.file;
  return node.name;
}

/**
 * What the server says does not resolve about ONE node.
 *
 * `resolveScratch` takes a whole record because the spec reports every problem at once; a
 * single-node document is how one node's sentence is asked for without restating the rule. The
 * catalog entries are the store's `CatalogActor`s, which carry everything a `CatalogEntry` needs
 * and more.
 */
function catalogSays(node: ScratchNode, actors: readonly CatalogActor[]): string {
  const record = { id: '', name: '', document: { nodes: [node], edges: [], notes: [] }, updatedAt: 0 };
  return resolveScratch(record, actors).problems[0]?.detail ?? '';
}

/**
 * The lake listing folded into groups, once per listing.
 *
 * ONE ENTRY, KEYED ON THE ARRAY ITSELF. This used to be called for the SELECTED node only, and it
 * is now called for every node in the document every time the document changes — the canvas derives
 * its ports from these readings (`scratchPorts.ts`), and a drag rewrites the document on every
 * pointer move. Re-folding a lake of several hundred dispatch rows once per Dataset node per frame
 * is work nothing asked for: the listing changes when the poll answers and not otherwise, and the
 * store hands out the same array until it does.
 */
let folded: { rows: readonly DatasetInfo[]; at: number; groups: DatasetGroup[] } | null = null;

function groupsOnce(rows: readonly DatasetInfo[], at: number): DatasetGroup[] {
  if (folded && folded.rows === rows && folded.at === at) return folded.groups;
  folded = { rows, at, groups: groupDatasets(rows, at) };
  return folded.groups;
}

/** Read one node against the catalog. */
export function readScratchNode(node: ScratchNode, cat: ScratchCatalogue): ScratchReading {
  if (node.kind === 'actor') {
    const entry = cat.actors.find((a) => a.name === node.actor && a.version === node.version);
    if (!entry) {
      return { kind: 'actor', node, methods: [], standing: { state: 'unknown-actor', detail: catalogSays(node, cat.actors) } };
    }
    const methods = entry.operations;
    if (!node.method) return { kind: 'actor', node, methods, standing: { state: 'unchosen' } };
    const op = methods.find((o) => o.name === node.method);
    return {
      kind: 'actor',
      node,
      methods,
      standing: op
        ? { state: 'resolved', op }
        : { state: 'unknown-method', detail: catalogSays(node, cat.actors) },
    };
  }

  if (node.kind === 'workflow') {
    const file = cat.workflows?.find((w) => w.name === node.file) ?? null;
    return {
      kind: 'workflow',
      node,
      file,
      listed: cat.workflows !== null,
      standing: describeWorkflow(node.file, cat.registered),
    };
  }

  const groups = groupsOnce(cat.datasets, cat.datasetsAt).filter((g) => g.name === node.name);
  // ONE ENTRY PER NAME, which is what `querySchema` emits (it dedupes before it DESCRIBEs) — so an
  // operator's list and an Actor's output sharing a name share one column list here, where the
  // listing above keeps them as two groups. That is the schema route's own answer, not a choice
  // made here, and taking the first match is taking the only one.
  const entry = cat.columns?.find((c) => c.name === node.name);
  return {
    kind: 'dataset',
    node,
    measuredAt: cat.datasetsAt,
    columns:
      cat.columns === null
        ? { state: 'unread' }
        : entry
          ? { state: 'listed', columns: entry.columns }
          : { state: 'absent' },
    standing:
      cat.datasetsAt === 0
        ? { state: 'unread' }
        : groups.length > 0
          ? { state: 'listed', groups }
          : { state: 'absent' },
  };
}

/**
 * The descriptor a workflow FILE's contract is in, joined by name.
 *
 * THE TWO SIDES ARE KEYED DIFFERENTLY AND ONLY ONE OF THEM IS HERE. A descriptor is keyed by the
 * `@workflow.defn` TYPE (`NsCheck`); a Scratch node names the FILE (`nscheck`), because that is
 * what the palette lists and what `kontra workflow serve` takes. The Workflows page joins them by
 * reading the type out of the source it is showing — a fetch per node, which a sidecar that opens
 * on every click cannot afford.
 *
 * SO THE JOIN IS THE ONE `guessTypeFromFilename` ALREADY MAKES, run backwards: strip everything but
 * letters and digits and compare case-insensitively, which is exactly the transformation between
 * `dns_sweep.py` and `DnsSweep`. A `@workflow.defn(name="Sweep")` does not match — and rather than
 * guess, that reads as "nothing registered a contract for this file", which is true of the join and
 * says what to do about it.
 */
export function describeWorkflow(
  file: string,
  registered: readonly WorkflowDescriptor[]
): WorkflowStanding {
  const key = typeKey(file);
  if (!key) return { state: 'unregistered' };
  const hits = registered.filter((d) => typeKey(d.name) === key);
  if (hits.length === 0) return { state: 'unregistered' };
  // A file CAN declare several `@workflow.defn` classes and each registers separately. Showing the
  // first one's schemas under this node would be a contract picked by array order.
  if (hits.length > 1) return { state: 'ambiguous', types: hits.map((d) => d.name) };
  return { state: 'described', descriptor: hits[0] as WorkflowDescriptor };
}

/** `nscheck.py` and `NsCheck` are the same key; `sweep` and `Sweep` are too. */
function typeKey(name: string): string {
  return name.replace(/\.py$/, '').replace(/[^a-zA-Z0-9]/g, '').toLowerCase();
}

/**
 * One schema as a comparable, printable port.
 *
 * The SAME flattening the field tables draw (`schemaFields`), so what a fallout sentence says a
 * Method takes is what the row under it lists. `not declared` stays its own answer here for the
 * same reason it does there: a Method that declares nothing and a Method that takes nothing are
 * different facts, and switching between two of them IS a change worth reporting.
 */
export function portText(schema?: JsonSchema): string {
  const fields = schemaFields(schema);
  if (fields === null) return 'not declared';
  if (fields.length === 0) return '{}';
  return `{${fields.map((f) => `${f.name}: ${f.type}`).join(', ')}}`;
}

/** One edge a Method change left to be reckoned with. Nothing is removed — see {@link methodFallout}. */
export interface EdgeFallout {
  edgeId: string;
  /** Which side of the changed node this edge is on: `in` feeds it, `out` is fed by it. */
  side: 'in' | 'out';
  /** The other end, in the words the canvas puts on it. */
  other: string;
  /** One sentence, addressed to whoever drew the edge. */
  detail: string;
}

/**
 * What a Method change did to the edges already attached to that node.
 *
 * CHANGING THE METHOD CHANGES WHAT THE NODE IS. `fetch` takes a URL and emits a body while `title`
 * takes a body — so an edge drawn into the node was drawn against a signature that is no longer
 * there, and pretending otherwise leaves the author with a drawing that reads correct and generates
 * a workflow that cannot be wired.
 *
 * REPORTED, NEVER DELETED. The author drew those edges; a canvas that removed a line to keep itself
 * consistent would be editing the drawing rather than describing it — the same rule
 * `scratchHandles.ts` follows for an edge that does not line up, and the reason `isValidConnection`
 * is left unwired.
 *
 * BY SIDE, because the two halves of a signature reach different edges: what FEEDS this node is
 * answered by its input, and what it feeds by its output. A Method whose output is unchanged leaves
 * everything downstream of it exactly as true as it was, and reporting it anyway would train the
 * author to dismiss the report without reading it.
 *
 * AND NOTHING IS REPORTED WHEN THE NODE HAD NO METHOD, which is the ordinary way a sketch is drawn:
 * place the Actor, draw the order, then come here and resolve it. There was no signature for those
 * edges to disagree with, so there is nothing to check.
 */
export function methodFallout(
  doc: ScratchDocument,
  nodeId: string,
  before: ActorOperation | undefined,
  after: ActorOperation | undefined
): EdgeFallout[] {
  if (!before || !after || before.name === after.name) return [];
  const byId = new Map(doc.nodes.map((n) => [n.id, n]));
  const takesChanged = portText(before.input) !== portText(after.input);
  const emitsChanged = portText(before.output) !== portText(after.output);
  if (!takesChanged && !emitsChanged) return [];

  const out: EdgeFallout[] = [];
  for (const e of doc.edges) {
    if (e.to === nodeId && takesChanged) {
      const other = byId.get(e.from);
      out.push({
        edgeId: e.id,
        side: 'in',
        other: other ? nodeLabel(other) : e.from,
        detail:
          `${after.name}() takes ${portText(after.input)}, where ${before.name}() took ` +
          `${portText(before.input)} — ${other ? nodeLabel(other) : e.from} was drawn feeding the old one.`,
      });
    }
    if (e.from === nodeId && emitsChanged) {
      const other = byId.get(e.to);
      out.push({
        edgeId: e.id,
        side: 'out',
        other: other ? nodeLabel(other) : e.to,
        detail:
          `${after.name}() emits ${portText(after.output)}, where ${before.name}() emitted ` +
          `${portText(before.output)} — ${other ? nodeLabel(other) : e.to} was drawn reading the old one.`,
      });
    }
  }
  return out;
}

/** The Method one Actor entry declares by that name, for {@link methodFallout}'s two ends. */
export function operationOf(
  actors: readonly CatalogActor[],
  node: Extract<ScratchNode, { kind: 'actor' }>,
  method: string
): ActorOperation | undefined {
  const entry = actors.find((a) => a.name === node.actor && a.version === node.version);
  return entry?.operations.find((o) => o.name === method);
}
