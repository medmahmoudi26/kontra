import { describe, expect, it } from 'vitest';
import {
  parseScratchDocument,
  renderScratch,
  resolveScratch,
  type CatalogEntry,
  type ScratchDocument,
  type ScratchRecordLike,
} from './scratch';

/**
 * The Scratch document, and the spec an agent writes code from.
 *
 * THE WHOLE FEATURE RESTS ON ONE PROPERTY: a node is a REAL thing, not a rectangle with a label.
 * `actor nscheck@0.1.0 method=delegation` is the catalog's own key and a Method that key declares,
 * so an agent reading it has nothing to guess — while a label somebody typed leaves it guessing
 * which actor, which version, and whether the second word is a Method or a note.
 *
 * The other half is that what the drawing names and the catalog does not have is REPORTED, never
 * silently corrected and never a reason to refuse the drawing. Sketching an Actor that does not
 * exist yet is how somebody works out what to build.
 */

const CATALOG: CatalogEntry[] = [
  {
    name: 'nscheck',
    version: '0.1.0',
    operations: [
      { name: 'delegation', description: "Resolve each domain's delegated NS set" },
      { name: 'ask', description: 'Ask one nameserver whether it serves the zone' },
    ],
  },
  { name: 'probe', version: '0.1.0', operations: [{ name: 'fetch' }, { name: 'head' }] },
];

const at = { x: 0, y: 0 };

function record(document: ScratchDocument, name = 'a recon sweep'): ScratchRecordLike {
  return { id: 's1', name, document, updatedAt: 1 };
}

describe('what a document keeps', () => {
  it('keeps the three kinds of node', () => {
    const doc = parseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'w', kind: 'workflow', at, file: 'nscheck.py' },
        { id: 'd', kind: 'dataset', at, name: 'domains', direction: 'in' },
      ],
      edges: [{ id: 'e', from: 'd', to: 'a' }],
      notes: [{ id: 'n', at, text: 'page 200 at a time' }],
    });
    expect(doc.nodes.map((n) => n.kind)).toEqual(['actor', 'workflow', 'dataset']);
    expect(doc.edges).toHaveLength(1);
    expect(doc.notes[0]?.text).toBe('page 200 at a time');
  });

  it('DROPS a node of a kind this build has no meaning for', () => {
    // The document is handed to an agent that will write code from it. A node nothing understands
    // would be an uninterpretable thing in front of that agent; refusing the whole document would
    // lose the twenty nodes that were fine.
    const doc = parseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'x', kind: 'wormhole', at },
      ],
    });
    expect(doc.nodes.map((n) => n.id)).toEqual(['a']);
  });

  it('drops an edge that points at nothing', () => {
    // It cannot be drawn and cannot be followed, so carrying it forward carries a dangling
    // reference into whatever gets generated.
    const doc = parseScratchDocument({
      nodes: [{ id: 'a', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'ask' }],
      edges: [
        { id: 'ok', from: 'a', to: 'a' },
        { id: 'dangling', from: 'a', to: 'ghost' },
      ],
    });
    expect(doc.edges.map((e) => e.id)).toEqual(['ok']);
  });

  it('keeps the FIELD each end of an edge names', () => {
    // `fetch.body → title.html` says which value travels, where `fetch → title` leaves an agent to
    // work it out from two schemas. Dropped here, a drawing would read correct on the canvas and
    // lose its fields the first time somebody pressed reload.
    const doc = parseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at, actor: 'probe', version: '0.1.0', method: 'fetch' },
        { id: 'b', kind: 'actor', at, actor: 'probe', version: '0.1.0', method: 'head' },
      ],
      edges: [{ id: 'e', from: 'a', to: 'b', fromPort: 'body', toPort: 'url' }],
    });
    expect(doc.edges[0]).toEqual({ id: 'e', from: 'a', to: 'b', fromPort: 'body', toPort: 'url' });
  });

  it('leaves an edge that names no field exactly as it was', () => {
    // Which is every edge in every document drawn before typed ports, and what a Method declaring
    // neither `takes=` nor `emits=` can offer. An empty string is not a field name: stored as one,
    // it would be a port nothing declares and a handle nothing draws.
    const doc = parseScratchDocument({
      nodes: [{ id: 'a', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'ask' }],
      edges: [
        { id: 'plain', from: 'a', to: 'a' },
        { id: 'empty', from: 'a', to: 'a', fromPort: '', toPort: 7 },
      ],
    });
    expect(JSON.stringify(doc.edges)).toBe(
      '[{"id":"plain","from":"a","to":"a"},{"id":"empty","from":"a","to":"a"}]'
    );
  });

  it('unpins a note whose node is gone rather than losing the note', () => {
    // The words are the part a person wrote. A note that loses its anchor is still worth reading;
    // one that is dropped with its node takes intent out of the spec silently.
    const doc = parseScratchDocument({
      nodes: [],
      notes: [{ id: 'n', at, text: 'isolate, do not fail the run', on: 'gone' }],
    });
    expect(doc.notes).toHaveLength(1);
    expect(doc.notes[0]?.on).toBeUndefined();
  });

  it('survives junk without throwing', () => {
    // It arrives from a browser.
    expect(parseScratchDocument(undefined)).toEqual({ nodes: [], edges: [], notes: [] });
    expect(parseScratchDocument({ nodes: 'no', edges: 7 })).toEqual({ nodes: [], edges: [], notes: [] });
    expect(parseScratchDocument({ nodes: [{ id: 'a', kind: 'actor' }] }).nodes).toEqual([]);
  });
});

