/**
 * The control surface's guards.
 *
 * These two routes are UNAUTHENTICATED by the operator's choice, which moves the whole weight of
 * this file onto two things: what `serve` is allowed to run, and whether the choice is visible.
 *
 * Path confinement is the one that has to be right. `serve` executes a file from this host's disk
 * as this process's user, so the boundary is "inside the checkout" — and the two ways to leave a
 * directory are `..` and a symlink. A test that only tried `..` would pass against an
 * implementation that is trivially escapable.
 */

import {
  chmodSync,
  existsSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  symlinkSync,
  writeFileSync,
  rmSync,
} from 'node:fs';
import os, { tmpdir } from 'node:os';
import * as path from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { defaultRoot, inspectFolder } from './sources';
import { CURRENT_FILE } from './workspaces';
import { KontraTenant } from './visibility';
import {
  cliDetail,
  ControlRefused,
  describeExposure,
  kontraBin,
  listWorkflows,
  readWorkflow,
  resolveWorkflowFile,
  serveWorkflow,
  setRegisteredFolders,
  startRun,
  workflowQueue,
  workflowRoot,
  workflowSession,
} from './workflowControl';

/**
 * The Temporal client, stubbed — so `startRun` can be driven PAST its refusals.
 *
 * Every other test in this describe stops before the dial (no pollers, bad type, no manifest), which
 * is why this file never needed a client. Proving that a start SNAPSHOTS the caller's manifest
 * identity (ADR 0029 §2) requires the opposite: a start that succeeds. The stub returns the workflow
 * id it was handed, which is what a real handle does and what a Run IS (ADR 0023 §12).
 */
/** What `startRun` handed Temporal, captured — so the search attributes can be asserted. */
const started = vi.hoisted(() => ({ opts: undefined as Record<string, unknown> | undefined }));

/** What `serveWorkflow` handed the infra queue, and what it got back. See the mock below. */
const executed = vi.hoisted(() => ({
  type: undefined as string | undefined,
  opts: undefined as Record<string, unknown> | undefined,
  /** Settable per test: a throw here is how the infra side reports a refusal. */
  fail: undefined as Error | undefined,
  result: { worker: 'nscheck', queue: '', detail: '' },
}));

const { clientNamespaces } = vi.hoisted(() => ({ clientNamespaces: [] as string[] }));
vi.mock('./temporalClient', () => ({
  // `NAMESPACE` IS EXPORTED BY THE REAL MODULE AND MUST BE HERE TOO. A factory that returns only
  // `getClient` leaves every other binding `undefined`, and the tenant stamp below would write
  // `value: undefined` — which is the shape of bug a whole-module mock invites and a partial one
  // would have hidden.
  LEGACY_NAMESPACE: 'test-namespace',
  // ONE FAKE BEHIND BOTH. `startRun` opens the client of the RUN'S namespace through `clientFor`
  // (ADR 0051); everything else still reaches it through `getClient`. `clientNamespaces` records
  // which namespace each `clientFor` call asked for, so a test can hold the stamp to it.
  clientFor: vi.fn(async (namespace: string) => {
    clientNamespaces.push(namespace);
    const { getClient } = await import('./temporalClient');
    return getClient();
  }),
  getClient: vi.fn(async () => ({
    workflow: {
      start: async (_type: string, opts: { workflowId: string }) => {
        started.opts = opts as unknown as Record<string, unknown>;
        return { workflowId: opts.workflowId };
      },
      /*
       * `execute`, BECAUSE THE SERVE IS A HOP NOW AND NOT A SPAWN.
       *
       * `serveWorkflow` used to shell `kontra workflow serve` from THIS process; it asks
       * `kontra-infra` to do it instead, over the queue those two already share, because starting a
       * container needs the Docker socket and this process must not have one. So what a test of
       * this function can assert is the REQUEST — and the request is the security property: an id
       * and a kind, never a path or an argv. What the far side then runs is
       * `activities/serveDev.test.ts`'s subject.
       */
      execute: async (type: string, opts: Record<string, unknown>) => {
        executed.type = type;
        executed.opts = opts;
        if (executed.fail) throw executed.fail;
        return executed.result;
      },
    },
  })),
}));

let root: string;
let outside: string;
const saved = { ...process.env };

beforeEach(() => {
  root = mkdtempSync(path.join(tmpdir(), 'kontra-root-'));
  outside = mkdtempSync(path.join(tmpdir(), 'kontra-outside-'));
  mkdirSync(path.join(root, 'examples', 'python', 'workflows'), { recursive: true });
  writeFileSync(path.join(root, 'examples', 'python', 'workflows', 'nscheck.py'), '# workflow\n');
  writeFileSync(path.join(outside, 'secrets.py'), '# not yours\n');
  process.env.KONTRA_WORKFLOW_ROOT = root;
  // THE HOP'S RECORDER IS MODULE STATE, so a failure left set by one case would fail every case
  // after it — the shape of flake this file's `afterEach` already guards the provider against.
  executed.type = undefined;
  executed.opts = undefined;
  executed.fail = undefined;
  executed.result = { worker: 'nscheck', queue: '', detail: '' };
});

afterEach(() => {
  // THE PROVIDER IS MODULE STATE. Left installed it would widen the boundary for every case after
  // it, including the traversal refusals — which would then pass for the wrong reason.
  setRegisteredFolders(() => new Map());
  process.env = { ...saved };
  rmSync(root, { recursive: true, force: true });
  rmSync(outside, { recursive: true, force: true });
});

/**
 * issue #4 — a registered folder is a workflow wherever it lives.
 *
 * THE BUG WAS A ROW YOU COULD SEE AND COULD NOT READ. Registration accepts any absolute path and
 * the Workflows list draws what it accepted; this resolver looked only under the default root. So a
 * folder registered from `~/kontra-workflows/python/ping` appeared in the console and then failed
 * to open with "no such workflow in ~/.kontra/workflows" — and nothing said which of the two halves
 * was wrong. The reporter's workaround was to copy the folders in, which is the thing registration
 * exists to avoid.
 *
 * THE REFUSALS ABOVE MUST KEEP REFUSING. That is most of what is pinned here: widening a path
 * boundary is exactly the change that turns a fix into an arbitrary-read, so every case that used
 * to throw is re-asserted with a provider installed.
 */
