/**
 * Serve a registered Actor LOCALLY — the peer of `serveWorkflow` for the callee's side.
 *
 * LOCAL ONLY, and that is a decision rather than a first step. `kontra serve --actor` has three
 * modes: `local` puts the worker on this machine, `docker` starts managed containers, and `fleet`
 * places it on Machines that cost money and outlive the tab. A button that could reach the last
 * two would make "try this actor" and "deploy this actor to nine droplets" the same gesture, one
 * dropdown apart — and this page is opened to iterate on code, where the whole value is that
 * starting and stopping is cheap and reversible. The fleet path stays where it is: a command you
 * type, with the flags in front of you.
 *
 * Shells out to the CLI for the reason `serveWorkflow` does: the CLI already knows the repo root,
 * the venv interpreter, the actorkit PYTHONPATH and that a tmux pane needs every variable passed
 * explicitly. A second implementation of those drifts, and the failure when it drifts is a worker
 * that starts and registers nothing.
 */

// `callerFor` MOVED TO @kontra/core (caller.ts): the console renders the same snippet, and this
// module reaches secrets/store, which has no business in a browser bundle.
export { callerFor } from '@kontra/core/caller';

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { hostname } from 'node:os';
import { cliDetail, ControlRefused, kontraBin, serveEnv } from './workflowControl';
import { ACTOR_TOKEN_VAR } from './secrets/identity';
import { secretStore } from './secrets/store';
import { actorSession } from './panels/tmux';
import type { Source } from './sources';

export interface ActorServeResult {
  actor: string;
  version: string;
  /** Where the code was served from — the registered folder, echoed back. */
  path: string;
  /** The tmux session the worker landed in; the Monitor discovers it by this name. */
  session: string;
  attach: string;
}

/** A name that can reach a tmux session and a queue. Same bound as QUEUE_RE in workflowControl. */
const ACTOR_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

export async function serveActor(
  source: Source,
  { restart = false }: { restart?: boolean } = {}
): Promise<ActorServeResult> {
  if (!ACTOR_RE.test(source.name)) {
    throw new ControlRefused(`${JSON.stringify(source.name)} is not an actor name`);
  }
  // THE FOLDER CAN BE GONE while the registration is still listed — `SourceStore.list` keeps the
  // row on purpose and marks it `absent`, because a directory that left with a `git checkout` is
  // the operator's to restore or forget. Refusing here is the difference between a reason and a
  // riddle: the spawn below sets `cwd` to this path, and node reports a missing cwd with the same
  // errno and the same sentence as a missing binary — MEASURED, `spawn echo ENOENT` for
  // `cwd: '/definitely/not/here'`. An operator reads that as "kontra is not installed" and goes
  // looking for the wrong problem on the wrong machine.
  if (source.absent || !existsSync(source.path)) {
    throw new ControlRefused(
      `${source.path} is not on this machine any more — put the folder back, or forget the registration`
    );
  }
  const session = actorSession(source.name, source.version);

  /* RESTART IS THE ONLY WAY AN EDIT REACHES A RUNNING WORKER, and it is the operator's choice
     rather than this function's. A worker holds the code it imported at boot: pressing Serve after
     an edit, on a session that is already up, would otherwise be answered "already serving" — true,
     and the most misleading true sentence on the page, because the edit is not live and everything
     on screen says it is. Killing first is destructive (in-flight Units on that worker die with
     it), so it happens only when asked. A session that is not there is not a failure to report. */
  if (restart) await run('tmux', ['kill-session', '-t', session], source.path, serveEnv());

  // argv ARRAY, no shell. The path came from a registration that resolved it against the
  // filesystem, and the name is bounded above — there is no interpolation for either to escape.
  // `serve`, not `run`. The CLI verb was renamed and `run` is a REDIRECT there, not an alias, so
  // this argv would have exited 1 with a sentence about the new spelling — a serve button that
  // reports "serve failed (exit 1)" for a word only this file still says.
  const argv = ['serve', '--actor', source.path, '--mode', 'local', '--tmux'];
  const { code, stdout, stderr } = await run(kontraBin(), argv, source.path, {
    ...serveEnv(),
    ...actorIdentityEnv(source.name),
  });
  if (code !== 0) {
    const detail = cliDetail(stderr, stdout);
    /* ALREADY SERVING IS A STATE, NOT AN ERROR MESSAGE TO READ. The CLI refuses an existing
       session and says so perfectly well in a terminal, where the next line is a command you can
       type. On a page there is no next line — so the refusal is re-thrown as something the UI can
       branch on and answer with a button, and the sentence names the consequence of pressing it
       rather than a tmux invocation the reader cannot run from here. */
    if (/already exists/i.test(detail)) {
      throw new AlreadyServing(
        `a worker is already serving ${source.name} in tmux session ${session}. Restart it to pick up code you have edited — the worker holds what it imported at boot, and anything in flight on it dies with it.`
      );
    }
    throw new ControlRefused(`serve failed (exit ${code}): ${detail || 'no output'}`);
  }
  return { actor: source.name, version: source.version, path: source.path, session, attach: `tmux attach -t ${session}` };
}