describe('what the catalog says about it', () => {
  it('is silent when every node resolves', () => {
    const doc = parseScratchDocument({
      nodes: [{ id: 'a', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'ask' }],
    });
    expect(resolveScratch(record(doc), CATALOG).problems).toEqual([]);
  });

  it('names an Actor that is not registered — and still keeps the drawing', () => {
    // Sketching something that does not exist yet is how somebody works out what to build.
    const doc = parseScratchDocument({
      nodes: [{ id: 'a', kind: 'actor', at, actor: 'notbuilt', version: '0.1.0', method: 'go' }],
    });
    const got = resolveScratch(record(doc), CATALOG);
    expect(got.record.document.nodes).toHaveLength(1);
    expect(got.problems[0]?.detail).toMatch(/no Actor notbuilt@0.1.0 is registered/);
  });

  it('names a Method the Actor does not declare, and LISTS the ones it does', () => {
    // The list is what makes the report actionable: an agent can pick the right one, and a person
    // can see they meant `delegation`.
    const doc = parseScratchDocument({
      nodes: [{ id: 'a', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'delegate' }],
    });
    const got = resolveScratch(record(doc), CATALOG);
    expect(got.problems[0]?.detail).toContain('declares no Method delegate');
    expect(got.problems[0]?.detail).toContain('delegation, ask');
  });

  it('says when no Method was chosen at all', () => {
    const doc = parseScratchDocument({
      nodes: [{ id: 'a', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: '' }],
    });
    expect(resolveScratch(record(doc), CATALOG).problems[0]?.detail).toMatch(/no Method chosen/);
  });
});

