import { readFileSync } from 'node:fs';
import * as path from 'node:path';
import { describe, expect, it } from 'vitest';

import { coerceFleetArgs } from './infra/stacks';

/**
 * THE TYPESCRIPT ARM of `shared/conformance/placement.json`.
 *
 * This side is the READER — the only one. `sdk/python/actorkit/fleet.py` and `cli/fleet.go` both
 * build a **Fleet**'s desired state and neither imports anything from here; `coerceFleetArgs`
 * narrows whatever arrives into `FleetArgs` before `programs/fleet.ts` runs it, and everything it
 * does not recognise it DISCARDS WITHOUT A WORD.
 *
 * That silence is the whole reason for the corpus. `--tmux` was sent as a boolean for a release and
 * dropped here every time, so a Machine could deploy "successfully" and never be viewable. And the
 * opposite direction is worse than a dropped flag: Pulumi's desired state is total, so a key a
 * WRITER stops sending is a request to REMOVE what it describes — an absent `machines` coerces to 0
 * and the converge deletes every Droplet in the **Fleet** while reporting success.
 *
 * This file also pins the one list all three arms touch: `ResolvedBundle` is declared next door as
 * "exactly the placement fields FleetArgs takes, so the caller forwards it verbatim", and the
 * Python SDK does forward it verbatim. A field added to it and not to `FleetArgs` is dropped on
 * arrival; a field removed from it leaves every placement short.
 *
 * The writer arms are `tests/test_placement_conformance.py` and `cli/placement_conformance_test.go`.
 */

const CORPUS = path.resolve(__dirname, '../../shared/conformance/placement.json');

interface Corpus {
  keys: { why: string; sections: Record<string, { keys: string[]; why: string }> };
  placement_keys: { required: string[]; optional: string[]; not_here: string[] };
  types: {
    rules: { string: string[]; number: string[]; string_array: string[]; object_array: string[] };
  };
  writer_cases: Array<{ name: string; keys: string[]; forbidden: string[] }>;
  reader_cases: Array<{ why: string; sent: Record<string, unknown>; kept: Record<string, unknown> }>;
}

const corpus = JSON.parse(readFileSync(CORPUS, 'utf8')) as Corpus;

