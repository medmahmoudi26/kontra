/**
 * Reconcile the `open` marker against the Runs that actually wrote a Dataset.
 *
 * ── WHAT IS WRONG, AND WHY IT IS 35 OUT OF 35 RATHER THAN A SCATTERING ──────────────────────────
 *
 * `publishBatch` REWRITES `_state.json` to `open` on every append, and `sealed` is only ever
 * written by an explicit close — WHICH RUN OUTPUT NEVER PERFORMED. `closeDataset`
 * (`activities/datasets.ts`) and `Dataset.seal()` (`sdk/python/kontra/catalog.py`) both exist and
 * nothing in any workflow called either, so `open` is the only value run output has ever written.
 * The uniformity is the tell: a close that FAILS is intermittent, a close that no caller MAKES is
 * total.
 *
 * It survived because the one consumer that would have caught it — the retention sweep — was fixed
 * by ROUTING AROUND the marker rather than making it true. Every Dataset was exempt from collection
 * for ever because `open` never cleared; the fix made `open` a fact about the RUN, read from the
 * ledger. Correct for retention, and it left the marker lying to every other reader.
 *
 * ── TEMPORAL IS THE AUTHORITY, AND THE TERMINAL STATUS CHOOSES THE WORD ─────────────────────────
 *
 * The three states are three different sentences and collapsing them would be the same mistake one
 * layer along:
 *
 *   sealed     — the Run that wrote this FINISHED. Nothing more is coming, and that is the
 *                intended end.
 *   abandoned  — the Run stopped without finishing. Nothing more is coming, and the result is
 *                short of what was asked for.
 *   open       — nobody has said, which is exactly what a crash or a run still in flight leaves.
 *
 * So a COMPLETED Run's output is sealed and a FAILED/TERMINATED/CANCELED/TIMED_OUT Run's output is
 * abandoned. Writing `sealed` for everything would claim a deliberate ending for runs that were
 * killed, which is the more comfortable lie and still a lie.
 *
 * ── ONE MARKER PER LOGICAL DATASET, SO EVERY CONTRIBUTOR HAS TO BE TERMINAL ─────────────────────
 *
 * `datasetStateKey` is `datasets/<name>/_state.json` — ONE object per NAME, not per partition. A
 * name like `canary_signals` is written by many Runs across many dispatches. Sealing it because
 * ONE of them finished would declare a Dataset complete while another Run is still appending to
 * it, which is worse than the `open` it replaces: a false `sealed` tells a reader the data is
 * whole.
 *
 * So a name is only reconciled when EVERY Run that contributed to it is terminal, and it is
 * `abandoned` if ANY of those Runs ended badly — a Dataset half of whose contributors were killed
 * is not a complete result, whichever one finished last.
 *
 * ── IT NEVER INVENTS A RUN, AND IT NEVER GUESSES ────────────────────────────────────────────────
 *
 * A Dataset whose contributing Run is unknown to Temporal (dropped from history, or never recorded)
 * is LEFT ALONE. `open` is then still wrong, but "we cannot tell" is not a licence to write
 * `sealed` — and a dataset nobody can attribute is precisely the one where a wrong claim would
 * never be caught.
 */

import type { DatasetState } from '@kontra/core/contract/datasets';

/** What a Run's execution status is, as far as this reconciliation cares. */
export type RunOutcome = 'running' | 'completed' | 'failed' | 'unknown';

/** One Dataset name and the Runs that wrote into it. */
export interface DatasetContributors {
  name: string;
  /** Current marker, or null when nothing ever wrote one. */
  state: DatasetState | null;
  /** Every Run id the catalog attributes rows of this name to. */
  runIds: readonly string[];
}

export interface SealDecision {
  name: string;
  /** What should be written, or null to leave it alone. */
  to: DatasetState | null;
  /** Why — a sentence, so a dry run reads as a report rather than a diff. */
  why: string;
}

/**
 * Decide, for one Dataset, what its marker should say.
 *
 * PURE. Every input is a plain value and the caller supplies the outcomes, so the rule above can be
 * tested without Temporal, an object store or a lake — which matters because the rule is the whole
 * of this module and the I/O is three lines.
 */
export function decideSeal(
  ds: DatasetContributors,
  outcomeOf: (runId: string) => RunOutcome
): SealDecision {
  if (ds.state !== 'open') {
    return {
      name: ds.name,
      to: null,
      why:
        ds.state === null
          ? 'no writer ever wrote a marker — absent is not the same claim as open, and inventing one would say a writer touched this'
          : `already ${ds.state}`,
    };
  }
  if (ds.runIds.length === 0) {
    return { name: ds.name, to: null, why: 'no contributing Run recorded — nothing to reconcile against' };
  }

  const outcomes = ds.runIds.map((r) => ({ run: r, outcome: outcomeOf(r) }));
  const live = outcomes.filter((o) => o.outcome === 'running');
  if (live.length > 0) {
    return {
      name: ds.name,
      to: null,
      why: `${live.length} contributing Run(s) still running (${live[0]?.run}) — open is TRUE here`,
    };
  }
  const unknown = outcomes.filter((o) => o.outcome === 'unknown');
  if (unknown.length > 0) {
    return {
      name: ds.name,
      to: null,
      why: `${unknown.length} contributing Run(s) unknown to Temporal (${unknown[0]?.run}) — cannot tell, so not claimed`,
    };
  }
  const bad = outcomes.filter((o) => o.outcome === 'failed');
  if (bad.length > 0) {
    return {
      name: ds.name,
      to: 'abandoned',
      why: `${bad.length} of ${outcomes.length} contributing Run(s) ended badly (${bad[0]?.run}) — the result is short of what was asked for`,
    };
  }
  return {
    name: ds.name,
    to: 'sealed',
    why: `all ${outcomes.length} contributing Run(s) completed`,
  };
}

/** Map a Temporal execution status string onto the three outcomes this cares about. */
export function outcomeOfStatus(status: string | undefined | null): RunOutcome {
  const s = String(status ?? '').toLowerCase();
  if (s === '') return 'unknown';
  if (s.includes('running') || s === 'executing') return 'running';
  if (s.includes('completed')) return 'completed';
  if (
    s.includes('failed') ||
    s.includes('terminated') ||
    s.includes('canceled') ||
    s.includes('cancelled') ||
    s.includes('timed')
  ) {
    return 'failed';
  }
  return 'unknown';
}

export interface SealReport {
  applied: boolean;
  considered: number;
  sealed: number;
  abandoned: number;
  untouched: number;
  decisions: SealDecision[];
}

/**
 * Reconcile a whole catalog.
 *
 * `apply` is opt-in and the default is a REPORT, for the reason every destructive-looking thing in
 * this repo defaults to a dry run: the output is what a human reads before authorising it, and the
 * read costs nothing.
 */
export async function sealFinishedDatasets(
  datasets: readonly DatasetContributors[],
  outcomeOf: (runId: string) => RunOutcome,
  write: (name: string, state: DatasetState) => Promise<void>,
  opts: { apply?: boolean } = {}
): Promise<SealReport> {
  const apply = opts.apply ?? false;
  const decisions = datasets.map((d) => decideSeal(d, outcomeOf));
  const report: SealReport = {
    applied: apply,
    considered: datasets.length,
    sealed: 0,
    abandoned: 0,
    untouched: 0,
    decisions,
  };
  for (const d of decisions) {
    if (d.to === null) {
      report.untouched += 1;
      continue;
    }
    if (d.to === 'sealed') report.sealed += 1;
    if (d.to === 'abandoned') report.abandoned += 1;
    if (apply) await write(d.name, d.to);
  }
  return report;
}
