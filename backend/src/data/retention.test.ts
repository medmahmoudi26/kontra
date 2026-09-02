/**
 * The retention sweeper (ADR 0029 §3, §5): untagged Datasets expire after a TTL THIS repo owns; a
 * tagged one, an `open` one, and a temporary one are kept.
 *
 * The KEEP/COLLECT policy is a pure function ({@link classifySweep}) tested directly, and then the
 * whole read+collect path is exercised against a REAL DuckLake (a local dir standing in for S3) with
 * real SQLite ledger/record/summary stores — so "tagged after close survives", "a dry run deletes
 * nothing", and "a temporary is never swept" are proven end to end, not against doubles.
 */

import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import {
  datasetOwnerKey,
  datasetStateKey,
  deleteTemporaryDataset,
  listDatasets,
  type DatasetState,
} from './datasets';
import { DatasetRecordStore } from './datasetRecords';
import { MATERIALIZATION_SCHEMA_VERSION } from './materialization';
import { MaterializationStore } from './materializationStore';
import {
  LAKE,
  OUTPUT_SCHEMA,
  dtPartition,
  lakeConnection,
  resetLakeConnections,
  resolveLakeConfig,
  safeName,
  writeDatasetParquet,
  type LakeConfig,
} from './parquet';
import {
  DATASET_RETENTION_GRACE_MS,
  DATASET_RETENTION_TTL_MS,
  SWEEP_SAMPLE_SIZE,
  classifySweep,
  summarizeSweep,
  sweepDatasets,
  type SweepCandidate,
  type SweepDecision,
  type SweepReport,
} from './retention';
import { SummaryStore } from './summaries';

const HOUR = 60 * 60 * 1000;
const NOW = Date.UTC(2026, 7, 19, 12, 0, 0);

/** A candidate with everything defaulted to "a plain durable Dataset", so each test flips exactly the
 *  one field it is about. */
function candidate(over: Partial<SweepCandidate> = {}): SweepCandidate {
  return {
    runId: 'run-x',
    name: 'wf-nscheck-0.1.0--2026-08-18T00-00-00Z--abc123',
    lastWriteAt: NOW - 100 * HOUR,
    rows: 10,
    tagged: false,
    open: false,
    temporary: false,
    tables: ['nscheck'],
    ...over,
  };
}

const DEFAULTS = { ttlMs: DATASET_RETENTION_TTL_MS, graceMs: DATASET_RETENTION_GRACE_MS, now: NOW };

