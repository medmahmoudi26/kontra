import { describe, expect, it } from 'vitest';

import { Repo } from './repo';

describe('Repo actor digest (ADR 0011)', () => {
  const base = { name: 'echo', version: '0.1.0', schemaVersion: 'kontra.actor.v1', operations: [] };

  it('upsertActor persists an optional image digest', () => {
    const repo = new Repo(':memory:');
    repo.upsertActor({ key: 'echo@0.1.0', ...base, digest: 'sha256:abc' });
    expect(repo.getActor('echo@0.1.0')?.digest).toBe('sha256:abc');
    // omitted -> undefined (pinning off for a hand-fed catalog)
    repo.upsertActor({ key: 'echo@0.2.0', ...base, version: '0.2.0' });
    expect(repo.getActor('echo@0.2.0')?.digest).toBeUndefined();
  });

  it('setActorDigest preserves operations; upsertActor preserves a worker digest', () => {
    const repo = new Repo(':memory:');
    repo.upsertActor({ key: 'echo@0.1.0', ...base, operations: [{ name: 'run', input: { type: 'object' } }] });
    repo.setActorDigest({ key: 'echo@0.1.0', name: 'echo', version: '0.1.0', digest: 'sha256:w' });
    expect(repo.getActor('echo@0.1.0')?.digest).toBe('sha256:w');
    expect(repo.getActor('echo@0.1.0')?.operations).toHaveLength(1); // not clobbered
    // a later design-tool re-upload (no digest) keeps the worker-registered digest
    repo.upsertActor({ key: 'echo@0.1.0', ...base });
    expect(repo.getActor('echo@0.1.0')?.digest).toBe('sha256:w');
  });

  it('setActorDigest creates a minimal record when the actor is new', () => {
    const repo = new Repo(':memory:');
    repo.setActorDigest({ key: 'new@1.0.0', name: 'new', version: '1.0.0', digest: 'sha256:x' });
    expect(repo.getActor('new@1.0.0')).toMatchObject({ digest: 'sha256:x', operations: [] });
  });
});

