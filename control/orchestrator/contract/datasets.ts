/**
 * MOVED TO `@kontra/core/contract/datasets`.
 *
 * This re-export exists so the orchestrator's own imports did not have to change in the commit
 * that extracted the shared kernel. The retention window and the lifecycle union live in core
 * because the Datasets page reads both, and kontra-console is now a different repository — a
 * number the browser copies rather than imports is the exact drift `datasets.test.ts` was written
 * about after it shipped once.
 */
export * from '@kontra/core/contract/datasets';
