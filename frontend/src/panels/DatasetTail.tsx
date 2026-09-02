/**
 * The Data tab's row expander: ten rows of one Dataset, inline, without leaving the run.
 *
 * SPLIT ON PURPOSE, the same way `LiveRowTail.tsx` is. {@link TailBody} is a pure function of its
 * props, so every state — unopened, reading, a sealed tail, an open one following, fewer than ten
 * rows, no rows at all, a failed read — is asserted with `renderToStaticMarkup` in node, with no
 * fetch and no timer. {@link DatasetTail} is the thin wrapper that owns the request.
 *
 * THE STATE IS IN THE LEAF, AND THAT IS A REGRESSION GUARD RATHER THAN A STYLE — the same argument
 * `DatasetAccrual.tsx` writes down one surface over. Issue #14 fixed the results grid resetting on
 * every catalog poll by resting its identity on things a tick cannot change; a table of rows that
 * re-reads while a run is live is exactly the change that reintroduces that bug if its state is held
 * high. So `RunDataTab` holds NO tail state at all: each expander owns its own `open` flag and its
 * own rows, and React re-renders the component whose state changed and its children — so rows
 * landing here cannot reach the Dataset list above, let alone the console's grid.
 *
 * THE EXPANDER IS A `<details>`, so the browser owns the disclosure. Several Datasets expand
 * independently by construction (no `name` attribute, so they are not an accordion), the twisty
 * costs the list no re-render, and there is nothing for this file to remember when the operator
 * changes run.
 *
 * NOTHING IS READ UNTIL SOMETHING IS OPENED. A run that wrote nine Datasets issues nine requests for
 * five hundred rows each if the body is rendered eagerly — so the body is mounted only once `open`
 * is true, which is also what makes "an idle Dataset costs the server nothing" true here in the same
 * way `rowTail.ts` makes it true of its socket.
 */

import { useEffect, useMemo, useReducer, useRef, useState } from 'react';
import { ChevronRight } from 'lucide-react';

import { fetchDatasetPreview, type DatasetInfo } from '../run/api';
import { datasetState, type DatasetBadgeState } from '../datasets/state';
import {
  TAIL_START,
  columnSignature,
  emptyTail,
  readTail,
  tailCell,
  tailFollows,
  tailPlan,
  tailReachNote,
  tailReduce,
  tailStateWords,
  tailSummaryWords,
  tailWords,
  type TailState,
} from '../datasets/tailRows';
import type { RunDataset } from './runDatasets';

export interface TailBodyProps {
  /** The read, in whatever state it is in. */
  state: TailState;
  /** The catalog's row count for this Dataset — what the read was planned from, and the only thing
   *  that can be said before a read answers. */
  known: number;
  /** The §11 lifecycle, read through `datasets/state.ts`. FOUR values, never two. */
  lifecycle: DatasetBadgeState;
  /** How this Dataset was attributed to the run (`runDatasets.ts`). An `among` partition's rows are
   *  not necessarily this run's, and a table of them must not imply that they are. */
  how: RunDataset['how'];
  /** The Dataset's name, for the test hooks — never re-derived into a label, the row above owns
   *  that. */
  name: string;
}

/**
 * The expanded contents, from values alone.
 *
 * THE LIFECYCLE SENTENCE IS ALWAYS DRAWN, in all four of its states, because a table of rows is the
 * most finished-looking thing a surface can show and an `open` Dataset under one would read as a
 * result. `tailStateWords` keeps the four apart; the badge on the row above keeps them apart at a
 * glance; this says what the state means FOR THESE ROWS.
 */
