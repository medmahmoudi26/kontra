/**
 * What the inspector READS about a node, before anything is drawn.
 *
 * THREE PROPERTIES ARE WORTH TESTS HERE AND THE REST IS MARKUP (`scratchInspect.render.test.ts`).
 *
 * The first is that AN UNRESOLVED NODE SAYS WHAT THE AGENT WILL BE TOLD. `resolveScratch` on the
 * server decides what a sketch claims that the catalog does not have, and that sentence goes into
 * the spec; a second wording invented in the browser would give the author and the agent two
 * accounts of one drawing. So the sentences are asserted against the server's own function rather
 * than typed out here.
 *
 * The second is that NOT LOADED IS NEVER SAID AS NOT THERE. The lake listing polls and the workflow
 * listing is fetched once; before either answers the page holds an empty array, and reading that as
 * "no Dataset by that name exists" is a claim about the lake made without looking.
 *
 * The third is what a METHOD CHANGE does to the edges already drawn. It changes what the node takes
 * and emits — `fetch` takes a URL and emits a body while `title` takes a body — so an edge into it
 * was drawn against a signature that is no longer there. It is reported, and the document keeps
 * every edge the author drew.
 */

import { describe, expect, it } from 'vitest';
// The SERVER's own resolution, not a restatement of it: the sentence an author reads in the
// inspector is the sentence the agent reads in the spec, or the two disagree about one drawing.
import { resolveScratch } from '@core/scratch';
import type { DatasetInfo, ScratchDocument, ScratchNode, WorkflowDescriptor, WorkflowFile } from '../run/api';
import type { CatalogActor } from '../types';
import { withMethod } from './scratchFlow';
import {
  describeWorkflow,
  methodFallout,
  nodeLabel,
  operationOf,
  portText,
  readScratchNode,
  type ScratchCatalogue,
} from './scratchInspect';

/** `probe@0.1.0`: two Methods whose signatures differ, and one that repeats `fetch`'s exactly. */
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
    {
      name: 'title',
      description: 'Read the <title> out of a body.',
      input: { properties: { body: { type: 'string' } } },
      output: { properties: { title: { type: 'string' } } },
    },
    {
      // Same two schemas as `fetch`, and no docstring — the author wrote none.
      name: 'refetch',
      input: { properties: { url: { type: 'string' } }, required: ['url'] },
      output: { properties: { body: { type: 'string' } } },
    },
  ],
};

/** A load-only Actor: dispatchable, advertises nothing (`internals/catalog.py`). */
const LOADER: CatalogActor = {
  key: 'loader@0.1.0',
  name: 'loader',
  version: '0.1.0',
  schemaVersion: '1',
  operations: [],
};

const CATALOG = [PROBE, LOADER];

function actorNode(over: Partial<Extract<ScratchNode, { kind: 'actor' }>> = {}): ScratchNode {
  return { id: 'a1', kind: 'actor', at: { x: 0, y: 0 }, actor: 'probe', version: '0.1.0', method: 'fetch', ...over };
}

function cat(over: Partial<ScratchCatalogue> = {}): ScratchCatalogue {
  return {
    actors: CATALOG,
    workflows: [],
    registered: [],
    datasets: [],
    datasetsAt: 1_700_000_000_000,
    columns: [],
    ...over,
  };
}

/** The sentence the SERVER would put in the spec for this node. */
function serverSays(node: ScratchNode): string {
  const record = { id: 's1', name: 'sketch', document: { nodes: [node], edges: [], notes: [] }, updatedAt: 0 };
  return resolveScratch(record, CATALOG).problems[0]?.detail ?? '';
}