describe('classifySweep — the four safeguards', () => {
  it('collects a durable, untagged, sealed Dataset past TTL + grace', () => {
    const [d] = classifySweep([candidate()], DEFAULTS);
    expect(d!.disposition).toBe('collect');
  });

  // AC: the TTL is a repo constant, not read from the namespace config.
  it('the TTL is a repo constant of 24h and the grace is a distinct window', () => {
    expect(DATASET_RETENTION_TTL_MS).toBe(24 * HOUR);
    expect(DATASET_RETENTION_GRACE_MS).toBeGreaterThan(0);
    // The classifier takes the number as an argument; nothing here reads a namespace retention. Two
    // different TTLs over the SAME candidate give two different answers, which is the whole point of
    // owning the number: the Dataset's lifetime is decided by this constant, not by an ops setting.
    const old = candidate({ lastWriteAt: NOW - 25 * HOUR });
    expect(classifySweep([old], { ttlMs: 12 * HOUR, graceMs: 0, now: NOW })[0]!.disposition).toBe('collect');
    expect(classifySweep([old], { ttlMs: 48 * HOUR, graceMs: 0, now: NOW })[0]!.disposition).toBe('kept-fresh');
  });

  // AC: a Dataset tagged after its run closed survives the sweep. The classifier reads `tagged`, which
  // gatherCandidates fills from the RECORD (not KontraTag) — so WHEN it was tagged is irrelevant here.
  it('keeps a tagged Dataset however old', () => {
    const [d] = classifySweep([candidate({ tagged: true, lastWriteAt: NOW - 1000 * HOUR })], DEFAULTS);
    expect(d!.disposition).toBe('kept-tagged');
  });

  // AC: a Dataset still being appended to is never collected, regardless of age.
  it('keeps an open Dataset regardless of age', () => {
    const [d] = classifySweep([candidate({ open: true, lastWriteAt: 0 })], DEFAULTS);
    expect(d!.disposition).toBe('kept-open');
    // Even one that is BOTH ancient and untagged: `open` wins over the age gate.
    const [d2] = classifySweep([candidate({ open: true, lastWriteAt: NOW - 5000 * HOUR })], DEFAULTS);
    expect(d2!.disposition).toBe('kept-open');
  });

  // Critical interaction with ADR 0028: a temporary Dataset is deleted explicitly, NEVER swept.
  it('keeps a temporary Dataset regardless of age, tag or lifecycle', () => {
    const [d] = classifySweep(
      [candidate({ temporary: true, lastWriteAt: NOW - 9999 * HOUR })],
      DEFAULTS
    );
    expect(d!.disposition).toBe('kept-temporary');
  });

  /**
   * WHAT A TAG DOES TO A TEMP'S LIFETIME: nothing, and that is the answer rather than an omission.
   *
   * An operator can tag a temporary Dataset from the Datasets page — the record is keyed by the
   * **Run** (ADR 0029 §4) and a temp has exactly one, its owner — and the two landed decisions meet
   * here: §3 says tagged output is KEPT, ADR 0028 says a temp is staging, owned, explicitly
   * deletable. They do not fight, because a temp has no clock to extend: it is kept for being
   * temporary whether or not it is tagged, and the disposition says which safeguard applied. The
   * tag's real effect is on the Run's DURABLE output — pinned end to end below.
   */
  it('a tag does not change a temp’s disposition — it is kept for being temporary either way', () => {
    const old = { temporary: true, lastWriteAt: NOW - 9999 * HOUR };
    expect(classifySweep([candidate({ ...old })], DEFAULTS)[0]!.disposition).toBe('kept-temporary');
    expect(classifySweep([candidate({ ...old, tagged: true })], DEFAULTS)[0]!.disposition).toBe(
      'kept-temporary'
    );
    // Neither is a collection, which is the fact that matters — the precedence only decides which
    // of two true reasons is reported.
    for (const c of [candidate({ ...old }), candidate({ ...old, tagged: true })]) {
      expect(classifySweep([c], DEFAULTS)[0]!.disposition).not.toBe('collect');
    }
  });

  // AC / fix #3: a grace window, not a delete at exactly T+TTL.
  it('keeps a Dataset inside the grace window and collects one past it', () => {
    const insideGrace = candidate({ lastWriteAt: NOW - (DATASET_RETENTION_TTL_MS + DATASET_RETENTION_GRACE_MS) + 1000 });
    const pastGrace = candidate({ lastWriteAt: NOW - (DATASET_RETENTION_TTL_MS + DATASET_RETENTION_GRACE_MS) - 1000 });
    expect(classifySweep([insideGrace], DEFAULTS)[0]!.disposition).toBe('kept-fresh');
    expect(classifySweep([pastGrace], DEFAULTS)[0]!.disposition).toBe('collect');
    // Exactly at TTL (before grace closes) is still kept — the grace is what stops the sweep racing
    // an operator who tags at the edge.
    const atTtl = candidate({ lastWriteAt: NOW - DATASET_RETENTION_TTL_MS });
    expect(classifySweep([atTtl], DEFAULTS)[0]!.disposition).toBe('kept-fresh');
  });

  // fix #2: an unknown last write is unknown age, not infinite age — kept, not collected.
  it('keeps a Dataset whose last write is unknown rather than guessing it is ancient', () => {
    const [d] = classifySweep([candidate({ lastWriteAt: 0 })], DEFAULTS);
    expect(d!.disposition).toBe('kept-fresh');
  });
});

// --- end-to-end against a real DuckLake + real SQLite stores ------------------------------------

interface Ctx {
  store: ObjectStore;
  cfg: Partial<LakeConfig>;
  blobs: string;
  materialization: MaterializationStore;
  records: DatasetRecordStore;
  summaries: SummaryStore;
}

function lake(): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-retention-'));
  const dataPath = join(dir, 'data');
  const blobs = join(dir, 'blobs');
  mkdirSync(dataPath, { recursive: true });
  mkdirSync(blobs, { recursive: true });
  const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
  return {
    store,
    blobs,
    cfg: { catalog: join(dir, 'cat.ducklake'), dataPath: `${dataPath}/`, s3: false, blobBase: `${blobs}/` },
    materialization: new MaterializationStore({ url: ':memory:' }),
    records: new DatasetRecordStore({ url: ':memory:' }),
    summaries: new SummaryStore({ url: ':memory:' }),
  };
}

let seq = 0;

interface RunSpec {
  runId: string;
  actor: string;
  version: string;
  runStartedAt: number;
  rows?: unknown[];
  state?: DatasetState;
  owner?: string;
  /**
   * Record this Run in the materialization LEDGER. Defaults to true — the v1 shape, where the graph
   * interpreter declared every dispatch.
   *
   * FALSE IS THE v2 SHAPE, and it is not a hypothetical: measured on this box, `/api/datasets/runs`
   * holds 50 dispatches and NONE of them is a Run the actorkit path produced, because `publishBatch`
   * writes lake rows and no ledger record. So a v2 Run's Dataset is resolvable only from the rows'
   * own `run_id` statistics.
   */
  ledger?: boolean;
}

