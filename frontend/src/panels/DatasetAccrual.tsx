/**
 * The count on a Dataset's own page, and whether it is still moving.
 *
 * THE SPLIT IS THE POINT, AND IT IS A REGRESSION GUARD RATHER THAN A STYLE. Issue #14 fixed the
 * results grid resetting on every catalog poll by making the grid's identity rest on things a tick
 * cannot change — one frozen `defaultColDef`, and column defs keyed on the schema SIGNATURE. A LIVE
 * ROW COUNT IS EXACTLY THE CHANGE THAT REINTRODUCES THAT BUG: a number that moves every two seconds,
 * held in the console's own state, re-renders the console, and a console that re-renders rebuilds
 * whatever its memos do not protect.
 *
 * So the count's state does not live in the console at all. {@link LiveAccrual} owns the
 * subscription and the clock, and it is a SIBLING of the grid rather than an ancestor: React
 * re-renders the component whose state changed and its children, so a snapshot landing here cannot
 * reach `DatasetConsole`, its `columnDefs` memo, its `rowData`, the SQL in its editor or a query in
 * flight. That is a structural guarantee, not a discipline someone has to remember — the console
 * literally has no live count to hand the grid.
 *
 * THE TWO NUMBERS ARE NEVER ONE NUMBER. The catalog total is what `/api/datasets` measured, at the
 * scope its label states, and it does not move while a console is open (the console unsubscribes
 * from the poll for the reason above). The live count is the durable object count of ONE Run's path,
 * per chunk, from its own SSE endpoint. `1,246 rows · every run` beside `live · 1,203 rows` is two
 * correct numbers about two different things, so each carries its own label — the unlabelled pair is
 * the bug this surface has already shipped once.
 *
 * NOTHING BLINKS. Arrival is the number changing and the chunk age counting up; there is no pulse,
 * no flash and no spinner in any phase. Counts are `tabular-nums` so a digit changing does not
 * re-flow the line beside it.
 */

import type { DatasetInfo } from '../run/api';
import {
  accrualPhase,
  accrualWords,
  type AccrualPhase,
} from '../datasets/accrual';
import { ROW_TAIL_START, useRowTail, type RowTailState } from '../datasets/rowTail';
import { countText, listingScope, scopeWords, type CountScope, type DatasetKind } from '../datasets/scope';
import { datasetState } from '../datasets/state';
import { RowTailReadout, useNow } from './LiveRowTail';

export interface AccrualLineProps {
  /** The catalog's row count for what was opened. */
  rows: number;
  /** Which rows that number counted — drawn in words, never inferred (`datasets/scope.ts`). */
  scope: CountScope;
  kind: DatasetKind;
  phase: AccrualPhase;
  /** The live tail's state. Ignored unless the phase is `accruing`; passed always so this function
   *  stays pure and every phase is assertable without a socket. */
  tail: RowTailState;
  now: number;
}

/**
 * The count line, from values alone — the whole of what is drawn, in every phase.
 *
 * Pure for the reason the other panel suites record: this runs in node with no jsdom, so the four
 * lifecycle readings, the empty Dataset and the lost stream are asserted through the markup rather
 * than by mounting a socket.
 */
export function AccrualLine({ rows, scope, kind, phase, tail, now }: AccrualLineProps): JSX.Element {
  const words = accrualWords(phase);
  return (
    <span
      data-testid="dataset-accrual"
      data-phase={phase}
      className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5"
    >
      <span
        data-testid="dataset-header-rows"
        data-scope={scope}
        data-rows={rows}
        className="font-mono text-[11px] tabular-nums text-muted-foreground"
        title={scopeWords(scope, kind).title}
      >
        {countText(rows, scope, kind)}
      </span>
      {phase === 'accruing' ? (
        // LABELLED `live`, beside a total that is not. Both are row counts of the same Dataset and
        // they legitimately differ — the catalog's is every Run at the moment the page opened, the
        // tail's is this Run's committed objects right now — so neither may be drawn as the other.
        <span className="flex items-baseline gap-1" title={words.title}>
          <span className="text-[9px] uppercase tracking-wide text-muted-foreground">live</span>
          <RowTailReadout state={tail} now={now} />
        </span>
      ) : (
        <span data-testid="dataset-accrual-word" className="text-[10px] text-muted-foreground" title={words.title}>
          {words.label}
        </span>
      )}
    </span>
  );
}

/**
 * The wired count: subscribes to the Run's durable path and ticks a clock, so the number lands and
 * the chunk age advances.
 *
 * SEPARATE COMPONENT, DELIBERATELY — see the module header. Its state changing re-renders THIS and
 * nothing above it, which is what keeps a live count from touching the results grid, the editor or a
 * running query.
 */
function LiveAccrual(props: Omit<AccrualLineProps, 'tail' | 'now'> & { runId: string }): JSX.Element {
  const tail = useRowTail(props.runId, true);
  const now = useNow();
  return <AccrualLine {...props} tail={tail} now={now} />;
}

/**
 * The count for one opened Dataset — live while a Run is appending, and honest about why it is not
 * otherwise.
 *
 * A socket is opened ONLY in the `accruing` phase: an `open` Dataset with no Run to address, and
 * every sealed, abandoned or lifecycle-less one, renders a static word and the server polls nothing
 * for it (`datasets/accrual.ts` says why each is its own answer).
 */
export default function DatasetAccrual({ dataset }: { dataset: DatasetInfo }): JSX.Element {
  const state = datasetState(dataset);
  const phase = accrualPhase({ state, runId: dataset.runId });
  const shared = {
    rows: dataset.rows,
    scope: listingScope(dataset),
    kind: dataset.kind,
    phase,
  };
  return phase === 'accruing' && dataset.runId ? (
    <LiveAccrual {...shared} runId={dataset.runId} />
  ) : (
    <AccrualLine {...shared} tail={ROW_TAIL_START} now={0} />
  );
}
