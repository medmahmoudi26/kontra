/**
 * The registered folders, as a list the surfaces read and the allowlist enforces.
 *
 * TWO WAYS IN, ONE LIST OUT. A folder is here because somebody registered it, or because it sits
 * under a default root (`~/.kontra/actors`, `~/.kontra/workflows`) and looks like one. Discovery
 * keeps the zero-config path working — drop a folder in the conventional place and it is there,
 * which is the nuclei feel this is modelled on — while explicit registration is what lets code
 * live in your checkout beside the actors it drives.
 *
 * DISCOVERED ENTRIES ARE NOT WRITTEN DOWN. They are re-read on every listing, so deleting the
 * folder removes it and nothing has to be un-registered. A registered one IS written down, because
 * the path is the thing an operator chose and losing it means asking them again.
 */

import path from 'node:path';
import type { Repo } from './db/repo';
import {
  defaultRoot,
  discover,
  folderDigest,
  inspectFolder,
  SourceRefused,
  workspaceRoot,
  type Source,
  type SourceKind,
} from './sources';

export class SourceStore {
  constructor(private readonly repo: Repo) {}

  /**
   * Every folder of this kind: registered first, then discovered, deduped by path.
   *
   * REGISTERED WINS on a collision. Registering `~/.kontra/actors/probe` explicitly is a no-op in
   * terms of what is listed, but the row carries a registration time and an id that a discovered
   * one does not — and dropping the explicit row would make the register button look broken for
   * anyone whose code already lives in the conventional place.
   */
  list(kind: SourceKind): Source[] {
    const out: Source[] = [];
    const seen = new Set<string>();
    for (const row of this.repo.listSources(kind)) {
      // A registered folder can be deleted or renamed out from under us. Re-reading it on every
      // listing is what makes the row honest: name, version and description come from the folder as
      // it is now, and when there is no folder to read the row says `absent` instead of repeating
      // what was true at registration. Keeping the row is deliberate — `forget` is the operator's,
      // and a folder gone for a `git checkout` is not a registration to throw away — but an absent
      // one that listed like a healthy one sent them to `serve` to find out.
      // `kind` is re-stated from the argument rather than taken off the row: db/repo.ts keeps
      // SourceRecord.kind a plain string so the db layer stays standalone, and the rows came from
      // a query on this kind.
      try {
        /* THE STORED `digest` AND `endpoint` SURVIVE THE RE-READ, and that asymmetry is the point.
           `name`, `version`, `description` and `manifest` come from the folder as it is NOW —
           `inspectFolder` supplies all four, so the spread order puts the fresh ones on top. The
           digest is what the folder HELD when it was registered and the endpoint is cluster state
           neither of which a directory read can answer, so they stay as recorded.

           THE DIGEST IS NOT RECOMPUTED HERE. It reads every file in the folder, this runs on every
           page load, and a Go actor directory carries a compiled binary — a listing that hashed
           tens of megabytes per row to answer "has this changed" would make the Actors page slow in
           proportion to how much code you have. Drift is reported where the digest is computed
           anyway: `register` (see below), which is also the act that resolves it. */
        out.push({ ...row, ...inspectFolder(kind, row.path), kind });
      } catch {
        // The folder cannot be read — gone, renamed, or (now) a workflow with no `workflow.json`.
        // The row is kept and marked, because the registration is still the operator's to repair or
        // to drop, and it carries the name and version recorded at registration since there is
        // nothing on disk to re-read them from.
        out.push({ ...row, kind, description: '', absent: true });
      }
      seen.add(row.path);
    }
    for (const found of discover(kind, defaultRoot(kind))) {
      if (seen.has(found.path)) continue;
      seen.add(found.path);
      out.push({ ...found, id: `at:${found.path}`, registeredAt: 0 });
    }
    const workspace = workspaceRoot();
    if (workspace) {
      for (const found of discover(kind, workspace)) {
        if (seen.has(found.path)) continue;
        seen.add(found.path);
        out.push({ ...found, id: `at:${found.path}`, registeredAt: 0 });
      }
    }
    return out.sort((a, b) => a.name.localeCompare(b.name));
  }

  /** One folder by id, or undefined. What every path-taking route resolves through. */
  get(kind: SourceKind, id: string): Source | undefined {
    return this.list(kind).find((s) => s.id === id);
  }

  /**
   * Register a folder, or throw {@link SourceRefused} with the reason it is not one.
   *
   * REGISTERING IS ITS OWN ACT — not a side effect of serving, and not a prerequisite for it that
   * happens implicitly. What it records is everything a caller needs to know about code it has
   * never run: where it is, what it declares (`manifest`), what it held at the time (`digest`), and
   * the Nexus endpoint through which it is addressable.
   *
   * RE-REGISTERING IS NOT A NO-OP ANY MORE, and that is the change. It used to return the existing
   * row untouched, which was right when a registration was only a path — the path had not moved, so
   * there was nothing to write. Now the row also carries what the folder HELD, so returning the old
   * one after the code changed would keep asserting a digest that is no longer true. Re-registering
   * is how an operator says "this is the version I mean now", and it is the only way to move that
   * claim forward.
   *
   * THE ID SURVIVES. It is in URLs, in the workbench's open state, and in whatever the operator has
   * bookmarked; minting a new one because the code changed would break every one of those to record
   * a fact about the contents.
   */
  register(kind: SourceKind, dir: string): Source {
    const inspected = inspectFolder(kind, dir, { requireManifest: true });
    const already = this.repo.listSources(kind).find((row) => row.path === inspected.path);
    const source: Source = {
      ...inspected,
      id: already?.id ?? mintId(kind, inspected.path),
      registeredAt: already?.registeredAt ?? Date.now(),
      digest: folderDigest(inspected.path),
    };
    this.repo.saveSource(source);
    return source;
  }

  /**
   * Record the Nexus endpoint a registration owns.
   *
   * SEPARATE FROM `register` BECAUSE IT IS A NETWORK CALL and registering is not. `register` writes
   * a row from a filesystem read and cannot fail on a cluster being down; creating an endpoint can,
   * and a registration that rolled back because Temporal was unreachable would lose a fact about
   * THIS DISK over a fact about a server. So the folder is registered first, the endpoint is
   * attempted second, and a row with no `endpoint` is a real and repairable state rather than a
   * failed registration.
   */
  recordEndpoint(kind: SourceKind, id: string, endpoint: string): void {
    const row = this.repo.listSources(kind).find((s) => s.id === id);
    if (!row) return;
    this.repo.saveSource({ ...row, endpoint });
  }

  /**
   * Forget a registration. A DISCOVERED folder cannot be forgotten — it is not a registration,
   * it is what is in the conventional directory, and the way to remove it is to move the folder.
   */
  forget(id: string): boolean {
    if (id.startsWith('at:')) {
      throw new SourceRefused(
        'that folder is not registered — it is in the default directory. Move it to remove it.'
      );
    }
    return this.repo.deleteSource(id);
  }
}

/** `actor:probe:<hash>` — readable in a URL, and unique per path so re-registering is idempotent. */
function mintId(kind: SourceKind, dir: string): string {
  let h = 0;
  for (let i = 0; i < dir.length; i++) h = (Math.imul(31, h) + dir.charCodeAt(i)) | 0;
  return `${kind}:${path.basename(dir)}:${(h >>> 0).toString(36)}`;
}
