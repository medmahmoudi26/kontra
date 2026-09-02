/**
 * Who wrote a Dataset: which **Machines**, which **Actor** versions, and how many rows each.
 *
 * THE MEASURED FAILURE THIS EXISTS FOR. A four-Machine `nscheck` run put 1,246 rows into `lame`
 * and reported `completed`. `SELECT node, version, count(*) … GROUP BY 1,2` returned exactly one
 * row — `['w','0',1246]` — so "four Machines were asked, three produced rows" was a sentence the
 * console had no way to form, and that sentence is the shape of every silent-failure incident
 * this repo has had.
 *
 * THREE VALUES, AND THIS MODULE IS THE ONLY THING ALLOWED TO TELL THEM APART. The server hands
 * back what the lake holds, verbatim; the reading happens here:
 *
 *   - a hostname (`kf-dns-01`) — MEASURED: this Machine ran the Method;
 *   - `null` — UNRECORDED: nothing knew. A Batch paged out of a Dataset was produced by the lake
 *     and not by a Machine, and an actor host older than the provenance contract reports none.
 *     No producer can emit NULL, which is exactly why it is the representation;
 *   - `'w'` with version `'0'` — the LEGACY PLACEHOLDER the publish activity substituted before
 *     provenance travelled with the Batch. It is a recorded value, not a gap — and it names no
 *     Machine, so it is neither folded into the unrecorded bucket nor drawn as a host.
 *
 * The pair, not each column: `'w'` and `'0'` were written by ONE line in one code path, so they
 * always co-occur, and requiring both is what keeps an Actor whose version really is `0` from
 * being relabelled a placeholder. A real Machine named `w` running Actor version `0` would be
 * misread — which is the ambiguity that got the substitution deleted in the first place, and is
 * why the console says "placeholder" rather than silently dropping the bucket.
 */

import {
  counted,
  provenanceStatement,
  scopeWords,
  share,
  type Counted,
} from './scope';

/** One bucket the lake counted: the rows carrying this exact (Machine, Actor version, Run) triple. */
export interface ProvenanceGroup {
  /** The **Machine**, verbatim from the `node` column. `null` is unrecorded. */
  machine: string | null;
  /** The **Actor** version, verbatim. `null` is unrecorded. */
  version: string | null;
  /**
   * The **Run** that appended these rows, verbatim from `run_id` — the caller's workflow id, so
   * it addresses one run directly. `null` is unrecorded.
   *
   * A Dataset name SPANS Runs, which is the fact every count over it has to state: `lame` holds
   * 1,246 rows from two Runs of one workflow, and the listing's 623 is one of them.
   */
  run: string | null;
  rows: number;
}

/** What `/api/datasets/:name/provenance` measured, across every Run that wrote one Dataset. */
export interface DatasetProvenance {
  name: string;
  kind: 'output' | 'standalone';
  /**
   * Rows the group-by counted — DuckDB's own sum, from the SAME statement as the buckets. Never
   * divide these by a separately-issued `count(*)`: a Dataset a **Run** is still appending to
   * grows between two queries, and this project has shipped that bug before. `datasets/scope.ts`
   * makes that refusal mechanical.
   */
  rows: number;
  groups: ProvenanceGroup[];
  /**
   * Whether the table carries the Machine/version columns at all. False for an operator-loaded
   * list, which has neither — a different fact from "every row is unrecorded", and one the console
   * must not draw as a Dataset whose Machines were lost.
   */
  carriesProvenance: boolean;
  /** Whether the table carries `run_id`, and so whether a count here can be scoped to one Run. */
  carriesRun: boolean;
  /** When the one statement behind {@link rows} and {@link groups} answered (epoch ms). */
  measuredAt: number;
}

/** The exact pair the old publish activity substituted for missing provenance. */
export const LEGACY_MACHINE = 'w';
export const LEGACY_VERSION = '0';

/** Whether a bucket is the legacy placeholder — the PAIR, for the reason in the file comment. */
export function isLegacy(g: Pick<ProvenanceGroup, 'machine' | 'version'>): boolean {
  return g.machine === LEGACY_MACHINE && g.version === LEGACY_VERSION;
}

/** How a value reads: measured, absent, or the dead placeholder. Never two of these merged. */
export type ProvenanceState = 'recorded' | 'unrecorded' | 'legacy';

/** One row of the panel: what the value is, what kind of value it is, and what it contributed. */
export interface ProvenanceEntry {
  state: ProvenanceState;
  /** The lake's value, verbatim. `null` only when unrecorded — a gap has no value to show. */
  value: string | null;
  /** What the console draws. A placeholder says so in words, not only in colour. */
  label: string;
  rows: number;
  /**
   * `rows` as a fraction of the Dataset — both numbers from the one query, so this is a ratio
   * within one snapshot rather than across two moments.
   */
  share: number;
  /** Unique within its list: the React key and the `data-testid` suffix. */
  key: string;
}

