import { defineConfig } from 'vitest/config';

// NO ALIAS FOR @kontra/core, and that is the point. An alias pointing at ../core/src would let
// this suite pass against source that the orchestrator does not run — the orchestrator resolves
// the package, which is core/dist. A stale dist would then be invisible here and live in
// production. So the suite resolves what everything else resolves, and the workspace root's
// `test` script builds core first.
export default defineConfig({
  test: {
    include: ['src/**/*.test.ts', 'contract/**/*.test.ts', '../core/src/**/*.test.ts'],
    environment: 'node',
    // Workflow tests spin up a Temporal test server + bundle workflow code, which
    // is well over the 5s default. The codec/pure suites finish in milliseconds.
    testTimeout: 30_000,
    // The interpreter suite's beforeAll boots a real local Temporal server
    // (TestWorkflowEnvironment.createLocal downloads the Temporal CLI on a cold
    // runner) AND bundles workflow code — well over a minute on first run.
    hookTimeout: 300_000,
  },
});
