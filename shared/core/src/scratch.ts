/**
 * A **Scratch**: a drawing of an orchestration that an agent can read back and write code from.
 *
 * WHAT IT IS FOR. You have Actors, Workflows and Datasets. You know roughly how the next run
 * should hang together, and writing that as a caller workflow is fifty lines of Temporal you have
 * to get exactly right before you can see whether the SHAPE was right. So: draw it, copy the URL,
 * and hand it to an agent — `use kontra mcp to build this workflow <url>` — which reads this
 * document and writes the workflow.
 *
 * THE NODES ARE REAL THINGS, and that is the whole design. A node is not a rectangle with a label
 * somebody typed; it is `actor nscheck@0.1.0 method=delegation`, resolved against the catalog. That
 * distinction is what separates a document an agent can act on from one it has to guess at:
 *
 *   drawn                        an agent reading a LABEL must guess
 *   ─────                        ─────────────────────────────────────
 *   "nscheck.delegation"         which actor? which version? is `delegation` a Method or a note?
 *   actor nscheck@0.1.0          nothing. It is the catalog's own key, and the Method is one it
 *     method=delegation          declares, so the generated dispatch cannot name something absent.
 *
 * AND THE NOTES ARE FREE, because the types cannot carry intent. "page 200 at a time", "isolate,
 * do not fail the run", "this fan-out is about 4x" are the things a person knows and a schema does
 * not, and they are exactly what makes generated code right rather than merely valid. They travel
 * verbatim.
 *
 * IT IS A SKETCH, NOT AN EXECUTION PLAN. Nothing here runs. kontra HAD a canvas that was the
 * execution model — nodes were dispatches, edges were data flow, and the server interpreted it —
 * and it was removed (ADR 0023 §12) because a caller's own workflow is a better program than a
 * graph anybody can draw. This is the opposite direction: the drawing is INPUT to writing that
 * workflow, and the workflow is what runs.
 */

/** Where a node sits on the canvas. Decoration — the graph means the same thing at any position. */
export interface ScratchPoint {
  x: number;
  y: number;
}

/** One Method of one catalogued Actor. `actor@version` is the catalog key; `method` is one of the
 *  operations that key declares. */
export interface ScratchActorNode {
  id: string;
  kind: 'actor';
  at: ScratchPoint;
  actor: string;
  version: string;
  /** The Method to dispatch. Empty when the author has not picked one yet — a half-drawn sketch is
   *  a legitimate thing to save, and `resolveScratch` reports it rather than refusing to store it. */
  method: string;
}

/** A caller workflow that already exists, by the file that defines it. */
export interface ScratchWorkflowNode {
  id: string;
  kind: 'workflow';
  at: ScratchPoint;
  file: string;
}

/**
 * A Dataset, by name.
 *
 * `direction` is which of the two things is being drawn — the same name can be read by one node and
 * written by another, and the distinction produced a real bug once: reading and writing are the same
 * call up to `.writer()`, and counting both as output announced a thousand committed rows one
 * second after a run started, most of them an input list it had not read yet. So the spec says which
 * it is, and this field is what it says it from.
 *
 * WHO ANSWERS IT CHANGED, AND THE FIELD DID NOT. It used to be declared: the palette offered `IN`
 * and `OUT` buttons and the author picked one before drawing anything. With typed ports the drawing
 * already answers it — an edge onto the node's input is a write, an edge out of its output is a
 * read — so the canvas derives it (`scratchFlow.ts:withDerivedDirections`) and asking the author
 * separately would be asking to be told two different things. A node nothing is connected to keeps
 * whatever it has, because then there is no drawing to read it off.
 */
export interface ScratchDatasetNode {
  id: string;
  kind: 'dataset';
  at: ScratchPoint;
  name: string;
  direction: 'in' | 'out';
}

export type ScratchNode = ScratchActorNode | ScratchWorkflowNode | ScratchDatasetNode;

/**
 * Work flows from `from` to `to`. `label` is the author's word for the edge — "per page", "only
 * the failures" — and is carried verbatim into the spec.
 *
 * `fromPort` and `toPort` name the FIELD at each end: `fetch.body → title.html` says which value
 * travels, where `fetch → title` leaves an agent to work it out from two schemas that may have
 * three candidate fields between them.
 *
 * BOTH ARE OPTIONAL, AND ABSENT MEANS THE WHOLE NODE — which is exactly what every edge drawn
 * before typed ports means, so an existing document loads with no migration and renders the same
 * sentence it always did. It is also a state worth being able to draw: a Method that declares
 * neither `takes=` nor `emits=`, a workflow annotated `dict`, and a node whose Method has not been
 * picked yet all have no field to name, and "these two are connected, I have not said how yet" has
 * to stay sayable.
 */