/**
 * The three dimensions a Dataset's provenance has. Each is read the same way, out of the same
 * buckets and therefore out of the same statement.
 */
export type ProvenanceDimension = 'machine' | 'version' | 'run';

/**
 * Fold the buckets into one dimension's entries, largest first.
 *
 * Two buckets of the same Machine on different Actor versions are ONE Machine that contributed
 * the sum — but a recorded value and the placeholder are never folded together, even when they
 * read the same, because they are different facts.
 *
 * THE FOLD IS WHY THE BUCKETS ARE FINE-GRAINED. One Run's rows are spread across its Machines and
 * one Machine's rows across the Runs it served, so no single query grain answers both questions;
 * the lake groups by all three and each dimension is summed here, out of one snapshot.
 */
export function entriesOf(p: DatasetProvenance, dimension: ProvenanceDimension): ProvenanceEntry[] {
  const byKey = new Map<string, ProvenanceEntry>();
  for (const g of p.groups) {
    const value = dimension === 'machine' ? g.machine : dimension === 'version' ? g.version : g.run;
    // THE PLACEHOLDER IS A CLAIM ABOUT THE PAIR, NEVER ABOUT THE RUN. Publishing substituted
    // `('w','0')` for a missing Machine and Actor version; `run_id` was written for real
    // throughout, so a Run whose rows carry the dead pair is still a recorded Run — striking it
    // through, or leaving it out of the Run count, would delete a fact the lake actually holds.
    const state: ProvenanceState =
      value === null ? 'unrecorded' : dimension !== 'run' && isLegacy(g) ? 'legacy' : 'recorded';
    const key = state === 'unrecorded' ? 'unrecorded' : `${state}-${value}`;
    const existing = byKey.get(key);
    if (existing) {
      existing.rows += g.rows;
      existing.share = p.rows > 0 ? existing.rows / p.rows : 0;
      continue;
    }
    byKey.set(key, {
      state,
      value,
      label: labelFor(state, value),
      rows: g.rows,
      share: p.rows > 0 ? g.rows / p.rows : 0,
      key,
    });
  }
  // Biggest first, because "which Machine did most of this" is the question. Ties break by state
  // so a gap and a placeholder never sort above a measured value at the same size, then by label
  // so the order is stable between two reads of a Dataset that is still growing.
  const rank: Record<ProvenanceState, number> = { recorded: 0, legacy: 1, unrecorded: 2 };
  return [...byKey.values()].sort(
    (a, b) => b.rows - a.rows || rank[a.state] - rank[b.state] || a.label.localeCompare(b.label)
  );
}

function labelFor(state: ProvenanceState, value: string | null): string {
  if (state === 'unrecorded') return 'not recorded';
  if (state === 'legacy') return `${value} (placeholder)`;
  return String(value);
}

/** The entries that are real values — the only ones that may be counted as Machines or versions. */
export function recorded(entries: readonly ProvenanceEntry[]): ProvenanceEntry[] {
  return entries.filter((e) => e.state === 'recorded');
}

/**
 * The sentence above one dimension.
 *
 * It counts MEASURED values only. Counting the placeholder as a Machine is precisely the claim
 * this slice deletes — `lame` would report "1 Machine" for a run that used four — and counting
 * the gap would invent one.
 */
export function headline(entries: readonly ProvenanceEntry[], noun: string): string {
  const n = recorded(entries).length;
  if (n === 0) return `no ${noun} recorded`;
  return `${n} ${noun}${n === 1 ? '' : 's'}`;
}

/**
 * Whether a share bar is drawn at all.
 *
 * "No distribution is drawn from a single bucket." One bucket at 100% is a bar that says
 * "distributed" about something that is not distributed; with one Machine the view says one
 * Machine and nothing else.
 */
export function drawsDistribution(entries: readonly ProvenanceEntry[]): boolean {
  return entries.length > 1;
}

// --- scoping a count to one Run -----------------------------------------------------------------

/**
 * The Dataset's total, carrying the statement that measured it.
 *
 * Scoped `dataset`: every Run that ever wrote the name, which is also exactly what the console's
 * editor reads — `SELECT * FROM <name>`, unpartitioned.
 */
export function datasetCount(p: DatasetProvenance): Counted {
  return counted(p.rows, 'dataset', provenanceStatement(p), p.measuredAt);
}