describe('a workflow folder registered outside the default root', () => {
  let elsewhere: string;

  beforeEach(() => {
    elsewhere = mkdtempSync(path.join(tmpdir(), 'kontra-registered-'));
    mkdirSync(path.join(elsewhere, 'ping'), { recursive: true });
    writeFileSync(path.join(elsewhere, 'ping', 'workflow.py'), '# registered elsewhere\n');
    setRegisteredFolders(() => new Map([['ping', path.join(elsewhere, 'ping')]]));
  });

  afterEach(() => {
    rmSync(elsewhere, { recursive: true, force: true });
  });

  it('resolves by name to the marker inside it', () => {
    const got = resolveWorkflowFile('ping');
    expect(got).toBe(realpathSync(path.join(elsewhere, 'ping', 'workflow.py')));
  });

  it('is readable, which is the half that failed', () => {
    expect(readWorkflow('ping')).toContain('registered elsewhere');
  });

  it('refuses a registered folder with no workflow.py — that is what makes it a Workflow', () => {
    mkdirSync(path.join(elsewhere, 'empty'), { recursive: true });
    setRegisteredFolders(() => new Map([['empty', path.join(elsewhere, 'empty')]]));
    expect(() => resolveWorkflowFile('empty')).toThrow(/has no workflow\.py/);
  });

  it('does not let a registration shadow a workflow already in the default root', () => {
    // The ordinary case has to stay exactly as it was: a name that resolves under
    // `~/.kontra/workflows` resolves there and nowhere else.
    mkdirSync(path.join(root, 'ping'), { recursive: true });
    writeFileSync(path.join(root, 'ping', 'workflow.py'), '# the default root one\n');
    expect(readWorkflow('ping')).toContain('the default root one');
  });

  it('still refuses to climb out of the registered folder', () => {
    // The whole boundary, applied to the second root. A name is one segment by construction, but
    // the confinement is what the test is for — it is the property that must survive the widening.
    expect(() => resolveWorkflowFile('ping/../../secrets.py')).toThrow(ControlRefused);
    expect(() => resolveWorkflowFile('../outside/secrets.py')).toThrow(ControlRefused);
  });

  it('still refuses an absolute path, a symlink out, and an unregistered name', () => {
    expect(() => resolveWorkflowFile(path.join(elsewhere, 'ping'))).toThrow(ControlRefused);
    symlinkSync(path.join(outside, 'secrets.py'), path.join(root, 'innocent.py'));
    expect(() => resolveWorkflowFile('innocent.py')).toThrow(/outside \.kontra\/workflows/);
    // A name nobody registered is not admitted by the presence of OTHER registrations.
    expect(() => resolveWorkflowFile('neverregistered')).toThrow(ControlRefused);
  });

  it('ignores a registration whose folder is gone, rather than resolving into nothing', () => {
    setRegisteredFolders(() => new Map([['ghost', path.join(elsewhere, 'ghost')]]));
    expect(() => resolveWorkflowFile('ghost')).toThrow(ControlRefused);
  });

  it('survives a provider that throws, and still resolves the default root', () => {
    // An unreadable database must not take the path most workflows are on down with it.
    setRegisteredFolders(() => {
      throw new Error('store is closed');
    });
    expect(resolveWorkflowFile('examples/python/workflows/nscheck.py')).toContain('nscheck.py');
  });
});

describe('resolveWorkflowFile', () => {
  it('accepts a file inside the checkout and returns its real path', () => {
    const got = resolveWorkflowFile('examples/python/workflows/nscheck.py');
    expect(got.endsWith(path.join('examples', 'python', 'workflows', 'nscheck.py'))).toBe(true);
  });

  it('refuses to climb out with ..', () => {
    expect(() => resolveWorkflowFile('../outside/secrets.py')).toThrow(ControlRefused);
    expect(() => resolveWorkflowFile('examples/../../secrets.py')).toThrow(ControlRefused);
  });

  it('refuses a SYMLINK out of the workflows directory', () => {
    // The check that `..` alone would miss. A path with no `..` in it, entirely inside the
    // checkout by string comparison, pointing anywhere on the disk.
    symlinkSync(path.join(outside, 'secrets.py'), path.join(root, 'innocent.py'));
    expect(() => resolveWorkflowFile('innocent.py')).toThrow(/outside \.kontra\/workflows/);
  });

  it('refuses an absolute path', () => {
    expect(() => resolveWorkflowFile(path.join(outside, 'secrets.py'))).toThrow(ControlRefused);
  });

  it('refuses anything that is not a .py file', () => {
    writeFileSync(path.join(root, 'run.sh'), '#!/bin/sh\n');
    expect(() => resolveWorkflowFile('run.sh')).toThrow(/not a .py file/);
  });

  it('says which file is missing, in which root', () => {
    // The likeliest honest failure: a real typo. It should name the path AND where it looked,
    // because "no such file" against an unstated root is unactionable.
    expect(() => resolveWorkflowFile('examples/nope.py')).toThrow(/nope\.py/);
    expect(() => resolveWorkflowFile('examples/nope.py')).toThrow(new RegExp(root));
  });

  it('refuses an empty path rather than resolving to the root directory', () => {
    expect(() => resolveWorkflowFile('')).toThrow(ControlRefused);
  });

  it('accepts a FOLDER and answers with the workflow.py inside it', () => {
    // The layout this slice adds: a workflow is a folder holding `workflow.py` and its
    // description.md, the same shape an actor has with actor.json + actor.py. Both spellings of it
    // reach the same file, because `serve nscheck` is what the page sends and
    // `serve nscheck/workflow.py` is what an operator types.
    const dir = path.join(root, 'nscheck');
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# caller\n');
    expect(resolveWorkflowFile('nscheck')).toBe(path.join(realpathSync(root), 'nscheck', 'workflow.py'));
    expect(resolveWorkflowFile('nscheck/workflow.py')).toBe(
      path.join(realpathSync(root), 'nscheck', 'workflow.py')
    );
  });

  it('refuses a folder with no workflow.py, naming the file that would make it one', () => {
    // A directory that is not a workflow is the likeliest honest mistake — a __pycache__, a
    // half-written folder — and "no such workflow" about a path that plainly exists is unactionable.
    mkdirSync(path.join(root, 'notyet'), { recursive: true });
    expect(() => resolveWorkflowFile('notyet')).toThrow(/workflow\.py/);
  });

  it('refuses a folder that resolves outside the root', () => {
    // Confinement is unchanged by folders being servable: the boundary is applied to the folder
    // exactly as it was to a file, after realpath rather than by string.
    mkdirSync(path.join(outside, 'evil'), { recursive: true });
    writeFileSync(path.join(outside, 'evil', 'workflow.py'), '# not yours\n');
    symlinkSync(path.join(outside, 'evil'), path.join(root, 'innocent'));
    expect(() => resolveWorkflowFile('innocent')).toThrow(/outside \.kontra\/workflows/);
    expect(() => resolveWorkflowFile('../outside/evil')).toThrow(ControlRefused);
  });

  it('refuses a workflow.py that is a symlink out, inside a folder that is not', () => {
    // THE FOLDER CLEARING THE BOUNDARY IS NOT THE FILE CLEARING IT. The directory is genuinely
    // inside the root; the marker in it is a separate path, and it is the one that gets executed.
    // Confining only the folder would run anything on the disk that a symlink pointed at.
    const dir = path.join(root, 'trojan');
    mkdirSync(dir, { recursive: true });
    symlinkSync(path.join(outside, 'secrets.py'), path.join(dir, 'workflow.py'));
    expect(() => resolveWorkflowFile('trojan')).toThrow(/outside \.kontra\/workflows/);
  });

  it('resolves the OLD FLAT NAME of a workflow that has become a folder', () => {
    // `serve nscheck.py` is in muscle memory, in the wiki and in this repo's own docs, and
    // `.kontra/workflows/nscheck.py` is now `nscheck/workflow.py`. Without this the move turned
    // every remembered command into "no such workflow" about a workflow that is right there.
    const dir = path.join(root, 'nscheck');
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# caller\n');
    expect(resolveWorkflowFile('nscheck.py')).toBe(
      path.join(realpathSync(root), 'nscheck', 'workflow.py')
    );
  });

  it('prefers a real flat file over the folder of the same name', () => {
    // A `nscheck.py` sitting beside a `nscheck/` folder is TWO workflows, and the legacy fallback
    // must not shadow the one that was actually named. It only fires when the flat path is absent.
    const dir = path.join(root, 'nscheck');
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# the folder\n');
    writeFileSync(path.join(root, 'nscheck.py'), '# the flat one\n');
    expect(resolveWorkflowFile('nscheck.py')).toBe(path.join(realpathSync(root), 'nscheck.py'));
  });

  it('does not let the legacy fallback escape the root', () => {
    // The fallback rewrites the path it resolves, so it has to clear the same boundary the typed
    // one does — otherwise `../outside/evil.py` would be admitted for a folder the caller could
    // not have reached by naming it.
    mkdirSync(path.join(outside, 'evil'), { recursive: true });
    writeFileSync(path.join(outside, 'evil', 'workflow.py'), '# not yours\n');
    symlinkSync(path.join(outside, 'evil'), path.join(root, 'innocent'));
    expect(() => resolveWorkflowFile('innocent.py')).toThrow(/outside \.kontra\/workflows/);
    expect(() => resolveWorkflowFile('../outside/evil.py')).toThrow(ControlRefused);
  });

  it('is not fooled by a sibling directory sharing the root as a prefix', () => {
    // `/tmp/kontra-root-x` vs `/tmp/kontra-root-x-evil`: a `startsWith` check admits the second.
    const evil = `${root}-evil`;
    mkdirSync(evil, { recursive: true });
    writeFileSync(path.join(evil, 'x.py'), '# nope\n');
    try {
      symlinkSync(path.join(evil, 'x.py'), path.join(root, 'link.py'));
      expect(() => resolveWorkflowFile('link.py')).toThrow(/outside \.kontra\/workflows/);
    } finally {
      rmSync(evil, { recursive: true, force: true });
    }
  });
});

