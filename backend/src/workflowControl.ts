/**
 * The two verbs behind the Playground's two buttons: **serve** a workflow worker, and **start** a
 * Run on it.
 *
 * THIS DOES NOT PUT EXECUTION BACK IN THE ORCHESTRATOR (ADR 0023 §12). Both verbs are the
 * operator's own terminal commands, reached from a page instead of a shell:
 *
 *   serve   spawns `kontra workflow serve <folder> --tmux` (the queue is derived, not passed). The worker is an ordinary
 *           local process in an ordinary tmux session; this process does not host it, supervise
 *           it, or know what is in it. It exits and the worker keeps running.
 *   start   is `client.workflow.start` — the same call `kontra workflow start` makes. The caller's
 *           workflow id IS the Run id; nothing is minted here and no record is written, because a
 *           record written here could only ever be a copy of Temporal's that drifts.
 *
 * So the server still owns nothing it did not own before. What the UI gains is the two gestures,
 * and — because a local `kontra-*` session IS the Dashboard's inventory (`panels/local.ts`) — a
 * served worker shows up as a Terminal with nothing else registered. That is the whole reason
 * serve goes through tmux rather than being a bare child process: a worker you cannot watch is
 * how "it is running" and "it died on boot" look identical.
 *
 * ── AUTHORITY ──────────────────────────────────────────────────────────────────────────────────
 *
 * These routes are UNAUTHENTICATED by default, deliberately and at the operator's instruction. Two
 * consequences that must not be discovered later:
 *
 *   • `serve` runs a FILE FROM THIS HOST'S DISK as this process's user. The path is confined to
 *     the checkout (see {@link resolveWorkflowFile}) so it is not arbitrary code execution by
 *     upload, but anything already in the repo can be run.
 *   • `start` can start a workflow that calls `fleet.up()`, which PROVISIONS CLOUD MACHINES.
 *
 * `KONTRA_RUN_TOKEN` turns on bearer admission for both without any other change. It is off by
 * default because that was the choice; `describeExposure()` exists so the choice is stated out
 * loud in the boot log and in the page, rather than living in this comment where nobody looks.
 */

import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  existsSync,
  readFileSync,
  readdirSync,
  realpathSync,
  statSync,
} from 'node:fs';
import * as path from 'node:path';
import { tmuxSafeName } from './panels/tmux';
import { describeQueue, temporalQueueDescriber, type QueueDescriber } from './panels/pollers';
import { runWorkflowStore } from './data/runWorkflows';
import {
  DESCRIPTION_FILE,
  MANIFEST,
  MARKER,
  defaultRoot,
  firstParagraph,
  folderDigest,
  readManifest,
} from './sources';
import { getClient } from './temporalClient';
import { toActorRef } from './secrets/slotRoutes';
import { slotStore } from './secrets/slotStore';
import type { ActorRef } from './secrets/slots';

/** Bearer admission for these two routes. Unset means open — see the header. */
export const RUN_TOKEN_VARS = ['KONTRA_RUN_TOKEN'] as const;

/** The `kontra` binary. Configurable because the orchestrator may not have the checkout's `bin`
 *  on PATH, and a spawn that fails with ENOENT should be fixable without a rebuild. */
export function kontraBin(): string {
  return process.env.KONTRA_BIN || 'kontra';
}

/**
 * The environment the SERVED WORKER needs, which is not the one this process has.
 *
 * Serving crosses a boundary: `kontra` runs here, in a container, and the worker it starts runs
 * out there, on the host, because the tmux client here drives the host's tmux server through a
 * mounted socket. Every address in this process's environment is a compose-network name —
 * `temporal:7233`, `http://seaweed:8333` — and not one of them resolves on the other side. A
 * worker handed them starts, fails to dial, and retries forever against a hostname that does not
 * exist.
 *
 * So the host's view is DECLARED rather than derived: `KONTRA_SERVE_ENV` is whitespace-separated
 * `KEY=VALUE`, set by whoever wired the mounts, since that is the only party who knows how the
 * host reaches these services. Nothing is guessed — an unset variable stays unset, and the worker
 * fails the same way it would if you had run it by hand.
 */
export function serveEnv(): Record<string, string> {
  const raw = process.env.KONTRA_SERVE_ENV;
  if (!raw) return {};
  const out: Record<string, string> = {};
  for (const pair of raw.split(/\s+/)) {
    if (!pair) continue;
    const eq = pair.indexOf('=');
    // No `=` is a typo, not a variable to unset. Skipping it silently would be another quiet
    // misconfiguration, so it is dropped loudly enough to find in the boot log.
    if (eq <= 0) {
      console.warn(`[serve] ignoring KONTRA_SERVE_ENV entry ${JSON.stringify(pair)}: no KEY=VALUE`);
      continue;
    }
    out[pair.slice(0, eq)] = pair.slice(eq + 1);
  }
  return out;
}

/**
 * The ONE directory `serve` may run a file from: `.kontra/workflows/`.
 *
 * Narrower than it was, and deliberately. It used to be the whole checkout, which meant the route
 * could run any .py in the repository — and in a container, where there is no checkout at all, it
 * meant `serve` could not work: the first live call failed with `no such file in /app`. A
 * dedicated directory is both the smaller authority and the thing that actually mounts.
 *
 * THE ROOT IS `sources.ts:defaultRoot('workflow')` — literally the directory registration lists,
 * not a second path built from the same words. This module used to declare its own `kontraHome()`
 * defaulting to `<cwd>/.kontra` while `sources.ts` defaulted to `~/.kontra`, so with KONTRA_HOME
 * unset a folder registered under the default root sat outside this root and `serve` refused the
 * very path registration had just recorded. See `sources.ts:kontraHome` for why `~/.kontra` won.
 *
 * KONTRA_WORKFLOW_ROOT still overrides THIS root alone: it narrows what `serve` may run without
 * moving the home the rest of the installation defaults under, which is what lets a test point it
 * at a temp directory.
 *
 * Resolved through symlinks once, here, because it is the confinement boundary and the candidate
 * is resolved the same way before being compared to it.
 */
export function workflowRoot(): string {
  const raw = process.env.KONTRA_WORKFLOW_ROOT || defaultRoot('workflow');
  try {
    return realpathSync(raw);
  } catch {
    return path.resolve(raw);
  }
}

