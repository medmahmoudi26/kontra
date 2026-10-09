import { readFileSync } from 'node:fs';
import * as path from 'node:path';

import { describe, expect, it } from 'vitest';

import { MarkdownEscapeError, markdownEscape } from './escape';

/**
 * THE DRIVER FOR `escape.cases.json`.
 *
 * The cases live in JSON rather than inline so a reviewer can read what a scanned target is
 * prevented from doing to a report without reading a regex. This file's only job is to run them and
 * to refuse to pass vacuously — the failure mode `shared/conformance/README.md` step 3 names, and
 * the one a table-driven test is most prone to: a loop over an empty array reports success for
 * having found nothing.
 */

interface Case {
  why: string;
  value?: unknown;
  special?: string;
  out?: string;
  throws?: string;
}

interface Corpus {
  sections: Record<string, { why: string; cases: Case[] }>;
}

const corpus = JSON.parse(readFileSync(path.join(__dirname, 'escape.cases.json'), 'utf8')) as Corpus;

/** A 100k string, built here rather than stored in the corpus so the JSON stays readable. */
const LONG = 'long'.repeat(25_000);

/** The inputs JSON cannot express. Named in the corpus by `special`. */
const SPECIALS: Record<string, unknown> = {
  undefined: undefined,
  nan: Number.NaN,
  infinity: Number.POSITIVE_INFINITY,
  bigint: BigInt('9007199254740993'),
  date: new Date('2026-10-06T12:00:00.000Z'),
  function: () => 'nope',
  long: LONG,
};

function inputOf(c: Case): unknown {
  if (c.special !== undefined) {
    if (!(c.special in SPECIALS)) throw new Error(`corpus names an unknown special: ${c.special}`);
    return SPECIALS[c.special];
  }
  return c.value;
}

describe('the corpus itself', () => {
  it('carries sections, and every section carries cases', () => {
    const names = Object.keys(corpus.sections);
    expect(names.length).toBeGreaterThanOrEqual(9);
    for (const name of names) {
      expect(corpus.sections[name]!.cases.length, `section ${name} is empty`).toBeGreaterThan(0);
      expect(corpus.sections[name]!.why, `section ${name} has no why`).toBeTruthy();
    }
  });

  it('still carries the cases that break a naive implementation', () => {
    // Each of these is a case a rewrite would plausibly drop, and each one caught something real.
    const all = Object.values(corpus.sections).flatMap((s) => s.cases);
    const whys = all.map((c) => c.why).join(' | ');
    expect(whys, 'the single-escaped backslash case is gone').toMatch(/escaped exactly once/);
    expect(whys, 'the CRLF-is-one-space case is gone').toMatch(/CRLF becomes ONE space/);
    expect(whys, 'the NUL case is gone').toMatch(/NUL is removed/);
    expect(whys, 'the control-between-backslash-and-special case is gone').toMatch(
      /cannot sit between a backslash/
    );
    expect(whys, 'the composite-value refusals are gone').toMatch(/meant to loop/);
    expect(all.length).toBeGreaterThanOrEqual(50);
  });

  it('gives every case exactly one expectation', () => {
    for (const [name, section] of Object.entries(corpus.sections)) {
      for (const c of section.cases) {
        const hasOut = c.out !== undefined;
        const hasThrows = c.throws !== undefined;
        expect(hasOut !== hasThrows, `${name}: "${c.why}" has both or neither`).toBe(true);
      }
    }
  });
});

for (const [name, section] of Object.entries(corpus.sections)) {
  describe(`markdownEscape · ${name}`, () => {
    for (const c of section.cases) {
      it(c.why, () => {
        const input = inputOf(c);
        if (c.throws !== undefined) {
          expect(() => markdownEscape(input)).toThrow(MarkdownEscapeError);
          expect(() => markdownEscape(input)).toThrow(c.throws);
          return;
        }
        const got = markdownEscape(input);
        // The long case asserts a shape rather than 100k characters of expected output.
        if (c.special === 'long') {
          expect(got).toHaveLength(LONG.length);
          expect(got.startsWith(c.out!)).toBe(true);
          return;
        }
        expect(got).toBe(c.out);
      });
    }
  });
}

describe('the invariant, asserted directly rather than case by case', () => {
  /**
   * The cases say what happens to particular inputs. This says what must be true of EVERY output,
   * which is the claim the rest of the pipeline depends on: nothing coming out of here can change
   * the structure of the document it lands in.
   */
  const HOSTILE = [
    '| a | b |\n|---|---|\n| 1 | 2 |',
    '```\nfence\n```',
    '# heading\n\n- list\n\n> quote',
    '<img src=x onerror=alert(1)>',
    'a\u0000\u0007\u001b\u007f\r\n\tb',
    '[link](javascript:alert(1))',
    '*'.repeat(500),
    '\\'.repeat(500),
    '|'.repeat(500),
  ];

  for (const [i, raw] of HOSTILE.entries()) {
    it(`output ${i} carries no structural character`, () => {
      const out = markdownEscape(raw);
      expect(out, 'a line break survived').not.toMatch(/[\r\n]/);
      expect(out, 'a control character survived').not.toMatch(
        /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/
      );
      // Every special must be preceded by a backslash. Strip escaped pairs and nothing may remain.
      const withoutEscapes = out.replace(/\\[\\`*_{}[\]<>()#+\-!|~]/g, '');
      expect(withoutEscapes, 'an unescaped special survived').not.toMatch(
        /[\\`*_{}[\]<>()#+\-!|~]/
      );
    });
  }

  it('is idempotent in shape but NOT in value, which is why capture double-escapes', () => {
    // Documented, not desired: LiquidJS applies outputEscape to `{% capture %}`'s body AND to the
    // captured variable when it is printed, so a value that transits a capture is escaped twice.
    // Measured on liquidjs 10.30.0 and recorded in ADR 0055; the `serve` lint warns on it.
    const once = markdownEscape('a*b');
    const twice = markdownEscape(once);
    expect(once).toBe('a\\*b');
    expect(twice).toBe('a\\\\\\*b');
  });
});