describe('shared/conformance/placement.json — the reader', () => {
  it('still contains the inputs it exists for', () => {
    // A CORPUS THAT SILENTLY SHRANK TO NOTHING PASSES EVERYTHING. These are the rows whose loss
    // would take the guard with them, named individually rather than counted.
    expect(corpus.reader_cases.length).toBeGreaterThanOrEqual(11);
    const sent = corpus.reader_cases.map((c) => JSON.stringify(c.sent));
    expect(sent.some((s) => !s.includes('machines'))).toBe(true); // the fleet-destroying one
    expect(sent.some((s) => s.includes('true'))).toBe(true); // the dropped boolean
    expect(sent.some((s) => s.includes('credential'))).toBe(true);
    expect(sent.some((s) => s.includes('placements'))).toBe(true); // the packed one
    expect(corpus.writer_cases.map((c) => c.name)).toEqual([
      'machines_only',
      'placed',
      'placed_with_density',
      'packed',
      'spread',
    ]);
    expect(corpus.placement_keys.required).toEqual(['actorName', 'bundleUrl']);
  });

  it.each(corpus.reader_cases.map((c, i) => [i, c] as const))(
    'coerces case %i as the corpus says',
    (_i, c) => {
      expect(coerceFleetArgs(c.sent)).toEqual(c.kept);
    }
  );

  it('keeps every key the corpus lists and nothing else', () => {
    const kept = new Set([
      ...corpus.keys.sections.required_always.keys,
      ...corpus.keys.sections.provider.keys,
      ...corpus.keys.sections.from_resolver.keys,
      ...corpus.keys.sections.density.keys,
      ...corpus.keys.sections.packing.keys,
    ]);
    // Sent as a legal value of its own declared type, so anything missing from the result was
    // dropped because the READER does not know the key, not because the value was wrong.
    const sent: Record<string, unknown> = { };
    for (const k of corpus.types.rules.string) sent[k] = 'x';
    for (const k of corpus.types.rules.number) sent[k] = 7;
    for (const k of corpus.types.rules.string_array) sent[k] = ['1'];
    // A LEGAL ARRAY MEANS A NON-EMPTY ONE HERE. `placements: []` is a real desired state — a Fleet
    // that places nothing — and it survives coercion as `[]`, so an empty fixture would leave the
    // key present and prove the same thing; a one-entry fixture proves the entries survive too.
    for (const k of corpus.types.rules.object_array) {
      sent[k] = [{ actorName: 'a', bundleUrl: 'http://c/a' }];
    }
    const got = new Set(Object.keys(coerceFleetArgs(sent)));
    expect([...got].sort()).toEqual([...kept].sort());

    // AND THE ENTRY'S OWN KEY SET, which is a SECOND narrowing with a second list. A key the entry
    // reader does not know vanishes exactly as silently as one the converge reader does not know —
    // and `not_here` is the half worth pinning, because `region` inside a placement would be a
    // second answer to a question the converge has already answered.
    const entry: Record<string, unknown> = {};
    for (const k of [...corpus.placement_keys.required, ...corpus.placement_keys.optional]) {
      entry[k] = corpus.types.rules.number.includes(k) ? 7 : 'x';
    }
    entry.bundleUrl = 'http://c/a'; // must be non-empty or the whole entry is dropped
    for (const k of corpus.placement_keys.not_here) entry[k] = 'x';
    const coerced = coerceFleetArgs({ tag: 'dns', machines: 2, placements: [entry] });
    expect(Object.keys(coerced.placements![0]).sort()).toEqual(
      [...corpus.placement_keys.required, ...corpus.placement_keys.optional].sort()
    );

    // AND THE CREDENTIAL IS NOT AMONG THEM, even though both writers send it. `FleetArgs` is what
    // reaches the only provider-coupled file in the tree; a credential field on that type would be
    // one careless interpolation away from a token in a world-readable Bundle.
    for (const k of corpus.keys.sections.not_in_fleet_args.keys) {
      expect(Object.keys(coerceFleetArgs({ ...sent, [k]: 'do-prod' }))).not.toContain(k);
    }
  });

  it('never lets a boolean reach the program under any key', () => {
    // The rule with a corpse behind it (`--tmux`). Asserted over the whole declared key set rather
    // than over the one key that was lost, because the next one will be a different key.
    //
    // "NEVER ARRIVES AS A BOOLEAN" AND NOT "IS ABSENT", because the reader is not uniform and the
    // corpus says so: `tag` and `machines` are built before the narrowing loop and have hardcoded
    // defaults, so a boolean `tag` becomes '' — which `validateTag` then refuses loudly — while
    // every other key simply vanishes. Asserting absence would have been a green test that
    // described the wrong mechanism.
    const numeric = new Set(corpus.types.rules.number);
    const all = [
      ...corpus.types.rules.string,
      ...corpus.types.rules.number,
      ...corpus.types.rules.string_array,
      ...corpus.types.rules.object_array,
    ];
    for (const k of all) {
      const got = coerceFleetArgs({ tag: 'dns', machines: 2, [k]: true }) as Record<string, unknown>;
      expect(typeof got[k], `${k} sent as a boolean must not reach the program as one`).not.toBe(
        'boolean'
      );
      if (!numeric.has(k) && k !== 'tag') {
        expect(got[k], `${k} sent as a boolean must be dropped entirely`).toBeUndefined();
      }
    }
    // INSIDE A PLACEMENT TOO, and this is the one the design has to keep true: `spread=` is a
    // boolean at the caller SDK's call site and resolves to the number `workers` before anything
    // crosses. A writer that forwarded the flag would be narrowed away here, in silence, and one
    // Worker per Machine would quietly become whatever the default was.
    for (const k of [...corpus.placement_keys.required, ...corpus.placement_keys.optional]) {
      const entry = { actorName: 'a', bundleUrl: 'http://c/a', [k]: true };
      const got = coerceFleetArgs({ tag: 'dns', machines: 2, placements: [entry] });
      const kept = (got.placements?.[0] ?? {}) as Record<string, unknown>;
      expect(typeof kept[k], `${k} in a placement must not reach the program as a boolean`).not.toBe(
        'boolean'
      );
    }
    // …and the two required keys drop the WHOLE entry when they are a boolean, rather than leaving
    // a placement with nothing to place on it.
    for (const k of corpus.placement_keys.required) {
      const entry = { actorName: 'a', bundleUrl: 'http://c/a', [k]: true };
      expect(
        coerceFleetArgs({ tag: 'dns', machines: 2, placements: [entry] }).placements,
        `a boolean ${k} must take its entry with it`
      ).toEqual([]);
    }
    // AND THE TWO THAT ARE NOT MERELY DROPPED, pinned because they are silent rather than because
    // they are right. Both number keys go through `Number()`, so `true` is 1: a writer sending a
    // boolean count gets a one-Machine Fleet and a boolean density gets a cap of one Session, and
    // neither is refused anywhere downstream. `tag` is the third: it is built before the loop with
    // a hardcoded '' default, and `validateTag('')` is what makes THAT one loud.
    expect(coerceFleetArgs({ tag: 'dns', machines: true }).machines).toBe(1);
    expect(coerceFleetArgs({ tag: 'dns', machines: 2, maxSessions: true }).maxSessions).toBe(1);
    expect(coerceFleetArgs({ tag: true, machines: 2 }).tag).toBe('');
  });

  it('declares ResolvedBundle as exactly the corpus`s from_resolver list', () => {
    // THE ONE LIST ALL THREE ARMS TOUCH. Read off the source because the interface is erased at
    // runtime — the honest exception ADR 0035 rule two names, and the only one in this file.
    const src = readFileSync(path.resolve(__dirname, 'activities/fleet.ts'), 'utf8');
    const body = /export interface ResolvedBundle \{(.*?)\n\}/s.exec(src);
    expect(body, 'ResolvedBundle is not declared where this test expects it').toBeTruthy();
    const declared = [...(body![1] ?? '').matchAll(/^\s{2}(\w+):/gm)].map((m) => m[1]!);
    expect(declared.sort()).toEqual([...corpus.keys.sections.from_resolver.keys].sort());
  });
});
