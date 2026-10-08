/**
 * WHERE THIS INSTALLATION KEEPS THE STATE IT OWNS — resolved here, in Node, to the identical path
 * `cli/datadir.go:installDataDir` resolves in Go.
 *
 * The binary and the orchestrator are one installation with one directory, and the orchestrator
 * writes the DuckLake catalog under it. A second location for the same installation is a second
 * thing to find, a second thing to back up, and a second thing to get wrong — which is the mistake
 * `sources.ts:kontraHome` already records having made once, with two functions reading one variable
 * and naming two directories.
 *
 * `KONTRA_HOME`, NOT A SIBLING OF THE CHECKOUT: KONTRA_HOME is already the answer to "where does
 * this installation keep its things" — `config.yaml`, `workflows/` and `actors/` are there.
 * `KONTRA_DATA_DIR` is what `docker-compose.yml` sets to the container path of the volume that
 * survives a recreate.
 *
 * KONTRA_DATA_DIR IS THE FIRST RULE ON BOTH SIDES, AND THAT IS LOAD-BEARING RATHER THAN TIDY. The
 * FALLBACKS do not agree and never have: `cli/internal/config/config.go:kontraRoot` prefers a
 * checkout's `.kontra/config.yaml` over `~/.kontra`, while `sources.ts:kontraHome` — which is what
 * this builds on — knows only `KONTRA_HOME || ~/.kontra`, and says in its own header that it is an
 * approximation of the CLI's rule kept deliberately. So a binary started inside a checkout with no
 * KONTRA_HOME would name `<checkout>/.kontra/data` while this names `~/.kontra/data`: two
 * processes, one installation, two lakes, and the second one empty. Compose sets the variable, and
 * setting it is what keeps that true.
 */

import path from 'node:path';

import { kontraHome } from '../sources';

/** The variable that overrides it — set by compose, and by `--data-dir`. */
export const DATA_DIR_VAR = 'KONTRA_DATA_DIR';

/**
 * The directory this installation's own state lives in.
 *
 * NOT CREATED HERE. Resolving a path and creating a directory are different acts, and a
 * `resolve…` that makes a directory as a side effect turns every test that merely reads the
 * default into one that writes to the operator's home. The writers create their own parent (see
 * `parquet.ts:ensureCatalogFile`).
 */
export function installDataDir(): string {
  const named = process.env[DATA_DIR_VAR];
  if (named && named.trim() !== '') return path.resolve(named.trim());
  return path.join(kontraHome(), 'data');
}
