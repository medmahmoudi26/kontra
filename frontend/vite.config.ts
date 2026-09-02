/// <reference types="node" />
import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

// The web app reuses the orchestrator's pure core (contract types + graph/typecheck/
// shard) as the single source of truth — no duplication. `@core` is `../backend/src`;
// `@contract` is `../backend`, where the contract surface lives (the hand-written
// `types.ts` + the generated `_gen`); `server.fs.allow` lets the dev server read them.
// `@` is the app's own src (the shadcn/ui convention its copied components import by).
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@core': path.resolve(here, '../backend/src'),
      '@contract': path.resolve(here, '../backend/contract'),
      '@': path.resolve(here, 'src'),
    },
  },
  server: {
    fs: { allow: ['..'] },
    // The backend (Unit 6) serves the run API; proxy it in dev so the SPA can use
    // a same-origin `/api` base.
    //
    // THERE IS DELIBERATELY NO `/api/panels` ENTRY HERE (ADR 0020). The Dashboard's routes live under
    // `/api/panels/*` on the STREAMER's own port, and it would be easy to read that as a bug this
    // proxy should fix — it is not: `panelsClient.ts` builds an absolute base URL
    // (`VITE_KONTRA_PANEL_BASE`, else the page's host on :8090), so those requests never reach this
    // proxy and cannot be routed to 8088 by accident.
    //
    // Adding an entry would make the streamer look same-origin in dev, which means the CORS
    // allow-list and the WS `Origin` check — the two things that actually guard it in production —
    // would never run until they were in front of an operator. Two origins is the decision, so dev
    // pays the same price production does: put the dev origin in the streamer's
    // `KONTRA_PANEL_ORIGIN` (e.g. `http://localhost:8088,http://localhost:5173`) or the ticket POST
    // is blocked and the socket upgrade answers 403.
    proxy: {
      '/api': {
        target: process.env.VITE_API_TARGET ?? 'http://localhost:8088',
        changeOrigin: true,
      },
    },
  },
  /**
   * ── THE BROWSER SUITE HAS A DOM ───────────────────────────────────────────────────────────────
   *
   * It ran in `environment: 'node'` until now, which meant no `useEffect` ever ran and no handler
   * could be fired. That is not a gap in coverage, it is a shape: the two largest surfaces in the
   * console — `WorkflowsPage` at 1,551 lines with 31 pieces of state and 14 effects, `DashboardPage`
   * at 1,240 with 14 and 12 — had zero tests between them, and about sixty pure modules sat beside
   * components so that *something* was assertable. The tested surface was the decision and the
   * untested surface was the wiring, which is where the polling, subscription and resubscribe bugs
   * live. `jsdom` + `@testing-library/react` is what makes the wiring reachable.
   *
   * ── WALL TIME, MEASURED ON THIS BOX (2 vCPU / 3.8 GB, nothing else running) ───────────────────
   *
   *   before   environment 'node',  pool 'forks'      121 files / 2,092 tests    78 s
   *   after    environment 'jsdom', pool 'forks'      127 files / 2,234 tests   545 s   ← 7.0×
   *   after    environment 'jsdom', pool 'vmThreads'  127 files / 2,234 tests    65–72 s ← 0.9×
   *
   * Both "after" rows are the same suite, and the before row is the OLD config re-run against the
   * same warm vite cache — the 95 s this suite is usually quoted at is a cold one. The suite that
   * came out of this has a DOM and is slightly FASTER than the one that did not.
   *
   * THE POOL IS THE WHOLE DIFFERENCE, AND IT IS NOT THE DOM THAT IS SLOW. A jsdom window costs
   * 13–20 ms to construct here, measured directly against the library. What costs ~2.8 s is
   * building one in a FRESH PROCESS: vitest's default `forks` pool starts a child per test file, so
   * jsdom's module graph is imported and JIT-ed 127 times over, and the reported `environment` time
   * went from 44 ms to 343 s. `vmThreads` reuses the worker — jsdom is imported once per worker —
   * and gives each file its own `vm` context, so per-file isolation is unchanged. Measured on one
   * directory of 11 files: forks 68 s, threads 50 s, vmThreads 10.5 s.
   *
   * `isolate: false` IS FASTER STILL AND IS WRONG. It brought the suite to 50 s and failed 95 tests
   * across 6 files: the module registry and the globals are shared between files in a worker, so a
   * `vi.mock` from one survives into the next. Isolation is what the new component tests are made
   * of, and it stays on.
   *
   * ONE FILE OPTS BACK OUT, AND THE REASON IS A NODE BUILTIN, NOT A REALM. `methodCall.render.test.ts`
   * imports `@core/actorControl`, which reaches `backend/src/data/sql.ts`, which `createRequire`s
   * `node:sqlite` at module load. On Node 22 that module is experimental and is NOT in
   * `module.builtinModules` (verified in a REPL), so nothing that decides "builtin or file" from
   * that list can recognise it: under `forks` the require is Node's own and resolves natively,
   * while `vmThreads` routes it through the VM's module runner, which takes it for a path and fails
   * the whole file with `ENOENT: node:sqlite` before a test runs. Three narrower fixes were tried
   * and none reaches a RUNTIME `createRequire`: `test.alias`, `server.deps.external: [/^node:/]`,
   * and a `vi.mock('node:sqlite')` in the file itself. So it runs in `forks`, where it always did.
   *
   * THE GLOB IS AN ABSOLUTE PATH, AND THAT IS NOT AN ACCIDENT. `poolMatchGlobs` matches with
   * micromatch's defaults, where `**` does NOT cross a segment beginning with a dot — so
   * a leading `**` followed by the basename matches in a plain checkout and matches NOTHING under
   * a path like `…/.claude/worktrees/…`, which is where an agent runs the suite. A pattern that is
   * already the full path has no `**` to be defeated. (`poolMatchGlobs` is deprecated in vitest 3;
   * this paragraph is what the migration to `projects` has to preserve.)
   */
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
    setupFiles: ['./src/vitest.setup.ts'],
    pool: 'vmThreads',
    poolMatchGlobs: [[path.resolve(here, 'src/panels/methodCall.render.test.ts'), 'forks']],
  },
});
