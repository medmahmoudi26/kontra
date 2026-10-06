import { Liquid } from 'liquidjs';
import { describe, expect, it } from 'vitest';

import {
  CodeBlockSink,
  fenceFor,
  hexDump,
  normaliseLang,
  registerCodeTag,
  type RefResolution,
} from './codeTag';
import { markdownEscape } from './escape';

/**
 * `{% code %}` — the tag that carries evidence, so its tests are about bytes surviving and about a
 * fence that cannot be closed from inside.
 *
 * Acceptance tests 1 (fence breakout) and 7 (CRLF/NUL round trip) live here in their rendering half;
 * their storage and HTTP halves live in store and routes tests.
 */

/** A redaction double: the real rules arrive with the shared corpus. Enough to prove the wiring. */
function fakeRedact(text: string): string {
  return text
    .replace(/(Authorization:\s*)([^\r\n]+)/gi, '$1[redacted]')
    .replace(/(Cookie:\s*)([^\r\n]+)/gi, '$1[redacted]');
}

function engine(): { liquid: Liquid; sink: CodeBlockSink } {
  const DENY = () => {
    throw new Error('closed');
  };
  const liquid = new Liquid({
    fs: {
      readFileSync: DENY,
      readFile: DENY,
      existsSync: () => false,
      exists: async () => false,
      resolve: DENY,
      dirname: DENY,
      sep: '/',
      contains: () => false,
      fallback: () => undefined,
    } as never,
    root: [],
    relativeReference: false,
    strictVariables: true,
    strictFilters: true,
    lenientIf: true,
    ownPropertyOnly: true,
    jsTruthy: false,
    outputEscape: markdownEscape as never,
  });
  const sink = new CodeBlockSink();
  registerCodeTag(liquid, {
    async resolveRef(ref: string, limitBytes: number): Promise<RefResolution> {
      if (ref === 'cc://missing') return { unresolved: 'no actor served this ref within 5s' };
      if (ref === 'cc://big') {
        const full = Buffer.alloc(4 * 1024 * 1024, 0x41);
        return { bytes: full.subarray(0, limitBytes), fullBytes: full.length };
      }
      return { bytes: Buffer.from('from the store'), fullBytes: 14 };
    },
    redactLatin1: fakeRedact,
    sink,
    limitBytes: 1024,
  });
  return { liquid, sink };
}

describe('fenceFor — acceptance test 1, at the unit', () => {
  it('is three backticks when the content has none', () => {
    expect(fenceFor('plain')).toBe('```');
  });

  it('is one longer than the longest RUN, not the count of occurrences', () => {
    // ``` is ONE run of three. A fence of four closes it; a count-based implementation would see
    // three runs of one and emit two, which is not a fence at all.
    expect(fenceFor('a ``` b')).toBe('````');
    expect(fenceFor('a ` b ` c')).toBe('```');
    expect(fenceFor('`````')).toBe('``````');
  });

  it('counts a run that ends at the end of the content', () => {
    expect(fenceFor('text````')).toBe('`````');
  });
});

describe('normaliseLang', () => {
  it('accepts the documented class and lowercases', () => {
    expect(normaliseLang('http')).toBe('http');
    expect(normaliseLang('HTTP')).toBe('http');
    expect(normaliseLang('c++')).toBe('c++');
    expect(normaliseLang('objective-c')).toBe('objective-c');
  });

  it('falls back to text rather than erroring, because a bad lang is cosmetic', () => {
    expect(normaliseLang('java script')).toBe('text');
    expect(normaliseLang('a'.repeat(21))).toBe('text');
    expect(normaliseLang('<script>')).toBe('text');
    expect(normaliseLang(42)).toBe('text');
    expect(normaliseLang(undefined)).toBe('text');
  });
});