export function TailBody({ state, known, lifecycle, how, name }: TailBodyProps): JSX.Element {
  const read = state.read;
  // THE HEADER IS KEYED ON THE SCHEMA SIGNATURE, not on the read — issue #14's rule, applied to this
  // table. An open Dataset re-reads every time its count moves, and every one of those reads returns
  // the same columns; rebuilding the header off the read object would hand it new cells ten times a
  // minute for no change at all.
  const signature = read ? columnSignature(read.columns) : '';
  const columns = useMemo(
    () => read?.columns ?? [],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [signature]
  );

  return (
    <div
      className="mt-1 flex flex-col gap-1 rounded border border-border/70 bg-muted/20 px-2 py-1.5"
      data-testid={`run-tail-${name}`}
      data-phase={state.phase}
      data-reach={read ? read.reach : undefined}
      data-state={lifecycle}
      data-follows={tailFollows(lifecycle) ? 'true' : 'false'}
      data-rows={read ? read.rows.length : undefined}
    >
      {!read ? (
        <p className="m-0 text-[10.5px] text-muted-foreground" data-testid={`run-tail-reading-${name}`}>
          {state.phase === 'failed'
            ? 'The rows could not be read.'
            : `Reading ${tailSummaryWords(known)}…`}
        </p>
      ) : read.seen === 0 ? (
        // NO ROWS IS A FINDING, AND ITS TWO CASES ARE NOT THE SAME. A Dataset a run opened and has
        // not written to yet is waiting; one nothing ever wrote to is done. Neither of them is the
        // tab's "no Dataset in the lake carries this run", which is a fact one level up and stays
        // where it is.
        <p className="m-0 text-[10.5px] text-muted-foreground" data-testid={`run-tail-empty-${name}`}>
          {tailFollows(lifecycle)
            ? 'This Dataset exists and holds no rows yet — its Run opened it and nothing has committed to it so far.'
            : 'This Dataset holds no rows. It was created and never written to.'}
        </p>
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-left font-mono text-[10.5px] tabular-nums">
              <thead>
                <tr className="border-b border-border">
                  {columns.map((c) => (
                    <th
                      key={c.name}
                      title={c.type}
                      scope="col"
                      className="whitespace-nowrap px-1.5 py-0.5 font-normal uppercase tracking-wide text-muted-foreground"
                    >
                      {c.name}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {/* KEYED BY POSITION, deliberately: a row has no id, and a tail that shifts by one
                    is the same ten slots holding new text. React updates the cells in place, which
                    is what "a row landing" looks like when nothing is allowed to blink. */}
                {read.rows.map((row, i) => (
                  <tr key={i} className="border-b border-border/40 last:border-0">
                    {columns.map((c, j) => {
                      const cell = tailCell(row[j]);
                      return (
                        <td
                          key={c.name}
                          title={cell.clipped ? cell.full : undefined}
                          className="max-w-[28rem] truncate px-1.5 py-0.5 align-top"
                        >
                          {cell.text}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <p
            className="m-0 text-[10px] text-muted-foreground"
            data-testid={`run-tail-words-${name}`}
          >
            <span className="font-mono">{tailWords(read)}</span> — {tailStateWords(lifecycle)}
          </p>
        </>
      )}

      {read && tailReachNote(read) && (
        <p className="m-0 text-[10px] text-muted-foreground" data-testid={`run-tail-reach-${name}`}>
          {tailReachNote(read)}
        </p>
      )}

      {how === 'among' && (
        <p className="m-0 text-[10px] text-muted-foreground" data-testid={`run-tail-among-${name}`}>
          Several Runs wrote this partition, so these rows are the partition’s and not necessarily
          this run’s — the console can filter them by <span className="font-mono">run_id</span>.
        </p>
      )}

      {state.phase === 'failed' && state.error && (
        // THE SERVER'S OWN SENTENCE. "that dataset is gone" (404) and "could not preview dataset: …"
        // (502) are different next actions, and replacing either with "failed" throws away the only
        // part worth reading. Any rows from before the failure stay on screen above, marked stale by
        // this line rather than blanked.
        <p className="m-0 text-[10px] text-amber-600 dark:text-amber-400" data-testid={`run-tail-failed-${name}`}>
          {read ? 'These rows are the last read that worked. ' : ''}
          {state.error}
        </p>
      )}
    </div>
  );
}

/**
 * The wired expander: a `<details>` whose body reads this Dataset's rows once it is opened, and
 * again whenever the catalog's count for it moves.
 *
 * THE READ KEY IS THE FOUR COORDINATES PLUS THE COUNT, and that is the whole of "an open Dataset's
 * tail follows". The row object itself is NOT a dependency: `/api/datasets` is polled once for the
 * whole app and hands this component a brand new object every two seconds, so keying the effect on
 * the row would re-read five hundred rows for a Dataset that has not changed — tick-driven work, on
 * the surface that already paid for that lesson once. Keying it on the COUNT means a Dataset that
 * grew re-reads, and one that did not costs nothing.
 *
 * WHAT FOLLOWS IS WHAT COMMITTED. The count this keys on is the catalog's, so rows appear here when
 * they are in the lake — never before. That is the same "one source of truth: the durable path"
 * rule `rowTail.ts` states for its count; it is also why a long call can leave this still while the
 * Run is plainly producing (ADR 0028: the caller does not publish until the call returns).
 */
export function DatasetTail({ info, how }: { info: DatasetInfo; how: RunDataset['how'] }): JSX.Element {
  const [open, setOpen] = useState(false);
  const [state, dispatch] = useReducer(tailReduce, TAIL_START);
  const { kind, name, version, dt, rows } = info;
  // The row rides a ref so the request always addresses the CURRENT catalog row while the effect
  // below stays keyed on the coordinates alone. Synced in its own effect rather than during render:
  // a render must stay pure, and this one runs first, so the read effect beside it never sees a
  // stale row.
  const row = useRef(info);
  useEffect(() => {
    row.current = info;
  }, [info]);

  useEffect(() => {
    if (!open) return;
    const plan = tailPlan(rows);
    if (plan.limit === 0) {
      dispatch({ type: 'rows', read: emptyTail(plan, Date.now()) });
      return;
    }
    let live = true;
    dispatch({ type: 'read' });
    fetchDatasetPreview(row.current, plan.limit)
      .then((preview) => {
        if (live) dispatch({ type: 'rows', read: readTail(preview, plan, Date.now()) });
      })
      .catch((err: unknown) => {
        if (live) dispatch({ type: 'failed', error: err instanceof Error ? err.message : String(err) });
      });
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, kind, name, version, dt, rows]);

  const lifecycle = datasetState(info);
  return (
    <details
      className="group mt-0.5 px-2"
      data-testid={`run-dataset-expander-${name}`}
      data-open={open ? 'true' : 'false'}
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      {/* THE CONTROL SAYS WHAT IT WILL SHOW, from the count alone. A Dataset longer than the window
          shows its FIRST rows, and promising "the last 10" on the twisty would be a lie the operator
          only finds after clicking. */}
      <summary
        className="flex w-fit cursor-pointer list-none items-center gap-1 rounded text-[10px] text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
        title={
          tailFollows(lifecycle)
            ? 'Show the rows themselves. This Dataset is open, so the tail follows as rows commit to the lake.'
            : 'Show the rows themselves. Ten rows is a look — the link above opens the whole Dataset in the console.'
        }
      >
        <ChevronRight
          size={10}
          aria-hidden
          className="shrink-0 transition-none group-open:rotate-90"
        />
        {tailSummaryWords(rows)}
      </summary>
      {open && (
        <TailBody state={state} known={rows} lifecycle={lifecycle} how={how} name={name} />
      )}
    </details>
  );
}