/**
 * The actor's own identity, minted here and handed to its worker in the environment.
 *
 * THIS IS WHAT MAKES "AN ACTOR FETCHES ITS OWN SECRET, AUTHENTICATED AS ITSELF" TRUE (issue 19).
 * The credential is never handed to the actor — not through a **Batch**, not as an argument, both
 * of which are workflow history in the clear — so the actor asks for it, and an asker has to be
 * somebody. Serving is the moment this process knows which actor is starting, so it is the moment
 * the identity is minted. `cli/serve.go` starts the worker from `os.Environ()`, so a variable set
 * on the spawn reaches the actor even through the tmux path, where a pane otherwise inherits the
 * tmux SERVER's environment and nothing an operator exported.
 *
 * MINTED EVERY TIME, not only when a secret exists. The alternative — mint if this actor owns one
 * — makes an actor that was served BEFORE its secret was written fail with a missing-identity
 * error for a secret that is plainly there, which is a riddle. The cost of always minting is the
 * store's key file coming into existence at first serve, which is a file, not a decision.
 *
 * BEST-EFFORT, LOUDLY. A store directory that cannot be written must not stop an actor serving:
 * most actors need no secret at all, and refusing to start one over a store it never asks for
 * would be a new way for the console's Serve button to fail. The worker simply starts without an
 * identity, and `secrets.get()` in the actor says exactly that when it is called.
 */
function actorIdentityEnv(actor: string): Record<string, string> {
  try {
    return { [ACTOR_TOKEN_VAR]: secretStore().mintIdentity(actor).token };
  } catch (err) {
    console.warn(
      `[serve] no identity minted for ${actor}: ${(err as Error).message} — it serves, but ` +
        '`secrets.get()` inside it will refuse until the secret store is writable'
    );
    return {};
  }
}

/**
 * The one refusal that is really a question: there is a worker there already.
 *
 * Its own class rather than a string the route greps, because the route has to answer it
 * DIFFERENTLY — 409, not 400 — and a page has to tell "nothing happened, and here is the button"
 * apart from "this failed". A `ControlRefused` subclass so every existing catch still holds.
 */
export class AlreadyServing extends ControlRefused {}

/** Spawn, collect, never throw — the same shape workflowControl uses, kept local to avoid
 *  exporting a general-purpose command runner from a module about serving. */
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
      child = spawn(bin, argv, { cwd, env: { ...process.env, ...envOverride }, stdio: ['ignore', 'pipe', 'pipe'] });
    } catch (err) {
      resolve({ code: 127, stdout: '', stderr: (err as Error).message });
      return;
    }
    child.stdout?.on('data', (d: Buffer) => (stdout += d.toString()));
    child.stderr?.on('data', (d: Buffer) => (stderr += d.toString()));
    // The folder is checked before this runs, so an ENOENT here is the BINARY and nothing else —
    // and `spawn kontra ENOENT` alone names a file nobody chose to install at a path nobody set.
    // Same sentence `workflowControl.ts` answers with, because it is the same first failure.
    child.on('error', (err) =>
      resolve({ code: 127, stdout, stderr: stderr || `${err.message} (is \`${bin}\` on PATH? set KONTRA_BIN)` })
    );
    child.on('close', (code) => resolve({ code: code ?? 1, stdout, stderr }));
  });
}

