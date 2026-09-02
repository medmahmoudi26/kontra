/**
 * A JSON Schema in, a normalized FIELD TREE out. No DOM, no React, no fetch.
 *
 * IT LIVED IN `MethodContract.tsx` AND OUTGREW A COMPONENT FILE. The flat half of this reader — the
 * type string, the closed set, the declared default, the top-level flattening — was written beside
 * the field table that draws it, which was right while the reading was one level deep. It is not any
 * more: a nested object is a nested GROUP of fields, an array is a repeatable ROW, and every quirk of
 * what the two derivers emit has to be understood in one place or three surfaces learn it three
 * ways. So the reader moved here and `MethodContract.tsx` re-exports it — every existing importer
 * keeps its import, and the module that knows the formats is a `.ts` file a node suite can read.
 *
 * WHAT THE TWO DERIVERS ACTUALLY EMIT, which is the whole reason this is deep:
 *
 *   Go   `jsonschema.Reflector{DoNotReference: true}` (`registrar.go:129`) INLINES nested structs —
 *        no `$defs`, no `$ref` — and spells a closed set `{"type":"string","enum":[…]}`.
 *   Py   `TypeAdapter(tp).json_schema()` (`actorkit/schema.py:schema_of`) does the opposite: a
 *        nested dataclass arrives as `{"$ref":"#/$defs/Retry"}` with the body under a sibling
 *        `$defs` on the ROOT document, and an OPTIONAL one as
 *        `{"anyOf":[{"$ref":"#/$defs/Retry"},{"type":"null"}],"default":null}`.
 *
 * SO `$ref` RESOLUTION IS NOT OPTIONAL, and it is the piece the PRD's list of quirks leaves out. A
 * reader that treated a `$ref` as an opaque scalar would draw every Python actor's nested model as a
 * JSON textarea — the exact failure this whole slice exists to remove — while Go's actors nested
 * correctly, which is the worst kind of gap because it looks like a language problem.
 *
 * AND A RECURSIVE MODEL MUST NOT HANG THE PAGE. `$defs` can point at itself (`Node.children:
 * list[Node]` is ordinary), so every `$ref` followed on the way down is remembered and a pointer met
 * twice degrades to a JSON box instead of recursing forever. `MAX_DEPTH` is the same guard for a
 * schema that nests without a `$ref` to notice.
 *
 * THREE ANSWERS, AT EVERY DEPTH. `not declared`, `no fields` and a real field table are three
 * different facts, and collapsing any two tells the reader something false:
 *
 *   not declared   no type information at all (`{}`, an absent schema). {@link schemaTree} answers
 *                  `null` at the root; deeper it is a LEAF whose type is `any`.
 *   no fields      a declared object that names no properties. `dict` derives
 *                  `{"type":"object","additionalProperties":true}` — "any object", so a LEAF with a
 *                  JSON box; an empty dataclass derives `{"type":"object","properties":{}}` — a
 *                  GROUP with no children, which is a different sentence again.
 *   fields         the ordinary case, and the only one with inputs under it.
 *
 * THE SHALLOW READING IS THE TOP LEVEL, NOT A SECOND WALK. {@link schemaFields} IS {@link schemaTree}
 * — the contract table reads the same nodes the form does and stops one level down — so a table and
 * the form beside it can never describe two different Methods. One reading, rendered at two depths.
 */

import type { JsonSchema } from '../types';

/**
 * How deep a form is drawn before an unusual document is handed to a JSON box.
 *
 * Not a style rule — a legal schema can nest without bound (and a `$ref` cycle is caught separately
 * by {@link resolveShape}), and a renderer that walked one would take the tab with it.
 */
const MAX_DEPTH = 12;

