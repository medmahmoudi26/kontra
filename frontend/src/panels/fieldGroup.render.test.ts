/**
 * A nested form, DRAWN — every state an operator can put it in, and both surfaces that draw it.
 *
 * `renderToStaticMarkup` for the reason `HealthChips.test.ts` records: this suite runs in node with
 * no jsdom and no testing-library, and every assertion here is about text and `data-testid`, both of
 * which are in the markup. `FieldGroup` takes props and returns markup precisely so it can be drawn
 * here; the two pages that hold it are the halves that fetch and hold state.
 *
 * THE LAST TWO DESCRIBES ARE THE ANTI-DRIFT TESTS, and they are the reason this file draws the whole
 * Actors panel and the whole Workflows form rather than only the component between them. A shared
 * cell that is only shared in a diagram is not shared: the failure it prevents is a `<select>` that
 * grows a third state on one page, or a nested object that stays a JSON textarea on the page nobody
 * happened to open — invisible in either page's own suite, because each one passes on its own.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { FieldGroup } from './FieldGroup';
import { MethodCallPanel } from './MethodCallPanes';
import { WorkflowInputForm } from './WorkflowInputForm';
import { schemaTree, type FieldNode } from './schemaTree';
import { addRow, initialValues, type FieldValues } from './formFields';
import { draftFor, toggleUnit, unitsOf, type BatchDraft } from './methodCall';
import { draftForInput, inputOf } from './workflowInput';
import type { ActorOperation, CatalogActor } from '../types';

const NESTED = {
  type: 'object',
  properties: {
    url: { type: 'string' },
    retry: {
      type: 'object',
      properties: {
        tries: { type: 'integer', default: 3 },
        mode: { type: 'string', enum: ['fast', 'slow'] },
      },
    },
    open: { type: 'object' },
    undeclared: {},
    empty: { type: 'object', properties: {} },
    ports: { type: 'array', items: { type: 'integer' } },
    targets: {
      type: 'array',
      items: {
        type: 'object',
        properties: { host: { type: 'string' }, port: { type: 'integer', default: 80 } },
        required: ['host'],
      },
    },
  },
  required: ['url'],
};

const tree = (schema: Record<string, unknown>): FieldNode[] => schemaTree(schema)!;
const node = (nodes: FieldNode[], name: string): FieldNode => nodes.find((n) => n.name === name)!;

const draw = (nodes: FieldNode[], values: FieldValues = initialValues(nodes)): string =>
  renderToStaticMarkup(
    createElement(FieldGroup, { nodes, values, onValues: () => {}, testPrefix: 'f' })
  );

describe('a nested object is a nested GROUP, not a JSON textarea', () => {
  it('draws an input per nested field, addressed by path', () => {
    const html = draw(tree(NESTED));
    expect(html).toContain('data-testid="f-group-retry"');
    expect(html).toContain('data-testid="f-retry.tries"');
    expect(html).toContain('data-testid="f-retry.mode"');
    // The thing this slice removes: a nested object collected as a one-line JSON paste.
    expect(html).not.toContain('data-testid="f-retry"');
  });

  it('prefills a nested field with its declared default', () => {
    expect(draw(tree(NESTED))).toContain('data-testid="f-retry.tries" spellcheck="false" value="3"');
  });

  it('offers a closed set nested inside a group as a LIST, through the same cell', () => {
    // The value cannot be wrong because the wrong values were never on the screen — at any depth.
    const html = draw(tree(NESTED));
    expect(html).toMatch(/<select[^>]*data-testid="f-retry\.mode"/);
    expect(html).toContain('>fast</option>');
    expect(html).toContain('>slow</option>');
  });

  it('keeps nesting legible past the second level', () => {
    const html = draw(
      tree({
        properties: {
          a: { type: 'object', properties: { b: { type: 'object', properties: { c: { type: 'string' } } } } },
        },
      })
    );
    expect(html).toContain('data-testid="f-group-a"');
    expect(html).toContain('data-testid="f-group-a.b"');
    expect(html).toContain('data-testid="f-a.b.c"');
  });
});

describe('the three answers are three renderings, at depth', () => {
  const html = draw(tree(NESTED));

  it('says NOT DECLARED for a field nobody typed, and names the fix', () => {
    // An author's omission is one annotation away from a form, and naming it is how it gets fixed.
    expect(html).toContain('data-testid="f-why-undeclared"');
    expect(html).toContain('not declared');
    expect(html).toContain('A type annotation here turns it into fields');
  });

  it('says something ELSE for a declared-open object — that one is a choice, not a fault', () => {
    expect(html).toContain('data-testid="f-why-open"');
    expect(html).toContain('declares an open shape');
  });

  it('says NO FIELDS for a group that declares none — a third sentence, not a blank', () => {
    expect(html).toContain('data-testid="f-nofields-empty"');
    expect(html).toContain('no fields');
  });

  it('gives both JSON cases a box and neither of them a group', () => {
    expect(html).toContain('data-testid="f-open"');
    expect(html).toContain('data-testid="f-undeclared"');
    expect(html).not.toContain('data-testid="f-group-open"');
    expect(html).not.toContain('data-testid="f-group-undeclared"');
  });
});

describe('an array is a repeatable row, with add and remove', () => {
  it('draws no rows and says so when there are none yet', () => {
    const html = draw(tree(NESTED));
    expect(html).toContain('data-testid="f-empty-ports"');
    expect(html).toContain('no items yet');
    expect(html).toContain('data-testid="f-add-ports"');
    // Nothing to type into: an empty list is zero inputs, not one blank one.
    expect(html).not.toContain('data-testid="f-ports.0"');
    expect(html).not.toContain('data-testid="f-remove-ports.0"');
  });

  it('draws one input and one remove per row of scalars', () => {
    const nodes = tree(NESTED);
    const values = addRow(node(nodes, 'ports'), addRow(node(nodes, 'ports'), initialValues(nodes)));
    const html = draw(nodes, values);
    expect(html).toContain('data-testid="f-ports.0"');
    expect(html).toContain('data-testid="f-ports.1"');
    expect(html).toContain('data-testid="f-remove-ports.0"');
    expect(html).toContain('data-testid="f-remove-ports.1"');
    expect(html).not.toContain('data-testid="f-ports.2"');
    expect(html).not.toContain('data-testid="f-empty-ports"');
  });

  it('draws the ELEMENT SCHEMA’s own fields on every row of a list of objects', () => {
    const nodes = tree(NESTED);
    const values = addRow(node(nodes, 'targets'), addRow(node(nodes, 'targets'), initialValues(nodes)));
    const html = draw(nodes, values);
    for (const id of ['f-targets.0.host', 'f-targets.0.port', 'f-targets.1.host', 'f-targets.1.port']) {
      expect(html).toContain(`data-testid="${id}"`);
    }
    // The element's declared default reaches every row, not just the first.
    expect(html).toContain('data-testid="f-targets.1.port" spellcheck="false" value="80"');
    // And its required field is marked on every row, because row 4 refuses the way row 1 does.
    expect(html).toContain('required');
  });
});

describe('the Workflows run form draws the whole tree through the shared cell', () => {
  const drawInput = (schema?: Record<string, unknown>): string => {
    const draft = draftForInput(schema);
    return renderToStaticMarkup(
      createElement(WorkflowInputForm, { draft, onDraft: () => {}, result: inputOf(draft) })
    );
  };

  it('draws a nested group rather than a JSON box for a workflow that declares one', () => {
    const html = drawInput(NESTED);
    expect(html).toContain('data-testid="workflow-input-field-retry.tries"');
    expect(html).toContain('data-testid="workflow-input-field-group-retry"');
    expect(html).not.toContain('data-testid="workflow-input-json"');
  });

  it('keeps the flat form’s ids exactly, because a top-level path IS its field name', () => {
    expect(drawInput(NESTED)).toContain('data-testid="workflow-input-field-url"');
  });

  it('names the fix when a workflow declares no input at all', () => {
    // `undefined` is the ABSENT key — nobody wrote a type down. `{}` would be a declared open shape,
    // which is the other sentence and not an omission anybody should be told to fix.
    const html = drawInput(undefined);
    expect(html).toContain('data-testid="workflow-input-json"');
    expect(html).toContain('this becomes a form');
  });
});

describe('the Actors Batch draws the same tree, in the container a Batch needs', () => {
  const ACTOR: CatalogActor = {
    key: 'probe@0.1.0',
    name: 'probe',
    version: '0.1.0',
    schemaVersion: '1',
    operations: [],
    source: '/srv/checkout/examples/python/probe',
  };
  const OP: ActorOperation = { name: 'head', input: NESTED };

  const drawBatch = (draft: BatchDraft): string =>
    renderToStaticMarkup(
      createElement(MethodCallPanel, {
        actor: ACTOR,
        op: OP,
        draft,
        batch: unitsOf(draft),
        onDraft: () => {},
        caller: null,
        // A serving Actor, because this suite is about the FORM and a disabled Run button would
        // change nothing it asserts — `methodCall.render.test.ts` owns the serve states.
        serve: {
          state: 'serving' as const,
          words: { label: 'serving', title: 'a worker is polling probe-0.1.0 now.' },
          queue: 'probe-0.1.0',
          serving: 1,
        },
        busy: null,
        error: null,
        started: null,
        reading: null,
        onRun: () => {},
        onCopy: () => {},
        onOpenDataset: () => {},
        copied: false,
        onClose: () => {},
      })
    );

  it('keeps the scalar columns a table and offers the nested fields behind a disclosure', () => {
    // A `retry` object as a COLUMN would make every row as tall as the deepest thing in the schema,
    // and a table inside a table is unreadable at the second level.
    const html = drawBatch(draftFor(NESTED));
    expect(html).toContain('data-testid="method-call-field-0-url"');
    expect(html).toContain('data-testid="method-call-expand-0"');
    expect(html).not.toContain('data-testid="method-call-nested-0"');
    expect(html).not.toContain('data-testid="method-call-field-0-retry.tries"');
  });

  it('opens the nested fields under the Unit that owns them', () => {
    const html = drawBatch(toggleUnit(draftFor(NESTED), 0));
    expect(html).toContain('data-testid="method-call-nested-0"');
    expect(html).toContain('data-testid="method-call-field-0-retry.tries"');
    expect(html).toContain('data-testid="method-call-field-0-add-targets"');
    // Per Unit, not per Method: opening Unit 1 does not open Unit 2's.
    expect(html).not.toContain('data-testid="method-call-field-1-retry.tries"');
  });

  it('draws no disclosure at all for a Method that is all scalars', () => {
    const flat = { type: 'object', properties: { url: { type: 'string' } }, required: ['url'] };
    const html = drawBatch(draftFor(flat));
    expect(html).toContain('data-testid="method-call-field-0-url"');
    expect(html).not.toContain('data-testid="method-call-expand-0"');
  });

  it('refuses a nested required field by its path, with the Unit number the Batch adds', () => {
    // The path is what identifies `targets.1.host` in a Batch of forty; the `unit 1 · ` prefix is the
    // container's to add, which is why the shared core never spells it.
    const nodes = (draftFor(NESTED) as { fields: FieldNode[] }).fields;
    let draft = draftFor(NESTED);
    if (draft.kind === 'fields') {
      draft = { ...draft, units: [addRow(node(nodes, 'targets'), { url: 'https://a.test' })] };
    }
    expect(drawBatch(draft)).toContain('unit 1 · targets.0.host is required');
  });
});