/** Materialize one Run's output through the real claim-check path AND record it in the ledger, so the
 *  ledger↔lake join gatherCandidates does resolves the Run. */
async function seedRun(ctx: Ctx, spec: RunSpec): Promise<void> {
  const rows = spec.rows ?? [{ url: 'a', ok: true }];
  const name = `u${(seq += 1)}.json`;
  const body = JSON.stringify(rows);
  writeFileSync(join(ctx.blobs, name), body);
  const ref = { $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length } };
  const src = join(mkdtempSync(join(tmpdir(), 'kontra-src-')), 'blob.json');
  writeFileSync(src, JSON.stringify({ results: [ref], failures: [] }));
  await writeDatasetParquet(
    ctx.store,
    { sha256: 'unused', actor: spec.actor, version: spec.version, runId: spec.runId, node: 'n1', runStartedAt: spec.runStartedAt },
    { ...ctx.cfg, sourceUri: src }
  );

  if (spec.ledger !== false) {
    const key = { runId: spec.runId, actor: spec.actor, version: spec.version, node: 'n1', schemaVersion: MATERIALIZATION_SCHEMA_VERSION };
    await ctx.materialization.declare(key, spec.runStartedAt);
    await ctx.materialization.complete(key, { rows: rows.length, bytes: body.length, snapshotId: 1, tbl: safeName(spec.actor) });
  }

  if (spec.state) {
    await ctx.store.put(datasetStateKey(spec.actor), Buffer.from(JSON.stringify({ state: spec.state }), 'utf8'));
  }
  if (spec.owner) {
    await ctx.store.put(datasetOwnerKey(spec.actor), Buffer.from(JSON.stringify({ owner: spec.owner, createdAt: spec.runStartedAt }), 'utf8'));
  }
}

/** A direct count of a table's live rows for one Run — bypasses the listing so a collection's physical
 *  DELETE is asserted at the source, not through the catalog's row-count summing. */
async function rowsInLake(ctx: Ctx, table: string, runId: string): Promise<number> {
  const conn = await lakeConnection(ctx.store, resolveLakeConfig(ctx.store, ctx.cfg));
  const res = await conn.runAndReadAll(
    `SELECT count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."${safeName(table)}" WHERE run_id = '${runId}'`
  );
  return Number(res.getRows()[0]?.[0] ?? 0);
}

function deps(ctx: Ctx) {
  return { store: ctx.store, lake: ctx.cfg, materialization: ctx.materialization, records: ctx.records, summaries: ctx.summaries };
}

