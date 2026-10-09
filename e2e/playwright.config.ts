import { defineConfig, devices } from '@playwright/test';

/**
 * CHECKS AGAINST AN INSTALL THAT IS ALREADY UP. There is no `webServer` here and there must not be:
 * the thing under test is the install itself — images built from this commit, `docker compose up`
 * from the quickstart files — and CI's `docker` job is what brings it up. Point `KONTRA_UI` at any
 * other install to run these by hand.
 *
 * NO TRACE, NO HTML REPORT. Both record what the browser typed, and the first thing this suite types
 * is the console password. That password belongs to a throwaway CI install, but the artifacts of a
 * public repository are public, and a habit of uploading credentials is the thing to not have. On a
 * failure you get the list reporter's output, a screenshot and Playwright's page snapshot.
 */
export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  // The canary holds a Fleet and sweeps for ~20s, and the report renders on the next 60-second
  // pass after the Run closes. The test sets its own, larger budget; this is the default for any
  // test added later.
  timeout: 10 * 60_000,
  expect: { timeout: 30_000 },
  workers: 1,
  fullyParallel: false,
  retries: 0,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [['list'], ['github']] : [['list']],
  outputDir: 'test-results',
  use: {
    baseURL: process.env.KONTRA_UI ?? 'http://127.0.0.1:8088',
    trace: 'off',
    screenshot: 'only-on-failure',
    ...devices['Desktop Chrome'],
    viewport: { width: 1440, height: 1000 },
  },
});
