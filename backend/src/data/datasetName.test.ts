/**
 * The DERIVED Dataset name (ADR 0029 §2) — the one string, and the join that attaches it.
 *
 * The name is a pure rendering of ledger facts, so it is pinned here without a lake: the exact
 * format (the `Z`, the `--` separators, the 6-char run-id fragment), the property that two Runs of
 * one workflow in the same second still get different names, and that the identity it renders is the
 * producer's manifest name — never a task-queue string a `--queue` could override.
 */

import { describe, expect, it } from 'vitest';

import type { DatasetInfo } from './datasets';
import { datasetRunIds, withDatasetDeviations, withDatasetNames } from './datasets';
import type { DatasetDeviation } from './datasetRecords';
import { RUN_ID_FRAGMENT, datasetName, runFragment } from './datasetName';
import type { DispatchRef } from './materializationStore';
import { dtPartition } from './parquet';

const T = Date.UTC(2026, 7, 19, 14, 32, 7); // 2026-08-19T14:32:07Z

describe('datasetName — the one string', () => {
  it('pins the exact format, including the Z and the 6-char run-id fragment', () => {
    // A REAL run id. Both start paths mint `<type>-<unixseconds>` (workflowControl.ts behind
    // POST /api/runs, and cli/workflow.go), and the ledger's runId is that workflow id — so a
    // test that feeds a UUID here is testing a shape the system never produces.
    const name = datasetName({
      workflow: 'nscheck',
      version: '0.1.0',
      runStartedAt: T,
      runId: 'nscheck-1755612727',
    });
    // The literal spelling of ADR 0029 §2. If a refactor changes any separator, the `Z`, or the
    // fragment length, this is the line that fails — which is the point of pinning it whole.
    expect(name).toBe('wf-nscheck-0.1.0--2026-08-19T14-32-07Z--2faa3d');
    expect(name.endsWith(`--${runFragment('nscheck-1755612727')}`)).toBe(true);
    // The datetime is UTC and colons are dashed — the same `dt=` partition render, with a `Z`.
    expect(name).toContain(`--${dtPartition(T)}Z--`);
  });

  it('does not put the run id\'s PREFIX in the name — that carries no run entropy', () => {
    // The regression this pins. ADR 0029 §2 wrote the fragment as `runId[:6]`, and under the id
    // scheme this repo actually mints that is the workflow NAME's prefix: every nscheck run ever
    // would render `--nschec`. Two consecutive runs, one second apart, are the minimal proof.
    const one = 'nscheck-1755612727';
    const two = 'nscheck-1755612728';
    expect(one.slice(0, RUN_ID_FRAGMENT)).toBe(two.slice(0, RUN_ID_FRAGMENT)); // 'nschec' — the bug
    expect(runFragment(one)).not.toBe(runFragment(two));                       // the fix
    const name = datasetName({ workflow: 'nscheck', version: '0.1.0', runStartedAt: T, runId: one });
    const fragment = name.slice(name.lastIndexOf('--') + 2);
    expect(fragment).not.toBe(one.slice(0, RUN_ID_FRAGMENT));
    expect(fragment).toBe(runFragment(one));
  });

  it('gives two Runs of the same workflow in the same second different names', () => {
    // Same workflow, same version, SAME second — the run-starts differ only by milliseconds,
    // which `dtPartition` collapses, so the datetime portion is identical and the fragment is the
    // only thing left to tell them apart. Two ids in one second means an explicit `--id` (the
    // default `<type>-<unixseconds>` scheme cannot mint two, and Temporal would reject the
    // duplicate WorkflowId), so these are UUID-shaped on purpose.
    const a = datasetName({ workflow: 'nscheck', version: '0.1.0', runStartedAt: T + 120, runId: 'a3f9c1e2-7b04-4a1d-9c88-0f21e6b3d5aa' });
    const b = datasetName({ workflow: 'nscheck', version: '0.1.0', runStartedAt: T + 880, runId: 'a3f9c1e2-7b04-4a1d-9c88-0f21e6b3d5ab' });

    expect(a).toContain(`${dtPartition(T)}Z`);
    expect(b).toContain(`${dtPartition(T)}Z`);
    // Note these two ids share their first 35 characters and differ in the LAST one. A prefix
    // fragment would have collided here too; a digest does not.
    expect(a).not.toBe(b);
    // Everything left of the fragment is identical; only the fragment differs.
    const stem = (s: string): string => s.slice(0, s.lastIndexOf('--'));
    expect(stem(a)).toBe(stem(b));
  });

  it('renders the manifest identity it is handed, with no way to reach a task-queue string', () => {
    // ADR 0029 §2: `--queue` overrides the transport, so the name must derive from the manifest, not
    // the queue. The function takes `workflow` explicitly and has NO queue parameter — the queue
    // cannot leak in even by accident. A run served on an overridden queue like
    // `wf-nscheck-3af91c2b` still names `wf-nscheck-0.1.0--…`.
    const name = datasetName({ workflow: 'nscheck', version: '0.1.0', runStartedAt: T, runId: 'abcdef012345' });
    expect(name.startsWith('wf-nscheck-0.1.0--')).toBe(true);
    expect(name).not.toContain('3af91c2b');
  });
});

