/**
 * WHAT THE READ SIDE OF PROVISIONING NEEDS, with no engine attached.
 *
 * Two facts live here — where stack state is on disk, and how a stack is named — and both are
 * read by processes that will never run a `pulumi up`: the API renders the `/infra` dashboard off
 * the checkpoint files (`infra/state.ts`) and parses an fqn out of a route (`server.ts`).
 *
 * THEY MOVED OUT OF `workspace.ts` FOR ONE MEASURABLE REASON. That module imports
 * `@pulumi/pulumi/automation` at its top, so a `parseFqn` — five lines of string handling — pulled
 * the entire Automation API into the module graph of every process that read a route parameter.
 * Since ADR 0031 §1 that graph is the INSTALL's: one process, three roles, and its headline
 * claim is that no Pulumi engine, no cloud credential and no state directory is in it. A
 * provisioner loaded but never called is not a credential leak, but it is the first thing anyone
 * auditing that claim finds, and "it is only imported" is not an answer worth having to give.
 *
 * `workspace.ts` re-exports all of it, so the provisioner's own call sites are unchanged.
 */

/**
 * Where stack state lives. Its own directory, on a volume no teardown path removes — a
 * `docker compose down -v` that took this with it would orphan every machine we own.
 *
 * READ BY THE DASHBOARD TOO, which is why it is here rather than beside the engine. On an
 * install nothing ever writes it and the path simply does not exist; `infra/state.ts` answers an
 * empty list, and the surface says there are no stacks — which is true.
 */
export function stateDir(): string {
  return process.env.KONTRA_PULUMI_STATE_DIR ?? '/data/pulumi-state';
}

export interface StackRef {
  /** Pulumi project, e.g. `kontra-fleet`. */
  project: string;
  /** Pulumi stack, e.g. `run-apex-119`. */
  stack: string;
}

/** `project/stack` — also the Temporal workflow id, which is what makes the workflow the
 * mutex for this stack (ADR 0019). One id, one writer, structurally. */
export function fqn(ref: StackRef): string {
  return `${ref.project}/${ref.stack}`;
}

/** Parse a `project/stack` FQN, rejecting anything that would escape into a path. */
export function parseFqn(s: string): StackRef {
  const [project, stack, ...rest] = s.split('/');
  if (!project || !stack || rest.length) throw new Error(`bad stack fqn: ${s}`);
  for (const part of [project, stack]) {
    if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]*$/.test(part)) throw new Error(`bad stack fqn: ${s}`);
  }
  return { project, stack };
}
