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

import { AsyncLocalStorage } from 'node:async_hooks';
import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';

export const CURRENT_FILE = '.current';
export const DEFAULT_WORKSPACE = 'hello';

/**
 * A WORKSPACE NAME IS AN S3 BUCKET NAME, so it is bounded by the strictest of the three things it
 * has to be at once: a directory, a Temporal namespace, and a bucket (**ADR 0051**).
 *
 * This used to be `/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/`, which accepts five things AWS does not:
 * uppercase, underscores, a trailing `.`/`-`/`_`, names under 3 characters, and names of 64. Dots
 * are legal in a bucket name and still excluded here, because they break virtual-host-style TLS.
 *
 * BOTH ENDS ARE PINNED, not just the first. The old rule anchored the leading character and let
 * the rest run to the end, so `trailing-` passed — and a bucket name may not END on a hyphen any
 * more than it may start on one. Caught by the test below, which is the only thing that could
 * catch it here.
 *
 * THE TRAP IS THAT THIS CANNOT BE FOUND LOCALLY. SeaweedFS backs a bucket with a directory and is
 * far more permissive than S3, so `ws-Client_A` creates cleanly on a laptop and fails on
 * DigitalOcean Spaces — works here, breaks in the cloud, discovered at the worst moment.
 *
 * The bound is 60 rather than 63 because the address is `ws-<name>` and the prefix costs three.
 *
 * A SLUG FUNCTION WAS THE ALTERNATIVE AND IS WORSE. Mapping `Client_A` to `ws-client-a` is
 * many-to-one, so two workspaces can collide on one bucket — which is the isolation failure this
 * boundary exists to prevent, arriving through the code that was supposed to enforce it. Refusing
 * the name is the honest version: it fails at creation, where it can be fixed by typing.
 *
 * Costs nothing today: `bugbounty`, `default` and `scraping` all pass unchanged.
 */
const NAME_RE = /^[a-z0-9][a-z0-9-]{0,58}[a-z0-9]$/;

export class WorkspaceRefused extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'WorkspaceRefused';
  }
}

/** The prefix every derived address carries, so a workspace's stores are recognisable on sight. */
export const ADDRESS_PREFIX = 'ws-';

/**
 * WHERE A WORKSPACE'S THINGS LIVE — the three addresses, derived and never stored (**ADR 0051**).
 *
 * ── DERIVED, NOT LOOKED UP ──────────────────────────────────────────────────────────────────────
 *
 * There is no workspace→address table, and adding one walks back something this repo deleted on
 * purpose: `data/runWorkflows.ts` records that the `_kontra_dataset` name→path registry existed
 * "only for translating a hash back into a name, and nothing may walk that back". A table is a
 * lookup that must be kept in sync; a function cannot drift. Delete every row of anything and
 * these three strings are still computable from the folder name.
 *
 * ── ISOLATION IS BY ADDRESS, NEVER BY FILTER (§3) ───────────────────────────────────────────────
 *
 * A `WHERE workspace = ?` is one forgotten clause away from not existing, and the forgotten clause
 * reads as an ordinary result. A separate namespace, bucket and catalog fail the other way: get
 * the address wrong and you see nothing, loudly.
 *
 * ── THERE IS NO GRANDFATHER CLAUSE HERE, AND THAT IS DELIBERATE ─────────────────────────────────
 *
 * `bugbounty` is meant to end up holding what the shared lake holds today, but that is a ONE-TIME
 * DATA MOVE performed by the migration — not a special case in this function. If `default` mapped
 * to the old addresses and everything else to `ws-…`, the derivation would carry a permanent
 * exception, and an exception inside an address function is a filter wearing an address's clothes.
 * So this stays total: every workspace, including `default`, derives `ws-<name>`.
 */