describe('the spec an agent reads', () => {
  const doc = parseScratchDocument({
    nodes: [
      { id: 'd1', kind: 'dataset', at, name: 'domains', direction: 'in' },
      { id: 'a1', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'delegation' },
      { id: 'a2', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'ask' },
      { id: 'd2', kind: 'dataset', at, name: 'lame', direction: 'out' },
    ],
    edges: [
      { id: 'e1', from: 'd1', to: 'a1', label: 'a page at a time' },
      { id: 'e2', from: 'a1', to: 'a2' },
      { id: 'e3', from: 'a2', to: 'd2' },
    ],
    notes: [
      { id: 'n1', at, text: 'page 200 at a time' },
      { id: 'n2', at, text: 'this fan-out is about 4x', on: 'a1' },
    ],
  });

  it('resolves every name, with the Method’s own description', () => {
    const spec = renderScratch(resolveScratch(record(doc), CATALOG), CATALOG);
    expect(spec).toContain('`nscheck@0.1.0` method `delegation`');
    // The description is what tells an agent what the step DOES rather than only what it is called.
    expect(spec).toContain("Resolve each domain's delegated NS set");
    expect(spec).toContain('**dataset** `domains` — read from');
    expect(spec).toContain('**dataset** `lame` — written to');
  });

  it('states the order, and carries an edge’s label', () => {
    const spec = renderScratch(resolveScratch(record(doc), CATALOG), CATALOG);
    expect(spec).toContain('`domains` (dataset) → `nscheck@0.1.0.delegation`');
    expect(spec).toContain('`nscheck@0.1.0.delegation` → `nscheck@0.1.0.ask`');
    expect(spec).toContain('_(a page at a time)_');
  });

  it('carries the notes VERBATIM, and says what a pinned one is pinned to', () => {
    // These are the things a person knows and a schema does not, and they are what makes generated
    // code right rather than merely valid.
    const spec = renderScratch(resolveScratch(record(doc), CATALOG), CATALOG);
    expect(spec).toContain('- page 200 at a time');
    expect(spec).toContain('on `nscheck@0.1.0.delegation`: this fan-out is about 4x');
  });

  it('tells the agent NOT to invent what does not resolve', () => {
    const broken = parseScratchDocument({
      nodes: [{ id: 'a', kind: 'actor', at, actor: 'ghost', version: '9.9.9', method: 'go' }],
    });
    const spec = renderScratch(resolveScratch(record(broken), CATALOG), CATALOG);
    expect(spec).toContain('What does not resolve');
    expect(spec).toMatch(/Do not invent them/);
  });

  it('says an empty canvas is empty rather than rendering a shape', () => {
    const spec = renderScratch(resolveScratch(record(parseScratchDocument({})), CATALOG), CATALOG);
    expect(spec).toContain('The canvas is empty');
  });

  it('names the FIELD at each end of an edge that has one, and explains the notation once', () => {
    // The reason typed ports exist: an agent reading `fetch → title` has to guess which of three
    // fields carries the page. The legend is drawn only for a drawing that has one — a document
    // whose edges name no field renders exactly the bytes it always did (ADR 0026).
    const ported = parseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at, actor: 'probe', version: '0.1.0', method: 'fetch' },
        { id: 'd', kind: 'dataset', at, name: 'lame', direction: 'out' },
      ],
      edges: [{ id: 'e', from: 'a', to: 'd', fromPort: 'body', toPort: 'html' }],
    });
    const spec = renderScratch(resolveScratch(record(ported), CATALOG), CATALOG);
    expect(spec).toContain('- `probe@0.1.0.fetch.body` → `lame.html` (dataset)');
    expect(spec).toContain('An edge written `a.field → b.field` names the FIELDS it carries');
    // and a drawing without ports is never handed the legend
    expect(renderScratch(resolveScratch(record(doc), CATALOG), CATALOG)).not.toContain(
      'names the FIELDS it carries'
    );
  });

  it('says when pieces were placed and never connected', () => {
    // A real and common half-finished state, and one an agent must not paper over by guessing an
    // order from the coordinates.
    const loose = parseScratchDocument({
      nodes: [
        { id: 'a', kind: 'actor', at, actor: 'nscheck', version: '0.1.0', method: 'ask' },
        { id: 'b', kind: 'dataset', at, name: 'lame', direction: 'out' },
      ],
    });
    expect(renderScratch(resolveScratch(record(loose), CATALOG), CATALOG)).toContain(
      'Nothing is connected'
    );
  });

  it('tells the agent what to BUILD, not what to run', () => {
    // Nothing in a Scratch runs. kontra had a canvas that WAS the execution model and it was
    // removed (ADR 0023 §12); this is the opposite direction — a drawing that is input to writing
    // the workflow, and the workflow is what runs.
    const spec = renderScratch(resolveScratch(record(doc), CATALOG), CATALOG);
    expect(spec).toMatch(/CALLER WORKFLOW/);
    expect(spec).toMatch(/Nothing here runs/);
  });
});

