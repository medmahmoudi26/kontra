/**
 * The join between a buildpack image's account of itself and its manifest.
 *
 * The fixture is the REAL metadata and the REAL layer list from a `pack` build published to this
 * install's zot (actors/probe:0.2.0), because the defect this guards against is precisely the one that
 * looks fine against invented data: metadata digests are uncompressed diff_ids and manifest digests are
 * compressed, so a wrong join produces a plausible stack of `unknown` bars and no error anywhere.
 */

import { describe, expect, it } from 'vitest';

import { classifyLayers, lifecycleMetadata, totalBytes, uniqueBytes } from './layers';

// Shapes and relationships are the real ones; the hex is shortened for legibility.
const DIFF = (n: string) => 'sha256:' + n.repeat(64).slice(0, 64);
const BLOB = (n: string) => 'sha256:' + n.repeat(64).slice(0, 64);

const diffIds = [DIFF('1'), DIFF('2'), DIFF('3'), DIFF('4'), DIFF('5'), DIFF('6')];
const layers = [
  { digest: BLOB('a'), size: 29_000_000 }, // run image
  { digest: BLOB('b'), size: 128_000_000 }, // run image (its top layer)
  { digest: BLOB('c'), size: 15_000_000 }, // heroku/python:python
  { digest: BLOB('d'), size: 1_400_000 }, // heroku/python:venv
  { digest: BLOB('e'), size: 374 }, // app
  { digest: BLOB('f'), size: 164 }, // lifecycle
];
const md = {
  app: [{ sha: diffIds[4] }],
  buildpacks: [
    { key: 'heroku/python', layers: { python: { sha: diffIds[2] }, venv: { sha: diffIds[3] } } },
  ],
  runImage: { topLayer: diffIds[1], reference: 'registry:5000/kontra-runtimes/python@sha256:42f2' },
};

describe('classifyLayers', () => {
  it('names every layer of a buildpack image', () => {
    const got = classifyLayers(layers, diffIds, md, 'python:1');
    expect(got.map((l) => l.kind)).toEqual(['runtime', 'runtime', 'deps', 'deps', 'app', 'deps']);
    expect(got[2]!.source).toBe('heroku/python:python');
    expect(got[3]!.source).toBe('heroku/python:venv');
    expect(got[4]!.source).toBe('app');
    expect(got[0]!.source).toBe('runtime python:1');
  });

  it('keeps the MANIFEST digest on every row, never the diff_id', () => {
    // The digest a reader acts on — to ask which other images share a layer, or to size it — is the
    // compressed one. Emitting the diff_id would make every cross-reference find nothing.
    const got = classifyLayers(layers, diffIds, md, 'python:1');
    expect(got.map((l) => l.digest)).toEqual(layers.map((l) => l.digest));
    for (const l of got) expect(diffIds).not.toContain(l.digest);
  });

  it('attributes the whole run image, not just its top layer', () => {
    // The OS is the bulk of the bytes. Marking only `topLayer` as runtime leaves everything beneath it
    // unattributed, which is the opposite of what the Layers tab exists to answer.
    const got = classifyLayers(layers, diffIds, md, 'python:1');
    const runtimeBytes = got.filter((l) => l.kind === 'runtime').reduce((n, l) => n + l.size, 0);
    expect(runtimeBytes).toBe(29_000_000 + 128_000_000);
  });

  it('classifies nothing when the config and the manifest disagree about the image', () => {
    // A wrong kind is a wrong answer to "why is the registry 40 GB", so a length mismatch yields
    // `unknown` rather than a shifted-by-one classification that reads as correct.
    const got = classifyLayers(layers, diffIds.slice(0, 5), md, 'python:1');
    expect(got.every((l) => l.kind === 'unknown')).toBe(true);
    expect(got).toHaveLength(layers.length);
  });

  it('classifies nothing for an image that is not a buildpack image', () => {
    const got = classifyLayers(layers, diffIds, null);
    expect(got.every((l) => l.kind === 'unknown' && l.source === '')).toBe(true);
  });
});

describe('lifecycleMetadata', () => {
  it('reads the label out of a config blob', () => {
    const blob = { config: { Labels: { 'io.buildpacks.lifecycle.metadata': JSON.stringify(md) } } };
    expect(lifecycleMetadata(blob)?.runImage?.topLayer).toBe(diffIds[1]);
  });

  it('is null rather than a throw for a label that will not parse', () => {
    // It is a string written by another process. An unparseable one means `unknown` layers, not a 500
    // on a page that is mostly about other images.
    expect(lifecycleMetadata({ config: { Labels: { 'io.buildpacks.lifecycle.metadata': '{nope' } } })).toBeNull();
    expect(lifecycleMetadata({ config: { Labels: {} } })).toBeNull();
    expect(lifecycleMetadata(null)).toBeNull();
    expect(lifecycleMetadata({})).toBeNull();
  });
});

describe('sizes', () => {
  it('counts only the layers nothing else references as unique', () => {
    // Deleting an image frees the layers nobody else holds. Reporting the total as reclaimable is how a
    // Storage tab promises gigabytes it cannot deliver.
    const got = classifyLayers(layers, diffIds, md, 'python:1');
    const shared = new Set([BLOB('a'), BLOB('b')]); // the run image, shared with every other actor
    expect(totalBytes(got)).toBe(173_400_538);
    expect(uniqueBytes(got, shared)).toBe(173_400_538 - 29_000_000 - 128_000_000);
  });

  it('is the full total when nothing is shared', () => {
    const got = classifyLayers(layers, diffIds, md, 'python:1');
    expect(uniqueBytes(got, new Set())).toBe(totalBytes(got));
  });
});
