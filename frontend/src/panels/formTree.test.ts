/**
 * What a nested form COLLECTS: the prefill it opens with, and the value it hands back.
 *
 * `formFields.test.ts` is the flat half and stays the flat half — its four properties (three answers
 * about a schema, an absent key for an empty optional, a declared default including a falsy one, and
 * membership checked before the type) are pinned there against a one-level form. This file is the
 * same four properties AT DEPTH, because each of them is easy to lose in a recursion and none of
 * them would fail loudly if it were: a group that quietly became `{}`, a `false` two levels down
 * that quietly became blank, and an enum checked after coercion all produce a Batch that dispatches
 * and does the wrong thing.
 *
 * Node, no DOM. Values are a flat map keyed by path, so every state a form can be in is a literal
 * this file can write down.
 */

import { describe, expect, it } from 'vitest';
import { schemaTree, type FieldNode } from './schemaTree';
import { addRow, coerceValues, initialValues, removeRow, rowCount } from './formFields';

const NESTED = {
  type: 'object',
  properties: {
    url: { type: 'string' },
    // Optional group whose own children are all optional — the case the absent-key rule turns on.
    retry: {
      type: 'object',
      properties: { tries: { type: 'integer', default: 3 }, verbose: { type: 'boolean', default: false } },
    },
    // Optional group carrying a REQUIRED child: untouched is legal, half-filled is not.
    auth: {
      type: 'object',
      properties: { user: { type: 'string' }, token: { type: 'string' } },
      required: ['token'],
    },
    // Required group with nothing but optionals under it.
    limits: { type: 'object', properties: { rps: { type: 'integer' } } },
  },
  required: ['url', 'limits'],
};

const LISTS = {
  type: 'object',
  properties: {
    ports: { type: 'array', items: { type: 'integer' } },
    targets: {
      type: 'array',
      items: {
        type: 'object',
        properties: { host: { type: 'string' }, port: { type: 'integer', default: 80 } },
        required: ['host'],
      },
    },
    tags: { type: 'array', items: { type: 'string' } },
  },
  required: ['tags'],
};

const tree = (schema: Record<string, unknown>): FieldNode[] => schemaTree(schema)!;
const node = (nodes: FieldNode[], name: string): FieldNode => nodes.find((n) => n.name === name)!;

/** The record, or the reason there is not one — flattened so a test can say which it expected. */
function collect(nodes: FieldNode[], values: Record<string, string>): unknown {
  const got = coerceValues(nodes, values);
  return 'error' in got ? `REFUSED: ${got.error}` : got.value;
}

describe('a declared default prefills, at every depth', () => {
  it('opens a nested field with the author’s answer, not blank', () => {
    expect(initialValues(tree(NESTED))).toEqual({
      url: '',
      'retry.tries': '3',
      'retry.verbose': 'false',
      'auth.user': '',
      'auth.token': '',
      'limits.rps': '',
    });
  });

  it('keeps `false` and `0` two levels down, which truthiness would have dropped', () => {
    const deep = tree({
      properties: {
        a: {
          type: 'object',
          properties: {
            b: {
              type: 'object',
              properties: { off: { type: 'boolean', default: false }, none: { type: 'integer', default: 0 } },
            },
          },
        },
      },
    });
    expect(initialValues(deep)).toEqual({ 'a.b.off': 'false', 'a.b.none': '0' });
  });

  it('carries a falsy default all the way to the submitted value', () => {
    // The whole point of prefilling: what the operator sees is what gets sent.
    const deep = tree({
      properties: {
        a: { type: 'object', properties: { off: { type: 'boolean', default: false } } },
      },
    });
    expect(collect(deep, initialValues(deep))).toEqual({ a: { off: false } });
  });

  it('opens a list with NO rows — a row nobody asked for is a key nobody asked for', () => {
    expect(initialValues(tree(LISTS))).toEqual({});
  });

  it('seeds rows when the author declared a list default', () => {
    const nodes = tree({ properties: { ports: { type: 'array', items: { type: 'integer' }, default: [80, 443] } } });
    const values = initialValues(nodes);
    expect(values).toEqual({ 'ports.0': '80', 'ports.1': '443' });
    expect(collect(nodes, values)).toEqual({ ports: [80, 443] });
  });

  it('seeds a nested group from a declared object default, leaving the child’s own where it says nothing', () => {
    const nodes = tree({
      properties: {
        retry: {
          type: 'object',
          properties: { tries: { type: 'integer', default: 3 }, backoff: { type: 'number', default: 0.5 } },
          default: { tries: 9 },
        },
      },
    });
    expect(initialValues(nodes)).toEqual({ 'retry.tries': '9', 'retry.backoff': '0.5' });
  });
});

