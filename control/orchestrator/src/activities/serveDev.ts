/**
 * serve-dev, run on the INFRA role — because that is the only role holding the Docker socket.
 *
 * ═══ WHY THIS IS NOT IN `server.ts` WITH THE REST OF THE SERVE PATH ═══
 *
 * Starting a container needs `/var/run/docker.sock`, and a read-write Docker socket is root on the
 * host: anything holding it can `docker run -v /:/host`. `docker-compose.yml`'s own header says so
 * — *"that is host-level Docker authority… Not tenant isolation"* — and `logship` mounts it
 * READ-ONLY with the line *"the socket is the whole host if it can"*.
 *
 * `kontra-api` is the container with the published port and the HTTP surface that parses untrusted
 * input. It has no socket and must not get one: any authentication bypass or path traversal there
 * would become host root. `kontra-infra` already holds the socket, holds the cloud credential, and
 * publishes NO port — it is reachable only over the Compose network. So the serve moves to the
 * authority rather than the authority moving to the serve. No new privilege exists anywhere as a
 * result of this file; one existing privilege gained one more caller.
 *
 * ═══ THE EDGE CARRIES AN ID, NOT A COMMAND, AND THAT IS THE WHOLE SECURITY PROPERTY ═══
 *
 * A hop that let the API say *"run this image with these arguments"* would give the API the socket
 * by proxy and buy nothing but latency. {@link ServeDevInput} therefore names a REGISTERED SOURCE by
 * its id and carries no image, no argv, no flags and no path. This activity resolves the id against
 * the same store the catalog reads, and everything it then executes is derived HERE.
 *
 * Read the input as hostile and it still cannot reach anything: the worst a caller can do is serve a
 * folder somebody already registered on this control plane, which is the operation's entire purpose.
 *
 * ═══ IT IS AN ACTIVITY, NOT A ROUTE ═══
 *
 * `kontra-api` reaches `kontra-infra` exactly one way today — a Temporal workflow on
 * `infraQueue()`, which is how `fleet.up()` spends money (`infraRoutes.ts:startStackOp`). Adding an
 * HTTP listener to the socket-holding container to carry this would put a second, newer, less
 * reviewed port on the process that must not have one. The queue is already there, already typed,
 * already attributable in history, and already the thing an operator can see in the Runs list.
 */

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';

/** What crosses the queue: an id this control plane issued, and a boolean. Nothing executable. */
export interface ServeDevInput {
  /**
   * The registered Source to serve. AN ID, deliberately — not a path. A path would make this
   * activity run whatever the caller pointed at, which is the socket by proxy.
   */
  sourceId: string;
  /**
   * What the CALLER believes this folder is. A hint for the lookup and nothing more — the store is
   * asked which kind it actually is, and {@link serveDev} branches on THAT. See the refusal there
   * for why a caller that is wrong about this is told rather than obeyed.
   */
  kind: 'workflow' | 'actor';
}

export interface ServeDevResult {
  /** `<name>@<version>` for an actor, the workflow's derived worker name otherwise. */
  worker: string;
  /**
   * What the CLI printed, trimmed. The container ids are in here, and so is the derived queue for a
   * workflow — an operator wants both.
   *
   * THERE WAS A PARSED `queue` FIELD AND IT WAS A LIE HALF THE TIME. It scraped `^queue:` out of
   * this same text, which the workflow verb prints and the actor verb does not, so it was a string
   * for one kind and `''` for the other while its comment called it "a FACT to read back". Neither
   * caller read it: `serveWorkflow` derives the queue itself from the folder — through the one
   * function `shared/conformance/queues.json` pins — and `serveActor` does not report one at all.
   * A field that is documented as a fact and is silently empty is worse than no field.
   */
  detail: string;
}

/** Raised when the id names nothing. Non-retryable by the workflow: a missing row does not heal. */
export class NoSuchSource extends Error {}

/**
 * The seam. `resolve` is how an id becomes a folder, and it is injected so this module can be
 * tested without the orchestrator's database and so the WORKFLOW cannot be handed a different one.
 */
export interface ServeDevDeps {
  resolve: (sourceId: string) => Promise<{ name: string; path: string; kind: string } | null>;
  kontraBin: () => string;
  serveEnv: () => NodeJS.ProcessEnv;
}

let deps: ServeDevDeps | null = null;

