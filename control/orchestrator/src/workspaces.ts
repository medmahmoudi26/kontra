/**
 * Named workspaces under KONTRA_WORKSPACES — one parent, many children, `.current` picks one.
 *
 * A workspace is a code folder (actors/ + workflows/). Datasets, runs and login stay cluster-wide
 * — which ADR 0051 says is the wrong half of the boundary: the workspace is meant to decide where
 * everything that code produces GOES, not only which code exists. Every store below resolves its
 * target once from the environment at process boot (`NAMESPACE` is a module-level `const`), so a
 * switch cannot move them today and does not try to. See docs/adr/0051.
 * Switching rewrites `.current`; discovery and watch re-read it. Nothing remounts.
 */

import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';

export const CURRENT_FILE = '.current';
export const DEFAULT_WORKSPACE = 'hello';

const NAME_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/;

export class WorkspaceRefused extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'WorkspaceRefused';
  }
}

/** Parent folder Compose bind-mounts. Empty means no named-workspace layout. */
export function workspacesParent(env: NodeJS.ProcessEnv = process.env): string {
  const raw = (env.KONTRA_WORKSPACES ?? '').trim();
  return raw ? path.resolve(raw) : '';
}

/**
 * The active code root for discovery.
 *
 * Prefers `KONTRA_WORKSPACES` + `.current`. Falls back to legacy `KONTRA_WORKSPACE` (single tree).
 */
export function workspaceRoot(env: NodeJS.ProcessEnv = process.env): string {
  const parent = workspacesParent(env);
  if (parent) {
    const name = readCurrentName(parent);
    if (!name) return '';
    const child = path.join(parent, name);
    return existsSync(child) ? child : '';
  }
  return (env.KONTRA_WORKSPACE ?? '').trim();
}

export function readCurrentName(parent: string): string {
  const file = path.join(parent, CURRENT_FILE);
  if (!existsSync(file)) return '';
  return readFileSync(file, 'utf8').trim();
}

export function writeCurrentName(parent: string, name: string): void {
  assertWorkspaceName(name);
  writeFileSync(path.join(parent, CURRENT_FILE), `${name}\n`, 'utf8');
}

export function assertWorkspaceName(name: string): void {
  if (!NAME_RE.test(name) || name === '.' || name === '..') {
    throw new WorkspaceRefused(
      `workspace name ${JSON.stringify(name)}: use letters, digits, . _ - (1–64 chars, start alnum)`
    );
  }
}

export function listWorkspaceNames(parent: string): string[] {
  if (!existsSync(parent)) return [];
  return readdirSync(parent, { withFileTypes: true })
    .filter((e) => e.isDirectory() && !e.name.startsWith('.'))
    .map((e) => e.name)
    .sort((a, b) => a.localeCompare(b));
}

export interface WorkspaceList {
  parent: string;
  current: string;
  names: string[];
  /** Absolute path of the current child, or '' when none. */
  currentPath: string;
  /** Host remount hint when the parent env/mount is missing. */
  mountHint: string;
}

export function describeWorkspaces(env: NodeJS.ProcessEnv = process.env): WorkspaceList {
  const parent = workspacesParent(env);
  const mountHint =
    'mkdir -p workspaces beside kontra/ and kontra-console/, then docker compose up -d';
  if (!parent) {
    return { parent: '', current: '', names: [], currentPath: '', mountHint };
  }
  const names = listWorkspaceNames(parent);
  const current = readCurrentName(parent);
  const currentPath = current ? path.join(parent, current) : '';
  return { parent, current, names, currentPath, mountHint };
}

export function useWorkspace(name: string, env: NodeJS.ProcessEnv = process.env): WorkspaceList {
  const parent = workspacesParent(env);
  if (!parent) throw new WorkspaceRefused('KONTRA_WORKSPACES is unset — the parent mount is missing');
  assertWorkspaceName(name);
  const child = path.join(parent, name);
  if (!existsSync(child)) {
    throw new WorkspaceRefused(`workspace ${JSON.stringify(name)} does not exist under ${parent}`);
  }
  writeCurrentName(parent, name);
  return describeWorkspaces(env);
}

export function createWorkspace(
  name: string,
  opts: { seed?: boolean; use?: boolean; seedDir?: string } = {},
  env: NodeJS.ProcessEnv = process.env
): WorkspaceList {
  const parent = workspacesParent(env);
  if (!parent) throw new WorkspaceRefused('KONTRA_WORKSPACES is unset — the parent mount is missing');
  assertWorkspaceName(name);
  mkdirSync(parent, { recursive: true });
  const child = path.join(parent, name);
  if (existsSync(child)) {
    throw new WorkspaceRefused(`workspace ${JSON.stringify(name)} already exists`);
  }
  mkdirSync(child, { recursive: true });
  // UNDO THE MKDIR IF THE SEED REFUSES. The directory is made before the only step that can fail,
  // so a refusal used to leave a half-made workspace on the bind mount — and then the retry hit
  // "already exists" above, which is a state the console cannot get out of. MEASURED: an image
  // with no /opt/kontra/seed answered 400 and left an empty `test/` the operator had to delete by
  // hand. The seed is present now; this is what keeps the failure recoverable if it is not.
  if (opts.seed) {
    try {
      seedInto(child, opts.seedDir ?? (env.KONTRA_SEED_DIR ?? '/opt/kontra/seed').trim());
    } catch (err) {
      rmSync(child, { recursive: true, force: true });
      throw err;
    }
  }
  if (opts.use !== false) {
    writeCurrentName(parent, name);
  }
  return describeWorkspaces(env);
}

/** Copy starter templates into an empty workspace directory. */
export function seedInto(dir: string, seedDir: string): void {
  if (!existsSync(seedDir)) {
    throw new WorkspaceRefused(`seed templates not found at ${seedDir}`);
  }
  mkdirSync(dir, { recursive: true });
  const ents = readdirSync(dir).filter((n) => n !== '.' && n !== '..' && !n.startsWith('.'));
  if (ents.length > 0) return;

  const tmp = path.join(dir, `.seed-${process.pid}`);
  mkdirSync(tmp, { recursive: true });
  try {
    copyTree(seedDir, tmp);
    for (const name of readdirSync(tmp)) {
      renameSync(path.join(tmp, name), path.join(dir, name));
    }
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}

function copyTree(src: string, dst: string): void {
  mkdirSync(dst, { recursive: true });
  for (const ent of readdirSync(src, { withFileTypes: true })) {
    const from = path.join(src, ent.name);
    const to = path.join(dst, ent.name);
    if (ent.isDirectory()) copyTree(from, to);
    else writeFileSync(to, readFileSync(from));
  }
}