describe('sweepDatasets — end to end over a real lake', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });
  afterEach(async () => {
    await ctx.materialization.close().catch(() => undefined);
    await ctx.records.close().catch(() => undefined);
    await ctx.summaries.close().catch(() => undefined);
  });

  // AC: a Dataset tagged AFTER its run closed survives the sweep. The tag is added to the record long
  // after the run sealed — the exact case a KontraTag query would miss — and the sweep keeps it.
  it('keeps a Dataset tagged after its run closed', async () => {
    const runStartedAt = NOW - 100 * HOUR;
    await seedRun(ctx, { runId: 'kept', actor: 'nscheck', version: '0.1.0', runStartedAt, state: 'sealed' });
    // Tagged now, 100h after the run's last write — nothing wrote KontraTag, only the record.
    await ctx.records.addTag('kept', 'keep-this');

    const report = await sweepDatasets(deps(ctx), { now: NOW, dryRun: false });

    expect(report.collected).toHaveLength(0);
    expect(report.kept.map((k) => k.disposition)).toContain('kept-tagged');
    expect(await rowsInLake(ctx, 'nscheck', 'kept')).toBe(1);
    expect(await ctx.materialization.listForRun('kept')).toHaveLength(1);
  });

  // AC: a dry-run mode reports what WOULD be collected before anything deletes.
  it('a dry run reports the collection but deletes nothing; a real run then collects', async () => {
    await seedRun(ctx, { runId: 'doomed', actor: 'nscheck', version: '0.1.0', runStartedAt: NOW - 100 * HOUR, state: 'sealed' });
    // The catalog stamps the file's commit time (real now) as the LAST WRITE, so the sweep clock is
    // aged past TTL + grace to make the sealed, untagged Dataset a collection candidate.
    const clock = Date.now() + DATASET_RETENTION_TTL_MS + DATASET_RETENTION_GRACE_MS + HOUR;

    // Default is a dry run — pass only the clock.
    const dry = await sweepDatasets(deps(ctx), { now: clock });
    expect(dry.dryRun).toBe(true);
    expect(dry.collected.map((c) => c.runId)).toEqual(['doomed']);
    expect(dry.purgedRuns).toBe(0);
    // Nothing gone.
    expect(await rowsInLake(ctx, 'nscheck', 'doomed')).toBe(1);
    expect(await ctx.materialization.listForRun('doomed')).toHaveLength(1);

    // Now for real — the SAME classification, but it collects.
    const real = await sweepDatasets(deps(ctx), { now: clock, dryRun: false });
    expect(real.collected.map((c) => c.runId)).toEqual(['doomed']);
    expect(real.purgedRuns).toBe(1);
    expect(await rowsInLake(ctx, 'nscheck', 'doomed')).toBe(0);
    expect(await ctx.materialization.listForRun('doomed')).toHaveLength(0);
  });

  // AC: the untagged TTL is the repo constant. With no ttl/grace passed, a run one second past
  // (TTL + grace) collects and one a second short is kept — proving the defaults ARE the constants.
  it('uses the repo TTL + grace constants by default', async () => {
    await seedRun(ctx, { runId: 'edge-old', actor: 'aa', version: '0.1.0', runStartedAt: NOW - 200 * HOUR, state: 'sealed' });
    const report = await sweepDatasets(deps(ctx), {
      // Age the clock so 'edge-old' sits just past TTL+grace from its (real) last write.
      now: Date.now() + DATASET_RETENTION_TTL_MS + DATASET_RETENTION_GRACE_MS + HOUR,
    });
    expect(report.ttlMs).toBe(DATASET_RETENTION_TTL_MS);
    expect(report.graceMs).toBe(DATASET_RETENTION_GRACE_MS);
    expect(report.collected.map((c) => c.runId)).toContain('edge-old');
  });

  // Critical interaction: a temporary Dataset is NEVER swept, even old, untagged and sealed — it is
  // removed explicitly (temp-datasets slice 03). A real (non-dry) sweep leaves it fully intact.
  it('never collects a temporary Dataset, and leaves it fully attributable after a real sweep', async () => {
    const runStartedAt = NOW - 500 * HOUR;
    await seedRun(ctx, { runId: 'tmp-run', actor: 'tmp_a7f3', version: '0.1.0', runStartedAt, state: 'sealed', owner: 'tmp-run' });

    const report = await sweepDatasets(deps(ctx), { now: NOW, dryRun: false });

    expect(report.collected).toHaveLength(0);
    expect(report.kept.find((k) => k.runId === 'tmp-run')?.disposition).toBe('kept-temporary');
    // Rows, ledger and the owner marker all survive.
    expect(await rowsInLake(ctx, 'tmp_a7f3', 'tmp-run')).toBe(1);
    expect(await ctx.materialization.listForRun('tmp-run')).toHaveLength(1);
    expect(await ctx.store.get(datasetOwnerKey('tmp_a7f3'))).toBeTruthy();
  });

  // A temp a Run is STILL WRITING to is doubly kept — temporary AND open — and never collected
  // regardless of age. This is the "must never be collected" the critical interaction names.
  it('never collects a temp a run is still appending to', async () => {
    await seedRun(ctx, { runId: 'live-tmp', actor: 'tmp_live', version: '0.1.0', runStartedAt: NOW - 900 * HOUR, state: 'open', owner: 'live-tmp' });
    const report = await sweepDatasets(deps(ctx), { now: NOW, dryRun: false });
    expect(report.collected).toHaveLength(0);
    expect(await rowsInLake(ctx, 'tmp_live', 'live-tmp')).toBe(1);
  });

  // fix #2: the clock is LAST WRITE, not creation. A run STARTED long ago but whose data was just
  // written (its catalog commit is recent) is kept — a creation clock would have collected it.
  it('measures age from last write, not from run creation', async () => {
    // runStartedAt is 300h in the past, but writeDatasetParquet commits the file at real now, so the
    // catalog's last-write time is ~now. Sweeping at real now must KEEP it.
    await seedRun(ctx, { runId: 'old-start', actor: 'freshwrite', version: '0.1.0', runStartedAt: NOW - 300 * HOUR, state: 'sealed' });
    const report = await sweepDatasets(deps(ctx), { now: Date.now(), dryRun: false });
    expect(report.collected.map((c) => c.runId)).not.toContain('old-start');
    expect(report.kept.find((k) => k.runId === 'old-start')?.disposition).toBe('kept-fresh');
    expect(await rowsInLake(ctx, 'freshwrite', 'old-start')).toBe(1);
  });

  /**
   * TAGGING A TEMP FROM THE FRONTEND, END TO END — what the tag means for what survives.
   *
   * One Run writes a temp (`tmp_stage`) and the durable Dataset it promotes into (`lame_demo`). The
   * operator tags THE TEMP, which is the act the Datasets page offers. Then:
   *
   *   1. While the temp exists the Run is kept for being TEMPORARY — the tag is not what saves it,
   *      and the report says so, so nobody reads "tagged" as the reason a temp is still here.
   *   2. The explicit delete STILL REMOVES the tagged temp. ADR 0028's ownership is not overridden
   *      by a label; a tag is a keep-mark, not a lock. (The page's confirmation names the tags for
   *      exactly this reason — `deletionConfirm`.)
   *   3. The tag then keeps the Run's DURABLE output past TTL + grace, because the record is keyed
   *      by the Run and not by the table. THIS is what tagging a temp buys, and it is the sensible
   *      reading of "keep this": the staging table goes when triage is done, the answer stays.
   *   4. The counterfactual, in the same test so it cannot silently stop being true: with the tag
   *      removed, the same durable output collects.
   */
  it('a tag on a TEMP keeps the Run’s durable output, and never blocks deleting the temp', async () => {
    const runStartedAt = NOW - 100 * HOUR;
    await seedRun(ctx, { runId: 'nscheck-1', actor: 'tmp_stage', version: '0.1.0', runStartedAt, state: 'sealed', owner: 'nscheck-1' });
    await seedRun(ctx, { runId: 'nscheck-1', actor: 'lame_demo', version: '0.1.0', runStartedAt, state: 'sealed' });
    // The operator tags the TEMP from the page. The route writes the record keyed by its owner Run.
    await ctx.records.addTag('nscheck-1', 'keep-this');
    // Aged past TTL + grace from the catalog's real commit time, so age is not what keeps anything.
    const aged = Date.now() + DATASET_RETENTION_TTL_MS + DATASET_RETENTION_GRACE_MS + HOUR;

    const before = await sweepDatasets(deps(ctx), { now: aged, dryRun: false });
    expect(before.collected).toHaveLength(0);
    expect(before.kept.find((k) => k.runId === 'nscheck-1')?.disposition).toBe('kept-temporary');

    // The tagged temp is still explicitly deletable — a tag marks, it does not lock.
    const freed = await deleteTemporaryDataset(ctx.store, 'tmp_stage', ctx.cfg);
    expect(freed).toMatchObject({ name: 'tmp_stage', owner: 'nscheck-1' });
    expect(await listDatasets(ctx.store, { name: 'tmp_stage' }, ctx.cfg)).toEqual([]);
    // The record is untouched by the drop: the tag was the RUN's, and the Run still has output.
    expect((await ctx.records.get('nscheck-1'))?.tags).toEqual(['keep-this']);

    // Now the tag is doing the work the operator meant it to do.
    const after = await sweepDatasets(deps(ctx), { now: aged, dryRun: false });
    expect(after.collected).toHaveLength(0);
    expect(after.kept.find((k) => k.runId === 'nscheck-1')?.disposition).toBe('kept-tagged');
    expect(await rowsInLake(ctx, 'lame_demo', 'nscheck-1')).toBe(1);

    // Untag, and the same Dataset collects — proving the tag, not the age, was what kept it.
    await ctx.records.removeTag('nscheck-1', 'keep-this');
    const untagged = await sweepDatasets(deps(ctx), { now: aged, dryRun: false });
    expect(untagged.collected.map((c) => c.runId)).toEqual(['nscheck-1']);
    expect(await rowsInLake(ctx, 'lame_demo', 'nscheck-1')).toBe(0);
  });

  // The record store is load-bearing, not best-effort: if it cannot be read the sweep ABORTS rather
  // than treat every Dataset as untagged and collect kept output (fix #1).
  it('aborts rather than sweep blind when the record store cannot be read', async () => {
    await seedRun(ctx, { runId: 'r', actor: 'aa', version: '0.1.0', runStartedAt: NOW - 100 * HOUR, state: 'sealed' });
    const broken = {
      ...deps(ctx),
      records: {
        list: async () => {
          throw new Error('record store down');
        },
        purgeRun: async () => 0,
      },
    };
    await expect(sweepDatasets(broken, { now: NOW, dryRun: false })).rejects.toThrow('record store down');
    // Nothing was collected — the run's rows are untouched.
    expect(await rowsInLake(ctx, 'aa', 'r')).toBe(1);
  });
});

