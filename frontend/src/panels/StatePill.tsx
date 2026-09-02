/**
 * One word about one dimension, in a colour.
 *
 * SHARED BY THE CATALOG AND BY RUNS, which is why it lives here rather than in either. Both
 * surfaces print a run's state and they must not drift into two vocabularies for one fact.
 *
 * The palette deliberately makes the two dimensions rhyme rather than match: `running` and
 * `writing` are both in flight, `completed` and `complete` both landed. What it never does is give
 * an UNKNOWN the colour of an answer — `unknown` and `unrecorded` are muted, because a pill that
 * looked settled would be the confident green label over lost work all over again (ADR 0017).
 */

const PILL: Record<string, string> = {
  // Execution — authority Temporal.
  running: 'bg-emerald-500/15 text-emerald-500',
  starting: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
  completed: 'bg-sky-500/15 text-sky-500',
  failed: 'bg-rose-500/15 text-rose-500',
  cancelled: 'bg-zinc-500/15 text-muted-foreground',
  pending: 'bg-zinc-500/15 text-muted-foreground',
  // NOT A TEMPORAL STATUS. A run waiting on a human is `running` to Temporal for the whole time it
  // waits; `parked` is what the run's own pending ask makes it, and it is here so the two do not
  // become two vocabularies for one row (`workflowThread.ts`'s `runWord` is the only producer).
  // Amber rather than muted: it is blocked and in flight, which is nothing like `cancelled`.
  parked: 'bg-amber-500/15 text-amber-500',
  // The projection over both (ADR 0017 §2).
  executing: 'bg-emerald-500/15 text-emerald-500',
  finalizing: 'bg-amber-500/15 text-amber-500',
  output_failed: 'bg-rose-500/15 text-rose-500',
  // Materialization — authority the ledger.
  complete: 'bg-sky-500/15 text-sky-500',
  writing: 'bg-amber-500/15 text-amber-500',
};

/** `testid` is REQUIRED rather than defaulted, and that is a correction: the same pill draws a
 *  header's state and every row's, so one shared hook matched a dozen elements — and a check that
 *  read "the first one" was reading a list row while believing it was reading the header. */
export function StatePill({
  state,
  testid,
  title,
  small,
}: {
  state: string;
  testid: string;
  title?: string;
  small?: boolean;
}): JSX.Element {
  return (
    <span
      data-testid={testid}
      data-state={state}
      title={title}
      className={`shrink-0 rounded-full px-2 py-px font-mono font-bold uppercase tracking-tight ${
        small ? 'text-[9.5px]' : 'text-[10.5px]'
      } ${PILL[state] ?? 'bg-zinc-500/10 text-muted-foreground'}`}
    >
      {state.replace('_', ' ')}
    </span>
  );
}
