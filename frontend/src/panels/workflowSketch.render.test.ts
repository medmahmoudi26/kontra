/**
 * The workflow's design tab, drawn — and the one property a reviewer should check first.
 *
 * NOTHING ON THIS TAB RUNS, AND THIS IS WHERE THAT IS PROVED. ADR 0026 exists because kontra HAD a
 * canvas that was the execution model (0023 §12): nodes were dispatches, edges were data flow, and
 * the server interpreted the drawing. This one is INPUT to writing the program, so a run, play or
 * dispatch affordance on a node is not a small regression — it is the whole distinction collapsing,
 * and it would collapse silently, because a play button looks like a feature.
 *
 * SO THE ASSERTION IS AN ALLOW-LIST AND NOT A SEARCH FOR BAD WORDS. Every interactive element in the
 * markup is pulled out and its `data-testid` is compared against the four things this tab is allowed
 * to have — save, place, note, and one remove per piece. A button added here without a name fails,
 * whatever it is called; a search for `run|play|dispatch` would pass anything spelt `▶`. The word
 * check is kept as well, over the affordances only, because the two nets catch different mistakes.
 *
 * `renderToStaticMarkup` for the reason `actorCard.test.ts` records: this suite runs in node with no
 * jsdom, and everything asserted here is text or a `data-testid`, all of which are in the markup.
 * The tab is a board rather than a `<ReactFlow>` for exactly this reason — see the component's own
 * header — so the nodes it draws are really in the string, which is what makes the allow-list mean
 * anything at all.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import type { ScratchDocument, ScratchNode } from '../run/api';
import type { CatalogActor } from '../types';
import type { ScratchCatalogue } from './scratchInspect';
import { WorkflowSketch, type WorkflowSketchProps } from './WorkflowSketch';
import type { SketchState } from './workflowSketch';

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

const cat = (over: Partial<ScratchCatalogue> = {}): ScratchCatalogue => ({
  actors: [PROBE],
  workflows: [],
  registered: [],
  datasets: [],
  datasetsAt: 1_700_000_000_000,
  columns: [{ name: 'lame', columns: [{ name: 'domain', type: 'VARCHAR' }] }],
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

/** One deployed Actor, one nobody has built, an edge between them and a note — the drawing this
 *  whole tab exists for. */
const MIXED = doc({
  nodes: [actor(), actor({ id: 'n2', actor: 'nscheck', version: '0.1.0', method: 'delegation' })],
  edges: [{ id: 'e1', from: 'a1', to: 'n2', fromPort: 'body', label: 'only the ones that answered' }],
  notes: [{ id: 'k1', at: { x: 0, y: 0 }, text: 'page 200 at a time' }],
});

function draw(over: Partial<WorkflowSketchProps> = {}): string {
  return renderToStaticMarkup(
    createElement(WorkflowSketch, {
      workflow: 'dnssweep',
      sketch: { state: 'none' },
      catalogue: cat(),
      onChange: () => undefined,
      onSave: () => undefined,
      ...over,
    })
  );
}

/* ───────────────────────────── the rule ───────────────────────────── */

/** Every `<button>`, `<a>`, `<input>` and `<select>` in the markup, whole. */
function affordances(html: string): string[] {
  return html.match(/<(?:button|a|input|select)\b[^>]*>(?:(?!<\/?(?:button|a|select)\b)[\s\S])*/g) ?? [];
}

/** The `data-testid` of each, or `''` for one that has none — which is itself a failure below. */
const named = (html: string): string[] =>
  affordances(html).map((el) => /data-testid="([^"]*)"/.exec(el)?.[1] ?? '');