describe('listWorkflows', () => {
  /** A workflow folder: the marker, and a description unless `description` is null. */
  function folder(name: string, description: string | null): string {
    const dir = path.join(root, name);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), `# ${name}\n`);
    if (description !== null) writeFileSync(path.join(dir, 'description.md'), description);
    return dir;
  }

  it('lists ONE entry per workflow, named for the folder, with its description', () => {
    // The marker is called the same thing inside every folder there is, so a listing built from
    // filenames would have been a column of identical `workflow.py` rows — a list that says how
    // many workflows exist and never which one you want.
    folder('nscheck', '# nscheck\n\nHunts lame delegations.\n\nMore prose here.\n');
    const got = listWorkflows();
    expect(got.map((f) => f.name)).toContain('nscheck');
    expect(got.filter((f) => f.name === 'workflow.py')).toHaveLength(0);
    const ns = got.find((f) => f.name === 'nscheck')!;
    // The FIRST PARAGRAPH, with the `# nscheck` heading skipped: echoing the title back would say
    // nothing the row's own name does not already say.
    expect(ns.description).toBe('Hunts lame delegations.');
    // The bytes and the mtime are the marker's — a directory's own size describes nothing an
    // operator could act on.
    expect(ns.bytes).toBe('# nscheck\n'.length);
  });

  it('lists a folder with no description.md, as absent rather than as an error', () => {
    // A description is optional. A folder without one has to list and has to serve; a row that
    // reported the operator's silence as a failure would be worse than a blank one.
    folder('ping', null);
    const got = listWorkflows().find((f) => f.name === 'ping');
    expect(got).toBeDefined();
    expect(got!.description).toBe('');
    expect(resolveWorkflowFile('ping')).toBe(path.join(realpathSync(root), 'ping', 'workflow.py'));
  });

  it('does NOT list __pycache__, or any other directory without a workflow.py', () => {
    // Python writes __pycache__ into whatever directory it imports from, so it appears in the
    // workflow root without anybody putting it there. Listed, it is a workflow that can only ever
    // fail to serve.
    mkdirSync(path.join(root, '__pycache__'), { recursive: true });
    writeFileSync(path.join(root, '__pycache__', 'nscheck.cpython-311.pyc'), 'not python source');
    mkdirSync(path.join(root, 'half-written'), { recursive: true });
    folder('nscheck', null);
    expect(listWorkflows().map((f) => f.name)).toEqual(['nscheck']);
  });

  it('still lists a flat <name>.py beside the folders', () => {
    // The folder layout was added beside the old one, not in place of it.
    folder('nscheck', null);
    writeFileSync(path.join(root, 'scratch.py'), '# a quick one\n');
    const got = listWorkflows().map((f) => f.name);
    expect(got).toContain('scratch.py');
    expect(got).toContain('nscheck');
  });

  it('does not list the directory’s README, or anything else that is not python', () => {
    writeFileSync(path.join(root, 'README.md'), '# workflows/\n');
    expect(listWorkflows().map((f) => f.name)).not.toContain('README.md');
  });
});

// `saveWorkflow` was removed with `PUT /api/workflows/file/:name` (ADR 0030): the Workflows page is a
// read-only viewer, and the way to change a workflow is the operator's own editor plus a re-serve.
// `readWorkflow` (still tested above) is the read half that survives; the write half is gone.