// --- a partition SEVERAL Runs wrote --------------------------------------------------------------

/**
 * THE TTL'S BIGGEST HOLE, and it pointed at the biggest Datasets.
 *
 * A **Dataset** accrues: several **Runs** append to one `(actor, version, dt)` partition, and the
 * longest-lived output on a box is exactly the output the most Runs wrote. The sweep used to key its
 * candidates on `DatasetInfo.runId`, the listing's SINGULAR field — which is deliberately absent the
 * moment two Runs share a partition — so those Datasets were exempt from the policy that exists for
 * them, in both directions at once (reproduced by hand:
 * `.scratch/post-merge-review/TRIAGE-2026-08-25.md` §2):
 *
 *   - No ledger rows (the v2 shape): the partition resolved to NO Run, so it appeared in neither
 *     `collected` nor `kept` — untouchable AND invisible in the report built to show blast radius.
 *   - Ledger rows for both (the v1 shape): the partition resolved to ONE ARBITRARY contributor, so a
 *     real sweep deleted under that Run alone and reported the Dataset collected while the other
 *     Run's rows and ledger row stayed behind — and a tag on the arbitrary winner shielded rows
 *     nobody had tagged.
 *
 * The fix keys a candidate on EVERY contributing Run. These tests are the proof, over the real lake.
 */
