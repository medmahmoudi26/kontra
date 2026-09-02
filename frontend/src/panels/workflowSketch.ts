/**
 * One workflow's sketch: what it is drawn out of, and which of those things exist.
 *
 * SCRATCH HAD NO SUBJECT, WHICH IS WHY NOBODY OPENED IT. It was a top-level surface holding a list
 * of drawings named by hand, so "what was this workflow meant to be" was a search through nine
 * sketches rather than a tab on the thing itself. It is a tab now, keyed to the workflow
 * (`workflowScratchId`), and this module is the reading that tab draws: the pieces the sketch
 * names, split by whether this installation actually has them.
 *
 * NOTHING HERE RUNS AND NOTHING HERE CAN (ADR 0026). There is no dispatch in this file, no run id,
 * no queue — a sketch is INPUT to writing the caller workflow, and the workflow is what runs. The
 * canvas ADR 0023 §12 removed was the execution model; the mistake that distinction exists to
 * prevent is a play button on a node, and the way to not make it is for the surface to have nothing
 * to press it with.
 *
 * THE SPLIT IS THE WHOLE POINT, and it is why an author can draw an Actor that does not exist. A
 * workflow is designed before its parts are built: you sketch `nscheck.delegation → nscheck.ask`,
 * see the shape, and then go and write the two Methods. A surface that refused the undrawn half
 * would be one nobody could think in, and one that drew it identically to the half that is deployed
 * would be lying about what is ready. So every piece carries a standing and a sentence, and the two
 * groups are drawn apart.
 *
 * THE SENTENCE IS THE SERVER'S OWN wherever the server has one. `resolveScratch` (`src/scratch.ts`)
 * already decides what a node claims that the catalog does not have, and that sentence is what
 * travels to the agent in the spec — so the author reads exactly what the agent will read.
 * `scratchInspect.ts` states the same rule for the same reason; this module reuses its readings
 * rather than resolving anything a second time.
 *
 * AND "NOT ASKED YET" IS NEVER DRAWN AS "NOT THERE". The lake listing polls and the workflow
 * listing is fetched once on mount, so before either answers an empty array is all the page holds —
 * and reading that as "no such Dataset exists" is a claim made without having looked. Those pieces
 * stay in the group of things the sketch uses, with a sentence saying nobody has looked, because
 * `yet to be built` is an assertion and this module may only make it from evidence.
 */

import type { ScratchDocument, ScratchNode, ScratchNote } from '../run/api';
import { nodeSlot, noteSlot } from './scratchFlow';
import {
  nodeLabel,
  readScratchNode,
  type ScratchCatalogue,
  type ScratchReading,
} from './scratchInspect';

/* ───────────────────────────── what the tab is looking at ───────────────────────────── */

/**
 * Whether this workflow has a sketch at all.
 *
 * FOUR STATES, AND THREE OF THEM ARE NOT ERRORS. `unread` is before the fetch answered, `none` is a
 * workflow nobody has drawn one for — the ordinary case, and the one the whole surface has to open
 * on gracefully — and `drawn` is a document. Only `unreadable` is a fault, and it is kept apart
 * from `none` precisely because a tab that said "no sketch yet" over an unreachable appliance would
 * invite somebody to draw a second one on top of the one that is already stored.
 */
export type SketchState =
  | { state: 'unread' }
  | { state: 'none' }
  | {
      state: 'drawn';
      document: ScratchDocument;
      /** When the store last took it. `null` for a drawing that has only ever been on screen — the
       *  first piece placed on a workflow that had no sketch is `drawn` and unsaved, and a bar that
       *  read `saved 01/01/1970` off a zero would be worse than saying nothing. */
      updatedAt: number | null;
    }
  | { state: 'unreadable'; detail: string };

/** Nothing drawn. Shared so "no sketch" and "a sketch somebody emptied" are the same document. */
export const NO_SKETCH: ScratchDocument = { nodes: [], edges: [], notes: [] };

/** The document the tab is editing, whatever the state says. A workflow with no sketch is one whose
 *  sketch is empty — which is what makes drawing the first node on it the same gesture as drawing
 *  the ninth, rather than a "create" somebody has to press first. */
export function sketchDocument(sketch: SketchState): ScratchDocument {
  return sketch.state === 'drawn' ? sketch.document : NO_SKETCH;
}

/* ───────────────────────────── one piece of the drawing ───────────────────────────── */

/**
 * How one thing the sketch names stands against what this installation has.
 *
 *  - `here`    the catalog, the workflow listing or the lake has it.
 *  - `unbuilt` it has been looked for and is not there. NOT a fault: this is how somebody works out
 *              what to build, and it is the state the whole tab exists to make drawable.
 *  - `unread`  nothing has answered yet, so nothing is being claimed either way.
 */
export type SketchStanding = 'here' | 'unbuilt' | 'unread';

