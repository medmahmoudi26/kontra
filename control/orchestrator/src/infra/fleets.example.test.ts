import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { parseFleets, publicFleets } from './fleets';

// THE SHIPPED EXAMPLE PARSES, and only with dummy secrets. A template an install copies must be one
// the infra role accepts, or the first `kontra fleet up` fails on a file nobody wrote.
describe('kontra.example.yaml', () => {
  const text = readFileSync(join(__dirname, '../../../../kontra.example.yaml'), 'utf8');

  it('is a valid fleets file with a local default', () => {
    const fleets = parseFleets(text);
    expect(fleets.default).toBe('local');
    expect(Object.keys(fleets.profiles).sort()).toEqual(['customer-eks', 'do-fra', 'local']);
    expect(publicFleets(fleets).profiles['do-fra']!.secrets).toEqual(['token']);
  });

  it('carries only dummy credentials', () => {
    const fleets = parseFleets(text);
    const token = (fleets.profiles['do-fra'] as { token: string }).token;
    expect(token).toBe('dop_v1_EXAMPLE_not_a_real_token');
  });
});