describe('an Actor node, read against the catalog', () => {
  it('finds the Method the node names', () => {
    const reading = readScratchNode(actorNode(), cat());
    expect(reading.kind).toBe('actor');
    if (reading.kind !== 'actor') return;
    expect(reading.standing).toEqual({ state: 'resolved', op: PROBE.operations[0] });
    // and every Method is offered, because picking a different one is the point of the panel
    expect(reading.methods.map((o) => o.name)).toEqual(['fetch', 'title', 'refetch']);
  });

  it('treats no Method chosen as a state to resolve, not as a problem', () => {
    // `ScratchActorNode.method` is allowed to be empty precisely because a half-drawn sketch is a
    // legitimate thing to save. The inspector is where it gets decided, so it must still list what
    // there is to decide between.
    const reading = readScratchNode(actorNode({ method: '' }), cat());
    if (reading.kind !== 'actor') throw new Error('expected an actor reading');
    expect(reading.standing.state).toBe('unchosen');
    expect(reading.methods).toHaveLength(3);
  });

  it('says a Method the Actor does not declare in the server’s own words', () => {
    const node = actorNode({ method: 'delegation' });
    const reading = readScratchNode(node, cat());
    if (reading.kind !== 'actor') throw new Error('expected an actor reading');
    expect(reading.standing.state).toBe('unknown-method');
    // The sentence is the spec's, not a second one written for the browser.
    expect(reading.standing).toEqual({ state: 'unknown-method', detail: serverSays(node) });
    expect(serverSays(node)).toContain('declares no Method delegation');
    // and the Methods it DOES declare are still offered — this is a fixable node, not a dead one
    expect(reading.methods.map((o) => o.name)).toContain('title');
  });

  it('names the catalog key it wanted when nothing is registered under it', () => {
    const node = actorNode({ version: '9.9.9' });
    const reading = readScratchNode(node, cat());
    if (reading.kind !== 'actor') throw new Error('expected an actor reading');
    expect(reading.standing).toEqual({ state: 'unknown-actor', detail: serverSays(node) });
    expect(reading.standing.state === 'unknown-actor' && reading.standing.detail).toContain(
      'probe@9.9.9'
    );
    // Nothing to pick from: the entry is what the Methods come from.
    expect(reading.methods).toEqual([]);
  });

  it('reads a load-only Actor as an Actor with nothing to dispatch', () => {
    const reading = readScratchNode(actorNode({ actor: 'loader', method: '' }), cat());
    if (reading.kind !== 'actor') throw new Error('expected an actor reading');
    expect(reading.standing.state).toBe('unchosen');
    expect(reading.methods).toEqual([]);
  });
});

describe('a Workflow node, joined to what a worker registered', () => {
  const FILE: WorkflowFile = { name: 'nscheck', bytes: 900, modifiedAt: 0, description: 'lame sweep' };
  const DESC: WorkflowDescriptor = {
    name: 'NsCheck',
    description: 'Sweep every nameserver.',
    input: { properties: { domains: { type: 'array', items: { type: 'string' } } } },
    savedAt: 0,
  };
  const node: ScratchNode = { id: 'w1', kind: 'workflow', at: { x: 0, y: 0 }, file: 'nscheck' };

  it('joins the FILE a node names to the TYPE a worker described', () => {
    // The two sides are keyed differently: a descriptor is keyed by the `@workflow.defn` type
    // (`NsCheck`), a node by the file (`nscheck`). This is `guessTypeFromFilename` run backwards.
    expect(describeWorkflow('nscheck', [DESC])).toEqual({ state: 'described', descriptor: DESC });
    expect(describeWorkflow('dns_sweep.py', [{ ...DESC, name: 'DnsSweep' }])).toMatchObject({
      state: 'described',
    });
  });

  it('says nothing registered rather than guessing when the type was renamed', () => {
    // `@workflow.defn(name="Sweep")` on a file called `nscheck.py` cannot be joined by name, and a
    // contract picked by proximity would be the wrong workflow's schemas under this node.
    expect(describeWorkflow('nscheck', [{ ...DESC, name: 'Sweep' }])).toEqual({
      state: 'unregistered',
    });
  });

  it('refuses to choose when one file’s name matches several registered types', () => {
    const two = [DESC, { ...DESC, name: 'ns_check' }];
    expect(describeWorkflow('nscheck', two)).toEqual({
      state: 'ambiguous',
      types: ['NsCheck', 'ns_check'],
    });
  });

  it('reports a file the listing does not have — but only once the listing answered', () => {
    const missing = readScratchNode(node, cat({ workflows: [], registered: [DESC] }));
    if (missing.kind !== 'workflow') throw new Error('expected a workflow reading');
    expect(missing.listed).toBe(true);
    expect(missing.file).toBeNull();

    // Before the listing answers, the page holds no list at all — and "we have not looked" is not
    // "it is not there".
    const unread = readScratchNode(node, cat({ workflows: null, registered: [DESC] }));
    if (unread.kind !== 'workflow') throw new Error('expected a workflow reading');
    expect(unread.listed).toBe(false);
    expect(unread.file).toBeNull();
  });

  it('carries the file’s own description beside the registered contract', () => {
    const reading = readScratchNode(node, cat({ workflows: [FILE], registered: [DESC] }));
    if (reading.kind !== 'workflow') throw new Error('expected a workflow reading');
    expect(reading.file).toBe(FILE);
    expect(reading.standing).toEqual({ state: 'described', descriptor: DESC });
  });
});

