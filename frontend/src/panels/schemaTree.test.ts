/**
 * The schema reader, all the way down — a JSON Schema in, a field tree out.
 *
 * NO DOM, for the reason the rest of this directory records: the suite runs in node, and a reader
 * that needed a browser to be tested would be a reader nobody tested. `schemaTree.ts` imports one
 * type and nothing else, which is what makes that possible.
 *
 * THE FIXTURES COME FROM THE DERIVERS, NOT FROM AN IDEA OF THEM. `PYDANTIC` below is the literal
 * output of `TypeAdapter(Args).json_schema()` — the call `sdk/python/actorkit/schema.py`
 * makes — with `$defs`, `$ref`, an `anyOf` null arm and a `default: null` exactly where pydantic
 * puts them. Tests that supply their own input have hidden real bugs in this repo before; the shape
 * a deriver actually emits is the one thing a unit test cannot invent.
 *
 * AND THE GO SIDE IS THE OPPOSITE SPELLING. `jsonschema.Reflector{DoNotReference: true}`
 * (`runtime/go/registrar/registrar.go:129`) INLINES nested structs, so the same actor
 * written in Go arrives with no `$defs` at all — which is why both spellings are pinned here and why
 * a reader that only understood one would look like a language problem rather than a bug.
 */

import { describe, expect, it } from 'vitest';
import { elementAt, schemaTree, schemaFields, whyJson, type FieldNode } from './schemaTree';

/** Literal output of `TypeAdapter(Args).json_schema()` for a dataclass with a nested model, an
 *  optional one, a list of models, a list of scalars and a `Literal`. Regenerate by running that
 *  call — not by editing this by hand, which is how a fixture stops describing the deriver. */
const PYDANTIC = {
  $defs: {
    Retry: {
      properties: {
        tries: { default: 3, title: 'Tries', type: 'integer' },
        verbose: { default: false, title: 'Verbose', type: 'boolean' },
      },
      title: 'Retry',
      type: 'object',
    },
    Target: {
      properties: {
        host: { title: 'Host', type: 'string' },
        port: { default: 80, title: 'Port', type: 'integer' },
      },
      required: ['host'],
      title: 'Target',
      type: 'object',
    },
  },
  properties: {
    url: { title: 'Url', type: 'string' },
    retry: { $ref: '#/$defs/Retry' },
    fallback: { anyOf: [{ $ref: '#/$defs/Retry' }, { type: 'null' }], default: null },
    targets: { items: { $ref: '#/$defs/Target' }, title: 'Targets', type: 'array' },
    ports: { items: { type: 'integer' }, title: 'Ports', type: 'array' },
    mode: { default: 'fast', enum: ['fast', 'slow'], title: 'Mode', type: 'string' },
  },
  required: ['url', 'retry'],
  title: 'Args',
  type: 'object',
};

