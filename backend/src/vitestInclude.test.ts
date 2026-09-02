/**
 * Every `include` glob in `vitest.config.ts` matches at least one file.
 *
 * WHY THIS EXISTS. The suite reaches OUTSIDE this package for one entry —
 * `../shared/core/src/**\/*.test.ts`, the shared kernel's own tests — and when `core/` moved under
 * `shared/` that glob kept its old prefix. vitest does not warn about a pattern that matches
 * nothing: the run went from 109 files to 105 and reported success, so four test files stopped
 * running and the only visible trace was a number nobody was comparing.
 *
 * That is the same shape as `tests/test_conformance_tree.py` and `tests/test_workflow_paths.py`
 * guard for, in the one place neither of them looks. A relative glob is an ACCUMULATED path: every
 * fragment reads correctly on its own, and a sweep for the old directory name does not match it.
 *
 * IT CHECKS THE LITERAL PREFIX, not the whole pattern, because that is the part that rots. A glob
 * is wrong in exactly two ways — the directory it starts from stopped existing, or it exists and
 * holds no tests — and both are asserted below. What it deliberately does NOT do is re-implement
 * glob matching to count exact files; that would be a second implementation of vitest's own
 * behaviour, and this only has to catch a path that stopped resolving.
 */
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const CONFIG = path.join(__dirname, '..', 'vitest.config.ts');

/** The `include: [...]` entries, read out of the config as source. */
function includeGlobs(): string[] {
  const src = readFileSync(CONFIG, 'utf8');
  const at = src.indexOf('include:');
  expect(at, 'no `include:` in vitest.config.ts — this guard is reading the wrong file').toBeGreaterThan(-1);
  const list = src.slice(src.indexOf('[', at), src.indexOf(']', at) + 1);
  return [...list.matchAll(/'([^']+)'/g)].map((m) => m[1]!);
}

/** The part of a glob before the first wildcard — the directory it starts from. */
function literalPrefix(glob: string): string {
  const star = glob.indexOf('*');
  const head = star === -1 ? glob : glob.slice(0, star);
  return path.resolve(path.join(__dirname, '..'), path.dirname(head + 'x'));
}

function testFilesUnder(dir: string): number {
  let found = 0;
  for (const entry of readdirSync(dir)) {
    const full = path.join(dir, entry);
    if (statSync(full).isDirectory()) {
      if (entry === 'node_modules' || entry === 'dist') continue;
      found += testFilesUnder(full);
    } else if (entry.endsWith('.test.ts')) {
      found += 1;
    }
  }
  return found;
}

describe('the suite collects everything it says it does', () => {
  const globs = includeGlobs();

  it('reads the include list, so a config it cannot parse is a failure and not a pass', () => {
    expect(globs.length).toBeGreaterThan(0);
    expect(globs.some((g) => g.startsWith('../'))).toBe(true);
  });

  it.each(includeGlobs())('%s resolves to a directory that holds tests', (glob) => {
    const dir = literalPrefix(glob);
    expect(existsSync(dir), `${glob} starts from ${dir}, which does not exist`).toBe(true);
    expect(
      testFilesUnder(dir),
      `${glob} starts from ${dir}, which holds no *.test.ts — this glob is collecting nothing`
    ).toBeGreaterThan(0);
  });
});
