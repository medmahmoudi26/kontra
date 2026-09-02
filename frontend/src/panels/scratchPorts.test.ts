import { describe, expect, it } from 'vitest';
import type {
  DatasetInfo,
  ScratchDocument,
  ScratchNode,
  WorkflowDescriptor,
} from '../run/api';
import type { CatalogActor } from '../types';
import { readScratchNode, type ScratchCatalogue } from './scratchInspect';
import {
  documentPorts,
  nodePorts,
  portFaults,
  shortType,
  typesLineUp,
  type NodePorts,
} from './scratchPorts';

/**
 * What a node's FIELDS are, per kind — and what it means when an edge joins two of them.
 *
 * THE SCHEMA IS THE EDGES. `fetch → title` is ambiguous about which of three fields carries the
 * page; `fetch.body → title.html` is not. So the assertions here are about where a field comes
 * from — the SELECTED Method, the registered descriptor, the lake's columns — and about the three
 * states that have no field to offer at all, because a node that read as broken in any of them is a
 * node somebody deletes and draws again.
 *
 * AND EVERY JUDGEMENT IS A SENTENCE, never a refusal (ADR 0026). A mismatched edge is marked and
 * still drawn; a field that is not there at all is marked and the edge is still drawn. Scratch is
 * where an operator thinks, and a surface that argues with an unfinished drawing is one they stop
 * opening.
 */

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
      output: { properties: { body: { type: 'string' }, status: { type: 'integer' } } },
    },
    // `title` takes what `fetch` emits, which is the whole point of drawing one into the other.
    {
      name: 'title',
      input: { properties: { html: { type: 'string' } } },
      output: { properties: { title: { type: 'string' } } },
    },
    // A Method that declares NEITHER. `examples/go/dnsfacts` ships like this, and it is dispatchable
    // — the whole-node port is the only thing an edge can attach to.
    { name: 'facts' },
  ],
};

const SWEEP: WorkflowDescriptor = {
  name: 'NsCheck',
  description: 'Sweep every domain for a lame delegation.',
  input: { properties: { domains: { type: 'array', items: { type: 'string' } } } },
  output: { properties: { lame: { type: 'integer' } } },
  savedAt: 1_700_000_000_000,
};

const LAKE: DatasetInfo[] = [
  { kind: 'output', name: 'lame', version: '0.1.0', dt: '2026-08-01T10-00-00', rows: 6, bytes: 9, state: 'sealed' },
];

function cat(over: Partial<ScratchCatalogue> = {}): ScratchCatalogue {
  return {
    actors: [PROBE],
    workflows: [],
    registered: [SWEEP],
    datasets: LAKE,
    datasetsAt: 1_700_000_000_000,
    columns: [
      { name: 'lame', columns: [{ name: 'domain', type: 'VARCHAR' }, { name: 'ns_addr', type: 'VARCHAR' }] },
    ],
    ...over,
  };
}

const actor = (over: Partial<Extract<ScratchNode, { kind: 'actor' }>> = {}): ScratchNode => ({
  id: 'a1',
  kind: 'actor',
  at: { x: 0, y: 0 },
  actor: 'probe',
  version: '0.1.0',
  method: 'fetch',
  ...over,
});

const dataset = (over: Partial<Extract<ScratchNode, { kind: 'dataset' }>> = {}): ScratchNode => ({
  id: 'd1',
  kind: 'dataset',
  at: { x: 0, y: 0 },
  name: 'lame',
  direction: 'in',
  ...over,
});

const workflow: ScratchNode = { id: 'w1', kind: 'workflow', at: { x: 0, y: 0 }, file: 'nscheck.py' };

/** The ports of one node, read the way the canvas reads them: through the inspector's own reading,
 *  so the panel and the handles cannot answer differently. */
const portsOf = (node: ScratchNode, sources = cat()): NodePorts =>
  nodePorts(readScratchNode(node, sources));

const names = (side: { ports: Array<{ name: string }> }): string[] => side.ports.map((p) => p.name);