/** One workflow, as the Workflows page lists it. */
export interface WorkflowFile {
  /** The FOLDER's name (`nscheck`), or the bare filename of a flat `<name>.py` beside it. Either
   *  way it is what every other route here takes back — `serve`, the editor, and the pane. */
  name: string;
  /** The `workflow.py`'s, for a folder. Nobody edits a directory. */
  bytes: number;
  modifiedAt: number;
  /** First paragraph of `description.md`, or '' — see {@link describeFolder}. */
  description: string;
}

/**
 * Every workflow in `.kontra/workflows/`, newest first: a FOLDER holding `workflow.py`, or a flat
 * `.py` beside it.
 *
 * ONE ENTRY PER WORKFLOW, NAMED FOR THE FOLDER. The marker is called the same thing inside every
 * folder there is, so a listing built from filenames would have shown `workflow.py` once per
 * workflow — a list of identical rows, none of which says which workflow it is. The bytes and the
 * mtime are the marker's, because that is the file the editor opens and `serve` runs; a directory's
 * own mtime changes when anything in it is touched and says nothing about the code.
 *
 * A DIRECTORY WITHOUT THE MARKER IS NOT A WORKFLOW, and `__pycache__` is the one that proves it:
 * Python writes it into whatever directory it imports from, so it appears in the workflow root
 * without anybody putting it there, and every surface that globbed the directory offered it as a
 * workflow that could only ever fail to serve.
 *
 * A MISSING directory is an empty list, not an error: an installation that has never run
 * `kontra init` is a legitimate state, and the page says "nothing here yet" rather than showing a
 * failure the operator cannot act on.
 */
export function listWorkflows(): WorkflowFile[] {
  const root = workflowRoot();
  let names: string[];
  try {
    names = readdirSync(root);
  } catch {
    return [];
  }
  const out: WorkflowFile[] = [];
  for (const name of names) {
    try {
      const full = path.join(root, name);
      const st = statSync(full);
      if (st.isDirectory()) {
        const marker = statSync(path.join(full, WORKFLOW_MARKER));
        if (!marker.isFile()) continue;
        out.push({
          name,
          bytes: marker.size,
          modifiedAt: marker.mtimeMs,
          description: describeFolder(full),
        });
        continue;
      }
      if (!st.isFile() || !name.endsWith('.py')) continue;
      // A flat file has nowhere to put a description — there is no folder to hold one — which is
      // the whole reason the folder layout exists.
      out.push({ name, bytes: st.size, modifiedAt: st.mtimeMs, description: '' });
    } catch {
      /* vanished between readdir and stat, or a directory with no marker — not a listing failure */
    }
  }
  return out.sort((a, b) => b.modifiedAt - a.modifiedAt);
}

/**
 * A folder's `description.md`, reduced to its first paragraph.
 *
 * THE SAME RULE `sources.ts` READS AN ACTOR'S BY, imported rather than re-spelled, so one file
 * called `description.md` means one thing on both pages.
 *
 * ABSENT IS '', never a placeholder and never a throw: a workflow nobody has written a description
 * for still has to list and still has to serve, and a row saying "could not read description" would
 * report the operator's silence as a fault.
 */
function describeFolder(dir: string): string {
  try {
    return firstParagraph(readFileSync(path.join(dir, DESCRIPTION_FILE), 'utf8'));
  } catch {
    return '';
  }
}

/** Read one workflow's source, for the editor. */
export function readWorkflow(name: string): string {
  return readFileSync(resolveWorkflowFile(name), 'utf8');
}

// There is no `saveWorkflow`, and `PUT /api/workflows/file/:name` went with it (ADR 0030). The
// Workflows page is a read-only viewer: `readWorkflow` above serves the bytes on disk, and changing
// a workflow is the operator's own editor plus a re-serve. A browser write let the file and the
// registered digest disagree — the same reason ADR 0020 gave Terminals no input path. Writing a
// caller into a workflow folder still exists, but it is `writeInside` (sources.ts) behind
// `POST …/caller` (ADR 0023 §12) — a new file, not an in-place edit of what is deployed.

/** Is this an existing directory? A missing path and a file are both "no" — the caller is choosing
 *  between two shapes, not reporting a failure. */
function isDirectory(candidate: string): boolean {
  try {
    return statSync(candidate).isDirectory();
  } catch {
    return false;
  }
}

export class ControlRefused extends Error {}

/** A queue name reaches a tmux session name and a Temporal task queue. Bounded for both. */
export const QUEUE_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
/** A workflow TYPE is a Python class name — what `@workflow.defn` registered. */
const TYPE_RE = /^[A-Za-z_][A-Za-z0-9_]{0,63}$/;

function need(ok: boolean, message: string): void {
  if (!ok) throw new ControlRefused(message);
}

/**
 * The file that makes a FOLDER a workflow, taken from `sources.ts` rather than spelled again here.
 *
 * The same constant registration inspects a folder for. Two spellings of it would let registration
 * accept a folder that serve then finds nothing runnable in — a folder that lists, and refuses.
 */
const WORKFLOW_MARKER = MARKER.workflow;

/** `workflow`, the stem every folder's marker shares. The reason {@link workflowSession} cannot
 *  take a stem for a folder: every folder would answer with this one. */
const WORKFLOW_STEM = path.basename(WORKFLOW_MARKER, path.extname(WORKFLOW_MARKER));

/**
 * Resolve a caller-supplied path to a file inside the checkout, or refuse.
 *
 * A WORKFLOW IS A FOLDER OR A FILE. `nscheck` — a directory holding `workflow.py` and its
 * `description.md` — resolves to the marker inside it, which is the same shape an Actor already
 * has with `actor.json` + `actor.py`. A flat `nscheck.py` still resolves to itself; the folder
 * layout is added beside it rather than in place of it, because both spellings are in muscle
 * memory and in this repo's docs.
 *
 * AND THE OLD SPELLING OF A MOVED ONE. See {@link legacyFolderFor}.
 *
 * THE `..` CHECK IS NOT ENOUGH ON ITS OWN, which is why this resolves symlinks: `examples/x.py`
 * containing no `..` can still be a symlink to `/etc/shadow`, and the comparison that matters is
 * between real paths. `path.relative` rather than `startsWith`, because `/srv/kontra-evil` starts
 * with `/srv/kontra`.
 */