describe('the rendered block', () => {
  it('ACCEPTANCE 1: a value holding a fence and a close tag stays one block, and text after it survives as Markdown', async () => {
    const { liquid } = engine();
    const payload = '```\n</code>\n**not bold**\n```';
    const out = await liquid.parseAndRender('{% code "text", payload %}\n\n# A real heading\n', { payload });
    // One opening fence of four, one closing fence of four, and the payload verbatim inside.
    expect(out).toContain('````text kontra-block=b1\n');
    expect(out).toContain(payload);
    expect(out.match(/````/g)).toHaveLength(2);
    // The heading after the block is still a heading: the block did not swallow it.
    expect(out).toMatch(/# A real heading/);
  });

  it('writes the fence at the start of a line even when the tag is mid-paragraph', async () => {
    const { liquid } = engine();
    const out = await liquid.parseAndRender('text before {% code "sh", "ls" %}', {});
    expect(out).toContain('before \n```sh kontra-block=b1\n');
  });

  it('bypasses outputEscape — CONFIRMED on the pinned version, not assumed', async () => {
    const { liquid } = engine();
    // The same value through a hole and through the tag. The hole escapes, the tag does not.
    const out = await liquid.parseAndRender('{{ v }}|{% code "text", v %}', { v: 'a*b|c' });
    expect(out.startsWith('a\\*b\\|c')).toBe(true);
    expect(out).toContain('\n```text kontra-block=b1\na*b|c\n```\n');
  });

  it('fails under the SYNC api, which is why engine.ts has no sync path', () => {
    const { liquid } = engine();
    // The value comes from a variable because Liquid has no object literal: a template cannot
    // type {"ref": ...}, it can only name what the workflow returned.
    //
    // Under `parseAndRenderSync` a yielded promise comes back to the tag AS a promise. What happens
    // next depends on what the tag does with it, and both outcomes were measured on 10.30.0:
    // a tag that writes it to the emitter produces the literal string `[object Promise]` in the
    // document — silent corruption — and this tag, which reads a field off it, throws. Loud is
    // better, but neither is acceptable, so the engine has no sync path and this pins the reason.
    expect(() => liquid.parseAndRenderSync('{% code "text", v %}', { v: { ref: 'cc://ok' } })).toThrow(
      /Cannot read properties of undefined/
    );
  });
});

describe('value forms', () => {
  it('ACCEPTANCE 7: b64 carries CRLF, a NUL and invalid UTF-8 through unchanged', async () => {
    const { liquid, sink } = engine();
    const bytes = Buffer.from([
      0x47, 0x45, 0x54, 0x0d, 0x0a, // GET\r\n
      0x00, // NUL
      0x61, 0x09, 0x62, 0x0a, // a\tb\n
      0xff, 0xfe, // not valid UTF-8
    ]);
    await liquid.parseAndRender('{% code "http", v %}', { v: { b64: bytes.toString('base64') } });
    const block = sink.all()[0]!;
    expect(block.bytes.equals(bytes)).toBe(true);
    expect(block.fullBytes).toBe(bytes.length);
    expect(block.source).toBe('b64');
  });

  it('names a malformed b64 instead of decoding it leniently', async () => {
    const { liquid } = engine();
    await expect(liquid.parseAndRender('{% code "text", v %}', { v: { b64: 'not base64!!' } })).rejects.toThrow(
      'b64 is not base64'
    );
  });

  it('refuses a value that is neither a string, a b64 nor a ref — an author error, so it fails', async () => {
    const { liquid } = engine();
    await expect(liquid.parseAndRender('{% code "text", v %}', { v: 42 })).rejects.toThrow('must be a string');
    await expect(liquid.parseAndRender('{% code "text", v %}', { v: [1] })).rejects.toThrow('an array');
    await expect(liquid.parseAndRender('{% code "text", v %}', { v: null })).rejects.toThrow('value is empty');
  });

  it('takes a plain string as UTF-8 text', async () => {
    const { liquid, sink } = engine();
    await liquid.parseAndRender('{% code "py", "print(\\"é\\")" %}', {});
    expect(sink.all()[0]!.bytes.toString('utf8')).toBe('print("é")');
    expect(sink.all()[0]!.source).toBe('text');
  });
});

describe('a ref that cannot be read', () => {
  it('renders a marker naming the ref and the reason, and does NOT fail the render', async () => {
    const { liquid, sink } = engine();
    const out = await liquid.parseAndRender('{% code "http", v %}', { v: { ref: 'cc://missing' } });
    expect(out).toContain('this block was not read: no actor served this ref within 5s');
    expect(out).toContain('cc://missing');
    const block = sink.all()[0]!;
    expect(block.unresolved).toBe('no actor served this ref within 5s');
    expect(block.ref).toBe('cc://missing');
    expect(block.bytes).toHaveLength(0);
  });

  it('is distinguishable from a ref that legitimately held nothing', async () => {
    // The confusion this avoids cost a live run elsewhere in this system: an empty result and a
    // broken read looked identical.
    const { liquid, sink } = engine();
    await liquid.parseAndRender('{% code "text", v %}{% code "text", w %}', {
      v: { ref: 'cc://missing' },
      w: { b64: '' },
    });
    const [bad, empty] = sink.all();
    expect(bad!.unresolved).toBeTruthy();
    expect(empty!.unresolved).toBeUndefined();
    expect(empty!.bytes).toHaveLength(0);
  });

  it('ACCEPTANCE 8: truncates at the cap, keeps the full size, and says so in the record', async () => {
    const { liquid, sink } = engine();
    await liquid.parseAndRender('{% code "text", v %}', { v: { ref: 'cc://big' } });
    const block = sink.all()[0]!;
    expect(block.truncated).toBe(true);
    expect(block.bytes).toHaveLength(1024);
    expect(block.fullBytes).toBe(4 * 1024 * 1024);
  });
});

describe('redaction', () => {
  it('always redacts an http block, without the template asking', async () => {
    const { liquid, sink } = engine();
    await liquid.parseAndRender('{% code "http", v %}', {
      v: 'GET / HTTP/1.1\r\nAuthorization: Bearer abc123\r\nCookie: s=1\r\n',
    });
    const block = sink.all()[0]!;
    expect(block.redacted).toBe(true);
    expect(block.bytes.toString('latin1')).toContain('Authorization: [redacted]');
    expect(block.bytes.toString('latin1')).toContain('Cookie: [redacted]');
    expect(block.bytes.toString('latin1')).not.toContain('abc123');
    // The originals are kept for the audited reveal path, and only because redaction fired.
    expect(block.raw!.toString('latin1')).toContain('Bearer abc123');
  });

  it('redacts a non-http block when asked', async () => {
    const { liquid, sink } = engine();
    await liquid.parseAndRender('{% code "sh", v, redact: true %}', { v: 'curl -H "Authorization: Bearer t"' });
    expect(sink.all()[0]!.redacted).toBe(true);
  });

  it('keeps no unredacted copy when nothing was redacted', async () => {
    const { liquid, sink } = engine();
    await liquid.parseAndRender('{% code "http", "GET / HTTP/1.1\\r\\n\\r\\n" %}', {});
    const block = sink.all()[0]!;
    expect(block.redacted).toBe(false);
    expect(block.raw).toBeUndefined();
  });

  it('preserves every byte the rules do not touch, which a UTF-8 decode would not', async () => {
    const { liquid, sink } = engine();
    // 0xff is not valid UTF-8. Through a lossy decode it would become U+FFFD and never come back.
    const bytes = Buffer.concat([
      Buffer.from('POST / HTTP/1.1\r\nAuthorization: Bearer t\r\n\r\n', 'latin1'),
      Buffer.from([0xff, 0x00, 0xfe]),
    ]);
    await liquid.parseAndRender('{% code "http", v %}', { v: { b64: bytes.toString('base64') } });
    const stored = sink.all()[0]!.bytes;
    expect(stored.toString('latin1')).toContain('Authorization: [redacted]');
    expect(stored.subarray(stored.length - 3).equals(Buffer.from([0xff, 0x00, 0xfe]))).toBe(true);
  });
});

describe('the block marker in the info string', () => {
  it('carries the id so render.ts can match a block to its bytes without counting positions', async () => {
    const { liquid } = engine();
    const out = await liquid.parseAndRender('{% code "http", "x" %}', {});
    expect(out).toContain('```http kontra-block=b1\n');
  });

  it('leaves an author\'s own literal fence alone, which is why matching is by marker and not by order', async () => {
    const { liquid, sink } = engine();
    const out = await liquid.parseAndRender('```sh\nls -la\n```\n{% code "http", "x" %}', {});
    // The author's fence has no marker; the tag's does. render.test.ts asserts the consequence.
    expect(out).toContain('```sh\nls -la\n```');
    expect(out).toContain('```http kontra-block=b1\n');
    expect(sink.all()).toHaveLength(1);
  });
});

describe('block ids and the sink', () => {
  it('numbers blocks positionally in render order, so a re-render reuses the ids', async () => {
    const { liquid, sink } = engine();
    await liquid.parseAndRender('{% code "text", "one" %}{% code "text", "two" %}{% code "text", "one" %}', {});
    expect(sink.all().map((b) => b.id)).toEqual(['b1', 'b2', 'b3']);
    // Identical content is still two blocks: ids are positions, not hashes.
    expect(sink.all()[0]!.bytes.equals(sink.all()[2]!.bytes)).toBe(true);
  });

  it('exposes the id map the snapshot stores', async () => {
    const { liquid, sink } = engine();
    await liquid.parseAndRender('{% code "text", "x" %}', {});
    expect(Object.keys(sink.byId())).toEqual(['b1']);
  });
});

describe('show: "bytes"', () => {
  it('renders an xxd-style dump instead of the decoded text', async () => {
    const { liquid } = engine();
    const out = await liquid.parseAndRender('{% code "text", v, show: "bytes" %}', { v: 'AB' });
    expect(out).toContain('00000000  41 42');
    expect(out).toContain('|AB|');
  });

  it('hexDump marks unprintable bytes with a dot and pads the last line', () => {
    const dump = hexDump(Buffer.from([0x00, 0x41, 0xff]));
    expect(dump).toMatch(/^00000000  00 41 ff {41}\|\.A\.\|$/);
  });
});