/** One field as the contract table reads it — the shallow half, unchanged since the flat form. */
export interface SchemaField {
  name: string;
  type: string;
  required: boolean;
  /**
   * The declared choices, as the strings a text input holds — absent when the field is open.
   *
   * A CLOSED FIELD IS A DIFFERENT FACT FROM A NARROW ONE. `status: string` and
   * `status: "open" | "closed"` are both strings to {@link schemaType}, and only one of them can be
   * offered as a list — so the enum travels beside the type rather than folded into it, which
   * also keeps `coerceField`'s job (a type string in, a value out) unchanged.
   */
  enum?: string[];
  /** The author's declared default, as the string a text input holds. Absent when none. */
  default?: string;
}

/**
 * What a node IS to a form: one input, a nested group of them, or a repeatable row of them.
 *
 * A `leaf` is anything a single control can collect — a scalar, a closed set, and also the two
 * shapes that DEGRADE to a JSON box (an undeclared field, a declared-open object), because a box is
 * a single control too.
 */
export type FieldKind = 'leaf' | 'group' | 'list';

/**
 * One node of the field tree.
 *
 * `path` IS THE ADDRESS AND THE NAME IS NOT. Values are collected into a flat map keyed by path
 * (`retry.backoff`, `targets.1.host`) rather than into a nested value tree, so coercion stays a walk
 * over leaves and only assembly folds them back up — and a refusal can name `targets.1.host`, which
 * is identifiable in a Batch of forty, rather than `host`, which is not.
 */
export interface FieldNode extends SchemaField {
  kind: FieldKind;
  /** Dotted address from the record root. Equal to `name` at the top level, which is what keeps
   *  every flat-form value, test and `data-testid` working unchanged. */
  path: string;
  /** `group` only: the fields the nested object declares. `[]` is "declares no fields" — a real
   *  answer, drawn as such, never the same thing as an absent schema. */
  children?: FieldNode[];
  /**
   * `list` only: the shape of ONE element, as a TEMPLATE — its paths are rooted at `''` because a
   * template belongs to no row yet. {@link elementAt} is what turns it into a row's real node.
   */
  element?: FieldNode;
}

/** `retry` + `backoff` → `retry.backoff`; the root prefix is empty, so a top-level path is its name. */
export function joinPath(prefix: string, name: string | number): string {
  return prefix === '' ? String(name) : `${prefix}.${name}`;
}

/**
 * Flatten a JSON Schema's top level into readable fields — the SHALLOW READING, by name.
 *
 * It is literally {@link schemaTree}: the shallow reading is the top level of the tree rather than a
 * second walk, so the contract table and the form beside it can never describe two different
 * Methods. The name is kept because five surfaces already import it, and because "the fields this
 * declares" is what those surfaces are asking for.
 *
 * Returns `null` for "no schema declared", which the caller must render differently from an empty
 * list — that is the distinction this whole module turns on. Deliberately shallow AS A READING: a
 * nested object shows as `object`/`Retry` here because a contract table says what a caller puts in
 * a Unit rather than being a schema viewer. It is the same walk the form draws from, stopped one
 * level down, so the two can never disagree about what a Method takes.
 */
export function schemaFields(schema?: JsonSchema): FieldNode[] | null {
  return schemaTree(schema);
}

/**
 * The whole field tree for one schema — `null` when nothing was declared.
 *
 * `null` and `[]` ARE DIFFERENT ANSWERS and always have been: `undefined`, or a document with no
 * `properties`, is an author who declared nothing (`examples/go/dnsfacts`), and `properties: {}` is
 * an author who declared an empty shape. Only one of those can be called with anything.
 */
export function schemaTree(schema?: JsonSchema): FieldNode[] | null {
  if (!schema || typeof schema !== 'object') return null;
  return childrenOf(schema, schema, '', new Set(), 0);
}

/**
 * One row of a list, as a node with real paths.
 *
 * The stored `element` is a template rooted at `''`; a row is that template rooted at
 * `${list.path}.${index}`. Doing it here rather than storing one node per row means the tree stays
 * a function of the schema alone — it does not change when the operator presses "add".
 */
