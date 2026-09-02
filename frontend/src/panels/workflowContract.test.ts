/**
 * A registered workflow's contract, read and drawn.
 *
 * BOTH READINGS EXIST IN THE SHIPPED WORKFLOWS. `ping` annotates `dict`, which pydantic derives as
 * a schema with no `properties` — "any object"; drawn as an empty field table it reads as "this
 * workflow takes an object with no fields in it", the opposite of what it says. `nscheck` now names
 * its request (`NsCheckInput`, instrument-panel slice 01), so its input derives `properties` and
 * draws a field table. The fixtures here are the real emissions (see tests/test_workflow_catalog.py,
 * which pins the same documents from the Python side), not documents written to make this pass.
 *
 * `renderToStaticMarkup` for the reason `HealthChips.test.ts` records: this suite runs in node with
 * no jsdom and no testing-library, and every assertion is about text and `data-testid`, both of
 * which are in the markup.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { WorkflowContract } from './WorkflowContract';
import { readSchema } from './workflowContract';
import type { WorkflowDescriptor } from '../run/api';

/** What `.kontra/workflows/ping` really registers: `dict | None` in, `dict` out. */
const PING: WorkflowDescriptor = {
  name: 'Ping',
  input: { anyOf: [{ type: 'object', additionalProperties: true }, { type: 'null' }] },
  output: { type: 'object', additionalProperties: true },
  savedAt: 1_700_000_000_000,
};

const TYPED: WorkflowDescriptor = {
  name: 'Sweep',
  description: 'Check every domain delegation.',
  input: {
    type: 'object',
    properties: { dataset: { type: 'string' }, machines: { type: 'integer' } },
    required: ['dataset'],
  },
  output: { type: 'object', properties: { checked: { type: 'integer' } } },
  savedAt: 1_700_000_000_000,
};

/**
 * What `examples/python/workflows/nscheck` really registers after slice 01: `NsCheckInput` in — a
 * `TypedDict` naming the five knobs its body reads, none required (`total=False`) — and `dict` out.
 * Byte-for-byte the descriptor `workflow_descriptor(NsCheck)` emits; the input's field table is the
 * whole point of the slice, and the output still falls through the no-fields path on the same card.
 */
const NSCHECK: WorkflowDescriptor = {
  name: 'NsCheck',
  input: {
    type: 'object',
    properties: {
      dataset: { type: 'string' },
      into: { type: 'string' },
      machines: { type: 'integer' },
      sessions: { type: 'integer' },
      size: { type: 'integer' },
    },
  },
  output: { type: 'object', additionalProperties: true },
  savedAt: 1_700_000_000_000,
};

const draw = (type: string, descriptor?: WorkflowDescriptor): string =>
  renderToStaticMarkup(createElement(WorkflowContract, { type, descriptor }));

describe('readSchema', () => {
  it('tells an undeclared slot from one that declares no fields', () => {
    // The distinction the whole surface turns on: `undefined` is "nobody wrote a type down", a
    // property-less document is "any object". One of those is the author's omission and the other
    // is the author's choice.
    expect(readSchema(undefined)).toEqual({ kind: 'undeclared' });
    expect(readSchema({ type: 'object', additionalProperties: true })).toEqual({ kind: 'no-fields' });
  });

  it('reads an anyOf — what `dict | None` derives — as declaring no fields', () => {
    expect(readSchema(PING.input)).toEqual({ kind: 'no-fields' });
  });

  it('reads a declared `properties: {}` as no fields too', () => {
    // A schema that declares the shape and leaves it open. Same answer, same sentence: there is no
    // table to draw and the reader must not be shown an empty one.
    expect(readSchema({ type: 'object', properties: {} })).toEqual({ kind: 'no-fields' });
  });

  it('flattens the ordinary case into fields, carrying required', () => {
    // Each field is a NODE of the tree the form draws (`schemaTree.ts`): the kind says which control
    // collects it, the path says where its value lives. A flat schema is all leaves, and a top-level
    // path is its own name — which is why the contract table reads exactly as it always did.
    const reading = readSchema(TYPED.input);
    expect(reading.kind).toBe('fields');
    expect(reading.kind === 'fields' && reading.fields).toEqual([
      { kind: 'leaf', path: 'dataset', name: 'dataset', type: 'string', required: true },
      { kind: 'leaf', path: 'machines', name: 'machines', type: 'integer', required: false },
    ]);
  });
});

