import { mkdirSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from './codec/objectStore';
import { resolveLakeConfig } from './data/parquet';
import { activeLakeWorkspace, assertWorkspaceName, createWorkspace, describeWorkspaces, useWorkspace, workspaceAddress, workspaceRoot } from './workspaces';

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

/**
 * THE ADDRESS, CONSUMED (issue 08).
 *
 * `workspaceAddress` derived all three addresses and nothing read it — the gap the issue names.
 * These hold the wiring in `resolveLakeConfig`, and specifically the two properties that make it
 * safe to land before issue 10 moves any bytes.
 */
describe('resolveLakeConfig honours a workspace address', () => {
  const store = new ObjectStore({
    backing: new MemoryStore(),
    prefix: 'kontra',
    endpoint: 'http://seaweed:8333',
    bucket: 'kontra',
  });

  afterEach(() => {
    delete process.env.KONTRA_LAKE_WORKSPACE;
    delete process.env.KONTRA_DUCKLAKE_CATALOG;
    delete process.env.KONTRA_WORKSPACES;
  });

  // `.current` is now the primary answer (see `activeLakeWorkspace`), so a stray workspaces mount
  // would silently supply one to every case below. Cleared for each.
  beforeEach(() => {
    delete process.env.KONTRA_WORKSPACES;
  });

  /**
   * THE DEFAULT IS THE LEGACY ADDRESS, BYTE FOR BYTE. This install holds 152 datasets there, and
   * resolving to `ws-…` before the bytes have moved would answer every query from an empty
   * catalog — datasets not gone but unreachable, which reads exactly like the 2026-09-28 wipe.
   */
  it('changes nothing at all when no workspace is in force', () => {
    const cfg = resolveLakeConfig(store);
    expect(cfg.dataPath).toBe('s3://kontra/kontra/');
    expect(cfg.dataPath).not.toContain('ws-');
    expect(cfg.catalog).not.toContain('ws_');
  });

  it('gives a workspace its own bucket and its own catalog database', () => {
    process.env.KONTRA_DUCKLAKE_CATALOG = 'postgres:dbname=kontra_ducklake host=postgres';
    process.env.KONTRA_LAKE_WORKSPACE = 'bugbounty';

    const cfg = resolveLakeConfig(store);
    expect(cfg.dataPath).toBe('s3://ws-bugbounty/');
    expect(cfg.catalog).toContain('dbname=kontra_ducklake_ws_bugbounty');
    // The bucket moves WITH the catalog, never apart from it: two catalogs sharing one bucket
    // makes `ducklake_delete_orphaned_files` a cross-workspace deletion machine.
    expect(cfg.dataPath).toContain('ws-bugbounty');
  });

  /** TOTAL, with no exception for `default` — a special case inside an address function is a
   *  filter wearing an address's clothes. */
  it('derives `default` the same way as any other name', () => {
    process.env.KONTRA_DUCKLAKE_CATALOG = 'postgres:dbname=kontra_ducklake host=postgres';
    process.env.KONTRA_LAKE_WORKSPACE = 'default';
    const cfg = resolveLakeConfig(store);
    expect(cfg.dataPath).toBe('s3://ws-default/');
    expect(cfg.catalog).toContain('dbname=kontra_ducklake_ws_default');
  });

  /** Two workspaces cannot land on one address — the whole point, asserted rather than assumed. */
  it('gives two workspaces disjoint addresses', () => {
    process.env.KONTRA_DUCKLAKE_CATALOG = 'postgres:dbname=kontra_ducklake host=postgres';
    process.env.KONTRA_LAKE_WORKSPACE = 'bugbounty';
    const a = resolveLakeConfig(store);
    process.env.KONTRA_LAKE_WORKSPACE = 'scraping';
    const b = resolveLakeConfig(store);

    expect(a.dataPath).not.toBe(b.dataPath);
    expect(a.catalog).not.toBe(b.catalog);
  });

  /** An explicit override still wins — it is how a test points at a local directory. */
  it('lets an explicit override beat the derived address', () => {
    process.env.KONTRA_LAKE_WORKSPACE = 'bugbounty';
    const cfg = resolveLakeConfig(store, { dataPath: '/tmp/x/', catalog: '/tmp/cat.ducklake' });
    expect(cfg.dataPath).toBe('/tmp/x/');
    expect(cfg.catalog).toBe('/tmp/cat.ducklake');
  });

  /** A name that is not a legal bucket name is refused HERE, not at the first write. */
  it('refuses an illegal workspace name rather than deriving a broken address', () => {
    process.env.KONTRA_LAKE_WORKSPACE = 'Client_A';
    expect(() => resolveLakeConfig(store)).toThrow();
  });
});

/**
 * THE NAME COLLISION THAT WOULD HAVE BROKEN EVERY LAKE RESOLUTION.
 *
 * `KONTRA_WORKSPACE` already exists and means the code ROOT DIRECTORY — `cli/workspace.go` reads it,
 * and `workspaceRoot` above falls back to it. The lake address switch was briefly spelled with that
 * name, which would have handed `workspaceAddress` a filesystem path on any install still setting
 * the legacy variable, and a path is not a legal workspace name. Two variables, two meanings, and
 * this is what keeps them apart.
 */
describe('the lake workspace switch does not collide with the legacy code-root variable', () => {
  const store = new ObjectStore({
    backing: new MemoryStore(),
    prefix: 'kontra',
    endpoint: 'http://seaweed:8333',
    bucket: 'kontra',
  });

  afterEach(() => {
    delete process.env.KONTRA_WORKSPACE;
    delete process.env.KONTRA_LAKE_WORKSPACE;
  });

  it('ignores KONTRA_WORKSPACE, which holds a directory and not a name', () => {
    process.env.KONTRA_WORKSPACE = '/srv/kontra/workspaces';
    // Would throw if this were read as a workspace name.
    const cfg = resolveLakeConfig(store);
    expect(cfg.dataPath).toBe('s3://kontra/kontra/');
  });

  it('still reads the legacy variable for what it actually means', () => {
    expect(workspaceRoot({ KONTRA_WORKSPACE: '/srv/legacy' })).toBe('/srv/legacy');
  });
});

/**
 * AND THE LOCAL LAKE TOO — an appliance runs a FILE catalog, so two workspaces get two catalogs.
 * Two catalogs over one data directory is the same cross-workspace deletion hazard as two over one
 * bucket: `ducklake_delete_orphaned_files` does not care whether the store is S3 or a folder.
 */
describe('a workspace address scopes the LOCAL data path as well as the bucket', () => {
  const local = new ObjectStore({ backing: new MemoryStore(), prefix: '' });

  afterEach(() => {
    delete process.env.KONTRA_LAKE_WORKSPACE;
    delete process.env.KONTRA_DUCKLAKE_DATA_PATH;
  });

  it('puts each workspace under its own directory', () => {
    process.env.KONTRA_DUCKLAKE_DATA_PATH = '/var/lib/kontra/lake';
    process.env.KONTRA_LAKE_WORKSPACE = 'bugbounty';
    const a = resolveLakeConfig(local);
    process.env.KONTRA_LAKE_WORKSPACE = 'scraping';
    const b = resolveLakeConfig(local);

    expect(a.dataPath).toBe('/var/lib/kontra/lake/ws-bugbounty/');
    expect(b.dataPath).toBe('/var/lib/kontra/lake/ws-scraping/');
    expect(a.catalog).not.toBe(b.catalog);
  });

  it('leaves the local path exactly as it was when no workspace is in force', () => {
    process.env.KONTRA_DUCKLAKE_DATA_PATH = '/var/lib/kontra/lake';
    expect(resolveLakeConfig(local).dataPath).toBe('/var/lib/kontra/lake');
  });
});


/**
 * THE LAKE ADDRESS FOLLOWS `.current`, AND THE VARIABLE IS ONLY AN OVERRIDE.
 *
 * Before this, `KONTRA_LAKE_WORKSPACE` was the only input, so an install could serve one
 * workspace's code (`.current`) against another workspace's lake (the variable) with both answers
 * individually valid and nothing comparing them. That is the drift `workspaceAddress` is a
 * derivation to avoid, reintroduced one layer up.
 */
describe('which workspace the lake belongs to', () => {
  const store = new ObjectStore({
    backing: new MemoryStore(),
    prefix: 'kontra',
    endpoint: 'http://seaweed:8333',
    bucket: 'kontra',
  });

  function parentWith(current: string): string {
    const dir = mkdirSync(path.join(os.tmpdir(), `kontra-ws-${Date.now()}-${Math.random()}`), {
      recursive: true,
    }) as unknown as string;
    const root = dir ?? '';
    mkdirSync(path.join(root, current), { recursive: true });
    writeFileSync(path.join(root, '.current'), `${current}\n`, 'utf8');
    return root;
  }

  beforeEach(() => {
    delete process.env.KONTRA_LAKE_WORKSPACE;
    delete process.env.KONTRA_WORKSPACES;
    delete process.env.KONTRA_DUCKLAKE_CATALOG;
  });
  afterEach(() => {
    delete process.env.KONTRA_LAKE_WORKSPACE;
    delete process.env.KONTRA_WORKSPACES;
    delete process.env.KONTRA_DUCKLAKE_CATALOG;
  });

  it('reads `.current` when no variable is set', () => {
    process.env.KONTRA_WORKSPACES = parentWith('bugbounty');
    expect(activeLakeWorkspace()).toBe('bugbounty');
  });

  it('lets the variable override `.current`', () => {
    process.env.KONTRA_WORKSPACES = parentWith('bugbounty');
    process.env.KONTRA_LAKE_WORKSPACE = 'scraping';
    expect(activeLakeWorkspace()).toBe('scraping');
  });

  it('is empty — the legacy address — when neither says', () => {
    expect(activeLakeWorkspace()).toBe('');
    expect(resolveLakeConfig(store).dataPath).toBe('s3://kontra/kontra/');
  });

  /** The whole point: switching the active workspace moves the lake with it. */
  it('resolves the lake to whatever `.current` names', () => {
    process.env.KONTRA_DUCKLAKE_CATALOG = 'postgres:dbname=kontra_ducklake host=postgres';
    process.env.KONTRA_WORKSPACES = parentWith('bugbounty');
    const a = resolveLakeConfig(store);
    expect(a.dataPath).toBe('s3://ws-bugbounty/');
    expect(a.catalog).toContain('dbname=kontra_ducklake_ws_bugbounty');

    process.env.KONTRA_WORKSPACES = parentWith('scraping');
    const b = resolveLakeConfig(store);
    expect(b.dataPath).toBe('s3://ws-scraping/');
    expect(b.catalog).toContain('dbname=kontra_ducklake_ws_scraping');
  });

  /** A parent with no `.current` is not a workspace layout — legacy address, not a guess. */
  it('falls back to the legacy address when `.current` is absent', () => {
    const dir = path.join(os.tmpdir(), `kontra-ws-empty-${Date.now()}`);
    mkdirSync(dir, { recursive: true });
    process.env.KONTRA_WORKSPACES = dir;
    expect(activeLakeWorkspace()).toBe('');
    expect(resolveLakeConfig(store).dataPath).toBe('s3://kontra/kontra/');
  });
});