export interface SketchPiece {
  node: ScratchNode;
  /** The full reading, so the view can draw the node component the canvas draws. */
  reading: ScratchReading;
  standing: SketchStanding;
  /**
   * One sentence about this piece, addressed to the author.
   *
   * Empty for a piece that resolves cleanly — there is nothing to say about an Actor that is
   * deployed and declares the Method the node names, and a surface that said something anyway
   * would train its reader to skip the line that matters.
   */
  detail: string;
}

/** One edge, in words rather than as a curve — see the header of `WorkflowSketch.tsx` for why this
 *  tab draws sentences. Each end is `nodeLabel`'s, the words the canvas and the inspector already
 *  share; the SPEC an agent reads is `renderScratch`'s and is not derived here. */
export interface SketchLine {
  id: string;
  from: string;
  to: string;
  /** The author's own word for the edge — "per page", "only the failures" — carried verbatim. */
  label: string;
}

export interface SketchReadingResult {
  /** Everything the installation has, plus everything nothing has looked for yet. */
  uses: SketchPiece[];
  /** Everything that has been looked for and is not there — the half that is yet to be built. */
  unbuilt: SketchPiece[];
  lines: SketchLine[];
  notes: ScratchNote[];
  /** No nodes and no notes. A workflow whose sketch is this reads as having none. */
  empty: boolean;
}

/**
 * Read a sketch against what this installation has.
 *
 * IT RESOLVES NOTHING ITSELF. Every standing below comes out of `readScratchNode`, which is the
 * same call the canvas derives its ports from and the inspector draws its panel from — a second
 * lookup here would be a second opinion about what is deployed, arriving at a different moment and
 * with nothing to say which was right.
 */
export function readSketch(doc: ScratchDocument, cat: ScratchCatalogue): SketchReadingResult {
  const uses: SketchPiece[] = [];
  const unbuilt: SketchPiece[] = [];

  for (const node of doc.nodes) {
    const piece = readPiece(node, cat);
    (piece.standing === 'unbuilt' ? unbuilt : uses).push(piece);
  }

  const byId = new Map(doc.nodes.map((n) => [n.id, n]));
  const lines = doc.edges.map((e) => ({
    id: e.id,
    from: endLabel(byId.get(e.from), e.fromPort),
    to: endLabel(byId.get(e.to), e.toPort),
    label: e.label ?? '',
  }));

  return { uses, unbuilt, lines, notes: doc.notes, empty: doc.nodes.length === 0 && doc.notes.length === 0 };
}

/**
 * One node's standing, per kind.
 *
 * AN ACTOR THE CATALOG DOES NOT HAVE AND A METHOD IT DOES NOT DECLARE ARE THE SAME ANSWER HERE, and
 * that is deliberate rather than a shortcut: both are code somebody still has to write, and both
 * carry `resolveScratch`'s own sentence, which already says which of the two it is and in the words
 * the agent will read. Splitting them into two groups on screen would be two headings for one
 * question — "is this built?" — answered `no` in both.
 *
 * A METHOD NOT PICKED YET IS NOT UNBUILT. The Actor is deployed; the author has not said which of
 * its Methods this step is (`ScratchActorNode.method` is allowed to be empty precisely so that a
 * half-drawn sketch saves). Filing that under "yet to be built" would claim something about the
 * installation on the strength of something about the drawing.
 *
 * A WORKFLOW FILE ON DISK IS BUILT EVEN IF NOTHING HAS SERVED IT. `unregistered` means no worker
 * has registered a contract, which is a fact about what is RUNNING, and this tab is about what
 * EXISTS. A file the listing does not carry is the one that is yet to be written — and before the
 * listing has answered at all, neither can be said.
 */
function readPiece(node: ScratchNode, cat: ScratchCatalogue): SketchPiece {
  const reading = readScratchNode(node, cat);

  if (reading.kind === 'actor') {
    const { standing, node: actor } = reading;
    if (standing.state === 'unknown-actor' || standing.state === 'unknown-method') {
      return { node, reading, standing: 'unbuilt', detail: standing.detail };
    }
    if (standing.state === 'unchosen') {
      return {
        node,
        reading,
        standing: 'here',
        detail:
          `${actor.actor}@${actor.version} is deployed and no Method is chosen yet — ` +
          `it declares: ${reading.methods.map((o) => o.name).join(', ') || 'none'}`,
      };
    }
    return { node, reading, standing: 'here', detail: '' };
  }

  if (reading.kind === 'workflow') {
    const file = reading.node.file;
    if (!reading.listed) {
      return {
        node,
        reading,
        standing: 'unread',
        detail: `nobody has listed .kontra/workflows/ yet, so nothing is known about ${file}`,
      };
    }
    if (!reading.file) {
      return {
        node,
        reading,
        standing: 'unbuilt',
        detail: `no file called ${file} is registered here — this is a caller workflow yet to be written`,
      };
    }
    return {
      node,
      reading,
      standing: 'here',
      detail:
        reading.standing.state === 'described'
          ? ''
          : `${file} is on this disk; no worker has registered its contract, so its input and output are not known here`,
    };
  }

  const dataset = reading.node.name;
  if (reading.standing.state === 'unread') {
    return {
      node,
      reading,
      standing: 'unread',
      detail: `the lake listing has not answered yet, so nothing is known about ${dataset}`,
    };
  }
  if (reading.standing.state === 'absent') {
    return {
      node,
      reading,
      standing: 'unbuilt',
      detail: `nothing in the lake is called ${dataset} — this is a Dataset the workflow is yet to write`,
    };
  }
  return { node, reading, standing: 'here', detail: '' };
}

