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

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
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

/**
 * The caller workflow that dispatches one Method over a Batch — the ARTEFACT beside the button.
 *
 * IT IS NO LONGER AN ERRAND, AND IT IS NO LONGER THE DISPATCH (ADR 0033 §6). The Actors page calls
 * the Method now: a kontra-owned one-shot workflow performs exactly one Method call over the Batch
 * the form collected, through the same Nexus operation production uses (`backend/src/probe.ts`,
 * `sdk/python/actorkit/probe.py`). What survives here is the half that TEACHES — the page shows this
 * source read-only beside the Run button, regenerating as the form changes, and there is nowhere to
 * write it to any more.
 *
 * WHICH IS WHAT MAKES IT HONEST RATHER THAN MERELY ILLUSTRATIVE: the probe runs the same three
 * lines. `catalog.actor(...)`, a callable handle, a destructured `(results, dropped)`, an optional
 * Dataset writer — not a reimplementation of them, so the code shown beside the button is the code
 * the button runs. An operator who wants to own the dispatch — a loop, a second Method, a retry of
 * the drops — copies this and serves it, and that is composition inside their own workflow (ADR
 * 0023 §16), which was always where it belonged.
 *
 * A NOTE ON A SENTENCE THAT USED TO BE HERE. This comment said "`POST /api/runs` returns 404 on
 * purpose". It has not since 2026-08-15: what ADR 0023 §12 removed was the route that took a GRAPH
 * and started the interpreter. The invariant that survived is narrower and still holds —
 * **the orchestrator starts workflows; it does not execute Batches.**
 *
 * THE DISPATCH IS `handle.<method>(batch)`, RETURNING `(results, dropped)` (ADR 0028 §4). The handle
 * is callable now — a Method name is an attribute on the `ActorHandle` `catalog.actor()` returns
 * (`ActorHandle.__getattr__` in `sdk/python/actorkit/catalog.py`), and awaiting the call hands back
 * a `(results, dropped)` tuple the caller must destructure. This is the SAME spelling the SDK
 * documents and every example uses, so the first file an operator meets teaches the surface the
 * rest of the repo speaks — and it works with or without an `async with`, because a one-shot call
 * on a bare handle is a load/run/close on the actor's shared queue.
 *
 * IT HAS DIED HERE BEFORE, ONE RENAME BEHIND THE SDK. This generator once emitted `handle.<method>`
 * against a handle that had NO `__getattr__` and every file raised `AttributeError` the moment the
 * worker ran it — generated fine, served fine, started fine, died in the worker. The fix then was
 * `handle.dispatch(batch, method="…")`, which the callable handle (ADR 0028) has since retired.
 * `actorControl.test.ts` no longer only parses the output: it EXECUTES it against the real SDK
 * handle, so a spelling the parser accepts but the worker rejects fails the test where it is cheap.
 *
 * A METHOD NAME THE GO SDK ALLOWS BUT PYTHON CANNOT SPELL rides `getattr`. `core.Registry.AddMethod`
 * takes any non-empty string, so `dns-facts` is a real catalogued Method — and `handle.dns-facts`
 * is a SyntaxError, not a slightly-wrong call. `getattr(handle, "dns-facts")(batch)` reaches it and
 * still goes through the same callable-handle path, so the tuple return is identical.
 *
 * THE CALLER NAMES WHERE OUTPUT GOES (ADR 0028 §2) when one is asked for. Given a Dataset name, the
 * caller publishes each returned Batch into that named Dataset once the call returns (per chunk, not
 * per push) — passed as the SECOND positional argument, `handle.<method>(batch, out)`, the same
 * shape the SDK and examples use. Omitted, the results stay an unnamed, chainable Batch and the
 * caller's `run` just reports the counts.
 */
export function callerFor(
  actor: string,
  version: string,
  method: string,
  units: unknown,
  dataset?: string
): string {
  const handle = safeIdent(actor);
  const cls = className(actor, method);
  // A MODULE-LEVEL CONSTANT, not a literal inside the method. Embedding it in the body means
  // re-indenting a multi-line JSON blob to whatever column the statement sits at, and the result
  // parses while looking like a mistake. At module level the JSON's own indentation is correct as
  // printed, and the Batch is the first thing an operator sees when they open the file to edit it.
  const batch = pyLiteral(units ?? [], 4);
  // A NAME REACHES THE CODE AS AN ESCAPED LITERAL. The actor's name and version are read off
  // `actor.json` on the operator's own disk, which is a file nothing bounds, and interpolating one
  // raw meant a single `"` closed the string it sat in and left the rest of the line as source — in
  // a file the operator is about to run. In the DOCSTRING they are still prose, and a `"""` there
  // breaks the file loudly at the first line python reads; inside `catalog.actor(…)` the same
  // character was silent.
  const py = (s: string) => JSON.stringify(s);
  // `handle.head(batch)` when the Method name is a legal Python attribute, `getattr(handle,
  // "dns-facts")(batch)` when it is not — the latter reaches a Method the Go SDK registers that
  // Python cannot spell as an attribute at all. Either way it is one callable-handle call.
  const target = pyAttr(method) ? `${handle}.${method}` : `getattr(${handle}, ${py(method)})`;

  const named = (dataset ?? '').trim();
  // The dispatch line and its surrounding scope. Named: an open Dataset writer the caller hands
  // over as the second positional argument (ADR 0028 §2), so the rows publish under a name a query
  // can reach. Unnamed: the results stay a chainable Batch and nothing is materialized.
  const body = named
    ? `        async with catalog.dataset(${py(named)}).writer() as out:
            results, dropped = await ${target}(batch, out)`
    : `        results, dropped = await ${target}(batch)`;

  return `"""Dispatch ${actor}@${version}.${method}() over a Batch.

Shown by the Actors page beside its Run button — and this IS what that button runs (ADR 0033 §3):
the probe workflow makes the same call, on the same handle, through the same Nexus operation. So
copying this is not a second way to do it; it is the same way, in a file you own.

Own it when you want more than one call: a loop, a second Method, a retry of what was dropped.
That is composition inside YOUR workflow, which is where it belongs — the probe deliberately has
nowhere to put a second Method (ADR 0033 §1). Serve and start it with:

    kontra workflow serve workflow.py --tmux
    kontra workflow start workflow.py --wait

Both verbs take the FOLDER, never a typed queue: the queue is derived from this folder's content
(GitHub #15), so serve and start always agree and a pasted nickname cannot route your run wrong.
"""

from actorkit import catalog
from temporalio import workflow

${handle} = catalog.actor(${py(actor)}, ${py(version)})

# The Batch, as typed on the Actors page. Edit it here, or pass one to workflow start --input.
BATCH = ${batch}


@workflow.defn
class ${cls}:
    @workflow.run
    async def run(self, units: list | None = None) -> dict:
        batch = units if units is not None else BATCH
        # The handle is callable and a Method call returns (results, dropped) (ADR 0028 §4): you
        # cannot reach results without naming the drops, so ignoring them is a choice a reviewer
        # sees. len(dropped) costs no fetch; await dropped.rows() gets the Units back for a retry.
${body}
        return {"units": len(batch), "out": len(results), "dropped": len(dropped)}


if __name__ == "__main__":
    catalog.serve([${cls}])
`;
}

