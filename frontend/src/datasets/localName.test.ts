/**
 * The derived name is stored UTC and READ local (ADR 0029 §2 fixes both). These pin that the
 * canonical string never moves, that only the instant is re-rendered, and that a rename — which
 * has no instant — passes through untouched.
 */

import { describe, expect, it } from 'vitest';

import { localDatasetName } from './localName';

const DERIVED = 'wf-nscheck-0.1.0--2026-08-19T14-49-20Z--4e9b1b';

describe('localDatasetName', () => {
  it('re-renders the instant in the viewer’s zone and leaves everything else alone', () => {
    // Two hours ahead of UTC: 14:49:20Z is 16:49:20 for this viewer, which is the number on the
    // clock beside them while the run is going.
    const out = localDatasetName(DERIVED, { locale: 'en-GB', timeZone: 'Europe/Paris' })!;
    expect(out.text).toBe('wf-nscheck-0.1.0--19 Aug 2026 16:49:20--4e9b1b');
    expect(out.localized).toBe(true);
    // The identity is untouched — this is the string the API returns and `kontra dataset ls`
    // prints, and it is what a rename addresses.
    expect(out.canonical).toBe(DERIVED);
  });

  it('renders a viewer west of UTC on the earlier date, not a shifted clock on the same one', () => {
    const out = localDatasetName(DERIVED, { locale: 'en-GB', timeZone: 'America/Los_Angeles' })!;
    expect(out.text).toBe('wf-nscheck-0.1.0--19 Aug 2026 07:49:20--4e9b1b');
  });

  it('crosses the date line where the zone does, rather than only moving the clock', () => {
    const midnight = 'wf-nscheck-0.1.0--2026-08-19T01-30-00Z--4e9b1b';
    const out = localDatasetName(midnight, { locale: 'en-GB', timeZone: 'America/Los_Angeles' })!;
    // 01:30Z on the 19th is 18:30 on the 18th in Los Angeles — the DAY has to move too, which a
    // string edit that only replaced the time portion would get wrong.
    expect(out.text).toBe('wf-nscheck-0.1.0--18 Aug 2026 18:30:00--4e9b1b');
  });

  it('keeps the ORDER fixed while the zone and the month spelling follow the viewer', () => {
    // A locale that would otherwise order month-first: the fields are assembled here, so two
    // operators comparing screens read the same shape.
    const us = localDatasetName(DERIVED, { locale: 'en-US', timeZone: 'UTC' })!;
    expect(us.text).toBe('wf-nscheck-0.1.0--19 Aug 2026 14:49:20--4e9b1b');
  });

  it('passes a RENAME through untouched — an operator’s words have no instant to localise', () => {
    const out = localDatasetName('the-interesting-sweep')!;
    expect(out.text).toBe('the-interesting-sweep');
    expect(out.localized).toBe(false);
    expect(out.canonical).toBe('the-interesting-sweep');
  });

  it('leaves a string that is not a derived name exactly as it arrived', () => {
    // A Dataset named before the format existed, or a spelling a future build mints that this one
    // predates. Neither is mangled; both are reported as un-localised so the caller can skip the
    // "canonical" hover that would then be a duplicate.
    for (const odd of ['lame', 'wf-nscheck-0.1.0--not-a-date--4e9b1b', 'wf-x--2026-08-19T14-49-20--y']) {
      const out = localDatasetName(odd)!;
      expect(out.text).toBe(odd);
      expect(out.localized).toBe(false);
    }
  });

  it('is undefined for an absent name, so a caller can render nothing without a branch', () => {
    expect(localDatasetName(undefined)).toBeUndefined();
    expect(localDatasetName('')).toBeUndefined();
  });
});