describe('withDatasetNames — the ledger join', () => {
  const dispatch = (o: Partial<DispatchRef> = {}): DispatchRef => ({
    actor: 'echo',
    version: '0.1.0',
    runId: 'run-abc123-xyz',
    runStartedAt: T,
    dt: dtPartition(T),
    nodes: 1,
    rows: 1,
    state: 'complete',
    ...o,
  });
  const info = (o: Partial<DatasetInfo> = {}): DatasetInfo => ({
    kind: 'output',
    name: 'echo',
    version: '0.1.0',
    dt: dtPartition(T),
    rows: 1,
    bytes: 0,
    ...o,
  });

  it('names an output row from the ledger Run that wrote its partition, falling back to the Actor', () => {
    // No recorded caller identity — the pre-0029-§2-fix world and every Dataset that predates the
    // identity store. The row still gets a name, rendered from the producing Actor.
    const [out] = withDatasetNames([info()], [dispatch()]);
    expect(out!.datasetName).toBe(`wf-echo-0.1.0--${dtPartition(T)}Z--${runFragment('run-abc123-xyz')}`);
  });

  it('renders the CALLER WORKFLOW, not the Actor, when the Run\'s identity was recorded', () => {
    // THE DEFECT THIS FIXES. ADR 0029 §2 derives the name from the caller WORKFLOW's manifest; the
    // first implementation passed the producing DATASET's stamped identity, so a workflow `foo`
    // dispatching an actor `bar` named its output `wf-bar-…` — the wrong thing entirely.
    const [out] = withDatasetNames(
      [info({ name: 'bar', version: '3.0.0' })],
      [dispatch({ actor: 'bar', version: '3.0.0', runId: 'foo-1755612727' })],
      [{ runId: 'foo-1755612727', workflow: 'foo', version: '1.2.0' }]
    );
    expect(out!.datasetName).toBe(`wf-foo-1.2.0--${dtPartition(T)}Z--${runFragment('foo-1755612727')}`);
    // Neither half of the ACTOR's identity survives into the name — not the name, not the version.
    expect(out!.datasetName).not.toContain('bar');
    expect(out!.datasetName).not.toContain('3.0.0');
  });

  it('gives ONE Run writing TWO actor tables ONE name — the Consequence §2 states', () => {
    // "the name labels the run's output, which may span several actor tables". Under an Actor-grain
    // identity this is structurally impossible: two tables, two names, one Run. Here one Run fans out
    // to `bar` and `baz` in the same second and both rows carry the same string.
    const RUN = 'foo-1755612727';
    const out = withDatasetNames(
      [info({ name: 'bar', version: '3.0.0' }), info({ name: 'baz', version: '0.9.1' })],
      [
        dispatch({ actor: 'bar', version: '3.0.0', runId: RUN }),
        dispatch({ actor: 'baz', version: '0.9.1', runId: RUN }),
      ],
      [{ runId: RUN, workflow: 'foo', version: '1.2.0' }]
    );
    expect(out[0]!.datasetName).toBe(`wf-foo-1.2.0--${dtPartition(T)}Z--${runFragment(RUN)}`);
    expect(out[1]!.datasetName).toBe(out[0]!.datasetName);
    // The rows still address their own physical tables — the name is a LABEL over run-grain output,
    // never a rename of the storage identity (ADR 0029 Consequences).
    expect([out[0]!.name, out[1]!.name]).toEqual(['bar', 'baz']);
  });

  it('falls back per-Run, so an unstamped Run beside a stamped one still gets a name', () => {
    // The mixed state a real lake is in the day this ships: old Datasets have no identity row, new
    // ones do. Neither is dropped and neither is blank.
    const out = withDatasetNames(
      [info({ name: 'bar', version: '3.0.0' }), info({ name: 'baz', version: '0.9.1' })],
      [
        dispatch({ actor: 'bar', version: '3.0.0', runId: 'foo-1755612727' }),
        dispatch({ actor: 'baz', version: '0.9.1', runId: 'legacy-1600000000' }),
      ],
      [{ runId: 'foo-1755612727', workflow: 'foo', version: '1.2.0' }]
    );
    expect(out[0]!.datasetName).toBe(`wf-foo-1.2.0--${dtPartition(T)}Z--${runFragment('foo-1755612727')}`);
    expect(out[1]!.datasetName).toBe(
      `wf-baz-0.9.1--${dtPartition(T)}Z--${runFragment('legacy-1600000000')}`
    );
  });

  it('never pairs a workflow name with an actor version — the identity is taken whole', () => {
    // A `workflow ?? actorName` and a separate `version ?? actorVersion` would render
    // `wf-foo-3.0.0--…`: a manifest identity that never existed. The two halves come from one source
    // or the other, together.
    const stamped = withDatasetNames(
      [info({ name: 'bar', version: '3.0.0' })],
      [dispatch({ actor: 'bar', version: '3.0.0', runId: 'foo-1' })],
      [{ runId: 'foo-1', workflow: 'foo', version: '1.2.0' }]
    );
    expect(stamped[0]!.datasetName).toContain('wf-foo-1.2.0--');
    const unstamped = withDatasetNames(
      [info({ name: 'bar', version: '3.0.0' })],
      [dispatch({ actor: 'bar', version: '3.0.0', runId: 'foo-1' })]
    );
    expect(unstamped[0]!.datasetName).toContain('wf-bar-3.0.0--');
  });

  it('the run-id fragment stays a DIGEST under the workflow identity too', () => {
    // The regression guard from the `runId[:6]` fix, re-pinned on the path that now renders the name:
    // two Runs of one WORKFLOW one second apart share every component but the fragment.
    const a = withDatasetNames(
      [info({ name: 'bar', version: '3.0.0' })],
      [dispatch({ actor: 'bar', version: '3.0.0', runId: 'foo-1755612727' })],
      [{ runId: 'foo-1755612727', workflow: 'foo', version: '1.2.0' }]
    )[0]!.datasetName!;
    const b = withDatasetNames(
      [info({ name: 'bar', version: '3.0.0' })],
      [dispatch({ actor: 'bar', version: '3.0.0', runId: 'foo-1755612728' })],
      [{ runId: 'foo-1755612728', workflow: 'foo', version: '1.2.0' }]
    )[0]!.datasetName!;
    expect(a).not.toBe(b);
    expect(a.endsWith(`--${runFragment('foo-1755612727')}`)).toBe(true);
    expect(a.slice(a.lastIndexOf('--') + 2)).not.toBe('foo-17'); // the prefix that carries no entropy
  });

  it('derives from the recorded manifest identity, so a --queue override cannot change the name', () => {
    // The join reads the Run id from the ledger and the name/version from the identity record. A
    // DispatchRef carries no queue and a RunWorkflow carries no queue, so no queue string can reach
    // the name from either side — the wiring-level proof of the manifest-not-queue rule.
    const queueish = 'wf-nscheck-3af91c2b'; // what `--queue` used to be able to say
    const [out] = withDatasetNames(
      [info()],
      [dispatch({ runId: 'nscheck-1755612727' })],
      [{ runId: 'nscheck-1755612727', workflow: 'nscheck', version: '0.1.0' }]
    );
    expect(out!.datasetName).toBe(
      `wf-nscheck-0.1.0--${dtPartition(T)}Z--${runFragment('nscheck-1755612727')}`
    );
    expect(out!.datasetName).not.toContain(queueish);
    expect(out!.datasetName).not.toContain('3af91c2b');
  });

  it('ignores an identity for a Run this page did not resolve', () => {
    // The identities arrive as a page keyed by runId; one for a different Run must not attach itself
    // to a row it has nothing to do with.
    const [out] = withDatasetNames(
      [info()],
      [dispatch()],
      [{ runId: 'somebody-else', workflow: 'foo', version: '1.2.0' }]
    );
    expect(out!.datasetName).toBe(`wf-echo-0.1.0--${dtPartition(T)}Z--${runFragment('run-abc123-xyz')}`);
  });

  it('leaves a standalone list and a version-less row unnamed', () => {
    const standalone = info({ kind: 'standalone', version: undefined, dt: undefined });
    const versionless = info({ version: undefined });
    const out = withDatasetNames([standalone, versionless], [dispatch()]);
    expect(out[0]!.datasetName).toBeUndefined();
    expect(out[1]!.datasetName).toBeUndefined();
  });

  it('leaves a partition unnamed when NOTHING can say which Run wrote it', () => {
    // No matching DispatchRef, no owner marker, no `run_id` statistics: three authorities and none
    // of them answers, so the row is returned as it arrived rather than guessed.
    const [out] = withDatasetNames([info({ name: 'lame' })], [dispatch()]);
    expect(out!.datasetName).toBeUndefined();
    expect(out!.runId).toBeUndefined();
  });

  it('names a promoted durable Dataset from the ROWS, which the ledger never saw', () => {
    // THE DEFECT THIS FIXES, measured on the local controller: `lame_demo` held 430 rows promoted
    // out of one Run's temp and listed with `runId=None, datasetName=None`, because promotion
    // copies rows and publishes no ledger record — and because the actorkit path writes no ledger
    // record for ANY Run (50 dispatches in the ledger, not one of them a v2 Run). The rows carried
    // `run_id` the whole time, and the catalog's per-file statistics surface it for free.
    const [out] = withDatasetNames(
      [info({ name: 'lame_demo', contributingRuns: ['nscheck-1787150959'] })],
      [],
      [{ runId: 'nscheck-1787150959', workflow: 'nscheck', version: '0.1.0' }]
    );
    expect(out!.runId).toBe('nscheck-1787150959');
    expect(out!.datasetName).toBe(
      `wf-nscheck-0.1.0--${dtPartition(T)}Z--${runFragment('nscheck-1787150959')}`
    );
  });

  it('names a temporary Dataset from its OWNER — the fact recorded before any row lands', () => {
    // A temp's table matches no DispatchRef (it is named after the temp, not the Actor), so it
    // listed unnamed too. Its owner IS a runId, and the workflow identity is a direct lookup.
    const [out] = withDatasetNames(
      [info({ name: 'tmp_nscheck-1787150959_4e9b1b23', temporary: true, owner: 'nscheck-1787150959' })],
      [],
      [{ runId: 'nscheck-1787150959', workflow: 'nscheck', version: '0.1.0' }]
    );
    expect(out!.runId).toBe('nscheck-1787150959');
    expect(out!.datasetName).toBe(
      `wf-nscheck-0.1.0--${dtPartition(T)}Z--${runFragment('nscheck-1787150959')}`
    );
  });

  it('gives a temp and the Dataset it was promoted into THE SAME name — one Run, one name', () => {
    // ADR 0029's Consequence: the name labels the RUN's output, which may span several tables.
    // A temp and its promotion target are two tables of one Run, so they read as one thing.
    const identities = [{ runId: 'nscheck-1787150959', workflow: 'nscheck', version: '0.1.0' }];
    const out = withDatasetNames(
      [
        info({ name: 'tmp_nscheck-1787150959_4e9b1b23', owner: 'nscheck-1787150959' }),
        info({ name: 'lame_demo', contributingRuns: ['nscheck-1787150959'] }),
      ],
      [],
      identities
    );
    expect(out[0]!.datasetName).toBe(out[1]!.datasetName);
  });

  it('REFUSES the Actor fallback for a Run the LEDGER did not resolve', () => {
    // The fallback renders `info.name` as a workflow identity, and that is only true where a
    // DispatchRef matched — there it IS the producing Actor's table. On a temp it is the temp's
    // storage name and on a promoted Dataset it is the caller's own name, so `wf-tmp_nscheck-…`
    // would assert a workflow that never existed. The runId still travels, which is the
    // traceability that matters, and the name appears as soon as the Run's identity is recorded.
    const [temp] = withDatasetNames([info({ name: 'tmp_nscheck-1_ab', owner: 'nscheck-1' })], []);
    expect(temp!.runId).toBe('nscheck-1');
    expect(temp!.datasetName).toBeUndefined();

    const [promoted] = withDatasetNames([info({ name: 'lame_demo', contributingRuns: ['nscheck-1'] })], []);
    expect(promoted!.runId).toBe('nscheck-1');
    expect(promoted!.datasetName).toBeUndefined();

    // The ledger's own row keeps the fallback it has always had.
    const [dispatched] = withDatasetNames([info()], [dispatch()]);
    expect(dispatched!.datasetName).toBe(
      `wf-echo-0.1.0--${dtPartition(T)}Z--${runFragment('run-abc123-xyz')}`
    );
  });

  it('REFUSES a single runId for a partition several Runs contributed to', () => {
    // The lie this design makes unreachable: `lame` accumulates from many Runs, so one `runId`
    // over it would be false the second time anything promoted in. The plural fact stays on the
    // row and the singular one is simply absent — no name either, since a name is run-grain.
    const [out] = withDatasetNames(
      [info({ name: 'lame', contributingRuns: ['nscheck-1', 'nscheck-2'] })],
      [],
      [{ runId: 'nscheck-1', workflow: 'nscheck', version: '0.1.0' }]
    );
    expect(out!.runId).toBeUndefined();
    expect(out!.datasetName).toBeUndefined();
    expect(out!.contributingRuns).toEqual(['nscheck-1', 'nscheck-2']);
  });

  it('REFUSES a single runId when a data file spans Runs, however few it can name', () => {
    // A file's statistics are a min and a max, so one Run between them is invisible. A bound is
    // not a set, and the singular field is refused rather than filled from a bound.
    const [out] = withDatasetNames(
      [info({ name: 'lame', contributingRuns: ['nscheck-1'], contributingRunsPartial: true })],
      []
    );
    expect(out!.runId).toBeUndefined();
    expect(out!.datasetName).toBeUndefined();
  });

  it('prefers the LEDGER over the rows, and the OWNER over the rows', () => {
    // Order matters where they can disagree: the ledger resolves an Actor's output table (which
    // the rows' statistics also cover), and the owner marker answers for an EMPTY temp, which no
    // row statistic can. A temp with no rows yet still says whose it is.
    const [ledgerWins] = withDatasetNames(
      [info({ contributingRuns: ['from-the-rows'] })],
      [dispatch({ runId: 'from-the-ledger' })]
    );
    expect(ledgerWins!.runId).toBe('from-the-ledger');

    const [emptyTemp] = withDatasetNames([info({ owner: 'nscheck-1', temporary: true })], []);
    expect(emptyTemp!.runId).toBe('nscheck-1');
  });

  it('names the newest Run when two share one partition', () => {
    // Same Actor version started twice in one second folds into one listing row; the newest Run wins
    // the name, and the run-id fragment is what still tells the two apart in the string.
    const [out] = withDatasetNames(
      [info()],
      [
        dispatch({ runId: 'older0-run', runStartedAt: T + 100 }),
        dispatch({ runId: 'newer0-run', runStartedAt: T + 900 }),
      ]
    );
    expect(out!.datasetName).toBe(`wf-echo-0.1.0--${dtPartition(T)}Z--${runFragment('newer0-run')}`);
  });

  it('stamps the resolved runId on the row — the key a tag/rename addresses', () => {
    // ADR 0029 §4: the record is keyed by runId. The listing carries the full id, not only its
    // 6-char fragment inside the name, so a surface holding a row can mutate the record directly.
    const [out] = withDatasetNames([info()], [dispatch({ runId: 'run-abc123-xyz' })]);
    expect(out!.runId).toBe('run-abc123-xyz');
    // Absent exactly where the name is: a standalone list resolves to no Run.
    const [standalone] = withDatasetNames(
      [info({ kind: 'standalone', version: undefined, dt: undefined })],
      [dispatch()]
    );
    expect(standalone!.runId).toBeUndefined();
  });
});

