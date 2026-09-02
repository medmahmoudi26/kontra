/**
 * What can be done to a registered folder, rendered.
 *
 * THIS FILE REPLACES `folderList.test.ts`, and the reason is the change it is testing. The folder
 * shelf is gone: it drew a row per registered folder above the Actors grid and below the Workflows
 * list, which on an ordinary installation is the folders those surfaces already name — the same
 * workflow twice on one screen, in two vocabularies. The path moved onto the row that names it, and
 * the affordances moved into one component.
 *
 * WHAT SURVIVES IS THE RULE THAT COSTS A 400. A discovered folder has no registration to remove
 * (`SourceStore.forget` refuses an `at:` id), so it must never be offered a forget button — and now
 * that two surfaces draw it, one component is what stops one of them forgetting.
 *
 * Rendered rather than asserted on helpers, because what is being pinned is a property of the
 * MARKUP. Static markup is enough: this holds no state, and its one handler is the server call the
 * API test covers.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { Source } from '../run/api';
import { FolderAbsent, FolderActions } from './RegisteredFolders';

function registered(name: string, dir: string, over: Partial<Source> = {}): Source {
  return {
    id: `actor:${name}:1f3k`,
    kind: 'actor',
    name,
    path: dir,
    version: '0.1.0',
    description: '',
    registeredAt: 1_700_000_000_000,
    ...over,
  };
}

/** What `GET /api/sources/:kind` returns for a folder it found under the default root. */
function discovered(name: string, dir: string): Source {
  return { ...registered(name, dir), id: `at:${dir}`, registeredAt: 0 };
}

const draw = (source: Source, over: Record<string, unknown> = {}): string =>
  renderToStaticMarkup(
    createElement(FolderActions, {
      source,
      busy: false,
      testid: 'folder',
      onForget: () => {},
      ...over,
    })
  );

describe('forgetting a registration', () => {
  it('offers forget on a registered folder, and names the path it will NOT touch', () => {
    const html = draw(registered('probe', '/srv/checkout/probe'));
    expect(html).toContain('data-testid="folder-forget"');
    expect(html).toContain('/srv/checkout/probe is not touched');
  });

  it('offers NO forget on a discovered one, because the server refuses it', () => {
    // The whole reason this is one component: `SourceStore.forget` 400s on an `at:` id, and a
    // surface that drew the button anyway would be an affordance that exists only to fail.
    const html = draw(discovered('probe', '/home/me/.kontra/actors/probe'));
    expect(html).toContain('data-testid="folder-discovered"');
    expect(html).not.toContain('data-testid="folder-forget"');
  });

  it('draws nothing at all for a surface that passes no handler', () => {
    expect(draw(registered('probe', '/srv/checkout/probe'), { onForget: undefined })).toBe('');
  });

  it('disables while a forget is in flight', () => {
    // The whole group disables: two forgets in flight against one list is a race the shelf's own
    // `forgetting: string | null` cannot represent.
    expect(draw(registered('probe', '/srv/checkout/probe'), { busy: true })).toContain('disabled');
  });

  it('is addressable per surface, so a card and a row are not the same element', () => {
    const card = draw(registered('probe', '/srv/checkout/probe'), { testid: 'actor-probe' });
    expect(card).toContain('data-testid="actor-probe-forget"');
  });
});

describe('a folder that is gone', () => {
  const gone = registered('probe', '/srv/checkout/probe', { absent: true });

  it('is marked absent, and keeps its forget button — which is when it is wanted most', () => {
    expect(renderToStaticMarkup(createElement(FolderAbsent, { source: gone, testid: 'folder' })))
      .toContain('data-testid="folder-absent"');
    expect(draw(gone)).toContain('data-testid="folder-forget"');
  });

  it('marks nothing on a folder that is present', () => {
    const html = renderToStaticMarkup(
      createElement(FolderAbsent, { source: registered('probe', '/srv/checkout/probe'), testid: 'folder' })
    );
    expect(html).toBe('');
  });
});
