/**
 * Settings, drawn in each of its states.
 *
 * Node, no jsdom, no testing-library. `SettingsSurface` takes its sections as a prop, so "nothing
 * to configure", "a section whose backend does not exist" and "a section you can act on" are three
 * renders and not three mocks.
 *
 * THE ASSERTION THAT MATTERS MOST IS A NEGATIVE ONE: an unbuilt section must not draw a control. A
 * form that looks like it stores a credential and stores nothing is the single worst thing this
 * page could ship, and it is exactly what "just disable it for now" produces.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { SettingsSurface, type SettingsSection } from './SettingsPage';

const SECRETS: SettingsSection = {
  id: 'secrets',
  title: 'Secrets',
  blurb: 'Named, versioned references — never inlined.',
  state: 'unbuilt',
  needs: 'There is no secret store on this appliance yet.',
};

const APPEARANCE: SettingsSection = {
  id: 'appearance',
  title: 'Appearance',
  blurb: 'Light or dark.',
  state: 'ready',
  body: createElement('button', { type: 'button', 'data-testid': 'a-control' }, 'dark'),
};

function draw(sections: SettingsSection[]): string {
  return renderToStaticMarkup(createElement(SettingsSurface, { sections }));
}

describe('the Settings surface', () => {
  it('names itself and what a secret is, before there is a single secret', () => {
    const html = draw([SECRETS, APPEARANCE]);
    expect(html).toContain('data-testid="settings-page"');
    expect(html).toContain('Settings');
    // WRITE-ONLY IS THE CONTRACT, and it is stated on the page rather than only in a PRD: an
    // operator has to know before they type a value that they will never read it back.
    expect(html).toContain('write-only');
  });

  it('draws nothing but a sentence when there is nothing to configure', () => {
    // A header floating over an empty page reads as a page that failed to load.
    const html = draw([]);
    expect(html).toContain('data-testid="settings-empty"');
    expect(html).not.toContain('data-testid="settings-secrets"');
  });

  it('draws a section an operator can act on, with its control', () => {
    const html = draw([APPEARANCE]);
    expect(html).toContain('data-testid="settings-appearance"');
    expect(html).toContain('data-state="ready"');
    expect(html).toContain('data-testid="a-control"');
  });
});

describe('a section whose backend does not exist', () => {
  it('says so, and says where the setting lives until it does', () => {
    const html = draw([SECRETS]);
    expect(html).toContain('data-state="unbuilt"');
    expect(html).toContain('data-testid="settings-secrets-needs"');
    expect(html).toContain('no secret store on this appliance yet');
  });

  it('draws NO control — not a disabled one, not an empty form', () => {
    // A disabled input is a thing people keep clicking; a live-looking form that keeps nothing is
    // worse than an honest absence.
    const html = draw([{ ...SECRETS, body: APPEARANCE.body }]);
    expect(html).not.toContain('data-testid="a-control"');
    expect(html).toContain('data-testid="settings-secrets-needs"');
  });

  it('is a fact about this build, not a badge about a future one', () => {
    expect(draw([SECRETS])).toContain('no store yet');
  });
});

describe('what is deliberately not here', () => {
  it('invents no secrets, because there is no store to have any in', () => {
    // The one thing this page must never do is draw a plausible list. Nothing in the surface
    // supplies rows of its own — it draws what it is handed and nothing else.
    const html = draw([SECRETS, APPEARANCE]);
    expect(html).not.toMatch(/api[_-]?key/i);
    expect(html).not.toMatch(/sk-|AKIA|ghp_/);
  });
});
