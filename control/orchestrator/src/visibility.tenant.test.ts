/**
 * EVERY START IN THIS CONTROL PLANE CARRIES A TENANT — swept over the source, not asserted per site.
 *
 * WHY A SWEEP AND NOT FOUR UNIT TESTS. `KontraTenant` was registered on the namespace and written by
 * NOTHING, and the way that survived was not that anyone decided against it: `startRun` was fixed
 * first and the INFRA side simply never went through `startRun`. `leaseWorkflow`, `stackWorkflow`,
 * `tmuxSessionWorkflow` and the Probe each start a workflow from their own file, so a per-site test
 * proves only the sites somebody remembered to write one for — and the next start site added to this
 * codebase is exactly the one nobody would.
 *
 * So the unit is THE FILE, the way `tests/test_temporal_tls.py` treats a file as the unit for
 * `connect_tls`: a module that starts a workflow must also name `tenantAttributes`. That is looser
 * than checking the option object and it is deliberately looser — a tight structural assertion here
 * would have to parse TypeScript, and the failure it would catch (a stamp built by hand with the
 * wrong value) is caught by `workflowControl.test.ts`, which reads the attribute back off the start
 * options.
 *
 * NON-VACUITY FIRST, because a regex that stopped matching reports a clean sweep over nothing —
 * this repository's most repeated failure shape, and the reason the Python guard opens the same way.
 */

import { describe as test, expect, it } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import path from 'node:path';

const SRC = path.join(__dirname);

/** Every `.ts` under `src/`, excluding tests and generated code. */
function sources(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry);
    if (statSync(full).isDirectory()) {
      if (entry === 'node_modules' || entry === '_gen' || entry === 'dist') continue;
      sources(full, out);
      continue;
    }
    if (!entry.endsWith('.ts') || entry.endsWith('.test.ts')) continue;
    out.push(full);
  }
  return out;
}

/**
 * A client-side workflow START — the only moment a search attribute can be set without costing an
 * event. Deliberately NOT matching `getHandle(...)`, a child workflow start inside workflow code, or
 * `.start()` on something that is not a workflow client: a child inherits nothing here and workflow
 * code cannot reach `tenantAttributes` anyway (it is a client concern, and the sandbox forbids it).
 */
const STARTS = /\bworkflow\.(start|signalWithStart)\s*\(/;

test('the tenant stamp', () => {
  const files = sources(SRC).map((f) => ({ rel: path.relative(SRC, f), body: readFileSync(f, 'utf8') }));

  const starters = files.filter((f) => STARTS.test(f.body));

  it('found the start sites it is sweeping over', () => {
    // Four infra sites plus `startRun`. If this drops, the regex stopped matching and every
    // assertion below is passing over an empty list.
    expect(starters.length, 'the start-site sweep found nothing — the pattern stopped matching').toBeGreaterThanOrEqual(4);
  });

  it('stamps the tenant at every one of them', () => {
    const bare = starters.filter((f) => !f.body.includes('tenantAttributes')).map((f) => f.rel);
    expect(
      bare,
      'a workflow start that records no tenant — see `tenantAttributes` in visibility.ts:\n  ' +
        bare.join('\n  ')
    ).toEqual([]);
  });

  /**
   * AT START, NEVER BY UPSERT — the rule the event-log audit exists to enforce.
   *
   * Attributes on `start` ride inside `WorkflowExecutionStarted` and write no extra event. An
   * upsert inside a workflow is a command of its own, and one per Machine per attach would be added
   * to a `wardenWorkflow` history that already has a measured death date at Temporal's 51,200-event
   * ceiling. This asserts nothing in the control plane reaches for the other spelling.
   */
  it('never reaches for the in-workflow upsert to do it', () => {
    const upserts = files
      .filter((f) => /upsertSearchAttributes|UpsertWorkflowSearchAttributes/.test(f.body))
      .map((f) => f.rel);
    expect(upserts, 'an in-workflow upsert writes an event per call:\n  ' + upserts.join('\n  ')).toEqual([]);
  });
});
