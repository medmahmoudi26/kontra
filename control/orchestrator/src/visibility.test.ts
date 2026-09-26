/**
 * Search-attribute registration (ADR 0029 §4, issue 03).
 *
 * `KontraTag` is the projection an in-workflow `publish(..., tag=…)` mirrors to over the live
 * window — a fast query path, never the retention truth (that is the Dataset record). For the fast
 * path to exist at all it must be REGISTERED on the namespace, in the next free Keyword slot beside
 * the three kontra already registers. This pins that registration; the "never read as truth" half
 * is a discipline the sweeper (issue 04) and the record store carry, not something this file asserts.
 */

import { describe, expect, it } from 'vitest';

import {
  KONTRA_INTERNAL_WORKFLOW_TYPES,
  buildRunDiscoveryQuery,
  buildWorkflowIdQuery,
  registerSearchAttributes,
} from './visibility';
import { LEASE_WORKFLOW } from './lease';
import { SERVE_DEV_WORKFLOW, serveDevWorkflowId } from './queues';

/** A stand-in Connection that records every addSearchAttributes call — the one seam registration
 *  reaches. Registration must be idempotent and self-healing, so an AlreadyExists is swallowed;
 *  here every add succeeds and we read back which names were offered. */
function fakeConnection(onAdd: (name: string) => void) {
  return {
    operatorService: {
      async addSearchAttributes(req: { searchAttributes: Record<string, unknown> }) {
        for (const name of Object.keys(req.searchAttributes)) onAdd(name);
      },
    },
  } as unknown as Parameters<typeof registerSearchAttributes>[0];
}

describe('registerSearchAttributes', () => {
  it('registers KontraTag beside the other kontra search attributes', async () => {
    const registered: string[] = [];
    await registerSearchAttributes(fakeConnection((n) => registered.push(n)), 'default');

    // KontraTag is the new one this slice adds — without it the live-window query has no column.
    expect(registered).toContain('KontraTag');
    // The three that were already there stay, so the mirror is additive, not a replacement.
    expect(registered).toEqual(
      expect.arrayContaining(['KontraTenant', 'KontraRunId', 'KontraActor', 'KontraTag'])
    );
  });

  it('never throws — a registration hiccup must not break run reads', async () => {
    const flaky = {
      operatorService: {
        async addSearchAttributes() {
          throw new Error('already exists');
        },
      },
    } as unknown as Parameters<typeof registerSearchAttributes>[0];
    await expect(registerSearchAttributes(flaky, 'default')).resolves.toBeUndefined();
  });
});

/**
 * THE PERPETUAL WORKFLOWS ARE EXCLUDED, AND AFTER kontra#10 THAT IS LOAD-BEARING.
 *
 * Both now continue-as-new when the server suggests it, so each resolves to a CHAIN of executions
 * rather than one. `historyArchive.ts` sweeps by workflow id with no `execId`, and `fetchRunHistory`
 * documents what Temporal answers to that — "whichever ran last" — so a swept chain would be
 * archived as its FINAL LEG ALONE with every earlier leg silently absent. That is finding F4 of the
 * event-log audit: fixing F2 makes F1 worse unless the two land together.
 *
 * They land together by EXCLUSION here, which is correct for these two because neither is a caller's
 * Run. It is not a general answer, and `visibility.ts` says so at the entry: a CALLER workflow that
 * continues-as-new (kontra#12) is a chain the sweep cannot exclude.
 */
describe('the internal workflow types', () => {
  it('excludes both perpetual workflows, so the sweep never meets a continued chain', () => {
    expect(KONTRA_INTERNAL_WORKFLOW_TYPES).toContain('wardenWorkflow');
    expect(KONTRA_INTERNAL_WORKFLOW_TYPES).toContain('fleetLeaseWorkflow');
  });

  it('keeps them out of the discovery query rather than filtering afterwards', () => {
    // In the QUERY, because a post-filter would still have paged them — and at ten Machines per
    // Fleet that is the page the caller's own Run falls off the bottom of.
    const q = buildRunDiscoveryQuery(KONTRA_INTERNAL_WORKFLOW_TYPES);
    expect(q).toContain("'wardenWorkflow'");
    expect(q).toContain("'fleetLeaseWorkflow'");
    expect(q).toMatch(/WorkflowType NOT IN \(/);
  });

  it('names the Lease workflow exactly as `lease.ts` registers it', () => {
    // A type name that disagrees with the registration excludes NOTHING and fails silently — the
    // list would look right and the Runs page would still carry the rows.
    expect(KONTRA_INTERNAL_WORKFLOW_TYPES).toContain(LEASE_WORKFLOW);
  });

  /**
   * THE HIDING HALF AND THE SHOWING HALF MUST NAME THE SAME TYPE.
   *
   * serve-dev is the one entry on this list that is drawn somewhere else: the Actors page reads a
   * folder's serve history through `temporalClient.listServes`, which queries this type by name.
   * Two literals would fail in opposite directions — the Runs page filling with infrastructure
   * (wrong and loud) or the history reporting "nothing has ever served this" about a folder served
   * all week (wrong and silent). Pinning the exclusion against the shared constant is what makes
   * the second one impossible.
   */
  it('hides serve-dev under the same name `queues.ts` starts and lists it under', () => {
    expect(KONTRA_INTERNAL_WORKFLOW_TYPES).toContain(SERVE_DEV_WORKFLOW);
    // …and the exclusion really reaches the query, not just the array.
    expect(buildRunDiscoveryQuery(KONTRA_INTERNAL_WORKFLOW_TYPES)).toContain(
      `'${SERVE_DEV_WORKFLOW}'`
    );
  });
});

/**
 * The query behind a REUSED workflow id's history — `serve-dev/at:<folder>` and `kontra-fleet/<stack>`
 * are both one id with many executions under it.
 */
describe('buildWorkflowIdQuery', () => {
  it('narrows to one type AND one id, so a folder sees only its own serves', () => {
    const q = buildWorkflowIdQuery(SERVE_DEV_WORKFLOW, serveDevWorkflowId('at:/w/actors/desync'));
    expect(q).toBe(
      "WorkflowType = 'serveDevWorkflow' AND WorkflowId = 'serve-dev/at:/w/actors/desync'"
    );
  });

  it('has no ORDER BY — SQLite visibility rejects it outright', () => {
    // The scar this file's neighbours carry: `temporal start-dev` and CI run SQLite visibility,
    // which refuses ORDER BY, so every listing in `temporalClient.ts` sorts client-side instead.
    expect(buildWorkflowIdQuery(SERVE_DEV_WORKFLOW, 'serve-dev/at:/w')).not.toMatch(/ORDER BY/i);
  });

  it("escapes a quote in the id rather than ending the literal early", () => {
    // A folder path can legally contain an apostrophe (`/w/mo's actors/x`), and the id is that path.
    // Unescaped it would close the string literal and hand the rest of the path to the parser.
    const q = buildWorkflowIdQuery(SERVE_DEV_WORKFLOW, "serve-dev/at:/w/mo's actors/x");
    expect(q).toContain("'serve-dev/at:/w/mo''s actors/x'");
  });
});