/**
 * The Batch as PYTHON SOURCE, not as JSON — the same layout, three different words.
 *
 * `JSON.stringify(units, null, 4)` was here, and the file it wrote parsed cleanly and then died at
 * import: JSON spells the three atoms `true`, `false` and `null`, and Python reads those as three
 * undefined names. So a Batch with one boolean in it — which is one checkbox on the form that feeds
 * this — generated a caller that raised `NameError: name 'true' is not defined` the first time a
 * worker loaded it, one `kontra workflow serve` and several minutes after the page said it had
 * handed over something runnable. `ast.parse` does not catch it (a bare `true` is a legal Name),
 * which is why `actorControl.test.ts` EVALUATES the constant instead of only parsing the file.
 *
 * Everything else is JSON's layout on purpose — four spaces, one entry per line, keys in
 * double quotes — because the module-level constant this feeds is indented for exactly that, and
 * because `JSON.stringify` on a string is already a valid Python string literal (the escapes it
 * emits are the ones Python reads).
 */
function pyLiteral(value: unknown, indent: number, depth = 0): string {
  const pad = ' '.repeat(indent * (depth + 1));
  const close = ' '.repeat(indent * depth);
  if (value === null || value === undefined) return 'None';
  if (typeof value === 'boolean') return value ? 'True' : 'False';
  // A non-finite number has no Python literal either (`Infinity` is one more undefined name), and
  // `JSON.stringify` turns it into `null` — which is the bug above wearing a different hat.
  if (typeof value === 'number') return Number.isFinite(value) ? JSON.stringify(value) : 'None';
  if (typeof value === 'string') return JSON.stringify(value);
  if (Array.isArray(value)) {
    if (value.length === 0) return '[]';
    return `[\n${value.map((v) => `${pad}${pyLiteral(v, indent, depth + 1)}`).join(',\n')}\n${close}]`;
  }
  if (typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) return '{}';
    const rows = entries.map(([k, v]) => `${pad}${JSON.stringify(k)}: ${pyLiteral(v, indent, depth + 1)}`);
    return `{\n${rows.join(',\n')}\n${close}}`;
  }
  // A bigint or a symbol cannot arrive from a parsed request body; if one ever does, `None` is a
  // Batch entry an operator can see and fix, and a bigint's `toString` is not.
  return 'None';
}

/**
 * Whether a Method name can be spelled as a Python attribute — `handle.head` — or has to go
 * through `getattr(handle, "…")`.
 *
 * The Go SDK's `core.Registry.AddMethod` takes any non-empty string, so `dns-facts` is a Method a
 * worker really self-registers and `handle.dns-facts` is a SyntaxError the parser catches at import,
 * not a wrong call at runtime. A keyword (`class`, `for`) is the same trap wearing a legal-looking
 * name, so both are refused here and routed to `getattr`, which takes the name as a plain string.
 */
function pyAttr(method: string): boolean {
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(method)) return false;
  return !PY_KEYWORDS.has(method);
}

/** Python's reserved words — an attribute cannot be one, so a Method so named rides `getattr`. */
const PY_KEYWORDS = new Set([
  'False', 'None', 'True', 'and', 'as', 'assert', 'async', 'await', 'break', 'class', 'continue',
  'def', 'del', 'elif', 'else', 'except', 'finally', 'for', 'from', 'global', 'if', 'import', 'in',
  'is', 'lambda', 'nonlocal', 'not', 'or', 'pass', 'raise', 'return', 'try', 'while', 'with',
  'yield',
]);

/** A Python identifier from an actor name: `crawl4ai-canon` → `crawl4ai_canon`. */
function safeIdent(name: string): string {
  const ident = name.replace(/[^A-Za-z0-9_]/g, '_');
  return /^[0-9]/.test(ident) ? `a_${ident}` : ident;
}

/** `probe` + `head` → `ProbeHead`. The class name `workflow start` is given. */
function className(actor: string, method: string): string {
  const camel = (s: string) =>
    s
      .split(/[^A-Za-z0-9]+/)
      .filter(Boolean)
      .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
      .join('');
  const name = `${camel(actor)}${camel(method)}`;
  return /^[0-9]/.test(name) || name === '' ? `Call${name}` : name;
}
