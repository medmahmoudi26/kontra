/**
 * Copy the conformance corpora into `dist/` so a consumer in ANOTHER REPOSITORY can execute them.
 *
 * WHY THIS EXISTS. `conformance/*.json` is the cross-language contract: Go, Python, the CLI and the
 * orchestrator each drive the same file, and ADR 0035's second rule is that a contract with two
 * writers gets a corpus rather than two string literals. kontra-console is one of those writers —
 * `src/panels/actorSession.test.ts` calls itself "the BROWSER ARM of conformance/queues.json
 * §tmux_session" — and after ADR 0038 it is no longer in this repository and cannot read the file.
 *
 * The alternative was for the console to keep its own copy of the corpus. That is the precise thing
 * a corpus is for preventing: a second copy of the answers is a second implementation, and the
 * first divergence would be invisible on both sides. So the corpus stays a single file here and
 * rides along inside the published package.
 *
 * IT IS COPIED, NOT DUPLICATED IN SOURCE — `core/conformance` does not exist in the tree, only in
 * `dist`, so there is nothing to keep in step by hand.
 */
import { cpSync, existsSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const src = join(here, '..', '..', 'conformance');
const dst = join(here, '..', 'dist', 'conformance');

// THE MARKER THAT MAKES THE ESM HALF ESM. The package has no top-level `"type"`, so Node reads
// every `.js` under it as CommonJS — including `dist/esm/`, whose files are `import`/`export` and
// would die on the first line. One `package.json` in that directory is the whole fix, and it has to
// be generated rather than committed: it lives inside a build output.
//
// `dist/cjs/` needs no marker; commonjs is already what the absence of `"type"` means.
writeFileSync(join(here, '..', 'dist', 'esm', 'package.json'), '{ "type": "module" }\n');

if (!existsSync(src)) {
  throw new Error(`conformance corpus not found at ${src} — @kontra/core cannot be built without it`);
}

cpSync(src, dst, { recursive: true });

// A COPY THAT COPIED NOTHING IS THE FAILURE THIS CHECKS FOR. `cpSync` of an empty or wrong
// directory succeeds silently, the package publishes without the corpora, and every consuming arm
// skips or fails far away from the cause. Assert the result, not the call.
const copied = readdirSync(dst).filter((f) => f.endsWith('.json'));
if (copied.length === 0) {
  throw new Error(`copied 0 corpora from ${src} — the package would publish without them`);
}
if (!copied.includes('queues.json')) {
  throw new Error(`queues.json missing from ${dst} — kontra-console's browser arm reads it by name`);
}
console.log(`conformance: ${copied.length} corpora -> dist/conformance`);