export interface ScratchEdge {
  id: string;
  from: string;
  to: string;
  fromPort?: string;
  toPort?: string;
  label?: string;
}

/** Intent the types cannot carry. `on` pins a note to a node; without it it is about the drawing. */
export interface ScratchNote {
  id: string;
  at: ScratchPoint;
  text: string;
  on?: string;
}

export interface ScratchDocument {
  nodes: ScratchNode[];
  edges: ScratchEdge[];
  notes: ScratchNote[];
}

export interface ScratchRecordLike {
  id: string;
  name: string;
  document: ScratchDocument;
  updatedAt: number;
}

/* ───────────────────────────── the subject ───────────────────────────── */

/**
 * The id one WORKFLOW's sketch is stored under.
 *
 * THE SUBJECT IS THE KEY, and that is the whole of what changed. A Scratch was a drawing with
 * nothing it was about: the surface listed them by a name somebody typed, so opening one meant
 * remembering which of nine sketches went with the run in front of you, and the answer to
 * "what was this workflow meant to be" was a search rather than a tab. Deriving the id from the
 * workflow makes that question an address — the sketch loads with the thing it is a sketch OF, and
 * a workflow with none reads as having none rather than as a list nobody scrolled far enough
 * through.
 *
 * IT IS A KEY AND NOT A COLUMN, deliberately. `scratches` already has the id as its primary key and
 * the save is already an upsert on it (`db/repo.ts`), so a sketch attached to a workflow needs no
 * migration, no join, and no second way to find it — and one workflow can only have one sketch,
 * which is the constraint a column would have had to be given anyway.
 *
 * THE PREFIX IS WHAT MAKES A SKETCH DRAWN BEFORE THIS DISTINGUISHABLE FROM ONE DRAWN AFTER. The old
 * surface minted a `randomUUID`, so nothing in the store can collide with `workflow:<name>`, and a
 * sketch with no subject stays exactly what it is: readable through the API, attached to nothing.
 */
export const WORKFLOW_SCRATCH_PREFIX = 'workflow:';

/** Where `<workflow>`'s sketch lives. The workflow's own name — the one a folder is registered and
 *  opened under, not the decorated `@workflow.defn` type, because the type is not knowable without
 *  reading source and a sketch must be findable before anything has been served. */
export function workflowScratchId(workflow: string): string {
  return `${WORKFLOW_SCRATCH_PREFIX}${workflow}`;
}

/** Which workflow a stored Scratch belongs to, or `null` for one drawn before sketches had a
 *  subject. Those are not errors and are not migrated: they are drawings about nothing, which is
 *  what they were saved as. */
export function scratchWorkflow(id: string): string | null {
  if (!id.startsWith(WORKFLOW_SCRATCH_PREFIX)) return null;
  return id.slice(WORKFLOW_SCRATCH_PREFIX.length) || null;
}

/** An Actor as the catalog holds it — only the parts a Scratch checks against. */
export interface CatalogEntry {
  name: string;
  version: string;
  operations: Array<{ name: string; description?: string }>;
}

/**
 * Something a Scratch says that the catalog does not agree with.
 *
 * REPORTED, NEVER CORRECTED, and never a reason to refuse the document. A sketch naming an Actor
 * that does not exist yet is a perfectly ordinary thing to draw — it is how somebody works out
 * what to build — and a canvas that refused to save it would be a canvas nobody could think in.
 * What matters is that the AGENT is told, so it writes a dispatch against a Method that exists or
 * says plainly that it cannot.
 */
export interface ScratchProblem {
  nodeId: string;
  /** One sentence, addressed to whoever reads the spec — a person or an agent. */
  detail: string;
}

export interface ResolvedScratch {
  record: ScratchRecordLike;
  problems: ScratchProblem[];
}

const isPoint = (v: unknown): v is ScratchPoint =>
  !!v && typeof v === 'object' && typeof (v as ScratchPoint).x === 'number' && typeof (v as ScratchPoint).y === 'number';

const str = (v: unknown): string => (typeof v === 'string' ? v : '');

/**
 * Narrow untrusted JSON into a ScratchDocument.
 *
 * DROPS WHAT IT CANNOT UNDERSTAND rather than throwing. A document arrives from a browser and is
 * handed to an agent that will write code from it, so a node of an unknown kind is a node this
 * build has no meaning for — keeping it would put an uninterpretable thing in front of the agent,
 * and refusing the whole document would lose the twenty nodes that were fine.
 *
 * An edge pointing at a node that is not here is dropped for the same reason: it cannot be drawn
 * and cannot be followed, so carrying it forward is carrying a dangling reference into generated
 * code.
 */