/** One **Run**'s contribution, summed from the buckets of the SAME statement as the total. */
export function runCount(p: DatasetProvenance, run: string): Counted {
  const rows = p.groups.reduce((n, g) => (g.run === run ? n + g.rows : n), 0);
  return counted(rows, 'run', provenanceStatement(p), p.measuredAt);
}

/** A Dataset read as one Run wrote it: the part, the whole, and the sentence joining them. */
export interface RunScope {
  run: string;
  /** Rows this Run contributed. */
  part: Counted;
  /** Rows under the name, every Run. */
  whole: Counted;
  /**
   * The part as a fraction of the whole. `null` only for an EMPTY Dataset — never because the two
   * numbers are incomparable, since one statement measured both.
   */
  share: number | null;
  /** `623 of 1,246 rows · this run`. */
  sentence: string;
}

/**
 * Scope one Dataset's counts to one **Run**.
 *
 * THE SENTENCE THE CONSOLE COULD NOT FORM. `lame` holds 1,246 rows from two Runs and the listing
 * showed 623; "623 of 1,246 rows, from this run" says both numbers and what each counted, in one
 * breath. It is formable at all only because both come out of ONE scan — the two figures an
 * operator would otherwise have divided are a catalog listing and a `count(*)` issued seconds
 * apart, against a Dataset that may have grown in between.
 */
export function runScope(p: DatasetProvenance, run: string): RunScope {
  const part = runCount(p, run);
  const whole = datasetCount(p);
  return {
    run,
    part,
    whole,
    share: share(part, whole),
    sentence: `${part.rows.toLocaleString()} of ${whole.rows.toLocaleString()} rows · ${
      scopeWords('run').label
    }`,
  };
}

/** How one state is drawn. Four channels, so no two states are one another at another opacity. */
export interface ProvenanceBadge {
  /** The row's own border, fill and text colour. */
  className: string;
  /** The VALUE's own styling — a placeholder is struck through, because it names nothing. */
  valueClassName: string;
  /** Decorative and marked so; the label already says the state. */
  glyph: string;
  /** The sentence an operator needs when the label alone is not enough. */
  title: string;
}

const BADGES: Record<ProvenanceState, ProvenanceBadge> = {
  recorded: {
    className: 'border-solid border-border bg-transparent text-foreground',
    valueClassName: '',
    glyph: '●',
    title: 'Measured: the Batch that produced these rows recorded this value.',
  },
  unrecorded: {
    className: 'border-dashed border-muted-foreground/40 bg-transparent text-muted-foreground italic',
    valueClassName: '',
    glyph: '?',
    title:
      'Not recorded: nothing knew this. A Batch paged out of a Dataset was produced by the lake ' +
      'and not by a Machine, and an actor host older than the provenance contract reports none. ' +
      'A gap, never a value — and never merged with one.',
  },
  legacy: {
    className: 'border-solid border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400',
    valueClassName: 'line-through decoration-1',
    glyph: '⚠',
    title:
      'A dead placeholder, not a real value: publishing substituted `w` and `0` for missing ' +
      'provenance before the Machine travelled with the Batch. No Machine named `w` exists, and ' +
      'no Actor version `0` was deployed — but these rows really do carry it, which is why it is ' +
      'shown rather than folded into "not recorded".',
  },
};

export function provenanceBadge(state: ProvenanceState): ProvenanceBadge {
  return BADGES[state];
}

export const PROVENANCE_STATES: readonly ProvenanceState[] = ['recorded', 'legacy', 'unrecorded'];

// --- the wire ------------------------------------------------------------------------------

const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api';

/**
 * One Dataset's provenance, read when the Dataset is OPENED.
 *
 * Not on the listing poll: `/api/datasets` is a catalog scan the app already polls every 2 s,
 * and a group-by is a real column scan. Ungated, like the preview route it sits beside — the SQL
 * is composed on the server and no object-store URL reaches the page, so no token has to be baked
 * into the bundle for it.
 */
export async function fetchProvenance(d: {
  kind: 'output' | 'standalone';
  name: string;
}): Promise<DatasetProvenance> {
  const q = new URLSearchParams({ kind: d.kind });
  const res = await fetch(`${BASE}/datasets/${encodeURIComponent(d.name)}/provenance?${q}`);
  if (!res.ok) {
    // The server's own sentence, when it sent one: "no output dataset named …" is the answer,
    // and "502 Bad Gateway" is not.
    let message = `${res.status} ${res.statusText}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body?.error) message = body.error;
    } catch {
      /* not JSON — keep the status line */
    }
    throw new Error(`provenance: ${message}`);
  }
  return (await res.json()) as DatasetProvenance;
}
