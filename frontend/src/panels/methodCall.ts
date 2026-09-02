/**
 * The Batch a Method is called with — and how to read what the call came back with.
 *
 * CALLING A METHOD CALLS THE METHOD (ADR 0033). What this collects is dispatched: a kontra-owned
 * one-shot workflow performs exactly ONE Method call over it, through the same Nexus operation
 * production uses, and writes to an untagged Dataset. The same Batch is also what
 * `backend/src/actorControl.ts:callerFor` embeds in the caller shown read-only beside the Run
 * button — one Batch, two readings of it, and they cannot come apart because they are one value.
 *
 * THAT IS NOT THE INTERPRETER, and the distinction is a COUNT: one Actor, one version, one Method,
 * one Batch. ADR 0023 §12 removed a general interpreter executing user-composed topologies; the
 * probe composes nothing and has no field for a second Method (ADR 0033 §1). Two Methods is a
 * topology and belongs in the operator's own workflow — which is what the artefact beside the
 * button is for.
 *
 * A DECLARED SCHEMA BECOMES FIELDS; NOTHING DECLARED STAYS JSON. The two cases are different facts
 * about the Actor and not two levels of polish: `examples/go/dnsfacts` and `examples/go/nscheck`
 * declare no types at all, so a form derived from nothing would have been an empty box claiming the
 * Method takes nothing — the one reading the Actors page exists to prevent. The fields come from the
 * SAME reading the card's field table draws (`schemaTree`, whose top level IS `schemaFields`),
 * because a form offering a field the table above it does not show is a form about a different
 * Method. The table stops at the top level; the form goes all the way down.
 *
 * A UNIT IS A ROW, AND THE BATCH IS THE ROWS. `head(batch)` takes a list; a form that collected one
 * object and quietly wrapped it would teach the shape wrong on the surface whose whole job is to
 * teach the shape — and `len(batch)` for a dict is its number of KEYS, so the counts the run
 * reports would be wrong too. The server refuses the same shape for the same reason
 * (`backend/src/probe.ts`), because this module is not the only way to reach that route.
 *
 * VALUES ARE COERCED BY THE DECLARED TYPE, and refused when they cannot be. Every input in a browser
 * is a string; sending `"8080"` where the Actor's model wants an int is a dispatch that fails at the
 * far end of a `kontra workflow serve`, minutes later, in a tmux pane. Failing here names the field.
 */

import type { JsonSchema } from '../types';
import { schemaTree, type FieldNode } from './schemaTree';
import { initialValues, coerceValues, setPath, type FieldValues } from './formFields';

// The per-value coercion is SHARED with the Workflows page's run form (`workflowInput.ts`), which
// collects the same typed strings into one object rather than a list of Units. Re-exported so the
// callers and tests that reach for it through this module keep their import.
export { coerceField } from './formFields';

/** The Batch under construction: a table when the Method declares its input, raw JSON when not. */
export type BatchDraft = FieldsDraft | JsonDraft;

export interface FieldsDraft {
  kind: 'fields';
  /** The Method's declared fields, in the order the card lists them — the whole TREE, since a
   *  nested object is drawn as nested fields and an array as repeatable rows. */
  fields: FieldNode[];
  /** One entry per Unit, keyed by PATH; every value is the string that is in the input. */
  units: FieldValues[];
  /**
   * Which Units have their nested fields open, by position.
   *
   * NESTING IN A TABLE IS AN EXPANDABLE ROW, NOT A TABLE IN A TABLE. A Batch is a list and the form
   * is the list, so a `retry` object cannot be a column without making every row as tall as the
   * deepest schema in it. The leaf columns stay a table an operator can scan down; the rest opens
   * under the row that owns it. It lives on the DRAFT rather than in the panel because the panel is
   * markup-out only — that is what lets a render test draw a Unit both ways.
   */
  expanded: number[];
}

export interface JsonDraft {
  kind: 'json';
  text: string;
}

/** Either the Batch, or the one sentence saying why there is not one yet. */
export type BatchResult = { units: unknown[] } | { error: string };

/** What an empty JSON Batch looks like before anybody types: a list, holding one Unit. */
const JSON_TEMPLATE = '[\n  {}\n]';

/**
 * The form for this Method's input schema.
 *
 * A DECLARED SCHEMA WITH NO PROPERTIES FALLS BACK TO JSON. `{"type": "object"}` with nothing under
 * `properties` is a Method whose author declared the shape and left it open; a field table with zero
 * columns would offer no way to type anything into it at all.
 */
