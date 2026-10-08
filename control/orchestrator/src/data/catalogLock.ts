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

import { closeSync, openSync, readFileSync, rmSync, statSync, utimesSync, writeSync } from 'node:fs';
import { hostname } from 'node:os';

import { catalogFilePath, ensureCatalogDir, resolveCatalog } from './parquet';

/** The suffix appended to the catalog path. Named in every refusal, so it is not a secret. */
export const LOCK_SUFFIX = '.lock';

/**
 * How often the holder touches its own lock file, and how long a lock may go untouched before
 * another process may take it.
 *
 * ── WHY A HEARTBEAT EXISTS AT ALL: THE CONTAINER THAT COULD NEVER START AGAIN ────────────────────
 *
 * `holderAlive` used to answer TRUE for any record written by a different hostname, on the
 * reasoning that a pid on another machine cannot be asked of this kernel. That reasoning is sound
 * for two machines over NFS and CATASTROPHIC in a container, because **a container's hostname is
 * its id and a recreated container gets a new one.** The volume is the same; the identity is not.
 *
 * MEASURED on the copy-paste docker install: `docker compose up -d --force-recreate`, the ordinary
 * shape of an upgrade, left a lock recorded by container `752f94518051` and started container
 * `78c90931e7aa` on the same volume. The new one refused — correctly, by its own rule — and
 * `restart: unless-stopped` restarted it to refuse again, forever. The way out was to `rm` a file
 * inside a named volume, in a container that is restarting and so cannot be `exec`'d into. An
 * unrecoverable brick, reached by typing the documented upgrade command.
 *
 * A heartbeat makes the question answerable without asking another kernel: a holder that is
 * running touches the file, and one that is gone stops. The hostname stops being the authority.
 *
 * ── AND THIS LOCK IS ALLOWED TO BE SLIGHTLY PERMISSIVE ───────────────────────────────────────────
 *
 * It is ADVISORY and the header says so: DuckDB's own file lock is what protects the bytes, and it
 * is kernel-enforced and released on death. This one exists to make the collision LEGIBLE at boot
 * instead of forty minutes into a run. Being wrong for {@link STALE_AFTER_MS} in the direction of
 * "take it" costs a clear error at the first attach; being wrong in the direction of "honour it"
 * cost the whole installation.
 */
export const HEARTBEAT_MS = 10_000;

/**
 * Four and a half missed beats. Long enough that a paused container, a stop-the-world GC or a
 * loaded host does not lose a lock it still holds; short enough that a recreated container
 * recovers on its own within a minute rather than needing a person.
 */
export const STALE_AFTER_MS = 45_000;

/**
 * Has the holder touched this file recently enough to still be running?
 *
 * A file whose mtime cannot be read is treated as NOT fresh — it was removed between the failed
 * open and this stat, which means there is no holder.
 *
 * A LOCK FROM THE FUTURE IS FRESH. Clock skew between two machines sharing a directory would
 * otherwise read as "stale by a lot" and hand the catalog to a second writer; `Math.abs` makes the
 * comparison about distance rather than direction.
 */
function heartbeatFresh(path: string, now: number = Date.now()): boolean {
  try {
    return Math.abs(now - statSync(path).mtimeMs) < STALE_AFTER_MS;
  } catch {
    return false;
  }
}

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
 * TWO QUESTIONS, AND WHICH ONE APPLIES DEPENDS ON WHETHER THE KERNEL CAN BE ASKED.
 *
 * **Same host — ask the kernel.** `kill(pid, 0)` answers three things: the process exists (no
 * throw), it does not (`ESRCH`), or it exists and belongs to someone else (`EPERM` — still
 * running, so still a holder). That is exact and instant, so a control plane killed with `kill -9`
 * on this machine does not wait out a timeout before its successor can start.
 *
 * **Another host — read the heartbeat.** A pid on another machine cannot be asked of this kernel,
 * and this used to return TRUE for that case on the reasoning that guessing "probably dead" is how
 * two machines both open one catalog. In a container that reasoning inverts: the hostname is the
 * container id, so EVERY recreate looks like another machine and the lock is never reclaimable.
 * See {@link HEARTBEAT_MS} for the install this bricked. The heartbeat answers the same question
 * without guessing — a holder that is running touches its file, and one that is gone stops.
 *
 * A HOLDER WRITTEN BY A VERSION WITH NO HEARTBEAT, ON ANOTHER HOST, IS RECLAIMED AFTER
 * {@link STALE_AFTER_MS}. That is a real behaviour change and it is the right one: the topology it
 * affects — two machines sharing one data directory over a network filesystem — is stated in the
 * header as unsupported, and DuckDB's own lock still refuses the second writer.
 */
function holderAlive(rec: LockRecord, path: string, now: number = Date.now()): boolean {
  if (rec.host && rec.host !== hostname()) return heartbeatFresh(path, now);
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
  // `openSync(…, 'wx')` would answer ENOENT and the FIRST boot of a new install would die on the
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
      if (holder && !holderAlive(holder, path) && attempt === 0) {
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

    /* THE HEARTBEAT. Touching the file is the whole of it — the record's contents never change, so
       there is nothing to rewrite and nothing a reader can catch half-written. `utimesSync` on a
       path we hold is two syscalls every ten seconds.

       `unref()` IS LOAD-BEARING: without it this timer keeps the event loop alive and a process
       that has finished its work never exits, which turns a lock meant to protect shutdown into a
       reason the process will not shut down.

       A FAILED TOUCH IS SWALLOWED. The lock file can be removed out from under us by an operator
       following the refusal's own advice; throwing from a timer callback is an unhandled exception
       that takes down a control plane over housekeeping. The next acquirer sees no file and takes
       the lock, which is the same outcome the removal asked for. */
    const beat = setInterval(() => {
      try {
        const at = new Date();
        utimesSync(path, at, at);
      } catch {
        /* see above */
      }
    }, HEARTBEAT_MS);
    beat.unref();

    let released = false;
    return {
      path,
      catalog,
      release() {
        if (released) return;
        released = true;
        clearInterval(beat);
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
    '  A holder on ANOTHER HOST — which includes a container that was recreated, since its hostname ' +
    `is its id — reclaims itself: the lock is taken automatically once it has gone ${Math.round(STALE_AFTER_MS / 1000)}s ` +
    'without a heartbeat, so a restart loop here ends on its own rather than needing a person.\n' +
    `  If it does not, the record is unreadable and is not assumed dead: remove ${path}.`
  );
}
