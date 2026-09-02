/**
 * A typed input, collected as strings and handed back as the values its schema says they are.
 *
 * TWO SURFACES COLLECT THE SAME WAY AND MUST NOT DRIFT. The Actors page builds a Batch — a LIST of
 * Units — from a Method's declared input (`methodCall.ts`); the Workflows page builds ONE argument
 * from a workflow's declared input (`workflowInput.ts`). The container differs — a list of rows
 * versus a single object — but the row itself does not: every value in a browser input is a string,
 * an empty optional is an ABSENT key rather than `""`, a required one that is blank is a refusal,
 * and every other value is coerced by its declared type or named where it cannot be. That per-record
 * logic is what lives here, so neither surface owns a second copy of it to let rot apart. (A third
 * caller is coming — answering a HITL ask — and it is this same core, which is the point.)
 *
 * VALUES ARE ADDRESSED BY PATH, AND THE MAP STAYS FLAT. `retry.backoff` and `targets.1.host` are
 * keys in the same `Record<string, string>` a flat form always held, rather than a nested value
 * tree, so coercion stays a walk that reads leaves and only ASSEMBLY folds them back into the shape
 * the schema describes. A nested value tree would have made every reducer recursive for no gain, and
 * a top-level path is still just its field name — which is why every flat-form value, test and
 * `data-testid` carried over untouched.
 *
 * HOW MANY ROWS A LIST HAS IS IN THAT SAME MAP. Adding a row writes the row's own keys, and one of
 * them is always the row's own path (`targets.0`) — the VALUE for a scalar row, an inert marker for
 * a group row. So {@link rowCount} reads the length off the values rather than off a second
 * structure that could disagree with them, and there is exactly one thing to hand a component.
 *
 * The type strings are `schemaType`'s (`schemaTree.ts`), which is what the field table beside
 * either form draws — so the rule this applies to a value is the one the operator reads next to it.
 */

import { declaredValue, elementAt, type FieldNode } from './schemaTree';

/** One record's worth of typed-as-string inputs, keyed by PATH — what a form holds. */
export type FieldValues = Record<string, string>;

/**
 * A fresh record for a field tree: every declared leaf present, each holding its declared default,
 * or blank where the author declared none.
 *
 * IT IS NOT `blankValues` ANY MORE, and the rename is the point. A schema that carries
 * `size: int = 200` has already answered "what goes here"; a form that opened blank made the
 * operator retype it, or start a run without it because nothing on screen said there was a sensible
 * value. A prefilled field is still a field — clearing it means the same absent key it always did.
 * That holds at depth: `retry.tries = 3` prefills, and so does a `false` or a `0` two levels down,
 * because the test is for the declared KEY and never for truthiness.
 *
 * A LIST STARTS WITH NO ROWS unless its author declared some. A row nobody asked for is a key
 * nobody asked for, which is the same mistake as an empty optional arriving as `""`.
 */
export function initialValues(nodes: readonly FieldNode[]): FieldValues {
  const values: FieldValues = {};
  for (const node of nodes) seed(node, values);
  return values;
}

/** The operator typed in one input. */
export function setPath(values: FieldValues, path: string, next: string): FieldValues {
  return { ...values, [path]: next };
}

/**
 * How many rows a list currently has.
 *
 * Read off the value map rather than stored beside it: every row writes at least its own path
 * (`targets.0`), so the highest index present IS the length. Highest-plus-one rather than a count,
 * so a map that arrived from somewhere other than {@link addRow} cannot silently lose a row.
 */
export function rowCount(values: FieldValues, listPath: string): number {
  const prefix = `${listPath}.`;
  let highest = -1;
  for (const key of Object.keys(values)) {
    if (!key.startsWith(prefix)) continue;
    const head = key.slice(prefix.length).split('.')[0] ?? '';
    const index = Number(head);
    if (String(index) === head && Number.isInteger(index) && index > highest) highest = index;
  }
  return highest + 1;
}

/** One more row on a list, seeded from the element's declared defaults — the whole point of an array
 *  being repeatable rather than a bracket the operator types by hand. */
export function addRow(list: FieldNode, values: FieldValues): FieldValues {
  const row = elementAt(list, rowCount(values, list.path));
  const added: FieldValues = {};
  seed(row, added);
  // A GROUP ROW WHOSE ELEMENT DECLARES NOTHING WOULD OTHERWISE BE INVISIBLE. Every row writes its
  // own path so `rowCount` can see it; for a scalar row that key is the value itself.
  if (!(row.path in added)) added[row.path] = '';
  return { ...values, ...added };
}

/**
 * Drop one row, and close the gap.
 *
 * THE ROWS AFTER IT MOVE DOWN. Positions are the only identity a row has — `targets.1` IS the second
 * target — so leaving a hole would leave a list whose second element is missing and whose third is
 * still called third.
 */
