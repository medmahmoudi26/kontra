import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['src/**/*.test.ts', 'contract/**/*.test.ts'],
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