describe('sweepDatasets — a partition several Runs wrote', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });
  afterEach(async () => {
    await ctx.materialization.close().catch(() => undefined);
    await ctx.records.close().catch(() => undefined);
    await ctx.summaries.close().catch(() => undefined);
  });

  /** The lake's files are committed NOW, whatever `runStartedAt` says, so a clock past TTL + grace
   *  from the REAL commit is what makes an aged Dataset a candidate. */
  const aged = (): number => Date.now() + DATASET_RETENTION_TTL_MS + DATASET_RETENTION_GRACE_MS + HOUR;

  /** The claim, exactly: two v2 Runs in one partition, no ledger. Every one of them is weighed. */
  it('weighs EVERY contributing Run when the ledger never saw them', async () => {
    const runStartedAt = NOW - 100 * HOUR;
    await seedRun(ctx, { runId: 'v2-a', actor: 'shared', version: '0.1.0', runStartedAt, ledger: false, state: 'sealed' });
    await seedRun(ctx, { runId: 'v2-b', actor: 'shared', version: '0.1.0', runStartedAt, ledger: false, state: 'sealed' });
    await seedRun(ctx, { runId: 'v2-solo', actor: 'alone', version: '0.1.0', runStartedAt, ledger: false, state: 'sealed' });

    const report = await sweepDatasets(deps(ctx), { now: aged(), dryRun: false });

    // Before the fix this was `['v2-solo']` — the shared partition was in neither list.
    expect([...report.collected, ...report.kept].map((d) => d.runId).sort()).toEqual([
      'v2-a',
      'v2-b',
      'v2-solo',
    ]);
    expect(report.collected.map((d) => d.runId).sort()).toEqual(['v2-a', 'v2-b', 'v2-solo']);
    expect(await rowsInLake(ctx, 'shared', 'v2-a')).toBe(0);
    expect(await rowsInLake(ctx, 'shared', 'v2-b')).toBe(0);
    // And the report says its row counts are a bound, because the catalog counts per partition.
    expect(report.collected.find((d) => d.runId === 'v2-a')?.shared).toBe(true);
    expect(report.collected.find((d) => d.runId === 'v2-solo')?.shared).toBeUndefined();
  });

  /**
   * THE SAFETY REQUIREMENT, on the case that used to break it: a TAGGED Dataset is never collected,
   * and a tag on ONE contributor keeps that contributor's rows and nobody else's.
   *
   * Both directions were wrong before: the tagged Run's rows were deleted when attribution picked the
   * other one, and the untagged Run's rows were spared when attribution picked the tagged one.
   */
  it('never collects a TAGGED contributor, and never spares an untagged one beside it', async () => {
    const runStartedAt = NOW - 100 * HOUR;
    await seedRun(ctx, { runId: 'multi-a', actor: 'shared', version: '0.1.0', runStartedAt, state: 'sealed' });
    await seedRun(ctx, { runId: 'multi-b', actor: 'shared', version: '0.1.0', runStartedAt, state: 'sealed' });
    // Tagged long after both Runs closed — the retroactive case the record exists for.
    await ctx.records.addTag('multi-a', 'keep-this');

    const report = await sweepDatasets(deps(ctx), { now: aged(), dryRun: false });

    expect(report.collected.map((d) => d.runId)).toEqual(['multi-b']);
    expect(report.kept.find((d) => d.runId === 'multi-a')?.disposition).toBe('kept-tagged');
    // The tag kept exactly its own Run's rows.
    expect(await rowsInLake(ctx, 'shared', 'multi-a')).toBe(1);
    expect(await rowsInLake(ctx, 'shared', 'multi-b')).toBe(0);
    // And the ledger row of the Run that WAS collected went with it — the arm that used to be left
    // behind forever when attribution picked the other contributor.
    expect(await ctx.materialization.listDispatches({})).toHaveLength(1);
  });

  /** With the ledger knowing both, a sweep that collects must collect BOTH — not one arbitrarily. */
  it('collects every contributor when none is tagged, rather than one arbitrary winner', async () => {
    const runStartedAt = NOW - 100 * HOUR;
    await seedRun(ctx, { runId: 'multi-a', actor: 'shared', version: '0.1.0', runStartedAt, state: 'sealed' });
    await seedRun(ctx, { runId: 'multi-b', actor: 'shared', version: '0.1.0', runStartedAt, state: 'sealed' });

    const report = await sweepDatasets(deps(ctx), { now: aged(), dryRun: false });

    expect(report.collected.map((d) => d.runId).sort()).toEqual(['multi-a', 'multi-b']);
    expect(await rowsInLake(ctx, 'shared', 'multi-a')).toBe(0);
    expect(await rowsInLake(ctx, 'shared', 'multi-b')).toBe(0);
    expect(await ctx.materialization.listDispatches({})).toHaveLength(0);
  });

  /** A dry run still weighs them — the preview must show the blast radius it used to hide. */
  it('a dry run reports the shared partition and deletes nothing', async () => {
    const runStartedAt = NOW - 100 * HOUR;
    await seedRun(ctx, { runId: 'v2-a', actor: 'shared', version: '0.1.0', runStartedAt, ledger: false, state: 'sealed' });
    await seedRun(ctx, { runId: 'v2-b', actor: 'shared', version: '0.1.0', runStartedAt, ledger: false, state: 'sealed' });

    const dry = await sweepDatasets(deps(ctx), { now: aged() });

    expect(dry.dryRun).toBe(true);
    expect(dry.collected.map((d) => d.runId).sort()).toEqual(['v2-a', 'v2-b']);
    expect(dry.purgedRuns).toBe(0);
    expect(await rowsInLake(ctx, 'shared', 'v2-a')).toBe(1);
    expect(await rowsInLake(ctx, 'shared', 'v2-b')).toBe(1);
  });
});