export function resolveWorkflowFile(rel: string): string {
  const root = workflowRoot();
  need(typeof rel === 'string' && rel.trim() !== '', 'file is required');
  need(!path.isAbsolute(rel), 'file must be a name inside .kontra/workflows/');

  // The path this actually resolves, which is the caller's unless they typed the old flat name of
  // a workflow that has since become a folder. `rel` is still what a refusal reports, because the
  // string an operator can fix is the one they typed.
  const asked = legacyFolderFor(root, rel) ?? rel;
  const real = confine(root, rel, path.resolve(root, asked));
  if (statSync(real).isDirectory()) {
    const marker = path.join(real, WORKFLOW_MARKER);
    need(
      existsSync(marker),
      `${rel} has no ${WORKFLOW_MARKER} — that is what makes a folder a Workflow`
    );
    // CONFINED AGAIN, not once. The folder cleared the boundary; the file inside it is a separate
    // path and may be a symlink to anywhere on the disk — and it is the one that gets executed.
    return confine(root, path.join(asked, WORKFLOW_MARKER), marker);
  }
  // AFTER resolution, because a folder has no extension to check — and against the RESOLVED path,
  // so a `link.py` pointing at something else is refused for what it is rather than admitted for
  // what it is called. Reported as what the caller typed, which is the string they can fix.
  need(real.endsWith('.py'), `${JSON.stringify(rel)} is not a .py file`);
  return real;
}

/**
 * `nscheck.py` → `nscheck`, when the flat file is gone and the folder is there.
 *
 * THE OLD SPELLING HAS TO KEEP WORKING. `serve nscheck.py` is in muscle memory, in the wiki and in
 * this repo's own docs, and `nscheck.py` is now `nscheck/workflow.py` — so without this the move
 * turned every remembered command into "no such workflow" about a workflow that is right there.
 *
 * IT RESOLVES TO THE FOLDER RATHER THAN ALIASING THE PATH, which is the difference that matters
 * downstream: the resolved path is `nscheck/workflow.py`, so the session name, the pane the Monitor
 * looks for and the relative path handed to the CLI all come from the folder. Aliased, the same
 * workflow would have served into `nscheck` under one spelling and `nscheck` under the other only
 * by luck — and any spelling that produced a second session would have been a second worker on the
 * same queue.
 *
 * ONLY WHEN THE FLAT PATH IS NOT THERE. A real flat `nscheck.py` beside a `nscheck/` folder is two
 * workflows, and the one the operator named is the one that exists.
 */
function legacyFolderFor(root: string, rel: string): string | undefined {
  if (!rel.endsWith('.py')) return undefined;
  const stem = rel.slice(0, -'.py'.length);
  if (stem === '' || existsSync(path.resolve(root, rel))) return undefined;
  const candidate = path.resolve(root, stem);
  if (!isDirectory(candidate)) return undefined;
  return existsSync(path.join(candidate, WORKFLOW_MARKER)) ? stem : undefined;
}

/** Exists, and inside the root after symlinks — the boundary itself, applied to a folder and then
 *  again to the file inside it. `shown` is what the caller typed, so the refusal names their path
 *  and not the one this resolved to. */
function confine(root: string, shown: string, candidate: string): string {
  need(existsSync(candidate), `${shown}: no such workflow in ${root}`);
  const real = realpathSync(candidate);
  const inside = path.relative(root, real);
  need(
    inside !== '' && !inside.startsWith('..') && !path.isAbsolute(inside),
    `${shown} resolves outside .kontra/workflows/ — refusing to serve it`
  );
  return real;
}

/**
 * The tmux session a served workflow runs in: THE WORKFLOW'S NAME, from the file.
 *
 * `cli/identity.go:workflowSession` is the peer, derived independently there. It was
 * `kontra-wf-<queue>`, and both prefixes were compensating for the same thing: `kontra-` was how
 * `panels/local.ts` found local Terminals at all, and `wf-` kept a workflow served on a queue named
 * after an actor from colliding with that actor's Worker session. Discovery is the `@kontra` tmux
 * option now and it carries the KIND, so neither has anything left to do.
 *
 * TAKES THE FILE, not the queue. A queue is a routing decision made per session; the file is what
 * is being served, and it is what an operator would type at a `tmux attach`.
 *
 * AND THE FOLDER, when the file is that folder's `workflow.py`. Every workflow folder holds a file
 * of that one name, so the stem alone named all of them `workflow`: `nscheck/workflow.py` and
 * `ping/workflow.py` served into ONE session, the second serve was refused as "already exists"
 * against the first one's worker, and the Monitor — which finds a pane by this name — showed one
 * pane for two workflows.
 */
export function workflowSession(file: string): string {
  const stem = path.basename(file, path.extname(file));
  const named = stem === WORKFLOW_STEM ? parentName(file) || stem : stem;
  // Sanitised because tmux rewrites `.` and `:` to `_` at creation, and a name may carry either —
  // `my.workflow.py` has a stem with a dot in it. See `panels/tmux.ts:tmuxSafeName`.
  const base = tmuxSafeName(named);
  return base === '' || base === '_' ? 'workflow' : base;
}

/** The name of the folder a path sits in, or '' when there is none to take a name from — a bare
 *  `workflow.py` has `.` for a parent and `/workflow.py` has `/`, and neither is a workflow's name. */
function parentName(file: string): string {
  const parent = path.basename(path.dirname(file));
  return parent === '' || parent === '.' || parent === '..' || parent === path.sep ? '' : parent;
}

