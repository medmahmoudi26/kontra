/**
 * A registered folder — where an Actor's or a Workflow's code lives on THIS machine.
 *
 * THE NUCLEI MODEL, deliberately. Not a registry you push to and pull from: a directory you point
 * at. You write an actor wherever you write code, register that folder once, and the platform
 * remembers where it is and keeps saying so. There is no upload, no artifact, no digest game at
 * this layer — the code stays yours, in your checkout, and registration records a PATH.
 *
 * WHY THIS EXISTS AT ALL. `.kontra/workflows/` was the one directory `serve` could run a file from,
 * hard-coded, and it was the right call at the time: the route runs arbitrary Python, and "anywhere
 * on disk" is not an authority a web surface should hold. But it also meant your workflows had to
 * be moved into a directory you did not choose, away from the actors they drive and the checkout
 * they belong to — and an operator with actors in `examples/python/probe` and run code in a
 * repo somewhere had nowhere to put either.
 *
 * SO THE BOUNDARY BECOMES AN ALLOWLIST RATHER THAN A CONSTANT. Registration is the act that grants
 * it: `serve` may run code from a folder you explicitly registered, and from nowhere else. That is
 * a weaker claim than "one fixed directory" and a far stronger one than "any path in a request" —
 * and it is the same shape the confinement always had, with the operator naming the root instead of
 * the code naming it.
 *
 * DEFAULTING UNDER `~/.kontra/` keeps the zero-config path zero-config: register nothing and the
 * two conventional directories are already registered, which is what an installation that has only
 * ever run `kontra init` should see.
 */

