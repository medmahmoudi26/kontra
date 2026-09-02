/**
 * A run's input, collected as a form — one OBJECT, not a Batch's list.
 *
 * THIS IS THE SAME CORE THE ACTORS PAGE COERCES A BATCH WITH, exercised through the other container.
 * `coerceValues` lives in `formFields.ts` and both `methodCall.ts` (a list of Units) and this
 * (`workflowInput.ts`, one argument) reach it — so a value that will not coerce is refused the same
 * way on both surfaces, and the difference this file pins is only the shape: a workflow's `run` takes
 * one thing, and a blank form is that one thing with its optionals left out, not an empty list.
 *
 * The schemas are the real emissions from `internals/catalog.py` — `NSCHECK` is what
 * `examples/python/workflows/nscheck` registers after instrument-panel slice 01 (five knobs, none
 * required), `PING` is what `dict | None` derives, and `TYPED` carries a required field so the
 * refusal path has one to name.
 */

import { describe, expect, it } from 'vitest';
import {
  draftForInput,
  inputArg,
  inputOf,
  setField,
  setInputJson,
  type InputDraft,
} from './workflowInput';

/** The run's argument, or the reason there is not one — flattened so a test can say which it meant. */
function run(draft: InputDraft): unknown {
  const got = inputOf(draft);
  if ('error' in got) return `REFUSED: ${got.error}`;
  return 'value' in got && got.value !== undefined ? got.value : 'NO ARGUMENT';
}

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

const PING = { anyOf: [{ type: 'object', additionalProperties: true }, { type: 'null' }] };

describe('a workflow that declares its input', () => {
  it('becomes a field per property, in the order the contract lists them', () => {
    const draft = draftForInput(NSCHECK);
    expect(draft.kind).toBe('fields');
    if (draft.kind !== 'fields') return;
    expect(draft.fields.map((f) => f.name)).toEqual([
      'dataset',
      'into',
      'machines',
      'sessions',
      'size',
    ]);
  });

  it('coerces every value by its declared type, because an input only holds strings', () => {
    // `"4"` where the workflow wants an int is a start that fails at the worker, not here — unless it
    // is coerced here, which is the whole reason the box became a form.
    let draft = draftForInput(NSCHECK);
    draft = setField(draft, 'dataset', 'domains');
    draft = setField(draft, 'machines', '4');
    draft = setField(draft, 'sessions', '2');
    expect(run(draft)).toEqual({ dataset: 'domains', machines: 4, sessions: 2 });
  });

  it('leaves an untyped optional field OUT rather than sending an empty string', () => {
    // A workflow reading `req.get("into")` wants a MISSING key to mean "use the default", which `""`
    // is not. An all-optional form filled with nothing is the empty object those defaults read from.
    expect(run(draftForInput(NSCHECK))).toEqual({});
  });

  it('refuses a required field nobody filled in, and names it — with no Unit number', () => {
    // The Batch names the Unit as well (`unit 1 · …`) because it has many; a run has one argument, so
    // the field's own sentence is the whole error. Same core, different container.
    expect(run(draftForInput(TYPED))).toBe('REFUSED: dataset is required');
  });

  it('refuses a value that cannot coerce, and names the field', () => {
    let draft = setField(draftForInput(TYPED), 'dataset', 'domains');
    draft = setField(draft, 'machines', 'lots');
    expect(run(draft)).toBe('REFUSED: machines: "lots" is not a number');
  });
});

describe('a workflow that declares nothing', () => {
  it('gets the raw JSON box, and says WHY — an undeclared input is the author’s omission', () => {
    const draft = draftForInput(undefined);
    expect(draft.kind).toBe('json');
    expect(draft.kind === 'json' && draft.why).toBe('undeclared');
  });

  it('gets the raw box for a `dict`/`anyOf` too, told apart as a declared-open shape', () => {
    // `ping` annotates `dict`, which derives properties-less "any object". A field table with zero
    // columns would claim it takes nothing; the box says it takes an open shape instead.
    const draft = draftForInput(PING);
    expect(draft.kind).toBe('json');
    expect(draft.kind === 'json' && draft.why).toBe('no-fields');
  });

  it('reads a blank box as NO ARGUMENT — not `null`, not `{}`', () => {
    // A workflow whose `run(self)` takes nothing must be startable, and `startRun` posts no `input`
    // when there is none. `{}` would be an argument it does not accept.
    expect(run(draftForInput(undefined))).toBe('NO ARGUMENT');
  });

  it('takes the argument as typed — any JSON shape, no wrap-it-in-a-list rule', () => {
    // A run is not a Batch: its one argument can be an object, a list, or a scalar, and there is no
    // "wrap it in [ ]" here because there is no list to be the outside of.
    expect(run(setInputJson(draftForInput(undefined), '{"dataset": "a"}'))).toEqual({ dataset: 'a' });
    expect(run(setInputJson(draftForInput(undefined), '"just a string"'))).toBe('just a string');
    expect(run(setInputJson(draftForInput(PING), '[1, 2, 3]'))).toEqual([1, 2, 3]);
  });

  it('says it is not JSON, with the parser’s own complaint', () => {
    expect(String(run(setInputJson(draftForInput(undefined), '{dataset: 1}')))).toContain(
      'the input is not JSON'
    );
  });
});

describe('the --input fragment reflects what the form collected', () => {
  it('prints the coerced value, so an int reads as an int and not a quoted string', () => {
    // The failure the form exists to catch, pinned in the command line too: `machines 4` must print
    // `"machines":4`, the value the Run button posts, never `"machines":"4"`.
    let draft = draftForInput(NSCHECK);
    draft = setField(draft, 'dataset', 'domains');
    draft = setField(draft, 'machines', '4');
    expect(inputArg(draft)).toBe(` --input '{"dataset":"domains","machines":4}'`);
  });

  it('prints the JSON box verbatim once it parses', () => {
    expect(inputArg(setInputJson(draftForInput(undefined), '{"dataset": "a"}'))).toBe(
      ` --input '{"dataset":"a"}'`
    );
  });

  it('is empty when there is no argument, and empty while the input does not start', () => {
    // The line is a thing to copy and run, so it names only an input that would actually start.
    expect(inputArg(draftForInput(undefined))).toBe('');
    expect(inputArg(draftForInput(TYPED))).toBe(''); // dataset required and blank — not startable
  });
});