describe('where each kind’s fields come from', () => {
  it('takes an Actor’s from the Method THIS NODE selected', () => {
    const ports = portsOf(actor());
    expect(names(ports.in)).toEqual(['url']);
    expect(names(ports.out)).toEqual(['body', 'status']);
    expect(ports.in.ports[0]).toMatchObject({ type: 'string', required: true, declared: true });
    expect(ports.out.ports[1]).toMatchObject({ name: 'status', type: 'integer' });
  });

  it('reshapes them when the Method changes, because that is what the node IS', () => {
    // `fetch` takes a URL and emits a body; `title` takes a body and emits a title. The inspector
    // reports the edges this strands (`methodFallout`); the ports are the other half of that fact.
    expect(names(portsOf(actor({ method: 'title' })).in)).toEqual(['html']);
    expect(names(portsOf(actor({ method: 'title' })).out)).toEqual(['title']);
  });

  it('takes a workflow’s from the descriptor its worker registered', () => {
    const ports = portsOf(workflow);
    expect(names(ports.in)).toEqual(['domains']);
    expect(names(ports.out)).toEqual(['lame']);
    expect(ports.in.ports[0]?.type).toBe('string[]');
  });

  it('takes a Dataset’s from its real columns, the same ones on both sides', () => {
    // Writing into `domain` and reading out of `domain` are one column seen from two ends.
    const ports = portsOf(dataset());
    expect(names(ports.in)).toEqual(['domain', 'ns_addr']);
    expect(names(ports.out)).toEqual(['domain', 'ns_addr']);
    expect(ports.out.ports[0]?.type).toBe('VARCHAR');
  });
});

describe('a node with nothing to connect per field', () => {
  it('says the Method declares none, which is not the same as broken', () => {
    // A Method declaring neither `takes=` nor `emits=` is dispatchable and ordinary; the whole-node
    // port is what it offers, and the sentence is what stops it reading as a node that failed.
    const ports = portsOf(actor({ method: 'facts' }));
    expect(ports.in.ports).toEqual([]);
    expect(ports.in.declared).toBe(false);
    expect(ports.in.why).toBe('not declared');
    expect(ports.out.why).toBe('not declared');
  });

  it('says a Method has not been chosen, rather than that the node has no fields', () => {
    // The ordinary way a sketch is drawn: place the Actor, draw the order, resolve it in the
    // inspector. `ScratchActorNode.method` is allowed to be empty for exactly this.
    expect(portsOf(actor({ method: '' })).in.why).toBe('no Method chosen yet');
  });

  it('says a workflow nobody has served has no contract, not that it takes nothing', () => {
    expect(portsOf(workflow, cat({ registered: [] })).out.why).toBe(
      'no worker has registered its contract'
    );
  });

  it('keeps `declares no fields` apart from `not declared` for a `dict` workflow', () => {
    // Both workflows this repo ships annotate `dict`, which derives `{"type":"object"}` — "any
    // object", not "an object with no fields". An empty table under a column header would say the
    // opposite of what it means.
    const dictish: WorkflowDescriptor = {
      name: 'NsCheck',
      input: { type: 'object' },
      output: undefined,
      savedAt: 1_700_000_000_000,
    };
    const ports = portsOf(workflow, cat({ registered: [dictish] }));
    expect(ports.in.why).toBe('declares no fields');
    expect(ports.out.why).toBe('not declared');
  });

  it('claims nothing about a Dataset before the lake has answered', () => {
    // `null` is the page before `/api/datasets/schema` returns, or after it failed. "We could not
    // ask" is not "it has no columns".
    expect(portsOf(dataset(), cat({ columns: null })).in.why).toBe(
      'the lake has not answered with its columns yet'
    );
    expect(portsOf(dataset({ name: 'ghost' }), cat()).in.why).toBe(
      'nothing in the lake has this name yet'
    );
  });
});

