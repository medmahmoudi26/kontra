/**
 * The Actors and Workflows in the workspace, as a list the surfaces read and the allowlist enforces.
 *
 * ── ONE WAY IN: BE IN THE WORKSPACE ─────────────────────────────────────────────────────────────
 *
 * A folder is here because it sits under the workspace root and looks like its kind — `actor.json`
 * for an Actor, `workflow.py` for a Workflow. That is the whole rule. There is no `register`, no
 * stored path, and nothing to forget.
 *
 * WHAT THE REGISTRY COST, measured on a live install: four registered folders pointing at three
 * different checkouts, one of them (`crawl`) naming a directory deleted weeks earlier. It listed,
 * it rendered, it offered a Run button, and pressing anything on it produced
 * `404 — crawl: no such workflow`. A recorded path outlives what it names; a directory listing
 * cannot.
 *
 * THE LISTING IS THE FILESYSTEM. Adding a folder adds it, deleting it removes it, and `git
 * checkout` of a branch without it is not a state anybody has to repair. The `absent` flag, the
 * `forget` verb and the re-register-to-update dance all go with it.
 *
 * WHAT IS LOST, honestly: code outside the workspace is not reachable any more. That was the
 * registry's whole purpose — point at a checkout wherever it lives — and it is what the mount
 * replaces: the workspace is a volume, so "wherever your code lives" is expressed once, in the
 * compose file, instead of once per folder in a database.
 */

import { codeRoot, discover, type Source, type SourceKind } from './sources';

export class SourceStore {
  /**
   * Every folder of this kind, read from disk on every call.
   *
   * NOT CACHED, DELIBERATELY. A listing that cached would be wrong the moment somebody saved a file
   * — and "the console shows my new actor without a restart" is the property this whole change is
   * for. A directory read of a workspace is cheap; the digest, which is not, stays out of it.
   */
  list(kind: SourceKind): Source[] {
    return discover(kind, codeRoot(kind))
      .map((found) => ({ ...found, id: `at:${found.path}`, registeredAt: 0 }))
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** One folder by id, or undefined. What every path-taking route resolves through. */
  get(kind: SourceKind, id: string): Source | undefined {
    return this.list(kind).find((s) => s.id === id);
  }
}
