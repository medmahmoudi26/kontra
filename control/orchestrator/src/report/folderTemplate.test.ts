/**
 * A run started by `kontra workflow start` pins nothing, and used to get the DEFAULT report even
 * when its folder had a `report.md`. These pin the lookup that finds it, and the sweep's use of it.
 */
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { folderTemplateFor, workspaceDirFor } from './folderTemplate';
import { ReportStore } from './store';
import { contextForRun, sweepFinishedRuns, templateHash, type SweepRun } from './sweep';

let parent: string;
const env = () => ({ KONTRA_WORKSPACES: parent }) as NodeJS.ProcessEnv;

function workflow(workspace: string, folder: string, cls: string, report?: string): void {
  const dir = path.join(parent, workspace, 'workflows', folder);
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'workflow.json'), JSON.stringify({ name: folder, version: '0.1.0', workflow: cls }));
  if (report !== undefined) writeFileSync(path.join(dir, 'report.md'), report);
}

beforeEach(() => {
  parent = mkdtempSync(path.join(tmpdir(), 'kontra-ws-'));
});
afterEach(() => rmSync(parent, { recursive: true, force: true }));

describe('workspaceDirFor', () => {
  it('maps a workspace namespace to its folder and the legacy namespace to `default`', () => {
    expect(workspaceDirFor('ws-scraping', env())).toBe(path.join(parent, 'scraping'));
    expect(workspaceDirFor('default', env())).toBe(path.join(parent, 'default'));
  });
});

describe('folderTemplateFor', () => {
  it('finds the one folder in the run\'s workspace that declares its type', () => {
    workflow('scraping', 'redditscan', 'RedditScan', '# Scout {{ run.id }}');
    workflow('scraping', 'other', 'Other', '# other');
    expect(folderTemplateFor('RedditScan', 'ws-scraping', env())).toEqual({
      folder: path.join(parent, 'scraping', 'workflows', 'redditscan'),
      text: '# Scout {{ run.id }}',
      workspace: 'scraping',
    });
  });

  it('does not look in another workspace', () => {
    workflow('scraping', 'redditscan', 'RedditScan', '# Scout');
    expect(folderTemplateFor('RedditScan', 'ws-hello', env())).toBeUndefined();
    expect(folderTemplateFor('RedditScan', 'default', env())).toBeUndefined();
  });

  it('answers nothing when two folders declare the type, rather than guessing', () => {
    workflow('hello', 'a', 'Canary', '# a');
    workflow('hello', 'b', 'Canary', '# b');
    expect(folderTemplateFor('Canary', 'ws-hello', env())).toBeUndefined();
  });

  it('finds a folder with no report.md, which means the default report', () => {
    workflow('hello', 'canary', 'Canary');
    expect(folderTemplateFor('Canary', 'ws-hello', env())).toEqual({
      folder: path.join(parent, 'hello', 'workflows', 'canary'),
      workspace: 'hello',
    });
  });
});

describe('a run nothing pinned', () => {
  const RUN: SweepRun = {
    runId: 'redditscan-1791572567',
    status: 'completed',
    startedAt: 1_791_572_567_000,
    closedAt: 1_791_572_610_000,
    type: 'RedditScan',
  };
  const TEMPLATE = '# Scout for {{ run.id }}\n';

  it('renders its folder\'s report.md, and pins it so the next render reads the same one', async () => {
    const store = new ReportStore({ url: ':memory:' });
    let lookups = 0;
    const deps = {
      store,
      list: async () => [RUN],
      io: async () => ({ input: {}, output: {} }),
      close: async () => undefined,
      // Rendered ten minutes after it started: a pin this late is the folder's, not the start's.
      now: () => 1_791_573_167_000,
      folderTemplate: (type: string) => {
        lookups += 1;
        return type === 'RedditScan' ? { folder: '/x', text: TEMPLATE, workspace: 'scraping' } : undefined;
      },
    };
    const built = await contextForRun(RUN, {}, deps);
    expect(built.template).toBe(TEMPLATE);
    expect(built.templateHash).toBe(templateHash(TEMPLATE));
    expect(built.pinned).toBe(true);
    expect((await store.template(RUN.runId))?.templateText).toBe(TEMPLATE);

    const result = await sweepFinishedRuns(deps);
    expect(result.rendered).toBe(1);
    expect(result.noTemplate).toBe(0);
    const stored = await store.version(RUN.runId);
    expect(stored?.templateHash).toBe(templateHash(TEMPLATE));
    const snapshot = JSON.parse(stored!.snapshotJson!);
    expect(JSON.stringify(snapshot)).toContain('Scout for redditscan-1791572567');
    // And says where the template came from, because it was not pinned at the start.
    expect(snapshot.warnings?.[0]).toMatch(/started outside the console/);
    // Pinned on the first render, so the sweep did not look again.
    expect(lookups).toBe(1);
  });

  it('still gets the default report, with its warning, when no folder declares the type', async () => {
    const store = new ReportStore({ url: ':memory:' });
    const result = await sweepFinishedRuns({
      store,
      list: async () => [RUN],
      io: async () => ({ input: {}, output: {} }),
      close: async () => undefined,
      now: () => 1,
      folderTemplate: () => undefined,
    });
    expect(result.noTemplate).toBe(1);
    expect(JSON.parse((await store.version(RUN.runId))!.snapshotJson!).warnings?.[0]).toMatch(/No report.md was pinned/);
  });
});