export interface WorkspaceAddress {
  /** The workspace this addresses — the folder name under `KONTRA_WORKSPACES`. */
  workspace: string;
  /**
   * Temporal namespace. THE ONLY ONE OF THE THREE TEMPORAL WILL ENFORCE FOR US (§2): a client
   * bound here cannot read another by constructing a different query. The other two need code to
   * honour them.
   */
  namespace: string;
  /**
   * S3 bucket. A BUCKET RATHER THAN A PREFIX, because `ducklake_delete_orphaned_files` deletes
   * files under a catalog's DATA_PATH that "the catalog never knew about" — which is precisely
   * what another workspace's parquet looks like. Sharing a bucket makes lake maintenance a
   * cross-workspace deletion machine; separate buckets make it physically impossible.
   */
  bucket: string;
  /**
   * DuckLake catalog. A separate Postgres DATABASE, not a schema: `metaSchemaFor` returns
   * `public` for every `postgres:` catalog, so two DuckLakes in one database would put their
   * `ducklake_*` metadata tables in the same place and corrupt each other.
   */
  catalog: string;
}

/**
 * The Postgres database name for a workspace's catalog.
 *
 * Hyphens become underscores because an unquoted Postgres identifier cannot hold one. That
 * substitution is INJECTIVE ONLY BECAUSE `NAME_RE` FORBIDS UNDERSCORES — if both were legal,
 * `a-b` and `a_b` would collide on one database, which is the isolation failure this whole module
 * exists to prevent arriving through the code meant to enforce it. The two rules are load-bearing
 * together; loosening either one alone breaks this.
 */
function catalogDbName(workspace: string): string {
  return `kontra_ducklake_${ADDRESS_PREFIX}${workspace}`.replace(/-/g, '_');
}

/**
 * Derive a workspace's three addresses.
 *
 * The catalog keeps the host/user/password of {@link https://duckdb.org/docs/extensions/ducklake
 * DuckLake}'s configured connstring and swaps only `dbname`, so credentials and host stay in one
 * place (the environment) and only the part that identifies the workspace varies.
 */
export function workspaceAddress(
  name: string,
  env: NodeJS.ProcessEnv = process.env
): WorkspaceAddress {
  assertWorkspaceName(name);
  const base = (env.KONTRA_DUCKLAKE_CATALOG ?? '').trim();
  const db = catalogDbName(name);
  // Swap `dbname=` in place when a connstring is configured; otherwise name a file catalog, which
  // is what a single-process install runs (ADR 0031 §1b).
  // The boundary includes `:` as well as whitespace, because the FIRST key sits directly against
  // the scheme — `postgres:dbname=…`, with no space. Anchoring on `(^|\s)` alone misses exactly
  // that case and appends a SECOND `dbname=`, which libpq resolves last-wins, so it happens to
  // work and reads as a connstring with two of everything.
  const DBNAME = /(^|[\s:])dbname=\S+/;
  const catalog = base.startsWith('postgres:')
    ? DBNAME.test(base)
      ? base.replace(DBNAME, `$1dbname=${db}`)
      : `${base} dbname=${db}`
    : `${ADDRESS_PREFIX}${name}.ducklake`;
  return {
    workspace: name,
    namespace: `${ADDRESS_PREFIX}${name}`,
    bucket: `${ADDRESS_PREFIX}${name}`,
    catalog,
  };
}

/** The workspace whose Temporal namespace is the install's legacy one (ADR 0051 §6). */
export const LEGACY_WORKSPACE = 'default';

/**
 * WHICH TEMPORAL NAMESPACE A WORKSPACE'S RUNS LIVE IN — the address Temporal enforces (ADR 0051 §2).
 *
 * `default`, and an install with no named workspace, keep the legacy namespace: `KONTRA_NAMESPACE`,
 * or `default`. That is §6, and it is what keeps every Run that existed before isolation readable:
 * they are all in that namespace and they cannot be moved, because Temporal has no way to move a
 * history between namespaces. Every other workspace gets `ws-<name>`, its own namespace, so a
 * client bound to it cannot read another workspace's Runs by constructing a different query.
 *
 * NOT `workspaceAddress(name).namespace`, which derives `ws-default` too. That function's "no
 * grandfather clause" is right for the lake, whose old data a migration moves; here no migration
 * can move anything, so `default` stays where its Runs are.
 *
 * Pinned against the CLI's copy by `shared/conformance/workspace_namespace.json`.
 */
