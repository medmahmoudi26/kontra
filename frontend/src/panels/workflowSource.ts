/**
 * Reading a workflow file well enough to be useful — and no further.
 *
 * Two questions, both of which the Workflows page needs an answer to before it can prefill a
 * field or print a command an operator can paste:
 *
 *   1. What can I `kontra workflow start`?  → the `@workflow.defn` type names.
 *   2. What has to be up first?             → the Actors it dispatches to, the Datasets it reads
 *      and writes, and whether it provisions its own Machines with `fleet.up(...)`.
 *
 * WHY NOT A PYTHON PARSER. Because the answer does not have to be exact to be honest. This is a
 * text scan whose failure mode is a missing line in a printed checklist, which the operator reads;
 * a real parser in the browser (a WASM tree-sitter, say) would be megabytes to make the same list
 * slightly more complete. What it does NOT do is decide anything at runtime — nothing here
 * executes, dispatches or schedules, so being wrong costs a wrong hint, never a wrong run. The
 * limits are recorded on each function.
 *
 * THE DOCSTRING STRIP IS THE LOAD-BEARING PART, and it is why the naive version of this belongs in
 * the bin. `nscheck.py`'s module docstring contains the very commands this page generates —
 * `kontra workflow start nscheck` — plus an ASCII diagram naming its own methods. A
 * scan that does not blank comments and triple-quoted strings first reads documentation as code.
 */

/** A deployed thing a workflow drives, or a dataset it moves through. */
export interface WorkflowRef {
  kind: 'actor' | 'dataset' | 'fleet';
  name: string;
  /** As written in the source. Absent when the call omitted it. */
  version?: string;
  /**
   * This file WRITES the Dataset — either it opens a writer (`catalog.dataset(x).writer()`) or it
   * PROMOTES into it (`catalog.dataset(x).insert_from(tmp, where=...)`). Only ever set on a
   * `dataset` ref.
   *
   * PROMOTION IS A WRITE, and missing it under-reports what a workflow outputs to zero. Slice 02
   * rewrote `nscheck` to STAGE every verdict into a temporary Dataset and PROMOTE only the lame ones
   * into `lame` with `insert_from` after the fleet is gone (ADR temp-datasets §02). A workflow whose
   * ONLY output is promoted — no direct `.writer()` on the named Dataset at all — then showed no
   * output whatsoever, which is the one thing a run-output panel exists not to do.
   *
   * WHY IT IS WORTH SCANNING FOR. Reading and writing a Dataset are the same call up to the
   * `.writer()`, so a page that could not tell them apart summed a workflow's INPUT list into its
   * "units committed" — `nscheck` reads 400 domains and writes ~600 verdicts, and the header
   * announced 1,023 committed units one second after Run, before a single Machine existed. A number
   * that large and that wrong, on the stat an operator uses to decide whether a run is
   * working, is worse than no number.
   *
   * Absent means "not seen as a writer", which is not the same as "read-only" — a name built from a
   * variable is invisible to this scan either way. The run's own materialization ledger is the
   * authority once it has anything to say; this is what the page has before then.
   */
  writes?: boolean;
}

export interface ParsedWorkflow {
  /** Startable type names, in source order. The `name=` override wins over the class name. */
  defns: string[];
  /** Actors, datasets and fleets referenced, deduped, in source order. */
  refs: WorkflowRef[];
}

/**
 * Blank out `#` comments and triple-quoted strings, keeping single-line strings verbatim.
 *
 * Both halves are load-bearing. The module docstring of every example in this repo contains the
 * commands and the actor names the page is trying to discover, so a naive scan reads prose as
 * source. Single-line strings must survive because the actor and dataset names live *inside*
 * them.
 *
 * Newlines are preserved so line-oriented matching downstream sees the same shape.
 */