export function draftFor(schema?: JsonSchema): BatchDraft {
  const fields = schemaTree(schema);
  if (fields === null || fields.length === 0) return { kind: 'json', text: JSON_TEMPLATE };
  return { kind: 'fields', fields, units: [initialValues(fields)], expanded: [] };
}

/** The operator typed in one cell. `path` is a bare field name for a top-level column, which is why
 *  every caller and test written against the flat table reads unchanged. */
export function setCell(draft: BatchDraft, unit: number, path: string, value: string): BatchDraft {
  if (draft.kind !== 'fields' || !draft.units[unit]) return draft;
  return {
    ...draft,
    units: draft.units.map((u, i) => (i === unit ? setPath(u, path, value) : u)),
  };
}

/**
 * One Unit's whole record at once — what a nested group or a repeatable row hands back.
 *
 * ADDING OR DROPPING A ROW IS NOT ONE KEY. `addRow` writes a row's whole seed and `removeRow`
 * re-addresses every row after the one that went, so the reducers in `formFields.ts` return the map
 * and this only has to put it back on the Unit it belongs to.
 */
export function setUnitValues(draft: BatchDraft, unit: number, values: FieldValues): BatchDraft {
  if (draft.kind !== 'fields' || !draft.units[unit]) return draft;
  return { ...draft, units: draft.units.map((u, i) => (i === unit ? values : u)) };
}

/** Open or close one Unit's nested fields. */
export function toggleUnit(draft: BatchDraft, unit: number): BatchDraft {
  if (draft.kind !== 'fields') return draft;
  const open = draft.expanded.includes(unit);
  return {
    ...draft,
    expanded: open ? draft.expanded.filter((i) => i !== unit) : [...draft.expanded, unit],
  };
}

/** The columns a table can hold. Everything else is nested and opens under its row. */
export function leafFields(fields: readonly FieldNode[]): FieldNode[] {
  return fields.filter((f) => f.kind === 'leaf');
}

/** The fields that cannot be a column — a nested object, a repeatable list. */
export function nestedFields(fields: readonly FieldNode[]): FieldNode[] {
  return fields.filter((f) => f.kind !== 'leaf');
}

/** One more Unit in the Batch — the whole reason the form is a table. */
export function addUnit(draft: BatchDraft): BatchDraft {
  if (draft.kind !== 'fields') return draft;
  return { ...draft, units: [...draft.units, initialValues(draft.fields)] };
}

/**
 * Drop one Unit.
 *
 * THE LAST ONE STAYS. A table with no rows has no inputs, so removing the only Unit would leave a
 * form that cannot be typed into and no control to get a row back except the one beside it —
 * clearing it is what "remove" means when there is one.
 */
export function removeUnit(draft: BatchDraft, unit: number): BatchDraft {
  if (draft.kind !== 'fields') return draft;
  if (draft.units.length <= 1) return { ...draft, units: [initialValues(draft.fields)], expanded: [] };
  return {
    ...draft,
    units: draft.units.filter((_, i) => i !== unit),
    // Expansion follows the ROW, not the number: the Units after the one that went move up, so a
    // reader who had Unit 3 open keeps looking at the same Unit rather than at its neighbour.
    expanded: draft.expanded.filter((i) => i !== unit).map((i) => (i > unit ? i - 1 : i)),
  };
}

export function setJson(draft: BatchDraft, text: string): BatchDraft {
  return draft.kind === 'json' ? { ...draft, text } : draft;
}

/**
 * The Batch, or the first reason it is not one.
 *
 * ONE REASON, NAMED. A list of every problem at once reads as a wall; the first one, with the Unit
 * and the field in it, is the one the operator is about to fix.
 */