/** One end of an edge, in the words the node carries — with the FIELD after a dot when the edge
 *  names one, which is how a drawing with typed ports reads (`fetch.body`). */
function endLabel(node: ScratchNode | undefined, port: string | undefined): string {
  if (!node) return '?';
  return port ? `${nodeLabel(node)}.${port}` : nodeLabel(node);
}

/* ───────────────────────────── drawing on it ───────────────────────────── */

/**
 * Ids are only unique WITHIN a document, so they are minted from a counter and the clock rather
 * than from crypto. A sketch is keyed to one workflow (`workflowScratchId`) and nothing joins
 * across two of them, so two sketches sharing `n3` is meaningless rather than a collision.
 */
let seq = 0;
export function mintSketchId(): string {
  return `n${Date.now().toString(36)}${(seq += 1).toString(36)}`;
}

/** What an author types to put an Actor on the sketch. `version` and `method` are free text on
 *  purpose: an Actor that does not exist has no catalog row to pick a version off, and a Method
 *  nobody has written has no entry to choose. Refusing either is refusing the design case. */
export interface SketchActorDraft {
  actor: string;
  version: string;
  method: string;
}

/**
 * Put one Actor on the sketch — built or not.
 *
 * NOTHING IS CHECKED AGAINST THE CATALOG HERE, and that is the feature rather than an omission.
 * `resolveScratch` reports what does not resolve, at READ time, in one sentence per node
 * (`readSketch` above puts it on the piece); a canvas that refused the node instead would be a
 * canvas nobody could design in, which is exactly what ADR 0026 says about it: reported, never
 * corrected, never refused.
 *
 * KEYS IN THE SERVER'S OWN ORDER (`id, kind, at, actor, version, method`), because what is stored is
 * `JSON.stringify` of these objects and `parseScratchDocument` rebuilds them in that order — a node
 * built any other way differs from the same node after one save-and-reload in nothing but key
 * order, which is a diff on a sketch nobody edited.
 */
export function withActor(
  doc: ScratchDocument,
  id: string,
  draft: SketchActorDraft
): ScratchDocument {
  const node: ScratchNode = {
    id,
    kind: 'actor',
    at: nodeSlot(doc.nodes.length),
    actor: draft.actor.trim(),
    version: draft.version.trim(),
    method: draft.method.trim(),
  };
  return { ...doc, nodes: [...doc.nodes, node] };
}

/** Put a free note on the sketch. The types cannot carry "page 200 at a time" or "isolate, do not
 *  fail the run", and those are exactly what make generated code right rather than merely valid. */
export function withNote(doc: ScratchDocument, id: string, text: string): ScratchDocument {
  return { ...doc, notes: [...doc.notes, { id, at: noteSlot(doc.notes.length), text }] };
}

/** Retype one note in place. Its text travels to the agent verbatim, so what is typed is what is
 *  sent. Retyping it to what it already says returns the document itself, so a focus-and-blur does
 *  not mark the sketch unsaved. */
export function withNoteText(doc: ScratchDocument, id: string, text: string): ScratchDocument {
  let touched = false;
  const notes = doc.notes.map((n) => {
    if (n.id !== id || n.text === text) return n;
    touched = true;
    return { ...n, text };
  });
  return touched ? { ...doc, notes } : doc;
}

/** What the tab says about itself in one line, above the drawing. */
export function sketchSummary(reading: SketchReadingResult): string {
  const pieces = reading.uses.length + reading.unbuilt.length;
  const parts = [`${pieces} ${pieces === 1 ? 'piece' : 'pieces'}`];
  if (reading.unbuilt.length > 0) parts.push(`${reading.unbuilt.length} yet to be built`);
  if (reading.lines.length > 0) {
    parts.push(`${reading.lines.length} ${reading.lines.length === 1 ? 'connection' : 'connections'}`);
  }
  if (reading.notes.length > 0) {
    parts.push(`${reading.notes.length} ${reading.notes.length === 1 ? 'note' : 'notes'}`);
  }
  return parts.join(' · ');
}