describe('where an actor came from, and what its Methods are for', () => {
  const base = { name: 'nscheck', version: '0.1.0', schemaVersion: 'kontra.actor.v1', operations: [] };

  it('remembers the directory the worker loaded it from', () => {
    // Nothing linked a registered actor back to its code: `.kontra/actors/` is where an operator's
    // own actors go and is usually empty, so a catalog of twenty-three actors offered no way to
    // reach any of their source.
    const repo = new Repo(':memory:');
    repo.upsertActor({ key: 'nscheck@0.1.0', ...base, source: '/repo/examples/go/nscheck' });
    expect(repo.getActor('nscheck@0.1.0')?.source).toBe('/repo/examples/go/nscheck');
  });

  it('leaves it ABSENT for a hand-fed entry rather than inventing a path', () => {
    // Nothing loaded a hand-registered actor from anywhere, so a path here would be a link to a
    // directory that does not exist.
    const repo = new Repo(':memory:');
    repo.upsertActor({ key: 'nscheck@0.1.0', ...base });
    const rec = repo.getActor('nscheck@0.1.0');
    expect(rec?.source).toBeUndefined();
    expect('source' in (rec ?? {})).toBe(false);
  });

  it('MIGRATES a database that already has the table', async () => {
    // THE FAILURE THIS EXISTS FOR. `CREATE TABLE IF NOT EXISTS` is a no-op against a database that
    // HAS the table, so a column added to SCHEMA reaches a fresh install and never an existing
    // one — and the symptom is a `SELECT *` returning rows without it, which reads as "no actor
    // has a source" rather than as a migration nobody ran. Every real installation is the second
    // case; only a test with `:memory:` is the first, which is exactly why this one is not.
    const { mkdtempSync, rmSync } = await import('node:fs');
    const { tmpdir } = await import('node:os');
    const { join } = await import('node:path');
    // `createRequire`, not `import('node:sqlite')`, for the reason repo.ts documents at its own
    // import: the bundler vitest runs under does not list it as a builtin and tries to resolve it
    // as a file. A runtime require is a genuine load the bundler leaves alone.
    const { createRequire } = await import('node:module');
    const { DatabaseSync } = createRequire(__filename)('node:sqlite') as typeof import('node:sqlite');
    const dir = mkdtempSync(join(tmpdir(), 'kontra-repo-'));
    const file = join(dir, 'orchestrator.db');
    try {
      // The table in its OLD shape, created by hand — so this exercises the real migration and not
      // one applied to a table that already had the column.
      const old = new DatabaseSync(file);
      old.exec(`CREATE TABLE actors (
        key TEXT PRIMARY KEY, name TEXT NOT NULL, version TEXT NOT NULL,
        schema_version TEXT NOT NULL, operations TEXT NOT NULL, digest TEXT,
        saved_at INTEGER NOT NULL);`);
      old
        .prepare(`INSERT INTO actors VALUES ('old@1.0.0','old','1.0.0','kontra.actor.v1','[]',NULL,1)`)
        .run();
      old.close();

      const repo = new Repo(file);
      // The pre-existing row survives and simply has no source.
      expect(repo.getActor('old@1.0.0')?.source).toBeUndefined();
      // …and the column is really there, so a re-registration records one.
      repo.upsertActor({
        key: 'old@1.0.0',
        name: 'old',
        version: '1.0.0',
        schemaVersion: 'kontra.actor.v1',
        operations: [],
        source: '/opt/kontra/actor/old',
      });
      expect(repo.getActor('old@1.0.0')?.source).toBe('/opt/kontra/actor/old');

      // Idempotent: opening it again must not fail on a duplicate column.
      expect(() => new Repo(file)).not.toThrow();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('MIGRATES a database whose actors table predates the cross-version finding', async () => {
    // Same failure as the `source` migration above, and worth its own case because the symptom is
    // even quieter: a `SELECT *` without the column reads as "no version here has ever broken a
    // caller", which is the exact claim this feature exists to stop anybody making by accident.
    const { mkdtempSync, rmSync } = await import('node:fs');
    const { tmpdir } = await import('node:os');
    const { join } = await import('node:path');
    const { createRequire } = await import('node:module');
    const { DatabaseSync } = createRequire(__filename)('node:sqlite') as typeof import('node:sqlite');
    const dir = mkdtempSync(join(tmpdir(), 'kontra-repo-'));
    const file = join(dir, 'orchestrator.db');
    try {
      const old = new DatabaseSync(file);
      old.exec(`CREATE TABLE actors (
        key TEXT PRIMARY KEY, name TEXT NOT NULL, version TEXT NOT NULL,
        schema_version TEXT NOT NULL, operations TEXT NOT NULL, digest TEXT, source TEXT,
        saved_at INTEGER NOT NULL);`);
      old.close();

      const repo = new Repo(file);
      repo.upsertActor({
        key: 'probe@0.2.0',
        name: 'probe',
        version: '0.2.0',
        schemaVersion: 'kontra.actor.v1',
        operations: [],
        incompatibilities: [
          {
            method: 'fetch',
            field: 'output',
            rule: 'FORWARD',
            previous: '0.1.0',
            detail: 'field "status" removed — a caller reading it gets nothing',
          },
        ],
      });
      expect(repo.getActor('probe@0.2.0')?.incompatibilities?.[0]?.method).toBe('fetch');
      expect(() => new Repo(file)).not.toThrow();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('keeps a cross-version finding across a restart, and leaves it ABSENT when there is none', async () => {
    // THE POINT OF STORING IT. The finding is made once, at registration, by comparing against a
    // version that may be deleted afterwards; recomputing it on read would mean the Actors page
    // stops saying a version broke a caller as soon as the catalog changes shape around it. So the
    // check is that it survives the process, not just the transaction.
    const { mkdtempSync, rmSync } = await import('node:fs');
    const { tmpdir } = await import('node:os');
    const { join } = await import('node:path');
    const dir = mkdtempSync(join(tmpdir(), 'kontra-repo-'));
    const file = join(dir, 'orchestrator.db');
    try {
      const first = new Repo(file);
      first.upsertActor({
        key: 'probe@0.1.0',
        name: 'probe',
        version: '0.1.0',
        schemaVersion: 'kontra.actor.v1',
        operations: [],
      });
      first.upsertActor({
        key: 'probe@0.2.0',
        name: 'probe',
        version: '0.2.0',
        schemaVersion: 'kontra.actor.v1',
        operations: [],
        incompatibilities: [
          {
            method: 'fetch',
            field: 'input',
            rule: 'BACKWARD',
            previous: '0.1.0',
            detail: 'required field "timeout" added',
          },
        ],
      });

      // A second Repo over the same file is what a restart is.
      const reopened = new Repo(file);
      const found = reopened.getActor('probe@0.2.0')?.incompatibilities;
      expect(found).toHaveLength(1);
      expect(found?.[0]).toMatchObject({ method: 'fetch', rule: 'BACKWARD', previous: '0.1.0' });

      // …and the version with nothing reported has no key at all. An empty array here would read
      // as "compared, and compatible", which is a claim nothing made about 0.1.0: it is the first
      // version and was compared against nothing.
      const clean = reopened.getActor('probe@0.1.0') ?? {};
      expect('incompatibilities' in clean).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('carries a Method description through the catalog', () => {
    // The other half of what a card needs: what a Method IS FOR, from the author's own docstring.
    const repo = new Repo(':memory:');
    repo.upsertActor({
      key: 'nscheck@0.1.0',
      ...base,
      operations: [{ name: 'ask', description: 'Ask one nameserver whether it serves the zone' }],
    });
    expect(repo.getActor('nscheck@0.1.0')?.operations[0]?.description).toBe(
      'Ask one nameserver whether it serves the zone'
    );
  });
});

describe('what a workflow registered about itself', () => {
  it('round-trips the four descriptor fields', () => {
    const repo = new Repo(':memory:');
    repo.upsertWorkflow({
      name: 'NsCheck',
      description: 'Check every domain delegation.',
      input: { type: 'object', properties: { dataset: { type: 'string' } } },
      output: { type: 'object', additionalProperties: true },
    });
    const rec = repo.getWorkflow('NsCheck');
    expect(rec).toMatchObject({
      name: 'NsCheck',
      description: 'Check every domain delegation.',
      output: { type: 'object', additionalProperties: true },
    });
    expect(rec?.input).toEqual({ type: 'object', properties: { dataset: { type: 'string' } } });
    expect(rec?.savedAt).toBeGreaterThan(0);
  });

  it('leaves what the author declared nothing about ABSENT, not empty', () => {
    // The distinction the Workflows page turns on. A row that answered `input: {}` for an
    // unannotated run method would say "takes an object with no fields" about a workflow whose
    // author never wrote a type down — and the two draw differently on purpose.
    const repo = new Repo(':memory:');
    repo.upsertWorkflow({ name: 'Untyped' });
    const rec = repo.getWorkflow('Untyped') ?? {};
    expect('description' in rec).toBe(false);
    expect('input' in rec).toBe(false);
    expect('output' in rec).toBe(false);
  });

  it('lets a re-served workflow replace its own descriptor', () => {
    // There is no version to bump — a workflow is the operator's own file, served from the
    // operator's own process — so the descriptor that must win is the one the RUNNING worker just
    // derived. Keeping the first would leave the catalog describing code nobody is executing.
    const repo = new Repo(':memory:');
    repo.upsertWorkflow({ name: 'Ping', input: { type: 'object' } });
    repo.upsertWorkflow({ name: 'Ping' });
    expect(repo.listWorkflows()).toHaveLength(1);
    expect('input' in (repo.getWorkflow('Ping') ?? {})).toBe(false);
  });

  it('replaces a good descriptor with a broken-file state, then recovers it', () => {
    // Instrument-panel slice 03: a broken file is a STATE, not a silence. A watch-mode serve that
    // fails to re-import posts an `error` with no schema, so the store must drop the last good form
    // (input/output → NULL) and hold the error. Fixing the file re-posts the schema with no error,
    // and the store restores the form — the round-trip the panel shows without restarting serve.
    const repo = new Repo(':memory:');
    repo.upsertWorkflow({
      name: 'NsCheck',
      queue: 'wf-nscheck-abc',
      input: { type: 'object', properties: { dataset: { type: 'string' } } },
    });

    // The save that broke: error + queue, no schema.
    repo.upsertWorkflow({ name: 'NsCheck', queue: 'wf-nscheck-abc', error: 'no longer imports: SyntaxError' });
    const broken = repo.getWorkflow('NsCheck') ?? {};
    expect(broken.error).toBe('no longer imports: SyntaxError');
    expect(broken.queue).toBe('wf-nscheck-abc'); // the poller signal survives the break
    expect('input' in broken).toBe(false); // the stale form dropped

    // The save that fixed it: schema back, error gone.
    repo.upsertWorkflow({
      name: 'NsCheck',
      queue: 'wf-nscheck-abc',
      input: { type: 'object', properties: { dataset: { type: 'string' } } },
    });
    const fixed = repo.getWorkflow('NsCheck') ?? {};
    expect('error' in fixed).toBe(false);
    expect(fixed.input).toEqual({ type: 'object', properties: { dataset: { type: 'string' } } });
  });

  it('leaves the error absent for a healthy workflow, and drops an empty one', () => {
    // NULL and '' both mean "the file imports"; only a real message is a broken state to draw.
    const repo = new Repo(':memory:');
    repo.upsertWorkflow({ name: 'Ping' });
    expect('error' in (repo.getWorkflow('Ping') ?? {})).toBe(false);
    repo.upsertWorkflow({ name: 'Ping', error: '' });
    expect('error' in (repo.getWorkflow('Ping') ?? {})).toBe(false);
  });

  it('keeps two workflows out of one file apart', () => {
    // A file can declare several `@workflow.defn` classes and each registers on its own; the type
    // is what Temporal routes on, so the type is the key.
    const repo = new Repo(':memory:');
    repo.upsertWorkflow({ name: 'First', description: 'the first' });
    repo.upsertWorkflow({ name: 'Second', description: 'the second' });
    expect(repo.listWorkflows().map((w) => w.name)).toEqual(['First', 'Second']);
  });
});