export function parseScratchDocument(raw: unknown): ScratchDocument {
  const doc = (raw ?? {}) as Partial<ScratchDocument>;
  const nodes: ScratchNode[] = [];
  for (const n of Array.isArray(doc.nodes) ? doc.nodes : []) {
    const base = n as Partial<ScratchNode> & { at?: unknown };
    const id = str(base.id);
    if (!id || !isPoint(base.at)) continue;
    if (base.kind === 'actor') {
      const a = n as Partial<ScratchActorNode>;
      if (!str(a.actor)) continue;
      nodes.push({ id, kind: 'actor', at: base.at, actor: str(a.actor), version: str(a.version), method: str(a.method) });
    } else if (base.kind === 'workflow') {
      const w = n as Partial<ScratchWorkflowNode>;
      if (!str(w.file)) continue;
      nodes.push({ id, kind: 'workflow', at: base.at, file: str(w.file) });
    } else if (base.kind === 'dataset') {
      const d = n as Partial<ScratchDatasetNode>;
      if (!str(d.name)) continue;
      nodes.push({ id, kind: 'dataset', at: base.at, name: str(d.name), direction: d.direction === 'out' ? 'out' : 'in' });
    }
  }

  const known = new Set(nodes.map((n) => n.id));
  const edges: ScratchEdge[] = [];
  for (const e of Array.isArray(doc.edges) ? doc.edges : []) {
    const edge = e as Partial<ScratchEdge>;
    const id = str(edge.id);
    const from = str(edge.from);
    const to = str(edge.to);
    // A dangling edge cannot be drawn and cannot be followed. Dropping it here is what keeps a
    // dangling reference out of whatever an agent generates.
    if (!id || !known.has(from) || !known.has(to)) continue;
    // ASSEMBLED KEY BY KEY, so an edge that names no field is byte for byte the object it was
    // before ports existed. What is stored is `JSON.stringify` of these, and building
    // `{id, from, to, fromPort, toPort, label}` with three undefineds would still serialise to the
    // old string — but only by accident of how JSON drops them, and one `?? ''` away from
    // rewriting every edge in every saved sketch.
    const kept: ScratchEdge = { id, from, to };
    const fromPort = str(edge.fromPort);
    const toPort = str(edge.toPort);
    const label = str(edge.label);
    if (fromPort) kept.fromPort = fromPort;
    if (toPort) kept.toPort = toPort;
    if (label) kept.label = label;
    edges.push(kept);
  }

  const notes: ScratchNote[] = [];
  for (const n of Array.isArray(doc.notes) ? doc.notes : []) {
    const note = n as Partial<ScratchNote>;
    const id = str(note.id);
    if (!id || !isPoint(note.at) || !str(note.text)) continue;
    const on = str(note.on);
    notes.push(on && known.has(on) ? { id, at: note.at, text: str(note.text), on } : { id, at: note.at, text: str(note.text) });
  }

  return { nodes, edges, notes };
}

/** What a Scratch claims, checked against the catalog. See {@link ScratchProblem}. */
export function resolveScratch(
  record: ScratchRecordLike,
  catalog: readonly CatalogEntry[]
): ResolvedScratch {
  const problems: ScratchProblem[] = [];
  for (const node of record.document.nodes) {
    if (node.kind !== 'actor') continue;
    const entry = catalog.find((a) => a.name === node.actor && a.version === node.version);
    if (!entry) {
      problems.push({
        nodeId: node.id,
        detail:
          `no Actor ${node.actor}@${node.version || '?'} is registered — this is a sketch of one ` +
          'that does not exist yet, or a version that has not been deployed',
      });
      continue;
    }
    if (!node.method) {
      problems.push({
        nodeId: node.id,
        detail:
          `${node.actor}@${node.version} has no Method chosen. It declares: ` +
          `${entry.operations.map((o) => o.name).join(', ') || 'none'}`,
      });
      continue;
    }
    if (!entry.operations.some((o) => o.name === node.method)) {
      problems.push({
        nodeId: node.id,
        detail:
          `${node.actor}@${node.version} declares no Method ${node.method} — it has: ` +
          `${entry.operations.map((o) => o.name).join(', ') || 'none'}`,
      });
    }
  }
  return { record, problems };
}

