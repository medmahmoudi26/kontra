/**
 * The ORCHESTRATOR ARM of the Run-id slug contract (shared/conformance/slug.json).
 *
 * The other two arms are `tests/test_slug_conformance.py` and
 * `sdk/go/catalog/slug_conformance_test.go`, which assert that the two SDKs derive the same slug
 * from the same Run id. This one asserts the OTHER half of the contract, which had no test at all
 * on any side: that `safeName` — applied again on the way to a DuckLake table and an object key —
 * leaves what the SDKs produced alone.
 *
 * WHY THAT IS THE ASSERTION AND NOT `safeName(in) === slug`. Both SDK comments claimed `safeName`
 * applies "the same rule". It does not, and the corpus's `measured` field records the three places
 * it differs: `safeName` never truncates, its empty fallback is `unnamed` rather than `run`, and it
 * prefixes `a` to anything starting with a digit so the result is a legal bare SQL identifier.
 * There is a fourth, subtler one — its regex carries no `u` flag, so it walks UTF-16 code units and
 * writes TWO underscores for one astral code point where both SDKs write one. Asserting equality on
 * raw input would therefore be asserting a falsehood; the fixed point is what is actually true and
 * is what the system depends on.
 *
 * NONE OF THE FOUR IS LIVE, and this file is where that claim is checked rather than asserted:
 * every name the orchestrator receives is `tmp_<slug>_<hex8>`, which starts with `t`, is ASCII by
 * construction and is under any bound that matters — so the last test composes exactly that shape
 * from every row and pins it as a fixed point of `safeName`.
 */
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

import { safeName } from './parquet';

interface SlugCase {
  why: string;
  in: string;
  slug: string;
  safe_name: string;
}

const CORPUS = path.resolve(__dirname, '../../../../shared/conformance/slug.json');
const doc = JSON.parse(readFileSync(CORPUS, 'utf8')) as {
  bound: number;
  empty: string;
  cases: SlugCase[];
};

describe('the Run-id slug corpus, from the orchestrator side', () => {
  it('is not empty and still carries the inputs that diverge', () => {
    // A corpus that silently shrank to nothing would pass every case below.
    expect(doc.cases.length).toBeGreaterThanOrEqual(12);
    // Over the DECODED inputs, never the file's text — `́` and a literal combining acute are
    // the same input and only one of them survives an editor.
    const ins = doc.cases.map((c) => c.in).join('\n');
    for (const ch of ['/', ':', "'", ';', 'é', '\u{1f4e6}', '́', '.']) {
      expect(ins, `the corpus no longer exercises ${JSON.stringify(ch)}`).toContain(ch);
    }
  });

  it('leaves every slug the SDKs produce exactly as it found it, except where the corpus says otherwise', () => {
    for (const c of doc.cases) {
      expect(safeName(c.slug), c.why).toBe(c.safe_name);
    }
  });

  it('rewrites exactly one class of slug, and it is the leading digit', () => {
    // The divergence is CHARACTERISED rather than tolerated. A second class appearing here — a
    // character `safeName` maps that the SDKs do not, a new fallback — would go red on this line
    // rather than hide inside a fixture row somebody updated to make a suite green.
    const rewritten = doc.cases.filter((c) => c.slug !== c.safe_name);
    expect(rewritten.length).toBeGreaterThan(0);
    for (const c of rewritten) {
      expect(c.slug, c.why).toMatch(/^[0-9]/);
      expect(c.safe_name).toBe(`a${c.slug}`);
    }
  });

  it('is a no-op on the composed name, which is the only shape it is ever handed', () => {
    // `tmp_<slug>_<hex8>` — the name both SDKs build (kontra/catalog.py:TempDataset,
    // sdk/go/catalog/dataset.go:TempDataset). This is the whole reason the divergences above are
    // recorded rather than fixed: `tmp_` in front makes every one of them unreachable.
    for (const c of doc.cases) {
      const composed = `tmp_${c.slug}_deadbeef`;
      expect(safeName(composed), c.why).toBe(composed);
    }
  });

  it('does NOT agree with the SDKs on raw input, which is why nothing hands it one', () => {
    // The negative that keeps the bound honest. If somebody ever routes a raw Run id through
    // `safeName` and compares it to the SDK's slug, this row is the counter-example waiting for
    // them: one astral code point, two underscores.
    const astral = doc.cases.find((c) => /\u{1f4e6}/u.test(c.in));
    expect(astral, 'the corpus no longer carries an astral row').toBeDefined();
    expect(safeName(astral!.in)).not.toBe(astral!.slug);
    expect(safeName(astral!.in)).toBe('sweep-__');
  });
});
