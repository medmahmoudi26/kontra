/**
 * The orchestrator's arm of `shared/conformance/fleets.json`. The CLI's arm is
 * `cli/internal/config/fleets_test.go`; both read the same rows.
 */
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { FleetConfigError, loadFleets, parseFleets, publicFleets, resolveProfile } from './fleets';

const corpus = JSON.parse(
  require('node:fs').readFileSync(join(__dirname, '../../../../shared/conformance/fleets.json'), 'utf8')
) as {
  secret_fields: string[];
  valid: Array<{ why: string; yaml: string; expect: unknown; public: unknown }>;
  invalid: Array<{ why: string; yaml: string; error: string }>;
};

describe('fleets.json: valid files', () => {
  for (const c of corpus.valid) {
    it(c.why, () => {
      const got = parseFleets(c.yaml);
      expect(got).toEqual(c.expect);
      expect(publicFleets(got)).toEqual(c.public);
    });
  }
});

describe('fleets.json: refused files', () => {
  for (const c of corpus.invalid) {
    it(c.why, () => {
      expect(() => parseFleets(c.yaml)).toThrow(FleetConfigError);
      expect(() => parseFleets(c.yaml)).toThrow(c.error);
    });
  }
});

describe('the secret fields', () => {
  it('are the corpus list', async () => {
    const { SECRET_FIELDS } = await import('./fleets');
    expect([...SECRET_FIELDS].sort()).toEqual([...corpus.secret_fields].sort());
  });

  it('never appear in the public view, whatever the profile', () => {
    for (const c of corpus.valid) {
      const shown = JSON.stringify(publicFleets(parseFleets(c.yaml)));
      for (const field of corpus.secret_fields) expect(shown).not.toContain(`"${field}":`);
      expect(shown).not.toContain('dop_v1_');
    }
  });
});

describe('loadFleets / resolveProfile', () => {
  it('reads no file as the built-in local profile', () => {
    expect(loadFleets('/nonexistent/kontra.yaml')).toEqual(parseFleets(''));
  });

  it('reads the file it is pointed at', () => {
    const dir = mkdtempSync(join(tmpdir(), 'kontra-fleets-'));
    try {
      writeFileSync(join(dir, 'kontra.yaml'), 'fleets:\n  local:\n    provider: local\n    nodes: 3\n');
      expect((loadFleets(join(dir, 'kontra.yaml')).profiles.local as { nodes: number }).nodes).toBe(3);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('resolves a named profile, the default, and refuses one that is not there', () => {
    const fleets = parseFleets(corpus.valid[1]!.yaml);
    expect(resolveProfile(fleets).name).toBe('local');
    expect(resolveProfile(fleets, 'do-fra').profile.provider).toBe('digital_ocean');
    expect(() => resolveProfile(fleets, 'nowhere')).toThrow(/local, do-fra, customer-eks/);
  });
});
