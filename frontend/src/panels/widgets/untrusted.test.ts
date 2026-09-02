/**
 * The untrusted-content boundary, pinned against the source itself (ADR 0020, slice 7b).
 *
 * Modelled on `backend/src/panels/readonly.test.ts`, and for the same reason: the invariants below
 * are not things a type can express, and each of them is one import away from being undone.
 *
 *   `rehype-raw`              turns every string in this directory into markup
 *   `dangerouslySetInnerHTML` / `innerHTML`   does the same, one component at a time
 *   `fetch` / `XMLHttpRequest`               makes `__FILE__:` an arbitrary read on the Controller,
 *                                            and any remote URL in pane text a beacon
 *
 * A grep is blunt on purpose. The next person to reach for `rehype-raw` because a summary needs a
 * `<details>` element should have to delete a test that explains why they must not.
 */

import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import * as path from 'node:path';
import { describe, expect, it } from 'vitest';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const PANELS = path.dirname(HERE);

/** Sources OUTSIDE this directory that also feed the widget path, named by hand because no walk
 *  would find them. Every entry is a filename nothing type-checks — see `sources()`. */
const HAND_ADDED = ['DetailDrawer.tsx'] as const;

/** Every widget source, plus the generator that feeds them. Not a fixed list of files: a new file
 * in this directory is covered the moment it exists.
 *
 * IT WAS THREE GENERATORS AND IS NOW ONE. `topology.ts` and `summaries.ts` were named here by hand
 * and were deleted as unreachable — nothing imported either from `main.tsx` or from `.ds-entry.ts`,
 * and their only importers were their own tests. This test is the one place in the tree that
 * mentioned them, which is worth recording rather than tidying away: it was guarding markdown
 * generators that nothing rendered, so it was the LAST reference and not a live caller.
 *
 * The two hand-added names are also why the deletion could not be proved safe from the compiler.
 * A path in a string is invisible to `tsc` and to an import-graph walk; this file failed at
 * `readFileSync` on the first run of the suite afterwards, which is the whole argument for running
 * it. Anything added back here is a filename nothing type-checks — keep the list short. */
function sources(): Array<{ file: string; source: string }> {
  const out: Array<{ file: string; source: string }> = [];
  const add = (file: string): void => {
    out.push({ file, source: readFileSync(file, 'utf8') });
  };
  for (const entry of readdirSync(HERE).sort()) {
    if (entry.endsWith('.test.ts') || entry.endsWith('.test.tsx')) continue;
    if (entry.endsWith('.ts') || entry.endsWith('.tsx')) add(path.join(HERE, entry));
  }
  for (const name of HAND_ADDED) add(path.join(PANELS, name));
  return out;
}

/** Code only. A comment is allowed — and required — to name what is forbidden. */
function codeOf(source: string): string {
  return source
    .split('\n')
    .filter((l) => {
      const t = l.trim();
      return !t.startsWith('*') && !t.startsWith('//') && !t.startsWith('/*');
    })
    .join('\n');
}

describe('nothing in the widget path can render markup or make a request', () => {
  const files = sources();

  it('reads every widget source, so a new file cannot slip past this test', () => {
    const names = files.map((f) => path.basename(f.file));
    expect(names).toContain('Markdown.tsx');
    expect(names).toContain('MermaidBlock.tsx');
    expect(names).toContain('parseWidget.ts');
    expect(names).toContain('WidgetView.tsx');
    expect(names).toContain('DetailDrawer.tsx');

    // EVERY DIRECTORY ENTRY, PLUS THE HAND-ADDED ONES — derived, not a number typed in.
    //
    // This was `toBeGreaterThan(8)`, calibrated when `sources()` returned ten files, and it went
    // red the moment two of them were deleted — reporting a count where the fact is "did the walk
    // cover the directory". A floor cannot tell a directory that shrank from a walk that silently
    // returned nothing, which is the failure the assertion is FOR: `readdirSync` on the wrong path
    // yields `[]`, every loop below passes vacuously, and the boundary is unguarded and green.
    const onDisk = readdirSync(HERE).filter(
      (e) => /\.tsx?$/.test(e) && !/\.test\.tsx?$/.test(e)
    );
    expect(onDisk.length).toBeGreaterThan(0);
    expect(files.length).toBe(onDisk.length + HAND_ADDED.length);
  });

  it('never enables raw HTML', () => {
    for (const { file, source } of files) {
      const code = codeOf(source);
      for (const needle of ['rehype-raw', 'rehypeRaw', 'dangerouslySetInnerHTML', 'innerHTML', 'outerHTML']) {
        expect(code, `${path.basename(file)} must not use ${needle}`).not.toContain(needle);
      }
    }
  });

  it('never fetches anything — `__FILE__:` is a label, not a path to read', () => {
    for (const { file, source } of files) {
      const code = codeOf(source);
      for (const needle of ['fetch(', 'XMLHttpRequest', 'EventSource', 'new WebSocket', 'import(']) {
        // `import(` is here for the same reason as the rest: the ONE dynamic import in this directory
        // is mermaid's, and it lives in `MermaidBlock.tsx`, which is exempted below by name.
        if (needle === 'import(' && path.basename(file) === 'MermaidBlock.tsx') continue;
        expect(code, `${path.basename(file)} must not use ${needle}`).not.toContain(needle);
      }
    }
  });

  it('imports mermaid ONLY through the lazy import, so the Dashboard chunk does not carry it', () => {
    for (const { file, source } of files) {
      if (path.basename(file) === 'MermaidBlock.tsx') continue;
      expect(codeOf(source), `${path.basename(file)} must not import mermaid`).not.toMatch(
        /from ['"]mermaid['"]/
      );
    }
    const block = files.find((f) => path.basename(f.file) === 'MermaidBlock.tsx');
    expect(block).toBeDefined();
    // A STATIC import would put ~500 KB of renderer in the Dashboard's entry chunk.
    expect(codeOf(block?.source ?? '')).not.toMatch(/^import .*from ['"]mermaid['"]/m);
    expect(codeOf(block?.source ?? '')).toContain("import('mermaid')");
  });
});
