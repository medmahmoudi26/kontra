/**
 * `kontra deploy --actor`, run on the INFRA role — because that is the only role holding the
 * Docker socket.
 *
 * ═══ THIS FILE IS `serveDev.ts`'S ARGUMENT, APPLIED TO A SECOND VERB ═══
 *
 * Building an image runs `docker build` and `docker push`. That needs `/var/run/docker.sock`, and a
 * read-write Docker socket is root on the host — anything holding it can `docker run -v /:/host`.
 * `kontra-api` is the container with the published port and the HTTP surface that parses untrusted
 * input; it has no socket and must not get one, because any authentication bypass or path traversal
 * there would then be host root. `kontra-infra` already holds the socket, publishes NO port, and is
 * reachable only over the Compose network.
 *
 * So the build moves to the authority rather than the authority moving to the build. No new
 * privilege exists anywhere as a result of this file; one existing privilege gained one more caller.
 *
 * ═══ THE EDGE CARRIES AN ID, NOT A COMMAND ═══
 *
 * {@link BuildActorInput} names a REGISTERED SOURCE by its id and carries no path, no image name, no
 * tag, no registry and no flags. An edge that let the API say *"build this directory and push it
 * there"* would hand the API the socket by proxy and buy nothing but latency. Every argument below
 * is derived HERE, from what the registration store says the folder is.
 *
 * Read the input as hostile and the worst a caller can do is build a folder somebody already
 * registered on this control plane — which is the operation's entire purpose.
 *
 * ═══ ONE ATTEMPT ═══
 *
 * A build failure is a compile error, a missing dependency, an unreachable registry or a full disk.
 * None of those heal on retry, and retrying a Dockerfile error three times only delays the sentence
 * an operator needs to read. `kontra deploy` is also not free to repeat: it pushes, and a partial
 * push retried is wasted minutes on the one operation a human is watching.
 */

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';

/** What crosses the queue: an id this control plane issued. Nothing executable. */
export interface BuildActorInput {
  /**
   * The registered Source to build. AN ID, deliberately — not a path. A path would make this
   * activity build whatever the caller pointed at, which is the socket by proxy.
   */
  sourceId: string;
}

export interface BuildActorResult {
  actor: string;
  version: string;
  /**
   * The pull reference `kontra deploy` reported, when it reported one.
   *
   * OPTIONAL, AND SCRAPED FROM OUTPUT — which is exactly the shape `serveDev.ts` records having
   * got wrong once ("a field documented as a fact and silently empty is worse than no field"). It
   * is optional HERE rather than promised, and {@link detail} always carries the truth. A reader
   * that needs certainty about what exists should ask the registry (`images.ts`), which is the
   * authority on what was actually pushed.
   */
  image?: string;
  /** The digest, same caveat as {@link image}. */
  digest?: string;
  /** What the CLI printed, trimmed — the whole of it, for an operator to read. */
  detail: string;
}

/** Raised when the id names nothing. Non-retryable by the workflow: a missing row does not heal. */
export class NoSuchSource extends Error {}

/**
 * The seam. `resolve` is how an id becomes a folder, injected so this module can be tested without
 * the orchestrator's database and so the WORKFLOW cannot be handed a different one.
 */
export interface BuildActorDeps {
  resolve: (sourceId: string) => Promise<{ name: string; path: string; kind: string; version?: string } | null>;
  kontraBin: () => string;
  serveEnv: () => NodeJS.ProcessEnv;
}

let deps: BuildActorDeps | null = null;

/** Wired once at worker start — see `infra.ts`. */
export function configureBuildActor(d: BuildActorDeps): void {
  deps = d;
}

/** Test seam: drop the wiring so a test can assert the unconfigured refusal. */
export function resetBuildActor(): void {
  deps = null;
}

export async function buildActor(input: BuildActorInput): Promise<BuildActorResult> {
  if (deps === null) throw new Error('buildActor was not configured — see infra.ts');
  const source = await deps.resolve(input.sourceId);
  if (source === null) {
    throw new NoSuchSource(`no registered source ${JSON.stringify(input.sourceId)}`);
  }
  // THE FOLDER CAN BE GONE while the registration is still listed — the store keeps the row and
  // marks it absent on purpose. Refusing here is the difference between a reason and a riddle:
  // `spawn` reports a missing `cwd` with the same errno and the same sentence as a missing binary,
  // so an operator reads "kontra is not installed" and looks on the wrong machine.
  if (!existsSync(source.path)) {
    throw new NoSuchSource(
      `${source.path} is not on this machine any more — put the folder back, or forget the registration`
    );
  }
  // `source.kind` and not a caller-supplied hint — the store KNOWS, because it discovered the
  // folder and read its manifest. `kontra deploy --actor` on a workflow folder fails several layers
  // from its cause; saying so here is the difference between a reason and that.
  if (source.kind !== 'actor') {
    throw new NoSuchSource(
      `${input.sourceId} is registered as ${JSON.stringify(source.kind)} — only an actor has an image to build`
    );
  }

  const { code, stdout, stderr } = await run(
    deps.kontraBin(),
    ['deploy', '--actor', source.path],
    source.path,
    deps.serveEnv()
  );
  const detail = (stdout.trim() || stderr.trim()).slice(0, 8000);
  if (code !== 0) {
    const why = (stderr.trim() || stdout.trim()).slice(0, 8000);
    throw new Error(`build failed (exit ${code}): ${why || 'no output'}`);
  }
  return {
    actor: source.name,
    version: source.version ?? parseVersion(stdout) ?? '',
    image: parseField(stdout, 'image'),
    digest: parseField(stdout, 'digest'),
    detail,
  };
}

/** `  image:  10.124.0.2:5000/canary:1.1.1` → the value. Undefined when absent — never ''. */
function parseField(out: string, field: 'image' | 'digest'): string | undefined {
  const m = new RegExp(`^\\s*${field}:\\s*(\\S+)\\s*$`, 'mi').exec(out);
  return m ? m[1] : undefined;
}

/** `actor deployed: canary@1.1.1` → `1.1.1`. Only used when the store did not carry a version. */
function parseVersion(out: string): string | undefined {
  const m = /^actor deployed:\s*\S+@(\S+)\s*$/mi.exec(out);
  return m ? m[1] : undefined;
}

/** Spawn and collect, bounded. The workflow's timeout is what stops a runaway build. */
function run(
  bin: string,
  argv: string[],
  cwd: string,
  env: NodeJS.ProcessEnv
): Promise<{ code: number; stdout: string; stderr: string }> {
  return new Promise((resolve) => {
    // MERGED ONTO THIS PROCESS'S ENVIRONMENT, NEVER USED AS THE WHOLE OF IT — `serveEnv()` returns
    // OVERRIDES and is `{}` in the normal Compose case, so passing it as `env` would replace the
    // environment and leave the child with no `PATH`. The error that produces names the wrong
    // thing: `spawn kontra ENOENT`, which reads as "kontra is not installed" when it is on `PATH`
    // and executable the whole time. `serveDev.ts` records paying for that once.
    const child = spawn(bin, argv, {
      cwd,
      env: { ...process.env, ...env },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '';
    let stderr = '';
    // Bigger than serve-dev's cap: a build prints every layer, and the useful part — the failing
    // step — is at the END, so a small cap keeps the half nobody needs.
    const cap = 256 * 1024;
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