export function stripCommentsAndDocstrings(source: string): string {
  let out = '';
  let i = 0;
  const n = source.length;
  while (i < n) {
    const ch = source[i]!;
    const three = source.slice(i, i + 3);
    if (three === '"""' || three === "'''") {
      const end = source.indexOf(three, i + 3);
      const stop = end === -1 ? n : end + 3;
      // Replaced with spaces rather than deleted, so a `catalog.actor(` in prose cannot be
      // spliced onto the code that follows it.
      out += source.slice(i, stop).replace(/[^\n]/g, ' ');
      i = stop;
      continue;
    }
    if (ch === '"' || ch === "'") {
      let j = i + 1;
      while (j < n) {
        const c = source[j];
        if (c === '\\') {
          j += 2;
          continue;
        }
        if (c === ch || c === '\n') break;
        j += 1;
      }
      out += source.slice(i, Math.min(j + 1, n));
      i = j + 1;
      continue;
    }
    if (ch === '#') {
      const nl = source.indexOf('\n', i);
      i = nl === -1 ? n : nl;
      continue;
    }
    out += ch;
    i += 1;
  }
  return out;
}

/**
 * The `@workflow.defn` type names — the classes a worker registers, and what the page shows and
 * dispatches. (`kontra workflow start` takes the FOLDER now, deriving the queue from it — GitHub
 * #15 — but the type is still the class a Run executes, read from the file the page has open.)
 *
 * `@workflow.defn(name="Sweep2")` registers under that name, not the class's, and starting the
 * wrong one of the two is a workflow that never gets a task: Temporal accepts a start for any
 * type name, and only a worker that registered it picks the task up. Other decorators may sit
 * between the decorator and the `class`, so the match runs to the first `class` after it.
 *
 * LIMIT: it looks for the `workflow.defn` spelling. `from temporalio.workflow import defn` and
 * `@defn` is not found — the page then shows no startable type, which reads as "this file has no
 * workflow in it" rather than as a wrong name.
 */
