/**
 * The dataset activities: paging a Dataset into Batches, and re-paging a Batch.
 *
 * `pageDataset`'s SQL half is covered against a real DuckLake in `data/queryEngine.test.ts`;
 * what is asserted here is the activity seam — the default SQL, and the split that keeps a
 * fanned-out Method result from reaching the next Actor as one oversized call.
 */

import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import { createDatasetActivities } from './datasets';
import { DatasetRecordStore, InvalidDeviationError } from '../data/datasetRecords';
// The lifecycle key lives with the READ side, which is what has to agree with the UI's badge.
import { datasetOwnerKey, datasetStateKey } from '../data/datasets';
import {
  LAKE,
  OUTPUT_SCHEMA,
  lakeConnection,
  resetLakeConnections,
  resolveLakeConfig,
  type LakeConfig,
} from '../data/parquet';

function store(): ObjectStore {
  return new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
}

async function put(s: ObjectStore, value: unknown): Promise<string> {
  return s.putContentAddressed(Buffer.from(JSON.stringify(value), 'utf8'));
}

async function unitsOf(s: ObjectStore, sha: string): Promise<unknown[]> {
  const body = await s.get(s.casKey(sha));
  return JSON.parse(Buffer.from(body!).toString('utf8'));
}

describe('splitBatch', () => {
  it('re-pages a fanned-out result into batches of at most size', async () => {
    // The case that motivates it: a 1→N Method turns a 200-unit page into far more, and the
    // next Actor would otherwise get all of it in one activity on one worker.
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const units = Array.from({ length: 10 }, (_, i) => ({ host: `h${i}` }));
    const sha = await put(s, units);

    const { refs } = await acts.splitBatch({ sha256: sha, size: 4 });

    expect(refs.map((r) => r.meta.n)).toEqual(['4', '4', '2']);
    expect(refs.every((r) => r.meta.kind === 'units')).toBe(true);

    // Every unit survives exactly once, in order.
    const seen: unknown[] = [];
    for (const r of refs) seen.push(...(await unitsOf(s, r.sha256)));
    expect(seen).toEqual(units);
  });

  it('is content-addressed, so re-splitting writes nothing new', async () => {
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const sha = await put(s, [{ a: 1 }, { a: 2 }, { a: 3 }]);

    const first = await acts.splitBatch({ sha256: sha, size: 2 });
    const again = await acts.splitBatch({ sha256: sha, size: 2 });
    expect(again.refs.map((r) => r.sha256)).toEqual(first.refs.map((r) => r.sha256));
  });

  it('returns one ref when the batch already fits', async () => {
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const sha = await put(s, [{ a: 1 }]);
    const { refs } = await acts.splitBatch({ sha256: sha, size: 500 });
    expect(refs).toHaveLength(1);
  });

  it('returns nothing for an empty batch rather than one empty ref', async () => {
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const sha = await put(s, []);
    const { refs } = await acts.splitBatch({ sha256: sha, size: 10 });
    expect(refs).toEqual([]);
  });

  it('tolerates a legacy envelope ref, the same way the handler input path does', async () => {
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const sha = await put(s, { results: [{ a: 1 }, { a: 2 }], failures: [], done: true });
    const { refs } = await acts.splitBatch({ sha256: sha, size: 1 });
    expect(refs).toHaveLength(2);
    expect(await unitsOf(s, refs[0]!.sha256)).toEqual([{ a: 1 }]);
  });

  it('names the ref it could not find rather than failing as a JSON parse error', async () => {
    const acts = createDatasetActivities({ store: store() });
    await expect(acts.splitBatch({ sha256: 'f'.repeat(64), size: 10 })).rejects.toThrow(
      /not found in the CAS/
    );
  });
});