describe('nothing on this tab runs', () => {
  const cases: Array<[string, SketchState]> = [
    ['a workflow with no sketch', { state: 'none' }],
    ['a sketch of Actors that exist', drawn(doc({ nodes: [actor()] }))],
    ['a sketch of Actors that do not', drawn(doc({ nodes: [actor({ id: 'n2', actor: 'nscheck', method: 'delegation' })] }))],
    ['a sketch of both, with an edge and a note', drawn(MIXED)],
  ];

  for (const [what, sketch] of cases) {
    it(`offers no run, play or dispatch affordance on ${what}`, () => {
      for (const el of affordances(draw({ sketch }))) {
        expect(el).not.toMatch(/\b(run|runs|play|dispatch|execute|launch|trigger|serve|start|kick|fire)\b/i);
        // The icon buttons a rebuild might reach for, by the names lucide gives them.
        expect(el).not.toMatch(/\b(Play|CirclePlay|Rocket|Zap|Send)\b/);
      }
    });

    it(`has only the four affordances it is allowed, on ${what}`, () => {
      // AN ALLOW-LIST, so a control added here fails until somebody names it and reads this file.
      // `delete-<id>` is `ScratchNodes.tsx`'s own remove button, which comes with the node
      // components this tab reuses; removing a piece from a drawing is not running anything.
      const allowed = /^(scratch-save|scratch-add-actor|scratch-add-version|scratch-add-method|scratch-add-place|scratch-add-note|delete-[a-z0-9]+)$/;
      for (const id of named(draw({ sketch }))) expect(id).toMatch(allowed);
    });
  }

  it('has no affordance at all when the page hands it nowhere to send one', () => {
    // A read-only tab draws a drawing and no controls, rather than buttons that do nothing — which
    // is how an operator learns not to trust the controls on a surface.
    const html = draw({ sketch: drawn(MIXED), onChange: undefined, onSave: undefined });
    expect(named(html)).toEqual([]);
    expect(html).toContain('data-testid="scratch-piece-a1"');
  });
});

/* ───────────────────────────── the states ───────────────────────────── */

describe('a workflow with no sketch', () => {
  it('reads as having none, not as an error', () => {
    const html = draw({ sketch: { state: 'none' } });
    expect(html).toContain('data-testid="scratch-none"');
    expect(html).toContain('No sketch has been drawn for');
    expect(html).toContain('dnssweep');
    expect(html).toContain('none drawn');
    expect(html).not.toContain('data-testid="scratch-unreadable"');
    expect(html).not.toMatch(/\b(error|failed|could not)\b/i);
  });

  it('still offers somewhere to place the first piece', () => {
    // No create to press first: the first node and the ninth are the same gesture.
    expect(draw({ sketch: { state: 'none' } })).toContain('data-testid="scratch-add-place"');
  });

  it('is not what a sketch that could not be READ looks like', () => {
    // Drawing "no sketch yet" over an unreachable appliance invites somebody to start again on top
    // of a document that is already stored.
    const html = draw({ sketch: { state: 'unreadable', detail: '503 Service Unavailable' } });
    expect(html).toContain('data-testid="scratch-unreadable"');
    expect(html).toContain('503 Service Unavailable');
    expect(html).not.toContain('data-testid="scratch-none"');
    // And it does not offer to draw over it either.
    expect(html).not.toContain('data-testid="scratch-add-place"');
  });

  it('says it is still reading rather than that there is nothing', () => {
    const html = draw({ sketch: { state: 'unread' } });
    expect(html).toContain('data-testid="scratch-loading"');
    expect(html).not.toContain('data-testid="scratch-none"');
  });
});

describe('a sketch of Actors that exist', () => {
  it('draws them with the canvas’s own node components, under what the workflow uses', () => {
    const html = draw({ sketch: drawn(doc({ nodes: [actor()] })) });
    expect(html).toContain('data-testid="scratch-uses"');
    expect(html).not.toContain('data-testid="scratch-unbuilt"');
    // `ScratchNodes.tsx`'s markup, not a second set of boxes built to fit here: the node's own test
    // id, its kind, its declared fields and the handle ids an edge lands on.
    expect(html).toContain('data-testid="scratch-node-a1"');
    expect(html).toContain('data-kind="actor"');
    expect(html).toContain('data-testid="port-in-a1-url"');
    expect(html).toContain('data-testid="port-out-a1-body"');
    expect(html).toContain('data-handleid="out:field:body"');
    expect(html).toContain('data-standing="here"');
  });

  it('says nothing about a piece that resolves cleanly', () => {
    expect(draw({ sketch: drawn(doc({ nodes: [actor()] })) })).not.toContain(
      'data-testid="scratch-detail-a1"'
    );
  });

  it('says the order was not drawn rather than showing an empty list', () => {
    const html = draw({ sketch: drawn(doc({ nodes: [actor()] })) });
    expect(html).toContain('data-testid="scratch-flow-none"');
    expect(html).toContain('Nothing is connected');
  });
});

