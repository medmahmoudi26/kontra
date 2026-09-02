/**
 * What a workflow's sketch SAYS about itself: which pieces this installation has, which are yet to
 * be built, and which nobody has looked for.
 *
 * THE SPLIT IS THE FEATURE. A sketch is how somebody works out what to build, so drawing an Actor
 * that does not exist has to be an ordinary thing to do — and the surface has to be able to say
 * which half is which without refusing either. These tests pin exactly that, and the one thing that
 * would quietly hollow it out: `yet to be built` said about a catalog nobody has read.
 *
 * THE KEY IS PINNED HERE TOO. A sketch is attached to its workflow by its store id
 * (`workflowScratchId`), which is what makes it reload with the thing it is a sketch of — so the
 * derivation is a contract between the browser and `db/repo.ts`'s upsert, not an implementation
 * detail of either.
 */

import { describe, expect, it } from 'vitest';
import { scratchWorkflow, workflowScratchId } from '@core/scratch';

import type { ScratchDocument, ScratchNode } from '../run/api';
import type { CatalogActor } from '../types';
import type { ScratchCatalogue } from './scratchInspect';
import {
  mintSketchId,
  readSketch,
  sketchDocument,
  sketchSummary,
  withActor,
  withNote,
  withNoteText,
  type SketchState,
} from './workflowSketch';

const PROBE: CatalogActor = {
  key: 'probe@0.1.0',
  name: 'probe',
  version: '0.1.0',
  schemaVersion: '1',
  operations: [
    {
      name: 'fetch',
      description: 'one request, no cache',
      input: { properties: { url: { type: 'string' } }, required: ['url'] },
      output: { properties: { body: { type: 'string' }, status: { type: 'integer' } } },
    },
    { name: 'facts' },
  ],
};

/** Everything the tab reads, as the page holds it. `datasetsAt` non-zero means the lake HAS
 *  answered — with nothing in it, which is a real installation and not a missing fixture. */
const cat = (over: Partial<ScratchCatalogue> = {}): ScratchCatalogue => ({
  actors: [PROBE],
  workflows: [],
  registered: [],
  datasets: [],
  datasetsAt: 1_700_000_000_000,
  columns: [],
  ...over,
});

const actor = (over: Partial<Extract<ScratchNode, { kind: 'actor' }>> = {}): ScratchNode => ({
  id: 'a1',
  kind: 'actor',
  at: { x: 0, y: 0 },
  actor: 'probe',
  version: '0.1.0',
  method: 'fetch',
  ...over,
});

const doc = (over: Partial<ScratchDocument> = {}): ScratchDocument => ({
  nodes: [],
  edges: [],
  notes: [],
  ...over,
});

const drawn = (document: ScratchDocument): SketchState => ({
  state: 'drawn',
  document,
  updatedAt: 1_700_000_000_000,
});

describe('the key a sketch is attached to its workflow by', () => {
  it('derives from the workflow name and reads back to it', () => {
    expect(workflowScratchId('dnssweep')).toBe('workflow:dnssweep');
    expect(scratchWorkflow(workflowScratchId('dnssweep'))).toBe('dnssweep');
  });

  it('does not claim a subject for a sketch drawn before sketches had one', () => {
    // The retired surface minted `randomUUID`s. Those are drawings about nothing and stay that way
    // — reading a workflow out of one would attach a design to a workflow nobody drew it for.
    expect(scratchWorkflow('9f0b3f1e-1c2d-4a5b-8e9f-0a1b2c3d4e5f')).toBeNull();
    expect(scratchWorkflow('workflow:')).toBeNull();
  });
});

describe('a workflow with no sketch', () => {
  it('reads as having none rather than as an error', () => {
    const reading = readSketch(sketchDocument({ state: 'none' }), cat());
    expect(reading.empty).toBe(true);
    expect(reading.uses).toEqual([]);
    expect(reading.unbuilt).toEqual([]);
    expect(reading.lines).toEqual([]);
  });

  it('is the same empty document whether nothing was stored or nothing has been read yet', () => {
    // Which is what makes placing the FIRST piece the same gesture as placing the ninth: there is
    // no create to press before a workflow can be designed.
    expect(sketchDocument({ state: 'none' })).toEqual(sketchDocument({ state: 'unread' }));
  });

  it('is empty for a stored sketch somebody emptied, too', () => {
    expect(readSketch(sketchDocument(drawn(doc())), cat()).empty).toBe(true);
  });
});