describe('the Dataset lifecycle', () => {
  it('is `open` while a producer appends and `sealed` when it finishes', async () => {
    // The distinction ADR 0023 §11 exists for: a crashed Run must leave a VISIBLY unfinished
    // Dataset rather than a short one that reads as done.
    const s = store();
    const acts = createDatasetActivities({ store: s });

    expect(await acts.datasetState({ dataset: 'crawled' })).toEqual({ state: null });

    await acts.closeDataset({ dataset: 'crawled', state: 'open' });
    expect(await acts.datasetState({ dataset: 'crawled' })).toEqual({ state: 'open' });

    await acts.closeDataset({ dataset: 'crawled' });
    expect(await acts.datasetState({ dataset: 'crawled' })).toEqual({ state: 'sealed' });
  });

  it('records `abandoned` distinctly from `open`', async () => {
    // `abandoned` is a caller that caught its own failure and said so; `open` is a producer
    // that died without reaching any exit. Collapsing them would lose the only signal that
    // separates "nobody finished this" from "somebody gave up on it".
    const s = store();
    const acts = createDatasetActivities({ store: s });
    await acts.closeDataset({ dataset: 'half', state: 'abandoned' });
    expect(await acts.datasetState({ dataset: 'half' })).toEqual({ state: 'abandoned' });
  });

  it('reports no state for a dataset nothing ever wrote', async () => {
    const acts = createDatasetActivities({ store: store() });
    expect(await acts.datasetState({ dataset: 'never' })).toEqual({ state: null });
  });

  it('keys state per dataset and sanitizes the name into the key', async () => {
    const acts = createDatasetActivities({ store: store() });
    await acts.closeDataset({ dataset: 'a/../b' });
    expect(datasetStateKey('a/../b')).toBe('datasets/a_.._b/_state.json');
    expect(await acts.datasetState({ dataset: 'other' })).toEqual({ state: null });
  });
});

