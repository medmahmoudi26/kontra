/**
 * What a widget tile calls itself, and what it refuses to be called (ADR 0020, slice 7b).
 */

import { describe, expect, it } from 'vitest';
import { firstHeading, getWidgetTitle } from './getWidgetTitle';
import { WIDGET_TITLE_CAP, type Widget } from './parseWidget';

function widget(over: Partial<Widget> = {}): Widget {
  return { kind: 'markdown', body: 'body', ...over };
}

describe('precedence', () => {
  it('prefers __TITLE__, then __FILE__, then the first heading, then the fallback', () => {
    expect(
      getWidgetTitle(widget({ title: 'chosen', file: '/x/f.md', body: '# heading' }), 'kf-crawl-01 · actor')
    ).toBe('chosen');
    expect(getWidgetTitle(widget({ file: '/x/f.md', body: '# heading' }), 'fallback')).toBe('f.md');
    expect(getWidgetTitle(widget({ body: '## heading' }), 'fallback')).toBe('heading');
    expect(getWidgetTitle(widget(), 'kf-crawl-01 · actor')).toBe('kf-crawl-01 · actor');
    expect(getWidgetTitle(widget())).toBeUndefined();
  });
});

describe('a URL in the body is NOT a title', () => {
  it('ignores links and bare URLs, unlike tmuxy', () => {
    // tmuxy pulls a filename out of the first URL it finds. Here the body is scanner output, so that
    // rule would let a crawled page write the words above a tile.
    const body = 'crawled https://target.example/login-to-continue.html and 40 more';
    expect(getWidgetTitle(widget({ body }), 'kf-crawl-01 · actor')).toBe('kf-crawl-01 · actor');
  });
});

describe('flattening and caps', () => {
  it('collapses whitespace in a heading', () => {
    expect(firstHeading('#   spaced    out   ')).toBe('spaced out');
  });

  it('caps a hostile heading', () => {
    const long = firstHeading(`# ${'A'.repeat(500)}`);
    expect(long?.length).toBe(WIDGET_TITLE_CAP);
  });

  it('caps a hostile fallback too — the caller’s string is not automatically short', () => {
    expect(getWidgetTitle(widget(), 'B'.repeat(500))?.length).toBe(WIDGET_TITLE_CAP);
  });
});

describe('firstHeading', () => {
  it('accepts every ATX level, with or without closing hashes', () => {
    expect(firstHeading('###### deep')).toBe('deep');
    expect(firstHeading('## closed ##')).toBe('closed');
    expect(firstHeading('   # indented')).toBe('indented');
  });

  it('rejects what is not a heading', () => {
    expect(firstHeading('#no-space')).toBeUndefined();
    expect(firstHeading('####### seven hashes')).toBeUndefined();
    expect(firstHeading('title\n=====')).toBeUndefined();
    expect(firstHeading('    # a code block')).toBeUndefined();
  });

  it('only looks at the opening lines — a heading further down is a section, not the subject', () => {
    expect(firstHeading(`${'text\n'.repeat(30)}# late`)).toBeUndefined();
  });
});