/**
 * The task queue a workflow serves on: `wf-<name>-<version>`, off its manifest — THE SAME SHAPE AN
 * ACTOR'S QUEUE HAS, which is the point.
 *
 * PEER OF `cli/identity.go:workflowQueue`, and the two must stay byte-identical: the CLI's string is
 * where the worker actually polls, and this one is what the page reports and what `start` routes to.
 * A drift is a run dispatched to a queue nobody serves, which reports `running` forever.
 *
 * ── IT USED TO BE `<digest12>`, A HASH OF THE FOLDER'S BYTES, AND THAT IS NOW GONE ──────────────
 *
 * The argument for the digest was real and is worth keeping written down, because this change gives
 * it up deliberately rather than by forgetting it: `(name, version)` is a free string on a folder
 * (ADR 0004 / 0011), so editing `workflow.py` without bumping `workflow.json` keeps the same queue,
 * and two checkouts of one manifest share it. A content digest made the queue BE the code — edit the
 * folder and a re-serve lands somewhere new, unable to steal a live run.
 *
 * WHAT IT COST, MEASURED ON 2026-08-29. An operator cannot read, type, remember or paste the queue,
 * and it moves under them: three edits to one `workflow.py` in one session produced
 * `wf-nscheck-1b6777a07395`, `-839897c865fe`, then `-12cd1fbaaf48`, and every `start` against a
 * remembered one failed with "no worker is serving this folder's code". The error is accurate and
 * unactionable. Worse, it is not the shape the rest of the product uses: an Actor is
 * `nscheck-0.1.0` everywhere — in `kontra workers list`, in the Monitor, in `task-queue describe` —
 * and a Workflow being a hash in the same column made two identities out of one idea.
 *
 * The digest's guarantee is recovered where it belongs: `resolveWorkflowFile` still reports the
 * folder's content digest, and the console shows it beside the queue. Identity for ROUTING is the
 * manifest; identity for WHICH BYTES is the digest. Conflating them made routing unspeakable in
 * order to answer a question routing was not asking.
 *
 * WHY `wf-` SURVIVES, AND IS NOT NEGOTIABLE. An actor's shared queue is `<name>-<version>`, and this
 * repo ships an actor AND a workflow both called `nscheck` at 0.1.0. Without the prefix they are the
 * SAME STRING — `nscheck-0.1.0` — and Temporal would hand `NsCheck` workflow tasks to the actor's
 * handler, which knows only `RunWorkflowName` and fails every task forever. The prefix is the one
 * piece of this name that is load-bearing rather than cosmetic.
 */
export function workflowQueue(name: string, version: string): string {
  if (name === '' || version === '') return '';
  return `wf-${name}-${version}`;
}

/** The first 12 hex of a folder's content digest — the untypable half of its queue name. */
export function workflowDigest12(dir: string): string {
  return folderDigest(dir).replace(/^sha256:/, '').slice(0, 12);
}

/** The same short digest for a FLAT `.py` workflow: the sha256 of its own bytes (the simplest
 *  cross-language contract, matching `cli/identity.go:fileDigest12`). A flat file has no folder to
 *  digest and no manifest to name it, so it is content-bound by its own bytes and named by its stem. */
export function fileDigest12(file: string): string {
  return createHash('sha256').update(readFileSync(file)).digest('hex').slice(0, 12);
}

/**
 * {@link workflowQueue} for a resolved workflow file, derived from its content and UNTYPABLE.
 *
 * A FOLDER's `workflow.py` is named by its manifest and digested over the whole folder; a flat
 * `.py` (still supported by {@link resolveWorkflowFile}) is named by its stem and digested by its
 * own bytes. Both are `wf-<name>-<digest12>`, and both move when the code moves — the peer of
 * `cli/identity.go:queueForWorkflow`.
 */
export function workflowQueueFor(file: string): string {
  const dir = path.dirname(file);
  if (path.basename(file) === MARKER.workflow) {
    let parsed: { name?: string; version?: string };
    try {
      parsed = readManifest(path.join(dir, MANIFEST.workflow));
    } catch {
      parsed = {};
    }
    const name = (parsed.name ?? '').trim();
    // A MANIFEST WITH NO VERSION IS A REFUSAL, not a default to `0.0.0` or to the digest. Both of
    // those put a workflow on a queue the operator did not choose and cannot predict, which is the
    // whole fault being fixed here. `--init` writes a version; the message says so.
    const version = (parsed.version ?? '').trim();
    const queue = name === '' || version === '' ? '' : workflowQueue(name, version);
    need(
      queue !== '',
      `${dir}/${MANIFEST.workflow} needs both "name" and "version" to derive a queue — register it with --init`
    );
    return queue;
  }
  // A FLAT `.py` HAS NO MANIFEST AND SO NO VERSION. It is named by its stem and versioned `0.0.0` —
  // stated, not hashed, so the queue is still readable and still stable across an edit. A flat file
  // is the shape a folder replaces; it does not get a second identity scheme of its own.
  const name = path.basename(file, path.extname(file));
  const queue = workflowQueue(name, FLAT_FILE_VERSION);
  need(queue !== '', `${file} could not be read to derive a queue from`);
  return queue;
}

/** What a flat `.py` workflow's queue carries where a folder carries its manifest version. */
export const FLAT_FILE_VERSION = '0.0.0';

export interface ServeInput {
  /** A workflow FOLDER (`nscheck`) or a flat file (`nscheck.py`), relative to the workflow root —
   *  whichever it is, {@link resolveWorkflowFile} answers with the file that gets run. There is NO
   *  queue field: the queue is derived from the folder's content and cannot be passed (see the
   *  header on GitHub #15 — a queue a caller can type is a routing decision an agent can get wrong). */
  file: string;
}

export interface ServeResult {
  file: string;
  queue: string;
  /** The tmux session the worker is in — also how the Dashboard will name its Terminal. */
  session: string;
  /** What an operator would type to reach the same thing from a shell. */
  attach: string;
}

/**
 * Serve a workflow file as a detached tmux worker.
 *
 * Shells out to the CLI rather than reimplementing it, because the CLI already knows the four
 * things that are easy to get wrong — the repo root, the venv interpreter, the actorkit
 * PYTHONPATH, and that a tmux pane needs each variable passed explicitly. A second implementation
 * of those would drift, and the failure when it did would be a worker that starts and registers
 * nothing.
 *
 * Rejects when the session already exists, which the CLI reports as an error: two workers on one
 * queue are rivals, and silently having two is worse than being told.
 */