import { createHash } from 'node:crypto';
import { existsSync, readFileSync, readdirSync, realpathSync, statSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';

/** What a registered folder holds. The two kinds are read differently and served differently. */
export type SourceKind = 'actor' | 'workflow';

/**
 * What a folder declares about itself — the manifest, parsed.
 *
 * OPEN-ENDED ON PURPOSE. The named fields are the ones registration and the surfaces read; the
 * index signature keeps everything else an author wrote, because `actor.json` is the SDKs' file and
 * this is not the place that gets to decide what may be in it. Whatever is here is stored verbatim
 * with the registration, so a schema an author declared is available to a caller that has never run
 * the code.
 */
export interface Manifest {
  name?: string;
  version?: string;
  /** The module `serve` runs — `workflow.py`, `actor.py`. Defaults to the kind's marker. */
  entry?: string;
  /** A workflow's `@workflow.defn` CLASS, which is what `kontra workflow start` takes. */
  workflow?: string;
  /** What a Method takes and emits, when the author declared it in the manifest rather than in
   *  code. Actors normally declare in code and a running worker registers the real thing. */
  operations?: unknown;
  [key: string]: unknown;
}

/** One registered folder, as the pages list it and the allowlist enforces it. */
export interface Source {
  /** Stable id, minted at registration. The path is the natural key but paths get re-registered. */
  id: string;
  kind: SourceKind;
  /** What this folder declares itself to be: an actor's name, or a workflow's. */
  name: string;
  /** Absolute, symlink-resolved. The thing the pages show as "where this lives". */
  path: string;
  /** The version its manifest declares. Workflows have one now too — see {@link MANIFEST}. */
  version: string;
  /** First paragraph of description.md, or ''. The long form is read on demand. */
  description: string;
  registeredAt: number;
  /**
   * What the folder held WHEN IT WAS REGISTERED: `sha256:<64 hex>` over its own files.
   *
   * THE ANSWER TO "IS THIS THE CODE I REGISTERED". A registration records a path, and a path is a
   * moving target — a `git pull`, an edit in the workbench, a colleague's rebase, and the folder is
   * different code under a name that says it is the same. An Actor already had this for its image
   * (ADR 0011's content-pinned digest) and it is what makes a rebuild detectable; a workflow had
   * nothing, so "this workflow" meant "whatever is in that directory right now".
   *
   * STORED, NEVER RE-DERIVED ON LISTING — which is the opposite of `name`/`version`/`description`
   * above, and the difference is the entire point. Those are re-read so the row is honest about the
   * folder as it is; this is kept so the row can also say what the folder WAS, and comparing the two
   * is what turns a silent divergence into a visible one.
   */
  digest?: string;
  /**
   * The folder's manifest at registration, verbatim. What it declared it was, versus what a running
   * worker later says about itself — two different authorities, and only the first exists before
   * anything is served.
   */
  manifest?: Manifest;
  /**
   * The Temporal Nexus endpoint this registration owns: `kontra-<name>-<version-dashed>`.
   *
   * REGISTRATION CREATES IT, not the worker. It used to be minted on worker boot
   * (`handler/nexus.go:ensureNexusEndpoint`), which made the route a caller dispatches through a
   * side effect of somebody having started a process — so a caller written against an Actor nobody
   * had run yet failed at dispatch on a name that did not exist, and thirty-one endpoints on this
   * cluster outlived every worker that made them, owned by nothing.
   */
  endpoint?: string;
  /**
   * The folder is not there any more — set on listing, never stored.
   *
   * A registered row outlives its directory (a `git checkout` that renames one, a mount that is not
   * up yet) and that is deliberate: the registration is still the operator's. But the row that came
   * back carried the name, version and path recorded at registration and NOTHING said they were
   * stale, so a folder that had been deleted listed exactly like one that was fine — and the first
   * thing to fail was `serve`, several clicks later. Only a registered row can carry this: a
   * discovered one is re-read from the default root and simply stops being listed.
   */
  absent?: boolean;
}

export class SourceRefused extends Error {}

/**
 * THE ONE REFUSAL THAT IS NOT THE CALLER'S MISTAKE: the file is simply not there.
 *
 * Every other {@link SourceRefused} says the caller asked for something they may not have — an
 * absolute path, a name that escapes the folder, no name at all — and 400 is the right answer to
 * each. "This folder has no `description.md`" is a different fact and it is the ORDINARY one: a
 * description is optional, so an Actor written without one is not a malformed request, it is an
 * Actor. The route answers 404 to this and 400 to the rest.
 *
 * A SUBCLASS RATHER THAN A MESSAGE MATCH, and rather than a second `existsSync` in the route,
 * which would put the check on both sides of the seam. `AlreadyServing extends ControlRefused` on
 * the serve route is the same shape and the same ordering hazard — the narrow `instanceof` must be
 * tested first — which is why both are called out where they are caught.
 */
export class SourceMissing extends SourceRefused {}

function refuse(ok: boolean, message: string): asserts ok {
  if (!ok) throw new SourceRefused(message);
}

/** {@link refuse}, for the not-there case the route turns into a 404 rather than a 400. */
function refuseMissing(ok: boolean, message: string): asserts ok {
  if (!ok) throw new SourceMissing(message);
}

/**
 * `~/.kontra`, or wherever KONTRA_HOME points — this installation's own directory, resolved HERE
 * and nowhere else.
 *
 * THERE WERE TWO OF THESE AND THEY DISAGREED. This one answered `~/.kontra`;
 * `workflowControl.ts` had its own, answering `<cwd>/.kontra` — an approximation of the CLI's rule
 * (`cli/internal/config/config.go:kontraRoot`, which walks up to the checkout's `.kontra/`). Each default was
 * locally sensible: registration should default under the home the operator was told about and the
 * register form prefills, and `serve` should be confined to the checkout it was started from. Read
 * together they were a trap. With KONTRA_HOME unset — every test, and any run started from another
 * directory — registering a folder recorded a path under `~/.kontra` that `serve` then refused,
 * because `serve` was looking under `<cwd>/.kontra`. One variable, read twice, naming two
 * directories, and the registration that fails is the one that looked like it worked.
 *
 * `~/.kontra` is the survivor because it is the location the operator was told about. `<cwd>` lost
 * because it was never the checkout in the deployment that actually runs this code: the container's
 * cwd is `/app` and `/app/.kontra` does not exist. KONTRA_HOME is what makes it resolve there —
 * compose sets it to the mounted `.kontra/` explicitly — so the running control plane finds the
 * same directory before and after this collapse.
 */
export function kontraHome(): string {
  return process.env.KONTRA_HOME || path.join(os.homedir(), '.kontra');
}

/** The conventional folder for a kind: `~/.kontra/actors`, `~/.kontra/workflows`. */
export function defaultRoot(kind: SourceKind): string {
  return path.join(kontraHome(), kind === 'actor' ? 'actors' : 'workflows');
}

/**
 * The file that makes a folder that kind of folder.
 *
 * An actor is `actor.json` — the manifest both SDKs already read, so nothing new is invented.
 * A workflow is `workflow.py`, which is the one convention this adds, and it is what lets a
 * workflow be a FOLDER (with its description.md beside it) rather than a bare file.
 *
 * THE WORKFLOW MARKER IS STILL THE CODE, NOT THE MANIFEST, and that is deliberate even though
 * registration now requires `workflow.json` too (see {@link MANIFEST}). The two files answer
 * different questions: `workflow.py` is what makes the directory a workflow at all — it is what
 * `serve` runs and what discovery under the default root looks for — and `workflow.json` is what
 * makes it REGISTRABLE, because a registration has to carry a version and a digest and a folder
 * cannot invent either. A folder with code and no manifest is a workflow you have not registered
 * yet, which is a state worth being able to see and fix; a folder with a manifest and no code is
 * nothing at all.
 */
export const MARKER: Record<SourceKind, string> = {
  actor: 'actor.json',
  workflow: 'workflow.py',
};

/**
 * The file registration reads: what this folder DECLARES about itself.
 *
 * For an Actor it is the marker, unchanged — `actor.json` has always carried the name and version,
 * and both SDKs read it. For a Workflow it is new, and it exists because a registration has to
 * record things a directory listing cannot supply:
 *
 *   • a VERSION. An Actor has one and a workflow did not, so a workflow could not be pinned, could
 *     not be compared against what ran last week, and could not appear beside an Actor in any
 *     inventory that sorted by version.
 *   • the WORKFLOW TYPE — the `@workflow.defn` class name. `kontra workflow start` takes it, and
 *     until now an operator had to open the file and read it, or guess it from the folder name and
 *     find out they were wrong when the start call hung on a type nobody registered.
 *   • the ENTRY, so a folder is free to hold more than one module.
 */
export const MANIFEST: Record<SourceKind, string> = {
  actor: 'actor.json',
  workflow: 'workflow.json',
};

/** The markdown file whose first paragraph becomes the description, for either kind. */
export const DESCRIPTION_FILE = 'description.md';

/**
 * Read a folder and say what it is, or refuse with the reason.
 *
 * REFUSES RATHER THAN GUESSES. A folder with no marker is not "an empty actor" — it is a path
 * somebody typed wrong, and registering it would put a row in the list that never resolves to
 * anything. The message names the file that was looked for, because the fix is to add it.
 */
export function inspectFolder(
  kind: SourceKind,
  dir: string,
  /**
   * Refuse a folder with no manifest.
   *
   * ONLY REGISTRATION DEMANDS ONE, and the split matters. `discover` reads the default root looking
   * for folders that LOOK like workflows, and `list` re-reads rows that are already registered;
   * neither may refuse over a missing `workflow.json`, because doing so would make every existing
   * workflow on every installation disappear from the page that offers the button to fix it — and
   * an operator whose two workflows vanished after an upgrade has no way to learn why.
   *
   * Registering is the act that records a version, a digest and an endpoint, and none of those can
   * be invented from a directory listing. That is where the file becomes required.
   */
  { requireManifest = false }: { requireManifest?: boolean } = {}
): Omit<Source, 'id' | 'registeredAt'> {
  refuse(typeof dir === 'string' && dir.trim() !== '', 'a path is required');
  const expanded = expandHome(dir.trim());
  refuse(path.isAbsolute(expanded), `${dir}: register an absolute path (or one starting with ~)`);
  refuse(existsSync(expanded), `${expanded}: no such directory`);

  // Resolved once, here, because this is what gets STORED and every later containment check
  // compares against it. A symlinked registration that resolved differently later would be a
  // folder that passes the allowlist and serves something else.
  const real = realpathSync(expanded);
  refuse(statSync(real).isDirectory(), `${real} is a file — register the folder that contains it`);

  const marker = path.join(real, MARKER[kind]);
  refuse(
    existsSync(marker),
    `${real} has no ${MARKER[kind]} — that is what makes a folder ${kind === 'actor' ? 'an Actor' : 'a Workflow'}`
  );

  const described = describe(real);

  /* THE MANIFEST IS SEPARATE FROM THE MARKER, and for a workflow it is a second file. `workflow.py`
     makes the directory a workflow; `workflow.json` makes it registrable, because a registration
     records a version and a workflow type and neither can be read off a directory listing. The
     refusal names the file AND shows what belongs in it — a message that says "add workflow.json"
     and stops has moved the problem rather than solved it, and this is the one refusal every
     existing workflow folder on every installation will hit exactly once. */
  const manifestFile = path.join(real, MANIFEST[kind]);
  const hasManifest = existsSync(manifestFile);
  if (!hasManifest && requireManifest) {
    const guess = path.basename(real);
    throw new SourceRefused(
      `${real} has no ${MANIFEST[kind]} — registering records a version and a digest, and a directory declares neither.\n` +
        `Write one beside ${MARKER[kind]}:\n` +
        (kind === 'workflow'
          ? `  {"name": "${guess}", "version": "0.1.0", "entry": "${MARKER[kind]}", "workflow": "<the @workflow.defn class>"}\n` +
            `or let the CLI write it for you: kontra workflow register ${real} --init`
          : `  {"name": "${guess}", "version": "0.1.0"}`)
    );
  }

  // NOT REGISTERED YET IS NOT MALFORMED. A folder found under the default root, or one registered
  // before manifests existed, has code and no declaration — it lists, it can be opened, and
  // registering it is the act that asks for the file.
  const manifest = hasManifest ? readManifest(manifestFile) : undefined;
  return {
    kind,
    // The manifest's name, then the FOLDER's. A workflow's name was always its directory's and can
    // stay so; naming it in the manifest is what lets a folder be called something else on disk.
    name: strField(manifest?.name) || path.basename(real),
    path: real,
    version: strField(manifest?.version) || '',
    ...(manifest ? { manifest } : {}),
    ...described,
  };
}

/** A manifest field that has to be a string to be worth anything. A number version (`0.1`) or a
 *  nested object is an author error, and taking it would put `[object Object]` in an endpoint name. */
function strField(v: unknown): string {
  return typeof v === 'string' ? v.trim() : '';
}

/** actor.json / workflow.json, parsed. A malformed one is refused by name. */
export function readManifest(file: string): Manifest {
  let raw: string;
  try {
    raw = readFileSync(file, 'utf8');
  } catch (err) {
    throw new SourceRefused(`${file} could not be read: ${(err as Error).message}`);
  }
  try {
    const parsed = JSON.parse(raw) as Manifest;
    // `typeof null === 'object'`, and an ARRAY is an object too — `[1,2]` would sail through a bare
    // typeof check and then answer `undefined` to every field, producing a registration named after
    // its directory with no version and no error.
    refuse(
      parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed),
      `${file} is not a JSON object`
    );
    return parsed;
  } catch (err) {
    if (err instanceof SourceRefused) throw err;
    throw new SourceRefused(`${file} is not valid JSON: ${(err as Error).message}`);
  }
}