describe('the contract, drawn', () => {
  it('says a dict-annotated workflow DECLARES NO FIELDS, and draws no table', () => {
    const html = draw('Ping', PING);
    expect(html).toContain('declares no fields');
    expect(html).not.toContain('not declared');
    // An empty <ul> is the bug this exists to prevent: a field table with no rows under a column
    // header reads as "takes an object with nothing in it".
    expect(html).not.toContain('<ul');
  });

  it('draws the fields, and the sentence the author wrote, when there are some', () => {
    const html = draw('Sweep', TYPED);
    expect(html).toContain('Check every domain delegation.');
    expect(html).toContain('dataset');
    expect(html).toContain('machines');
    expect(html).toContain('required');
  });

  it('draws nscheck a field table for its typed input, while its dict output keeps the no-fields path', () => {
    // The slice-01 payoff, drawn: the input names five knobs so the Workflows page can generate a
    // form from it, and the OUTPUT — still `dict` — legitimately says "declares no fields" on the
    // same card. Both readings on one shipped workflow, which is what makes slices 02/03 visible.
    const html = draw('NsCheck', NSCHECK);
    for (const knob of ['dataset', 'into', 'machines', 'sessions', 'size']) {
      expect(html).toContain(knob);
    }
    // A real table drew for the input (an actual <ul>, the thing PING must never grow), and nothing
    // in this card claims the schema is undeclared.
    expect(html).toContain('<ul');
    expect(html).not.toContain('not declared');
    // The output is `dict`, so the no-fields sentence still appears — exactly once, for that slot.
    expect(html.split('declares no fields').length - 1).toBe(1);
  });

  it('draws nothing where an undescribed workflow description would be', () => {
    // Absent is absent. A placeholder would report the author's silence as a fault of the system,
    // when it is one sentence in a docstring away from being fixed by whoever is reading.
    const html = draw('Ping', PING);
    expect(html).not.toContain('data-testid="workflow-description"');
  });

  it('says an unannotated slot is NOT DECLARED, which is a different sentence', () => {
    const html = draw('Untyped', { name: 'Untyped', savedAt: 1 });
    expect(html).toContain('not declared');
    expect(html).not.toContain('declares no fields');
  });

  it('says why there is no contract at all, rather than claiming the workflow takes nothing', () => {
    // The common state: a workflow that has never been served has told the catalog nothing. Drawing
    // "takes nothing" here would be an invention, and the operator can fix it with the button above.
    const html = draw('NsCheck', undefined);
    expect(html).toContain('data-testid="workflow-contract-absent"');
    expect(html).toContain('NsCheck');
    expect(html).toContain('SERVES');
  });

  it('says the file no longer imports, with the error, instead of the last good form', () => {
    // Instrument-panel slice 03: a broken file is a STATE, not a silence. A watch-mode serve that
    // fails to re-import posts an `error` with no schema, and the panel must draw that state — the
    // loop doubles as a liveness check on the operator's own code. Drawing the last good form here
    // would read as "your contract is fine" over a file that will not load.
    const html = draw('NsCheck', {
      name: 'NsCheck',
      queue: 'wf-nscheck-abc',
      error: 'workflow.py no longer imports: SyntaxError: unexpected EOF while parsing (line 12)',
      savedAt: 1,
    });
    expect(html).toContain('data-testid="workflow-contract-broken"');
    expect(html).toContain('this file no longer imports');
    expect(html).toContain('SyntaxError: unexpected EOF while parsing (line 12)');
    // The broken state REPLACES the contract: no field table, and none of the schema slots the good
    // card draws.
    expect(html).not.toContain('data-testid="workflow-contract"');
    expect(html).not.toContain('<ul');
    expect(html).not.toContain('declares no fields');
  });

  it('draws the contract again once the error clears — recovery restores the form', () => {
    // The recovering half of the loop: a clean re-derivation carries the schema and no error, so the
    // same type that was broken a moment ago draws its field table again. No page reload, no restart.
    const html = draw('NsCheck', NSCHECK);
    expect(html).toContain('data-testid="workflow-contract"');
    expect(html).not.toContain('workflow-contract-broken');
    expect(html).toContain('dataset');
  });
});
