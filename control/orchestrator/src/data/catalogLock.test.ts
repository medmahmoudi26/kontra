/**
 * The single-writer refusal (ADR 0031 §1b).
 *
 * These tests are about the BOOT guard, not about DuckDB: DuckDB's own file lock is what actually
 * protects the catalog, and it is proven in `parquet.test.ts`. What is proven here is that the
 * second control plane learns about the collision at boot, in a sentence that names the lock and
 * both ways out — rather than forty minutes later, from an activity retrying forever.
 */

import { existsSync, mkdtempSync, readFileSync, statSync, utimesSync, writeFileSync } from 'node:fs';
import { hostname, tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it, vi } from 'vitest';

import { CatalogLocked, HEARTBEAT_MS, LOCK_SUFFIX, acquireCatalogLock } from './catalogLock';

function dir(): string {
  return mkdtempSync(join(tmpdir(), 'kontra-lock-'));
}

const held: Array<{ release(): void }> = [];
function take(catalog?: string): ReturnType<typeof acquireCatalogLock> {
  const lock = acquireCatalogLock(catalog);
  if (lock) held.push(lock);
  return lock;
}

afterEach(() => {
  while (held.length) held.pop()!.release();
  delete process.env.KONTRA_DUCKLAKE_CATALOG;
  delete process.env.KONTRA_DATA_DIR;
});

