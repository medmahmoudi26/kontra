/**
 * The tag / untag / rename / reset calls the Datasets page makes (ADR 0029 §1, §4), against a
 * stubbed server.
 *
 * What is pinned here is what the page cannot see for itself: that each affordance addresses the
 * record by `runId` (the key, §4), with the right method and body — a tag ADDED and REMOVED rather
 * than assigned, a rename PUT and reset with DELETE — and that a refusal (an empty tag is a 400)
 * reaches the operator as the server's own sentence rather than a bare status.
 */

import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  addDatasetTag,
  removeDatasetTag,
  renameDataset,
  resetDatasetName,
  type DatasetDeviation,
} from './api';

function reply(status: number, body: unknown): Response {
  const text = typeof body === 'string' ? body : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 400 ? 'Bad Request' : 'OK',
    json: async () => JSON.parse(text) as unknown,
    text: async () => text,
  } as Response;
}

function stubFetch(...answers: Response[]): Array<[string, RequestInit | undefined]> {
  const calls: Array<[string, RequestInit | undefined]> = [];
  let i = 0;
  vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
    calls.push([url, init]);
    return Promise.resolve(answers[Math.min(i++, answers.length - 1)]!);
  });
  return calls;
}

const RUN = 'a3f9c1e2-7b04-4a1d-9c88-0f21e6b3d5aa';

afterEach(() => vi.unstubAllGlobals());

describe('tagging a Dataset — a set, keyed by runId', () => {
  it('POSTs the tag to the run\'s tags collection and returns the post-state deviation', async () => {
    const dev: DatasetDeviation = { runId: RUN, tags: ['prod'] };
    const calls = stubFetch(reply(200, dev));
    const got = await addDatasetTag(RUN, 'prod');
    expect(calls[0]![0]).toBe(`/api/datasets/runs/${RUN}/tags`);
    expect(calls[0]![1]).toMatchObject({ method: 'POST', body: JSON.stringify({ tag: 'prod' }) });
    expect(got).toEqual(dev);
  });

  it('DELETEs one tag by naming it in the path', async () => {
    const calls = stubFetch(reply(200, { runId: RUN, tags: [] }));
    await removeDatasetTag(RUN, 'prod');
    expect(calls[0]![0]).toBe(`/api/datasets/runs/${RUN}/tags/prod`);
    expect(calls[0]![1]).toMatchObject({ method: 'DELETE' });
  });

  it('surfaces the server\'s sentence when a tag is refused (a 400 empty tag)', async () => {
    stubFetch(reply(400, { error: 'a tag cannot be empty' }));
    await expect(addDatasetTag(RUN, '   ')).rejects.toThrow('a tag cannot be empty');
  });
});

describe('renaming a Dataset — a single choice, the derived default underneath', () => {
  it('PUTs the new name to the run\'s name', async () => {
    const dev: DatasetDeviation = { runId: RUN, tags: [], renamedTo: 'the-sweep' };
    const calls = stubFetch(reply(200, dev));
    const got = await renameDataset(RUN, 'the-sweep');
    expect(calls[0]![0]).toBe(`/api/datasets/runs/${RUN}/name`);
    expect(calls[0]![1]).toMatchObject({ method: 'PUT', body: JSON.stringify({ name: 'the-sweep' }) });
    expect(got.renamedTo).toBe('the-sweep');
  });

  it('DELETEs the name to reset to the derived default', async () => {
    const calls = stubFetch(reply(200, { runId: RUN, tags: [] }));
    const got = await resetDatasetName(RUN);
    expect(calls[0]![0]).toBe(`/api/datasets/runs/${RUN}/name`);
    expect(calls[0]![1]).toMatchObject({ method: 'DELETE' });
    expect(got.renamedTo).toBeUndefined();
  });
});
