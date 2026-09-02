/**
 * MOVED TO `@kontra/core/contract/types`.
 *
 * This re-export exists so the orchestrator's own imports did not have to change in the commit
 * that extracted the shared kernel. The declarations are in core because kontra-console — a
 * different repository — reads the same contract, and a contract with two copies is a drift with
 * no failure mode: MEASURED, `core/src/contract.ts` and this file were byte-identical for a day,
 * which is one edit away from an API whose two halves disagree and nothing goes red.
 */
export * from '@kontra/core/contract/types';