export function workflowDefns(source: string): string[] {
  const clean = stripCommentsAndDocstrings(source);
  const pattern = /@workflow\.defn(?:\s*\(([^)]*)\))?[\s\S]*?\bclass\s+([A-Za-z_]\w*)/g;
  const names: string[] = [];
  for (const m of clean.matchAll(pattern)) {
    const override = m[1] ? /\bname\s*=\s*(["'])(.*?)\1/.exec(m[1]) : null;
    const name = override?.[2] || m[2];
    if (name && !names.includes(name)) names.push(name);
  }
  return names;
}

/**
 * The local name(s) each caller-side module is bound to in this file.
 *
 * `from actorkit import catalog, fleet` is the canonical spelling and what every example uses, but
 * an alias is legal Python and silently changes every call site. Returning the prefixes rather
 * than hard-coding one keeps the scan honest about the file in front of it.
 *
 * LIMIT: a prefix is required, so `from actorkit.catalog import actor` and a bare `actor("x")` is
 * not seen. Deliberate — a bare `actor(` is too common a name to attribute to this module without
 * a real parser, and a WRONG prerequisite is worse than a missing one in a checklist.
 */
export function moduleAliases(source: string, module: 'catalog' | 'fleet'): string[] {
  const clean = stripCommentsAndDocstrings(source);
  const aliases: string[] = [];
  for (const m of clean.matchAll(/^\s*from\s+actorkit\s+import\s+([^\n]+)/gm)) {
    for (const part of (m[1] ?? '').split(',')) {
      const named = new RegExp(`^\\s*${module}(?:\\s+as\\s+([A-Za-z_]\\w*))?\\s*$`).exec(part);
      if (named) aliases.push(named[1] ?? module);
    }
  }
  for (const m of clean.matchAll(
    new RegExp(`^\\s*import\\s+actorkit(?:\\.${module})?(?:\\s+as\\s+([A-Za-z_]\\w*))?`, 'gm')
  )) {
    aliases.push(m[1] ?? `actorkit.${module}`);
  }
  // No recognizable import: assume the canonical spelling rather than reporting no prerequisites
  // at all. A file with `catalog.actor(...)` in it references an actor whatever its imports look
  // like, and an over-listed prerequisite is a line an operator skips.
  return aliases.length > 0 ? aliases : [module];
}

/**
 * `name=`/`version=` kwargs if present, else the first two positional string literals.
 *
 * The `or` case is the one worth spelling out. Every example reads its input as
 * `catalog.dataset(req.get("dataset") or "domains")`, where the FIRST literal is the key in the
 * request and the second is the dataset — so the ordinary positional rule gets it exactly
 * backwards and puts `dataset` on the checklist as though it were a name. After an `or`, the
 * literal that follows is the default, and the default is what the file means when the caller
 * says nothing.
 *
 * Anything else containing a nested call returns no name at all. That is the rule this whole
 * module runs on: a missing prerequisite is a line an operator adds back, a wrong one sends them
 * looking for something that was never there.
 */
/**
 * Local names bound to a string, one hop.
 *
 * `out_name = req.get("into") or "lame"` … `catalog.dataset(out_name)` is the idiom every example
 * uses for its OUTPUT dataset, and without this the scan reads `out_name` as an unreadable
 * expression and lists nothing. MEASURED: the Workflows page watched a real fleet sweep write 623
 * rows into `lame` and never named it, because the only dataset it could see was the INPUT — which
 * is the one written as a literal.
 *
 * DELIBERATELY ONE HOP, and only from a literal or the `or`-default idiom. A name reassigned in a
 * loop, built by concatenation, or read from another variable is not resolved: this whole module's
 * rule is that a missing prerequisite is a line an operator adds back and a wrong one sends them
 * looking for something that was never there.
 */
export function stringBindings(source: string): Map<string, string> {
  const clean = stripCommentsAndDocstrings(source);
  const out = new Map<string, string>();
  const seen = new Set<string>();
  for (const m of clean.matchAll(/^[ \t]*([A-Za-z_]\w*)\s*=\s*([^\n]+)$/gm)) {
    const name = m[1]!;
    const rhs = (m[2] ?? '').trim();
    // Rebound later in the file: refuse it rather than picking one of the two.
    if (seen.has(name)) {
      out.delete(name);
      continue;
    }
    seen.add(name);
    const literal = /^(["'])(.*?)\1\s*$/.exec(rhs);
    if (literal) {
      out.set(name, literal[2]!);
      continue;
    }
    const orDefault = /\bor\s+(["'])(.*?)\1\s*$/.exec(rhs);
    if (orDefault) out.set(name, orDefault[2]!);
  }
  return out;
}

function callArgs(argText: string, bound: Map<string, string> = new Map()): { name?: string; version?: string } {
  const kwName = /\b(?:name|actor|dataset)\s*=\s*(["'])(.*?)\1/.exec(argText);
  const kwVersion = /\bversion\s*=\s*(["'])(.*?)\1/.exec(argText);
  if (kwName) return { name: kwName[2], version: kwVersion?.[2] };

  const orDefault = /\bor\s+(["'])(.*?)\1\s*$/.exec(argText.trim());
  if (orDefault) return { name: orDefault[2] };

  // A bare local name the file bound to a string earlier — `catalog.dataset(out_name)`.
  const bare = /^\s*([A-Za-z_]\w*)\s*$/.exec(argText);
  if (bare && bound.has(bare[1]!)) return { name: bound.get(bare[1]!) };

  if (argText.includes('(')) return {};

  // Drop every `kw=<string>` before reading positionals, so `tag="dns"` cannot be read as a
  // name.
  const positional = argText.replace(/\b\w+\s*=\s*(["'])(?:.*?)\1/g, '');
  const literals = [...positional.matchAll(/(["'])(.*?)\1/g)].map((m) => m[2]!);
  return { name: literals[0], version: literals[1] };
}

function escapeForRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * The Actors, Datasets and Fleets this file drives.
 *
 * `catalog.actor(...)` is an Actor — dispatched over Nexus through its handler — and nothing polls
 * its queue until it is served or placed. `catalog.dataset(...)` is a Dataset, read or written.
 * `fleet.up(actor=..., version=...)` means the file provisions the Machines that will run that
 * actor ITSELF, which is the opposite prerequisite: you do not need to start that actor first, you
 * need its Bundle published.
 *
 * LIMIT: only calls whose name is a literal are seen. A handle built from a variable
 * (`catalog.actor(cfg.name, cfg.version)`) is invisible here, and there is no way to know it
 * without running the file. `nscheck.py` reads its input as `catalog.dataset(req.get(...) or
 * "domains")` — the literal default IS found, which is the honest answer for a checklist.
 */
export function workflowRefs(source: string): WorkflowRef[] {
  const clean = stripCommentsAndDocstrings(source);
  const bound = stringBindings(source);
  const found: Array<{ at: number; ref: WorkflowRef }> = [];
  const seen = new Set<string>();

  const add = (
    at: number,
    kind: WorkflowRef['kind'],
    name?: string,
    version?: string,
    writes?: boolean
  ) => {
    if (!name) return;
    const key = `${kind}:${name}:${version ?? ''}`;
    // A name seen twice keeps the WRITER reading. `catalog.dataset(out).writer()` inside a loop
    // that also reads the same name elsewhere is still a Dataset this file writes, and the summary
    // that matters — is this an output — must not depend on which line came first.
    if (seen.has(key)) {
      if (writes) {
        const prior = found.find((f) => `${f.ref.kind}:${f.ref.name}:${f.ref.version ?? ''}` === key);
        if (prior) prior.ref.writes = true;
      }
      return;
    }
    seen.add(key);
    const ref: WorkflowRef = version ? { kind, name, version } : { kind, name };
    if (writes) ref.writes = true;
    found.push({ at, ref });
  };

  for (const alias of moduleAliases(source, 'catalog')) {
    // ONE level of nesting, because the argument routinely is a call:
    // `catalog.dataset(req.get("dataset") or "domains")`. Deeper than that is not worth matching
    // by regex, and `callArgs` refuses to name anything it cannot read plainly.
    const pattern = new RegExp(
      `(?:^|[^\\w.])${escapeForRegExp(alias)}\\.(actor|dataset)\\s*\\(((?:[^()]|\\([^()]*\\))*)\\)`,
      'g'
    );
    for (const m of clean.matchAll(pattern)) {
      const { name, version } = callArgs(m[2] ?? '', bound);
      // A WRITE is either `.writer()` (staging rows) or `.insert_from(` (promotion, ADR
      // temp-datasets §02) on the handle this call returns. `insert_from` is the whole reason a
      // promotion-only output — the shape `nscheck` now has — is not missed: its `lame` never sees a
      // direct `.writer()`, only the promote. Read from the text immediately after the closing paren
      // rather than from the whole line, so `catalog.dataset(a)` on a line that happens to mention
      // another handle's write is not mistaken for an output. `catalog.dataset.temp()` is NOT matched
      // by the pattern above (no `(` follows `dataset`), so a staging temp never reaches this list —
      // deliberate: a temp is an implementation detail of staging, and printing it beside the named
      // output it promotes into is noise on a checklist.
      const after = clean.slice((m.index ?? 0) + m[0].length, (m.index ?? 0) + m[0].length + 28);
      add(
        m.index ?? 0,
        m[1] === 'actor' ? 'actor' : 'dataset',
        name,
        version,
        m[1] === 'dataset' && /^\s*\.\s*(?:writer|insert_from)\s*\(/.test(after)
      );
    }
  }

  for (const alias of moduleAliases(source, 'fleet')) {
    // `fleet.up(...)` spans lines and carries nested calls (`machines=int(...)`), so the argument
    // text is taken to the end of the statement rather than by balanced parens — which this scan
    // cannot do anyway. Bounded to the call's own indentation block by stopping at `) as`.
    const pattern = new RegExp(`(?:^|[^\\w.])${escapeForRegExp(alias)}\\.up\\s*\\(([\\s\\S]*?)\\)\\s*as\\b`, 'g');
    for (const m of clean.matchAll(pattern)) {
      const argText = m[1] ?? '';
      const actor = /\bactor\s*=\s*(["'])(.*?)\1/.exec(argText);
      const version = /\bversion\s*=\s*(["'])(.*?)\1/.exec(argText);
      add(m.index ?? 0, 'fleet', actor?.[2], version?.[2]);
    }
  }

  // THE OTHER DOOR (ADR 0037). `fleet.hold(tag=…)` names no Actor — capacity and placement are two
  // decisions now — so what this file provisions is on its `place()` calls and not on the scope
  // line, and a scan that read only the `as` line would report a file that provisions nothing.
  //
  // THE SCOPE HANDLE IS WHAT BOUNDS IT, and it has to be something: `.place(` on any receiver would
  // make `board.place("e4", "queen")` a Fleet on somebody's checklist, and `moduleAliases` cannot
  // supply the bound because it falls back to the canonical spelling for a file with no recognisable
  // import. So the handle is read off the `as` of the hold that opened the scope, which is the same
  // `) as` bounding `up` already uses and is exact rather than heuristic.
  for (const alias of moduleAliases(source, 'fleet')) {
    const opens = new RegExp(
      `(?:^|[^\\w.])${escapeForRegExp(alias)}\\.hold\\s*\\(([\\s\\S]*?)\\)\\s*as\\s+([A-Za-z_]\\w*)`,
      'g'
    );
    for (const open of clean.matchAll(opens)) {
      const handle = open[2] ?? '';
      const places = new RegExp(`(?:^|[^\\w.])${escapeForRegExp(handle)}\\.place\\s*\\(([^()]*)\\)`, 'g');
      for (const m of clean.matchAll(places)) {
        const { name, version } = callArgs(m[1] ?? '', bound);
        add(m.index ?? 0, 'fleet', name, version);
      }
    }
  }

  // Source order, not alias order: the list is read as a checklist, so it should match the file.
  return found.sort((a, b) => a.at - b.at).map((f) => f.ref);
}

export function parseWorkflow(source: string): ParsedWorkflow {
  return { defns: workflowDefns(source), refs: workflowRefs(source) };
}

/**
 * The workflow TYPE to start — read from the source, and only guessed from the filename when the
 * source does not say.
 *
 * IT LIVES HERE, beside the scan it delegates to, rather than on whichever page happens to need it.
 * It moved when the Workflows surface came back and the Catalog stopped being an editor; a helper
 * that follows a page around is one that eventually gets duplicated.
 *
 * THE FILENAME GUESS IS WRONG FOR THE VERY FILE THIS REPO SHIPS WITH: `nscheck.py` yields `Nscheck`
 * and the class is `NsCheck`. That mistake does not fail — Temporal accepts a start for any type
 * name and the task simply sits on the queue, because no worker has that type registered. A run
 * that hangs with no error is the worst shape this value can take, and it is one capital letter
 * away at all times.
 */
export function typeFromSource(source: string, filename = ''): string {
  return workflowDefns(source)[0] ?? guessTypeFromFilename(filename);
}

/**
 * The content digest a worker registered a workflow under, read out of its derived queue.
 *
 * A served workflow's queue is `wf-<name>-<digest12>` — the 12-hex suffix is the first bytes of the
 * folder's content digest (`workflowControl.ts:workflowQueue`, GitHub #15), which is the identity of
 * the code that is ACTUALLY POLLING. The read-only source viewer (ADR 0030) shows the file on disk
 * beside this, so a folder edited in the operator's own editor but not re-served reads as a
 * disagreement between the two rather than as a silent overwrite of one by the other.
 *
 * The suffix is anchored `[0-9a-f]{12}$`, so a workflow whose NAME happens to end in `-<12 hex>` is
 * still read for the true trailing digest and not its name. Empty means no worker has registered
 * this workflow yet — a different thing from a digest, and the viewer says so.
 */
export function queueDigest(queue: string | undefined): string {
  const m = /-([0-9a-f]{12})$/.exec(queue ?? '');
  return m ? m[1]! : '';
}

/**
 * Python's indentation, for the editor that writes this file.
 *
 * FOUR SPACES, NEVER A TAB. This is the file the worker imports: a tab mixed into a
 * space-indented module is a TabError at import time, which presents as a worker that dies on boot
 * in a tmux window and says nothing about where it came from.
 *
 * It lived on the Catalog page while that page was the editor. The editor is the Workflows surface
 * now and the Catalog is gone; a constant about Python source belongs beside the module that reads
 * Python source.
 */
export const PY_INDENT = '    ';

/**
 * The workflow's ONE argument, from the text an operator typed.
 *
 * BLANK MEANS NO ARGUMENT — which is different from `null` and different from `{}`. A workflow
 * whose `run(self)` takes nothing must be startable, and `{}` would be an argument it does not
 * accept.
 */
export function parseInput(raw: string): { ok: boolean; value?: unknown; error?: string } {
  if (raw.trim() === '') return { ok: true };
  try {
    return { ok: true, value: JSON.parse(raw) };
  } catch (err) {
    return { ok: false, error: (err as Error).message };
  }
}

/** `nscheck.py` → `Nscheck`. The FALLBACK only — see {@link typeFromSource}. A guess the operator
 *  can correct beats an empty required field. */
export function guessTypeFromFilename(filename: string): string {
  return filename
    .replace(/\.py$/, '')
    .split(/[-_]/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join('');
}