describe('a Dataset node, read against the lake listing', () => {
  const node: ScratchNode = { id: 'd1', kind: 'dataset', at: { x: 0, y: 0 }, name: 'lame', direction: 'out' };
  const rows: DatasetInfo[] = [
    { kind: 'output', name: 'lame', version: '0.1.0', dt: '2026-08-01T10-00-00', rows: 600, bytes: 1_000, state: 'sealed' },
    { kind: 'output', name: 'lame', version: '0.1.0', dt: '2026-08-02T10-00-00', rows: 646, bytes: 1_100, state: 'sealed' },
    { kind: 'output', name: 'other', rows: 5, bytes: 10 },
  ];

  it('folds every dispatch of the name into one reading', () => {
    const reading = readScratchNode(node, cat({ datasets: rows }));
    if (reading.kind !== 'dataset') throw new Error('expected a dataset reading');
    if (reading.standing.state !== 'listed') throw new Error('expected it listed');
    expect(reading.standing.groups).toHaveLength(1);
    expect(reading.standing.groups[0]?.total.total.rows).toBe(1246);
    expect(reading.standing.groups[0]?.dispatches).toHaveLength(2);
  });

  it('says the name is not in the lake when the listing has answered without it', () => {
    const reading = readScratchNode({ ...node, name: 'ghost' } as ScratchNode, cat({ datasets: rows }));
    if (reading.kind !== 'dataset') throw new Error('expected a dataset reading');
    expect(reading.standing.state).toBe('absent');
  });

  it('claims nothing at all before the listing has answered', () => {
    // `datasetsAt === 0` is the store before its first poll returns. An empty listing then is not
    // an empty lake, and drawing "no Dataset named lame" would be a claim made without looking.
    const reading = readScratchNode(node, cat({ datasets: [], datasetsAt: 0 }));
    if (reading.kind !== 'dataset') throw new Error('expected a dataset reading');
    expect(reading.standing.state).toBe('unread');
  });

  it('carries the COLUMNS, which are the node’s ports', () => {
    // The listing and the schema are two calls answering two questions — what exists, and what is
    // in it — and the ports on the canvas are read off this reading rather than looked up again
    // (`scratchPorts.ts`), so the panel and the handles cannot disagree.
    const reading = readScratchNode(
      node,
      cat({ datasets: rows, columns: [{ name: 'lame', columns: [{ name: 'domain', type: 'VARCHAR' }] }] })
    );
    if (reading.kind !== 'dataset') throw new Error('expected a dataset reading');
    expect(reading.columns).toEqual({ state: 'listed', columns: [{ name: 'domain', type: 'VARCHAR' }] });
  });

  it('keeps “not asked yet” apart from “it has no columns”', () => {
    // `null` is before `/api/datasets/schema` answered, or after it failed. Reading that as "this
    // Dataset has no columns" is a claim about the lake made without having looked.
    const unread = readScratchNode(node, cat({ datasets: rows, columns: null }));
    if (unread.kind !== 'dataset') throw new Error('expected a dataset reading');
    expect(unread.columns.state).toBe('unread');

    const answered = readScratchNode(node, cat({ datasets: rows, columns: [] }));
    if (answered.kind !== 'dataset') throw new Error('expected a dataset reading');
    expect(answered.columns.state).toBe('absent');
  });
});

