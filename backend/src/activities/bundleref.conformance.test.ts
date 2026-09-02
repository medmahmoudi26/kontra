/**
 * The TYPESCRIPT ARM of `conformance/bundleref.json` — where a Bundle lives once it is an OCI
 * artifact (ADR 0036).
 *
 * THIS SIDE IS THE READER AND THE CLI IS THE WRITER, and until this corpus existed the two were
 * pinned by a Go test asserting one literal and a vitest in this directory asserting another,
 * neither aware of the other. That is the hand-copied golden `conformance/README.md` opens with,
 * and the object-store layout it replaces had already drifted that way once.
 *
 * WHAT A DRIFT COSTS, in `resolveBundle`'s own words: it 404s on a Bundle that was published
 * perfectly well, and `fleet.ready()` then waits out its whole timeout on a queue nothing will
 * ever poll. Nothing fails loudly, on either side.
 *
 * The MANIFEST url has one implementation and it is this one — only the control plane resolves a
 * tag, and the CLI lets oras build the URL. That is recorded here rather than mirrored into a Go
 * helper nothing would call, the way `output_dataset.json` records being a two-of-three corpus.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import { bundleBlobUrl, bundleManifestUrl, bundleRepo, REGISTRY_PORT } from './fleet';

type RepoCase = { why: string; name: string; expect: string };
type RefCase = {
  why: string;
  registry: string;
  name: string;
  version: string;
  digest: string;
  tagged: string;
  pinned: string;
};
type UrlCase = {
  why: string;
  registry: string;
  name: string;
  version: string;
  sha: string;
  manifest: string;
  blob: string;
};

const raw = readFileSync(join(__dirname, '../../../conformance/bundleref.json'), 'utf8');
const corpus = JSON.parse(raw) as {
  repo: { cases: RepoCase[] };
  reference: { cases: RefCase[] };
  urls: { cases: UrlCase[] };
  media_types: { artifact: string; config: string; layer: string };
};

describe('the corpus itself', () => {
  it('has not shrunk to nothing', () => {
    // Every assertion below iterates the corpus, so an empty one passes them all.
    expect(corpus.repo.cases.length).toBeGreaterThanOrEqual(3);
    expect(corpus.reference.cases.length).toBeGreaterThanOrEqual(2);
    expect(corpus.urls.cases.length).toBeGreaterThanOrEqual(4);
  });

  it('still exercises the inputs that break', () => {
    // A name that prefixes another, a tag carrying both a dot and a hyphen, an all-zero digest a
    // truthiness check would read as absent, and a registry with a scheme — the airgap-mirror
    // case, and the one a hardcoded `http://` silently downgrades.
    for (const needle of ['nscheck-2', '0.2.0-rc1', '0'.repeat(64), 'https://']) {
      expect(raw, `the corpus no longer exercises ${needle}`).toContain(needle);
    }
  });
});

describe('bundleRepo', () => {
  it.each(corpus.repo.cases)('$why', (c) => {
    expect(bundleRepo(c.name)).toBe(c.expect);
  });
});

describe('the two urls', () => {
  it.each(corpus.urls.cases)('$why', (c) => {
    expect(bundleManifestUrl(c.registry, c.name, c.version)).toBe(c.manifest);
    expect(bundleBlobUrl(c.registry, c.name, c.sha)).toBe(c.blob);
  });

  // THE PAIRING INVARIANT. A url and the sha a Machine checks it against travel together through
  // resolveBundle, the stack args and MachineActor; `validateMachineActor` refuses a pair that
  // disagrees, and it can only do that because a blob url ends with its own digest.
  it.each(corpus.urls.cases)('$why — the blob url ends with the digest it serves', (c) => {
    expect(c.blob.endsWith(`sha256:${c.sha}`)).toBe(true);
  });
});

describe('the reference a mirror copies', () => {
  // Built here rather than derived, because this side has no reference builder: what is pinned is
  // that the repository component the CLI mints is the one this side reads from. A drift in the
  // prefix breaks both columns of the corpus at once, which is the point of having both.
  it.each(corpus.reference.cases)('$why', (c) => {
    expect(c.tagged).toBe(`${c.registry}/${bundleRepo(c.name)}:${c.version}`);
    expect(c.pinned).toBe(`${c.registry}/${bundleRepo(c.name)}@${c.digest}`);
  });

  it('every registry in the corpus is spelled host:port', () => {
    // The port is half of a reference. `REGISTRY_PORT` is this side's copy of
    // `cli/appliance/registry.DefaultPort`, and a corpus written against a different one would
    // pass every assertion above while describing a registry nobody serves.
    for (const c of corpus.reference.cases) {
      expect(c.registry).toMatch(/^[a-z0-9.-]+:\d+$/);
    }
    expect(corpus.urls.cases.some((c) => c.registry.endsWith(`:${REGISTRY_PORT}`))).toBe(true);
  });
});
