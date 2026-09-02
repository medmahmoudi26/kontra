/**
 * The cross-version check, on its own — the route's half is in server.test.ts.
 *
 * TWO THINGS ARE BEING PINNED and they pull in opposite directions. One is that the breaks are
 * FOUND: a new required input field and a removed output field are what a caller meets as data that
 * does not fit, and nothing anywhere recorded them before this. The other is that the quiet cases
 * stay quiet — an added optional input, an added output — because a check that reports every diff
 * is a check whose findings get scrolled past, and then the one that mattered is scrolled past too.
 *
 * And the third answer is UNKNOWN, which is not a shade of compatible. A first version, a Method
 * new in this one and a schema that declares no properties were not compared; saying "compatible"
 * about any of them would be the system claiming to have checked something it cannot read.
 */
import { describe, expect, it } from 'vitest';

import { compareWithPreceding, precedingVersion, type ComparableVersion } from './compat';

/** A catalogued version of one actor, with whatever Methods the case needs. */
const at = (version: string, operations: ComparableVersion['operations'] = []): ComparableVersion => ({
  name: 'probe',
  version,
  operations,
});

/** `{host}` with `host` required — the shape both directions are moved around from. */
const HOST = { type: 'object', properties: { host: { type: 'string' } }, required: ['host'] };

const check = (report: ReturnType<typeof compareWithPreceding>, method: string, field: string) =>
  report.checks.find((c) => c.method === method && c.field === field);

describe('input is BACKWARD', () => {
  it('reports a field that is now required, naming the Method and the direction', () => {
    // The break ADR 0027 §4 names: a caller shaped for 0.1.0 sends `{host}` and the new schema
    // demands `{host, timeout}`, so every existing producer and every saved pipeline fails —
    // registering cleanly, with nothing anywhere recording that the shape moved.
    const before = at('0.1.0', [{ name: 'fetch', input: HOST }]);
    const after = at('0.2.0', [
      {
        name: 'fetch',
        input: {
          type: 'object',
          properties: { host: { type: 'string' }, timeout: { type: 'integer' } },
          required: ['host', 'timeout'],
        },
      },
    ]);

    const report = compareWithPreceding(after, [before]);
    expect(report.previous).toBe('0.1.0');
    expect(report.findings).toHaveLength(1);
    const [found] = report.findings;
    expect(found?.method).toBe('fetch');
    expect(found?.field).toBe('input');
    expect(found?.rule).toBe('BACKWARD');
    expect(found?.previous).toBe('0.1.0');
    expect(found?.detail).toContain('"timeout"');
  });

  it('says nothing about a field that was ADDED as optional', () => {
    // The whole point of the direction rule. This is how an actor grows without breaking anybody,
    // and reporting it would train a reader to ignore the report.
    const before = at('0.1.0', [{ name: 'fetch', input: HOST }]);
    const after = at('0.2.0', [
      {
        name: 'fetch',
        input: {
          type: 'object',
          properties: { host: { type: 'string' }, timeout: { type: 'integer' } },
          required: ['host'],
        },
      },
    ]);
    expect(compareWithPreceding(after, [before]).findings).toEqual([]);
    expect(check(compareWithPreceding(after, [before]), 'fetch', 'input')?.verdict).toBe('compatible');
  });

  it('reports a field that no longer accepts what it used to', () => {
    // `Optional[str]` arrives as an anyOf union from pydantic, so making it non-nullable is a
    // narrowing an emitter produces routinely — and every caller that sent null is refused.
    const before = at('0.1.0', [
      {
        name: 'fetch',
        input: {
          type: 'object',
          properties: { host: { anyOf: [{ type: 'string' }, { type: 'null' }] } },
        },
      },
    ]);
    const after = at('0.2.0', [{ name: 'fetch', input: { type: 'object', properties: { host: { type: 'string' } } } }]);
    const [found] = compareWithPreceding(after, [before]).findings;
    expect(found?.rule).toBe('BACKWARD');
    expect(found?.detail).toContain('"null"');
  });
});

