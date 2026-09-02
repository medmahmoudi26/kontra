/**
 * THE REGRESSION GUARD FOR THE WHOLE DESIGN: run a workflow that needs a credential, then read its
 * history back off a real Temporal and prove the value is not in it.
 *
 * WHY THIS TEST AND NOT AN ASSERTION ON A TYPE. The property "a secret is never a workflow
 * argument" is not a property of a type — it is a property of every byte a run leaves behind, and
 * the mechanism that would protect a large payload does not apply: the codec is a CLAIM-CHECK, so
 * anything under 128 KiB rides INLINE IN HISTORY IN THE CLEAR (`codec/claimCheck.ts`, ADR 0034 §4
 * finding 6). A credential is a hundred bytes. Nothing hides it; only not putting it there does.
 *
 * WHAT IS SWEPT: the whole `History` proto, walked to the leaves, with every byte buffer decoded —
 * so workflow input, activity input, activity result, workflow result, memo, search attributes,
 * headers and failure messages are all in the haystack. A `JSON.stringify` of the history object
 * would NOT be: payload bodies are byte arrays there, and the sentinel would be invisible as
 * `{"0":100,"1":111,…}` — a green test asserting nothing, which is the worst outcome available.
 * The vacuity check at the end of each test is what keeps that honest: the secret's NAME must be
 * found in the same haystack the value is absent from.
 */

import path from 'node:path';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';

import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { TestWorkflowEnvironment } from '@temporalio/testing';
import { bundleWorkflowCode, Worker, type WorkflowBundle } from '@temporalio/worker';

import { FileSecretBackend } from './fileBackend';
import { withSecret } from './lastHop';
import { SecretStore } from './store';
import type { NamedSecretInput, ProviderCall, ProviderResult } from './namedSecret.testwf';

/** Distinctive enough that finding it anywhere is proof, and finding nothing is not luck. */
const SENTINEL = 'dop_v1_SENTINEL_never_in_the_clear_9f3c';
const SECRET_NAME = 'do-token';

let env: TestWorkflowEnvironment;
let bundle: WorkflowBundle;
let store: SecretStore;
/** What the fake provider was handed — the proof the credential reached the call it was for. */
let sawAuthorization = '';

let queueSeq = 0;
const nextQueue = (): string => `secret-history-${(queueSeq += 1)}`;

beforeAll(async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'kontra-secrets-'));
  store = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
  await store.put(SECRET_NAME, SENTINEL);
  env = await TestWorkflowEnvironment.createLocal();
  bundle = await bundleWorkflowCode({ workflowsPath: path.join(__dirname, 'namedSecret.testwf.ts') });
}, 300_000);

afterAll(async () => {
  await env?.teardown();
});

/**
 * The activity — the LAST HOP, written the way a real one is written.
 *
 * It resolves the name inside itself and answers with the provider's reply. Note what it does NOT
 * do: it does not return the token, it does not put it in a heartbeat, and it does not quote the
 * request in an error. Each of those is one line away, and each would land the credential in
 * history through a different event type.
 */
const activities = {
  async callProvider(input: ProviderCall): Promise<ProviderResult> {
    return withSecret(
      input.credential,
      async (token) => {
        // Stand-in for the provider call. A real one is `fetch(endpoint, { headers: … })`.
        sawAuthorization = `Bearer ${token}`;
        return { status: token === SENTINEL ? 200 : 401, body: `${input.endpoint} accepted the credential` };
      },
      store
    );
  },
};

/**
 * Run it, and hand back BOTH endings plus the history either way.
 *
 * The failure path has to be swept as thoroughly as the success one — a run that failed still
 * wrote a history, and the failure message is a place a careless implementation puts the thing it
 * could not use. So this never rethrows: it records the error and fetches the history regardless.
 */
async function run(
  input: NamedSecretInput
): Promise<{ result?: ProviderResult; error?: unknown; haystack: string }> {
  const taskQueue = nextQueue();
  const worker = await Worker.create({
    connection: env.nativeConnection,
    taskQueue,
    workflowBundle: bundle,
    activities,
  });
  const handle = await env.client.workflow.start('namedSecretWorkflow', {
    taskQueue,
    workflowId: `named-secret-${taskQueue}`,
    args: [input],
  });
  let result: ProviderResult | undefined;
  let error: unknown;
  try {
    result = await worker.runUntil(handle.result());
  } catch (err) {
    error = err;
  }
  return { result, error, haystack: flatten(await handle.fetchHistory()) };
}