/** One Method, as the code on disk declares it — the shape `MethodCall` renders a form from. */
export interface DiskMethod {
  name: string;
  input?: unknown;
  output?: unknown;
}

/** What `kontra actor schema` answers: the actor it read, and every Method it found. */
export interface DiskSchema {
  actor: { name: string; version: string };
  dir: string;
  methods: DiskMethod[];
  /** What an operator types to serve this folder HERE — see {@link serveCommand}. */
  serve: string;
}

/**
 * The command that actually serves this Actor in THIS deployment.
 *
 * IT IS COMPOSED SERVER-SIDE BECAUSE ONLY THE SERVER KNOWS. A browser pane cannot tell whether the
 * control plane is a container, an appliance or a checkout on the reader's own laptop, and the
 * answer differs in every one. Guessing produced a command that could not work: the Compose cluster
 * shows an operator `kontra serve --actor <dir> --watch`, they run it on the host, and the actor
 * process dies on `import temporalio` — the host has no SDK, and the whole point of this install is
 * that Docker is the only prerequisite. The path is right, the binary is on their PATH, and it
 * still cannot work. That is the worst shape an instruction can have.
 *
 * `/.dockerenv` is the container test, and the hostname is the container to exec into —
 * `docker-compose.yml` pins it to `kontra-api` (it has to, for the Monitor's poller identity), so
 * the name in this string is the name on the operator's machine.
 */
function serveCommand(dir: string): string {
  const local = `kontra serve --actor ${dir} --watch`;
  if (!existsSync('/.dockerenv')) return local;
  return `docker exec -it ${hostname()} ${local}`;
}

/**
 * The Methods a folder declares RIGHT NOW, read from the files rather than from the catalog.
 *
 * ── WHY THE CATALOG IS NOT THE ANSWER HERE ──────────────────────────────────────────────────────
 *
 * A catalog entry is published by a WORKER AT BOOT. It is therefore the truth about what is
 * serving, which is exactly what the Actors grid should show — and exactly the wrong thing to draw
 * a form from while somebody is editing the file. Add a parameter, save, and the catalog still
 * describes the code that booted an hour ago; the form beside the editor offers fields that no
 * longer exist and omits the one just written. The editor pane is the one surface where "what is
 * on disk" beats "what is running", because the next thing the author does is serve it.
 *
 * NOT A SECOND DERIVATION. It shells to `kontra actor schema`, which shells to
 * `internals.schemadump`, which calls the same `load_actor` + `operations_of` a booting worker
 * calls — one implementation, held there by `TestActorSchemaMatchesTheCatalogDerivation`. A schema
 * derived here in TypeScript would be a second answer to "what does this Method accept" and would
 * drift the first time either was fixed.
 */
export async function diskSchema(source: { path: string }): Promise<DiskSchema> {
  const { code, stdout, stderr } = await run(kontraBin(), ['actor', 'schema', source.path], source.path, serveEnv());
  if (code !== 0) {
    const detail = cliDetail(stderr, stdout);
    throw new ControlRefused(`reading the schema failed (exit ${code}): ${detail || 'no output'}`);
  }
  try {
    // The CLI answers the schema; the command is this process's to add — it is the only party that
    // knows where "here" is.
    return { ...(JSON.parse(stdout) as DiskSchema), serve: serveCommand(source.path) };
  } catch {
    // The CLI prints a banner before everything; a parse failure here is that banner or a partial
    // write, and the bytes are what a reader needs rather than "unexpected token".
    throw new ControlRefused(`the schema command did not answer JSON: ${stdout.slice(0, 400)}`);
  }
}