export function elementAt(list: FieldNode, index: number): FieldNode {
  const element = list.element ?? { kind: 'leaf', name: '', path: '', type: 'any', required: true };
  return repath(element, joinPath(list.path, index));
}

/**
 * Why a leaf is a JSON box rather than a set of fields — or `null` when it is an ordinary field.
 *
 * THE TWO REASONS ARE DIFFERENT SENTENCES because one is fixable by the person reading it. An
 * `undeclared` field is an author who wrote no annotation — one type away from a form, and saying so
 * is how it gets fixed (`examples/go/dnsfacts` is a real one). A `no-fields` object is an author who
 * chose an open shape on purpose, and telling them to fix it would be wrong.
 */
export function whyJson(node: FieldNode): 'undeclared' | 'no-fields' | null {
  if (node.kind !== 'leaf') return null;
  const members = node.type
    .split('|')
    .map((m) => m.trim())
    .filter((m) => m !== '' && m !== 'null');
  if (members.length !== 1) return null;
  if (members[0] === 'any') return 'undeclared';
  if (members[0] === 'object') return 'no-fields';
  return null;
}

/**
 * The declared choices for one property, as the strings a text input holds — or `null` when open.
 *
 * TWO SPELLINGS REACH HERE AND BOTH ARE DERIVED, not authored. Go's reflector emits
 * `{"type":"string","enum":[…]}`; Python's `Literal` can arrive as a bare `{"enum":[…]}`, and an
 * OPTIONAL `Literal` as `{"anyOf":[{"enum":[…]},{"type":"null"}]}` — so the `anyOf` arm is walked
 * rather than treated as an open field, which is the reading that would silently turn a closed set
 * back into a free-text box.
 *
 * The members are stringified because that is what an input holds; nothing is coerced here, and
 * membership is checked on the raw string before its type is applied (`formFields.ts`).
 */
export function schemaEnum(raw: unknown): string[] | null {
  if (!raw || typeof raw !== 'object') return null;
  const p = raw as { enum?: unknown; anyOf?: unknown };
  if (Array.isArray(p.enum)) {
    // `null` is spelled by the type union, not by a member: a nullable enum already reads as
    // `… | null`, and a `"null"` OPTION would be indistinguishable from the string "null".
    const members = p.enum.filter((v) => v !== null).map((v) => String(v));
    return members.length > 0 ? members : null;
  }
  if (Array.isArray(p.anyOf)) {
    for (const arm of p.anyOf) {
      const found = schemaEnum(arm);
      if (found) return found;
    }
  }
  return null;
}

/**
 * One property's declared default, as the string a text input holds — or `undefined` when none.
 *
 * A DECLARED DEFAULT IS THE AUTHOR'S ANSWER TO "what goes here", and a blank box throws it away:
 * the operator retypes a value the schema already carried, or starts a run without one because the
 * form never said there was a sensible choice. `false` and `0` are defaults like any other, which
 * is why this tests for the KEY rather than for truthiness — at every depth, since a falsy default
 * two levels down is exactly as easy to lose as one at the top.
 */
export function schemaDefault(raw: unknown): string | undefined {
  if (!raw || typeof raw !== 'object' || !('default' in raw)) return undefined;
  const value = (raw as { default: unknown }).default;
  if (value === null || value === undefined) return undefined;
  // A scalar is what it looks like; an object or list has to arrive as the JSON the box accepts —
  // and `declaredValue` reads that spelling back when the node it belongs to is a group or a list.
  return typeof value === 'object' ? JSON.stringify(value) : String(value);
}

/**
 * A group's or a list's declared default as the JSON VALUE it was, not the string it was flattened
 * to — the shape `initialValues` seeds nested fields and repeated rows from.
 *
 * `schemaDefault` stringified it with `JSON.stringify`, so parsing is the exact inverse.
 */