// --- the gather heartbeats through all four reads ------------------------------------------------

/**
 * THE GATHER USED TO BEAT ONCE, after the first of four reads (TRIAGE-2026-08-25 §3: one beat at
 * 392 ms, then `listDispatches`, `workflows.list` and `records.list` with nothing between them).
 * Against `heartbeatTimeout: '2 minutes'` and `maximumAttempts: 1` that is terminal — and nothing beat
 * during `listDatasets` either, which on a large catalog is the LONGEST of the four, so a scan over
 * two minutes failed the tick before the first beat ever fired.
 */
describe('gatherCandidates heartbeats through every read', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });
  afterEach(async () => {
    await ctx.materialization.close().catch(() => undefined);
    await ctx.records.close().catch(() => undefined);
    await ctx.summaries.close().catch(() => undefined);
  });

  it('beats at least once per read, not once per gather', async () => {
    await seedRun(ctx, { runId: 'r1', actor: 'nscheck', version: '0.1.0', runStartedAt: NOW - 100 * HOUR, state: 'sealed' });
    const beats: { phase: string }[] = [];

    await sweepDatasets(deps(ctx), { now: NOW, dryRun: true, heartbeat: (p) => beats.push(p) });

    // Four reads: the catalog, the ledger, the identities, the records. Today this was 1.
    expect(beats.filter((b) => b.phase === 'gather').length).toBeGreaterThanOrEqual(4);
  });

  /** A read that outlives the heartbeat interval keeps beating — the half a per-read beat alone does
   *  not fix, and the half that actually kills a tick on a big catalog. */
  it('keeps beating while a single read is still running', async () => {
    const beats: unknown[] = [];
    const slow = <T,>(v: T): Promise<T> => new Promise((r) => setTimeout(() => r(v), 120));
    const deadDeps = {
      ...deps(ctx),
      materialization: {
        listDispatches: () => slow([]),
        purgeRun: async () => 0,
      },
    };

    await sweepDatasets(deadDeps, {
      now: NOW,
      dryRun: true,
      heartbeat: (p) => beats.push(p),
      heartbeatIntervalMs: 20,
    });

    // The ledger read alone spans six intervals; without the keepalive the whole gather beat 4 times.
    expect(beats.length).toBeGreaterThan(6);
  });

  it('a throwing heartbeat never fails the sweep — liveness is not correctness', async () => {
    await seedRun(ctx, { runId: 'r1', actor: 'nscheck', version: '0.1.0', runStartedAt: NOW - 100 * HOUR, state: 'sealed' });
    const report = await sweepDatasets(deps(ctx), {
      now: NOW,
      dryRun: true,
      heartbeat: () => {
        throw new Error('activity context is gone');
      },
    });
    expect(report.scanned).toBe(1);
  });
});

