/**
 * What a CLOSED row in the Workflows list may honestly say about a workflow it is not measuring.
 *
 * IT LIVES IN ITS OWN MODULE, apart from `WorkflowsPage.tsx`, for a mechanical reason: the page
 * imports CodeMirror, which cannot load under the test runner, so anything that has to be tested in
 * isolation cannot ride in that file. The join this decides is exactly the thing #13 got wrong, so
 * it is exactly the thing that needs a test — hence here.
 *
 * THE JOIN KEY IS THE DECORATED TYPE, NOT THE FILENAME GUESS. A row is joined to its runs by the
 * `@workflow.defn` name the source declares — `dhmonitor`'s class is `DockerLeakMonitor` — and NOT
 * by `guessTypeFromFilename`, which reads the file `dhmonitor` as `Dhmonitor` and matches no run of
 * a workflow that is, at that moment, running. The list drew a live workflow as NEVER RUN because
 * the guess and the real type differ by more than a capital letter, and the run list keyed on the
 * real one. `scanimages` only ever looked right by luck (`Scanimages`.toLowerCase() equals
 * `ScanImages`.toLowerCase()); the moment a name diverges further the luck runs out.
 */

import type { RunRow } from '../run/api';
import type { WorkflowState } from '../run/workflowState';

export interface RowStatus {
  /** The dot's animation state. `unknown` never pulses — see `stateWords`. */
  state: WorkflowState;
  /** The pill's word. `never run` ONLY when the type is known and genuinely has no runs. */
  label: string;
  /** This row's runs, in the order given (the caller passes them newest-first). */
  runs: RunRow[];
  last?: RunRow;
}

/**
 * The status a closed row shows, from its DECORATED type and the run list.
 *
 * `type` is `undefined` when the source has not been read yet, or could not be — the join key is
 * unknown, and the only honest answer is `unknown`. `never run` here would be the #13 lie in a
 * different disguise: "no run of a type I cannot name" is not "this has never run". A row only says
 * `never run` once its type is known and no run carries it.
 *
 * EXACT MATCH on `RunRow.type`, which is the caller's workflow type — the same authority
 * `typeFromSource` reads out of the file, so the two are the same string and case-folding is neither
 * needed nor honest (it is what let the old guess-join limp along for one lucky filename).
 */
export function rowStatus(type: string | undefined, runs: readonly RunRow[]): RowStatus {
  if (!type) return { state: 'unknown', label: 'unknown', runs: [] };
  const mine = runs.filter((r) => r.type === type);
  const last = mine[0];
  return {
    state: mine.some((r) => r.status === 'running') ? 'running' : 'idle',
    label: last?.status ?? 'never run',
    runs: mine,
    last,
  };
}
