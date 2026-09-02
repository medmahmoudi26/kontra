/**
 * "Starting a run is a form" — drawn, every state an operator can put it in.
 *
 * `renderToStaticMarkup` for the reason `workflowContract.test.ts` records: the suite runs in node
 * with no jsdom, and every assertion here is about text and `data-testid`, both of which are in the
 * markup. `WorkflowInputForm` takes props and returns markup precisely so it can be drawn here, the
 * same split (and for the same reason) that keeps `WorkflowContract` out of `WorkflowsPage`.
 *
 * THE FORM AND THE CONTRACT ARE ONE READING. The fields this draws come from `draftForInput`, which
 * reads the schema through `readSchema` — the SAME reading `WorkflowContract` draws its table from.
 * The last test pins that: the names in the form are exactly `schemaFields`' names, so the two can
 * never disagree about what the workflow takes.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { WorkflowInputForm } from './WorkflowInputForm';
import { draftForInput, inputOf, setField, type InputDraft } from './workflowInput';
import { schemaFields } from './MethodContract';

const NSCHECK = {
  type: 'object',
  properties: {
    dataset: { type: 'string' },
    into: { type: 'string' },
    machines: { type: 'integer' },
    sessions: { type: 'integer' },
    size: { type: 'integer' },
  },
};

const TYPED = {
  type: 'object',
  properties: { dataset: { type: 'string' }, machines: { type: 'integer' } },
  required: ['dataset'],
};

const draw = (draft: InputDraft): string =>
  renderToStaticMarkup(
    createElement(WorkflowInputForm, { draft, onDraft: () => {}, result: inputOf(draft) })
  );

describe('a workflow that declares its input', () => {
  it('draws a labelled field per declared property, with its type', () => {
    const html = draw(draftForInput(NSCHECK));
    for (const knob of ['dataset', 'into', 'machines', 'sessions', 'size']) {
      expect(html).toContain(`data-testid="workflow-input-field-${knob}"`);
    }
    expect(html).toContain('integer');
    // Not the JSON fallback: a declared input is a form, never a box.
    expect(html).not.toContain('data-testid="workflow-input-json"');
  });

  it('marks a required field required', () => {
    const html = draw(draftForInput(TYPED));
    expect(html).toContain('data-testid="workflow-input-field-dataset"');
    expect(html).toContain('required');
  });

  it('names the field that will not start the run, and does not pretend it is JSON', () => {
    // `dataset` is required and blank, so the run is blocked — the block is drawn here, named, rather
    // than surfacing minutes later at the worker.
    const html = draw(draftForInput(TYPED));
    expect(html).toContain('data-testid="workflow-input-error"');
    expect(html).toContain('dataset is required');
  });

  it('names a value that cannot coerce, on the field it is on', () => {
    let draft = setField(draftForInput(TYPED), 'dataset', 'domains');
    draft = setField(draft, 'machines', 'lots');
    const html = draw(draft);
    expect(html).toContain('data-testid="workflow-input-error"');
    expect(html).toContain('machines: &quot;lots&quot; is not a number');
  });
});

describe('a workflow that declares nothing', () => {
  it('draws the raw JSON box and says why, for an undeclared input', () => {
    const html = draw(draftForInput(undefined));
    expect(html).toContain('data-testid="workflow-input-json"');
    expect(html).toContain('data-testid="workflow-input-why"');
    expect(html).toContain('declares no input');
    expect(html).not.toContain('data-testid="workflow-input-field-dataset"');
  });

  it('says the other sentence for a declared-open shape', () => {
    // `dict`/`anyOf` is a different fact from undeclared — the author chose an open shape rather than
    // omitting a type — and the box says so.
    const html = draw(draftForInput({ anyOf: [{ type: 'object' }, { type: 'null' }] }));
    expect(html).toContain('declares an open input');
  });
});

describe('the form and the contract are one reading', () => {
  it('offers exactly the fields schemaFields flattens, never one the contract table would not show', () => {
    // AC: the form and the contract table are generated from the same `schemaFields` reading. If they
    // ever drift, this is the test that catches it — the form can only draw what the flattening names.
    const names = (schemaFields(NSCHECK) ?? []).map((f) => f.name);
    const html = draw(draftForInput(NSCHECK));
    for (const name of names) {
      expect(html).toContain(`data-testid="workflow-input-field-${name}"`);
    }
    // And nothing beyond them — the count of field inputs equals the count of flattened fields.
    const drawn = html.match(/data-testid="workflow-input-field-/g) ?? [];
    expect(drawn).toHaveLength(names.length);
  });
});