describe('a sketch of Actors that do not exist yet', () => {
  const unbuilt = drawn(doc({ nodes: [actor({ id: 'n2', actor: 'nscheck', method: 'delegation' })] }));

  it('draws them, rather than refusing them', () => {
    const html = draw({ sketch: unbuilt });
    expect(html).toContain('data-testid="scratch-node-n2"');
    expect(html).toContain('nscheck');
  });

  it('puts them in their own group, visibly apart from the ones that are here', () => {
    const html = draw({ sketch: drawn(MIXED) });
    expect(html).toContain('data-testid="scratch-uses"');
    expect(html).toContain('data-testid="scratch-unbuilt"');
    expect(html).toContain('Yet to be built');
    // THE DIFFERENCE IS ON THE PIECE, not on the node: what a node IS does not change because
    // somebody deployed something this morning, and a node that redrew itself amber would be a
    // drawing that edited itself.
    expect(html).toContain('data-testid="scratch-piece-a1" data-standing="here"');
    expect(html).toContain('data-testid="scratch-piece-n2" data-standing="unbuilt"');
    expect(html).toMatch(/<div class="[^"]*border-dashed[^"]*" data-testid="scratch-piece-n2"/);
  });

  it('carries the server’s own sentence about each one', () => {
    // The author reads exactly what the agent reading the spec will read.
    const html = draw({ sketch: unbuilt });
    expect(html).toContain('data-testid="scratch-detail-n2"');
    expect(html).toContain('no Actor nscheck@0.1.0 is registered');
    expect(html).toContain('does not exist yet');
  });

  it('says the group is not a fault', () => {
    expect(draw({ sketch: unbuilt })).toContain('this is how somebody works out what to write next');
  });
});

describe('what else the drawing carries', () => {
  it('names both ends of an edge and the author’s own word for it', () => {
    const html = draw({ sketch: drawn(MIXED) });
    expect(html).toContain('data-testid="scratch-line-e1"');
    expect(html).toContain('probe@0.1.0.fetch().body');
    expect(html).toContain('nscheck@0.1.0.delegation()');
    expect(html).toContain('only the ones that answered');
  });

  it('draws a note with the same component the canvas uses, verbatim', () => {
    const html = draw({ sketch: drawn(MIXED) });
    expect(html).toContain('data-testid="scratch-note-k1"');
    expect(html).toContain('page 200 at a time');
  });

  it('says what the sketch is, and when it was last stored', () => {
    const html = draw({ sketch: drawn(MIXED) });
    expect(html).toContain('2 pieces · 1 yet to be built · 1 connection · 1 note');
    expect(html).toContain('data-testid="scratch-saved-at"');
  });

  it('does not claim a save date for a drawing that has never been stored', () => {
    const html = draw({ sketch: { state: 'drawn', document: MIXED, updatedAt: null }, dirty: true });
    expect(html).not.toContain('data-testid="scratch-saved-at"');
    expect(html).toContain('data-testid="scratch-dirty"');
  });

  it('keeps the drawing on screen when a save did not land', () => {
    const html = draw({ sketch: drawn(MIXED), saveError: 'disk full', dirty: true });
    expect(html).toContain('data-testid="scratch-save-error"');
    expect(html).toContain('disk full');
    expect(html).toContain('data-testid="scratch-node-a1"');
  });
});

describe('the tab is about the workflow', () => {
  it('says so, and takes no run', () => {
    // The one reading on this page that is NOT scoped to the open conversation. A sketch is drawn
    // before the first run and is the same drawing after the hundredth.
    const html = draw({ sketch: drawn(MIXED) });
    expect(html).toContain('data-workflow="dnssweep"');
    expect(html).toContain('About the workflow, not about the open run');
  });

  it('uses none of the dead execution-editor stylesheet classes', () => {
    // `.canvas`, `.actor-node`, `.io-row`, `.mapping-table`, `.link-panel`, `.seed-table` and
    // `.panel.inspector` are what the interpreter left behind in `styles.css`. Scratch touches none
    // of them, and a rebuild that reached for one would be reaching for the surface that ran things.
    const html = draw({ sketch: drawn(MIXED) });
    for (const dead of ['canvas', 'actor-node', 'io-row', 'mapping-table', 'link-panel', 'seed-table', 'inspector']) {
      expect(html).not.toMatch(new RegExp(`class="[^"]*\\b${dead}\\b`));
    }
  });
});