export function unitsOf(draft: BatchDraft): BatchResult {
  if (draft.kind === 'json') {
    // AN EMPTY BOX IS AN EMPTY BATCH, not a mistake. `BATCH = []` is a legal generated file, and it
    // is what an operator who means to type the Units into the file itself wants.
    if (draft.text.trim() === '') return { units: [] };
    let parsed: unknown;
    try {
      parsed = JSON.parse(draft.text) as unknown;
    } catch (err) {
      return { error: `the Batch is not JSON: ${err instanceof Error ? err.message : String(err)}` };
    }
    // A bare object is the ordinary slip, and wrapping it silently would teach the shape wrong: the
    // Method takes a list, and the generated file reports `len(batch)`, which for a dict is its
    // number of KEYS.
    if (!Array.isArray(parsed)) {
      return { error: 'a Batch is a list of Units — wrap it in [ ]' };
    }
    return { units: parsed };
  }

  const units: unknown[] = [];
  for (const [i, row] of draft.units.entries()) {
    // The row → object coercion is the SHARED core (`formFields.coerceValues`): every value is a
    // string, an empty optional is an absent key, a required blank is a refusal. It names the FIELD;
    // the Unit number is this side's to add, because a workflow's one-object form has no Units to
    // number.
    const got = coerceValues(draft.fields, row);
    if ('error' in got) return { error: `unit ${i + 1} · ${got.error}` };
    units.push(got.value);
  }
  return { units };
}

/*
 * WHAT USED TO BE HERE, AND WHY IT IS NOT (ADR 0033 §6).
 *
 * `SavedCaller`, `callerCommands` and `shellArg` served one flow: write the generated file into a
 * registered Workflow folder, then print the two commands that serve and start it, retargeted at
 * wherever it landed. That flow is gone — the page calls the Method — and `saveTargets`, which
 * filtered the folders that could be written to, went with it.
 *
 * The two commands survive where they always really lived: inside the generated file's own
 * docstring, which the panel shows. Reading them back out of the source was worth a function only
 * while the page had to print them somewhere else; a copied file carries its own instructions.
 */


/**
 * WHAT THE PROBE FOUND, in one sentence — and the whole point is that four different findings do
 * not render as one.
 *
 * ADR 0028 §4 made `(results, dropped)` undestructurable-around precisely so a caller could not
 * skip past the drops, and ADR 0033's own consequence list says isolated Units are a probe's most
 * useful output and must be drawn. Zero rows has THREE distinct causes and they send an operator to
 * three different places:
 *
 *   DROPPED   the Method permanently isolated Units. Something in the Method raised on them, and
 *             the rows are fetchable for a retry. This outranks everything else on the screen.
 *   PARTIAL   the Method returned before covering its input — `done` is false. Not the same as
 *             dropping: nothing was lost, the producer simply stopped.
 *   EMPTY     it covered every Unit and found nothing. A successful answer, and the ONE reading
 *             that a bare "0 results" would otherwise be confused with. This is the failure that
 *             let a 15,814-target run report `completed` in seven minutes having scanned almost
 *             nothing.
 *   OK        rows came back.
 *
 * SEVERITY DECIDES which one is said, in that order, and every count is drawn beside it either way
 * — the sentence names the finding, the numbers keep it honest.
 */
export type ProbeTone = 'dropped' | 'partial' | 'empty' | 'ok';

export interface ProbeCounts {
  units: number;
  results: number;
  isolated: number;
  done: boolean;
}

export interface ProbeVerdict {
  tone: ProbeTone;
  headline: string;
  detail: string;
}

export function probeVerdict(r: ProbeCounts): ProbeVerdict {
  const rows = `${r.results} ${r.results === 1 ? 'row' : 'rows'}`;
  if (r.isolated > 0) {
    return {
      tone: 'dropped',
      headline: `${r.isolated} of ${r.units} ${r.units === 1 ? 'Unit was' : 'Units were'} dropped`,
      detail:
        `${rows} came back from the rest. A dropped Unit is one the Method permanently isolated — ` +
        'the rows are still there: `await dropped.rows()` in a caller hands them back for a retry.',
    };
  }
  if (!r.done) {
    return {
      tone: 'partial',
      headline: 'the Method returned before covering its input',
      detail:
        `${rows} from ${r.units} ${r.units === 1 ? 'Unit' : 'Units'}, and nothing was dropped — ` +
        'the producer stopped early rather than losing anything. Its own pane says why.',
    };
  }
  if (r.results === 0) {
    return {
      tone: 'empty',
      headline:
        r.units === 1
          ? 'no rows — the one Unit was covered and produced none'
          : `no rows — every one of the ${r.units} Units was covered`,
      detail:
        'This is an answer, not a failure: the Method ran over the whole Batch, dropped nothing, ' +
        'and produced no records. A run that dropped everything reads differently, above.',
    };
  }
  return {
    tone: 'ok',
    headline: `${rows} from ${r.units} ${r.units === 1 ? 'Unit' : 'Units'}`,
    detail: 'Nothing was dropped and the Method covered its input.',
  };
}