/**
 * Files that are not the folder's code, and must not change its digest.
 *
 * `__pycache__` is the one that forces this to exist: Python writes it into whatever directory it
 * imports from, so merely SERVING a workflow changes the directory — and a digest that moved every
 * time you ran something would report drift on every run and mean nothing by the second one.
 * `.git` and `node_modules` are the same argument at a different scale.
 */
const NOT_CODE = new Set(['__pycache__', '.git', 'node_modules', '.venv', '.mypy_cache', '.pytest_cache', '.DS_Store']);

/**
 * `sha256:<hex>` over a folder's own files — what "this code" means when the code is a directory.
 *
 * THE PATH IS IN THE HASH, RELATIVE. Hashing contents alone would give the same digest to two
 * folders holding the same bytes under different filenames, and renaming `workflow.py` to
 * `main.py` is a change that breaks `serve` while leaving every byte in place.
 *
 * SORTED, so the digest is a property of the folder and not of the order the filesystem happened to
 * hand its entries back in — `readdirSync` order is not guaranteed across platforms, and a digest
 * that differed between two machines holding identical code would be worse than no digest.
 *
 * SIZE IS NOT IN IT and neither is mtime: a `git clone` rewrites every mtime, and a digest that
 * changed on clone would say the code changed when only the checkout did.
 */
