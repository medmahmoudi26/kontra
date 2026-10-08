/**
 * What each layer of an actor image IS — runtime, dependencies, or the app.
 *
 * THE JOIN IS BY INDEX, AND THAT IS NOT A SHORTCUT. A buildpack image carries its own account of
 * itself in the `io.buildpacks.lifecycle.metadata` label, and every digest in that account is an
 * UNCOMPRESSED `diff_id` — while a manifest's layers are COMPRESSED blob digests. They are different
 * digest spaces. Measured on a real `pack` build:
 *
 *     metadata shas also in rootfs.diff_ids:        3 of 3
 *     metadata shas also in MANIFEST layer digests: 0 of 3
 *
 * So matching the metadata's `sha` against a manifest layer's digest finds nothing, every layer reads
 * as `unknown`, and the page renders a plausible stack of unlabelled bars. The only thing that relates
 * the two is position: `config.rootfs.diff_ids[i]` and `manifest.layers[i]` describe the same layer.
 *
 * NO LAYER TARBALL IS EVER READ. Everything here comes from a manifest and a config blob — the blob
 * being where labels actually live, since zot's GraphQL `Labels` field returns an empty string even
 * for an image carrying ten of them (measured).
 */

/** What a layer is for. `unknown` is honest: an image not built by buildpacks has no account of itself. */
export type LayerKind = 'runtime' | 'deps' | 'app' | 'unknown';

export interface LayerFact {
  digest: string;
  size: number;
  kind: LayerKind;
  /** Where it came from: `heroku/python:venv`, `runtime python-browser:1`, `app`. */
  source: string;
}

/** The parts of `io.buildpacks.lifecycle.metadata` this reads. Everything else is ignored. */
export interface LifecycleMetadata {
  app?: Array<{ sha?: string }>;
  buildpacks?: Array<{ key?: string; layers?: Record<string, { sha?: string }> }>;
  runImage?: { topLayer?: string; reference?: string };
}

/** One manifest layer, as the registry reports it. */
export interface ManifestLayer {
  digest: string;
  size: number;
}

/**
 * Read the lifecycle label out of an image config blob, or `null` for an image that carries none.
 *
 * A malformed label is `null` rather than a throw: it is a string written by another process, and an
 * image whose metadata cannot be parsed is an image whose layers are `unknown` — not a 500 on a page
 * that is mostly about other images.
 */
export function lifecycleMetadata(configBlob: unknown): LifecycleMetadata | null {
  const labels = (configBlob as { config?: { Labels?: Record<string, string> } } | null)?.config?.Labels;
  const raw = labels?.['io.buildpacks.lifecycle.metadata'];
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as LifecycleMetadata;
    return parsed && typeof parsed === 'object' ? parsed : null;
  } catch {
    return null;
  }
}

/**
 * Classify every layer of one image.
 *
 * `diffIds` and `layers` are parallel and ordered bottom-up; a mismatch in length means the config and
 * the manifest disagree about the image, and nothing is classified rather than classified wrongly —
 * a wrong kind here is a wrong answer to "why is the registry 40 GB".
 */
export function classifyLayers(
  layers: readonly ManifestLayer[],
  diffIds: readonly string[],
  md: LifecycleMetadata | null,
  runtimeLabel?: string
): LayerFact[] {
  const plain = (): LayerFact[] =>
    layers.map((l) => ({ digest: l.digest, size: l.size, kind: 'unknown' as const, source: '' }));

  if (!md || layers.length !== diffIds.length) return plain();

  // diff_id -> what it is, built from the image's own account of itself.
  const byDiffId = new Map<string, { kind: LayerKind; source: string }>();
  for (const bp of md.buildpacks ?? []) {
    for (const [name, layer] of Object.entries(bp.layers ?? {})) {
      if (layer?.sha) byDiffId.set(layer.sha, { kind: 'deps', source: `${bp.key ?? 'buildpack'}:${name}` });
    }
  }
  for (const a of md.app ?? []) {
    if (a?.sha) byDiffId.set(a.sha, { kind: 'app', source: 'app' });
  }

  // THE RUN IMAGE IS EVERYTHING UP TO AND INCLUDING ITS TOP LAYER, which is the one fact the metadata
  // states positionally rather than per-layer: `runImage.topLayer` names the highest layer that came
  // from the runtime, and buildpack layers are stacked above it. Marking only that one layer as
  // `runtime` would leave the OS beneath it unattributed — and the OS is the bulk of the bytes.
  const topIndex = md.runImage?.topLayer ? diffIds.indexOf(md.runImage.topLayer) : -1;
  const runtimeSource = runtimeLabel ? `runtime ${runtimeLabel}` : 'runtime';

  return layers.map((l, i) => {
    const known = byDiffId.get(diffIds[i] as string);
    if (known) return { digest: l.digest, size: l.size, ...known };
    if (topIndex >= 0 && i <= topIndex) {
      return { digest: l.digest, size: l.size, kind: 'runtime' as const, source: runtimeSource };
    }
    // Above the run image and claimed by no buildpack: the lifecycle's own layers (launcher, config,
    // process-types, the SBOM). They are `deps` in the sense that matters here — not the app, not the
    // OS — and naming the lifecycle is more useful than calling them unknown.
    return { digest: l.digest, size: l.size, kind: 'deps' as const, source: 'buildpacksio/lifecycle' };
  });
}

/**
 * `size_unique` — the bytes that would actually be reclaimed by deleting this image.
 *
 * A layer shared with another image frees nothing when this one goes, so the honest number is the sum
 * of the layers nothing else references. `shared` is the set of digests known to be referenced
 * elsewhere, which the caller gets from the registry rather than guessing.
 */
export function uniqueBytes(layers: readonly LayerFact[], shared: ReadonlySet<string>): number {
  return layers.reduce((n, l) => (shared.has(l.digest) ? n : n + l.size), 0);
}

export function totalBytes(layers: readonly LayerFact[]): number {
  return layers.reduce((n, l) => n + l.size, 0);
}