describe('output is FORWARD', () => {
  it('reports a removed field, naming the Method and the direction', () => {
    const before = at('0.1.0', [
      { name: 'fetch', output: { type: 'object', properties: { body: { type: 'string' }, status: { type: 'integer' } } } },
    ]);
    const after = at('0.2.0', [
      { name: 'fetch', output: { type: 'object', properties: { body: { type: 'string' } } } },
    ]);

    const report = compareWithPreceding(after, [before]);
    expect(report.findings).toHaveLength(1);
    const [found] = report.findings;
    expect(found?.field).toBe('output');
    expect(found?.rule).toBe('FORWARD');
    expect(found?.detail).toContain('"status"');
    expect(found?.detail).toMatch(/removed/);
  });

  it('says nothing about an ADDED output field', () => {
    // An old consumer reads the fields it knows and ignores the rest — the asymmetry with the
    // input rule, which is the reason both directions exist rather than one "did it change".
    const before = at('0.1.0', [{ name: 'fetch', output: { type: 'object', properties: { body: { type: 'string' } } } }]);
    const after = at('0.2.0', [
      { name: 'fetch', output: { type: 'object', properties: { body: { type: 'string' }, status: { type: 'integer' } } } },
    ]);
    expect(compareWithPreceding(after, [before]).findings).toEqual([]);
  });

  it('reports a field that is no longer guaranteed', () => {
    // Present but optional: a consumer that reads it unconditionally breaks on the first unit that
    // omits it, which is a break that only shows up on some of the data.
    const before = at('0.1.0', [
      { name: 'fetch', output: { type: 'object', properties: { body: { type: 'string' } }, required: ['body'] } },
    ]);
    const after = at('0.2.0', [
      { name: 'fetch', output: { type: 'object', properties: { body: { type: 'string' } } } },
    ]);
    expect(compareWithPreceding(after, [before]).findings[0]?.detail).toMatch(/no longer guaranteed/);
  });
});

describe('nothing to compare is not compatible', () => {
  it('has no previous version, and no checks, for a first registration', () => {
    const report = compareWithPreceding(at('0.1.0', [{ name: 'fetch', input: HOST }]), []);
    expect(report.previous).toBeUndefined();
    expect(report.checks).toEqual([]);
    expect(report.findings).toEqual([]);
  });

  it('calls a Method that is new in this version unknown, not compatible', () => {
    const before = at('0.1.0', [{ name: 'fetch', input: HOST }]);
    const after = at('0.2.0', [{ name: 'fetch', input: HOST }, { name: 'title', input: HOST }]);
    const report = compareWithPreceding(after, [before]);
    expect(check(report, 'title', 'input')?.verdict).toBe('unknown');
    expect(check(report, 'title', 'input')?.detail).toContain('new in this version');
    expect(report.findings).toEqual([]);
  });

  it('calls an UNSET schema on either side unknown, not compatible', () => {
    // `probe` in the golden descriptor declares neither `takes` nor `emits`. Declaring one in the
    // next version is not a break of a promise nobody made, and it is not a promise kept either.
    const before = at('0.1.0', [{ name: 'fetch' }]);
    const after = at('0.2.0', [{ name: 'fetch', input: HOST }]);
    const forward = compareWithPreceding(after, [before]);
    expect(forward.findings).toEqual([]);
    expect(check(forward, 'fetch', 'input')?.verdict).toBe('unknown');

    // …and the other way round: a version that stops declaring one.
    const dropped = compareWithPreceding(at('0.3.0', [{ name: 'fetch' }]), [after]);
    expect(check(dropped, 'fetch', 'input')?.verdict).toBe('unknown');
    expect(dropped.findings).toEqual([]);
  });

  it('calls a schema that says nothing structural unknown, not compatible', () => {
    // `{"type":"object"}` is a legal schema that describes no fields — half the catalog's Go actors
    // emit exactly that. Comparing it to one that lists fields would read every field as added or
    // removed, which is a diff of two documents and not a statement about a caller.
    const before = at('0.1.0', [{ name: 'fetch', output: { type: 'object' } }]);
    const after = at('0.2.0', [
      { name: 'fetch', output: { type: 'object', properties: { body: { type: 'string' } } } },
    ]);
    const report = compareWithPreceding(after, [before]);
    expect(check(report, 'fetch', 'output')?.verdict).toBe('unknown');
    expect(check(report, 'fetch', 'output')?.detail).toContain('no properties');
    expect(report.findings).toEqual([]);
  });

  it('says nothing about a property whose type neither side declares', () => {
    // A `$ref` to a definition, a bare `{}`: unreadable here, and an unreadable property reported
    // as a break would be a finding an operator cannot act on.
    const before = at('0.1.0', [
      { name: 'fetch', output: { type: 'object', properties: { body: { $ref: '#/$defs/Body' } } } },
    ]);
    const after = at('0.2.0', [
      { name: 'fetch', output: { type: 'object', properties: { body: { $ref: '#/$defs/Other' } } } },
    ]);
    expect(compareWithPreceding(after, [before]).findings).toEqual([]);
  });
});