export function removeRow(list: FieldNode, values: FieldValues, index: number): FieldValues {
  if (index < 0 || index >= rowCount(values, list.path)) return values;
  const prefix = `${list.path}.`;
  const out: FieldValues = {};
  for (const [key, value] of Object.entries(values)) {
    if (!key.startsWith(prefix)) {
      out[key] = value;
      continue;
    }
    const rest = key.slice(prefix.length);
    const dot = rest.indexOf('.');
    const head = dot === -1 ? rest : rest.slice(0, dot);
    const at = Number(head);
    if (String(at) !== head || !Number.isInteger(at)) {
      out[key] = value;
      continue;
    }
    if (at === index) continue;
    out[`${prefix}${at > index ? at - 1 : at}${dot === -1 ? '' : rest.slice(dot)}`] = value;
  }
  return out;
}

/**
 * One record of typed strings as the object its fields declare — or the first field that refuses.
 *
 * ONE REASON, NAMED. The error carries the PATH, not the reason alone, because the caller (a Batch
 * with many Units, a workflow with one) is the only side that knows what to prefix — `unit 3 · ` or
 * nothing at all — and a message that already carried a Unit number would be wrong on the surface
 * that has no Units. At the top level a path is a field name, so the flat form's sentences are
 * unchanged; deeper, `targets.1.host` is identifiable in a Batch of forty and `host` is not.
 *
 * AN EMPTY OPTIONAL IS AN ABSENT KEY, not `""`. An Actor or workflow that reads a missing key as
 * "use the default" and an empty string as "the empty string" is the common case, and only one of
 * those is what a blank input means.
 */
export function coerceValues(
  nodes: readonly FieldNode[],
  values: FieldValues
): { value: Record<string, unknown> } | { error: string } {
  const out: Record<string, unknown> = {};
  for (const node of nodes) {
    const got = coerceNode(node, values);
    if ('error' in got) return { error: got.error };
    if ('value' in got) out[node.name] = got.value;
  }
  return { value: out };
}

/** One node's contribution to the record it sits in: a value, nothing at all, or a refusal. */
type Collected = { value: unknown } | { absent: true } | { error: string };

function coerceNode(node: FieldNode, values: FieldValues): Collected {
  if (node.kind === 'group') return coerceGroup(node, values);
  if (node.kind === 'list') return coerceList(node, values);

  const raw = values[node.path] ?? '';
  if (raw.trim() === '') {
    if (node.required) return { error: `${node.path} is required` };
    return { absent: true };
  }
  // MEMBERSHIP IS CHECKED ON THE RAW STRING, before the type is applied. The enum's members were
  // stringified out of the schema, so comparing here is the one place the two are in the same
  // notation — and it means an integer enum is checked the same way a string one is, without
  // this function learning what its members were declared as. That ordering holds at every depth.
  if (node.enum && !node.enum.includes(raw.trim())) {
    return { error: `${node.path}: ${JSON.stringify(raw)} is not one of ${node.enum.join(', ')}` };
  }
  const got = coerceField(node.type, raw);
  if ('error' in got) return { error: `${node.path}: ${got.error}` };
  return { value: got.value };
}

/**
 * A nested object, as the object it describes — or ABSENT.
 *
 * AN EMPTY OPTIONAL GROUP IS AN ABSENT KEY, NOT AN EMPTY OBJECT. It is the same rule as a blank
 * optional input, one level up: an Actor annotated `retry: Retry | None = None` reads a missing key
 * as "I did not ask for retries" and `{}` as "give me a Retry with its own defaults", and only the
 * first is what an untouched group means. A REQUIRED group is always present, and `{}` is then the
 * honest value — the key was demanded, the content was not.
 *
 * WHICH IS ALSO WHY AN UNTOUCHED OPTIONAL GROUP DOES NOT REFUSE ITS OWN REQUIRED CHILDREN. Not
 * filling in the retry block at all is legal; starting to fill it in and missing `retry.host` is
 * not, and that is the refusal an operator can act on.
 */
function coerceGroup(node: FieldNode, values: FieldValues): Collected {
  if (!node.required && !anyFilled(node, values)) return { absent: true };
  const inner: Record<string, unknown> = {};
  for (const child of node.children ?? []) {
    const got = coerceNode(child, values);
    if ('error' in got) return got;
    if ('value' in got) inner[child.name] = got.value;
  }
  return { value: inner };
}