export function declaredValue(node: FieldNode): unknown {
  if (node.default === undefined) return undefined;
  try {
    return JSON.parse(node.default) as unknown;
  } catch {
    return undefined;
  }
}

/** A readable type for one property. Unions arrive as arrays (`["string","null"]`) from a derived
 *  optional, which is worth showing as written rather than collapsing to the first member. A `$ref`
 *  shows as the MODEL NAME the author wrote, which is a better legend for a group than `object`. */
export function schemaType(raw: unknown): string {
  if (!raw || typeof raw !== 'object') return 'any';
  const p = raw as { type?: unknown; items?: unknown; $ref?: unknown; anyOf?: unknown };
  if (typeof p.$ref === 'string') return p.$ref.split('/').pop() || 'ref';
  if (Array.isArray(p.anyOf)) return p.anyOf.map(schemaType).join(' | ');
  if (Array.isArray(p.type)) return p.type.map(String).join(' | ');
  if (p.type === 'array') return `${schemaType(p.items)}[]`;
  return typeof p.type === 'string' ? p.type : 'any';
}

// ---------------------------------------------------------------------------------------------
// The walk
// ---------------------------------------------------------------------------------------------

/** The fields one object declares — `null` when it declares no `properties` at all. */
function childrenOf(
  raw: unknown,
  root: JsonSchema,
  prefix: string,
  seen: ReadonlySet<string>,
  depth: number
): FieldNode[] | null {
  if (!raw || typeof raw !== 'object') return null;
  const props = (raw as { properties?: unknown }).properties;
  if (!props || typeof props !== 'object') return null;
  const required = requiredOf(raw);
  return Object.entries(props as Record<string, unknown>).map(([name, prop]) =>
    nodeOf(name, joinPath(prefix, name), prop, root, required.has(name), seen, depth)
  );
}

function requiredOf(raw: unknown): Set<string> {
  const listed = (raw as { required?: unknown }).required;
  return new Set(
    Array.isArray(listed) ? (listed.filter((r) => typeof r === 'string') as string[]) : []
  );
}

/** One property, as whichever of the three kinds its resolved shape turns out to be. */
function nodeOf(
  name: string,
  path: string,
  raw: unknown,
  root: JsonSchema,
  required: boolean,
  seen: ReadonlySet<string>,
  depth: number
): FieldNode {
  const choices = schemaEnum(raw);
  const fallback = schemaDefault(raw);
  const base: SchemaField = {
    name,
    type: schemaType(raw),
    required,
    ...(choices ? { enum: choices } : {}),
    ...(fallback === undefined ? {} : { default: fallback }),
  };

  // A CLOSED SET IS A LEAF EVEN WHEN ITS ARM IS AN OBJECT. Membership is the whole contract there,
  // and a list of members is a better control than any group could be.
  const shape = choices ? null : resolveShape(raw, root, seen);
  if (shape && depth < MAX_DEPTH) {
    const kids = childrenOf(shape.schema, root, path, shape.seen, depth + 1);
    if (kids !== null) return { ...base, kind: 'group', path, children: kids };
    if (isArray(shape.schema)) {
      const items = (shape.schema as { items?: unknown }).items;
      return { ...base, kind: 'list', path, element: elementOf(items, root, shape.seen, depth + 1) };
    }
  }
  return { ...base, kind: 'leaf', path };
}

/**
 * The shape of ONE element of a list, as a template rooted at `''`.
 *
 * A ROW IS ALWAYS REQUIRED, whatever the list itself is. The operator pressed "add"; a row that
 * quietly coerced to nothing would be a list one shorter than the one on the screen.
 *
 * AN ARRAY OF ARRAYS DEGRADES TO A BOX. Its inner rows have no name to draw, and a legal-but-unusual
 * document is meant to reach a JSON box rather than a blank screen.
 */
