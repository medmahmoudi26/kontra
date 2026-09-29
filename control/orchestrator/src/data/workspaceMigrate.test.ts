/**
 * Moving a lake between workspace addresses, against TWO REAL DuckLakes.
 *
 * The 2026-09-28 wipe is reproduced literally: a partition is materialized normally and then its
 * parquet is deleted from under the catalog, which is exactly the state the object-store wipe left
 * 101 catalog entries in. That is the only honest way to test this — a mocked "dead" flag would
 * test the mock, and the entire difficulty of issue 09 is that the catalog cannot tell you.
 */

import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import { listDatasets } from './datasets';
import { resetLakeConnections, writeDatasetParquet, type LakeConfig } from './parquet';
import { migrateWorkspaceLake } from './workspaceMigrate';

interface Ctx {
  store: ObjectStore;
  src: Partial<LakeConfig>;
  dst: Partial<LakeConfig>;
  /** Root of the SOURCE lake's data files — where a partition gets deleted to fake the wipe. */
  srcData: string;
  blobs: string;
}

function twoLakes(): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-migrate-'));
  const srcData = join(dir, 'src-data');
  const dstData = join(dir, 'dst-data');
  const blobs = join(dir, 'blobs');
  for (const d of [srcData, dstData, blobs]) mkdirSync(d, { recursive: true });
  const store = new ObjectStore({
    backing: new MemoryStore(),
    prefix: '',
    endpoint: 'http://localhost:8333',
  });
  return {
    store,
    srcData,
    blobs,
    src: {
      catalog: join(dir, 'src.ducklake'),
      dataPath: `${srcData}/`,
      s3: false,
      blobBase: `${blobs}/`,
    },
    dst: {
      catalog: join(dir, 'dst.ducklake'),
      dataPath: `${dstData}/`,
      s3: false,
      blobBase: `${blobs}/`,
    },
  };
}

let seq = 0;

/** Materialize one dispatch through the real claim-check writer. */
async function write(
  ctx: Ctx,
  sel: { actor: string; version: string; runId: string; node: string; runStartedAt: number },
  rows: unknown[]
): Promise<void> {
  const name = `u${(seq += 1)}.json`;
  const body = JSON.stringify(rows);
  writeFileSync(join(ctx.blobs, name), body);
  const ref = {
    $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length },
  };
  const manifest = JSON.stringify({ results: [ref], failures: [] });
  const src = join(mkdtempSync(join(tmpdir(), 'kontra-src-')), 'blob.json');
  writeFileSync(src, manifest);
  await writeDatasetParquet(
    ctx.store,
    { sha256: createHash('sha256').update(manifest).digest('hex'), ...sel },
    { ...ctx.src, sourceUri: src }
  );
}

/**
 * Delete the parquet under one dispatch, leaving its catalog entry standing. THE WIPE, exactly:
 * the catalog still lists the partition and still reports its row count from statistics.
 */
function wipePartition(ctx: Ctx, actor: string, version: string, dt: string): void {
  const dir = join(ctx.srcData, 'output', actor, `version=${version}`, `dt=${dt}`);
  rmSync(dir, { recursive: true, force: true });
}

const T1 = Date.UTC(2026, 8, 20, 10, 0, 0);
const T2 = Date.UTC(2026, 8, 21, 10, 0, 0);
const T3 = Date.UTC(2026, 8, 22, 10, 0, 0);