describe('an empty optional is an ABSENT KEY, at every depth', () => {
  it('leaves a blank nested leaf out rather than sending ""', () => {
    const nodes = tree(NESTED);
    const got = collect(nodes, { ...initialValues(nodes), url: 'https://a.test', 'retry.tries': '' });
    expect(got).toEqual({ url: 'https://a.test', retry: { verbose: false }, limits: {} });
  });

  it('leaves an untouched OPTIONAL GROUP out entirely — not as `{}`', () => {
    // An actor annotated `auth: Auth | None = None` reads a missing key as "I did not ask for auth"
    // and `{}` as "give me an Auth with its own defaults". Only the first is what untouched means.
    const nodes = tree(NESTED);
    const got = collect(nodes, { url: 'https://a.test', 'retry.tries': '', 'retry.verbose': '' });
    expect(got).toEqual({ url: 'https://a.test', limits: {} });
  });

  it('keeps a REQUIRED group present, as `{}` — the key was demanded, the content was not', () => {
    // `limits` in the fixture. `required` is about the key being there; an empty object is a value.
    expect(collect(tree(NESTED), { url: 'https://a.test' })).toEqual({
      url: 'https://a.test',
      limits: {},
    });
  });

  it('does NOT refuse a required field inside a group nobody touched', () => {
    // `auth.token` is required, but not filling in the auth block at all is legal. A form that
    // refused here would make every optional block compulsory.
    const got = collect(tree(NESTED), { url: 'https://a.test' });
    expect(typeof got).toBe('object'); // an object, not a `REFUSED: …` sentence
    expect(Object.keys(got as object)).not.toContain('auth');
  });

  it('DOES refuse it once the group has been started, and names the path', () => {
    // Half-filled is the mistake worth naming: the operator meant to configure auth and missed one.
    const got = collect(tree(NESTED), { url: 'https://a.test', 'auth.user': 'me' });
    expect(got).toBe('REFUSED: auth.token is required');
  });

  it('leaves an empty optional LIST out, and keeps an empty required one as []', () => {
    // Same rule one shape over: nobody added a row, so `ports` was never mentioned. `tags` is
    // required, and `[]` is a present value — the constraint that would say otherwise is `minItems`,
    // which this form does not read.
    expect(collect(tree(LISTS), {})).toEqual({ tags: [] });
  });
});

describe('a nested value comes back the shape its schema describes', () => {
  it('folds path-addressed strings into the nested object', () => {
    const nodes = tree(NESTED);
    const got = collect(nodes, {
      url: 'https://a.test',
      'retry.tries': '5',
      'retry.verbose': 'true',
      'auth.user': 'me',
      'auth.token': 'sha256~x',
      'limits.rps': '20',
    });
    expect(got).toEqual({
      url: 'https://a.test',
      retry: { tries: 5, verbose: true },
      auth: { user: 'me', token: 'sha256~x' },
      limits: { rps: 20 },
    });
  });

  it('coerces every row of a list of scalars', () => {
    let values = {};
    const ports = node(tree(LISTS), 'ports');
    values = addRow(ports, values);
    values = { ...values, 'ports.0': '80' };
    values = addRow(ports, values);
    values = { ...values, 'ports.1': '443' };
    expect(collect(tree(LISTS), values)).toEqual({ ports: [80, 443], tags: [] });
  });

  it('coerces every row of a list of objects, defaults and all', () => {
    const nodes = tree(LISTS);
    const targets = node(nodes, 'targets');
    let values = addRow(targets, addRow(targets, {}));
    values = { ...values, 'targets.0.host': 'a.test', 'targets.1.host': 'b.test', 'targets.1.port': '8443' };
    expect(collect(nodes, values)).toEqual({
      // Row 0 kept the element's declared `port = 80`; row 1 was typed over.
      targets: [
        { host: 'a.test', port: 80 },
        { host: 'b.test', port: 8443 },
      ],
      tags: [],
    });
  });

  it('names the PATH when a row refuses, which is what identifies it in a Batch of forty', () => {
    const nodes = tree(LISTS);
    const targets = node(nodes, 'targets');
    let values = addRow(targets, addRow(targets, {}));
    values = { ...values, 'targets.0.host': 'a.test', 'targets.1.host': '' };
    expect(collect(nodes, values)).toBe('REFUSED: targets.1.host is required');
  });

  it('refuses a row somebody added and left blank rather than silently shortening the list', () => {
    // The list on the screen has two rows; handing back one would be this form editing the Batch.
    const nodes = tree(LISTS);
    const values = addRow(node(nodes, 'ports'), {});
    expect(collect(nodes, values)).toBe('REFUSED: ports.0 is required');
  });

  it('names the path when a nested value will not coerce', () => {
    const got = collect(tree(NESTED), { url: 'https://a.test', 'retry.tries': 'lots' });
    expect(got).toBe('REFUSED: retry.tries: "lots" is not a number');
  });
});