describe('the root serve runs from is the root registration defaults to', () => {
  // THE BUG THIS CLOSES. Two `kontraHome()`s read KONTRA_HOME and disagreed about its default:
  // `sources.ts` said `~/.kontra`, this module said `<cwd>/.kontra`. Unset, registering a folder
  // recorded a path under one home while `serve` looked under the other — so registration reported
  // success and serving the folder it had just recorded failed with "no such workflow".
  //
  // NEITHER VARIABLE IS SET IN THESE TESTS. With KONTRA_HOME set the two functions always agreed,
  // so a test that sets it proves nothing about the split; it is the UNSET default that has to be
  // exercised, and `~` is therefore pointed at a temp directory rather than the operator's real one.
  let fakeHome: string;

  beforeEach(() => {
    fakeHome = realpathSync(mkdtempSync(path.join(tmpdir(), 'kontra-home-')));
    // The outer beforeEach pins KONTRA_WORKFLOW_ROOT; the point here is the path taken when nobody
    // sets any of them and the root has to come from the home. KONTRA_WORKSPACES goes too: an
    // operator running this suite in a shell that exports it (which the Compose install tells them
    // to) would otherwise get the workspace root here and a failure about their own laptop.
    delete process.env.KONTRA_WORKFLOW_ROOT;
    delete process.env.KONTRA_WORKSPACES;
    delete process.env.KONTRA_HOME;
    vi.spyOn(os, 'homedir').mockReturnValue(fakeHome);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    rmSync(fakeHome, { recursive: true, force: true });
  });

  /** What `~/.kontra/workflows` is while `homedir` is faked. */
  const defaultWorkflows = () => path.join(fakeHome, '.kontra', 'workflows');

  it('defaults to ~/.kontra/workflows, not to the directory the process was started in', () => {
    expect(defaultRoot('workflow')).toBe(defaultWorkflows());
    expect(workflowRoot()).toBe(defaultWorkflows());
    // The old default, spelled out: it is what this module used to answer, and it is what made the
    // two roots different directories.
    expect(workflowRoot()).not.toBe(path.join(process.cwd(), '.kontra', 'workflows'));
  });

  it('resolves to the same directory as defaultRoot(workflow), through symlinks', () => {
    mkdirSync(defaultWorkflows(), { recursive: true });
    // `workflowRoot` resolves symlinks (it is the confinement boundary) and `defaultRoot` does not,
    // so the comparison is against the real path — same directory, not merely the same spelling.
    expect(workflowRoot()).toBe(realpathSync(defaultRoot('workflow')));
  });

  it('serves a folder registered under the default root', () => {
    const dir = path.join(defaultRoot('workflow'), 'nscheck');
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# caller\n');

    // What registration stores: the real, absolute path of the folder.
    const registered = inspectFolder('workflow', dir);
    expect(registered.path).toBe(dir);

    // What serve does with it. Under the split defaults this threw `no such workflow`, because
    // `workflowRoot()` was `<cwd>/.kontra/workflows` and the registered path was nowhere near it —
    // registration recorded a path that serve then refused.
    const rel = path.relative(workflowRoot(), path.join(registered.path, 'workflow.py'));
    expect(resolveWorkflowFile(rel)).toBe(path.join(dir, 'workflow.py'));
  });

  it('still refuses a symlink out of a root derived from the home', () => {
    // Confinement does not weaken because the root came from the home rather than from
    // KONTRA_WORKFLOW_ROOT: the escape that a `..` check alone would miss is still closed, and it is
    // still closed after realpath rather than by comparing prefixes.
    mkdirSync(defaultWorkflows(), { recursive: true });
    symlinkSync(path.join(outside, 'secrets.py'), path.join(defaultWorkflows(), 'innocent.py'));
    expect(() => resolveWorkflowFile('innocent.py')).toThrow(/outside \.kontra\/workflows/);
  });

  it('lets KONTRA_WORKFLOW_ROOT move serve’s root and nothing else', () => {
    process.env.KONTRA_WORKFLOW_ROOT = root;
    expect(workflowRoot()).toBe(realpathSync(root));
    // Registration is unmoved — the override is scoped to what serve may run, which is what makes it
    // a narrower confinement rather than a second home.
    expect(defaultRoot('workflow')).toBe(defaultWorkflows());
  });

  it('prefers the active workspace’s workflows/ over the home, and KONTRA_WORKFLOW_ROOT over both', () => {
    // ADR 0047. The Workflows page lists THIS directory, so when it was the home's while the code
    // lived in the mounted workspace, the page drew an empty list and named a directory the
    // operator had never put anything in — beside a Serve button that worked, because registration
    // had already reached the workspace. One root, pointed where the code is.
    const parent = mkdtempSync(path.join(tmpdir(), 'kontra-ws-'));
    mkdirSync(path.join(parent, 'demo', 'workflows'), { recursive: true });
    writeFileSync(path.join(parent, CURRENT_FILE), 'demo\n');
    process.env.KONTRA_WORKSPACES = parent;

    expect(workflowRoot()).toBe(realpathSync(path.join(parent, 'demo', 'workflows')));

    // The override still wins, which is what keeps a test able to pin its own directory.
    process.env.KONTRA_WORKFLOW_ROOT = root;
    expect(workflowRoot()).toBe(realpathSync(root));

    rmSync(parent, { recursive: true, force: true });
  });

  it('resolves the deployment’s explicit KONTRA_HOME exactly as it did before', () => {
    // docker-compose.yml sets `KONTRA_HOME: ${KONTRA_CHECKOUT}/.kontra` on orchestrator-api, and
    // that container's cwd is `/app` — `/app/.kontra` has never existed, so the cwd default was
    // already dead there. Moving the default from `<cwd>` to `~` therefore cannot move the running
    // control plane's workflow root: it resolves through the override, to this.
    process.env.KONTRA_HOME = '/srv/checkout/.kontra';
    expect(workflowRoot()).toBe(path.join('/srv/checkout/.kontra', 'workflows'));
  });
});