export async function serveWorkflow(input: ServeInput): Promise<ServeResult> {
  // THE FILE, not what the caller typed. A folder resolves to its `workflow.py`, and handing the
  // CLI the resolved path is what keeps the two independent session derivations fed the same
  // string — `cli/identity.go:workflowSession` names the session the worker actually lands in, and
  // this one names the session the page then goes looking for.
  const file = resolveWorkflowFile(input.file);

  // DERIVED FROM THE FOLDER, never passed. There is no queue argument on this route (see ServeInput),
  // so the queue below is the derived one the CLI will also compute from the same folder — reported
  // as a fact, not routed on. The CLI re-derives it; we do not pass `--queue` (there is no such flag).
  const queue = workflowQueueFor(file);
  need(QUEUE_RE.test(queue), `queue ${JSON.stringify(queue)} is not a task-queue name`);
  const root = workflowRoot();
  const rel = path.relative(root, file);

  // argv ARRAY, no shell: the path is validated above, and the CLI derives the queue itself from the
  // folder — the two derivations are byte-identical peers, so no queue crosses this boundary.
  const argv = ['workflow', 'serve', rel, '--tmux'];
  const { code, stdout, stderr } = await run(kontraBin(), argv, root, serveEnv());
  if (code !== 0) {
    const detail = cliDetail(stderr, stdout);
    throw new ControlRefused(`serve failed (exit ${code}): ${detail || 'no output'}`);
  }

  const session = workflowSession(rel);
  return { file: rel, queue, session, attach: `tmux attach -t ${session}` };
}

/**
 * The last few USEFUL lines a failed CLI run said, with the banner thrown away.
 *
 * THE BANNER IS FOUR LINES OF ASCII ART and the CLI prints it before everything, failures
 * included. Taking "the last four lines" therefore took three lines of the message and one line of
 * a drawing — which is what the Serve button reported for a refusal that had a perfectly good
 * sentence in it:
 *
 *   serve failed (exit 1): |_|\_\___/|_|\_| |_| |_|_\/_/ \_\ | error: tmux session "probe-0_1_0"
 *   already exists — a worker is already running there. | attach: … | replace: …
 *
 * An operator reads the backslashes, decides the button is broken, and never reaches the sentence
 * that tells them exactly what to do.
 *
 * A LINE WITH NO LETTER AND NO DIGIT IS NOT A MESSAGE. That is the whole rule, and it is exact
 * rather than a guess: every line of the banner is drawn from `_\/|'<>()-` and spaces alone, and
 * no diagnostic this CLI emits is. Blank lines go with them, so four lines of output are four
 * lines of content.
 */
export function cliDetail(stderr: string, stdout: string, keep = 4): string {
  const lines = (stderr || stdout)
    .split('\n')
    .map((l) => l.trimEnd())
    .filter((l) => /[A-Za-z0-9]/.test(l));
  return lines.slice(-keep).join(' | ').trim();
}

export interface StartInput {
  /** The workflow FOLDER (`nscheck`) or flat file, relative to the workflow root. The queue is
   *  DERIVED from it and is not a parameter — the whole point of GitHub #15. */
  file: string;
  /** The `@workflow.defn` class to start. Optional: absent reads the manifest's `workflow` field,
   *  which is the folder's one canonical class. A folder can declare several, so the page passes the
   *  one it is showing; the queue is still the folder's, never the type's. */
  type?: string;
  /** The workflow's ONE argument. Omitted starts it with none. */
  input?: unknown;
}

export interface StartResult {
  runId: string;
  type: string;
  queue: string;
  /**
   * The caller workflow's manifest identity as it was SNAPSHOTTED for this Run (ADR 0029 §2), or
   * absent when the folder has no manifest to snapshot (a flat `.py`) or the write did not land.
   *
   * Reported rather than silent because the Dataset name depends on it: a Run started without it
   * renders an Actor-grain name, and an operator watching a start deserves to see which they got.
   */
  workflow?: { name: string; version: string };
}

/**
 * Where a Run's caller-workflow identity is snapshotted — the one method {@link startRun} needs of
 * `data/runWorkflows.ts`. A seam, not the class, so a test proves the stamp without a database and
 * `startRun` keeps its "nothing is persisted here about the RUN's progress" property: the only thing
 * written is provenance that must outlive Temporal's retention (ADR 0025).
 */
export interface RunWorkflowRecorder {
  record(runId: string, workflow: string, version: string): Promise<void>;
}

/**
 * The CREDENTIAL preflight a start is refused by (issue 20; `secrets/slots.ts`).
 *
 * A seam rather than the class, for the reason the recorder above is one: a test proves the
 * refusal without a secret store, a key file or a directory in the operator's `~/.kontra`.
 */
export interface RunSlotGate {
  /** The sentence to refuse with, or `null` when every named actor can resolve what it declares. */
  refuseRun(actors: readonly ActorRef[]): Promise<string | null>;
}

/**
 * Which Actors a run will use, off the caller folder's own `workflow.json`.
 *
 * `"actors": ["probe@0.1.0", "subfinder"]` — names, optionally pinned. THE ORCHESTRATOR CANNOT
 * DERIVE THIS, and pretending otherwise is the one thing that would make the gate below dishonest:
 * a caller workflow dispatches with ordinary Python control flow (`catalog.actor(...)` inside a
 * `@workflow.defn`), so which Actors a run reaches is a fact about code this process does not
 * import and must not parse. Declaring them is therefore the operator's, and it is opt-in: a
 * manifest that names none is gated on nothing, which is honest rather than convenient.
 *
 * A malformed entry is DROPPED rather than refused. This field is the caller's aid, not their
 * contract — a typo in it must not make a workflow unstartable, and the credential it would have
 * gated still fails at the actor with the same sentence.
 */
export function manifestActors(manifest: { actors?: unknown }): ActorRef[] {
  if (!Array.isArray(manifest.actors)) return [];
  return manifest.actors.map(toActorRef).filter((a) => a.name !== '');
}

/**
 * Start a Run — one execution of a caller's workflow, on the queue DERIVED from its folder.
 *
 * NO QUEUE IS PASSED IN, and none is trusted from the client: the queue is `wf-<name>-<digest12>`
 * over the folder as it is on disk (GitHub #15). A start therefore cannot be aimed at an arbitrary
 * nickname, and — because the digest IS the code — it REFUSES if no worker polls that queue: a
 * stale or un-served checkout derives a queue nobody serves, and dispatching there would sit
 * `running` forever with no error. The refusal names the fix rather than hoping.
 *
 * The workflow id IS the run id (ADR 0023 §12), minted the same way `kontra workflow start` mints
 * it so a Run started from the page and one started from a shell are indistinguishable afterwards.
 * No run STATE is persisted here — the Runs view reads Temporal. The one thing that is written is
 * the caller's manifest identity (ADR 0029 §2, `data/runWorkflows.ts`): Temporal forgets the
 * execution after 24h and a Dataset outlives it, so the identity its name renders has to be
 * snapshotted while it is known (ADR 0025's pattern). The CLI's start path stamps the SAME record
 * over `PUT /api/runs/:runId/workflow`, because if only one path stamped it the name a Dataset
 * carries would depend on which command an operator happened to use.
 *
 * THE STAMP IS BEST-EFFORT AND HAPPENS AFTER THE START. The run has already begun by then, so a
 * store hiccup must not turn a successful start into a failure the caller retries — and it cannot
 * lose anything that is not recoverable: an unstamped Run renders an Actor-grain name (the documented
 * fallback in `withDatasetNames`) rather than none. Stamping BEFORE the start would instead leave a
 * record for a Run that never existed.
 *
 * The describer and the recorder are injected for the same reason `stopRun`'s clock is — so a test
 * never dials Temporal and never opens a database.
 */