describe('datasetRunIds — what to ask the identity store for', () => {
  const info = (o: Partial<DatasetInfo> = {}): DatasetInfo => ({
    kind: 'output',
    name: 'echo',
    version: '0.1.0',
    dt: dtPartition(T),
    rows: 1,
    bytes: 0,
    ...o,
  });

  it('unions the ledger\'s Runs with the ones the rows themselves resolve', () => {
    const ids = datasetRunIds(
      [
        info({ name: 'tmp_x', owner: 'nscheck-1' }),
        info({ name: 'lame_demo', contributingRuns: ['nscheck-2'] }),
        info({ name: 'lame', contributingRuns: ['nscheck-3', 'nscheck-4'] }),
      ],
      [
        {
          actor: 'echo',
          version: '0.1.0',
          runId: 'legacy-run',
          runStartedAt: T,
          dt: dtPartition(T),
          nodes: 1,
          rows: 1,
          state: 'complete',
        },
      ]
    );
    // The ledger's, the temp's owner, and the sole contributor — but NOT the two-Run partition,
    // which resolves to no single Run and so has no identity to render.
    expect([...ids].sort()).toEqual(['legacy-run', 'nscheck-1', 'nscheck-2']);
  });

  it('deduplicates, so one round trip asks for each Run once', () => {
    const ids = datasetRunIds([
      info({ name: 'tmp_x', owner: 'nscheck-1' }),
      info({ name: 'lame_demo', contributingRuns: ['nscheck-1'] }),
    ]);
    expect(ids).toEqual(['nscheck-1']);
  });
});