describe('acquireCatalogLock', () => {
  it('takes the lock beside the catalog and records who holds it', () => {
    const catalog = join(dir(), 'datasets.ducklake');
    const lock = take(catalog);
    expect(lock).not.toBeNull();
    expect(lock!.path).toBe(`${catalog}${LOCK_SUFFIX}`);
    const rec = JSON.parse(readFileSync(lock!.path, 'utf8')) as { pid: number; host: string };
    expect(rec.pid).toBe(process.pid);
    expect(rec.host).toBe(hostname());
  });

  /**
   * THE ACCEPTANCE CRITERION. A second start on the same data directory refuses, and the message
   * names the lock — because "already locked" without a path leaves an operator with a file they
   * do not recognise and no idea whether removing it is safe.
   */
  it('REFUSES a second start on the same catalog, naming the lock and the holder', () => {
    const catalog = join(dir(), 'datasets.ducklake');
    const first = take(catalog);

    let err: unknown;
    try {
      acquireCatalogLock(catalog);
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(CatalogLocked);
    const msg = (err as Error).message;
    expect(msg).toContain(first!.path); // the lock, by name
    expect(msg).toContain(catalog); // the catalog it guards
    expect(msg).toContain(`pid ${process.pid}`); // who holds it
    expect(msg).toContain('KONTRA_DATA_DIR'); // way out 1: a directory of its own
    expect(msg).toContain('KONTRA_DUCKLAKE_CATALOG=postgres:'); // way out 2: a shared server
  });

  /**
   * A SERVER CATALOG TAKES NO LOCK, and that is the whole reason the setting stays alive. Two
   * processes sharing one Postgres catalog is the deployment `KONTRA_DUCKLAKE_CATALOG` exists
   * for; a guard that refused it would break the only case it is for.
   */
  it('is a no-op for a connstring catalog — more than one process is the point there', () => {
    const dsn = 'postgres:dbname=kontra_ducklake host=db port=5432 user=kontra';
    expect(acquireCatalogLock(dsn)).toBeNull();
    expect(acquireCatalogLock(dsn)).toBeNull(); // and again: nothing to collide with
  });

  /**
   * A FRESH INSTALL HAS NO DATA DIRECTORY AT ALL, and the boot guard runs before anything creates
   * one. Without this the very first `kontra up` on a new machine would die with `ENOENT: … open
   * '<data-dir>/datasets.ducklake.lock'` — the guard failing on the case it is not even about.
   */
  it('creates the data directory rather than failing the FIRST boot', () => {
    const catalog = join(dir(), 'nested', 'deeper', 'datasets.ducklake');
    const lock = take(catalog);
    expect(lock).not.toBeNull();
    expect(existsSync(lock!.path)).toBe(true);
  });

  it('releases, so a clean restart takes it again', () => {
    const catalog = join(dir(), 'datasets.ducklake');
    const lock = acquireCatalogLock(catalog)!;
    lock.release();
    expect(existsSync(lock.path)).toBe(false);
    expect(() => take(catalog)).not.toThrow();
  });

  it('releases at most once, so an exit path may call it twice', () => {
    const catalog = join(dir(), 'datasets.ducklake');
    const lock = acquireCatalogLock(catalog)!;
    lock.release();
    const other = take(catalog); // a different "process" now owns the file
    lock.release(); // must NOT remove the new holder's lock
    expect(existsSync(other!.path)).toBe(true);
  });

  /**
   * A LOCK IS NOT A TOMBSTONE. A control plane killed with `kill -9` leaves its record behind,
   * and requiring a manual `rm` before the next start would make every crash a two-step recovery.
   * The holder is asked of the KERNEL, so this is a fact rather than a timeout.
   */
  it('takes over a lock whose holder is gone', () => {
    const catalog = join(dir(), 'datasets.ducklake');
    const path = `${catalog}${LOCK_SUFFIX}`;
    // pid 2^22 is above every Linux default `pid_max`, so it cannot be a live process.
    writeFileSync(
      path,
      JSON.stringify({ pid: 4_194_304, host: hostname(), since: new Date().toISOString() })
    );
    const lock = take(catalog);
    expect(lock).not.toBeNull();
    expect((JSON.parse(readFileSync(path, 'utf8')) as { pid: number }).pid).toBe(process.pid);
  });

  /**
   * AND AN UNREADABLE ONE IS NOT ASSUMED DEAD. A truncated record is exactly what a kill mid-write
   * leaves, and "it did not parse, so I took it" is how two processes open one catalog.
   */
  it('refuses on a lock record it cannot read, rather than stealing it', () => {
    const catalog = join(dir(), 'datasets.ducklake');
    writeFileSync(`${catalog}${LOCK_SUFFIX}`, '{"pid":');
    expect(() => acquireCatalogLock(catalog)).toThrow(CatalogLocked);
  });

  /**
   * A PID FROM ANOTHER HOST CANNOT BE ASKED, so the HEARTBEAT answers instead. A file touched
   * moments ago is held by something that is running, wherever it is running.
   */
  it('treats a holder on another host that is still beating as live', () => {
    const catalog = join(dir(), 'datasets.ducklake');
    writeFileSync(
      `${catalog}${LOCK_SUFFIX}`,
      JSON.stringify({ pid: 4_194_304, host: 'some-other-box', since: '' })
    );
    expect(() => acquireCatalogLock(catalog)).toThrow(/some-other-box/);
  });

  /**
   * THE BUG THIS FIXES, AND IT BRICKED THE DOCKER INSTALL.
   *
   * A container's hostname is its id, so `docker compose up -d --force-recreate` — the ordinary
   * upgrade — starts a new hostname on the same volume. The old rule answered "another host,
   * therefore live" and the install refused to boot, forever, with `restart: unless-stopped`
   * retrying the refusal. The way out was to `rm` a file inside a named volume belonging to a
   * container that was restarting and so could not be `exec`'d into.
   *
   * The stale mtime here is what a container that is GONE leaves: nothing is touching the file.
   */
  it('takes over a lock from another host that stopped beating', () => {
    const catalog = join(dir(), 'datasets.ducklake');
    const path = `${catalog}${LOCK_SUFFIX}`;
    writeFileSync(
      path,
      // A plausible record: a real pid, a container id for a hostname — the shape the recreate left.
      JSON.stringify({ pid: 20, host: '752f94518051', since: new Date().toISOString() })
    );
    // Older than STALE_AFTER_MS. Set rather than waited for: a test that sleeps 45 seconds is a
    // test somebody deletes.
    const long_ago = new Date(Date.now() - 10 * 60 * 1000);
    utimesSync(path, long_ago, long_ago);

    const lock = take(catalog);
    expect(lock).not.toBeNull();
    expect((JSON.parse(readFileSync(path, 'utf8')) as { pid: number }).pid).toBe(process.pid);
  });

  /**
   * AND THE HOLDER KEEPS ITS OWN LOCK FRESH, which is what makes the rule above safe. Without this
   * every lock goes stale on its own after {@link STALE_AFTER_MS} and a live control plane can be
   * displaced by any process that waits long enough.
   *
   * ASSERTED BY ADVANCING THE CLOCK, not by waiting: the timer is what is under test, so it is the
   * timer that gets faked.
   */
  it('touches its own lock file on an interval, so a live holder never goes stale', () => {
    vi.useFakeTimers();
    try {
      const catalog = join(dir(), 'datasets.ducklake');
      const path = `${catalog}${LOCK_SUFFIX}`;
      const lock = take(catalog);
      expect(lock).not.toBeNull();

      // Backdate it to well past stale, then let one beat fire.
      const long_ago = new Date(Date.now() - 10 * 60 * 1000);
      utimesSync(path, long_ago, long_ago);
      const before = statSync(path).mtimeMs;

      vi.advanceTimersByTime(HEARTBEAT_MS + 1);
      expect(statSync(path).mtimeMs).toBeGreaterThan(before);

      // AND RELEASING STOPS IT. A timer left running after release touches a path that now belongs
      // to somebody else — which would keep a stranger's dead lock looking alive.
      lock!.release();
      const gone = existsSync(path);
      vi.advanceTimersByTime(HEARTBEAT_MS * 3);
      expect(gone).toBe(false);
      expect(existsSync(path)).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  /**
   * THE DEFAULT IS THE CATALOG THIS PROCESS WOULD ACTUALLY ATTACH. A guard that resolved the name
   * itself would agree with `resolveLakeConfig` until one of the two was edited, and then it would
   * pass while guarding a different file.
   */
  it('defaults to the catalog resolveLakeConfig resolves', () => {
    const d = dir();
    process.env.KONTRA_DATA_DIR = d;
    expect(take()?.path).toBe(join(d, `datasets.ducklake${LOCK_SUFFIX}`));
  });

  it('follows KONTRA_DUCKLAKE_CATALOG when one is set', () => {
    const catalog = join(dir(), 'elsewhere.ducklake');
    process.env.KONTRA_DUCKLAKE_CATALOG = catalog;
    expect(take()?.catalog).toBe(catalog);
  });
});