describe('what a Method change does to the edges already drawn', () => {
  /** `domains → probe.fetch → title`, drawn while the middle node was `fetch`. */
  const doc: ScratchDocument = {
    nodes: [
      { id: 'd1', kind: 'dataset', at: { x: 0, y: 0 }, name: 'domains', direction: 'in' },
      actorNode(),
      { id: 'a2', kind: 'actor', at: { x: 0, y: 0 }, actor: 'probe', version: '0.1.0', method: 'title' },
    ],
    edges: [
      { id: 'e1', from: 'd1', to: 'a1' },
      { id: 'e2', from: 'a1', to: 'a2' },
    ],
    notes: [],
  };
  const node = doc.nodes[1] as Extract<ScratchNode, { kind: 'actor' }>;
  const opOf = (m: string) => operationOf(CATALOG, node, m);

  it('reports both sides when both halves of the signature moved', () => {
    const fallout = methodFallout(doc, 'a1', opOf('fetch'), opOf('title'));
    expect(fallout.map((f) => [f.edgeId, f.side])).toEqual([
      ['e1', 'in'],
      ['e2', 'out'],
    ]);
    // The sentence names the change, in the same words the field table under it uses.
    expect(fallout[0]?.detail).toContain('title() takes {body: string}, where fetch() took {url: string}');
    expect(fallout[0]?.detail).toContain('domains');
    expect(fallout[1]?.detail).toContain('title() emits {title: string}, where fetch() emitted {body: string}');
    expect(fallout[1]?.detail).toContain('probe@0.1.0.title()');
  });

  it('reports nothing for a Method with the very same signature', () => {
    // `refetch` takes and emits exactly what `fetch` did. Nothing an edge was drawn for has moved,
    // and a report here would train the author to dismiss the strip without reading it.
    expect(methodFallout(doc, 'a1', opOf('fetch'), opOf('refetch'))).toEqual([]);
  });

  it('reports nothing when the node had no Method to disagree with', () => {
    // The ordinary way a sketch is drawn: place the Actor, draw the order, then resolve the Method
    // here. There was no signature behind those edges, so there is nothing to check.
    expect(methodFallout(doc, 'a1', undefined, opOf('title'))).toEqual([]);
  });

  it('reports only the side that moved', () => {
    // A Method whose output is unchanged leaves everything downstream exactly as true as it was.
    const sameOut = { name: 'other', input: { properties: { host: { type: 'string' } } }, output: { properties: { body: { type: 'string' } } } };
    const fallout = methodFallout(doc, 'a1', opOf('fetch'), sameOut);
    expect(fallout.map((f) => f.edgeId)).toEqual(['e1']);
  });

  it('keeps every edge the author drew', () => {
    // THE WHOLE POINT: reported, never deleted. A canvas that removed a line to keep itself
    // consistent would be editing the drawing rather than describing it.
    const after = withMethod(doc, 'a1', 'title');
    expect(after.edges).toEqual(doc.edges);
    expect(methodFallout(doc, 'a1', opOf('fetch'), opOf('title'))).toHaveLength(2);
  });

  it('changes the reading with the Method, and only that node', () => {
    const after = withMethod(doc, 'a1', 'title');
    const reading = readScratchNode(after.nodes[1] as ScratchNode, cat());
    if (reading.kind !== 'actor') throw new Error('expected an actor reading');
    expect(reading.standing).toEqual({ state: 'resolved', op: PROBE.operations[1] });
    // the OTHER probe node, which names the same Actor and version, is untouched
    expect(after.nodes[2]).toBe(doc.nodes[2]);
  });
});

describe('opening the panel is not an edit', () => {
  it('reads a node without touching the document it came from', () => {
    // Selection is CANVAS state, never document state: a click must not dirty a drawing, and two
    // people opening the same Scratch do not share a cursor. The page keeps the selected id in
    // React state; this is the half of that rule the suite can reach — reading a node produces a
    // reading and leaves the bytes that would be saved exactly as they were.
    const doc: ScratchDocument = { nodes: [actorNode()], edges: [], notes: [] };
    const before = JSON.stringify(doc);
    readScratchNode(doc.nodes[0] as ScratchNode, cat());
    expect(JSON.stringify(doc)).toBe(before);
  });
});

describe('the words the panel and the sentences share', () => {
  it('names a node the way the canvas and the spec do', () => {
    expect(nodeLabel(actorNode())).toBe('probe@0.1.0.fetch()');
    expect(nodeLabel(actorNode({ method: '' }))).toBe('probe@0.1.0.?()');
    expect(nodeLabel({ id: 'w1', kind: 'workflow', at: { x: 0, y: 0 }, file: 'nscheck' })).toBe('nscheck');
    expect(nodeLabel({ id: 'd1', kind: 'dataset', at: { x: 0, y: 0 }, name: 'lame', direction: 'out' })).toBe('lame');
  });

  it('keeps “declares nothing” apart from “takes nothing”', () => {
    // The same distinction the field tables draw. Switching between the two IS a change of what the
    // node takes, and collapsing them would hide it.
    expect(portText(undefined)).toBe('not declared');
    expect(portText({ type: 'object' })).toBe('not declared');
    expect(portText({ properties: {} })).toBe('{}');
    expect(portText({ properties: { url: { type: 'string' } } })).toBe('{url: string}');
  });
});