describe('serveWorkflow', () => {
  /** A servable workflow FOLDER: the marker plus a manifest, which is what the derived queue needs
   *  now that there is no `--queue` to type. Without the manifest there is no name to bind the
   *  queue to and serve refuses (see the no-manifest test below). */
  function servable(name: string, opts: { version?: string; cls?: string } = {}): void {
    const dir = path.join(root, name);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# caller\n');
    writeFileSync(
      path.join(dir, 'workflow.json'),
      JSON.stringify({
        name,
        version: opts.version ?? '0.1.0',
        workflow: opts.cls ?? 'NsCheck',
        entry: 'workflow.py',
      })
    );
  }

  it('refuses a folder with no workflow.json, because there is no name to bind a queue to', async () => {
    // THE FLAG THAT LET A MANIFEST-LESS FILE SERVE IS GONE (GitHub #15). The queue is
    // `wf-<name>-<digest12>`; with no manifest there is no name, and there is no `--queue` to supply
    // one, so serve refuses and names the fix rather than inventing a queue.
    process.env.KONTRA_BIN = 'true';
    mkdirSync(path.join(root, 'bare'), { recursive: true });
    writeFileSync(path.join(root, 'bare', 'workflow.py'), '# no manifest beside me\n');
    await expect(serveWorkflow({ file: 'bare' })).rejects.toThrow(/derive a queue|--init/);
  });

  it('asks the infra role, and the request carries an ID and a kind — never a path or an argv', async () => {
    /*
     * THE SECURITY PROPERTY, ASSERTED RATHER THAN DESCRIBED.
     *
     * This process has no Docker socket and must not get one: it is the container with the
     * published port and the untrusted HTTP input, and a read-write socket here is root on the
     * host. `kontra-infra` holds it and publishes no port, so the serve goes there. That hop is
     * only worth anything if the API cannot say WHAT to run — otherwise it has the socket by proxy.
     *
     * So: an id and a kind cross, and nothing else. If somebody ever adds an `image`, an `argv` or
     * a `path` to this payload, this test is what says no.
     */
    servable('nscheck');
    await serveWorkflow({ file: 'nscheck' });

    expect(executed.type).toBe('serveDevWorkflow');
    expect(executed.opts?.taskQueue).toBe('kontra-infra');
    const args = executed.opts?.args as Array<Record<string, unknown>>;
    expect(Object.keys(args[0] ?? {}).sort()).toEqual(['kind', 'sourceId']);
    expect(args[0]?.kind).toBe('workflow');
    expect(String(args[0]?.sourceId)).toMatch(/^at:.*nscheck$/);
  });

  it('keys the serve by the folder, so two presses cannot race two starts', async () => {
    // Temporal refuses a second execution with a live id, which is a structural guard rather than
    // a check somebody remembered to write. `FAIL` and not `USE_EXISTING`: the caller wants an
    // answer about the serve IT asked for.
    servable('nscheck');
    await serveWorkflow({ file: 'nscheck' });
    expect(String(executed.opts?.workflowId)).toMatch(/^serve-dev\/at:.*nscheck$/);
    expect(executed.opts?.workflowIdConflictPolicy).toBe('FAIL');
  });

  it('reports a missing file as a missing workflow, not a queue problem', async () => {
    // The queue is derived FROM the file, so there is nothing to validate until the file resolves.
    // The refusal an operator gets is the accurate one.
    await expect(serveWorkflow({ file: 'does/not/exist.py' })).rejects.toThrow(/no such workflow/);
  });

  it('derives a queue for a FLAT .py from its stem and the flat-file version', async () => {
    // A flat file has no manifest to version it, so it carries `0.0.0` — STATED, not hashed. It
    // used to be digested by its own bytes, which made the queue move on every edit; a flat file is
    // the shape a folder replaces and does not get a second identity scheme of its own.
    const got = await serveWorkflow({ file: 'examples/python/workflows/nscheck.py' });
    expect(got.queue).toBe('wf-nscheck-0.0.0');
    expect(got.session).toBe('nscheck');
  });

  it('derives wf-<name>-<digest12>, untypable and bound to the folder content', () => {
    // The shape: name for readability, a 12-hex content digest for identity. `<digest>` replaces
    // the old `<version>` because a version is a free string on a folder (edit the code, keep the
    // version, keep the queue) while the digest IS the code.
    const digest = 'a1b2c3d4e5f6';
    expect(workflowQueue('nscheck', digest)).toBe(`wf-nscheck-${digest}`);

    // The reason the `wf-` prefix exists. This repo ships an ACTOR and a WORKFLOW both called
    // `nscheck`; an actor's shared queue is `<name>-<version>`, and no digest value can make a
    // `wf-`-prefixed name collide with it.
    expect(workflowQueue('nscheck', digest)).not.toBe('nscheck-0.1.0');

    // Neither half alone is a queue.
    expect(workflowQueue('', digest)).toBe('');
    expect(workflowQueue('nscheck', '')).toBe('');
  });

  it('reports the worker name, WHAT TO SEARCH LOGS BY, and the DERIVED queue on success', async () => {
    servable('nscheck');
    executed.result = { worker: 'nscheck', queue: 'wf-nscheck-0.1.0', detail: '' };
    const got = await serveWorkflow({ file: 'nscheck' });
    // The FOLDER names the session, not the queue.
    expect(got.session).toBe('nscheck');
    // `attach` NAMES THE WORKER, and the field has outlived two mechanisms: `tmux attach -t …` only
    // ever worked from a shell on the box, and the log FILE that replaced it was written by a pid
    // registry that no longer exists. A serve-dev Worker's output is its container's stdout, which
    // logship ships by the `KONTRA_WORKER` label — so what a caller needs is the name to search by.
    expect(got.attach).toBe('nscheck');
    // The path comes back RELATIVE to the checkout — the file the CLI was handed.
    expect(got.file).toBe(path.join('nscheck', 'workflow.py'));
    // The queue is the derived one: `wf-<name>-<version>` off the manifest — the SAME shape an
    // Actor's queue has, and a fact reported rather than a string passed in. It is readable and it
    // does NOT move when `workflow.py` is edited, which is the whole reason it stopped being a
    // content digest: three edits in one session produced three queues and every remembered `start`
    // failed against a worker that had moved.
    expect(got.queue).toBe('wf-nscheck-0.1.0');
  });

  it('serves a FOLDER, and two folders land under two worker names', async () => {
    // What `kontra workflow serve` is handed is the RESOLVED file, so the CLI's own independently
    // derived session name is taken from the same string this one is — and the page then goes
    // looking for a pane by a name the worker really is in.
    servable('nscheck');
    servable('ping', { cls: 'Ping' });

    const first = await serveWorkflow({ file: 'nscheck' });
    const second = await serveWorkflow({ file: 'ping' });

    expect(first.session).toBe('nscheck');
    expect(second.session).toBe('ping');
    // Under the old stem rule both were `workflow`, and the second serve would have been refused
    // as "already exists" against the first one's worker.
    expect(first.session).not.toBe(second.session);
    // Two folders, two queues — the digest binds each queue to its own folder's content.
    expect(first.queue).not.toBe(second.queue);
    expect(first.file).toBe(path.join('nscheck', 'workflow.py'));
  });

  it('reaches one folder, and one session, from all three spellings of it', async () => {
    // THE MOVE MUST NOT SPLIT A WORKFLOW IN TWO. `nscheck.py` is what is in muscle memory and in
    // the docs, `nscheck` is what the page sends, `nscheck/workflow.py` is what an operator types
    // after a tab-completion — and each one that produced a different session would be a second
    // worker on the same queue, with the Monitor showing whichever pane it found first.
    servable('nscheck');

    const rel = path.join('nscheck', 'workflow.py');
    let firstQueue: string | undefined;
    for (const spelling of ['nscheck.py', 'nscheck', rel]) {
      const got = await serveWorkflow({ file: spelling });
      expect(got.session).toBe('nscheck');
      // The path handed to the CLI is the FOLDER's file, whichever spelling arrived — which is
      // what feeds `cli/identity.go:workflowSession` the same string this one was derived from.
      expect(got.file).toBe(rel);
      // And all three spellings resolve to ONE folder, so all three derive ONE queue.
      firstQueue ??= got.queue;
      expect(got.queue).toBe(firstQueue);
    }
  });

  it('surfaces an infra-side refusal as a failure, and names it', async () => {
    // A serve that could not happen must not read as one that did. The likeliest cause in practice
    // is that the infra role is not running at all — the workflow is accepted onto a queue nobody
    // polls and the await never returns — so whatever comes back is reported rather than swallowed.
    servable('nscheck');
    executed.fail = new Error('worker exited on startup: ModuleNotFoundError: no module named foo');
    await expect(serveWorkflow({ file: 'nscheck' })).rejects.toThrow(/serve failed/);
    await expect(serveWorkflow({ file: 'nscheck' })).rejects.toThrow(/ModuleNotFoundError/);
  });
});