/** The same actor as Go's reflector emits it: nested structs INLINED, no `$defs`, no `$ref`. */
const GO_INLINE = {
  type: 'object',
  properties: {
    url: { type: 'string' },
    retry: {
      type: 'object',
      properties: { tries: { type: 'integer', default: 3 }, verbose: { type: 'boolean', default: false } },
    },
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

const at = (nodes: FieldNode[] | null, name: string): FieldNode =>
  (nodes ?? []).find((n) => n.name === name)!;

describe('the three answers stay three, at the root', () => {
  it('is null for a schema nobody declared', () => {
    expect(schemaTree(undefined)).toBeNull();
  });

  it('is null for a document that names no properties — `dict` derives one of those', () => {
    expect(schemaTree({ type: 'object', additionalProperties: true })).toBeNull();
  });

  it('is an EMPTY TREE for a declared-but-empty object, which is a different fact', () => {
    // An actor whose Method takes an empty dataclass and one whose author wrote no annotation are
    // not the same thing to a caller, and only one of them can be called with anything.
    expect(schemaTree({ type: 'object', properties: {} })).toEqual([]);
  });

  it('is the same reading `schemaFields` hands the contract table', () => {
    // The shallow reading is the TOP LEVEL of this tree, not a second walk — which is what keeps a
    // form from ever offering a field the table above it does not show.
    expect(schemaFields(PYDANTIC)).toEqual(schemaTree(PYDANTIC));
  });
});

describe('the three answers stay three, at depth', () => {
  const tree = schemaTree({
    type: 'object',
    properties: {
      undeclared: {},
      open: { type: 'object' },
      empty: { type: 'object', properties: {} },
      real: { type: 'object', properties: { host: { type: 'string' } } },
    },
  });

  it('draws a field nobody typed as a leaf that says NOT DECLARED', () => {
    const node = at(tree, 'undeclared');
    expect(node.kind).toBe('leaf');
    expect(node.type).toBe('any');
    expect(whyJson(node)).toBe('undeclared');
  });

  it('draws a declared-open object as a leaf that says something ELSE — it is not an omission', () => {
    const node = at(tree, 'open');
    expect(node.kind).toBe('leaf');
    expect(whyJson(node)).toBe('no-fields');
  });

  it('draws a declared-but-empty object as a GROUP with no children — the third answer', () => {
    const node = at(tree, 'empty');
    expect(node.kind).toBe('group');
    expect(node.children).toEqual([]);
  });

  it('draws a declared object with properties as a group with them', () => {
    const node = at(tree, 'real');
    expect(node.kind).toBe('group');
    expect(node.children?.map((c) => c.path)).toEqual(['real.host']);
  });
});

describe('a nested object, in both spellings a deriver emits', () => {
  it('follows Python’s $ref into $defs rather than treating it as a scalar', () => {
    // Every nested model on the Python side arrives as a pointer. A reader that stopped here would
    // give every Python actor a JSON textarea while Go actors nested correctly.
    const retry = at(schemaTree(PYDANTIC), 'retry');
    expect(retry.kind).toBe('group');
    expect(retry.required).toBe(true);
    expect(retry.children?.map((c) => c.path)).toEqual(['retry.tries', 'retry.verbose']);
  });

  it('reads Go’s inlined struct the same way, with no $ref to follow', () => {
    const retry = at(schemaTree(GO_INLINE), 'retry');
    expect(retry.kind).toBe('group');
    expect(retry.children?.map((c) => c.path)).toEqual(['retry.tries', 'retry.verbose']);
  });

  it('walks the anyOf null arm, so an OPTIONAL nested model is still a group', () => {
    const fallback = at(schemaTree(PYDANTIC), 'fallback');
    expect(fallback.kind).toBe('group');
    expect(fallback.required).toBe(false);
    expect(fallback.children?.map((c) => c.path)).toEqual(['fallback.tries', 'fallback.verbose']);
  });

  it('names the group by the MODEL, which reads better over a set of fields than `object`', () => {
    expect(at(schemaTree(PYDANTIC), 'retry').type).toBe('Retry');
  });

  it('keeps a declared default at depth, including a falsy one', () => {
    // `verbose: bool = False` is the author's answer to "what goes here". Truthiness would drop it.
    const retry = at(schemaTree(PYDANTIC), 'retry');
    expect(retry.children?.map((c) => c.default)).toEqual(['3', 'false']);
  });

  it('nests without a floor — a five-level schema is a five-level path', () => {
    const deep = schemaTree({
      properties: {
        a: {
          type: 'object',
          properties: {
            b: {
              type: 'object',
              properties: {
                c: { type: 'object', properties: { d: { type: 'object', properties: { e: { type: 'integer', default: 0 } } } } },
              },
            },
          },
        },
      },
    });
    const e = at(at(at(at(deep, 'a').children!, 'b').children!, 'c').children!, 'd').children![0]!;
    expect(e.path).toBe('a.b.c.d.e');
    expect(e.default).toBe('0');
  });
});

describe('an array', () => {
  it('is a list of scalars, carrying the element’s own type', () => {
    const ports = at(schemaTree(PYDANTIC), 'ports');
    expect(ports.kind).toBe('list');
    expect(ports.type).toBe('integer[]');
    expect(ports.element?.kind).toBe('leaf');
    expect(ports.element?.type).toBe('integer');
  });

  it('is a list of GROUPS when the element schema declares fields', () => {
    const targets = at(schemaTree(PYDANTIC), 'targets');
    expect(targets.kind).toBe('list');
    expect(targets.element?.kind).toBe('group');
    expect(targets.element?.children?.map((c) => c.name)).toEqual(['host', 'port']);
  });

  it('addresses a row by its index, all the way down', () => {
    const targets = at(schemaTree(PYDANTIC), 'targets');
    const second = elementAt(targets, 1);
    expect(second.path).toBe('targets.1');
    expect(second.children?.map((c) => c.path)).toEqual(['targets.1.host', 'targets.1.port']);
  });

  it('carries the element’s required fields onto every row', () => {
    // `Target.host` is required; row 4 of a Batch has to refuse the same way row 1 does.
    const row = elementAt(at(schemaTree(PYDANTIC), 'targets'), 3);
    expect(row.children?.map((c) => [c.path, c.required])).toEqual([
      ['targets.3.host', true],
      ['targets.3.port', false],
    ]);
  });

  it('treats a ROW as required whatever the list is, because somebody pressed add', () => {
    expect(elementAt(at(schemaTree(PYDANTIC), 'ports'), 0).required).toBe(true);
  });

  it('keeps the element template rooted at nothing — it belongs to no row yet', () => {
    expect(at(schemaTree(PYDANTIC), 'targets').element?.path).toBe('');
  });

  it('reads Go’s inlined element the same way', () => {
    const targets = at(schemaTree(GO_INLINE), 'targets');
    expect(targets.element?.children?.map((c) => c.name)).toEqual(['host', 'port']);
  });
});

describe('a closed set survives the recursion', () => {
  it('stays a closed set at the top level', () => {
    expect(at(schemaTree(PYDANTIC), 'mode').enum).toEqual(['fast', 'slow']);
  });

  it('stays one two levels down, in every spelling', () => {
    const tree = schemaTree({
      properties: {
        outer: {
          type: 'object',
          properties: {
            go: { type: 'string', enum: ['open', 'closed'] },
            py: { enum: ['a', 'b'] },
            optional: { anyOf: [{ enum: ['x', 'y'] }, { type: 'null' }] },
            ints: { type: 'integer', enum: [1, 2] },
          },
        },
      },
    });
    const kids = at(tree, 'outer').children!;
    expect(kids.map((c) => c.enum)).toEqual([
      ['open', 'closed'],
      ['a', 'b'],
      ['x', 'y'],
      ['1', '2'],
    ]);
    // Every one of them is a LEAF: a closed set is collected by a list of members, never by a group.
    expect(kids.every((c) => c.kind === 'leaf')).toBe(true);
  });

  it('stays one inside an array element', () => {
    const modes = at(schemaTree({ properties: { modes: { type: 'array', items: { type: 'string', enum: ['fast', 'slow'] } } } }), 'modes');
    expect(modes.element?.enum).toEqual(['fast', 'slow']);
  });
});

describe('an unusual but legal document degrades rather than breaking', () => {
  it('does not recurse forever on a self-referential model', () => {
    // `Node.children: list[Node]` is ordinary, and pydantic spells it with a `$ref` that points back
    // at its own `$defs` entry. Following it without a memory takes the tab, not the form.
    const tree = schemaTree({
      $defs: {
        Node: {
          type: 'object',
          properties: { name: { type: 'string' }, child: { $ref: '#/$defs/Node' } },
        },
      },
      properties: { root: { $ref: '#/$defs/Node' } },
    });
    const root = at(tree, 'root');
    expect(root.kind).toBe('group');
    const child = at(root.children!, 'child');
    // The second time round the pointer, it is a JSON box — a legible stop, not a hang.
    expect(child.kind).toBe('leaf');
    expect(child.path).toBe('root.child');
  });

  it('hands an array of arrays to a JSON box, since its rows have no name to draw', () => {
    const grid = at(schemaTree({ properties: { grid: { type: 'array', items: { type: 'array', items: { type: 'integer' } } } } }), 'grid');
    expect(grid.kind).toBe('list');
    expect(grid.element?.kind).toBe('leaf');
    expect(grid.element?.type).toBe('integer[]');
  });

  it('leaves a $ref that resolves to nothing as a leaf rather than throwing', () => {
    const gone = at(schemaTree({ properties: { gone: { $ref: '#/$defs/Missing' } } }), 'gone');
    expect(gone.kind).toBe('leaf');
    expect(gone.type).toBe('Missing');
  });
});
