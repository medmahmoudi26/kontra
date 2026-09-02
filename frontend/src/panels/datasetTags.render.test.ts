/**
 * Tagging from the Dataset's own page — the strip the listing cell and the console both draw.
 *
 * IT IS ONE COMPONENT FOR ONE REASON: a tag is what keeps output past the retention TTL (ADR 0029
 * §3), and two implementations of that affordance is two rules for one act. This suite is what makes
 * the sharing worth having — it asserts the strip's whole contract once, in node with no jsdom.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { TagStrip } from './DatasetPage';

const draw = (over: Partial<Parameters<typeof TagStrip>[0]> = {}): string =>
  renderToStaticMarkup(
    createElement(TagStrip, {
      dataset: 'lame',
      tags: ['keep', 'prod'],
      runId: 'nscheck-1',
      temporary: false,
      onAdd: () => undefined,
      onRemove: () => undefined,
      ...over,
    })
  );

describe('the tag set, drawn and editable', () => {
  it('draws a chip per tag, each with its own remove', () => {
    const html = draw();
    expect(html).toContain('data-testid="dataset-tag-lame-keep"');
    expect(html).toContain('data-testid="dataset-untag-lame-prod"');
    // Added and removed, never assigned: two operators tagging one Dataset converge.
    expect(html).toContain('data-testid="dataset-addtag-lame"');
  });

  it('says what a tag means where it differs — a temp keeps the RUN’s durable output', () => {
    // The chip is the same; the sentence is not. A tag on a temporary Dataset changes nothing about
    // the temp's own lifetime, and the affordance says so before it is used.
    expect(draw({ temporary: true })).toContain('never swept on a clock');
    expect(draw({ temporary: false })).toContain('expires after the retention TTL');
  });

  it('offers NO affordance where there is no Run to key the record on', () => {
    // The record is keyed by the Run (ADR 0029 §4). A button that can only 400 is worse than none.
    const html = draw({ runId: '' });
    expect(html).not.toContain('dataset-addtag');
    expect(html).toContain('nothing to address');
  });

  it('draws an untagged Dataset as an empty strip with the add still reachable', () => {
    const html = draw({ tags: [] });
    expect(html).toContain('data-testid="dataset-addtag-lame"');
    expect(html).not.toContain('data-testid="dataset-tag-lame-');
  });
});