describe('startRun', () => {

  /**
   * ADR 0046's prerequisite: WHOSE run this is, stamped at start.
   *
   * `KontraTenant` was registered on the namespace and written by NOTHING — two readers already took
   * it (`describeRun`, `listRuns`), so every `tenant` this control plane reported was the empty
   * string. MEASURED on the live cluster before the fix: of ten open executions only the backing
   * `kontra.v1.ActorService.Run` carried any `Kontra*` attribute at all.
   */
  it('stamps the tenant on the start, where it costs no event', async () => {
    // The run's namespace comes from its workspace (no layout here, so the legacy one, from the env).
    vi.stubEnv('KONTRA_NAMESPACE', 'test-namespace');
    started.opts = undefined;
    servable('ping', 'Ping');
    await startRun({ file: 'ping' }, pollers(1));
    vi.unstubAllEnvs();

    expect(started.opts, 'startRun never reached Temporal').toBeDefined();
    const attrs = started.opts!.typedSearchAttributes as
      | { get(key: unknown): unknown }
      | undefined;
    expect(attrs, 'no search attributes were sent').toBeDefined();
    expect(attrs!.get(KontraTenant)).toBe('test-namespace');
  });

  it('stamps the namespace it CONNECTS to, which is the run\'s workspace namespace (ADR 0051)', async () => {
    // A client opened in one namespace and a tenant stamp saying another is how a run comes to be
    // filed under the wrong workspace. Not `default`, so a hard-coded fallback fails this.
    vi.stubEnv('KONTRA_NAMESPACE', 'test-namespace');
    try {
      started.opts = undefined;
      clientNamespaces.length = 0;
      servable('ping2', 'Ping2');
      await startRun({ file: 'ping2' }, pollers(1));
      const attrs = started.opts!.typedSearchAttributes as { get(key: unknown): unknown };
      expect(clientNamespaces).toEqual(['test-namespace']);
      expect(attrs.get(KontraTenant)).toBe('test-namespace');
    } finally {
      vi.unstubAllEnvs();
    }
  });
  /** A folder that can be started: a manifest naming its @workflow.defn class. */
  function servable(name: string, cls = 'NsCheck'): void {
    const dir = path.join(root, name);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# caller\n');
    writeFileSync(
      path.join(dir, 'workflow.json'),
      JSON.stringify({ name, version: '0.1.0', workflow: cls, entry: 'workflow.py' })
    );
  }

  /** A describer that reports `n` distinct pollers on any queue, without touching Temporal. An
   *  `error` makes the describe THROW, which is how `describeQueue` learns Temporal could not be
   *  asked (a state that must read as unknown, not as zero). */
  function pollers(n: number, error?: string): import('./pollers').QueueDescriber {
    return {
      pollers: async () => {
        if (error !== undefined) throw new Error(error);
        return Array.from({ length: n }, (_, i) => ({ identity: `w${i}`, lastAccess: 1 }));
      },
      close: async () => {},
    };
  }

  it('REFUSES when nothing is serving the folder’s digest — the whole point of #15', async () => {
    // A start that dispatched onto a queue nobody polls would sit `running` forever with no error.
    // So start derives the queue from the folder and refuses when zero workers poll it, naming the
    // fix instead of hoping.
    servable('nscheck');
    await expect(startRun({ file: 'nscheck' }, pollers(0))).rejects.toThrow(/no pollers|has no pollers/);
  });

  it('refuses when Temporal cannot be asked, rather than dispatching blind', async () => {
    servable('nscheck');
    await expect(startRun({ file: 'nscheck' }, pollers(0, 'connection refused'))).rejects.toThrow(
      ControlRefused
    );
  });

  it('refuses a folder with no @workflow.defn class in its manifest', async () => {
    const dir = path.join(root, 'noclass');
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# caller\n');
    writeFileSync(path.join(dir, 'workflow.json'), JSON.stringify({ name: 'noclass', version: '0.1.0' }));
    await expect(startRun({ file: 'noclass' }, pollers(3))).rejects.toThrow(/@workflow\.defn|--init/);
  });

  it('refuses a bad explicit type before dialling Temporal', async () => {
    servable('nscheck');
    await expect(startRun({ file: 'nscheck', type: 'Ns Check' }, pollers(3))).rejects.toThrow(
      /workflow type name/
    );
  });

  it('SNAPSHOTS the caller workflow\'s manifest identity, keyed by the run id (ADR 0029 §2)', async () => {
    // Why a start writes anything at all: Temporal forgets the execution after its retention window
    // and a kept Dataset outlives it, so the identity the Dataset's name renders has to be recorded
    // while it is known (ADR 0025's pattern). The name is `wf-<workflow>-<version>--…` — the CALLER
    // workflow's, not the Actor's — and this is the only moment that identity is in hand.
    servable('nscheck');
    const stamped: Array<[string, string, string]> = [];
    const started = await startRun({ file: 'nscheck' }, pollers(3), {
      record: async (runId, workflow, version) => {
        stamped.push([runId, workflow, version]);
      },
    });
    // The run id IS the workflow id, `<type>-<unixseconds>` (ADR 0023 §12), and the identity is
    // stamped against exactly it — the key `/api/datasets` joins on.
    expect(started.runId).toMatch(/^nscheck-\d+$/);
    expect(stamped).toEqual([[started.runId, 'nscheck', '0.1.0']]);
    // Reported back, so an operator watching a start sees which identity their Datasets will carry.
    expect(started.workflow).toEqual({ name: 'nscheck', version: '0.1.0' });
  });

  it('does not fail a started Run when the snapshot cannot be written', async () => {
    // The run is ALREADY RUNNING when the stamp happens. Rethrowing would report a started Run as a
    // failed start; the caller would retry and Temporal would refuse the workflow id that now
    // exists. What is lost is bounded and documented — the Dataset renders an Actor-grain name via
    // `withDatasetNames`'s fallback — so the stamp swallows and the start stands.
    servable('nscheck');
    const started = await startRun({ file: 'nscheck' }, pollers(3), {
      record: async () => {
        throw new Error('the record store is unreachable');
      },
    });
    expect(started.runId).toMatch(/^nscheck-\d+$/);
    expect(started.workflow).toBeUndefined(); // said out loud: nothing was snapshotted
  });

  /* ── THE CREDENTIAL PREFLIGHT (issue 20) ───────────────────────────────────────────────────
     A run that needs an unbound slot has to fail HERE, naming the slot, rather than inside
     `@actor.load` — which on a fleet is after Machines have been paid for. The gate is injected
     (`RunSlotGate`) so these prove the refusal without a secret store or a key file. */

  /** A folder that declares which Actors it dispatches to. */
  function usingActors(name: string, actors: unknown[]): void {
    const dir = path.join(root, name);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# caller\n');
    writeFileSync(
      path.join(dir, 'workflow.json'),
      JSON.stringify({ name, version: '0.1.0', workflow: 'NsCheck', entry: 'workflow.py', actors })
    );
  }

  /** A gate that refuses everything, recording what it was asked about. */
  const refusing = (asked: unknown[][]) => ({
    refuseRun: async (actors: readonly { name: string; version?: string }[]) => {
      asked.push([...actors]);
      return 'this run cannot start: probe@0.2.0\n    api_key — declared, and nothing is bound to it';
    },
  });

  it('REFUSES AT THE START when an Actor this run uses has an unbound slot, naming the slot', async () => {
    usingActors('nscheck', ['probe@0.2.0']);
    const asked: unknown[][] = [];
    await expect(startRun({ file: 'nscheck' }, pollers(3), undefined, refusing(asked))).rejects.toThrow(
      /api_key/
    );
    expect(asked).toEqual([[{ name: 'probe', version: '0.2.0' }]]);
  });

  it('refuses BEFORE asking Temporal who is polling — the point is that nothing has been spent', async () => {
    // `pollers(0, …)` makes the describe THROW. Reaching it would produce the queue refusal
    // instead, which is how a gate placed one line too late stops being a gate that saves a fleet.
    usingActors('nscheck', ['probe@0.2.0']);
    await expect(
      startRun({ file: 'nscheck' }, pollers(0, 'connection refused'), undefined, refusing([]))
    ).rejects.toThrow(/api_key/);
  });

  it('reads unpinned and pinned names alike off the manifest', async () => {
    usingActors('nscheck', ['probe', { name: 'subfinder', version: '0.1.0' }]);
    const asked: unknown[][] = [];
    await expect(startRun({ file: 'nscheck' }, pollers(3), undefined, refusing(asked))).rejects.toThrow();
    expect(asked).toEqual([[{ name: 'probe' }, { name: 'subfinder', version: '0.1.0' }]]);
  });

  it('does not consult the gate at all for a manifest that names no Actors', async () => {
    // Opt-in, and honestly so: the orchestrator cannot derive which Actors a caller reaches, and a
    // gate that guessed would refuse runs over Actors they never touch.
    servable('nscheck');
    const asked: unknown[][] = [];
    const started = await startRun({ file: 'nscheck' }, pollers(3), undefined, refusing(asked));
    expect(asked).toEqual([]);
    expect(started.runId).toMatch(/^nscheck-\d+$/);
  });

  it('starts when the gate is satisfied', async () => {
    usingActors('nscheck', ['probe@0.2.0']);
    const started = await startRun({ file: 'nscheck' }, pollers(3), undefined, {
      refuseRun: async () => null,
    });
    expect(started.runId).toMatch(/^nscheck-\d+$/);
  });

  it('drops a malformed entry rather than making a typo unstartable', async () => {
    usingActors('nscheck', [42, '', { version: '0.1.0' }, 'probe']);
    const asked: unknown[][] = [];
    await expect(startRun({ file: 'nscheck' }, pollers(3), undefined, refusing(asked))).rejects.toThrow();
    expect(asked).toEqual([[{ name: 'probe' }]]);
  });

  it('snapshots nothing for a flat .py workflow — it has no manifest to be identified by', async () => {
    // A flat file is named by its stem and digested by its own bytes; there is no `workflow.json`
    // holding a name and a version, so there is no manifest identity, and inventing one from a
    // filename would be worse than the Actor-grain fallback.
    writeFileSync(
      path.join(root, 'examples', 'python', 'workflows', 'flat.py'),
      '@workflow.defn\nclass Flat:\n    pass\n'
    );
    const stamped: string[] = [];
    const started = await startRun(
      { file: 'examples/python/workflows/flat.py', type: 'Flat' },
      pollers(3),
      { record: async (runId) => void stamped.push(runId) }
    );
    expect(started.runId).toMatch(/^flat-\d+$/);
    expect(stamped).toEqual([]);
    expect(started.workflow).toBeUndefined();
  });
});

