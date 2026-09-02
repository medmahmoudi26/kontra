/**
 * No tracked source file may carry a raw control byte.
 *
 * WHY THIS IS A TEST AND NOT A STYLE RULE: `grep` without `-a` treats a file containing a NUL as
 * binary, finds nothing in it, and exits 1 — silently. Four files in this repo carried one, among
 * them the largest file in the console at 1,967 lines and the SSH transport. Every "no caller
 * exists", "this export is unused" and "nothing references this" conclusion drawn with plain text
 * search was unsound against them, and this repo makes those conclusions constantly — a whole
 * deletion sweep is built on them.
 *
 * THE BYTES DID NOT CHANGE AND MUST NOT. `\0` in a JavaScript string literal IS U+0000; the three
 * composite keys and the one sha256 input hash and compare exactly as before. What changed is that
 * the file is text to the tools that read it. So this test is about the SOURCE ENCODING, never
 * about the values — a separator may still be a control character, it may just not be typed as a
 * raw byte.
 *
 * `git ls-files` rather than a directory walk: what matters is what a reader greps, which is the
 * tracked tree, and it excludes `node_modules` and every build output for free.
 */
import { execFileSync } from 'node:child_process';
import { readFileSync, statSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const REPO = path.resolve(__dirname, '..', '..', '..');

/** Binary by intent — fonts, images, archives, recorded fixtures. Extension-based because these are
 *  files nobody greps for source, and a content sniff would just re-derive the extension. */
const BINARY = new Set([
  '.png', '.jpg', '.jpeg', '.gif', '.ico', '.webp', '.svg-bin',
  '.woff', '.woff2', '.ttf', '.otf', '.eot',
  '.gz', '.zip', '.tar', '.parquet', '.pdf', '.wasm', '.node', '.so', '.dylib', '.db',
]);

/** Everything a text file may hold below 0x20: tab, newline, carriage return. Nothing else. */
function controlBytes(buf: Buffer): number[] {
  const found = new Set<number>();
  for (const b of buf) {
    if (b < 0x20 && b !== 0x09 && b !== 0x0a && b !== 0x0d) found.add(b);
  }
  return [...found].sort((a, b) => a - b);
}

describe('the tracked tree is text, so text search is sound', () => {
  it('has no source file carrying a raw control byte', () => {
    const tracked = execFileSync('git', ['ls-files', '-z'], { cwd: REPO, maxBuffer: 64 * 1024 * 1024 })
      .toString('utf8')
      .split('\0')
      .filter(Boolean);
    // A guard on the guard: a listing this small means `git ls-files` did not do what we think,
    // and an empty sweep would pass while checking nothing.
    expect(tracked.length).toBeGreaterThan(500);

    const offenders: string[] = [];
    for (const rel of tracked) {
      if (BINARY.has(path.extname(rel).toLowerCase())) continue;
      const abs = path.join(REPO, rel);
      let buf: Buffer;
      try {
        if (!statSync(abs).isFile()) continue;
        buf = readFileSync(abs);
      } catch {
        continue; // a submodule or a path removed between the listing and the read
      }
      const bad = controlBytes(buf);
      if (bad.length > 0) offenders.push(`${rel} carries ${bad.map((b) => `0x${b.toString(16)}`).join(', ')}`);
    }

    // Named rather than counted: the fix is per-file (write the escape, not the byte), so the
    // failure has to say which file and which byte.
    expect(offenders).toEqual([]);
  });
});
