/**
 * The Pulumi Automation API workspace factory (ADR 0019).
 *
 * Everything provider-coupled about *how* Pulumi runs lives here: the state backend, the
 * secrets provider, the working directory, and the credentials handed to the engine. The
 * programs themselves (see ./programs) are ordinary TypeScript and know none of it.
 *
 * Three things here are load-bearing and were established by measurement, not preference:
 *
 * 1. **The backend is `file://`, not S3.** Pulumi's DIY S3 backend does not work against
 *    kontra's SeaweedFS: the first state write after login succeeds and every subsequent
 *    `up`/`destroy` fails with `unexpected end of JSON input`, while reads keep working — so
 *    the backend looks healthy while being unwritable. Measured on 3.244.0 and 3.256.0;
 *    `file://` completed three consecutive up→destroy cycles under the identical program.
 *    See .scratch/pulumi-migration/D5-FINDINGS.md.
 * 2. **The backend must be asserted, never assumed.** A failed `pulumi login` does not stop
 *    the CLI — it silently creates an ephemeral Pulumi Cloud account and deploys THERE, state
 *    included. For a design whose whole premise is no hosted dependency, that is the sharpest
 *    trap in the tool, so {@link assertBackend} fails closed before any operation runs.
 * 3. **Credentials ride in `envVars`, never in stack config.** Config lands in
 *    `Pulumi.<stack>.yaml` and its encrypted form in state; an env var does not appear in
 *    state at all. MEASURED both ways in `pulumiState.test.ts`: a real `up` with the token in
 *    `envVars` leaves it in no file under the state directory, and the same `up` with the same
 *    sentinel passed as stack config writes it into `Pulumi.<stack>.yaml` AND the state history.
 *
 * The credential itself is no longer read here. `DIGITALOCEAN_TOKEN` used to be pulled out of the
 * process environment by {@link engineEnv}, which made "the cloud token lives only in
 * `orchestrator-infra`" a fact about which process held a variable, and made rotation a `.env`
 * edit plus a service restart. Since ADR 0034 §4 the value arrives as an argument, resolved from
 * the secret store by the caller — `infra/credential.ts:resolveProviderEnv`, inside the activity
 * that is about to converge. This file's job is unchanged: put it in `envVars` and nowhere else.
 */

import { mkdir } from 'node:fs/promises';
import * as path from 'node:path';
import { LocalWorkspace, type LocalWorkspaceOptions, type Stack } from '@pulumi/pulumi/automation';
import { stateDir, type StackRef } from './paths';

/**
 * The stack-naming helpers and the state directory are `./paths`'s now, and re-exported here so
 * every provisioner call site is unchanged. They left because the READ side needs them — the
 * `/infra` dashboard and a route parameter — and importing this module to get them pulled the
 * whole Automation API into the appliance's process (ADR 0031 §1).
 */
export { stateDir, fqn, parseFqn, type StackRef } from './paths';

export function backendUrl(): string {
  return `file://${stateDir()}`;
}

/** Scratch space for the engine. Explicit rather than the OS temp default so a restart does
 * not silently change where plugins and workspace metadata are resolved from. */
export function workDir(): string {
  return process.env.KONTRA_PULUMI_WORK_DIR ?? '/data/pulumi';
}

/**
 * The environment the engine runs under.
 *
 * `providerEnv` is the resolved cloud credential — one variable, the one that provider's SDK
 * reads — and it is a PARAMETER rather than a read of `process.env`. That is the whole of ADR
 * 0034 §4 at this level: the value reaches the provider without ever reaching stack config, state
 * or a worker machine, and it exists for the length of one converge rather than the life of a
 * container. An empty object is a legitimate call — `state.ts` and the Dashboard read a checkpoint
 * off disk and need no credential at all.
 *
 * NOTHING HERE READS A CREDENTIAL OUT OF THE ENVIRONMENT, and `credential.test.ts` asserts that by
 * source: a future refactor that "restores" the old read would put the value back on a path that
 * survives a restart, which is precisely the property this change removed.
 */
export function engineEnv(providerEnv: Record<string, string> = {}): Record<string, string> {
  return {
    PULUMI_BACKEND_URL: backendUrl(),
    // Non-interactive by construction: a prompt in a Temporal activity is a hang.
    PULUMI_SKIP_UPDATE_CHECK: 'true',
    // DIY backends forbid the `default` secrets provider, and there is no KMS in this loop.
    // NOT MOVED BY ADR 0034 (§4, finding 3): this is a state DECRYPTION key whose loss makes
    // existing stacks unreadable, which is a different risk from a rotatable cloud token.
    PULUMI_CONFIG_PASSPHRASE: process.env.PULUMI_CONFIG_PASSPHRASE ?? '',
    ...providerEnv,
  };
}

/**
 * Refuse to run unless the workspace really is pointed at our own backend.
 *
 * This exists because of a measured failure, not a hypothetical one: a login that fails leaves
 * the CLI willing to deploy to Pulumi's SaaS instead. Checking the URL we are about to use is
 * cheap; discovering droplets in someone else's account is not.
 */
export function assertBackend(): void {
  const url = backendUrl();
  if (!url.startsWith('file://') && !url.startsWith('s3://')) {
    throw new Error(
      `refusing to run: PULUMI_BACKEND_URL must be a self-managed backend, got ${url}. ` +
        'An unset or failed backend silently deploys to app.pulumi.com.'
    );
  }
  if (!process.env.PULUMI_CONFIG_PASSPHRASE) {
    throw new Error(
      'refusing to run: PULUMI_CONFIG_PASSPHRASE is unset. A DIY backend cannot use the ' +
        'default secrets provider, and an empty passphrase silently changes how secrets encrypt.'
    );
  }
}

/**
 * Select (or create) a stack running `program`, wired to our backend.
 *
 * `providerEnv` is the already-resolved credential, and it goes into `envVars` — the one place a
 * credential may be. It is NOT put into `projectSettings`, `stackSettings` or any config: those
 * are persisted to `Pulumi.<stack>.yaml` and to the state file, which is the directory whose whole
 * job is to be durable and backed up.
 */
export async function selectStack(
  ref: StackRef,
  program: () => Promise<Record<string, unknown> | void>,
  providerEnv: Record<string, string> = {}
): Promise<Stack> {
  assertBackend();
  // The Automation API validates workDir EXISTS and refuses otherwise — it will not create it.
  // On a fresh volume that means the very first converge fails with "Invalid workDir passed to
  // local workspace", which reads like a configuration mistake rather than an empty directory.
  const dir = path.join(workDir(), ref.project);
  await mkdir(dir, { recursive: true });
  // Same for the state directory itself. The DIY backend does not create its own bucket
  // either: on a fresh volume `pulumi stack select` fails with "unable to open bucket", which
  // says nothing about the directory simply not being there yet.
  await mkdir(stateDir(), { recursive: true });

  const opts: LocalWorkspaceOptions = {
    workDir: dir,
    envVars: engineEnv(providerEnv),
    projectSettings: {
      name: ref.project,
      runtime: 'nodejs',
      backend: { url: backendUrl() },
    },
  };
  return LocalWorkspace.createOrSelectStack(
    { projectName: ref.project, stackName: ref.stack, program },
    opts
  );
}