describe('the session name is a cross-language contract', () => {
  it('matches `cli/identity.go:workflowSession`', () => {
    // Written independently on both sides. A drift is silent in the worst way: the worker starts
    // and runs perfectly, and the Monitor never shows it.
    //
    // THE FILE, NOT THE QUEUE. `kontra-wf-<queue>` named a routing decision an operator makes per
    // session rather than the thing being served, so `nscheck.py` ran in `kontra-wf-recon`. The
    // `kontra-` prefix went with it: discovery is the `@kontra` tmux option now, which can also say
    // what KIND of session it is.
    expect(workflowSession('examples/python/workflows/nscheck.py')).toBe('nscheck');
    expect(workflowSession('enumerate_scope.py')).toBe('enumerate_scope');
    expect(workflowSession('/abs/path/to/sweep.py')).toBe('sweep');
    expect(workflowSession('recon')).toBe('recon');
  });

  it('names a folder’s workflow after the FOLDER, never `workflow`', () => {
    // THE TRAP THE FOLDER LAYOUT SETS. Every workflow folder holds a file called `workflow.py`, so
    // the stem — which is all this used to take — is the same for every workflow there is.
    expect(workflowSession('nscheck/workflow.py')).toBe('nscheck');
    expect(workflowSession('/home/me/.kontra/workflows/ping/workflow.py')).toBe('ping');
    // A folder named as the folder, without the file, is the same workflow and the same session:
    // `kontra workflow pause nscheck` has to reach what `serve nscheck` created, with or without
    // the trailing slash a shell's tab-completion adds.
    expect(workflowSession('nscheck')).toBe('nscheck');
    expect(workflowSession('nscheck/')).toBe('nscheck');
  });

  it('gives two folders two sessions', () => {
    // The damage the stem did is not cosmetic: the second serve is REFUSED as "already exists"
    // against the first one's worker, and the Monitor discovers panes by this name.
    expect(workflowSession('nscheck/workflow.py')).not.toBe(workflowSession('ping/workflow.py'));
  });

  it('still names something attachable when there is no folder to be named after', () => {
    // A bare `workflow.py` has `.` for a parent and `/workflow.py` has `/`; neither is a name, and
    // a session called `.` or `` cannot be attached to at all.
    expect(workflowSession('workflow.py')).toBe('workflow');
    expect(workflowSession('/workflow.py')).toBe('workflow');
    // Not every stem containing the word is the marker — `my.workflow.py` is a flat file whose
    // stem has a dot in it, and it keeps its own name.
    expect(workflowSession('recon/my.workflow.py')).toBe('my_workflow');
  });
});

describe('exposure', () => {
  it('reports OPEN when no token is configured, and says what that admits', () => {
    delete process.env.KONTRA_RUN_TOKEN;
    const got = describeExposure();
    expect(got.open).toBe(true);
    // It must name the actual consequence. "Unauthenticated" is a property; "can provision cloud
    // machines" is what it costs.
    expect(got.detail).toMatch(/provision/i);
    expect(got.detail).toMatch(/KONTRA_RUN_TOKEN/);
  });

  it('reports closed once a token is set', () => {
    process.env.KONTRA_RUN_TOKEN = 'secret';
    expect(describeExposure().open).toBe(false);
  });
});