describe('a sketch of things that exist', () => {
  it('files a deployed Actor and a Method it declares under what the workflow uses', () => {
    const reading = readSketch(doc({ nodes: [actor()] }), cat());
    expect(reading.unbuilt).toEqual([]);
    expect(reading.uses.map((p) => p.standing)).toEqual(['here']);
    // NOTHING IS SAID ABOUT A PIECE THAT RESOLVES. A sentence on every node is a surface whose
    // reader learns to skip the line that matters.
    expect(reading.uses[0]?.detail).toBe('');
  });

  it('does not call an Actor unbuilt because the author has not picked a Method yet', () => {
    // `method` is allowed to be empty precisely so a half-drawn sketch saves. That is a fact about
    // the DRAWING; filing it under "yet to be built" would make it a claim about the installation.
    const reading = readSketch(doc({ nodes: [actor({ method: '' })] }), cat());
    expect(reading.unbuilt).toEqual([]);
    expect(reading.uses[0]?.standing).toBe('here');
    expect(reading.uses[0]?.detail).toContain('no Method is chosen yet');
    expect(reading.uses[0]?.detail).toContain('fetch, facts');
  });

  it('names each end of an edge, with the field it carries', () => {
    const nodes = [actor(), actor({ id: 'a2', method: 'facts' })];
    const reading = readSketch(
      doc({ nodes, edges: [{ id: 'e1', from: 'a1', to: 'a2', fromPort: 'body', label: 'per page' }] }),
      cat()
    );
    expect(reading.lines).toEqual([
      { id: 'e1', from: 'probe@0.1.0.fetch().body', to: 'probe@0.1.0.facts()', label: 'per page' },
    ]);
  });

  it('says nothing is connected rather than nothing is there', () => {
    const reading = readSketch(doc({ nodes: [actor()] }), cat());
    expect(reading.empty).toBe(false);
    expect(reading.lines).toEqual([]);
  });
});

describe('a sketch of things that do not exist yet', () => {
  it('files an Actor nobody has deployed under what is yet to be built', () => {
    const node = actor({ id: 'n1', actor: 'nscheck', version: '0.1.0', method: 'delegation' });
    const reading = readSketch(doc({ nodes: [node] }), cat());
    expect(reading.uses).toEqual([]);
    expect(reading.unbuilt.map((p) => p.node.id)).toEqual(['n1']);
    // THE SERVER'S OWN SENTENCE, which is the one the agent gets in the spec — so the author reads
    // exactly what the agent will read rather than a second account of the same sketch.
    expect(reading.unbuilt[0]?.detail).toContain('no Actor nscheck@0.1.0 is registered');
    expect(reading.unbuilt[0]?.detail).toContain('does not exist yet');
  });

  it('treats a Method the deployed Actor does not declare the same way, and says which it is', () => {
    // Both are code somebody still has to write. Two headings for one question — "is this built?" —
    // answered `no` in both would be two headings too many; the sentence carries the difference.
    const reading = readSketch(doc({ nodes: [actor({ method: 'ask' })] }), cat());
    expect(reading.unbuilt).toHaveLength(1);
    expect(reading.unbuilt[0]?.detail).toContain('declares no Method ask');
    expect(reading.unbuilt[0]?.detail).toContain('fetch, facts');
  });

  it('draws the built and the unbuilt halves of one sketch apart', () => {
    const nodes = [actor(), actor({ id: 'n2', actor: 'nscheck', method: 'delegation' })];
    const reading = readSketch(doc({ nodes }), cat());
    expect(reading.uses.map((p) => p.node.id)).toEqual(['a1']);
    expect(reading.unbuilt.map((p) => p.node.id)).toEqual(['n2']);
  });

  it('calls a Dataset the lake has never held one the workflow is yet to write', () => {
    const node: ScratchNode = { id: 'd1', kind: 'dataset', at: { x: 0, y: 0 }, name: 'lame', direction: 'out' };
    const reading = readSketch(doc({ nodes: [node] }), cat());
    expect(reading.unbuilt[0]?.detail).toContain('nothing in the lake is called lame');
  });

  it('calls a caller workflow no listing carries one yet to be written', () => {
    const node: ScratchNode = { id: 'w1', kind: 'workflow', at: { x: 0, y: 0 }, file: 'nscheck' };
    const reading = readSketch(doc({ nodes: [node] }), cat());
    expect(reading.unbuilt[0]?.detail).toContain('yet to be written');
  });
});

