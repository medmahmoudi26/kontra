/**
 * What the inspector actually DRAWS for each of the three kinds, rendered.
 *
 * `renderToStaticMarkup` for the reason `actorCard.test.ts` records: this suite runs in node with no
 * jsdom, and every assertion here is about text and `data-testid`, both of which are in the markup.
 * It is also the check that the panel imports nothing that touches the DOM at module load — the
 * failure mode being `ReferenceError: self is not defined` for a whole file.
 *
 * WHAT IS PINNED IS THE SENTENCES, because that is what this panel is for. A canvas can say
 * `probe@0.1.0 .fetch()`; only this can say what `fetch` is FOR and what it moves — and the cases
 * where it must say something rather than nothing are exactly the ones a rendering quietly gets
 * wrong: an author who wrote no description, a node the catalog cannot resolve, and a Method change
 * that left edges pointing at a signature that is gone.
 *
 * AND THE METHOD ROWS ARE THE ACTORS PAGE'S OWN (`MethodContract.tsx`). The `method-doc-…` test ids
 * asserted below are that module's, which is what makes this a check that the two surfaces draw one
 * rendering rather than two that agree today.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { DatasetInfo, ScratchNode, WorkflowDescriptor, WorkflowFile } from '../run/api';
import type { CatalogActor } from '../types';
import { ScratchInspector } from './ScratchInspector';
import { readScratchNode, type EdgeFallout, type ScratchCatalogue } from './scratchInspect';

const PROBE: CatalogActor = {
  key: 'probe@0.1.0',
  name: 'probe',
  version: '0.1.0',
  schemaVersion: '1',
  operations: [
    {
      name: 'fetch',
      description: 'GET each target and keep the body.',
      input: { properties: { url: { type: 'string' } }, required: ['url'] },
      output: { properties: { body: { type: 'string' } } },
      params: { properties: { timeout: { type: 'integer' } } },
    },
    // No docstring: the author wrote none, and both SDKs omit the key rather than sending "".
    { name: 'title', input: { properties: { body: { type: 'string' } } } },
  ],
};

function cat(over: Partial<ScratchCatalogue> = {}): ScratchCatalogue {
  return {
    actors: [PROBE],
    workflows: [],
    registered: [],
    datasets: [],
    datasetsAt: 1_700_000_000_000,
    columns: [{ name: 'lame', columns: [{ name: 'domain', type: 'VARCHAR' }] }],
    ...over,
  };
}

const draw = (node: ScratchNode, sources = cat(), fallout: EdgeFallout[] = []): string =>
  renderToStaticMarkup(
    createElement(ScratchInspector, {
      reading: readScratchNode(node, sources),
      fallout,
      onPickMethod: () => {},
    })
  );

const actor = (over: Partial<Extract<ScratchNode, { kind: 'actor' }>> = {}): ScratchNode => ({
  id: 'a1',
  kind: 'actor',
  at: { x: 0, y: 0 },
  actor: 'probe',
  version: '0.1.0',
  method: 'fetch',
  ...over,
});

describe('an Actor node', () => {
  it('says what the Method is for, and what it takes and emits', () => {
    // The reading that decides whether the edge being drawn makes sense, and the only reason to
    // open the panel: a box on a canvas can carry a name and never a paragraph or a schema.
    const html = draw(actor());
    expect(html).toContain('data-testid="scratch-inspector-a1"');
    expect(html).toContain('data-testid="method-doc-probe-fetch"');
    expect(html).toContain('GET each target and keep the body.');
    expect(html).toContain('takes');
    expect(html).toContain('url');
    expect(html).toContain('body');
    // params are ACTOR-level run-wide config, labelled so they do not read as a third argument
    expect(html).toContain('params (run-wide)');
    expect(html).toContain('timeout');
  });

  it('marks the Method in use and offers the others', () => {
    const html = draw(actor());
    expect(html).toContain('data-testid="inspect-inuse-a1"');
    expect(html).toContain('in use');
    // the one it is NOT using is offered, which is the half the canvas had no answer for
    expect(html).toContain('data-testid="inspect-pick-a1-title"');
    expect(html).not.toContain('data-testid="inspect-pick-a1-fetch"');
  });

  it('shows the OTHER Method’s types once it is the one in use', () => {
    // Changing the Method changes what the node IS — the panel is read again off the document, so
    // the description and the field tables move with it.
    const html = draw(actor({ method: 'title' }));
    expect(html).toContain('data-testid="inspect-inuse-a1"');
    expect(html).toContain('data-testid="inspect-pick-a1-fetch"');
    expect(html).not.toContain('data-testid="inspect-pick-a1-title"');
    // `title` declares no output at all, which is not the same as emitting nothing
    expect(html).toContain('not declared');
  });

  it('draws an undescribed Method as an author who wrote none', () => {
    // Both SDKs omit the key rather than sending "" — that difference is the whole reason the key
    // is omitted, and a blank line here would throw it away.
    const html = draw(actor({ method: 'title' }));
    expect(html).toContain('no description — add a docstring to this Method');
    expect(html).not.toContain('data-testid="method-doc-probe-title"');
  });

  it('treats no Method chosen as something to resolve here', () => {
    // Not an error, and not a reason to delete the node and place it again: the sketch saves fine
    // without one, and this is the surface where it gets decided.
    const html = draw(actor({ method: '' }));
    expect(html).toContain('data-testid="inspect-unchosen-a1"');
    expect(html).toContain('No Method chosen yet');
    expect(html).not.toContain('data-testid="inspect-unresolved-a1"');
    expect(html).toContain('data-testid="inspect-pick-a1-fetch"');
    expect(html).toContain('data-testid="inspect-pick-a1-title"');
  });

  it('names the catalog key it wanted when the node does not resolve', () => {
    const html = draw(actor({ version: '9.9.9' }));
    expect(html).toContain('data-testid="inspect-unresolved-a1"');
    expect(html).toContain('probe@9.9.9');
    // nothing to pick from — the entry is where the Methods come from
    expect(html).not.toContain('data-testid="inspect-pick-a1-fetch"');
  });

  it('says which Methods there ARE when the node names one that is not declared', () => {
    const html = draw(actor({ method: 'delegation' }));
    expect(html).toContain('data-testid="inspect-unresolved-a1"');
    expect(html).toContain('declares no Method delegation');
    // and it is fixable from right here
    expect(html).toContain('data-testid="inspect-pick-a1-fetch"');
  });
});

describe('the edges a Method change left to check', () => {
  const fallout: EdgeFallout[] = [
    {
      edgeId: 'e1',
      side: 'in',
      other: 'domains',
      detail: 'title() takes {body: string}, where fetch() took {url: string} — domains was drawn feeding the old one.',
    },
  ];

  it('reports them, and says nothing was removed', () => {
    // An amber strip raises exactly one question — "what did it just do to my drawing" — and the
    // answer has to be in it: the author drew those edges and they are all still there.
    const html = draw(actor({ method: 'title' }), cat(), fallout);
    expect(html).toContain('data-testid="inspect-fallout-a1"');
    expect(html).toContain('data-testid="inspect-fallout-edge-e1"');
    expect(html).toContain('1 edge to check');
    expect(html).toContain('nothing was removed');
    expect(html).toContain('was drawn feeding the old one');
  });

  it('draws no strip when nothing was left to check', () => {
    expect(draw(actor())).not.toContain('data-testid="inspect-fallout-a1"');
  });
});

describe('a Workflow node', () => {
  const node: ScratchNode = { id: 'w1', kind: 'workflow', at: { x: 0, y: 0 }, file: 'nscheck' };
  const file: WorkflowFile = { name: 'nscheck', bytes: 900, modifiedAt: 0, description: 'the lame sweep' };
  const descriptor: WorkflowDescriptor = {
    name: 'NsCheck',
    description: 'Sweep every nameserver of every domain.',
    input: { properties: { domains: { type: 'array', items: { type: 'string' } } } },
    savedAt: 0,
  };

  it('shows what the worker described: the words, the input and the output', () => {
    const html = draw(node, cat({ workflows: [file], registered: [descriptor] }));
    expect(html).toContain('data-testid="workflow-contract"');
    expect(html).toContain('Sweep every nameserver of every domain.');
    expect(html).toContain('domains');
    expect(html).toContain('string[]');
    // an output nobody annotated is `not declared`, never an empty table
    expect(html).toContain('data-testid="workflow-output"');
    expect(html).toContain('not declared');
    // and the file's own `description.md`, which is a different thing from the class docstring
    expect(html).toContain('the lame sweep');
  });

  it('says an undescribed workflow is an author who wrote none', () => {
    const html = draw(node, cat({ workflows: [file], registered: [{ ...descriptor, description: undefined }] }));
    expect(html).toContain('data-testid="inspect-undescribed-w1"');
    expect(html).toContain('no description — add a docstring to the workflow class');
  });

  it('says nothing has registered a contract, naming what it looked for', () => {
    const html = draw(node, cat({ workflows: [file], registered: [] }));
    expect(html).toContain('data-testid="workflow-contract-absent"');
    expect(html).toContain('nscheck');
    expect(html).toContain('has not registered a contract');
  });

  it('says a file this installation does not have is one that does not exist yet', () => {
    const html = draw(node, cat({ workflows: [], registered: [] }));
    expect(html).toContain('data-testid="inspect-unresolved-w1"');
    expect(html).toContain('.kontra/workflows/');
  });

  it('claims nothing about a listing that has not answered', () => {
    // `null` is the page before `/api/workflows` returns, or after it failed. "We could not ask" is
    // not "the file is gone".
    expect(draw(node, cat({ workflows: null }))).not.toContain('data-testid="inspect-unresolved-w1"');
  });
});

describe('a Dataset node', () => {
  const node: ScratchNode = { id: 'd1', kind: 'dataset', at: { x: 0, y: 0 }, name: 'lame', direction: 'out' };
  const rows: DatasetInfo[] = [
    { kind: 'output', name: 'lame', version: '0.1.0', dt: '2026-08-01T10-00-00', rows: 600, bytes: 1_000, state: 'sealed' },
    { kind: 'output', name: 'lame', version: '0.1.0', dt: '2026-08-02T10-00-00', rows: 646, bytes: 1_100, state: 'sealed' },
  ];

  it('says which of the two things it is, in words, off the drawing', () => {
    // Reading a Dataset and writing one are the same call up to `.writer()`, and counting both as
    // output once announced a thousand committed rows a second after a run started. The DRAWING
    // answers it now — an edge onto the node's input is a write — but it still has to be said in
    // words somewhere, and this is where.
    expect(draw(node, cat({ datasets: rows }))).toContain('Drawn as WRITTEN TO');
    expect(draw({ ...node, direction: 'in' } as ScratchNode, cat({ datasets: rows }))).toContain(
      'Drawn as READ FROM'
    );
  });

  it('lists the columns the node draws its ports from', () => {
    // The same reading the handles are built from (`scratchPorts.ts`), drawn twice on purpose: a
    // handle on the canvas is too small to read, and this is where the field you are about to
    // connect gets its name checked.
    const html = draw(node, cat({ datasets: rows }));
    expect(html).toContain('data-testid="inspect-columns-d1"');
    expect(html).toContain('domain');
    expect(html).toContain('VARCHAR');
  });

  it('says the columns have not been read rather than that there are none', () => {
    expect(draw(node, cat({ datasets: rows, columns: null }))).toContain('not read yet');
    expect(draw(node, cat({ datasets: rows, columns: [] }))).toContain(
      'nothing in the lake has written this name'
    );
  });

  it('shows what the lake holds under that name, with the scope of the count', () => {
    const html = draw(node, cat({ datasets: rows }));
    expect(html).toContain('data-testid="inspect-dataset-d1"');
    // one row per Dataset, not per dispatch — and the count says which rows it counted
    expect(html).toContain('1,246 rows · every run');
    expect(html).toContain('2 dispatches');
    expect(html).toContain('sealed');
    expect(html).toContain('v0.1.0');
  });

  it('says a name the lake does not have is one nothing has written yet', () => {
    const html = draw({ ...node, name: 'ghost' } as ScratchNode, cat({ datasets: rows }));
    expect(html).toContain('data-testid="inspect-unresolved-d1"');
    expect(html).toContain('ghost');
  });

  it('claims nothing before the listing has answered', () => {
    const html = draw(node, cat({ datasets: [], datasetsAt: 0 }));
    expect(html).toContain('data-testid="inspect-unread-d1"');
    expect(html).not.toContain('data-testid="inspect-unresolved-d1"');
  });
});
