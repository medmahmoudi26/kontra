import { Liquid } from 'liquidjs';
import { describe, expect, it } from 'vitest';

import {
  ALLOWED_FILTERS,
  DENIED_FILTERS,
  renderReport,
  type EngineDeps,
} from './engine';
import type { RefResolution } from './codeTag';

/**
 * The engine's tests are mostly about what a template CANNOT do, which is the only kind of claim a
 * sandbox makes. Acceptance tests 4 (template injection), 5 (limits) and 6 (strictness) live here.
 */

const deps: EngineDeps = {
  async resolveRef(): Promise<RefResolution> {
    return { bytes: Buffer.from('bytes'), fullBytes: 5 };
  },
  redactLatin1: (t) => t.replace(/(Authorization:\s*)([^\r\n]+)/gi, '$1[redacted]'),
  redactText: (t) => t.replace(/Bearer\s+\S+/g, '[redacted]'),
};

const render = (tpl: string, ctx: Record<string, unknown> = {}) => renderReport(tpl, ctx, deps);

describe('the filter allowlist is complete, which is what keeps it an allowlist', () => {
  it('classifies every filter liquidjs registers — so an upgrade fails here rather than silently granting one', () => {
    const builtins = Object.keys(new Liquid().filters as unknown as Record<string, unknown>);
    expect(builtins.length).toBeGreaterThan(80);

    const unclassified = builtins.filter((n) => !ALLOWED_FILTERS.has(n) && !(n in DENIED_FILTERS));
    expect(
      unclassified,
      `liquidjs registers ${unclassified.length} filter(s) this engine has never triaged: ${unclassified.join(', ')}. ` +
        'Add each to ALLOWED_FILTERS or to DENIED_FILTERS with a reason.'
    ).toEqual([]);

    // And nothing is in both, which would make the policy ambiguous.
    const both = builtins.filter((n) => ALLOWED_FILTERS.has(n) && n in DENIED_FILTERS);
    expect(both).toEqual([]);
  });

  it('names a reason for every refusal, because a reviewer should not have to re-derive the judgement', () => {
    for (const [name, why] of Object.entries(DENIED_FILTERS)) {
      expect(why.length, `${name} has no reason`).toBeGreaterThan(20);
    }
  });

  it('refuses a denied filter at PARSE time, so the author learns before a run instead of after', async () => {
    for (const name of ['raw', 'sample', 'json', 'base64_decode', 'url_decode', 'push']) {
      await expect(render(`{{ x | ${name} }}`, { x: 'a' }), `${name} was still usable`).rejects.toThrow(
        `undefined filter: ${name}`
      );
    }
  });

  it('keeps the filters a report actually needs', async () => {
    const out = await render(
      '{{ n | plus: 1 }} {{ s | upcase }} {{ a | size }} {{ a | join: "," }} {{ s | truncate: 3 }}',
      { n: 1, s: 'ab', a: [1, 2] }
    );
    expect(out.markdown).toBe('2 AB 2 1,2 ab');
  });

  it('adds | redact for a value an author knows is sensitive', async () => {
    const out = await render('{{ s | redact }}', { s: 'token Bearer abc123 here' });
    // The marker's own brackets are escaped, because `| redact` runs BEFORE outputEscape and its
    // result is still data. So a redacted value reads `\[redacted\]` in the Liquid output and
    // `[redacted]` once the mdast is rendered — correct at both stages, surprising at the first.
    expect(out.markdown).toBe('token \\[redacted\\] here');
    expect(out.markdown).not.toContain('abc123');
  });
});

describe('ACCEPTANCE 4: template injection fails cleanly, with no access to the process', () => {
  const cases: Array<[string, string, RegExp]> = [
    ['{{ result.constructor }}', 'the constructor', /undefined variable/i],
    ['{{ result.__proto__ }}', 'the prototype', /undefined variable/i],
    ['{{ result.constructor.constructor }}', 'the Function constructor', /undefined variable/i],
    ['{% include "/etc/passwd" %}', 'a file read', /not available in a report template/],
    ['{% render "/etc/passwd" %}', 'a partial render', /not available in a report template/],
    ['{% layout "x" %}', 'a layout', /not available in a report template/],
    ['{{ x | raw }}', 'the raw filter', /undefined filter: raw/],
  ];

  for (const [tpl, what, pattern] of cases) {
    it(`refuses ${what}`, async () => {
      await expect(render(tpl, { result: { a: 1 }, x: 'v' })).rejects.toThrow(pattern);
    });
  }

  it('leaks nothing about the process in the error it does produce', async () => {
    const err = await render('{% include "/etc/passwd" %}').catch((e: Error) => e);
    expect(String((err as Error).message)).not.toMatch(/\/root|node_modules|ENOENT|\/etc\/passwd/);
  });

  it('reserves {% image %} by name rather than leaving it free', async () => {
    await expect(render('{% image x %}', { x: 1 })).rejects.toThrow(/reserved for a future release/);
  });
});