describe('what has not been looked for', () => {
  // THE ONE WAY THIS SPLIT GOES QUIETLY WRONG. Every listing on this page starts as an empty array
  // and fills when a fetch answers, so reading emptiness as absence turns every piece of every
  // sketch into "yet to be built" for the first second the tab is open — and an author who has
  // learnt that the amber group is noise stops reading the one entry in it that is real.
  it('is never called yet to be built: the lake, before it answers', () => {
    const node: ScratchNode = { id: 'd1', kind: 'dataset', at: { x: 0, y: 0 }, name: 'lame', direction: 'in' };
    const reading = readSketch(doc({ nodes: [node] }), cat({ datasetsAt: 0 }));
    expect(reading.unbuilt).toEqual([]);
    expect(reading.uses[0]?.standing).toBe('unread');
    expect(reading.uses[0]?.detail).toContain('has not answered yet');
  });

  it('is never called yet to be built: the workflow listing, before it answers', () => {
    const node: ScratchNode = { id: 'w1', kind: 'workflow', at: { x: 0, y: 0 }, file: 'nscheck' };
    const reading = readSketch(doc({ nodes: [node] }), cat({ workflows: null }));
    expect(reading.unbuilt).toEqual([]);
    expect(reading.uses[0]?.standing).toBe('unread');
  });

  it('does not call a workflow file unbuilt merely because no worker is serving it', () => {
    // `unregistered` is a fact about what is RUNNING. This tab is about what EXISTS.
    const node: ScratchNode = { id: 'w1', kind: 'workflow', at: { x: 0, y: 0 }, file: 'nscheck' };
    const reading = readSketch(
      doc({ nodes: [node] }),
      cat({ workflows: [{ name: 'nscheck', path: 'nscheck.py' } as never] })
    );
    expect(reading.unbuilt).toEqual([]);
    expect(reading.uses[0]?.detail).toContain('no worker has registered its contract');
  });
});

describe('drawing on it', () => {
  it('places an Actor nobody has built, without checking anything', () => {
    // Reported, never corrected, never refused (ADR 0026) — a canvas that argued with a name would
    // be a canvas nobody could design in.
    const next = withActor(doc(), 'n1', { actor: 'nscheck', version: '0.2.0', method: 'delegation' });
    expect(next.nodes).toEqual([
      { id: 'n1', kind: 'actor', at: { x: 60, y: 48 }, actor: 'nscheck', version: '0.2.0', method: 'delegation' },
    ]);
    // The server's own key order, because what is stored is `JSON.stringify` of this and
    // `parseScratchDocument` rebuilds it in that order — any other order is a diff on a sketch
    // nobody edited.
    expect(Object.keys(next.nodes[0] as object)).toEqual(['id', 'kind', 'at', 'actor', 'version', 'method']);
  });

  it('trims what was typed and staggers the next node rather than stacking it', () => {
    const one = withActor(doc(), 'n1', { actor: ' probe ', version: ' 0.1.0 ', method: ' fetch ' });
    const two = withActor(one, 'n2', { actor: 'nscheck', version: '0.1.0', method: 'ask' });
    expect(one.nodes[0]).toMatchObject({ actor: 'probe', version: '0.1.0', method: 'fetch' });
    expect(two.nodes[1]?.at).not.toEqual(two.nodes[0]?.at);
  });

  it('adds a note and retypes it in place', () => {
    const one = withNote(doc(), 'k1', '');
    const two = withNoteText(one, 'k1', 'page 200 at a time');
    expect(two.notes[0]?.text).toBe('page 200 at a time');
    // Retyping it to what it already says returns the document itself, so a focus-and-blur does not
    // mark the sketch unsaved.
    expect(withNoteText(two, 'k1', 'page 200 at a time')).toBe(two);
  });

  it('mints ids that do not collide inside one document', () => {
    const ids = new Set(Array.from({ length: 50 }, () => mintSketchId()));
    expect(ids.size).toBe(50);
  });
});

describe('the line the tab says about itself', () => {
  it('counts the pieces, and says how many are yet to be built', () => {
    const nodes = [actor(), actor({ id: 'n2', actor: 'nscheck', method: 'delegation' })];
    const reading = readSketch(
      doc({ nodes, edges: [{ id: 'e1', from: 'a1', to: 'n2' }], notes: [{ id: 'k1', at: { x: 0, y: 0 }, text: 'isolate' }] }),
      cat()
    );
    expect(sketchSummary(reading)).toBe('2 pieces · 1 yet to be built · 1 connection · 1 note');
  });

  it('says nothing about an unbuilt half that is not there', () => {
    expect(sketchSummary(readSketch(doc({ nodes: [actor()] }), cat()))).toBe('1 piece');
  });
});