export async function startRun(
  input: StartInput,
  describer?: QueueDescriber,
  recorder?: RunWorkflowRecorder,
  gate?: RunSlotGate
): Promise<StartResult> {
  const file = resolveWorkflowFile(input.file);
  const queue = workflowQueueFor(file);
  need(QUEUE_RE.test(queue), `queue ${JSON.stringify(queue)} is not a task-queue name`);

  // The SAME manifest read that resolves the `@workflow.defn` class also carries the identity the
  // Dataset name renders — `workflow.json` holds `name` and `version` beside `workflow`, so this is
  // one file read, not a new authority.
  const manifest = (() => {
    try {
      return readManifest(path.join(path.dirname(file), MANIFEST.workflow)) as {
        workflow?: string;
        name?: string;
        version?: string;
        /** The Actors this caller dispatches to — see {@link manifestActors}. */
        actors?: unknown;
      };
    } catch {
      return {};
    }
  })();
  const type = ((input.type ?? '').trim() || (manifest.workflow ?? '').trim());
  need(
    TYPE_RE.test(type),
    type === ''
      ? `${path.dirname(file)} has no ${MANIFEST.workflow} naming its @workflow.defn class — register it with --init`
      : `${JSON.stringify(type)} is not a workflow type name`
  );

  /* REFUSE IF A CREDENTIAL THIS RUN NEEDS IS NOT GRANTED (issue 20).
     BEFORE the poller check and before the start, because this is the refusal that has to come
     first in wall-clock terms: an actor discovers an unbound slot in `@actor.load`, which on a
     fleet is after Machines have been provisioned, an image has been pulled and a Session has been
     opened — so failing there is a refusal that has already spent money and minutes. Here it costs
     one file read.
     THE SENTENCE NAMES EVERY SLOT (`slots.ts:unboundRefusal`), not the first one: an operator who
     binds one credential, starts again and is told about the next has been made to pay for the
     round trip twice. Nothing is resolved to perform this check — a preflight that read the
     credentials to prove they need not be read would be the wrong shape entirely. */
  const declared = manifestActors(manifest);
  if (declared.length > 0) {
    const refusal = await (gate ?? slotStore()).refuseRun(declared);
    if (refusal) throw new ControlRefused(refusal);
  }

  // REFUSE IF NOTHING SERVES THIS DIGEST. `describeQueue` reports identities and an `error` that
  // means "could not ask Temporal", which is not the same as "nobody is serving" — but for a start,
  // both are a refusal: we will not dispatch onto a queue we cannot confirm a worker polls.
  const state = await describeQueue(describer ?? temporalQueueDescriber(), queue);
  if (state.error !== undefined) {
    throw new ControlRefused(
      `cannot verify a worker is serving ${type} on ${queue}: ${state.error} — serve it first`
    );
  }
  if (state.identities.length === 0) {
    throw new ControlRefused(
      `no worker is serving this folder's code — queue ${queue} has no pollers. The queue is derived ` +
        `from the folder's content digest, so serve THIS code and start will find it (a pasted queue ` +
        `from an old session cannot help).`
    );
  }

  const client = await getClient();
  const workflowId = `${type.toLowerCase()}-${Math.floor(Date.now() / 1000)}`;
  const handle = await client.workflow.start(type, {
    taskQueue: queue,
    workflowId,
    args: input.input === undefined ? [] : [input.input],
  });

  const workflow = await stampRunWorkflow(handle.workflowId, manifest, recorder);
  return { runId: handle.workflowId, type, queue, ...(workflow ? { workflow } : {}) };
}

/**
 * Snapshot the caller's manifest identity for a Run that has just started — ADR 0029 §2's half of
 * ADR 0025's pattern. Returns what was recorded, or undefined when there was nothing to record or the
 * write did not land.
 *
 * SWALLOWS ITS FAILURES BY DESIGN, and this is the one place the reason has to be written down: the
 * Run is already running when this is called. Rethrowing would report a started Run as a failed
 * start, which is the worst outcome available — the caller retries, Temporal refuses the duplicate
 * id, and an operator concludes the run never began. The cost of losing the write is bounded and
 * documented: the Dataset renders an Actor-grain name via `withDatasetNames`'s fallback.
 *
 * A FOLDER WITH NO MANIFEST records nothing. A flat `.py` workflow has no `workflow.json` to name or
 * version it (`workflowQueueFor` digests its own bytes instead), so there is no identity to snapshot
 * and the fallback is the only honest answer.
 */
async function stampRunWorkflow(
  runId: string,
  manifest: { name?: string; version?: string },
  recorder?: RunWorkflowRecorder
): Promise<{ name: string; version: string } | undefined> {
  const name = (manifest.name ?? '').trim();
  const version = (manifest.version ?? '').trim();
  if (name === '' || version === '') return undefined;
  try {
    const store = recorder ?? runWorkflowStore();
    await store.record(runId, name, version);
    return { name, version };
  } catch {
    return undefined;
  }
}

export interface PauseResult {
  file: string;
  session: string;
  paused: boolean;
  /** What just happened to the worker, and what it does NOT mean. Always set. */
  detail: string;
}

/**
 * Pause or resume the WORKER a workflow is served in.
 *
 * NOT A TEMPORAL OPERATION. There is no primitive for pausing a workflow; what exists is a worker
 * that polls and one that does not. Stopping it stops workflow tasks being processed, so the run
 * makes no progress and resumes from history when the worker comes back — durable, and exactly as
 * simple as it sounds. Two consequences are not simple and the caller is told both:
 *
 *   • Activities ALREADY DISPATCHED keep running. They are on the actors' workers, so a paused
 *     caller does not pause the fleet; it stops deciding what to do next.
 *   • ScheduleToStart and StartToClose timers KEEP TICKING. A long pause does not hold a run, it
 *     FAILS one — the opposite of what the word promises.
 *
 * Shells out to the CLI for the same reason `serveWorkflow` does: the CLI owns the repo root, the
 * venv interpreter, the actorkit PYTHONPATH and the fact that a tmux pane must be told each
 * variable explicitly. A second implementation would drift, and the failure when it did would be a
 * worker that comes back with a different environment from the one that was paused.
 */