export function folderDigest(dir: string): string {
  const hash = createHash('sha256');
  for (const rel of walk(dir, '')) {
    hash.update(rel);
    hash.update('\0');
    try {
      hash.update(readFileSync(path.join(dir, rel)));
    } catch {
      // A file that cannot be read (a permission, a race with a save) is recorded as absent rather
      // than skipped: skipping would make an unreadable file and a missing one hash the same.
      hash.update('\0unreadable');
    }
    hash.update('\0');
  }
  return `sha256:${hash.digest('hex')}`;
}

/** Every file under `dir`, as paths relative to it, sorted, skipping {@link NOT_CODE}. */
function walk(dir: string, prefix: string): string[] {
  let entries: Array<{ name: string; isDirectory(): boolean; isFile(): boolean }>;
  try {
    entries = readdirSync(path.join(dir, prefix), { withFileTypes: true });
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const entry of [...entries].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))) {
    if (NOT_CODE.has(entry.name)) continue;
    const rel = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.isDirectory()) out.push(...walk(dir, rel));
    else if (entry.isFile()) out.push(rel);
  }
  return out;
}

/** The description, from `description.md`. Absent is '', never a placeholder. */
function describe(dir: string): { description: string } {
  try {
    return { description: firstParagraph(readFileSync(path.join(dir, DESCRIPTION_FILE), 'utf8')) };
  } catch {
    return { description: '' };
  }
}

