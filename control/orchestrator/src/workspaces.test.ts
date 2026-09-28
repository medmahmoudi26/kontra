import { mkdirSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  assertWorkspaceName,
  createWorkspace,
  describeWorkspaces,
  useWorkspace,
  workspaceAddress,
  workspaceRoot,
} from './workspaces';

function tmpParent(): string {
  const dir = path.join(os.tmpdir(), `kontra-ws-${process.pid}-${Math.random().toString(16).slice(2)}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

describe('named workspaces', () => {
  it('resolves the current child from KONTRA_WORKSPACES + .current', () => {
    const parent = tmpParent();
    mkdirSync(path.join(parent, 'hello'), { recursive: true });
    writeFileSync(path.join(parent, '.current'), 'hello\n');
    const env = { KONTRA_WORKSPACES: parent };
    expect(workspaceRoot(env)).toBe(path.join(parent, 'hello'));
    expect(describeWorkspaces(env).names).toEqual(['hello']);
  });

  it('falls back to legacy KONTRA_WORKSPACE when the parent is unset', () => {
    const legacy = tmpParent();
    expect(workspaceRoot({ KONTRA_WORKSPACE: legacy })).toBe(legacy);
  });

  it('switches and creates under the parent', () => {
    const parent = tmpParent();
    mkdirSync(path.join(parent, 'hello'), { recursive: true });
    writeFileSync(path.join(parent, '.current'), 'hello\n');
    const env = { KONTRA_WORKSPACES: parent };
    createWorkspace('two', { seed: false, use: true }, env);
    expect(useWorkspace('hello', env).current).toBe('hello');
    expect(describeWorkspaces(env).names).toEqual(['hello', 'two']);
  });
});

/*
 * A WORKSPACE NAME IS AN S3 BUCKET NAME (ADR 0051), and this is the only place that fact can be
 * enforced cheaply.
 *
 * These cases cannot be caught anywhere downstream on this machine: SeaweedFS backs a bucket with
 * a directory and accepts every one of them, so a name that is illegal on DigitalOcean Spaces
 * creates cleanly here and fails only in the cloud. The rule is the test.
 */
describe('workspace names are bucket names', () => {
  const ok = ['bugbounty', 'default', 'scraping', 'ab', 'a-b-c', 'x9', 'a'.repeat(60)];
  const no: Array<[string, string]> = [
    ['Client_A', 'uppercase and underscore — both illegal in a bucket name'],
    ['CAPS', 'uppercase alone'],
    ['has_underscore', 'underscore alone'],
    ['acme.2026', 'a dot breaks virtual-host-style TLS addressing'],
    ['trailing-', 'a trailing hyphen'],
    ['-leading', 'must start alphanumeric'],
    ['a', 'one character — ws-a is fine but the floor keeps it clear of the 3-char minimum'],
    ['a'.repeat(61), '61 + the 3-char `ws-` prefix exceeds 63'],
    ['..', 'path traversal, which the old rule also refused'],
  ];

  for (const name of ok) {
    it(`accepts ${JSON.stringify(name)}`, () => {
      expect(() => assertWorkspaceName(name)).not.toThrow();
    });
  }

  for (const [name, why] of no) {
    it(`refuses ${JSON.stringify(name)} — ${why}`, () => {
      expect(() => assertWorkspaceName(name)).toThrow();
    });
  }

  it('derives three addresses from the name alone, with no lookup', () => {
    const env = {
      KONTRA_DUCKLAKE_CATALOG:
        'postgres:dbname=kontra_ducklake host=postgres user=kontra password=kontra',
    };
    const a = workspaceAddress('bugbounty', env);
    expect(a.namespace).toBe('ws-bugbounty');
    expect(a.bucket).toBe('ws-bugbounty');
    // Host, user and password survive; ONLY dbname varies. Credentials stay in the environment.
    expect(a.catalog).toBe(
      'postgres:dbname=kontra_ducklake_ws_bugbounty host=postgres user=kontra password=kontra'
    );
  });

  it('has NO special case for `default` — the derivation is total', () => {
    // A grandfather clause inside an address function is a filter wearing an address's clothes.
    // `default` ending up with the old data is the MIGRATION's job, not this function's.
    const env = { KONTRA_DUCKLAKE_CATALOG: 'postgres:dbname=kontra_ducklake host=postgres' };
    expect(workspaceAddress('default', env).namespace).toBe('ws-default');
    expect(workspaceAddress('default', env).bucket).toBe('ws-default');
    expect(workspaceAddress('default', env).catalog).toContain('dbname=kontra_ducklake_ws_default');
  });

  it('turns hyphens into underscores for the Postgres database name, injectively', () => {
    const env = { KONTRA_DUCKLAKE_CATALOG: 'postgres:dbname=kontra_ducklake' };
    expect(workspaceAddress('client-a', env).catalog).toContain('dbname=kontra_ducklake_ws_client_a');
    // The substitution is only injective because underscores are forbidden in a workspace name.
    // If `client_a` were legal it would collide with `client-a` on one database — the isolation
    // failure arriving through the code meant to prevent it. Pin the rule that makes it safe.
    expect(() => assertWorkspaceName('client_a')).toThrow();
  });

  it('falls back to a file catalog when no Postgres connstring is configured', () => {
    // The appliance (ADR 0031 §1b) runs one process and a file catalog is enough.
    expect(workspaceAddress('solo', {}).catalog).toBe('ws-solo.ducklake');
  });

  it('refuses to address a workspace whose name it would not accept', () => {
    // The address function cannot be a second, looser gate — a name that reaches S3 must have
    // passed the same rule that created it.
    expect(() => workspaceAddress('Client_A', {})).toThrow();
  });

  it('costs the existing install nothing — every workspace on disk still passes', () => {
    // The tightening is only free at this exact moment. If this ever fails, a real workspace
    // would have to be renamed, which means moving its namespace, bucket and catalog too.
    for (const name of ['bugbounty', 'default', 'scraping']) {
      expect(() => assertWorkspaceName(name)).not.toThrow();
    }
  });
});