/**
 * The Scratch this installation has saved, and every character of the spec it renders to.
 *
 * PINNED WHOLE, and that is the point. The editor is being rebuilt on a canvas library, and the one
 * thing that must not move while it is is what an agent reads: `renderScratch` is the SINGLE
 * derivation of what a drawing said (ADR 0026), so a document reshaped to suit a library would
 * change this output and nothing else would notice. The assertions above each cover one clause; a
 * whole-output pin is what catches a clause going missing.
 *
 * The bytes are lifted out of the store verbatim, so the key order is `parseScratchDocument`'s own —
 * which is also what the browser's `normaliseScratchDocument` is held to.
 */
describe('the sketch that is actually saved', () => {
  const SAVED =
    '{"nodes":[{"id":"d1","kind":"dataset","at":{"x":40,"y":120},"name":"domains","direction":"in"},' +
    '{"id":"a1","kind":"actor","at":{"x":260,"y":120},"actor":"nscheck","version":"0.1.0","method":"delegation"},' +
    '{"id":"a2","kind":"actor","at":{"x":480,"y":120},"actor":"nscheck","version":"0.1.0","method":"ask"},' +
    '{"id":"a3","kind":"actor","at":{"x":480,"y":300},"actor":"probe","version":"0.1.0","method":"head"},' +
    '{"id":"d2","kind":"dataset","at":{"x":700,"y":120},"name":"lame","direction":"out"}],' +
    '"edges":[{"id":"e1","from":"d1","to":"a1","label":"a page at a time"},' +
    '{"id":"e2","from":"a1","to":"a2"},{"id":"e3","from":"a2","to":"d2"},' +
    '{"id":"e4","from":"a2","to":"a3","label":"only the ones that answered"}],' +
    '"notes":[{"id":"n1","at":{"x":40,"y":420},"text":"page 200 at a time — 400 units through one activity is past the measured guard"},' +
    '{"id":"n2","at":{"x":260,"y":220},"text":"this fan-out is about 4x: one domain becomes one unit per nameserver","on":"a1"}]}';

  it('parses back to exactly the bytes it was stored as', () => {
    // The store keeps `JSON.stringify(parseScratchDocument(...))`. If a read reordered or renamed
    // anything, every open would rewrite the row and no diff would ever mean an edit.
    expect(JSON.stringify(parseScratchDocument(JSON.parse(SAVED)))).toBe(SAVED);
  });

  it('renders the spec it renders today, character for character', () => {
    const rec = record(parseScratchDocument(JSON.parse(SAVED)), 'lame nameserver sweep');
    expect(renderScratch(resolveScratch(rec, CATALOG), CATALOG)).toBe(
      [
        '# lame nameserver sweep',
        '',
        'A sketch of an orchestration, drawn in kontra. Build it as a CALLER WORKFLOW — a Python ' +
          'module with an `@workflow.defn` class that dispatches the Actors below. Nothing here ' +
          'runs; this is the shape somebody drew, and the code is what will run.',
        '',
        '## What it uses',
        '',
        '- **dataset** `domains` — read from',
        "- **actor** `nscheck@0.1.0` method `delegation` — Resolve each domain's delegated NS set",
        '- **actor** `nscheck@0.1.0` method `ask` — Ask one nameserver whether it serves the zone',
        '- **actor** `probe@0.1.0` method `head`',
        '- **dataset** `lame` — written to',
        '',
        '## How it flows',
        '',
        '- `domains` (dataset) → `nscheck@0.1.0.delegation`  _(a page at a time)_',
        '- `nscheck@0.1.0.delegation` → `nscheck@0.1.0.ask`',
        '- `nscheck@0.1.0.ask` → `lame` (dataset)',
        '- `nscheck@0.1.0.ask` → `probe@0.1.0.head`  _(only the ones that answered)_',
        '',
        '## What the author said',
        '',
        '- page 200 at a time — 400 units through one activity is past the measured guard',
        '- on `nscheck@0.1.0.delegation`: this fan-out is about 4x: one domain becomes one unit ' +
          'per nameserver',
        '',
      ].join('\n')
    );
  });
});
