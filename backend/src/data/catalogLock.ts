/**
 * ONE PROCESS MAY HOLD THE FILE CATALOG, AND THE SECOND ONE REFUSES TO BOOT (ADR 0031 §1b).
 *
 * A Postgres catalog is a SERVER: many processes attaching it is the feature, and the connstring
 * exists for exactly that. A file catalog is a DATABASE THIS PROCESS OPENS, and DuckDB is
 * single-writer over it — measured, two `node` processes attaching one `.ducklake` read-write:
 *
 *     IO Error: Failed to attach DuckLake MetaData … Could not set lock on file
 *     "…/datasets.ducklake": Conflicting lock is held in /usr/local/bin/node (PID 1016582)
 *
 * That message is correct and it arrives in the WRONG PLACE. Nothing attaches the catalog at
 * boot; the first attach is inside the materialization activity, forty minutes into a run,
 * where Temporal retries it — so the second control plane comes up, reports healthy on every
 * surface, serves the SPA, accepts dispatches, and fails only the one thing it was started to do.
 * That is the invisible-failure shape this product exists to remove, so the refusal is moved to
 * boot, before a connection and before a poll, beside `roles.ts:assertDistinctQueues`.
 *
 * THE LOCK IS ADVISORY AND SAYS SO. It is a file this process writes and other kontra processes
 * read; it does not stop `duckdb`, a stray script, or an operator with a shell. DuckDB's own lock
 * is the one that actually protects the bytes. This one exists to make the collision LEGIBLE at
 * the moment it can still be fixed, and to name the two ways out.
 */

import { openSync, closeSync, readFileSync, rmSync, writeSync } from 'node:fs';
import { hostname } from 'node:os';

import { catalogFilePath, ensureCatalogDir, resolveCatalog } from './parquet';

/** The suffix appended to the catalog path. Named in every refusal, so it is not a secret. */
export const LOCK_SUFFIX = '.lock';

/** What the lock file holds — enough to tell a live holder from a crashed one, and no more. */
interface LockRecord {
  pid: number;
  host: string;
  /** ISO-8601, for an operator reading the file rather than the error. */
  since: string;
}

/** The refusal. Its own type so a caller can tell it from a filesystem failure. */
export class CatalogLocked extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'CatalogLocked';
  }
}

/** A held lock. `release` is idempotent and never throws — an exit path must not fail. */
export interface CatalogLock {
  /** The lock file, absolute. */
  path: string;
  /** The catalog it guards. */
  catalog: string;
  release(): void;
}

function readHolder(path: string): LockRecord | null {
  try {
    const raw = JSON.parse(readFileSync(path, 'utf8')) as Partial<LockRecord>;
    if (typeof raw.pid !== 'number' || !Number.isFinite(raw.pid)) return null;
    return { pid: raw.pid, host: String(raw.host ?? ''), since: String(raw.since ?? '') };
  } catch {
    // Unparseable or unreadable. Treated as a lock with NO KNOWN HOLDER rather than as an
    // absent one: a truncated file is what a kill -9 mid-write leaves, and stealing it because
    // it did not parse would be the corruption this guard is for.
    return null;
  }
}

/**
 * Is the recorded holder still running?
 *
 * `kill(pid, 0)` asks the kernel and answers three things: the process exists (no throw), it
 * does not (`ESRCH`), or it exists and belongs to someone else (`EPERM` — still running, so
 * still a holder). A pid from ANOTHER HOST cannot be asked at all, so it is treated as live: two
 * machines sharing one data directory over NFS is not a topology this supports, and guessing
 * "probably dead" there is how both of them open the catalog.
 */
function holderAlive(rec: LockRecord): boolean {
  if (rec.host && rec.host !== hostname()) return true;
  if (rec.pid === process.pid) return true;
  try {
    process.kill(rec.pid, 0);
    return true;
  } catch (err) {
    return (err as NodeJS.ErrnoException).code === 'EPERM';
  }
}