export function namespaceFor(workspace: string, env: NodeJS.ProcessEnv = process.env): string {
  const legacy = (env.KONTRA_NAMESPACE ?? '').trim() || 'default';
  if (workspace === '' || workspace === LEGACY_WORKSPACE) return legacy;
  assertWorkspaceName(workspace);
  return `${ADDRESS_PREFIX}${workspace}`;
}

/**
 * The workspace this install is looking at RIGHT NOW: `.current` under `KONTRA_WORKSPACES`, read on
 * every call, or '' for an install with no named-workspace layout.
 *
 * READ PER CALL, NOT AT BOOT (ADR 0051 §4). Switching workspace writes `.current` and nothing
 * restarts, so a value captured at module load is exactly the single process-wide namespace this
 * replaces. The console treats the switch as a server fact (`PUT /api/workspaces/current`), and
 * this is the server reading it.
 */
export function activeWorkspace(env: NodeJS.ProcessEnv = process.env): string {
  const parent = workspacesParent(env);
  return parent ? readCurrentName(parent) : '';
}

/**
 * The workspace a path is in: the first segment under `KONTRA_WORKSPACES`, or `undefined` when the
 * path is outside it or there is no named-workspace layout. Resolved against the configured parent,
 * not guessed from a path segment called `workspaces`, which any directory could be named.
 */
export function workspaceOfPath(p: string, env: NodeJS.ProcessEnv = process.env): string | undefined {
  const parent = workspacesParent(env);
  if (!parent) return undefined;
  const rel = path.relative(parent, path.resolve(p));
  if (rel === '' || rel.startsWith('..') || path.isAbsolute(rel)) return undefined;
  return rel.split(path.sep)[0] || undefined;
}

/** The Temporal namespace of the workspace this install is looking at now. */
export function currentNamespace(env: NodeJS.ProcessEnv = process.env): string {
  return namespaceFor(activeWorkspace(env), env);
}