describe('ACCEPTANCE 5: limits', () => {
  it('stops a 200 KB template at parseLimit, before rendering anything', async () => {
    const big = 'x'.repeat(200_000);
    await expect(render(big)).rejects.toThrow(/parse length limit exceeded/);
  });

  it('stops a 10^8-iteration loop — by memoryLimit, not renderLimit, and in 2 ms rather than 5,000', async () => {
    const started = Date.now();
    const err = await render('{% for i in (1..100000000) %}x{% endfor %}').catch((e: Error) => e);
    // Spec test 5 expects renderLimit to catch this. In the real configuration it does not get the
    // chance: each iteration allocates a character, so the cumulative-allocation counter trips first
    // and trips almost immediately. Both are a stop; this is the better one, and the test asserts
    // what actually happens rather than what the spec guessed.
    expect(String((err as Error).message)).toMatch(/memory alloc limit exceeded/);
    expect(Date.now() - started).toBeLessThan(1_000);
  }, 30_000);

  it('stops a long render when memory is NOT the binding limit, which is renderLimit\'s real case', async () => {
    const started = Date.now();
    // memoryLimit is raised out of the way on purpose. In the DEFAULT configuration it fires first on
    // any large loop — even `{% for i in (1..1e8) %}{% endfor %}` with an empty body, because the
    // range itself allocates — so renderLimit's own case is work that is slow without allocating
    // much: a big table through an expensive escape, which is exactly the 50,000-row measurement.
    const err = await renderReport(
      '{% for i in (1..100000000) %}{% endfor %}done',
      {},
      { ...deps, limits: { renderLimitMs: 300, memoryLimit: 10_000_000_000 } }
    ).catch((e: Error) => e);
    expect(String((err as Error).message)).toMatch(/render limit exceeded/);
    // The overshoot the worker deadline exists for: a 300 ms budget, checked periodically.
    expect(Date.now() - started).toBeGreaterThan(300);
  }, 30_000);

  it('stops a string-building loop at memoryLimit', async () => {
    await expect(
      render('{% assign s = "" %}{% for i in (1..4000) %}{% assign s = s | append: "0123456789" %}{% endfor %}{{ s | size }}')
    ).rejects.toThrow(/memory alloc limit exceeded/);
  });

  it('renders a 20,000-row table inside every limit, because a limit a real report trips is an outage and not a control', async () => {
    const rows = Array.from({ length: 20_000 }, (_, i) => ({ sku: `EU-${i}`, old: '1.00', new: '2.00' }));
    const out = await render(
      '{% for p in rows %}| {{ p.sku }} | {{ p.old }} | {{ p.new }} |\n{% endfor %}',
      { rows }
    );
    expect(out.markdown.split('\n')).toHaveLength(20_001);
  });
});

describe('ACCEPTANCE 6: strictness', () => {
  it('makes an unknown field a render error that names the path', async () => {
    const err = await render('{{ result.nonexistent }}', { result: { a: 1 } }).catch((e: Error) => e);
    expect(String((err as Error).message)).toMatch(/nonexistent/);
  });

  it('makes an unknown root a render error', async () => {
    await expect(render('{{ results.x }}', { result: {} })).rejects.toThrow(/undefined variable/i);
  });

  it('takes the else branch when a field is absent, rather than erroring', async () => {
    const out = await render('{% if result.maybe %}Y{% else %}N{% endif %}', { result: {} });
    expect(out.markdown).toBe('N');
  });

  it('takes the else branch when result is null, which is every run that did not complete', async () => {
    const out = await render('{% if result %}Y{% else %}N{% endif %}', { result: null });
    expect(out.markdown).toBe('N');
  });

  it('treats an empty string and zero as TRUTHY, which is Liquid and not JavaScript', async () => {
    // Documented here because it will surprise an author: `{% if result.sample_request %}` renders
    // the true branch for an empty string. The §2.4 contract does not say so; ADR 0055 does.
    expect((await render('{% if s %}Y{% else %}N{% endif %}', { s: '' })).markdown).toBe('Y');
    expect((await render('{% if z %}Y{% else %}N{% endif %}', { z: 0 })).markdown).toBe('Y');
    expect((await render('{% if n %}Y{% else %}N{% endif %}', { n: null })).markdown).toBe('N');
    expect((await render('{% if f %}Y{% else %}N{% endif %}', { f: false })).markdown).toBe('N');
  });
});

describe('escaping is wired, and the two trust levels stay separate', () => {
  it('escapes a value but not the template text around it', async () => {
    const out = await render('# Heading\n\n{{ v }}', { v: '# not a heading' });
    expect(out.markdown).toBe('# Heading\n\n\\# not a heading');
  });

  it('refuses to print a composite, instead of printing [object Object]', async () => {
    await expect(render('{{ result }}', { result: { a: 1 } })).rejects.toThrow(/pick a field/);
    await expect(render('{{ rows }}', { rows: [1, 2] })).rejects.toThrow(/loop over it/);
  });
});

describe('no state survives between renders', () => {
  it('numbers each render\'s blocks from b1, because a fresh instance holds nothing from the last', async () => {
    const first = await render('{% code "text", v %}{% code "text", v %}', { v: 'x' });
    const second = await render('{% code "text", v %}', { v: 'y' });
    expect(first.blocks.map((b) => b.id)).toEqual(['b1', 'b2']);
    expect(second.blocks.map((b) => b.id)).toEqual(['b1']);
  });

  it('produces identical output for identical input, which is what a re-render depends on', async () => {
    const tpl = '{{ run.id }} {{ result.n | plus: 1 }} {% code "http", req %}';
    const ctx = { run: { id: 'r_1' }, result: { n: 1 }, req: 'GET / HTTP/1.1\r\n' };
    const a = await render(tpl, ctx);
    const b = await render(tpl, ctx);
    expect(a.markdown).toBe(b.markdown);
    expect(a.blocks.map((x) => x.bytes.toString('base64'))).toEqual(
      b.blocks.map((x) => x.bytes.toString('base64'))
    );
  });
});