/**
 * Take the catalog lock, or refuse.
 *
 * Returns `null` — no lock, no refusal — when the catalog names a SERVER. That is not an
 * omission: a `postgres:…` connstring is the supported way to run the API and the materializer as
 * two processes (ADR 0031 §1b), and locking it would break the only deployment the setting
 * exists for.
 *
 * A STALE LOCK IS TAKEN, NOT HONOURED. The holder is asked of the kernel, so a control plane
 * killed with `kill -9` — or a laptop that lost power — does not need a manual `rm` before it can
 * start again. That is the difference between a lock and a tombstone.
 */
export function acquireCatalogLock(catalog: string = resolveCatalog()): CatalogLock | null {
  const file = catalogFilePath(catalog);
  if (file === null) return null;

  // THE DIRECTORY FIRST. On a fresh install nothing has created the data directory yet, so
  // `openSync(…, 'wx')` would answer ENOENT and the FIRST boot of a new appliance would die on the
  // guard that exists to protect the second one.
  ensureCatalogDir(catalog);

  const path = `${file}${LOCK_SUFFIX}`;
  const record: LockRecord = { pid: process.pid, host: hostname(), since: new Date().toISOString() };

  for (let attempt = 0; attempt < 2; attempt += 1) {
    let fd: number;
    try {
      // `wx` is the whole mutual exclusion: create-or-fail in one syscall, so two processes
      // racing here cannot both believe they won.
      fd = openSync(path, 'wx');
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== 'EEXIST') throw err;
      const holder = readHolder(path);
      if (holder && !holderAlive(holder) && attempt === 0) {
        // The holder is gone. Remove ITS file and try once more; a second EEXIST after this
        // means somebody else won the race, and that one is a live holder.
        rmSync(path, { force: true });
        continue;
      }
      throw new CatalogLocked(refusal(catalog, path, holder));
    }
    try {
      writeSync(fd, JSON.stringify(record));
    } finally {
      closeSync(fd);
    }
    let released = false;
    return {
      path,
      catalog,
      release() {
        if (released) return;
        released = true;
        try {
          // Only if it is still OURS. A stale-lock steal by a later process means this file
          // belongs to that one now, and removing it would unlock a live control plane.
          const holder = readHolder(path);
          if (holder && holder.pid === process.pid && holder.host === record.host) {
            rmSync(path, { force: true });
          }
        } catch {
          /* an exit path never fails on housekeeping */
        }
      },
    };
  }
  /* c8 ignore next -- the loop returns or throws on both attempts */
  throw new CatalogLocked(refusal(catalog, path, readHolder(path)));
}

/**
 * The message. It names the LOCK, the HOLDER and BOTH ways out, because "already locked" on its
 * own leaves an operator with a file they do not recognise and no idea whether deleting it is
 * safe.
 */
function refusal(catalog: string, path: string, holder: LockRecord | null): string {
  const who = holder
    ? `pid ${holder.pid}${holder.host ? ` on ${holder.host}` : ''}${holder.since ? `, since ${holder.since}` : ''}`
    : 'an unreadable lock record — a process killed mid-write leaves one, and it is NOT assumed dead';
  return (
    `the DuckLake catalog ${catalog} is already held: ${who} (lock file ${path}).\n` +
    '  A file catalog is single-writer — a second process on the same data directory does not ' +
    'share it, it corrupts it — so this one refuses to start rather than find out during a run.\n' +
    '  Either stop the control plane that holds it, or give this one a data directory of its own ' +
    '(KONTRA_DATA_DIR, or `kontra up --data-dir`).\n' +
    '  Two processes that must share ONE catalog need a server, which is what ' +
    'KONTRA_DUCKLAKE_CATALOG=postgres:… is for (ADR 0031 §1b).\n' +
    `  If nothing holds it, the holder crashed on another host or the record is unreadable: remove ${path}.`
  );
}
