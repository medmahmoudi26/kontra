/**
 * The retention PREVIEW route (ADR 0029 §3, §5): `GET /api/datasets/retention/preview` runs the exact
 * sweep the schedule runs, with `dryRun` forced on, so an operator sees what WOULD age out before
 * anything does. This pins the wiring — the route threads the same stores the listing uses, forces the
 * dry run, and deletes nothing.
 */

import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from './codec/objectStore';
import { Repo } from './db/repo';
import { datasetStateKey } from './data/datasets';
import { DatasetRecordStore } from './data/datasetRecords';
import { MATERIALIZATION_SCHEMA_VERSION } from './data/materialization';
import { MaterializationStore } from './data/materializationStore';
import { resetLakeConnections, safeName, writeDatasetParquet, type LakeConfig } from './data/parquet';
import { SummaryStore } from './data/summaries';
import { buildServer } from './server';
import type { FastifyInstance } from 'fastify';

interface Ctx {
  app: FastifyInstance;
  materialization: MaterializationStore;
}

const RUN_STARTED = Date.UTC(2026, 6, 1, 9, 0, 0);

async function build(): Promise<Ctx> {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-retroute-'));
  const dataPath = join(dir, 'data');
  const blobs = join(dir, 'blobs');
  mkdirSync(dataPath, { recursive: true });
  mkdirSync(blobs, { recursive: true });
  const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
  const lake: Partial<LakeConfig> = { catalog: join(dir, 'cat.ducklake'), dataPath: `${dataPath}/`, s3: false, blobBase: `${blobs}/` };
  const materialization = new MaterializationStore({ url: ':memory:' });
  const records = new DatasetRecordStore({ url: ':memory:' });
  const summaries = new SummaryStore({ url: ':memory:' });

  // One sealed, untagged Run's output — the shape a preview flags as collectable.
  const body = JSON.stringify([{ url: 'a', ok: true }]);
  writeFileSync(join(blobs, 'u1.json'), body);
  const ref = { $ref: { key: 'u1.json', sha256: createHash('sha256').update(body).digest('hex'), size: body.length } };
  const src = join(mkdtempSync(join(tmpdir(), 'kontra-src-')), 'blob.json');
  writeFileSync(src, JSON.stringify({ results: [ref], failures: [] }));
  await writeDatasetParquet(
    store,
    { sha256: 'unused', actor: 'nscheck', version: '0.1.0', runId: 'old-run', node: 'n1', runStartedAt: RUN_STARTED },
    { ...lake, sourceUri: src }
  );
  const key = { runId: 'old-run', actor: 'nscheck', version: '0.1.0', node: 'n1', schemaVersion: MATERIALIZATION_SCHEMA_VERSION };
  await materialization.declare(key, RUN_STARTED);
  await materialization.complete(key, { rows: 1, bytes: body.length, snapshotId: 1, tbl: safeName('nscheck') });
  await store.put(datasetStateKey('nscheck'), Buffer.from(JSON.stringify({ state: 'sealed' }), 'utf8'));

  const app = buildServer({ repo: new Repo(':memory:'), store, lake, materialization, records, summaries, webRoot: '' });
  return { app, materialization };
}

describe('GET /api/datasets/retention/preview', () => {
  let ctx: Ctx;
  beforeEach(async () => {
    resetLakeConnections();
    ctx = await build();
  });
  afterEach(async () => {
    await ctx.app.close();
  });

  it('reports what would be collected without deleting anything', async () => {
    // ttlMs=0&graceMs=0 makes any sealed, untagged Dataset past its (real) last write a candidate, so
    // the seeded Run shows up without having to age the clock.
    const res = await ctx.app.inject({ method: 'GET', url: '/api/datasets/retention/preview?ttlMs=0&graceMs=0' });
    expect(res.statusCode).toBe(200);
    const report = res.json();
    expect(report.dryRun).toBe(true);
    expect(report.collected.map((c: { runId: string }) => c.runId)).toContain('old-run');
    expect(report.purgedRuns).toBe(0);
    // The preview deleted nothing — the ledger record is still there.
    expect(await ctx.materialization.listForRun('old-run')).toHaveLength(1);
  });

  it('keeps everything under the real TTL, which no query parameter can lower into deleting', async () => {
    // With the default (24h) TTL and a run written seconds ago, nothing is collectable — and the route
    // has no mode that deletes, so even a collectable preview never ages anything out.
    const res = await ctx.app.inject({ method: 'GET', url: '/api/datasets/retention/preview' });
    expect(res.statusCode).toBe(200);
    expect(res.json().collected).toHaveLength(0);
    expect(await ctx.materialization.listForRun('old-run')).toHaveLength(1);
  });
});
