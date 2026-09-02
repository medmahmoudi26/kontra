/**
 * A node, drawn — with a handle per field down each edge.
 *
 * `renderToStaticMarkup` for the reason `actorCard.test.ts` records: this suite runs in node with no
 * jsdom, and everything asserted here is a `data-testid`, a `data-handleid` or the text on a row,
 * all of which are in the markup. The nodes are wrapped in a `ReactFlowProvider` because React
 * Flow's `Handle` reads its store on the way to rendering — that provider is the only thing being
 * borrowed, and nothing here lays anything out.
 *
 * WHAT IS PINNED IS THE HANDLE IDS AND THE SENTENCES. The ids are the contract between three
 * modules: the node draws them, `scratchFlow.ts` names the handle each stored edge lands on, and
 * `handleField` reads a field back out of a connection. React Flow silently drops an edge whose
 * named handle does not exist — error 008, no line — so a spelling that drifts here is edges that
 * are simply not on the canvas, with nothing in the document to say why.
 *
 * AND THAT A NODE WITH NO FIELDS READS AS A NODE. A Method declaring neither `takes=` nor `emits=`,
 * a workflow annotated `dict`, a Method not yet chosen: all ordinary, all of them a node whose only
 * port is the whole node, and none of them a node anybody should delete and draw again.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { ReactFlowProvider } from '@xyflow/react';
import type { ScratchNode } from '../run/api';
import type { CatalogActor } from '../types';
import { ScratchPortsProvider, scratchNodeTypes } from './ScratchNodes';
import { readScratchNode, type ScratchCatalogue } from './scratchInspect';
import { documentPorts, nodePorts } from './scratchPorts';

const PROBE: CatalogActor = {
  key: 'probe@0.1.0',
  name: 'probe',
  version: '0.1.0',
  schemaVersion: '1',
  operations: [
    {
      name: 'fetch',
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

/** One node, drawn with the ports the canvas would hand it. */
function draw(node: ScratchNode, sources = cat(), ports = nodePorts(readScratchNode(node, sources))): string {
  const Component = scratchNodeTypes[node.kind];
  return renderToStaticMarkup(
    createElement(
      ReactFlowProvider,
      null,
      createElement(
        ScratchPortsProvider,
        { value: new Map([[node.id, ports]]) },
        createElement(Component, {
          id: node.id,
          data: { node },
          // React Flow hands a node component a dozen more props than these; the components read
          // `id`, `data` and `selected` and nothing else, which is what makes this drawable at all.
          type: node.kind,
          dragging: false,
          zIndex: 0,
          isConnectable: true,
          positionAbsoluteX: 0,
          positionAbsoluteY: 0,
        } as never)
      )
    )
  );
}

describe('an Actor node', () => {
  it('puts its declared inputs on the left edge and its outputs on the right', () => {
    const html = draw(actor());
    expect(html).toContain('data-testid="port-in-a1-url"');
    expect(html).toContain('data-testid="port-out-a1-body"');
    expect(html).toContain('data-testid="port-out-a1-status"');
    // the ids an edge lands on, on the side it lands on
    expect(html).toContain('data-handleid="in:field:url"');
    expect(html).toContain('data-handleid="out:field:body"');
    expect(html).toContain('data-handlepos="left"');
    expect(html).toContain('data-handlepos="right"');
  });

  it('names each field and shows its declared type', () => {
    const html = draw(actor());
    expect(html).toContain('url');
    expect(html).toContain('string');
    expect(html).toContain('integer');
  });

  it('keeps a whole-node port on each side even when it has fields', () => {
    // Three ordinary states need it, and an edge to it is how "connected, I have not said how yet"
    // is drawn — so it is not a fallback that appears when something is missing.
    const html = draw(actor());
    expect(html).toContain('data-handleid="in:actor"');
    expect(html).toContain('data-handleid="out:actor"');
  });

  it('reshapes its handles when the Method changes', () => {
    // `fetch` emits `body`; `facts` declares nothing at all. The handle ids are what edges land on,
    // so this is also what `useUpdateNodeInternals` is fired for.
    const html = draw(actor({ method: 'facts' }));
    expect(html).not.toContain('out:field:body');
    expect(html).toContain('data-handleid="out:actor"');
  });

  it('reads as declaring no fields, not as broken, when it has none', () => {
    expect(draw(actor({ method: 'facts' }))).toContain('data-testid="ports-none-a1"');
    expect(draw(actor({ method: 'facts' }))).toContain('not declared');
    // and an Actor placed before the Method was decided says which of the two it is
    expect(draw(actor({ method: '' }))).toContain('no Method chosen yet');
  });

  it('draws a field only an edge names, marked, rather than dropping it', () => {
    // React Flow does not draw an edge whose named handle is missing, so a port that vanished with a
    // Method change would take the author's line off the canvas while leaving it in the document.
    const doc = {
      nodes: [actor({ method: 'facts' })],
      edges: [{ id: 'e1', from: 'a1', to: 'a1', fromPort: 'body' }],
      notes: [],
    };
    const ports = documentPorts(doc, cat()).get('a1');
    if (!ports) throw new Error('documentPorts dropped the only node it was given');
    const html = draw(actor({ method: 'facts' }), cat(), ports);
    expect(html).toContain('data-handleid="out:field:body"');
    expect(html).toContain('not declared');
  });
});

describe('a Dataset node', () => {
  const dataset: ScratchNode = {
    id: 'd1',
    kind: 'dataset',
    at: { x: 0, y: 0 },
    name: 'lame',
    direction: 'in',
  };

  it('draws its real columns as ports, on both sides', () => {
    // Writing into `domain` and reading out of `domain` are one column seen from two ends.
    const html = draw(dataset);
    expect(html).toContain('data-testid="port-in-d1-domain"');
    expect(html).toContain('data-testid="port-out-d1-domain"');
    expect(html).toContain('VARCHAR');
  });

  it('has no IN/OUT control on it — the direction is which side you attached to', () => {
    const html = draw(dataset);
    expect(html).toContain('→ read from');
    expect(html).not.toContain('data-testid="direction-d1"');
  });

  it('says the lake has not answered rather than showing a Dataset with no columns', () => {
    expect(draw(dataset, cat({ columns: null }))).toContain(
      'the lake has not answered with its columns yet'
    );
  });
});
