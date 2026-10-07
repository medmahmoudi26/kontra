/**
 * `inuse-` tags: the thing that makes registry retention safe to enforce.
 *
 * zot's retention keeps the five most recently pushed tags per repository. That rule alone will
 * delete a sixth-oldest version that a Fleet is still running, so the policy also keeps every tag
 * matching `^inuse-` and something has to write those tags. This is that something.
 *
 * WHY A TAG AND NOT A LIST SOMEWHERE. Retention is evaluated by zot, inside zot, with no callback
 * and no way to ask us a question. The only vocabulary we share with it is the tag namespace, so
 * "do not delete this" has to be spelled as a tag on the manifest it protects.
 *
 * WHAT IS IN USE, AND WHAT THIS DOES NOT YET KNOW. The spec names three sources: the catalog's
 * current digest, anything placed on a Machine, and anything a run still inside its retention window
 * references. Only the FIRST is implemented, and the reason is that the other two are not readable:
 * placements live in Pulumi stack state rather than a table, and for an enrolled Fleet the
 * assignments are operator-authored files that no code path writes.
 *
 * That limit is narrower than it sounds, because a placement resolves its digest FROM the catalog —
 * so the catalog is the source, and the dangerous case is a catalog entry whose tag has since moved.
 * That case is real and was measured: migrating this install found `desync@177f80c8` and
 * `webcrawl@e09d6df4` recorded in the catalog, present in the store, and reachable by no tag at all.
 * Those are exactly the digests this protects.
 */

import type { ActorRecord } from '../db/repo';

/** The width the console, the spec and this module all use. Changing it orphans every existing tag. */
const DIGEST_CHARS = 12;

/** `^inuse-` is the pattern the retention policy keeps; this is the writer's half of it. */
export const INUSE_PREFIX = 'inuse-';

export interface InuseDeps {
  /** Every actor the catalog knows, with whatever digest it recorded. */
  listActors(): ActorRecord[];
  /** Tags currently in a repository. Absent repository resolves to an empty list, never a throw. */
  listTags(repo: string): Promise<string[]>;
  /** Point `tag` at `digest` in `repo`, by re-putting the manifest under the new name. */
  tag(repo: string, digest: string, tag: string): Promise<void>;
  /** Remove a tag without removing the manifest it named. */
  untag(repo: string, tag: string): Promise<void>;
  onNote?: (note: string) => void;
  onError?: (err: unknown, where?: string) => void;
}

export interface InusePass {
  added: number;
  removed: number;
  kept: number;
  /** Removals the registry refused. Not a failure — see `reconcileInuseTags`. */
  refusedRemovals: number;
}

/**
 * The tag that protects one digest. Twelve hex characters, no `sha256:`.
 *
 * Returns `null` rather than a partial tag for anything that is not a sha256 digest: a tag built
 * from a truncated or empty digest would protect the wrong manifest or nothing at all, and both
 * read as success.
 */
export function inuseTagFor(digest: string): string | null {
  const hex = /^sha256:([0-9a-f]{64})$/.exec(digest)?.[1];
  if (!hex) return null;
  return INUSE_PREFIX + hex.slice(0, DIGEST_CHARS);
}

/**
 * Which repository holds an actor's images.
 *
 * TODAY IT IS THE BARE NAME. The spec's retention table says `actors/<name>`, and this install has
 * twelve bare repository names and nothing under `actors/` — renaming is a three-language change
 * that also re-aims the trust policy, so it belongs with the buildpack step. Both shapes are matched
 * by the retention policy; this is the one place that decides which one is WRITTEN.
 */
export function repoForActor(name: string): string {
  return name;
}

/**
 * The tags that should exist, per repository — a pure function of the catalog, so the decision is
 * testable without a registry.
 *
 * An actor with no digest contributes nothing. That is the normal state of a dev actor that was
 * never pushed (ADR 0011: `""` means unset), not an error.
 */
export function desiredInuseTags(actors: readonly ActorRecord[]): Map<string, Map<string, string>> {
  const byRepo = new Map<string, Map<string, string>>();
  for (const a of actors) {
    if (!a.digest) continue;
    const tag = inuseTagFor(a.digest);
    if (!tag) continue;
    const repo = repoForActor(a.name);
    let tags = byRepo.get(repo);
    if (!tags) {
      tags = new Map();
      byRepo.set(repo, tags);
    }
    // Two versions can record the same digest — a rebuild that changed nothing, or a retag. One tag
    // protects both, so last write wins and the count is of TAGS, not of catalog rows.
    tags.set(tag, a.digest);
  }
  return byRepo;
}

/**
 * One reconciliation pass: add the `inuse-` tags the catalog implies, remove the ones it no longer
 * does.
 *
 * REMOVAL IS BEST-EFFORT, AND THAT ASYMMETRY IS DELIBERATE. With no registry credential configured
 * the store permits read, create and update but NOT delete — deliberately, because zot answers an
 * unauthenticated manifest delete with 202 where `registry:2` refused it. So a stale `inuse-` tag may
 * outlive its reason, and the consequence is that retention keeps more than it strictly must. Keeping
 * too much is recoverable; deleting a running actor's image is not. A refused removal is counted and
 * noted, never thrown.
 */
export async function reconcileInuseTags(deps: InuseDeps): Promise<InusePass> {
  const want = desiredInuseTags(deps.listActors());
  const pass: InusePass = { added: 0, removed: 0, kept: 0, refusedRemovals: 0 };

  // Repositories that HAD inuse tags must be visited even when they want none now, or a tag whose
  // catalog row was deleted is never cleaned up.
  const repos = new Set(want.keys());
  for (const repo of repos) {
    let existing: string[];
    try {
      existing = await deps.listTags(repo);
    } catch (err) {
      deps.onError?.(err, `listing tags in ${repo}`);
      continue;
    }
    const have = new Set(existing.filter((t) => t.startsWith(INUSE_PREFIX)));
    const wanted = want.get(repo) ?? new Map<string, string>();

    for (const [tag, digest] of wanted) {
      if (have.has(tag)) {
        pass.kept++;
        continue;
      }
      try {
        await deps.tag(repo, digest, tag);
        pass.added++;
      } catch (err) {
        deps.onError?.(err, `tagging ${repo}@${digest.slice(7, 19)} as ${tag}`);
      }
    }

    for (const tag of have) {
      if (wanted.has(tag)) continue;
      try {
        await deps.untag(repo, tag);
        pass.removed++;
      } catch {
        pass.refusedRemovals++;
      }
    }
  }
  return pass;
}