/**
 * The first paragraph of a markdown file, as a single line.
 *
 * The SAME rule the Python SDK applies to a Method's docstring (`internals/catalog.py`), so a
 * one-line summary means the same thing wherever an author writes one. Leading `# Heading` lines
 * are skipped: a description.md almost always opens with the name as a title, and echoing that
 * back as the description would say nothing the row does not already say.
 */
export function firstParagraph(markdown: string): string {
  const blocks = markdown.replace(/\r\n/g, '\n').split(/\n\s*\n/);
  for (const block of blocks) {
    const text = block
      .split('\n')
      .filter((line) => !/^\s*#{1,6}\s/.test(line))
      .join(' ')
      .trim();
    if (text) return text.replace(/\s+/g, ' ');
  }
  return '';
}

/** `~/x` → `/home/you/x`. Accepted because it is what an operator types and what the form suggests. */
export function expandHome(p: string): string {
  if (p === '~') return os.homedir();
  if (p.startsWith('~/')) return path.join(os.homedir(), p.slice(2));
  return p;
}

/**
 * Is `candidate` inside `root`? The containment check the allowlist is built from.
 *
 * `path.relative` rather than `startsWith`, because `/srv/kontra-evil` starts with `/srv/kontra`.
 * Both sides are expected to be real paths already — resolving here would hide the case where a
 * caller forgot to, which is the case that matters.
 */
export function contains(root: string, candidate: string): boolean {
  const rel = path.relative(root, candidate);
  return rel === '' || (!rel.startsWith('..') && !path.isAbsolute(rel));
}

/**
 * Resolve a path INSIDE a registered folder, or refuse — the editor's and serve's gate.
 *
 * The registration granted authority over one directory; this is what holds a later request to it.
 * Symlinks are resolved before the comparison for the reason the old confinement documented: a
 * file containing no `..` can still point at `/etc/shadow`, and the comparison that matters is
 * between real paths.
 */
export function resolveInside(source: Source, rel: string): string {
  refuse(typeof rel === 'string' && rel.trim() !== '', 'a file is required');
  refuse(!path.isAbsolute(rel), 'file must be a name inside the registered folder');
  const candidate = path.resolve(source.path, rel);
  refuseMissing(existsSync(candidate), `${rel}: no such file in ${source.path}`);
  const real = realpathSync(candidate);
  refuse(
    contains(source.path, real),
    `${rel} resolves outside ${source.path} — refusing to read it`
  );
  return real;
}

/** What a file inside a registered folder must be named, for {@link filesIn} to list it. No
 *  separators at all, so a row it returns is always a name `resolveInside` can open. */
const FILE_RE = /^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/;

/** One file inside a registered folder, as the viewer lists it. */
export interface SourceFile {
  name: string;
  bytes: number;
  modifiedAt: number;
}

/* NOTHING HERE WRITES INTO A REGISTERED FOLDER ANY MORE (ADR 0033 §6, ADR 0030).
   `writeInside` lived here and had exactly one caller left: the Actors page writing a generated
   caller into a Workflow folder. ADR 0033 removed that errand — the page calls the Method now and
   shows the caller read-only beside the button — and ADR 0030 had already made both editors
   read-only viewers, so no surface in this repo writes source to disk. The route
   (`PUT /api/sources/:kind/:id/file`) went with it; an unused route is removed outright rather than
   left as a surface with no caller. Restoring either means restoring the authority it carried:
   writing into a folder `serve` executes from is the same authority as serving. */

/** What is in a registered folder, for the viewer's file picker. Flat: one level, no recursion. */
export function filesIn(source: Source): SourceFile[] {
  let names: string[];
  try {
    names = readdirSync(source.path);
  } catch {
    return [];
  }
  const out: SourceFile[] = [];
  for (const name of names) {
    if (!FILE_RE.test(name)) continue;
    try {
      const st = statSync(path.join(source.path, name));
      if (st.isFile()) out.push({ name, bytes: st.size, modifiedAt: st.mtimeMs });
    } catch {
      /* vanished between readdir and stat */
    }
  }
  return out.sort((a, b) => a.name.localeCompare(b.name));
}

/**
 * Every folder under a root that looks like one of these — what makes the default roots work
 * without anybody registering anything.
 *
 * A missing root is an empty list. An installation that has never run `kontra init` is a
 * legitimate state, and the page should say "nothing registered yet" rather than show a failure
 * about a directory the operator never asked for.
 */
export function discover(kind: SourceKind, root: string): Omit<Source, 'id' | 'registeredAt'>[] {
  let entries: string[];
  try {
    entries = readdirSync(root);
  } catch {
    return [];
  }
  const found: Omit<Source, 'id' | 'registeredAt'>[] = [];
  for (const entry of entries) {
    try {
      found.push(inspectFolder(kind, path.join(root, entry)));
    } catch {
      /* not one of these — a stray file, a __pycache__, a folder mid-write */
    }
  }
  return found.sort((a, b) => a.name.localeCompare(b.name));
}
