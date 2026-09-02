/**
 * Reading a registered workflow's two schemas — and keeping the three answers apart.
 *
 * A workflow's descriptor arrives from the worker that served it (`internals/catalog.py`), derived
 * from the annotations on the `@workflow.run` method. Three things it can say about either slot,
 * and collapsing any two of them tells the reader something false:
 *
 *   UNDECLARED   nobody wrote a type down. The key is absent from the descriptor, and the honest
 *                line is that the workflow does not say — not that it takes nothing.
 *   NO FIELDS    a type was declared and it names no properties. `dict` — which is what BOTH
 *                workflows this repo ships annotate — derives `{"type":"object",
 *                "additionalProperties":true}`, i.e. "any object". Drawn as an empty field table it
 *                would read as "this workflow takes an object with no fields in it", which is the
 *                opposite of what it means: anything at all fits.
 *   FIELDS       the ordinary case, and the only one with a table under it.
 *
 * The reading is `schemaTree` — the SAME one the Actors page draws a Method's ports with, and whose
 * top level IS `schemaFields` — so a workflow and a Method describe their types the same way or not
 * at all. It answers `null` for both "no schema" and "a schema with no properties"; the caller here
 * already knows which of those it has, because the descriptor either carried the key or did not.
 *
 * IT CARRIES THE WHOLE TREE, not just the top level, because the run form nests: a workflow whose
 * argument holds a nested object draws it as nested fields (`FieldGroup.tsx`) while the contract
 * table beside it stays deliberately shallow. One reading, rendered at two depths.
 */

import { schemaTree, type FieldNode } from './schemaTree';
import type { JsonSchema } from '../types';

export type SchemaReading =
  | { kind: 'undeclared' }
  | { kind: 'no-fields' }
  | { kind: 'fields'; fields: FieldNode[] };

/** What one slot of a descriptor says. `undefined` is the absent key, not an empty document. */
export function readSchema(schema?: JsonSchema): SchemaReading {
  if (schema === undefined) return { kind: 'undeclared' };
  const fields = schemaTree(schema);
  // `null` here is a document with no `properties` — an `anyOf`, a bare object, a `dict`. It is a
  // schema, so it is not `undeclared`; it just names nothing.
  if (fields === null || fields.length === 0) return { kind: 'no-fields' };
  return { kind: 'fields', fields };
}

/** The sentence for a slot that has no table under it. Never "no fields" alone: the reader has to
 *  be able to tell "the author declared an open shape" from "the author declared nothing". */
export function sayNothing(reading: SchemaReading): string {
  return reading.kind === 'undeclared' ? 'not declared' : 'declares no fields';
}
