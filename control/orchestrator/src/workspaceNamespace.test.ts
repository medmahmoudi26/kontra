import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { readFileSync } from 'node:fs';

import { describe, expect, it } from 'vitest';

import { allNamespaces, currentNamespace, namespaceFor } from './workspaces';

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