/** Every workspace's namespace, the legacy one included: what a per-workspace worker pool serves. */
export function allNamespaces(env: NodeJS.ProcessEnv = process.env): string[] {
  const parent = workspacesParent(env);
  const names = parent ? listWorkspaceNames(parent) : [];
  const out = new Set<string>([namespaceFor('', env)]);
  for (const n of names) {
    try {
      out.add(namespaceFor(n, env));
    } catch {
      // A folder whose name the rule refuses is not a workspace, and gets no namespace.
    }
  }
  return [...out];
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

/**
 * WHICH WORKSPACE'S LAKE THIS INSTALL READS AND WRITES (issue 10, ADR 0051).
 *
 * ── ONE QUESTION, ONE ANSWER ────────────────────────────────────────────────────────────────────
 *
 * `KONTRA_LAKE_WORKSPACE` was slice 08's switch, and as a SECOND way of saying which workspace is
 * active it could disagree with the first. `.current` says `bugbounty` while the variable says
 * `default` and the install serves one workspace's code against another's data — silently, because
 * both answers are individually valid. Two sources of truth for one question is the drift this
 * codebase already refuses elsewhere (`workspaceAddress` is a derivation precisely so "a function
 * cannot drift").
 *
 * So `.current` is the answer, and the variable is an OVERRIDE for the cases that have no
 * `.current` to read: a cluster install with no workspaces mount, a migration pointing at an
 * address deliberately, and the tests.
 *
 * ── AND IT STILL RETURNS '' WHEN NOTHING SAYS ───────────────────────────────────────────────────
 *
 * Empty means the LEGACY address, unchanged — which is what an install with no named-workspace
 * layout has always used and must keep using. Deriving `ws-<something>` from a guess would point a
 * working install at an empty catalog, and an empty catalog reads exactly like the 2026-09-28 wipe.
 */
export function activeLakeWorkspace(env: NodeJS.ProcessEnv = process.env): string {
  const explicit = (env.KONTRA_LAKE_WORKSPACE ?? '').trim();
  if (explicit) return explicit;
  // INSIDE A NAMESPACE SCOPE, THE RUN'S WORKSPACE, NOT THE CONSOLE'S (ADR 0051). An activity that
  // publishes a run's rows runs in a per-namespace worker that binds its namespace (see
  // `bindNamespace`), and a background pass binds each namespace in turn. Reading `.current` there
  // would put a run's rows in whichever workspace the console happened to have selected when the
  // batch landed — a silent wrong-lake commit, which is exactly the failure an address exists to
  // make impossible.
  const scoped = namespaceScope.getStore();
  if (scoped !== undefined) return workspaceOfNamespace(scoped, env);
  const parent = workspacesParent(env);
  return parent ? readCurrentName(parent) : '';
}

/**
 * The workspace whose runs live in `namespace` — the inverse of {@link namespaceFor}.
 *
 * `ws-<name>` is `<name>`. The legacy namespace is the `default` workspace's when there is a
 * named-workspace layout, and the legacy (unnamed) address when there is not, which is what
 * `activeLakeWorkspace` answered for that install before this existed. Pinned against
 * `namespaceFor` by `shared/conformance/workspace_namespace.json`.
 */
export function workspaceOfNamespace(namespace: string, env: NodeJS.ProcessEnv = process.env): string {
  if (namespace.startsWith(ADDRESS_PREFIX)) return namespace.slice(ADDRESS_PREFIX.length);
  return workspacesParent(env) ? LEGACY_WORKSPACE : '';
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
      // THE MESSAGE NAMES THE RULE `NAME_RE` ACTUALLY ENFORCES. It used to read "use letters,
      // digits, . _ -" — all three of which this regex refuses — so a reader holding
      // `demo_workspace` was told their name was legal by the error rejecting it.
      `workspace name ${JSON.stringify(name)}: use lowercase letters, digits and dashes ` +
        `(2–60 chars, start and end alphanumeric). No underscore, dot or uppercase — see ` +
        `catalogDbName: the hyphen-to-underscore mapping is only injective while underscores ` +
        `are illegal here`
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

/**
 * A namespace pinned for everything one piece of work does, carried through its awaits.
 *
 * WHY A SCOPE AND NOT A PARAMETER. Every reader in this module reaches Temporal through
 * `getClient`, and the background loops (the report renderer, the history archiver) must walk
 * EVERY workspace, not just the one the console has selected. Threading a namespace argument through
 * thirty functions would be thirty places to forget it. A forgotten one silently reads the console's
 * workspace instead, and that looks like an ordinary empty result. Inside a scope, getClient answers
 * with the scope's namespace; outside one, with the console's current workspace.
 */
const namespaceScope = new AsyncLocalStorage<string>();

/** Run `fn` with every `getClient` inside it bound to `namespace`. */
export function inNamespace<T>(namespace: string, fn: () => Promise<T>): Promise<T> {
  return namespaceScope.run(namespace, fn);
}

/** The namespace `getClient` would answer with right now. */
export function activeNamespace(): string {
  return namespaceScope.getStore() ?? currentNamespace();
}

/**
 * `activities` with every function run inside `namespace`'s scope.
 *
 * WHAT A PER-NAMESPACE WORKER IS FOR. `runPerNamespace` builds one worker per workspace namespace,
 * and every activity that worker takes belongs to a run in that namespace — so the namespace is
 * bound once, here, around each call, and everything the activity reaches (the lake, the report
 * tables, `getClient`) addresses the run's workspace without being told. One wrapper at the worker
 * rather than a call per activity, because a forgotten call is the silent wrong-workspace read.
 */
export function bindNamespace<T extends Record<string, unknown>>(namespace: string, activities: T): T {
  const out: Record<string, unknown> = {};
  for (const [name, fn] of Object.entries(activities)) {
    out[name] =
      typeof fn === 'function'
        ? (...args: unknown[]) => inNamespace(namespace, async () => (fn as (...a: unknown[]) => unknown)(...args))
        : fn;
  }
  return out as T;
}