export async function pauseWorkflow(
  input: { file: string },
  resume = false
): Promise<PauseResult> {
  const file = resolveWorkflowFile(input.file);
  const root = workflowRoot();
  const rel = path.relative(root, file);

  // NO QUEUE. `resume` re-derives it from the folder exactly as `serve` did — there is no `--queue`
  // flag to forward, and forwarding one would let a paused worker come back on a queue nobody typed
  // correctly. A folder edited while paused re-derives a NEW queue on resume, which is the intended
  // signal that the code changed (GitHub #15), not a bug to paper over.
  const argv = ['workflow', resume ? 'resume' : 'pause', rel];
  const { code, stdout, stderr } = await run(kontraBin(), argv, root, serveEnv());
  if (code !== 0) {
    const detail = cliDetail(stderr, stdout);
    throw new ControlRefused(`${resume ? 'resume' : 'pause'} failed (exit ${code}): ${detail || 'no output'}`);
  }
  return {
    file: rel,
    session: workflowSession(rel),
    paused: !resume,
    detail: resume
      ? 'the worker is polling again and the run continues from history.'
      : 'the worker stopped polling, so the run makes no progress. Activities already dispatched ' +
        'keep running, and its timeouts keep ticking — a long pause fails a run rather than holding it.',
  };
}

// --- stopping a run -----------------------------------------------------------------------------

/**
 * How a run ended, when somebody stopped it.
 *
 * `cancelled` is the good outcome and `terminated` is the one that costs something — see
 * {@link stopRun} for what, exactly.
 */
export type StopOutcome =
  /** It closed. Its scope exits ran, so anything it held has been torn down. */
  | 'cancelled'
  /**
   * The cancellation was DELIVERED and has not landed yet.
   *
   * A real and ordinary state, not a failure: a workflow blocked in an activity cannot act on a
   * cancellation until that activity returns. Reporting it as `cancelled` would claim a teardown
   * that has not happened, and reporting it as a failure would send an operator to terminate a run
   * that is in the middle of cleaning up properly.
   */
  | 'cancelling'
  /** Closed unilaterally. NO scope exit ran — see {@link stopRun}. */
  | 'terminated'
  | 'already-closed';

export interface StopResult {
  runId: string;
  outcome: StopOutcome;
  /** How long the cancel was given before terminating, when it had to be. */
  waitedMs: number;
  /** What an operator should know, in a sentence. Always set; it is the whole point of the shape. */
  detail: string;
}

/** How long a cancel is given to land before `terminate` stops waiting for it. */
export const STOP_GRACE_MS = 60_000;

/**
 * Stop a run — cancel first, terminate only if the cancel does not land.
 *
 * THE TWO ARE NOT INTERCHANGEABLE, and the difference is measured in Droplets.
 *
 *   CANCEL is cooperative. Temporal delivers a cancellation into the workflow, its `async with`
 *   scopes run their exits, and `fleet.up`'s scope exit destroys the Machines. `stackWorkflow`
 *   already carries the saga leg for exactly this (`compensateOnCancel`).
 *
 *   TERMINATE is unilateral. The workflow is closed where it stands and NO code in it runs again —
 *   so no scope exit, no teardown, and whatever it provisioned keeps billing. A run holding a
 *   four-Machine fleet that is terminated leaves four Machines that nothing is tracking.
 *
 * So "terminate" as a verb an operator reaches for has to mean cancel-then-terminate, or the
 * ordinary way to stop a run would be the way that strands infrastructure. The grace period is
 * what makes the difference visible: a run that cleans up in eleven seconds reports `cancelled`,
 * and one that will not is terminated with a sentence saying what may still be running.
 *
 * A cancel that never lands is a real case, not a hypothetical: a workflow blocked on an activity
 * with no heartbeat cannot be interrupted until that activity returns or times out.
 */
export async function stopRun(
  runId: string,
  opts: {
    /**
     * Terminate if the cancel does not land within the grace period. FALSE never terminates: an
     * operator who asked to cancel gets a cancel, and `cancelling` if it is still in flight.
     */
    escalate?: boolean;
    force?: boolean;
    graceMs?: number;
    now?: () => number;
    sleep?: (ms: number) => Promise<void>;
  } = {}
): Promise<StopResult> {
  need(RUN_ID_RE.test(runId), `${JSON.stringify(runId)} is not a run id`);
  const graceMs = opts.graceMs ?? STOP_GRACE_MS;
  const now = opts.now ?? (() => Date.now());
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const started = now();

  const client = await getClient();
  const handle = client.workflow.getHandle(runId);

  // Already finished? Then there is nothing to stop, and saying so beats a cancel that no-ops.
  const first = await describeOrUndefined(handle);
  if (first === undefined) throw new ControlRefused(`no run ${runId}`);
  if (first !== 'RUNNING') {
    return {
      runId,
      outcome: 'already-closed',
      waitedMs: 0,
      detail: `${runId} was already ${first.toLowerCase()} — nothing to stop.`,
    };
  }

  // FORCE SKIPS THE CANCEL, and it is opt-in for the reason above. It exists because a workflow
  // whose worker is gone cannot process a cancellation at all, and waiting a minute to learn that
  // is a minute an operator spends watching nothing happen.
  if (!opts.force) {
    await handle.cancel();
    // Poll rather than `await handle.result()`: the result throws on cancellation, and the thing
    // being waited for is the STATE CHANGE — a run that closes as `completed` because it finished
    // during the grace period is a perfectly good ending and must not read as a failure.
    const deadline = started + graceMs;
    while (now() < deadline) {
      await sleep(Math.min(1000, Math.max(0, deadline - now())));
      const status = await describeOrUndefined(handle);
      if (status !== 'RUNNING') {
        return {
          runId,
          outcome: 'cancelled',
          waitedMs: now() - started,
          detail:
            `${runId} ${(status ?? 'closed').toLowerCase()} — its scopes ran their exits, so any ` +
            'fleet it held has been destroyed.',
        };
      }
    }

    // STILL GOING, and not escalating. The cancellation is delivered and durable — it does not
    // need re-sending — so the honest answer is that it is in flight, not that it failed.
    if (!opts.escalate) {
      return {
        runId,
        outcome: 'cancelling',
        waitedMs: now() - started,
        detail:
          `${runId} was asked to cancel and has not closed yet. The request is durable, so it will ` +
          'land when the activity it is waiting on returns — a fleet teardown takes about half a ' +
          'minute. `kontra workflow terminate` stops waiting, at the cost of the teardown.',
      };
    }
  }

  await handle.terminate('stopped by an operator');
  return {
    runId,
    outcome: 'terminated',
    waitedMs: now() - started,
    detail:
      opts.force
        ? `${runId} was terminated without asking it to cancel. Its scope exits did NOT run: if it ` +
          'held a fleet, those Machines are still up — `kontra fleet status` lists them.'
        : `${runId} did not settle within ${Math.round(graceMs / 1000)}s, so it was terminated. Its ` +
          'scope exits did NOT run: if it held a fleet, those Machines are still up — ' +
          '`kontra fleet status` lists them.',
  };
}