/** Temporal wraps a workflow failure several layers deep and only the innermost carries what the
 *  activity said — the same unwrapping `workflows/stack.test.ts` needs. */
function causeChain(err: unknown): string {
  const parts: string[] = [];
  let cur: unknown = err;
  for (let i = 0; i < 8 && cur; i += 1) {
    parts.push(String((cur as Error).message ?? cur));
    cur = (cur as { cause?: unknown }).cause;
  }
  return parts.join(' | ');
}

/**
 * Every string and every byte of a history, concatenated.
 *
 * Walks to the leaves rather than stringifying, for the reason in the header: a payload body is a
 * `Uint8Array` in this object, and a substring search over its JSON form searches a list of
 * integers. Byte buffers are decoded as UTF-8, which is what an inline JSON payload is.
 */
function flatten(node: unknown, out: string[] = []): string {
  if (node === null || node === undefined) return out.join('\n');
  if (typeof node === 'string') out.push(node);
  else if (node instanceof Uint8Array) out.push(Buffer.from(node).toString('utf8'));
  else if (Array.isArray(node)) for (const item of node) flatten(item, out);
  else if (typeof node === 'object') for (const v of Object.values(node)) flatten(v, out);
  else out.push(String(node));
  return out.join('\n');
}

describe('a workflow that names a secret', () => {
  it('leaves no trace of the value in its history — and the name is right there', async () => {
    const { result, haystack } = await run({
      endpoint: 'https://api.example.test/v2/droplets',
      credential: { name: SECRET_NAME },
    });

    // The credential really did reach the call: without this the test could pass by resolving
    // nothing at all, which is the vacuous version of every assertion below.
    expect(result?.status).toBe(200);
    expect(sawAuthorization).toBe(`Bearer ${SENTINEL}`);

    // THE ASSERTION THIS WHOLE SLICE EXISTS FOR.
    expect(haystack).not.toContain(SENTINEL);
    // Not a fragment of it either.
    expect(haystack).not.toContain(SENTINEL.slice(0, 16));

    // …in a history that demonstrably holds this run: the NAME crossed every boundary the value
    // did not, which is the design in one line.
    expect(haystack).toContain(SECRET_NAME);
    expect(haystack).toContain('https://api.example.test/v2/droplets');
    expect(haystack).toContain('namedSecretWorkflow');
  });

  it('keeps the value out of a FAILURE too, which is where messages get careless', async () => {
    // A resolution that fails is the path where an implementation quotes what it was given. The
    // failure here is a revoked secret — the shape of a real incident — and its message travels
    // into history as an ActivityTaskFailed event.
    await store.put('leaked-token', SENTINEL);
    await store.revoke('leaked-token', 1);
    const { error, haystack } = await run({
      endpoint: 'https://api.example.test/v2/droplets',
      credential: { name: 'leaked-token' },
    });
    // It failed for the reason it should have, and the reason names the secret — not its value.
    expect(causeChain(error)).toContain('revoked');
    expect(causeChain(error)).toContain('leaked-token');
    expect(causeChain(error)).not.toContain(SENTINEL);
    // And the failed run's own history — which holds that message as an ActivityTaskFailed event —
    // is as clean as a successful one's.
    expect(haystack).toContain('leaked-token');
    expect(haystack).not.toContain(SENTINEL);
  });

  it('carries only the name when the version is pinned', async () => {
    await store.put(SECRET_NAME, `${SENTINEL}-v2`);
    const { result, haystack } = await run({
      endpoint: 'https://api.example.test/v2/droplets',
      credential: { name: SECRET_NAME, version: 1 },
    });
    // Version 1 is still the original value — a rotation does not break a pinned caller.
    expect(result?.status).toBe(200);
    expect(haystack).not.toContain(SENTINEL);
    expect(haystack).not.toContain(`${SENTINEL}-v2`);
    // A version NUMBER is not a secret, and it is what a pinned reference looks like in history.
    expect(haystack).toContain('"version":1');
  });
});