describe('which version counts as the preceding one', () => {
  it('compares against 0.9.0, not 0.10.0 — the ordering a string sort gets wrong', () => {
    // THE EDGE THIS SHARES WITH THE PALETTE. Lexically `0.10.0` < `0.9.0`, so a string comparison
    // would call 0.9.0 the newest older version of 0.10.0 only by accident, and here it would
    // compare 0.11.0 against 0.9.0 — reporting a diff nobody made in the version they last shipped.
    const catalog = [
      at('0.2.0', [{ name: 'fetch', output: { type: 'object', properties: { a: { type: 'string' } } } }]),
      at('0.9.0', [{ name: 'fetch', output: { type: 'object', properties: { b: { type: 'string' } } } }]),
      at('0.10.0', [{ name: 'fetch', output: { type: 'object', properties: { c: { type: 'string' } } } }]),
    ];
    expect(precedingVersion(at('0.11.0'), catalog)?.version).toBe('0.10.0');
    expect(precedingVersion(at('0.10.0'), catalog)?.version).toBe('0.9.0');

    // and the finding names the field 0.9.0 actually had, which is the observable half
    const report = compareWithPreceding(
      at('0.10.0', [{ name: 'fetch', output: { type: 'object', properties: { c: { type: 'string' } } } }]),
      catalog
    );
    expect(report.previous).toBe('0.9.0');
    expect(report.findings[0]?.detail).toContain('"b"');
  });

  it('orders a prerelease below its release', () => {
    const catalog = [at('1.0.0'), at('1.0.0-rc.1'), at('0.9.0')];
    expect(precedingVersion(at('1.0.0'), catalog)?.version).toBe('1.0.0-rc.1');
    expect(precedingVersion(at('1.0.0-rc.1'), catalog)?.version).toBe('0.9.0');
  });

  it('ignores other actors and never picks itself', () => {
    // A re-registration (a worker restarting) compares against the version BEFORE, not against the
    // row it is about to overwrite — otherwise every restart would compare a version to itself and
    // the finding would flip to empty on reboot.
    const catalog: ComparableVersion[] = [
      at('0.1.0'),
      at('0.2.0'),
      { name: 'beacon', version: '9.9.9', operations: [] },
    ];
    expect(precedingVersion(at('0.2.0'), catalog)?.version).toBe('0.1.0');
    expect(precedingVersion(at('0.1.0'), catalog)).toBeUndefined();
  });

  it('compares against the immediately preceding version only', () => {
    // 0.3.0 is checked against 0.2.0 and never against 0.1.0: a field dropped in 0.2.0 is reported
    // once, on the version that dropped it, rather than again on every release after it.
    const withField = { type: 'object', properties: { body: { type: 'string' }, status: { type: 'integer' } } };
    const withoutField = { type: 'object', properties: { body: { type: 'string' } } };
    const catalog = [
      at('0.1.0', [{ name: 'fetch', output: withField }]),
      at('0.2.0', [{ name: 'fetch', output: withoutField }]),
    ];
    expect(compareWithPreceding(at('0.3.0', [{ name: 'fetch', output: withoutField }]), catalog).findings).toEqual(
      []
    );
  });
});

describe('one registration, both directions at once', () => {
  it('reports each Method and each direction separately', () => {
    // A version can move both ways on one Method, and a single "incompatible" line would leave the
    // reader to work out which half of the signature to look at.
    const before = at('0.1.0', [
      { name: 'fetch', input: HOST, output: { type: 'object', properties: { body: { type: 'string' } } } },
    ]);
    const after = at('0.2.0', [
      {
        name: 'fetch',
        input: {
          type: 'object',
          properties: { host: { type: 'string' }, depth: { type: 'integer' } },
          required: ['host', 'depth'],
        },
        output: { type: 'object', properties: {} },
      },
    ]);
    const { findings } = compareWithPreceding(after, [before]);
    expect(findings.map((f) => `${f.method}.${f.field}:${f.rule}`)).toEqual([
      'fetch.input:BACKWARD',
      'fetch.output:FORWARD',
    ]);
  });
});
