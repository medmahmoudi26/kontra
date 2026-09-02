/**
 * What is on this disk, at this digest — drawn (ADR 0030).
 *
 * The read-only source viewer stopped writing, and this strip is what took the write's place: the
 * folder's path to open in your own editor, and the digest of the code a worker actually registered.
 * Both are the acceptance criteria of slice 04 that a node test can pin — the panel shows the
 * registered digest, and there is a copy-path affordance and an "open in your editor" hint — so they
 * are pinned here rather than left to a component that imports CodeMirror and cannot be node-rendered.
 *
 * Static rendering, the same posture as `folderWorkbench.render.test.ts`: no jsdom in this suite, and
 * every fact under test is text or an attribute in the initial render.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { SourceProvenance } from './SourceProvenance';

const draw = (props: Parameters<typeof SourceProvenance>[0]): string =>
  renderToStaticMarkup(createElement(SourceProvenance, props));

describe('the source-provenance strip', () => {
  it('shows the path with a copy affordance and an open-in-your-editor hint', () => {
    const html = draw({
      path: '/root/kontra-local/.kontra/workflows/nscheck',
      digest: 'a1b2c3d4e5f6',
      testid: 'workflow-source-provenance',
    });
    // The path itself, and the button that copies it — the way back to the operator's own editor.
    expect(html).toContain('/root/kontra-local/.kontra/workflows/nscheck');
    expect(html).toContain('data-testid="workflow-source-provenance-copy"');
    // The hint that names where editing happens now, since the viewer no longer does.
    expect(html).toContain('data-testid="workflow-source-provenance-open"');
    expect(html).toContain('open in your editor');
  });

  it('shows the registered digest beside the source', () => {
    // The criterion: the panel shows the registered digest alongside the source it is displaying.
    const html = draw({
      path: '/src/nscheck',
      digest: 'a1b2c3d4e5f6',
      testid: 'workflow-source-provenance',
    });
    expect(html).toContain('data-testid="workflow-source-provenance-digest"');
    expect(html).toContain('a1b2c3d4e5f6');
    expect(html).toContain('registered digest');
  });

  it('draws "not served yet" as its own state, never a blank digest', () => {
    // A workflow no worker has registered has no digest — which must read as "nothing is serving
    // this", not as a digest that is pinned to nothing.
    const html = draw({ path: '/src/nscheck', digest: '', digestAbsent: 'not served yet' });
    expect(html).toContain('not served yet');
    expect(html).not.toContain('a1b2c3d4e5f6');
  });

  it('draws an actor folder as "unpinned" when the catalog has no digest for it', () => {
    // The workbench passes an actor's OCI digest (ADR 0011); absent, the same strip says unpinned
    // rather than inventing one.
    const html = draw({ path: '/src/probe', digest: undefined, digestAbsent: 'unpinned' });
    expect(html).toContain('unpinned');
  });
});