describe('admission posture', () => {
  // The bug this pins cost a working feature and would have been read as a broken server.
  //
  // `checkBearer` FAILS CLOSED: no token configured means 503 `disabled`, which is right for the
  // explore and state surfaces it was written for. Wiring these two routes to it made an OPEN
  // control surface answer 503 to everything — the exact opposite of the choice, and indis-
  // tinguishable from an outage. `checkOptionalBearer` is the other posture, and the two must
  // stay separate functions so a call site cannot mean one and get the other.
  it('checkOptionalBearer admits when no token is set; checkBearer refuses', async () => {
    const { checkBearer, checkOptionalBearer } = await import('./auth');
    delete process.env.KONTRA_RUN_TOKEN;

    expect(checkOptionalBearer(undefined, RUN_TOKEN_VARS_FOR_TEST)).toBeNull();
    expect(checkBearer(undefined, RUN_TOKEN_VARS_FOR_TEST)?.code).toBe(503);
  });

  it('checkOptionalBearer enforces once a token IS set', async () => {
    const { checkOptionalBearer } = await import('./auth');
    process.env.KONTRA_RUN_TOKEN = 'secret';

    expect(checkOptionalBearer(undefined, RUN_TOKEN_VARS_FOR_TEST)?.code).toBe(401);
    expect(checkOptionalBearer('Bearer wrong', RUN_TOKEN_VARS_FOR_TEST)?.code).toBe(401);
    expect(checkOptionalBearer('Bearer secret', RUN_TOKEN_VARS_FOR_TEST)).toBeNull();
  });
});

/** Local alias so this file does not depend on the route module's export shape for a token name. */
const RUN_TOKEN_VARS_FOR_TEST = ['KONTRA_RUN_TOKEN'] as const;

describe('serveEnv — the HOST\'s view, not this process\'s', () => {
  // Serving crosses a boundary. `kontra` runs in the container; the worker it starts runs on the
  // host, because the tmux client here drives the host's tmux server through a mounted socket.
  // Every address in this process's environment is a compose-network name — `temporal:7233`,
  // `http://seaweed:8333` — and none of them resolve out there. A worker handed them starts
  // cleanly and then retries forever against a hostname that does not exist, which reads as a
  // hung worker rather than a misconfigured one.
  it('parses whitespace-separated KEY=VALUE', async () => {
    const { serveEnv } = await import('./workflowControl');
    process.env.KONTRA_SERVE_ENV =
      'KONTRA_ADDRESS=localhost:7233 KONTRA_S3_ENDPOINT=http://localhost:8333';
    expect(serveEnv()).toEqual({
      KONTRA_ADDRESS: 'localhost:7233',
      KONTRA_S3_ENDPOINT: 'http://localhost:8333',
    });
  });

  it('keeps a value containing = intact', async () => {
    // A URL with a query string, or any credential with padding, splits on the FIRST `=` only.
    const { serveEnv } = await import('./workflowControl');
    process.env.KONTRA_SERVE_ENV = 'KONTRA_S3_SECRET_KEY=abc==def';
    expect(serveEnv().KONTRA_S3_SECRET_KEY).toBe('abc==def');
  });

  it('is empty when unset, rather than inventing addresses', async () => {
    // Guessing is the bug this replaced. Unset means the worker gets this process's environment
    // and fails the same way a hand-run one would — visibly, and for a stated reason.
    const { serveEnv } = await import('./workflowControl');
    delete process.env.KONTRA_SERVE_ENV;
    expect(serveEnv()).toEqual({});
  });

  it('drops an entry with no `=` instead of treating it as a variable', async () => {
    const { serveEnv } = await import('./workflowControl');
    process.env.KONTRA_SERVE_ENV = 'KONTRA_ADDRESS=localhost:7233 OOPS';
    expect(serveEnv()).toEqual({ KONTRA_ADDRESS: 'localhost:7233' });
  });
});

describe('kontraBin', () => {
  it('defaults to `kontra` and is overridable', () => {
    delete process.env.KONTRA_BIN;
    expect(kontraBin()).toBe('kontra');
    process.env.KONTRA_BIN = '/opt/kontra/bin/kontra';
    expect(kontraBin()).toBe('/opt/kontra/bin/kontra');
  });
});

/**
 * WHAT A FAILED COMMAND SAID, WITHOUT THE DRAWING.
 *
 * The CLI prints a four-line ASCII banner before everything, failures included, so "the last four
 * lines" was three lines of message and one line of art. The Serve button reported this, verbatim,
 * for a refusal that had a perfectly good sentence in it:
 *
 *   serve failed (exit 1): |_|\_\___/|_|\_| |_| |_|_\/_/ \_\ | error: tmux session "probe-0_1_0"
 *   already exists …
 *
 * An operator reads the backslashes, concludes the button is broken, and never reaches the line
 * that tells them exactly what to do.
 */
describe('cliDetail', () => {
  /** The banner, exactly as the CLI prints it. */
  const BANNER = [
    ' _  _____  _  _ _____ ___    _',
    '| |/ / _ \\| \\| |_   _| _ \\  /_\\',
    "| ' < (_) | .' | | | |   / / _ \\",
    '|_|\\_\\___/|_|\\_| |_| |_|_\\/_/ \\_\\',
  ].join('\n');

  it('drops every line of the banner', () => {
    // The exact rule: a line with no letter and no digit is not a message. Every banner line is
    // drawn from `_\/|'<>()-` and spaces alone, and no diagnostic this CLI emits is.
    expect(cliDetail(BANNER, '')).toBe('');
  });

  it('keeps the whole message when the banner precedes it', () => {
    const out = cliDetail(
      `${BANNER}\nerror: tmux session "probe-0_1_0" already exists\n  attach:  tmux attach -t probe-0_1_0\n  replace: tmux kill-session -t probe-0_1_0`,
      ''
    );
    expect(out).not.toContain('\\_\\');
    expect(out).toContain('already exists');
    expect(out).toContain('kill-session');
  });

  it('still takes only the LAST few lines of a long failure', () => {
    // A Python traceback is fifty lines and the useful one is at the bottom.
    const long = [
      BANNER,
      ...Array.from({ length: 40 }, (_, i) => `frame ${i}`),
      'ModuleNotFoundError: httpx',
    ].join('\n');
    const out = cliDetail(long, '');
    expect(out).toContain('ModuleNotFoundError: httpx');
    expect(out.split(' | ')).toHaveLength(4);
  });

  it('falls back to stdout when stderr is empty, and says nothing when neither has anything', () => {
    expect(cliDetail('', 'wrote nothing')).toBe('wrote nothing');
    expect(cliDetail('', '')).toBe('');
    // Blank lines are not content: four lines of output must be four lines to read.
    expect(cliDetail('\n\n\nreal message\n\n', '')).toBe('real message');
  });
});