/**
 * A list, as the list it describes — or ABSENT when it has no rows and nobody required it.
 *
 * A ROW IS PRESENT BECAUSE SOMEBODY ADDED IT, so every row is collected as required: a scalar row
 * left blank is refused by its path rather than silently dropped, which would hand back a list
 * shorter than the one on the screen. An empty REQUIRED list is `[]` and not a refusal — `required`
 * is about the key being there, and `[]` is a value; a list that needs elements says so with
 * `minItems`, which is a constraint this form does not read.
 */
function coerceList(node: FieldNode, values: FieldValues): Collected {
  const rows = rowCount(values, node.path);
  if (rows === 0) return node.required ? { value: [] } : { absent: true };
  const out: unknown[] = [];
  for (let i = 0; i < rows; i += 1) {
    const got = coerceNode(elementAt(node, i), values);
    if ('error' in got) return got;
    if ('value' in got) out.push(got.value);
  }
  return { value: out };
}

/** Whether the operator has put anything at all under this node — the question "is this group
 *  untouched" reduces to, and the reason an empty optional group can be left out wholesale. */
function anyFilled(node: FieldNode, values: FieldValues): boolean {
  if (node.kind === 'group') return (node.children ?? []).some((c) => anyFilled(c, values));
  if (node.kind === 'list') return rowCount(values, node.path) > 0;
  return (values[node.path] ?? '').trim() !== '';
}

/**
 * Write one node's opening value(s) into the map.
 *
 * `given` is a value handed down from a GROUP's or a LIST's own declared default — `retry = {"tries":
 * 5}` seeds `retry.tries` — and `undefined` means "use whatever this node itself declared". The two
 * are different: a default object that names no `tries` leaves the child's own default in place.
 */
function seed(node: FieldNode, into: FieldValues, given?: unknown): void {
  if (node.kind === 'group') {
    const declared = given === undefined ? declaredValue(node) : given;
    const from =
      declared && typeof declared === 'object' && !Array.isArray(declared)
        ? (declared as Record<string, unknown>)
        : undefined;
    for (const child of node.children ?? []) seed(child, into, from?.[child.name]);
    return;
  }
  if (node.kind === 'list') {
    const declared = given === undefined ? declaredValue(node) : given;
    if (!Array.isArray(declared)) return;
    declared.forEach((item, i) => {
      const row = elementAt(node, i);
      seed(row, into, item);
      if (!(row.path in into)) into[row.path] = '';
    });
    return;
  }
  into[node.path] = given === undefined ? (node.default ?? '') : asText(given);
}

/** A JSON value as the string an input holds — the same spelling `schemaDefault` produces. */
function asText(value: unknown): string {
  if (value === null) return 'null';
  return typeof value === 'object' ? JSON.stringify(value) : String(value);
}

/**
 * One typed string as the value its declared type says it is.
 *
 * The type strings are `schemaType`'s, which is what the card's field table shows — so what the
 * operator reads beside the input is the rule this applies to it.
 */
export function coerceField(type: string, raw: string): { value: unknown } | { error: string } {
  const members = type.split('|').map((m) => m.trim()).filter(Boolean);
  // A derived optional arrives as `string | null`, and `null` is the only way to say "explicitly
  // nothing" in a text input — blank already means "leave the key out", which is a different fact.
  if (members.includes('null') && raw.trim() === 'null') return { value: null };
  const declared = members.find((m) => m !== 'null') ?? 'any';
  switch (declared) {
    case 'string':
      // NOT trimmed. Blankness was decided by the caller on the trimmed value; past that, the
      // spaces somebody typed inside a string are theirs.
      return { value: raw };
    case 'integer': {
      const n = Number(raw.trim());
      if (!Number.isFinite(n)) return { error: `${JSON.stringify(raw)} is not a number` };
      if (!Number.isInteger(n)) return { error: `${JSON.stringify(raw)} is not a whole number` };
      return { value: n };
    }
    case 'number': {
      const n = Number(raw.trim());
      return Number.isFinite(n) ? { value: n } : { error: `${JSON.stringify(raw)} is not a number` };
    }
    case 'boolean': {
      const lower = raw.trim().toLowerCase();
      if (lower === 'true') return { value: true };
      if (lower === 'false') return { value: false };
      return { error: `${JSON.stringify(raw)} is not true or false` };
    }
    default: {
      // A list, an object, a `$ref`ed model, or a field whose type the schema never said. JSON is
      // the only notation that can carry all of them out of a one-line input.
      try {
        return { value: JSON.parse(raw) as unknown };
      } catch {
        // `any` is a field the AUTHOR left untyped, so a bare word is a legitimate value for it and
        // refusing one would be this form inventing a constraint the Actor does not have.
        if (declared === 'any') return { value: raw };
        return { error: `${JSON.stringify(raw)} is not JSON — a ${declared} is typed as JSON` };
      }
    }
  }
}