/**
 * The Scratch as an agent reads it: markdown, with every name resolved.
 *
 * MARKDOWN AND NOT THE RAW DOCUMENT, because the raw document is coordinates and ids and an agent
 * reading it would spend its attention reconstructing what a person can see at a glance. This is
 * the same information with the geometry dropped and the catalog joined in — the Methods' own
 * descriptions included, so the agent knows what each step DOES and not only what it is called.
 *
 * ONE RENDERING, ON THE SERVER. The page shows a canvas and the MCP returns this, and if the two
 * were derived separately they would eventually disagree about what the drawing said — which is
 * the one failure this whole feature cannot survive, because nobody would be able to tell.
 */
export function renderScratch(resolved: ResolvedScratch, catalog: readonly CatalogEntry[]): string {
  const { record, problems } = resolved;
  const { nodes, edges, notes } = record.document;
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const out: string[] = [];

  out.push(`# ${record.name || 'untitled scratch'}`);
  out.push('');
  out.push(
    'A sketch of an orchestration, drawn in kontra. Build it as a CALLER WORKFLOW — a Python ' +
      'module with an `@workflow.defn` class that dispatches the Actors below. Nothing here runs; ' +
      'this is the shape somebody drew, and the code is what will run.'
  );
  out.push('');

  if (nodes.length === 0) {
    out.push('_The canvas is empty._');
    return out.join('\n');
  }

  out.push('## What it uses');
  out.push('');
  for (const node of nodes) {
    if (node.kind === 'actor') {
      const entry = catalog.find((a) => a.name === node.actor && a.version === node.version);
      const op = entry?.operations.find((o) => o.name === node.method);
      out.push(
        `- **actor** \`${node.actor}@${node.version}\` method \`${node.method || '(none chosen)'}\`` +
          (op?.description ? ` — ${op.description}` : '')
      );
    } else if (node.kind === 'workflow') {
      out.push(`- **workflow** \`${node.file}\` — an existing caller workflow`);
    } else {
      out.push(
        `- **dataset** \`${node.name}\` — ${node.direction === 'out' ? 'written to' : 'read from'}`
      );
    }
  }
  out.push('');

  out.push('## How it flows');
  out.push('');
  if (edges.length === 0) {
    out.push('_Nothing is connected — the author placed the pieces and did not draw the order._');
  } else {
    // THE LEGEND ONLY APPEARS WHEN SOMETHING NEEDS IT. A drawing whose edges name no field renders
    // exactly the bytes it always did (ADR 0026 — this rendering is the one thing that must not
    // move), and a drawing that does name fields tells the agent how to read the dots rather than
    // leaving it to infer that `.body` is a field and not part of the Method's name.
    if (edges.some((e) => e.fromPort || e.toPort)) {
      out.push(
        'An edge written `a.field → b.field` names the FIELDS it carries: that output field of the ' +
          'first feeds that input field of the second. An edge with no fields on it says only that ' +
          'the first step feeds the second — the author did not say how.'
      );
      out.push('');
    }
    for (const e of edges) {
      const from = byId.get(e.from);
      const to = byId.get(e.to);
      out.push(
        `- ${label(from, e.fromPort)} → ${label(to, e.toPort)}${e.label ? `  _(${e.label})_` : ''}`
      );
    }
  }
  out.push('');

  if (notes.length > 0) {
    out.push('## What the author said');
    out.push('');
    // VERBATIM, and pinned notes name what they are pinned to. These are the things a person knows
    // and a schema does not — "page 200 at a time", "isolate, do not fail the run" — and they are
    // what makes generated code right rather than merely valid.
    for (const n of notes) {
      const on = n.on ? byId.get(n.on) : undefined;
      out.push(on ? `- on ${label(on)}: ${n.text}` : `- ${n.text}`);
    }
    out.push('');
  }

  if (problems.length > 0) {
    out.push('## What does not resolve');
    out.push('');
    out.push(
      'The drawing names these and the catalog does not have them. Do not invent them: either ' +
        'they are yet to be built, or the sketch is wrong — say which you think it is.'
    );
    out.push('');
    for (const p of problems) out.push(`- ${p.detail}`);
    out.push('');
  }

  return out.join('\n');
}

/**
 * How one node is named in the spec — the same words the canvas puts on it.
 *
 * `port` is the FIELD one end of an edge attached to, and it goes inside the name, after a dot, the
 * way the drawing reads it: `fetch.body`. Nothing else in a spec line is dotted onto the end of a
 * name, and a node named without a port renders the string it rendered before ports existed.
 */
function label(node: ScratchNode | undefined, port?: string): string {
  if (!node) return '`?`';
  const field = port ? `.${port}` : '';
  if (node.kind === 'actor') {
    return `\`${node.actor}@${node.version}.${node.method || '?'}${field}\``;
  }
  if (node.kind === 'workflow') return `\`${node.file}${field}\``;
  return `\`${node.name}${field}\` (dataset)`;
}
