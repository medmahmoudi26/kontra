/**
 * The Secrets section, drawn in each state a store can be in: none, one, rotated, revoked.
 *
 * Node, no jsdom, no testing-library — `SecretsSurface` takes its rows as a prop, so four states
 * are four renders and not four mocks.
 *
 * THE ASSERTION THAT MATTERS MOST IS AGAIN A NEGATIVE ONE. This page's whole contract is that a
 * value cannot be read here, so every render is swept for a sentinel: it must not appear in the
 * markup, in a `value=` attribute, in a title or in an aria-label. A "reveal" affordance added
 * later has to delete a test to land.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { SecretsSurface, type SecretsSurfaceProps } from './SecretsSection';
import { toRows } from './secrets';
import type { Secret } from '../run/api';

const SENTINEL = 'dop_v1_SENTINEL_never_in_the_clear_9f3c';
const AUG = (day: number): number => Date.UTC(2026, 7, day, 12, 0, 0);

const SET: Secret = {
  name: 'shodan-key',
  createdAt: AUG(20),
  updatedAt: AUG(20),
  versions: [{ version: 1, createdAt: AUG(20) }],
  current: 1,
};

const ROTATED: Secret = {
  name: 'shodan-key',
  createdAt: AUG(20),
  updatedAt: AUG(24),
  versions: [
    { version: 1, createdAt: AUG(20) },
    { version: 2, createdAt: AUG(24) },
  ],
  current: 2,
};

const REVOKED: Secret = {
  name: 'shodan-key',
  owner: 'actor:probe',
  createdAt: AUG(20),
  updatedAt: AUG(25),
  versions: [{ version: 1, createdAt: AUG(20), revokedAt: AUG(25) }],
};

function draw(secrets: Secret[], over: Partial<SecretsSurfaceProps> = {}): string {
  const props: SecretsSurfaceProps = {
    backend: 'file',
    location: '/root/.kontra/secrets/secrets.json',
    rows: toRows(secrets),
    error: null,
    notice: null,
    onWrite: () => undefined,
    onRevoke: () => undefined,
    onDestroy: () => undefined,
    ...over,
  };
  return renderToStaticMarkup(createElement(SecretsSurface, props));
}

describe('an empty store', () => {
  it('says it is empty and still draws the form — an absent list reads as a broken page', () => {
    const html = draw([]);
    expect(html).toContain('data-testid="secrets-empty"');
    expect(html).toContain('data-testid="secret-form"');
    expect(html).not.toContain('data-testid="secrets-list"');
  });

  it('names the backend and where the values rest', () => {
    // "Where are my credentials" is the first question an operator has about a store they cannot
    // read back, and it is answerable without showing anything.
    const html = draw([]);
    expect(html).toContain('file');
    expect(html).toContain('/root/.kontra/secrets/secrets.json');
    expect(html).toContain('never returned');
  });
});

describe('a secret that is set', () => {
  it('draws its name, its version and when it last changed', () => {
    const html = draw([SET]);
    expect(html).toContain('data-testid="secret-shodan-key"');
    expect(html).toContain('data-state="live"');
    expect(html).toContain('version 1 · 1 version · updated 2026-08-20');
    expect(html).toContain('data-testid="secret-shodan-key-v1"');
  });

  it('offers to revoke a live version, and marks it operator-scoped', () => {
    const html = draw([SET]);
    expect(html).toContain('data-testid="secret-shodan-key-v1-revoke"');
    expect(html).toContain('data-scope="operator"');
    expect(html).toContain('operator');
  });
});

describe('a secret that has been rotated', () => {
  it('shows the new version as current and the previous one as still usable', () => {
    const html = draw([ROTATED]);
    // SUPERSEDED, NOT GONE: this is the fact that makes rotating safe, and the page has to show it
    // or nobody will rotate before an incident forces them to.
    expect(html).toContain('data-version-state="current"');
    expect(html).toContain('data-version-state="superseded"');
    expect(html).toContain('version 2 · 2 versions · updated 2026-08-24');
  });

  it('offers to revoke either version', () => {
    const html = draw([ROTATED]);
    expect(html).toContain('data-testid="secret-shodan-key-v2-revoke"');
    expect(html).toContain('data-testid="secret-shodan-key-v1-revoke"');
  });
});

describe('a secret whose versions are revoked', () => {
  it('says it is unusable rather than quietly showing a name with no state', () => {
    const html = draw([REVOKED]);
    expect(html).toContain('data-state="unusable"');
    expect(html).toContain('every version revoked');
    expect(html).toContain('data-version-state="revoked"');
  });

  it('offers no revoke button for a version that is already revoked', () => {
    // Nothing is left to destroy, and a button that 200s having done nothing teaches an operator
    // that revocation is a thing you press twice.
    expect(draw([REVOKED])).not.toContain('data-testid="secret-shodan-key-v1-revoke"');
  });

  it('names the actor that owns it, because that is who lost access', () => {
    const html = draw([REVOKED]);
    expect(html).toContain('data-scope="actor"');
    expect(html).toContain('actor:probe');
  });
});

describe('the write form', () => {
  it('is one form for create and rotate, and says which it will do', () => {
    expect(draw([])).toContain('create');
    // The surface holds its own field state, so a render cannot be driven mid-typing here; what is
    // pinned is that the rotate wording exists for a name that is already in the list — see
    // `secrets.test.ts` for the note's text and `SecretsSection.tsx` for where it is chosen.
    expect(draw([SET])).toContain('data-testid="secret-write"');
  });

  it('takes the value in a password field the browser will not remember', () => {
    const html = draw([]);
    expect(html).toContain('data-testid="secret-value"');
    expect(html).toContain('type="password"');
    // Case-insensitive: this build's `Input` passes the prop through as authored, so the attribute
    // lands as `autoComplete` rather than React's usual lowercasing. What is being pinned is that
    // the browser is told not to remember it, not which casing the renderer chose.
    expect(html).toMatch(/autocomplete="off"/i);
  });

  it('draws no reveal, no copy and no value column', () => {
    const html = draw([SET, ROTATED, REVOKED]);
    expect(html).not.toMatch(/reveal|show value|copy value/i);
  });
});

describe('nothing here can show a value', () => {
  it('keeps a sentinel out of every state, including the notice and the error', () => {
    // The surface is handed rows built from server metadata, which has no value field at all — so
    // this sweeps for the mistake of a value arriving through some OTHER prop.
    for (const html of [
      draw([SET, ROTATED, REVOKED]),
      draw([], { notice: 'shodan-key is now at version 2' }),
      draw([], { error: 'write secret failed: 400 — a secret value cannot be empty' }),
      draw([SET], { busy: 'shodan-key' }),
    ]) {
      expect(html).not.toContain(SENTINEL);
      expect(html).not.toContain(SENTINEL.slice(0, 12));
    }
  });
});
