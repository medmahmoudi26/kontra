/**
 * MermaidBlock's contract, minus the browser (ADR 0020, slice 7b).
 *
 * What a node-environment test CAN prove here: the configuration the ADR mandates, the source cap, the
 * SVG scrub, and — the one that matters most — that a diagram which has not rendered shows its SOURCE
 * rather than a blank box. That last one is provable precisely BECAUSE effects do not run under
 * `renderToStaticMarkup`: the markup below is the state a real browser is in for the whole time the
 * lazy `import('mermaid')` is in flight, and the state it stays in forever if the chart does not parse.
 *
 * What it cannot prove: that mermaid renders, that `parse` rejects what we think it rejects, and that
 * `DOMParser` + `importNode` puts an SVG on the page. Those need a real browser and belong in slice
 * 7a's Playwright suite — said plainly here rather than implied by a green suite.
 */

import { describe, expect, it } from 'vitest';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import MermaidBlock, { MERMAID_CONFIG, MERMAID_SOURCE_CAP, looksLikeSvg, scrubSvg } from './MermaidBlock';

describe('the configuration ADR 0020 requires', () => {
  it('is strict, and does not start on load', () => {
    expect(MERMAID_CONFIG.securityLevel).toBe('strict');
    expect(MERMAID_CONFIG.startOnLoad).toBe(false);
    // Redundant at strict level, stated anyway: this is the specific behaviour that would matter if
    // the level ever moved.
    expect(MERMAID_CONFIG.flowchart.htmlLabels).toBe(false);
  });
});

describe('a chart that has not rendered shows its source', () => {
  it('renders the source in a code block, never an empty box', () => {
    const chart = 'flowchart LR\n  a --> b';
    const html = renderToStaticMarkup(createElement(MermaidBlock, { chart }));
    expect(html).toContain('data-testid="mermaid-source"');
    expect(html).toContain('flowchart LR');
    expect(html).toContain('data-phase="source"');
  });

  it('escapes the source it shows, because a chart can come from pane text', () => {
    const html = renderToStaticMarkup(
      createElement(MermaidBlock, { chart: 'flowchart LR\n  a["<img src=x onerror=alert(1)>"] --> b' })
    );
    expect(html).not.toMatch(/<img[^>]/);
    expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;');
  });
});

describe('the source cap', () => {
  it('is smaller than the widget cap, because parsing is the expensive part', () => {
    expect(MERMAID_SOURCE_CAP).toBeLessThan(64 * 1024);
    expect(MERMAID_SOURCE_CAP).toBeGreaterThan(8 * 1024);
  });

  it('still renders an over-cap chart as its source', () => {
    const chart = `flowchart LR\n${'  a --> b\n'.repeat(4000)}`;
    expect(chart.length).toBeGreaterThan(MERMAID_SOURCE_CAP);
    const html = renderToStaticMarkup(createElement(MermaidBlock, { chart }));
    expect(html).toContain('data-testid="mermaid-source"');
  });
});

describe('scrubSvg — defence in depth, not the control', () => {
  it('removes script elements', () => {
    expect(scrubSvg('<svg><script>alert(1)</script><g/></svg>')).toBe('<svg><g/></svg>');
    expect(scrubSvg('<svg><script src="x"/><g/></svg>')).toBe('<svg><g/></svg>');
  });

  it('removes event-handler attributes, quoted or bare', () => {
    expect(scrubSvg('<svg onload="alert(1)"><g onclick=\'x\'/></svg>')).toBe('<svg><g/></svg>');
    expect(scrubSvg('<svg onload=alert(1)><g/></svg>')).toBe('<svg><g/></svg>');
  });

  it('removes foreignObject, the way HTML re-enters an SVG', () => {
    expect(scrubSvg('<svg><foreignObject><body onload=x/></foreignObject></svg>')).toBe('<svg></svg>');
  });

  it('leaves a normal mermaid SVG alone', () => {
    const svg = '<svg role="graphics-document document" viewBox="0 0 100 50"><g class="node"><text>a</text></g></svg>';
    expect(scrubSvg(svg)).toBe(svg);
  });
});

describe('looksLikeSvg', () => {
  it('accepts a document with or without a prologue', () => {
    expect(looksLikeSvg('<svg width="1"></svg>')).toBe(true);
    expect(looksLikeSvg('<?xml version="1.0"?>\n<svg>')).toBe(true);
    expect(looksLikeSvg('  \n<svg>')).toBe(true);
  });

  it('rejects anything else, including html that merely contains an svg', () => {
    expect(looksLikeSvg('<div><svg></svg></div>')).toBe(false);
    expect(looksLikeSvg('not markup')).toBe(false);
    expect(looksLikeSvg('')).toBe(false);
  });
});