function elementOf(
  items: unknown,
  root: JsonSchema,
  seen: ReadonlySet<string>,
  depth: number
): FieldNode {
  const choices = schemaEnum(items);
  const fallback = schemaDefault(items);
  const base: SchemaField = {
    name: '',
    type: schemaType(items),
    required: true,
    ...(choices ? { enum: choices } : {}),
    ...(fallback === undefined ? {} : { default: fallback }),
  };
  const shape = choices ? null : resolveShape(items, root, seen);
  if (shape && depth < MAX_DEPTH) {
    const kids = childrenOf(shape.schema, root, '', shape.seen, depth + 1);
    if (kids !== null) return { ...base, kind: 'group', path: '', children: kids };
  }
  return { ...base, kind: 'leaf', path: '' };
}

function isArray(raw: unknown): boolean {
  const t = (raw as { type?: unknown }).type;
  return t === 'array' || (Array.isArray(t) && t.includes('array'));
}

/**
 * Follow the wrappers a deriver puts between a property and the shape it really is.
 *
 * `$ref` into the root's `$defs` is Python's spelling for every nested model; `anyOf` with a `null`
 * arm is its spelling for an optional one; a single-armed `allOf` is the older pydantic spelling for
 * "this ref, plus a sibling default". Go needs none of this and gets it for free.
 *
 * `null` back means "stop here" — an unresolvable ref, or one already followed on this branch.
 */
function resolveShape(
  raw: unknown,
  root: JsonSchema,
  seen: ReadonlySet<string>
): { schema: unknown; seen: ReadonlySet<string> } | null {
  if (!raw || typeof raw !== 'object') return null;
  const p = raw as { $ref?: unknown; anyOf?: unknown; allOf?: unknown };

  if (typeof p.$ref === 'string') {
    // A POINTER MET TWICE ON ONE BRANCH IS A CYCLE. `Node.children: list[Node]` is ordinary, and
    // recursing it would take the tab down rather than draw a form.
    if (seen.has(p.$ref)) return null;
    const target = lookupRef(p.$ref, root);
    if (target === undefined) return null;
    const next: ReadonlySet<string> = new Set([...seen, p.$ref]);
    return resolveShape(target, root, next) ?? { schema: target, seen: next };
  }

  if (Array.isArray(p.anyOf)) {
    for (const arm of p.anyOf) {
      if (isNullArm(arm)) continue;
      const found = resolveShape(arm, root, seen);
      if (found && (hasProperties(found.schema) || isArray(found.schema))) return found;
    }
    return null;
  }

  if (Array.isArray(p.allOf) && p.allOf.length === 1) return resolveShape(p.allOf[0], root, seen);

  return { schema: raw, seen };
}

function isNullArm(arm: unknown): boolean {
  return !!arm && typeof arm === 'object' && (arm as { type?: unknown }).type === 'null';
}

function hasProperties(raw: unknown): boolean {
  const props = (raw as { properties?: unknown })?.properties;
  return !!props && typeof props === 'object';
}

/** `#/$defs/Retry` → the document under it. Only local pointers resolve; a remote `$ref` is not
 *  something either deriver emits, and fetching one from a form would be a surprise. */
function lookupRef(pointer: string, root: JsonSchema): unknown {
  if (!pointer.startsWith('#/')) return undefined;
  let at: unknown = root;
  for (const step of pointer.slice(2).split('/')) {
    if (!at || typeof at !== 'object') return undefined;
    at = (at as Record<string, unknown>)[step.replace(/~1/g, '/').replace(/~0/g, '~')];
  }
  return at;
}

/** The same node, re-addressed under `path`. An `element` template stays rooted at `''` — it is
 *  re-addressed by {@link elementAt} when its own row is drawn. */
function repath(node: FieldNode, path: string): FieldNode {
  const moved: FieldNode = { ...node, path };
  if (node.children) moved.children = node.children.map((c) => repath(c, joinPath(path, c.name)));
  return moved;
}