describe('migrating a lake to a workspace address', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = twoLakes();
  });

  it('carries every readable partition, and the rows survive the move', async () => {
    await write(ctx, { actor: 'scan', version: '1.0.0', runId: 'r1', node: 'n1', runStartedAt: T1 }, [
      { host: 'a.example', code: 200 },
      { host: 'b.example', code: 404 },
    ]);
    await write(ctx, { actor: 'scan', version: '1.0.0', runId: 'r2', node: 'n1', runStartedAt: T2 }, [
      { host: 'c.example', code: 500 },
    ]);

    const report = await migrateWorkspaceLake(ctx.store, { from: ctx.src, to: ctx.dst, apply: true });

    expect(report.applied).toBe(true);
    expect(report.dead).toBe(0);
    expect(report.copied).toBe(2);
    expect(report.rowsCopied).toBe(3);

    const after = await listDatasets(ctx.store, {}, ctx.dst);
    expect(after.map((d) => d.name)).toContain('scan');
    expect(after.reduce((n, d) => n + d.rows, 0)).toBe(3);
  });

  it('leaves an orphaned partition behind and still migrates the rest of its table', async () => {
    await write(ctx, { actor: 'scan', version: '1.0.0', runId: 'r1', node: 'n1', runStartedAt: T1 }, [
      { host: 'a.example', code: 200 },
    ]);
    await write(ctx, { actor: 'scan', version: '1.0.0', runId: 'r2', node: 'n1', runStartedAt: T2 }, [
      { host: 'b.example', code: 404 },
    ]);
    await write(ctx, { actor: 'scan', version: '1.0.0', runId: 'r3', node: 'n1', runStartedAt: T3 }, [
      { host: 'c.example', code: 500 },
    ]);

    const before = await listDatasets(ctx.store, {}, ctx.src);
    const victim = before.find((d) => d.dt !== undefined)!;
    wipePartition(ctx, 'scan', victim.version!, victim.dt!);
    resetLakeConnections();

    // The catalog has not noticed: it still lists the partition and still claims its rows.
    const stillListed = await listDatasets(ctx.store, {}, ctx.src);
    expect(stillListed.length).toBe(before.length);

    const report = await migrateWorkspaceLake(ctx.store, { from: ctx.src, to: ctx.dst, apply: true });

    expect(report.dead).toBe(1);
    expect(report.copied).toBe(2);
    expect(report.rowsCopied).toBe(2);
    expect(report.rowsAbandoned).toBe(victim.rows);

    const skipped = report.entries.find((e) => e.state === 'dead')!;
    expect(skipped.dt).toBe(victim.dt);
    expect(skipped.error).toBeTruthy();

    // The destination holds the survivors and NOT the orphan — the purge, without a DELETE.
    const after = await listDatasets(ctx.store, {}, ctx.dst);
    expect(after.reduce((n, d) => n + d.rows, 0)).toBe(2);
    expect(after.some((d) => d.dt === victim.dt)).toBe(false);
  });

  it('a dry run writes nothing at all', async () => {
    await write(ctx, { actor: 'scan', version: '1.0.0', runId: 'r1', node: 'n1', runStartedAt: T1 }, [
      { host: 'a.example', code: 200 },
    ]);

    const report = await migrateWorkspaceLake(ctx.store, { from: ctx.src, to: ctx.dst });

    expect(report.applied).toBe(false);
    expect(report.live).toBe(1);
    expect(report.copied).toBe(0);
    expect(report.rowsCopied).toBe(0);

    const after = await listDatasets(ctx.store, {}, ctx.dst);
    expect(after).toEqual([]);
  });

  it('re-running does not duplicate rows', async () => {
    await write(ctx, { actor: 'scan', version: '1.0.0', runId: 'r1', node: 'n1', runStartedAt: T1 }, [
      { host: 'a.example', code: 200 },
      { host: 'b.example', code: 404 },
    ]);

    await migrateWorkspaceLake(ctx.store, { from: ctx.src, to: ctx.dst, apply: true });
    const second = await migrateWorkspaceLake(ctx.store, { from: ctx.src, to: ctx.dst, apply: true });

    expect(second.rowsCopied).toBe(0);
    expect(second.entries[0]?.error).toMatch(/already present/);

    const after = await listDatasets(ctx.store, {}, ctx.dst);
    expect(after.reduce((n, d) => n + d.rows, 0)).toBe(2);
  });

  it('refuses to copy a lake onto itself', async () => {
    await expect(
      migrateWorkspaceLake(ctx.store, { from: ctx.src, to: ctx.src, apply: true })
    ).rejects.toThrow(/same address/);
  });

  it('does not touch the source lake', async () => {
    await write(ctx, { actor: 'scan', version: '1.0.0', runId: 'r1', node: 'n1', runStartedAt: T1 }, [
      { host: 'a.example', code: 200 },
    ]);
    const before = await listDatasets(ctx.store, {}, ctx.src);
    const filesBefore = readdirSync(join(ctx.srcData, 'output'), { recursive: true }).length;

    await migrateWorkspaceLake(ctx.store, { from: ctx.src, to: ctx.dst, apply: true });

    const after = await listDatasets(ctx.store, {}, ctx.src);
    expect(after).toEqual(before);
    expect(readdirSync(join(ctx.srcData, 'output'), { recursive: true }).length).toBe(filesBefore);
  });
});
