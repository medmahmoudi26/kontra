import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { readFileSync } from 'node:fs';

import { describe, expect, it } from 'vitest';

import {
  activeLakeWorkspace,
  activeNamespace,
  allNamespaces,
  bindNamespace,
  currentNamespace,
  inNamespace,
  namespaceFor,
  workspaceOfNamespace,
} from './workspaces';

/** ADR 0051 §2: the namespace a workspace's Runs live in. The CLI drives the same corpus. */
const corpus = JSON.parse(
  readFileSync(path.join(__dirname, '..', '..', '..', 'shared', 'conformance', 'workspace_namespace.json'), 'utf8')
) as {
  cases: Array<{ workspace: string; env_namespace: string | null; namespace: string }>;
  refused: Array<{ workspace: string }>;
};

const envWith = (ns: string | null): NodeJS.ProcessEnv => (ns === null ? {} : { KONTRA_NAMESPACE: ns });

describe('namespaceFor (shared/conformance/workspace_namespace.json)', () => {
  for (const c of corpus.cases) {
    it(`${JSON.stringify(c.workspace)} with KONTRA_NAMESPACE=${JSON.stringify(c.env_namespace)} -> ${c.namespace}`, () => {
      expect(namespaceFor(c.workspace, envWith(c.env_namespace))).toBe(c.namespace);
    });
  }
  for (const r of corpus.refused) {
    it(`refuses ${JSON.stringify(r.workspace)} rather than inventing a namespace for it`, () => {
      expect(() => namespaceFor(r.workspace, {})).toThrow(/workspace name/);
    });
  }
});

describe('the current namespace follows .current, read on every call', () => {
  it('switches the moment .current changes, with nothing restarted', () => {
    const parent = mkdtempSync(path.join(tmpdir(), 'ws-'));
    for (const n of ['default', 'hello', 'scraping']) mkdirSync(path.join(parent, n));
    const env = { KONTRA_WORKSPACES: parent };

    writeFileSync(path.join(parent, '.current'), 'hello\n');
    expect(currentNamespace(env)).toBe('ws-hello');
    writeFileSync(path.join(parent, '.current'), 'default\n');
    expect(currentNamespace(env)).toBe('default');

    expect(allNamespaces(env).sort()).toEqual(['default', 'ws-hello', 'ws-scraping']);
  });

  it('an install with no named-workspace layout has exactly the legacy namespace', () => {
    expect(currentNamespace({ KONTRA_NAMESPACE: 'acme' })).toBe('acme');
    expect(allNamespaces({ KONTRA_NAMESPACE: 'acme' })).toEqual(['acme']);
  });
});

describe('the lake follows the RUN, not the console (ADR 0051)', () => {
  function layout(current: string): NodeJS.ProcessEnv {
    const parent = mkdtempSync(path.join(tmpdir(), 'kontra-ws-'));
    for (const name of ['default', 'hello', 'scraping']) mkdirSync(path.join(parent, name));
    writeFileSync(path.join(parent, '.current'), current);
    return { KONTRA_WORKSPACES: parent };
  }

  it('maps every workspace namespace back to its workspace', () => {
    const env = layout('hello');
    for (const c of corpus.cases) {
      const ns = namespaceFor(c.workspace, { ...env, ...envWith(c.env_namespace) });
      const back = workspaceOfNamespace(ns, env);
      if (ns.startsWith('ws-')) expect(back).toBe(c.workspace);
      else expect(back).toBe('default');
    }
    // No named-workspace layout: the legacy namespace is the legacy (unnamed) lake address.
    expect(workspaceOfNamespace('default', {})).toBe('');
  });

  it('reads the scope before .current, so a switch mid-run cannot move a run\'s rows', async () => {
    const env = layout('scraping');
    const before = process.env.KONTRA_WORKSPACES;
    process.env.KONTRA_WORKSPACES = env.KONTRA_WORKSPACES;
    try {
      expect(activeLakeWorkspace()).toBe('scraping');
      expect(await inNamespace('ws-hello', async () => activeLakeWorkspace())).toBe('hello');
      expect(await inNamespace('default', async () => activeLakeWorkspace())).toBe('default');
      // The explicit override still wins: a migration points at an address on purpose.
      expect(activeLakeWorkspace({ ...env, KONTRA_LAKE_WORKSPACE: 'hello' })).toBe('hello');
    } finally {
      if (before === undefined) delete process.env.KONTRA_WORKSPACES;
      else process.env.KONTRA_WORKSPACES = before;
    }
  });

  it('binds a worker\'s namespace around every activity it runs', async () => {
    const seen: string[] = [];
    const bound = bindNamespace('ws-hello', {
      publishBatch: async (n: number) => {
        seen.push(activeNamespace());
        return n + 1;
      },
      notAFunction: 3,
    });
    expect(await bound.publishBatch(1)).toBe(2);
    expect(bound.notAFunction).toBe(3);
    expect(seen).toEqual(['ws-hello']);
    // And the binding does not leak out of the call.
    expect(activeNamespace()).not.toBe('ws-hello');
  });
});