/**
 * How long to wait for a cancel before escalating, bounded.
 *
 * Both ends matter. A grace of zero is a terminate wearing the word "terminate" — it cancels and
 * escalates in the same breath, which is the behaviour this whole function exists to not have. And
 * an unbounded one is an HTTP request that never returns.
 */
export function clampGrace(ms: number | undefined): number {
  if (typeof ms !== 'number' || !Number.isFinite(ms)) return STOP_GRACE_MS;
  return Math.min(Math.max(ms, 5_000), 300_000);
}

/** How long a plain `cancel` waits before reporting `cancelling`. Short: it is reporting, not
 *  waiting for an outcome, and the cancellation is durable either way. */
export const CANCEL_REPORT_MS = 5_000;

/** A run id is a workflow id kontra minted or an operator typed. Bounded because it reaches a
 *  Temporal call and a log line. */
const RUN_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,254}$/;

/** The execution's status name, or `undefined` when Temporal has never heard of it. */
async function describeOrUndefined(handle: {
  describe(): Promise<{ status: { name: string } }>;
}): Promise<string | undefined> {
  try {
    return (await handle.describe()).status.name;
  } catch (err) {
    if (isNotFound(err)) return undefined;
    // Anything else is an OUTAGE and must not be reported as "no such run" — a typo and an
    // unreachable cluster rendering identically is the distinction this whole branch exists for.
    throw err;
  }
}

/**
 * Did Temporal say it has never heard of this workflow?
 *
 * THREE CHECKS BECAUSE THE SDK DOES NOT GIVE ONE. Measured against a live cluster: asking for a
 * workflow id that does not exist throws `WorkflowNotFoundError: workflow not found for ID: …`,
 * and its `code` is NOT the gRPC 5 the raw service would carry — so the code check alone reported
 * a typo as a 502 `could not stop run`, which reads as an outage. The class name is the reliable
 * signal and the message is the belt to its braces, bounded to the exact sentence rather than a
 * bare `/not found/` that a genuine outage could contain.
 */
function isNotFound(err: unknown): boolean {
  if ((err as { code?: number } | null)?.code === 5) return true;
  const name = (err as { name?: string } | null)?.name ?? '';
  if (name === 'WorkflowNotFoundError') return true;
  return /workflow (execution )?not found/i.test((err as Error | null)?.message ?? '');
}

/**
 * What these routes actually admit, in one line, for the boot log and the page.
 *
 * Said out loud on purpose. The operator chose open, and a choice that is only recorded in a
 * source comment is one the next person meets as a surprise.
 *
 * THE TOKEN NOW GATES MORE THAN serve/start, so this sentence had to grow with it. A Dataset's tag
 * routes travel with the same authority (they are writes about a Run, keyed by its id), and a tag is
 * what the retention sweep reads to decide keep-or-collect — so leaving them open leaves a delete
 * button open, which is worth naming rather than filing under "serve/start".
 */
export function describeExposure(): { open: boolean; detail: string } {
  const open = !process.env.KONTRA_RUN_TOKEN;
  return {
    open,
    detail: open
      ? 'serve/start and a Dataset\'s tag/rename routes are OPEN: anything that can reach this API ' +
        'can run a workflow from the checkout (including one that provisions cloud machines), and ' +
        'can untag a Dataset, which hands it to the retention sweep. Set KONTRA_RUN_TOKEN to gate them.'
      : 'serve/start and a Dataset\'s tag/rename routes require a bearer token (KONTRA_RUN_TOKEN).',
  };
}

/** Spawn and collect. Bounded output and a timeout: `serve --tmux` returns immediately, so
 *  anything slow here is a `kontra` that is not going to answer. */
function run(
  bin: string,
  argv: string[],
  cwd: string,
  envOverride: Record<string, string> = {}
): Promise<{ code: number; stdout: string; stderr: string }> {
  return new Promise((resolve) => {
    let stdout = '';
    let stderr = '';
    let child: ReturnType<typeof spawn>;
    try {
      const env = { ...process.env, ...envOverride };
      child = spawn(bin, argv, { cwd, env, stdio: ['ignore', 'pipe', 'pipe'] });
    } catch (err) {
      resolve({ code: 127, stdout: '', stderr: (err as Error).message });
      return;
    }
    const timer = setTimeout(() => child.kill('SIGKILL'), 60_000);
    child.stdout?.on('data', (b: Buffer) => {
      stdout += b.toString('utf8').slice(0, 64_000);
    });
    child.stderr?.on('data', (b: Buffer) => {
      stderr += b.toString('utf8').slice(0, 64_000);
    });
    child.on('error', (err) => {
      clearTimeout(timer);
      // ENOENT here means the binary is not on PATH — by far the likeliest first failure, and
      // unrecognisable from a bare errno.
      resolve({
        code: 127,
        stdout,
        stderr: `${err.message} (is \`${bin}\` on PATH? set KONTRA_BIN)`,
      });
    });
    child.on('close', (code) => {
      clearTimeout(timer);
      resolve({ code: code ?? 1, stdout, stderr });
    });
  });
}
