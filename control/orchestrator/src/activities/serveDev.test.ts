/**
 * `serveDev` — the activity that actually starts a Worker, tested by RUNNING IT.
 *
 * ── WHY THIS FILE EXISTS, AND WHY ITS ABSENCE WAS THE BUG ───────────────────────────────────────
 *
 * There was no test for this module. The route tests around it (`sourceRoutes.test.ts`,
 * `workflowControl.test.ts`) stub `workflow.execute` outright, so every one of them passed against
 * an activity that could not spawn anything at all. It shipped, and every serve from the console
 * failed with `serve-dev failed (exit 127): Error: spawn kontra ENOENT` — a sentence that names a
 * missing binary when the binary was on `PATH` and executable the whole time.
 *
 * THE DEFECT WAS ONE MISSING SPREAD. `serveEnv()` is a set of OVERRIDES that returns `{}` in the
 * ordinary Compose case; it was handed to `spawn` as the `env` option, which REPLACES the
 * environment rather than extending it, so the child process had no `PATH`.
 *
 * SO THESE TESTS SPAWN A REAL PROCESS. A mocked `child_process` would have asserted the shape of a
 * call I got wrong on purpose — the whole failure was in what `spawn` DOES with that shape, which
 * only the real one can tell you. The "binary" is `/bin/sh` and a script that prints what it was
 * given, so the suite needs no kontra, no docker and no cluster.
 */

import { mkdtempSync, rmSync, writeFileSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureServeDev, serveDev } from './serveDev';

let dir: string;
let bin: string;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), 'kontra-servedev-'));
  // A stand-in for `kontra`: it reports the one thing under test — whether it inherited a PATH —
  // and echoes its argv so the verb assertions have something real to read.
  bin = join(dir, 'fake-kontra');
  writeFileSync(bin, '#!/bin/sh\necho "PATH=${PATH}"\necho "ARGV=$*"\necho "CWD=$(pwd)"\n');
  chmodSync(bin, 0o755);
});

afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

/** Point the activity at a folder that exists, since it checks before spawning. */
function wire(over: Partial<{ kind: string; envOverride: NodeJS.ProcessEnv; binary: string }> = {}) {
  configureServeDev({
    resolve: async () => ({ name: 'canary', path: dir, kind: over.kind ?? 'workflow' }),
    kontraBin: () => over.binary ?? bin,
    serveEnv: () => over.envOverride ?? {},
  });
}

describe('serveDev spawns with a usable environment', () => {
  it('inherits PATH when there are no overrides at all', async () => {
    // THE REGRESSION. `serveEnv()` is `{}` in the ordinary Compose deployment, and `{}` used to be
    // the CHILD'S WHOLE ENVIRONMENT. Without PATH, a bare `kontra` cannot be resolved and Node
    // reports ENOENT as though it were not installed.
    wire();
    const res = await serveDev({ sourceId: 'at:' + dir, kind: 'workflow' });
    expect(res.detail).toContain('PATH=');
    // Non-vacuous: a real, non-empty PATH, not the literal string with nothing after it.
    const path = /PATH=(.*)/.exec(res.detail)?.[1] ?? '';
    expect(path.length).toBeGreaterThan(0);
    expect(path).toBe(process.env.PATH);
  });

  it('applies overrides ON TOP of the inherited environment, not instead of it', async () => {
    wire({ envOverride: { KONTRA_TEST_MARKER: 'yes' } });
    const res = await serveDev({ sourceId: 'at:' + dir, kind: 'workflow' });
    // The override is honoured...
    expect(res.detail).toContain('PATH=');
    // ...and PATH survived it, which is the whole point of "overrides".
    expect(/PATH=(.*)/.exec(res.detail)?.[1]).toBe(process.env.PATH);
  });

  it('runs in the folder it resolved', async () => {
    wire();
    const res = await serveDev({ sourceId: 'at:' + dir, kind: 'workflow' });
    // macOS symlinks /tmp, so compare the basename rather than the whole path.
    expect(res.detail).toContain(dir.split('/').pop() as string);
  });
});

describe('serveDev builds its argv from the STORE, never the caller', () => {
  it('uses the workflow verb for a workflow folder', async () => {
    wire({ kind: 'workflow' });
    const res = await serveDev({ sourceId: 'at:' + dir, kind: 'workflow' });
    expect(res.detail).toContain(`ARGV=workflow serve ${dir} --mode dev --watch`);
  });

  it('uses the actor verb for an actor folder', async () => {
    wire({ kind: 'actor' });
    const res = await serveDev({ sourceId: 'at:' + dir, kind: 'actor' });
    expect(res.detail).toContain(`ARGV=serve --actor ${dir} --mode dev`);
  });

  it('refuses when the caller disagrees with the store about the kind', async () => {
    // Reported, not silently corrected — a caller that is wrong about its own catalog must not look
    // like one that is right.
    wire({ kind: 'actor' });
    await expect(serveDev({ sourceId: 'at:' + dir, kind: 'workflow' })).rejects.toThrow(/registered as "actor"/);
  });

  it('refuses an id the store does not know', async () => {
    configureServeDev({
      resolve: async () => null,
      kontraBin: () => bin,
      serveEnv: () => ({}),
    });
    await expect(serveDev({ sourceId: 'at:/nope', kind: 'workflow' })).rejects.toThrow(/no registered source/);
  });

  it('says the FOLDER is gone rather than letting spawn blame the binary', async () => {
    // A missing `cwd` and a missing binary share an errno and a sentence. This check is what keeps
    // an operator from going to look for kontra on the wrong machine.
    configureServeDev({
      resolve: async () => ({ name: 'canary', path: join(dir, 'not-here'), kind: 'workflow' }),
      kontraBin: () => bin,
      serveEnv: () => ({}),
    });
    await expect(serveDev({ sourceId: 'at:' + dir, kind: 'workflow' })).rejects.toThrow(/not on this machine any more/);
  });
});

describe('serveDev reports a failing CLI', () => {
  it('carries the exit code and the output into the error', async () => {
    const bad = join(dir, 'failing-kontra');
    writeFileSync(bad, '#!/bin/sh\necho "ModuleNotFoundError: no module named canary" >&2\nexit 1\n');
    chmodSync(bad, 0o755);
    wire({ binary: bad });
    await expect(serveDev({ sourceId: 'at:' + dir, kind: 'workflow' })).rejects.toThrow(
      /serve-dev failed \(exit 1\): ModuleNotFoundError/
    );
  });
});