describe('withDatasetDeviations — the record join', () => {
  const info = (o: Partial<DatasetInfo> = {}): DatasetInfo => ({
    kind: 'output',
    name: 'echo',
    version: '0.1.0',
    dt: dtPartition(T),
    rows: 1,
    bytes: 0,
    runId: 'run-abc123-xyz',
    datasetName: `wf-echo-0.1.0--${dtPartition(T)}Z--${runFragment('run-abc123-xyz')}`,
    ...o,
  });
  const dev = (o: Partial<DatasetDeviation> = {}): DatasetDeviation => ({
    runId: 'run-abc123-xyz',
    tags: [],
    ...o,
  });

  it('attaches a Run\'s tags and rename, keyed by the stamped runId', () => {
    const [out] = withDatasetDeviations(
      [info()],
      [dev({ tags: ['prod', 'nightly'], renamedTo: 'the-sweep' })]
    );
    expect(out!.tags).toEqual(['prod', 'nightly']);
    expect(out!.renamedTo).toBe('the-sweep');
    // The derived name is LEFT beside the rename, never replaced — issue 01's invariant. A surface
    // renders `renamedTo ?? datasetName`, so the default is there whenever no rename exists.
    expect(out!.datasetName).toBe(`wf-echo-0.1.0--${dtPartition(T)}Z--${runFragment('run-abc123-xyz')}`);
  });

  it('attaches only the half that exists — tags without a rename, and the reverse', () => {
    const [tagged] = withDatasetDeviations([info()], [dev({ tags: ['x'] })]);
    expect(tagged!.tags).toEqual(['x']);
    expect(tagged!.renamedTo).toBeUndefined();

    const [renamed] = withDatasetDeviations([info()], [dev({ renamedTo: 'y' })]);
    expect(renamed!.tags).toBeUndefined(); // an empty set is no field, not []
    expect(renamed!.renamedTo).toBe('y');
  });

  it('leaves a row with no deviation exactly as it arrived — untagged, derived name', () => {
    const [out] = withDatasetDeviations([info()], []);
    expect(out!.tags).toBeUndefined();
    expect(out!.renamedTo).toBeUndefined();
    expect(out!.datasetName).toBe(`wf-echo-0.1.0--${dtPartition(T)}Z--${runFragment('run-abc123-xyz')}`);
  });

  /**
   * TAGGING A TEMP WORKS, AND IT IS RUN-GRAIN — both halves matter and the second is the surprise.
   *
   * A temporary Dataset HAS a Run (its owner marker resolves one, `withDatasetNames` above), so the
   * record's key exists and the page's tag affordance is addressable — that is the "does it work"
   * half, verified rather than assumed.
   *
   * What it does NOT do is address one TABLE. The record is keyed by the **Run** (ADR 0029 §4), and
   * a Run's temp and the durable Dataset it promoted into share that key — measured on the live
   * controller, `tmp_4e9b1b23` and `lame_demo` both carry `nscheck-1787150959`. So a tag put on the
   * temp appears on both rows, which is the same run-grain the NAME already has (§2's Consequence:
   * "the name labels the run's output, which may span several actor tables"). The Datasets page says
   * so in the tag prompt rather than letting an operator think they tagged one table.
   */
  it('tags a TEMP and the Dataset its Run promoted into together — the record is Run-grain', () => {
    const temp = info({ name: 'tmp_4e9b1b23', temporary: true, owner: 'nscheck-1', runId: 'nscheck-1' });
    const durable = info({ name: 'lame_demo', runId: 'nscheck-1' });
    const out = withDatasetDeviations([temp, durable], [dev({ runId: 'nscheck-1', tags: ['keep'] })]);
    expect(out[0]!.tags).toEqual(['keep']);
    expect(out[1]!.tags).toEqual(['keep']);
    // And a tag does not make a temp durable: temp-ness is the OWNER MARKER's answer, and nothing
    // in the record store touches it. The badge, the delete route and the sweep all keep reading
    // the marker, so they cannot disagree about what a tagged temp is.
    expect(out[0]!.temporary).toBe(true);
    expect(out[0]!.owner).toBe('nscheck-1');
  });

  it('does not attach to a row with no runId — a standalone list has no record to key on', () => {
    const standalone = info({ kind: 'standalone', version: undefined, dt: undefined, runId: undefined, datasetName: undefined });
    const [out] = withDatasetDeviations([standalone], [dev({ runId: 'whatever', tags: ['x'] })]);
    expect(out!.tags).toBeUndefined();
  });
});