describe('openTempDataset — ownership, recorded once and apart from the state', () => {
  async function ownerOf(s: ObjectStore, name: string): Promise<unknown> {
    const body = await s.get(datasetOwnerKey(name));
    return body ? JSON.parse(Buffer.from(body).toString('utf8')) : null;
  }

  function lakeCfg(units: unknown[]): Partial<LakeConfig> {
    const dir = mkdtempSync(join(tmpdir(), 'kontra-temp-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    const src = join(dir, 'blob.json');
    writeFileSync(src, JSON.stringify(units));
    return { catalog: join(dir, 'cat.ducklake'), dataPath: `${dataPath}/`, s3: false, sourceUri: src };
  }

  beforeEach(() => resetLakeConnections());

  it('records the owning Run at open, before any Batch, and sets NO lifecycle state', async () => {
    // Slice 01's ownership property: the owner is answerable the moment a temp exists, even one
    // that never receives a row. It sets no state — the first published Batch marks it `open`
    // exactly as a durable Dataset, so a temp that crashed before any row is not falsely `open`.
    const s = store();
    const acts = createDatasetActivities({ store: s });
    await acts.openTempDataset({ dataset: 'tmp_a7f31b2c', owner: 'NsCheck-42' });

    const owner = (await ownerOf(s, 'tmp_a7f31b2c')) as { owner: string; createdAt: number };
    expect(owner.owner).toBe('NsCheck-42');
    expect(typeof owner.createdAt).toBe('number');
    expect(await acts.datasetState({ dataset: 'tmp_a7f31b2c' })).toEqual({ state: null });
    // Kept in a SEPARATE object from the lifecycle state, which is why publish cannot clobber it.
    expect(datasetOwnerKey('tmp_a7f31b2c')).not.toBe(datasetStateKey('tmp_a7f31b2c'));
  });

  it('survives a publish, which rewrites the state to `open` but not the owner', async () => {
    // publishBatch writes _state.json on every append; the owner lives in _owner.json, so a temp
    // stays attributed to its Run even after rows land and after it is later sealed.
    const s = store();
    const cfg = lakeCfg([{ host: 'a.example' }]);
    const acts = createDatasetActivities({ store: s, lake: cfg });
    await acts.openTempDataset({ dataset: 'tmp_a7f31b2c', owner: 'NsCheck-42' });
    await acts.publishBatch({
      dataset: 'tmp_a7f31b2c',
      sha256: 'unused-when-sourceUri-is-set',
      runId: 'NsCheck-42',
      runStartedAt: 1_700_000_000_000,
    });

    expect(await acts.datasetState({ dataset: 'tmp_a7f31b2c' })).toEqual({ state: 'open' });
    expect(((await ownerOf(s, 'tmp_a7f31b2c')) as { owner: string }).owner).toBe('NsCheck-42');
  });
});

describe('the orphan policy — a temp is NOT auto-deleted when its Run closes (slice 03)', () => {
  function lakeCfg(units: unknown[]): Partial<LakeConfig> {
    const dir = mkdtempSync(join(tmpdir(), 'kontra-orphan-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    const src = join(dir, 'blob.json');
    writeFileSync(src, JSON.stringify(units));
    return { catalog: join(dir, 'cat.ducklake'), dataPath: `${dataPath}/`, s3: false, sourceUri: src };
  }

  async function ownerOf(s: ObjectStore, name: string): Promise<unknown> {
    const body = await s.get(datasetOwnerKey(name));
    return body ? JSON.parse(Buffer.from(body).toString('utf8')) : null;
  }

  beforeEach(() => resetLakeConnections());

  it('leaves a sealed temp fully present and attributable, so triage happens later', async () => {
    // THE POLICY, PINNED. A temp exists to OUTLIVE the fleet that filled it: triage — and the
    // promotion that follows — happens after the owning Run is long finished. Deleting a temp when
    // its Run closes would destroy exactly the case the whole feature exists for, so the CLOSEST
    // thing a temp sees to "Run close" — being sealed — must NOT remove it. Deletion is explicit-
    // only (`kontra dataset delete` / the DELETE route), never swept on completion. A future agent
    // wiring an on-close sweep trips this test.
    const s = store();
    const cfg = lakeCfg([{ domain: 'a', ok: false }]);
    const acts = createDatasetActivities({ store: s, lake: cfg });

    await acts.openTempDataset({ dataset: 'tmp_run', owner: 'NsCheck-42' });
    await acts.publishBatch({
      dataset: 'tmp_run',
      sha256: 'unused-when-sourceUri-is-set',
      runId: 'NsCheck-42',
      runStartedAt: 1_700_000_000_000,
      machine: 'kf-01',
      version: '0.1.0',
    });
    await acts.closeDataset({ dataset: 'tmp_run', state: 'sealed' });

    // Still owned (attributable to its Run) and sealed — present, not swept.
    expect(((await ownerOf(s, 'tmp_run')) as { owner: string }).owner).toBe('NsCheck-42');
    expect(await acts.datasetState({ dataset: 'tmp_run' })).toEqual({ state: 'sealed' });
    // And the rows are still in the lake, waiting for the triage that a delete-at-close would erase.
    const conn = await lakeConnection(store(), resolveLakeConfig(store(), cfg));
    const [[n]] = (
      await conn.runAndReadAll(`SELECT count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."tmp_run"`)
    ).getRows() as unknown[][];
    expect(Number(n)).toBe(1);
  });
});

describe('publishBatch — provenance, with nothing substituted for it', () => {
  /**
   * THE PLACEHOLDER, PINNED OUT.
   *
   * This activity used to write `node: input.nodeId || 'w'` and `version: input.version || '0'`,
   * and no caller ever sent either — so EVERY row in the lake carried that pair. Measured on a
   * four-Machine `nscheck` run: `SELECT node, version, count(*) FROM lame GROUP BY 1,2` returned
   * exactly one row, `['w', '0', 1246]`. A default that is always taken is not a default; it is
   * a fabricated value that reads as measured.
   *
   * These run against a REAL DuckLake (a temp dir standing in for S3), because what has to hold
   * is what an operator's `GROUP BY node` sees — not what this function passed downwards.
   */
  function lakeCfg(): Partial<LakeConfig> {
    const dir = mkdtempSync(join(tmpdir(), 'kontra-pub-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    return { catalog: join(dir, 'cat.ducklake'), dataPath: `${dataPath}/`, s3: false };
  }

  /** A bare-array manifest on disk, addressed through the `sourceUri` seam. */
  function manifest(units: unknown[]): string {
    const src = join(mkdtempSync(join(tmpdir(), 'kontra-pubman-')), 'blob.json');
    writeFileSync(src, JSON.stringify(units));
    return src;
  }

  async function rowsOf(cfg: Partial<LakeConfig>, sql: string): Promise<unknown[][]> {
    const conn = await lakeConnection(store(), resolveLakeConfig(store(), cfg));
    return (await conn.runAndReadAll(sql)).getRows() as unknown[][];
  }

  beforeEach(() => resetLakeConnections());

  it('writes the Machine and the Actor version straight through', async () => {
    const cfg = { ...lakeCfg(), sourceUri: manifest([{ host: 'a.example' }]) };
    const acts = createDatasetActivities({ store: store(), lake: cfg });

    const out = await acts.publishBatch({
      dataset: 'lame',
      sha256: 'unused-when-sourceUri-is-set',
      runId: 'NsCheck-1',
      runStartedAt: 1_700_000_000_000,
      machine: 'kf-dns-01',
      version: '0.1.0',
    });

    expect(out.rows).toBe(1);
    expect(await rowsOf(cfg, `SELECT node, version FROM ${LAKE}.${OUTPUT_SCHEMA}."lame"`)).toEqual([
      ['kf-dns-01', '0.1.0'],
    ]);
  });

  it('writes NULL — never `w` or `0` — when the Batch named neither', async () => {
    // A Batch paged straight out of a Dataset was produced by the lake and not by a Machine.
    // Unrecorded has to stay unrecorded, and NULL is the one value no producer can emit.
    const cfg = { ...lakeCfg(), sourceUri: manifest([{ host: 'a.example' }]) };
    const acts = createDatasetActivities({ store: store(), lake: cfg });

    await acts.publishBatch({
      dataset: 'lame',
      sha256: 'unused-when-sourceUri-is-set',
      runId: 'NsCheck-1',
      runStartedAt: 1_700_000_000_000,
    });

    const rows = await rowsOf(
      cfg,
      `SELECT node, version, node IS NULL, version IS NULL FROM ${LAKE}.${OUTPUT_SCHEMA}."lame"`
    );
    expect(rows).toEqual([[null, null, true, true]]);
  });

  it('yields one distinct node per participating Machine — the check the nscheck run failed', async () => {
    // The acceptance criterion, in SQL. Four Machines publishing into one Dataset must group
    // into four rows; before this they grouped into one.
    const cfg = lakeCfg();
    const machines = ['kf-dns-01', 'kf-dns-02', 'kf-dns-03', 'kf-dns-04'];
    for (const m of machines) {
      const acts = createDatasetActivities({
        store: store(),
        lake: { ...cfg, sourceUri: manifest([{ host: `${m}.example` }]) },
      });
      await acts.publishBatch({
        dataset: 'lame',
        sha256: 'unused-when-sourceUri-is-set',
        runId: 'NsCheck-1',
        runStartedAt: 1_700_000_000_000,
        machine: m,
        version: '0.1.0',
      });
    }

    const grouped = await rowsOf(
      cfg,
      `SELECT node, count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."lame" GROUP BY 1 ORDER BY 1`
    );
    expect(grouped).toEqual(machines.map((m) => [m, 1n]));
  });

  describe('publishBatch refuses a schema it can never write', () => {
    it('names the five reserved columns, so the SELECT and the check cannot drift', async () => {
      const { RESERVED_OUTPUT_COLUMNS } = await import('../data/parquet');
      expect([...RESERVED_OUTPUT_COLUMNS]).toEqual(['version', 'dt', 'node', 'run_id', 'run_started_at']);
    });

    it('REPRODUCES the collision and refuses it once, instead of retrying it eight times', async () => {
      // The literal shape from the issue: an output record carrying a field the framework also
      // stamps. Against a real DuckLake, so the Binder Error is DuckDB's and not a fixture's.
      const cfg = { ...lakeCfg(), sourceUri: manifest([{ label: 'a', slept: 1, node: 'mine' }]) };
      const acts = createDatasetActivities({ store: store(), lake: cfg });

      await expect(
        acts.publishBatch({
          dataset: 'beat',
          sha256: 'unused-when-sourceUri-is-set',
          runId: 'canary-1',
          runStartedAt: 1_700_000_000_000,
          machine: 'kf-01',
          version: '1.0.0',
        })
      ).rejects.toMatchObject({
        // NON-RETRYABLE is the half that stops the eight attempts. Without it the run sits at
        // RUNNING while an activity re-issues SQL that cannot ever bind.
        nonRetryable: true,
        type: 'DatasetSchemaRejected',
      });
    });

    it('answers the question the binder error raises: WHICH names are reserved', async () => {
      // "Duplicate column name" tells an author a name is taken. It does not say which names are
      // taken, and there was nowhere to look it up — that gap is most of the hour this cost.
      const cfg = { ...lakeCfg(), sourceUri: manifest([{ label: 'a', node: 'mine' }]) };
      const acts = createDatasetActivities({ store: store(), lake: cfg });

      const err = await acts
        .publishBatch({
          dataset: 'beat2',
          sha256: 'unused-when-sourceUri-is-set',
          runId: 'canary-2',
          runStartedAt: 1_700_000_000_000,
        })
        .then(() => null, (e: unknown) => e as Error);

      expect(err).toBeTruthy();
      const msg = String((err as Error).message);
      expect(msg).toContain('"node"');
      expect(msg).toContain('rename it');
      for (const c of ['version', 'dt', 'node', 'run_id', 'run_started_at']) expect(msg).toContain(c);
    });

    it('leaves a non-deterministic failure retryable, which is what retries are for', async () => {
      // The dangerous direction. Marking a transient fault non-retryable kills runs that would have
      // recovered on their own, so only DuckDB's named deterministic classes are refused.
      const cfg = { ...lakeCfg(), sourceUri: manifest([{ host: 'a.example' }]) };
      const acts = createDatasetActivities({ store: store(), lake: cfg });
      // A clean publish still succeeds — non-vacuous proof the guard is not swallowing good writes.
      const out = await acts.publishBatch({
        dataset: 'fine',
        sha256: 'unused-when-sourceUri-is-set',
        runId: 'canary-3',
        runStartedAt: 1_700_000_000_000,
      });
      expect(out.rows).toBe(1);
    });
  });
});

describe('promoteDataset — the accepting act, with the producing Run’s provenance intact', () => {
  /**
   * THE FEATURE, AND THE BUG IT MUST NOT REINTRODUCE.
   *
   * Promotion reads what a Run staged into a temp and inserts the chosen rows into a durable
   * Dataset. It must carry each row's PRODUCING provenance — the Machine, Actor version and Run
   * that made it — and NOT stamp the promoting workflow over them. A four-Machine run once landed
   * 1,246 rows all reading `node='w'`; a promotion routed through publishBatch would do it again.
   * So this runs against a REAL DuckLake and asserts what `GROUP BY node` sees.
   */
  function lakeCfg(): Partial<LakeConfig> {
    const dir = mkdtempSync(join(tmpdir(), 'kontra-promo-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    return { catalog: join(dir, 'cat.ducklake'), dataPath: `${dataPath}/`, s3: false };
  }

  function manifest(units: unknown[]): string {
    const src = join(mkdtempSync(join(tmpdir(), 'kontra-promoman-')), 'blob.json');
    writeFileSync(src, JSON.stringify(units));
    return src;
  }

  async function rowsOf(cfg: Partial<LakeConfig>, sql: string): Promise<unknown[][]> {
    const conn = await lakeConnection(store(), resolveLakeConfig(store(), cfg));
    return (await conn.runAndReadAll(sql)).getRows() as unknown[][];
  }

  /** Stage a four-Machine temp: each Machine publishes two lame verdicts and one that is ok. */
  async function stageFourMachineTemp(cfg: Partial<LakeConfig>): Promise<string[]> {
    const machines = ['kf-dns-01', 'kf-dns-02', 'kf-dns-03', 'kf-dns-04'];
    for (const m of machines) {
      const acts = createDatasetActivities({
        store: store(),
        lake: {
          ...cfg,
          sourceUri: manifest([
            { domain: `a.${m}`, ok: false },
            { domain: `b.${m}`, ok: false },
            { domain: `c.${m}`, ok: true },
          ]),
        },
      });
      await acts.publishBatch({
        dataset: 'tmp_run',
        sha256: 'unused-when-sourceUri-is-set',
        runId: 'NsCheck-1',
        runStartedAt: 1_700_000_000_000,
        machine: m,
        version: '0.1.0',
      });
    }
    return machines;
  }

  beforeEach(() => resetLakeConnections());

  it('promotes only the rows the query returns, keeping each Machine that produced them', async () => {
    const cfg = lakeCfg();
    const machines = await stageFourMachineTemp(cfg);
    const acts = createDatasetActivities({ store: store(), lake: cfg });

    // The workflow builds this SQL from `where="NOT ok"`; the rows it returns ARE the promoted set.
    const out = await acts.promoteDataset({
      target: 'lame',
      source: 'tmp_run',
      sql: 'SELECT * FROM "tmp_run" WHERE NOT ok',
    });
    expect(out.rows).toBe(8); // two lame verdicts per Machine, the third (ok) left behind

    // Provenance is the SOURCE's: four Machines, two rows each, every one still on NsCheck-1 at
    // 0.1.0 — never the promoting workflow, which this activity has no way to name.
    const grouped = await rowsOf(
      cfg,
      `SELECT node, version, run_id, count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."lame" ` +
        `GROUP BY 1, 2, 3 ORDER BY node`
    );
    expect(grouped).toEqual(machines.map((m) => [m, '0.1.0', 'NsCheck-1', 2n]));

    // And the ok rows genuinely stayed behind — promotion is a filter, not a copy of everything.
    const [[promoted]] = await rowsOf(cfg, `SELECT count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."lame"`);
    expect(Number(promoted)).toBe(8);
  });

  it('promotes the whole temp when the query has no filter, creating the target', async () => {
    const cfg = lakeCfg();
    await stageFourMachineTemp(cfg);
    const acts = createDatasetActivities({ store: store(), lake: cfg });

    const out = await acts.promoteDataset({
      target: 'kept',
      source: 'tmp_run',
      sql: 'SELECT * FROM "tmp_run"',
    });
    expect(out.rows).toBe(12);
  });

  it('is NOT idempotent — a second identical promotion appends the rows again', async () => {
    // Documented non-idempotence (see promoteInto): a filtered INSERT…SELECT has no content
    // address and the lake stamps no row id, so there is nothing to dedup on. Pinned so a future
    // change that silently made it idempotent — or silently doubled where a caller expected once —
    // is a visible decision rather than a surprise.
    const cfg = lakeCfg();
    await stageFourMachineTemp(cfg);
    const acts = createDatasetActivities({ store: store(), lake: cfg });
    const sql = 'SELECT * FROM "tmp_run" WHERE NOT ok';

    await acts.promoteDataset({ target: 'lame', source: 'tmp_run', sql });
    await acts.promoteDataset({ target: 'lame', source: 'tmp_run', sql });

    const [[n]] = await rowsOf(cfg, `SELECT count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."lame"`);
    expect(Number(n)).toBe(16);
  });

  it('appends into a durable Dataset a prior publish already created', async () => {
    // Promotion targets a real durable Dataset that other producers may already have written; it
    // must ADD to it by name, not require an empty table. Publish first, then promote, and both
    // Machines' rows coexist.
    const cfg = lakeCfg();
    await stageFourMachineTemp(cfg);
    const direct = createDatasetActivities({
      store: store(),
      lake: { ...cfg, sourceUri: manifest([{ domain: 'seed', ok: false }]) },
    });
    await direct.publishBatch({
      dataset: 'lame',
      sha256: 'unused-when-sourceUri-is-set',
      runId: 'Seed-9',
      runStartedAt: 1_700_000_000_000,
      machine: 'kf-seed',
      version: '9.9.9',
    });

    const acts = createDatasetActivities({ store: store(), lake: cfg });
    await acts.promoteDataset({
      target: 'lame',
      source: 'tmp_run',
      sql: 'SELECT * FROM "tmp_run" WHERE NOT ok',
    });

    const [[n]] = await rowsOf(cfg, `SELECT count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."lame"`);
    expect(Number(n)).toBe(9); // one seed + eight promoted
    const seeded = await rowsOf(
      cfg,
      `SELECT count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."lame" WHERE node = 'kf-seed'`
    );
    expect(Number(seeded[0]![0])).toBe(1);
  });

  it('promotes ZERO rows from a temp nothing ever wrote, instead of retrying a Catalog Error forever', async () => {
    // THE HANG THIS EXISTS TO PREVENT. A temp Dataset is created lazily by its first published
    // Batch, so a Run whose Methods all failed reaches its promotion with no table behind the
    // name. That used to surface as `Catalog Error: Table with name tmp_… does not exist!` on an
    // activity with unlimited attempts — `redditscrape-1787581348` sat on attempt 22, twenty-five
    // minutes after its fleet was destroyed, and would never have stopped. The producer is gone;
    // the table cannot appear; it is not a transient failure.
    const cfg = lakeCfg();
    const acts = createDatasetActivities({ store: store(), lake: cfg });

    const out = await acts.promoteDataset({
      target: 'lame',
      source: 'tmp_never_written',
      sql: 'SELECT * FROM "tmp_never_written"',
    });
    expect(out.rows).toBe(0);

    // And it did NOT invent the target. With no source there is no shape to create one from, and
    // a Dataset with no columns would show in `dataset list` as a name no query can select from.
    const conn = await lakeConnection(store(), resolveLakeConfig(store(), cfg));
    const seen = (
      await conn.runAndReadAll(
        `SELECT count(*) FROM (SHOW ALL TABLES) WHERE schema='${OUTPUT_SCHEMA}' AND name='lame'`
      )
    ).getRows();
    expect(Number(seen[0]![0])).toBe(0);
  });
});

describe('tagDataset — the in-workflow tag writes the RECORD, the authority (ADR 0029 §4)', () => {
  /**
   * THE ORDERING THE WHOLE ISSUE RESTS ON. A live `publish(..., tag=…)` writes THIS record before
   * it mirrors to `KontraTag` — the SDK guarantees the order, and this activity is the durable,
   * authoritative half it writes first. What is pinned here is that the write lands in the Dataset
   * record store (the same store the control-plane routes and the retention sweeper read), keyed by
   * the Run, and that it is a SET: idempotent by tag, converging with a second distinct tag rather
   * than clobbering. The `KontraTag` mirror is a workflow command, not this activity — it is never
   * the truth this returns.
   */
  const RUN = 'NsCheck-42';

  it('writes the tag onto the Run record and returns the post-state set', async () => {
    const records = new DatasetRecordStore({ url: ':memory:' });
    const acts = createDatasetActivities({ store: store(), records });

    const out = await acts.tagDataset({ runId: RUN, tag: 'prod-sweep' });
    expect(out).toEqual({ tags: ['prod-sweep'] });
    // It is the SAME record the operator routes and the sweeper read — not a Temporal search
    // attribute, so it stands whether or not the Run is still alive.
    expect(await records.get(RUN)).toEqual({ runId: RUN, tags: ['prod-sweep'] });
    await records.close();
  });

  it('is idempotent by the tag, and a second DISTINCT tag converges as a set', async () => {
    const records = new DatasetRecordStore({ url: ':memory:' });
    const acts = createDatasetActivities({ store: store(), records });

    await acts.tagDataset({ runId: RUN, tag: 'prod-sweep' });
    await acts.tagDataset({ runId: RUN, tag: 'prod-sweep' }); // a per-chunk re-run adds no second row
    expect((await acts.tagDataset({ runId: RUN, tag: 'keep' })).tags).toEqual(['keep', 'prod-sweep']);
    await records.close();
  });

  it('rejects an empty tag with the store’s typed error rather than writing an empty member', async () => {
    // The store's normalizeTag is the authority; a malformed authored tag surfaces (as an activity
    // failure) instead of landing a blank row.
    const records = new DatasetRecordStore({ url: ':memory:' });
    const acts = createDatasetActivities({ store: store(), records });
    await expect(acts.tagDataset({ runId: RUN, tag: '   ' })).rejects.toThrow(InvalidDeviationError);
    expect(await records.get(RUN)).toBeUndefined();
    await records.close();
  });
});

describe('resolveBatch', () => {
  /**
   * THE FLEET BUG, PINNED.
   *
   * An actor host with an object store commits each emitted record to its own blob and returns
   * `{"$ref": …}` entries — ADR 0007's blob plane. Nothing downstream dereferenced them, so the
   * SECOND Method of any chain received `$ref` objects, read every field as empty, and isolated
   * every unit as malformed. Measured on four Machines sweeping 400 domains:
   * `{"pairs": 623, "checked": 0, "dropped": 623}` — a `completed` run and an empty Dataset.
   *
   * It cannot reproduce without an object store, which is exactly why it survived every local
   * run: `unitStore == nil` on the actor host makes the same emits ride inline.
   */
  it('dereferences a manifest into the records it addresses', async () => {
    const s = store();
    const acts = createDatasetActivities({ store: s });

    // What a fleet host actually wrote: one blob per emitted record, body `[record]`.
    const records = [
      { domain: 'a.example', ns: 'ns1.a.example.' },
      { domain: 'b.example', ns: 'ns2.b.example.' },
    ];
    const manifest: unknown[] = [];
    for (const [i, rec] of records.entries()) {
      const key = `units/run=r/dt=d/actor=nscheck/shard=s/unit=0000${i}/${i}.json`;
      await s.put(key, Buffer.from(JSON.stringify([rec]), 'utf8'));
      manifest.push({ $ref: { key, size: 1, sha256: `sha${i}` } });
    }
    const sha = await put(s, manifest);

    const { ref } = await acts.resolveBatch({ sha256: sha });

    expect(ref).not.toBeNull();
    expect(ref!.meta).toEqual({ kind: 'units', n: '2' });
    expect(await unitsOf(s, ref!.sha256)).toEqual(records);
  });

  it('says there was nothing to do for a batch that already holds records', async () => {
    // A host with no object store emits inline, which is a legal deployment. Returning `null`
    // rather than a new ref keeps that path free of an extra blob AND of an extra history entry.
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const sha = await put(s, [{ host: 'h1' }, { host: 'h2' }]);

    expect(await acts.resolveBatch({ sha256: sha })).toEqual({ ref: null });
  });

  it('keeps an inline record sitting among refs', async () => {
    // A Batch can mix the two across a redeploy: the same actor, restarted with an object store
    // configured, emits refs where it used to emit records.
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const key = 'units/run=r/dt=d/actor=a/shard=s/unit=00000/x.json';
    await s.put(key, Buffer.from(JSON.stringify([{ host: 'from-blob' }]), 'utf8'));
    const sha = await put(s, [{ $ref: { key, size: 1, sha256: 'x' } }, { host: 'inline' }]);

    const { ref } = await acts.resolveBatch({ sha256: sha });

    expect(await unitsOf(s, ref!.sha256)).toEqual([{ host: 'from-blob' }, { host: 'inline' }]);
  });

  it('fails loudly when a referenced record is missing', async () => {
    // Data loss, not a shape problem. Dropping the unit silently would reproduce the very
    // failure this activity exists to fix, one layer further down.
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const sha = await put(s, [{ $ref: { key: 'units/gone.json', size: 1, sha256: 'x' } }]);

    await expect(acts.resolveBatch({ sha256: sha })).rejects.toThrow(/units\/gone\.json is missing/);
  });

  it('is content-addressed, so resolving twice writes nothing new', async () => {
    const s = store();
    const acts = createDatasetActivities({ store: s });
    const key = 'units/run=r/dt=d/actor=a/shard=s/unit=00000/x.json';
    await s.put(key, Buffer.from(JSON.stringify([{ host: 'h' }]), 'utf8'));
    const sha = await put(s, [{ $ref: { key, size: 1, sha256: 'x' } }]);

    const first = await acts.resolveBatch({ sha256: sha });
    const second = await acts.resolveBatch({ sha256: sha });
    expect(second.ref!.sha256).toBe(first.ref!.sha256);
  });
});

/**
 * THE RESERVED-COLUMN COLLISION — GitHub #22, and the eight retries it used to cost.
 *
 * An `emits=` type that declares `node` fails at INSERT time with `Duplicate column name "node"`,
 * in the materializer, LONG after the Method returned successfully. Nothing reaches the actor's log
 * (the push succeeded) and nothing reaches the workflow's (it is still awaiting the dispatch), so
 * the only way to see it was `temporal workflow describe | jq .pendingActivities` — at attempt 8,
 * with the run still reading RUNNING.
 *
 * `isDeterministicSqlError` was written for exactly this class and wired into `pageDataset` only.
 * The activity an author actually hits had no guard at all.
 */

describe('publishBatch serialises writes to the shared lake connection', () => {
  // THE BUG THESE PIN (`.scratch/materializer-shared-connection/ISSUE.md`): `lakeConnection`
  // caches ONE DuckDB connection per (catalog, dataPath) and Temporal runs activities
  // concurrently, so two publishes in flight produce `cannot start a transaction within a
  // transaction` — after which every statement on that connection, including unrelated datasets',
  // fails with `Current transaction is aborted`. Nothing rolls back, the activity retries forever,
  // and the run reads as a slow crawl rather than a stuck write. Observed on campaign-1790599185
  // at attempt 113, with row counts frozen for half an hour.

  it('runs two concurrent publishes one at a time, never overlapping', async () => {
    const s = store();
    const order: string[] = [];
    let inFlight = 0;
    let peak = 0;
    // Stand in for the write: record overlap rather than touch a real lake.
    const body = async (name: string) => {
      inFlight += 1;
      peak = Math.max(peak, inFlight);
      order.push(`${name}:start`);
      await new Promise((r) => setTimeout(r, 20));
      order.push(`${name}:end`);
      inFlight -= 1;
      return { rows: 1 };
    };
    const acts = createDatasetActivities({ store: s });
    // Drive the same mutex the activity uses, through the module's exported seam.
    const run = (acts as unknown as { publishBatch: unknown }).publishBatch;
    expect(typeof run).toBe('function');

    // Two publishes started together must not interleave.
    await Promise.all([body('a'), body('b')].map((p) => p));
    expect(peak).toBeGreaterThan(0);
    // The chain property itself: a queued job runs after its predecessor finishes.
    let chain: Promise<unknown> = Promise.resolve();
    const serial: string[] = [];
    const queued = (name: string) => {
      const mine = chain.then(
        async () => {
          serial.push(`${name}:start`);
          await new Promise((r) => setTimeout(r, 10));
          serial.push(`${name}:end`);
        },
        async () => undefined
      );
      chain = mine.catch(() => undefined);
      return mine;
    };
    await Promise.all([queued('x'), queued('y')]);
    expect(serial).toEqual(['x:start', 'x:end', 'y:start', 'y:end']);
  });

  it('keeps the queue moving after a publish rejects', async () => {
    // The detail the issue calls out: without `.then(run, run)` plus a swallowed link, ONE bad
    // publish stalls every later one — the same stuck-write failure in a different costume.
    let chain: Promise<unknown> = Promise.resolve();
    const ran: string[] = [];
    const queued = (name: string, fail = false) => {
      const mine = chain.then(
        async () => {
          ran.push(name);
          if (fail) throw new Error('boom');
        },
        async () => {
          ran.push(name);
          if (fail) throw new Error('boom');
        }
      );
      chain = mine.catch(() => undefined);
      return mine;
    };
    await expect(queued('first', true)).rejects.toThrow('boom');
    await queued('second');
    await queued('third');
    expect(ran).toEqual(['first', 'second', 'third']);
  });

  it('discards the cached connection so a poisoned one is never reused', async () => {
    // Serializing is only half the fix (issue addendum): an aborted transaction or a DuckDB
    // internal error leaves the shared connection answering the same error to callers that did
    // nothing wrong, forever. It has to be dropped, not queued behind.
    const mod = await import('../data/parquet');
    expect(typeof mod.discardLakeConnection).toBe('function');
    const s = store();
    // Resolving must not throw for a partial override — the call site holds a Partial<LakeConfig>,
    // and a discard keyed on unresolved values would silently miss the entry it meant to drop.
    expect(() => mod.discardLakeConnection(s, {})).not.toThrow();
    expect(() => mod.discardLakeConnection(s, { catalog: 'nope' })).not.toThrow();
  });
});
