import { readFileSync } from 'node:fs';
import * as path from 'node:path';

import {
  REDACTED,
  REDACTED_VALUE,
  redactHttp,
  redactSentence,
  redactValue,
} from '@kontra/core';
import { describe, expect, it } from 'vitest';

/**
 * THE TYPESCRIPT ARM of `shared/conformance/redaction.json`.
 *
 * ── WHY THIS FILE DID NOT EXIST BEFORE ─────────────────────────────────────────────────────────
 *
 * `shared/conformance/README.md` said this corpus had "Python and Go arms, no TypeScript arm", and
 * gave a reason that was true: "the orchestrator does not compose asks or narrations, so there is no
 * third writer of *these* rules to drift." Run reports make it false. The orchestrator redacts an HTTP
 * message before storing it in a report snapshot, which is a third writer, so this arm and the
 * README's revised row arrived together.
 *
 * ── IT DRIVES ALL THREE SECTIONS, AND THAT IS NOT DECORATION ───────────────────────────────────
 *
 * The `http_headers` rule is the one this repository needed. `sentences` and `values` are driven here
 * too because `redactHttp` CALLS the sentence rule as its second pass — so a TypeScript sentence rule
 * that drifted from Python's would change HTTP redaction without any http case failing. An arm that
 * tested only its own new section would have a hole exactly where the composition is.
 *
 * ── THE STALE-BUILD TRAP ───────────────────────────────────────────────────────────────────────
 *
 * This imports `@kontra/core`, which resolves through node_modules to `shared/core/dist` — NOT to
 * `shared/core/src`. Editing `src/redaction.ts` and running a bare `vitest run` tests the PREVIOUS
 * build. `pnpm test` builds core first, which is why the package's scripts do that and why this file
 * must be run through it.
 */

interface StringCase {
  why: string;
  input: string;
  expect: string;
}

interface ValueCase {
  why: string;
  input: unknown;
  expect: unknown;
}

interface Corpus {
  sentence_redacted: string;
  value_redacted: string;
  sentences: StringCase[];
  values: ValueCase[];
  http_headers: StringCase[];
}

const corpus = JSON.parse(
  readFileSync(path.join(__dirname, '..', '..', '..', 'shared', 'conformance', 'redaction.json'), 'utf8')
) as Corpus;

describe('the corpus itself', () => {
  /**
   * THE GUARD THAT KEEPS THIS FILE FROM PASSING VACUOUSLY — `shared/conformance/README.md` step 3.
   * A loop over an empty array reports success for having found nothing, and the two existing arms
   * both have this guard with floors of 8 and 6. The third floor is new.
   */
  it('carries cases in all three sections', () => {
    expect(corpus.sentences.length).toBeGreaterThanOrEqual(8);
    expect(corpus.values.length).toBeGreaterThanOrEqual(6);
    expect(corpus.http_headers.length).toBeGreaterThanOrEqual(12);
  });

  it('still carries the cases that break a naive implementation', () => {
    const whys = corpus.http_headers.map((c) => c.why).join(' | ').toLowerCase();
    // Each of these is a case a rewrite would plausibly drop, and each one was a measured bug.
    expect(whys, 'the half-redacted multi-valued Cookie case is gone').toContain('multi-valued cookie');
    expect(whys, 'the JSON-body case is gone').toContain('json body');
    expect(whys, 'the non-breaking-space parity case is gone').toContain('non-breaking space');
    expect(whys, 'the idempotence case is gone').toContain('its own output');
    const inputs = corpus.http_headers.map((c) => c.input).join('\n');
    expect(inputs, 'nothing exercises a CRLF message any more').toContain('\r\n');
  });

  it('agrees with this implementation about the two markers', () => {
    expect(corpus.sentence_redacted).toBe(REDACTED);
    expect(corpus.value_redacted).toBe(REDACTED_VALUE);
  });
});

describe('sentences — the prose rule, driven here because redactHttp calls it', () => {
  for (const c of corpus.sentences) {
    it(c.why, () => {
      expect(redactSentence(c.input)).toBe(c.expect);
    });
  }
});

describe('values — the whole-key rule over a parsed tree', () => {
  for (const c of corpus.values) {
    it(c.why, () => {
      // Compared through a JSON round trip on both sides, as the Go arm does, so a number that
      // survived as an int on one side and a float on the other is not a false failure.
      expect(JSON.parse(JSON.stringify(redactValue(c.input)))).toEqual(
        JSON.parse(JSON.stringify(c.expect))
      );
    });
  }
});

describe('http_headers — the rule this repository added', () => {
  for (const c of corpus.http_headers) {
    it(c.why.slice(0, 120), () => {
      expect(redactHttp(c.input)).toBe(c.expect);
    });
  }

  it('is idempotent over every case, not only the one that says so', () => {
    for (const c of corpus.http_headers) {
      const once = redactHttp(c.input);
      expect(redactHttp(once), `not idempotent: ${c.why.slice(0, 60)}`).toBe(once);
    }
  });

  it('never leaves a credential it claims to have redacted', () => {
    // A property over the corpus rather than a case: if the output says [redacted], the secret that
    // was there must be gone. This is the assertion that would have caught `Cookie: [redacted] b=2`.
    const secrets = ['abc.def.ghi', 'dXNlcjpwYXNz', 'sk-live-0123', 'hunter2', 'IQoJb3JpZ2lu', 'a=1', 'sid=zzz'];
    for (const c of corpus.http_headers) {
      const out = redactHttp(c.input);
      if (!out.includes(REDACTED)) continue;
      for (const s of secrets) {
        if (!c.input.includes(s)) continue;
        // `a=1` survives legitimately in the request-line case, where the rule redacts a different
        // part of the line; the assertion is about the line that WAS redacted holding nothing.
        const redactedLines = out.split('\n').filter((l) => l.includes(REDACTED));
        for (const line of redactedLines) {
          expect(line, `${c.why.slice(0, 50)}: a redacted line still carries ${s}`).not.toContain(s);
        }
      }
    }
  });
});
