/**
 * A run's input, collected the way the Actors page collects a Batch — but as ONE OBJECT, not a list.
 *
 * A RUN IS ONE EXECUTION OF A WORKFLOW, and a workflow's `@workflow.run` takes ONE argument. So where
 * `methodCall.ts` builds a Batch (a list of Units), this builds a single value — and that is the only
 * difference between them worth a second module. Everything under the surface — a field per declared
 * property, a value coerced by its declared type or refused where it cannot be, an empty optional
 * left OUT rather than sent as `""` — is the SHARED core in `formFields.ts`, reached through
 * `coerceValues`. A copy of that logic here is the thing this file exists NOT to be.
 *
 * A DECLARED INPUT BECOMES FIELDS; ANYTHING ELSE STAYS JSON. The reading is `readSchema`, the SAME
 * one `WorkflowContract` draws the field table from (`workflowContract.ts`), so the form can never
 * offer a field the contract above it does not show. Its three answers collapse to two here: only a
 * schema with `properties` gets a form, and both `undeclared` (nobody wrote a type down) and
 * `no-fields` (`dict`, "any object") fall through to the raw box — carrying WHICH of the two, because
 * the reason a run's input is free-form is worth saying and the two reasons are different.
 *
 * BLANK IS NO ARGUMENT, which is neither `null` nor `{}`. A workflow whose `run(self)` takes nothing
 * must be startable, so an empty JSON box yields `undefined` — `startRun` then posts no `input` at
 * all. `parseInput` already owns that rule for the raw box (`workflowSource.ts`); the fields box owns
 * the other end, where an empty optional-only form is the empty object the workflow's `req.get(...)`
 * reads its defaults out of.
 */

import type { JsonSchema } from '../types';
import type { FieldNode } from './schemaTree';
import { readSchema } from './workflowContract';
import { initialValues, coerceValues, setPath, type FieldValues } from './formFields';
import { parseInput } from './workflowSource';

/** The run input under construction: a form when the workflow declares its input, raw JSON when not. */
export type InputDraft = InputFieldsDraft | InputJsonDraft;

export interface InputFieldsDraft {
  kind: 'fields';
  /** The workflow's declared fields, in the order the contract lists them — the whole tree, since
   *  a nested object is drawn as nested fields rather than as a JSON box. */
  fields: FieldNode[];
  /** The one object under construction, keyed by PATH; every value is the string in the input. */
  values: FieldValues;
}

export interface InputJsonDraft {
  kind: 'json';
  text: string;
  /**
   * Why there is a raw box and not a form — so the page can say it. `undeclared` is "nobody wrote a
   * type down", `no-fields` is a `dict`/`anyOf` that declares an open shape; the operator can fix the
   * first with a type annotation and is meant to leave the second alone.
   */
  why: 'undeclared' | 'no-fields';
}

/** The run's argument, or the first reason it is not one yet. `value` absent means NO argument. */
export type InputResult = { value?: unknown } | { error: string };

/**
 * The form for a workflow's input schema.
 *
 * Only a schema with `properties` gets fields; an undeclared or property-less one gets the JSON box,
 * carrying which it is. An empty text is the honest default there — blank means "no argument", and a
 * workflow that takes one will have said so with a type the form would then have drawn.
 */
export function draftForInput(schema?: JsonSchema): InputDraft {
  const reading = readSchema(schema);
  if (reading.kind === 'fields') {
    return { kind: 'fields', fields: reading.fields, values: initialValues(reading.fields) };
  }
  return { kind: 'json', text: '', why: reading.kind };
}

/** The operator typed in one input. `path` is a bare field name at the top level, which is why every
 *  flat-form caller of this reads unchanged. */
export function setField(draft: InputDraft, path: string, value: string): InputDraft {
  if (draft.kind !== 'fields') return draft;
  return { ...draft, values: setPath(draft.values, path, value) };
}

/**
 * The whole record at once — what a nested group or a repeatable row hands back.
 *
 * ADDING OR DROPPING A ROW IS NOT ONE KEY. `addRow` writes a row's whole seed and `removeRow`
 * re-addresses every row after the one that went, so the reducers in `formFields.ts` return the map
 * and this only has to put it back on the draft.
 */
export function setValues(draft: InputDraft, values: FieldValues): InputDraft {
  return draft.kind === 'fields' ? { ...draft, values } : draft;
}

export function setInputJson(draft: InputDraft, text: string): InputDraft {
  return draft.kind === 'json' ? { ...draft, text } : draft;
}

/**
 * The argument this form collected, or the first field that refuses.
 *
 * The fields path is the shared coercion (`coerceValues`) with no Unit to prefix — a workflow has
 * one input, so the error is the field's own sentence. The JSON path is `parseInput`, which already
 * reads a blank box as "no argument" and any non-blank text as that workflow's one argument, whatever
 * its JSON shape — an object, a list, a scalar. There is no "wrap it in a list" rule here: that is a
 * fact about a Batch, and a run is not one.
 */
export function inputOf(draft: InputDraft): InputResult {
  if (draft.kind === 'json') {
    const parsed = parseInput(draft.text);
    return parsed.ok ? { value: parsed.value } : { error: `the input is not JSON: ${parsed.error}` };
  }
  return coerceValues(draft.fields, draft.values);
}

/**
 * The `--input '...'` fragment of the start command, reflecting exactly what the form collected.
 *
 * Empty when there is no argument (a blank box, or a form that coerced to nothing worth sending) and
 * empty when the input does not yet parse — the command line is a thing to copy and run, so it names
 * only an input that would actually start. What it prints is the coerced value, not the raw strings,
 * so `machines 4` reads as `"machines": 4` and not `"machines": "4"` — the same value the Run button
 * posts.
 */
export function inputArg(draft: InputDraft): string {
  const got = inputOf(draft);
  if ('error' in got || got.value === undefined) return '';
  return ` --input '${JSON.stringify(got.value)}'`;
}
