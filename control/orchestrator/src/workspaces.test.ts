import { mkdirSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  createWorkspace,
  describeWorkspaces,
  useWorkspace,
  workspaceRoot,
} from './workspaces';

function tmpParent(): string {
  const dir = path.join(os.tmpdir(), `kontra-ws-${process.pid}-${Math.random().toString(16).slice(2)}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

describe('named workspaces', () => {
  it('resolves the current child from KONTRA_WORKSPACES + .current', () => {
    const parent = tmpParent();
    mkdirSync(path.join(parent, 'hello'), { recursive: true });
    writeFileSync(path.join(parent, '.current'), 'hello\n');
    const env = { KONTRA_WORKSPACES: parent };
    expect(workspaceRoot(env)).toBe(path.join(parent, 'hello'));
    expect(describeWorkspaces(env).names).toEqual(['hello']);
  });

  it('falls back to legacy KONTRA_WORKSPACE when the parent is unset', () => {
    const legacy = tmpParent();
    expect(workspaceRoot({ KONTRA_WORKSPACE: legacy })).toBe(legacy);
  });

  it('switches and creates under the parent', () => {
    const parent = tmpParent();
    mkdirSync(path.join(parent, 'hello'), { recursive: true });
    writeFileSync(path.join(parent, '.current'), 'hello\n');
    const env = { KONTRA_WORKSPACES: parent };
    createWorkspace('two', { seed: false, use: true }, env);
    expect(useWorkspace('hello', env).current).toBe('hello');
    expect(describeWorkspaces(env).names).toEqual(['hello', 'two']);
  });
});