/** Wired once at worker start — see `infra.ts`. Separate from the activity so the import graph of
 *  this file stays free of the database for anything that only wants the types. */
export function configureServeDev(d: ServeDevDeps): void {
  deps = d;
}

export async function serveDev(input: ServeDevInput): Promise<ServeDevResult> {
  if (deps === null) throw new Error('serveDev was not configured — see infra.ts');
  const source = await deps.resolve(input.sourceId);
  if (source === null) {
    throw new NoSuchSource(`no registered source ${JSON.stringify(input.sourceId)}`);
  }
  // THE FOLDER CAN BE GONE while the registration is still listed, and the failure mode is worth
  // the check: `spawn` reports a missing `cwd` with the same errno and the same sentence as a
  // missing binary, so an operator reads "kontra is not installed" and goes looking on the wrong
  // machine for the wrong problem. (`actorControl.ts` records the measurement.)
  if (!existsSync(source.path)) {
    throw new NoSuchSource(
      `${source.path} is not on this machine any more — put the folder back, or forget the registration`
    );
  }

  /*
   * EVERY ARGUMENT IS BUILT HERE, FROM THE STORE'S ANSWER AND NOT THE CALLER'S.
   *
   * `source.kind` and not `input.kind`, and the difference is the whole id-only rule in miniature.
   * The request says what it believes the folder is; the registration store KNOWS, because it
   * discovered the folder and read its manifest. Branching on the request would let a caller pick
   * the actor verb for a workflow folder — not a privilege escape, since both verbs are derived
   * here and both run the same registered path, but a confusing failure several layers from its
   * cause, and a lie in this file's own header.
   *
   * A DISAGREEMENT IS REPORTED RATHER THAN SILENTLY CORRECTED. Quietly serving the other kind would
   * make a caller that is wrong about its own catalog look like one that is right.
   */
  if (source.kind !== input.kind) {
    throw new NoSuchSource(
      `${input.sourceId} is registered as ${JSON.stringify(source.kind)}, not ${JSON.stringify(input.kind)}`
    );
  }
  const argv =
    source.kind === 'workflow'
      ? ['workflow', 'serve', source.path, '--mode', 'dev', '--watch']
      : ['serve', '--actor', source.path, '--mode', 'dev'];

  const { code, stdout, stderr } = await run(deps.kontraBin(), argv, source.path, deps.serveEnv());
  const detail = (stderr.trim() || stdout.trim()).slice(0, 4000);
  if (code !== 0) throw new Error(`serve-dev failed (exit ${code}): ${detail || 'no output'}`);
  return { worker: source.name, detail: stdout.trim().slice(0, 4000) };
}

/** Spawn and collect, bounded. serve-dev returns as soon as the containers are up. */
function run(
  bin: string,
  argv: string[],
  cwd: string,
  env: NodeJS.ProcessEnv
): Promise<{ code: number; stdout: string; stderr: string }> {
  return new Promise((resolve) => {
    // MERGED ONTO THIS PROCESS'S ENVIRONMENT, NEVER USED AS THE WHOLE OF IT.
    //
    // `serveEnv()` is a set of OVERRIDES — its own header says so, and it returns `{}` whenever
    // `KONTRA_SERVE_ENV` is unset, which is the normal Compose case. Passing that object straight
    // to `spawn` as `env` REPLACES the environment instead of extending it, so the child got no
    // `PATH` and Node could not resolve the bare name `kontra`.
    //
    // THE ERROR IT PRODUCES NAMES THE WRONG THING: `spawn kontra ENOENT`, which reads as "kontra is
    // not installed" and sends an operator looking for a missing binary. It was on `PATH` and
    // executable at `/usr/local/bin/kontra` the whole time — the comment above about a missing
    // `cwd` wearing the same errno is the same trap, one field over. `actorControl.ts:173` has
    // always spread correctly; this is the one place that did not.
    const child = spawn(bin, argv, {
      cwd,
      env: { ...process.env, ...env },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '';
    let stderr = '';
    const cap = 64 * 1024;
    child.stdout.on('data', (b: Buffer) => {
      if (stdout.length < cap) stdout += b.toString();
    });
    child.stderr.on('data', (b: Buffer) => {
      if (stderr.length < cap) stderr += b.toString();
    });
    child.on('error', (err) => resolve({ code: 127, stdout, stderr: String(err) }));
    child.on('close', (code) => resolve({ code: code ?? 1, stdout, stderr }));
  });
}