// --- one tick's history footprint ----------------------------------------------------------------

/**
 * A TICK'S COST IN TEMPORAL HISTORY MUST NOT BE THE CATALOG'S SIZE.
 *
 * The sweep report holds one decision per **Run** in the lake and used to be returned as BOTH the
 * activity result and the workflow result — so an hourly schedule wrote the whole catalog into
 * history twice per tick, against a wall this repo has measured (`GrpcMessageTooLarge` at ~8k
 * references). {@link summarizeSweep} is what the durable path returns instead, and this is the
 * property that makes that true rather than intended.
 */
describe('summarizeSweep — the durable return value', () => {
  const decision = (i: number, rows: number): SweepDecision => ({
    runId: `run-${String(i).padStart(6, '0')}`,
    name: `wf-nscheck-0.1.0--2026-08-18T00-00-00Z--${String(i).padStart(6, '0')}`,
    disposition: i % 2 === 0 ? 'collect' : 'kept-tagged',
    ageMs: 1_000,
    lastWriteAt: NOW - 1_000,
    rows,
  });
  /** `rows` is FIXED by default so two reports of different sizes differ by nothing but their counts
   *  — the footprint test would otherwise measure the width of a row number. */
  const report = (n: number, rows: (i: number) => number = () => 1_000): SweepReport => {
    const all = Array.from({ length: n }, (_, i) => decision(i, rows(i)));
    return {
      scanned: n,
      collected: all.filter((d) => d.disposition === 'collect'),
      kept: all.filter((d) => d.disposition !== 'collect'),
      dryRun: false,
      ttlMs: DATASET_RETENTION_TTL_MS,
      graceMs: DATASET_RETENTION_GRACE_MS,
      purgedRuns: n / 2,
      purgedRows: n,
    };
  };

  it('does not grow with the catalog — 100 Datasets and 10,000 serialize to the same size', () => {
    const small = JSON.stringify(summarizeSweep(report(100)));
    const large = JSON.stringify(summarizeSweep(report(10_000)));
    // A hundredfold more catalog. Only the DIGITS of the counts differ — nothing here is
    // proportional to n, which is the whole property: an hourly tick's history cost is a constant.
    expect(Math.abs(large.length - small.length)).toBeLessThan(64);
    expect(large.length).toBeLessThan(8_192);
    // The full report it came from is not: 10,000 decisions, and it used to be written into history
    // twice per tick (activity result + workflow result).
    expect(JSON.stringify(report(10_000)).length).toBeGreaterThan(1_000_000);
  });

  it('keeps every COUNT in full — only the per-Dataset detail is sampled', () => {
    const s = summarizeSweep(report(10_000));
    expect(s.scanned).toBe(10_000);
    expect(s.counts.collect).toBe(5_000);
    expect(s.counts['kept-tagged']).toBe(5_000);
    expect(s.counts.collect + s.counts['kept-tagged']).toBe(s.scanned);
    expect(s.sample.collected).toHaveLength(SWEEP_SAMPLE_SIZE);
    expect(s.sample.kept).toHaveLength(SWEEP_SAMPLE_SIZE);
    expect(s.truncated).toBe(true);
  });

  it('samples the biggest first — the part of a long list worth carrying', () => {
    const s = summarizeSweep(report(100, (i) => i));
    expect(s.sample.collected[0]?.rows).toBe(98);
    expect(s.sample.collected.at(-1)?.rows).toBe(60);
  });

  it('says so when nothing was truncated, so a short list is not mistaken for a sample', () => {
    const s = summarizeSweep(report(4));
    expect(s.truncated).toBe(false);
    expect(s.sample.collected).toHaveLength(2);
    expect(s.counts.collect).toBe(2);
  });

  /** The whole sweep, end to end, summarized — the shape a schedule tick actually returns. */
  it('summarizes a real sweep without losing the mode or the constants', async () => {
    resetLakeConnections();
    const c = lake();
    try {
      await seedRun(c, { runId: 'r1', actor: 'nscheck', version: '0.1.0', runStartedAt: NOW - 100 * HOUR, state: 'sealed' });
      const s = summarizeSweep(
        await sweepDatasets(deps(c), {
          now: Date.now() + DATASET_RETENTION_TTL_MS + DATASET_RETENTION_GRACE_MS + HOUR,
          dryRun: true,
        })
      );
      expect(s).toMatchObject({ scanned: 1, dryRun: true, purgedRuns: 0, truncated: false });
      expect(s.counts.collect).toBe(1);
      expect(s.sample.collected[0]?.runId).toBe('r1');
    } finally {
      await c.materialization.close().catch(() => undefined);
      await c.records.close().catch(() => undefined);
      await c.summaries.close().catch(() => undefined);
    }
  });
});