describe('membership is still checked on the raw string, before the type is applied', () => {
  const nodes = tree({
    properties: {
      outer: {
        type: 'object',
        properties: { level: { type: 'integer', enum: [1, 2, 3] } },
        required: ['level'],
      },
    },
    required: ['outer'],
  });

  it('accepts a declared member and coerces it afterwards', () => {
    // The members were stringified out of the schema, so the comparison happens in the one notation
    // both sides share. Reversing the order would compare `2` against `"2"` and refuse every int.
    expect(collect(nodes, { 'outer.level': '2' })).toEqual({ outer: { level: 2 } });
  });

  it('refuses a non-member as a MEMBERSHIP problem, named by path', () => {
    expect(collect(nodes, { 'outer.level': '9' })).toBe(
      'REFUSED: outer.level: "9" is not one of 1, 2, 3'
    );
  });

  it('still refuses a blank required nested field as required, not as a bad member', () => {
    expect(collect(nodes, { 'outer.level': '' })).toBe('REFUSED: outer.level is required');
  });
});

describe('the length of a list is the operator’s', () => {
  const targets = node(tree(LISTS), 'targets');

  it('counts nothing before anybody presses add', () => {
    expect(rowCount({}, 'targets')).toBe(0);
  });

  it('adds a row seeded from the element’s declared defaults', () => {
    expect(addRow(targets, {})).toEqual({ 'targets.0': '', 'targets.0.host': '', 'targets.0.port': '80' });
  });

  it('closes the gap when a row in the middle goes, because position IS a row’s identity', () => {
    let values = addRow(targets, addRow(targets, addRow(targets, {})));
    values = { ...values, 'targets.0.host': 'a', 'targets.1.host': 'b', 'targets.2.host': 'c' };
    const after = removeRow(targets, values, 1);
    expect(rowCount(after, 'targets')).toBe(2);
    expect(after['targets.0.host']).toBe('a');
    expect(after['targets.1.host']).toBe('c');
  });

  it('leaves everything outside the list alone', () => {
    const values = { ...addRow(targets, {}), url: 'https://a.test', 'targets_other': 'x' };
    expect(removeRow(targets, values, 0)).toEqual({ url: 'https://a.test', targets_other: 'x' });
  });

  it('sees a row that only has child keys, so a map from elsewhere cannot lose one', () => {
    expect(rowCount({ 'targets.0.host': 'a', 'targets.1.host': 'b' }, 'targets')).toBe(2);
  });

  it('adds and removes rows on a list nested inside a group', () => {
    const nodes = tree({
      properties: {
        outer: { type: 'object', properties: { ports: { type: 'array', items: { type: 'integer' } } } },
      },
    });
    const ports = node(node(nodes, 'outer').children!, 'ports');
    const values = { ...addRow(ports, {}), 'outer.ports.0': '80' };
    expect(collect(nodes, values)).toEqual({ outer: { ports: [80] } });
    expect(collect(nodes, removeRow(ports, values, 0))).toEqual({});
  });
});