describe('the edges of a whole drawing', () => {
  /** `fetch.body → title.html`, drawn against the signature `fetch` had. */
  const doc = (over: Partial<ScratchDocument> = {}): ScratchDocument => ({
    nodes: [actor(), actor({ id: 'a2', method: 'title' })],
    edges: [{ id: 'e1', from: 'a1', to: 'a2', fromPort: 'body', toPort: 'html' }],
    notes: [],
    ...over,
  });

  it('says nothing about an edge whose two fields line up', () => {
    expect([...portFaults(doc(), documentPorts(doc(), cat())).keys()]).toEqual([]);
  });

  it('says nothing about an edge that names no field at all', () => {
    // The whole-node port: "these two are connected, I have not said how yet" — and what every edge
    // drawn before typed ports means.
    const plain = doc({ edges: [{ id: 'e1', from: 'a1', to: 'a2' }] });
    expect([...portFaults(plain, documentPorts(plain, cat())).keys()]).toEqual([]);
  });

  it('keeps drawing a field a Method change stranded, and marks it', () => {
    // THE FAILURE THIS EXISTS FOR: React Flow drops an edge whose named handle is gone, so a port
    // that vanished would take the author's line off the canvas while leaving it in the document.
    // The port comes back undeclared instead — the edge stays drawn and says what is wrong.
    const changed = doc({ nodes: [actor({ method: 'title' }), actor({ id: 'a2', method: 'title' })] });
    const ports = documentPorts(changed, cat());
    expect(names(ports.get('a1')!.out)).toEqual(['title', 'body']);
    expect(ports.get('a1')!.out.ports.find((p) => p.name === 'body')?.declared).toBe(false);

    const fault = portFaults(changed, ports).get('e1');
    expect(fault).toContain('declares no output field body');
    expect(fault).toContain('probe@0.1.0.title()');
  });

  it('marks two fields of different types, and still draws the edge', () => {
    const crossed = doc({
      edges: [{ id: 'e1', from: 'a1', to: 'a2', fromPort: 'status', toPort: 'html' }],
    });
    const faults = portFaults(crossed, documentPorts(crossed, cat()));
    expect(faults.get('e1')).toBe(
      'status leaves as integer and html takes string — this edge joins two fields of different types.'
    );
    // and the edge is still in the document nobody removed it from
    expect(crossed.edges).toHaveLength(1);
  });

  it('says nothing when one language cannot be compared with the other', () => {
    // A Method's field is a JSON Schema type and a Dataset's column is a DuckDB one. Marking every
    // Actor→Dataset edge would be a marking nobody reads.
    const toLake: ScratchDocument = {
      nodes: [actor(), dataset()],
      edges: [{ id: 'e1', from: 'a1', to: 'd1', fromPort: 'body', toPort: 'domain' }],
      notes: [],
    };
    expect([...portFaults(toLake, documentPorts(toLake, cat())).keys()]).toEqual([]);
  });
});

describe('two declared types line up when they could be the same value', () => {
  it('reads DuckDB and JSON Schema as one vocabulary', () => {
    expect(typesLineUp('string', 'VARCHAR')).toBe(true);
    expect(typesLineUp('integer', 'HUGEINT')).toBe(true);
    expect(typesLineUp('number', 'DECIMAL(18,3)')).toBe(true);
    expect(typesLineUp('object', 'STRUCT(body_len BIGINT)')).toBe(true);
    expect(typesLineUp('string[]', 'VARCHAR[]')).toBe(true);
    expect(typesLineUp('boolean', 'BOOLEAN')).toBe(true);
    // a timestamp column takes a string, because JSON has no date
    expect(typesLineUp('string', 'TIMESTAMP WITH TIME ZONE')).toBe(true);
  });

  it('does not line a string up with a number or a list with an object', () => {
    expect(typesLineUp('string', 'integer')).toBe(false);
    expect(typesLineUp('string[]', 'object')).toBe(false);
    expect(typesLineUp('boolean', 'VARCHAR')).toBe(false);
  });

  it('stays silent about anything it cannot read', () => {
    // A `$ref`'d model name, `any`, a BLOB. Asserting a mismatch it cannot actually see is worse
    // than saying nothing — this marks a value going somewhere it plainly cannot.
    expect(typesLineUp('any', 'VARCHAR')).toBe(true);
    expect(typesLineUp('Target', 'integer')).toBe(true);
    expect(typesLineUp('BLOB', 'integer')).toBe(true);
  });

  it('lines a union up when any one member does', () => {
    // `string | null` is what a derived optional arrives as.
    expect(typesLineUp('string | null', 'VARCHAR')).toBe(true);
    expect(typesLineUp('string | null', 'BIGINT')).toBe(false);
  });
});

describe('a type short enough to draw on a handle', () => {
  it('keeps a plain one and cuts a struct down', () => {
    // A lake column can be two hundred characters of `STRUCT(...)`; a node is 216 pixels wide, and
    // the full type is in the handle's title.
    expect(shortType('VARCHAR')).toBe('VARCHAR');
    expect(shortType('STRUCT(body_len BIGINT, body_preview VARCHAR)')).toBe('STRUCT…');
  });
});
