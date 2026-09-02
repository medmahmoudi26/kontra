/**
 * The closed-set and default-carrying half of a derived schema, which the field table and both
 * forms read through one flattening.
 */

import { describe, expect, it } from 'vitest';
import { schemaEnum, schemaDefault, schemaFields } from './MethodContract';
import { initialValues, coerceValues } from './formFields';

describe('a closed set is read from every spelling a deriver emits', () => {
  it('reads Go’s {type, enum}', () => {
    expect(schemaEnum({ type: 'string', enum: ['open', 'closed'] })).toEqual(['open', 'closed']);
  });

  it('reads a bare {enum}, which is how a Python Literal can arrive', () => {
    expect(schemaEnum({ enum: ['a', 'b'] })).toEqual(['a', 'b']);
  });

  it('walks anyOf, so an OPTIONAL Literal is still a closed set', () => {
    expect(schemaEnum({ anyOf: [{ enum: ['a', 'b'] }, { type: 'null' }] })).toEqual(['a', 'b']);
  });

  it('stringifies non-string members so an int enum is comparable to what an input holds', () => {
    expect(schemaEnum({ type: 'integer', enum: [1, 2, 3] })).toEqual(['1', '2', '3']);
  });

  it('is null for an open field — a free-text box, not an empty list', () => {
    expect(schemaEnum({ type: 'string' })).toBeNull();
  });
});

describe('a declared default reaches the form', () => {
  it('carries a scalar', () => {
    expect(schemaDefault({ type: 'integer', default: 200 })).toBe('200');
  });

  it('carries false and 0, which truthiness would have dropped', () => {
    expect(schemaDefault({ type: 'boolean', default: false })).toBe('false');
    expect(schemaDefault({ type: 'integer', default: 0 })).toBe('0');
  });

  it('is undefined when the author declared none', () => {
    expect(schemaDefault({ type: 'string' })).toBeUndefined();
  });

  it('prefills the form rather than opening blank', () => {
    const fields = schemaFields({
      properties: { size: { type: 'integer', default: 200 }, host: { type: 'string' } },
      required: ['host'],
    })!;
    expect(initialValues(fields)).toEqual({ size: '200', host: '' });
  });
});

describe('membership is refused before the value reaches an actor', () => {
  const fields = schemaFields({
    properties: { mode: { type: 'string', enum: ['fast', 'slow'] } },
    required: ['mode'],
  })!;

  it('accepts a declared member', () => {
    expect(coerceValues(fields, { mode: 'fast' })).toEqual({ value: { mode: 'fast' } });
  });

  it('refuses a value that is not one, and names the field and the choices', () => {
    const got = coerceValues(fields, { mode: 'medium' });
    expect(got).toEqual({ error: 'mode: "medium" is not one of fast, slow' });
  });

  it('still refuses a blank required field as required, not as a bad member', () => {
    expect(coerceValues(fields, { mode: '' })).toEqual({ error: 'mode is required' });
  });

  it('leaves an optional closed field out when blank, rather than sending ""', () => {
    const optional = schemaFields({
      properties: { mode: { type: 'string', enum: ['fast', 'slow'] } },
    })!;
    expect(coerceValues(optional, { mode: '' })).toEqual({ value: {} });
  });
});
