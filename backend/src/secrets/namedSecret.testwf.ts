/**
 * The fixture workflow for the history-sentinel test — and the SHAPE issue 24 copies.
 *
 * A workflow that needs a credential carries a {@link SecretRef}: a name, and optionally a pinned
 * version. It never receives a value, never passes one to an activity and never returns one. The
 * activity it calls resolves that name at the LAST HOP, inside itself, and answers with the RESULT
 * of the call the credential was for.
 *
 * `.testwf.ts` so `tsconfig.json` leaves it out of the build: it is bundled by the test that runs
 * it and is not part of any shipped worker.
 */

import * as wf from '@temporalio/workflow';

import type { SecretRef } from './types';

export interface NamedSecretInput {
  endpoint: string;
  /** THE NAME, and nothing else. This is the field ADR 0034 §4 is about. */
  credential: SecretRef;
}

export interface ProviderCall {
  endpoint: string;
  credential: SecretRef;
}

export interface ProviderResult {
  status: number;
  /** What the provider said, minus anything that came from the credential. */
  body: string;
}

const { callProvider } = wf.proxyActivities<{
  callProvider(input: ProviderCall): Promise<ProviderResult>;
}>({
  startToCloseTimeout: '1 minute',
  retry: { maximumAttempts: 1 },
});

export async function namedSecretWorkflow(input: NamedSecretInput): Promise<ProviderResult> {
  // The name is in workflow history, in the clear, forever — which is exactly why it is a name.
  return callProvider({ endpoint: input.endpoint, credential: input.credential });
}
