/**
 * A Dataset's state, and how it is read off the wire (ADR 0023 §11).
 *
 * A Dataset is **open** while a Run may still be appending to it, **sealed** when the caller
 * declares it complete, and **abandoned** when the caller gives up on it. The point of the
 * vocabulary is that a partial Dataset can never be read as a finished one, so this module has
 * exactly one rule: nothing becomes `sealed` except a Dataset the server says is sealed.
 *
 * THE WORDS ARE THE CONTRACT with `backend/src/data/datasets.ts`, which writes them, and
 * with both SDK writers. They are spelled out on each side with no shared code, and a drift has
 * no loud failure mode — it renders as the wrong badge, forever.
 */

/** What the server records. A Dataset no writer ever touched has no state at all. */
export type DatasetState = 'open' | 'sealed' | 'abandoned';

/** What the UI draws — the three states, plus the absence of one. */
export type DatasetBadgeState = DatasetState | 'none';

/**
 * The state of one dataset as listed by the API.
 *
 * A listing that carries NO state is `none`, not `open`: no writer ever recorded a lifecycle
 * for it — every dataset materialized before §11, and every operator-loaded list — and nothing
 * is appending to it. Calling that `open` would put "a Run may still be writing this" on a
 * static list nobody is touching, which is the same lie as calling a partial Dataset finished,
 * pointed the other way.
 *
 * An unrecognised value is also `none`, which is the shape a wire from a NEWER peer arrives in:
 * report that this build cannot say, rather than guessing between "done" and "not done".
 */
export function datasetState(d: { state?: string }): DatasetBadgeState {
  return d.state === 'open' || d.state === 'sealed' || d.state === 'abandoned' ? d.state : 'none';
}

export const DATASET_STATES: readonly DatasetState[] = ['open', 'sealed', 'abandoned'];

/** How one state is drawn: a word and a colour, both its own. */
export interface DatasetBadge {
  label: string;
  /** Tailwind classes — the pill's own colours, not the neutral catalogue chip's. */
  className: string;
  /** The sentence an operator needs when the word alone is not enough. */
  title: string;
}

/**
 * The badge for one state.
 *
 * Three states, three colours, deliberately: rendering `open` the way `sealed` is rendered
 * would let a Dataset a Run is still appending to be read as the finished article, which is
 * the single failure §11 exists to prevent. Amber reads as "not yet", green as "done", and a
 * struck-through grey as "this one is not coming back".
 */
export function datasetBadge(state: DatasetBadgeState): DatasetBadge {
  switch (state) {
    case 'sealed':
      return {
        label: 'sealed',
        className: 'border-transparent bg-emerald-500/15 text-emerald-600 dark:text-emerald-400',
        title: 'The caller declared this Dataset complete.',
      };
    case 'abandoned':
      return {
        label: 'abandoned',
        className:
          'border-transparent bg-muted text-muted-foreground line-through decoration-1',
        title: 'The caller gave up on this Dataset — what is here is partial and final.',
      };
    case 'open':
      return {
        label: 'open',
        className: 'border-amber-500/40 bg-amber-500/15 text-amber-700 dark:text-amber-400',
        title:
          'A Run may still be appending, or its producer died before sealing — this Dataset is not a finished result.',
      };
    default:
      return {
        label: 'no lifecycle',
        className: 'border-dashed border-muted-foreground/40 text-muted-foreground',
        title:
          'No writer recorded a lifecycle for this Dataset: an operator-loaded list, or output materialized before Datasets had one.',
      };
  }
}
