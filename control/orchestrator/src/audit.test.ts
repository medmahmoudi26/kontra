import { mkdtempSync, readFileSync, readdirSync, writeFileSync, utimesSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AuditLog, callerOf, retentionDays, type AuditEntry } from './audit';
import { sessions } from './auth/session';

function dir(): string {
  return mkdtempSync(path.join(tmpdir(), 'kontra-audit-'));
}

function entry(over: Partial<AuditEntry> = {}): AuditEntry {
  return {
    at: 1_789_865_677_000,
    action: 'run.start',
    outcome: 'allowed',
    who: 'mohamed',
    via: 'session',
    target: 'hunt-1789865677',
    ip: '10.0.0.4',
    machine: 'kontra-api',
    role: 'api',
    ...over,
  };
}

describe('the trail records what happened', () => {
  it('names the person, the action, the target and the outcome', () => {
    const log = new AuditLog({ dir: dir() });
    log.record(entry());
    const [got] = log.list();
    expect(got?.who).toBe('mohamed');
    expect(got?.action).toBe('run.start');
    expect(got?.target).toBe('hunt-1789865677');
    expect(got?.outcome).toBe('allowed');
  });

  it('records a refusal, which is the half most systems omit', () => {
    // An operator trying something they may not do is the interesting event. A trail of successes
    // only goes quiet exactly when something is happening.
    const log = new AuditLog({ dir: dir() });
    log.record(entry({ action: 'secret.bind', outcome: 'refused', who: 'anonymous', via: 'anonymous' }));
    expect(log.list({ outcome: 'refused' })).toHaveLength(1);
  });

  it('returns entries newest first', () => {
    const log = new AuditLog({ dir: dir() });
    log.record(entry({ at: 1000, target: 'oldest' }));
    log.record(entry({ at: 2000, target: 'newest' }));
    expect(log.list().map((e) => e.target)).toEqual(['newest', 'oldest']);
  });

  it('never throws out of record, because a failed trail must not fail the action', () => {
    // A directory that cannot exist: its parent is a FILE, so `mkdir -p` fails with ENOTDIR.
    //
    // NOT a path under `/proc`, which is what this test tried first: `mkdirSync` with
    // `recursive: true` there does not error, it BLOCKS — measured, and it hung the whole suite
    // before any test reported. A synthetic-filesystem path is a bad way to say "unwritable".
    const d = dir();
    const notADir = path.join(d, 'this-is-a-file');
    writeFileSync(notADir, 'x');
    const log = new AuditLog({ dir: path.join(notADir, 'nested') });
    expect(() => log.record(entry())).not.toThrow();
  });

  it('skips a torn line rather than losing the page', () => {
    const d = dir();
    const log = new AuditLog({ dir: d });
    log.record(entry({ target: 'before' }));
    writeFileSync(path.join(d, 'audit.log'), `${readFileSync(path.join(d, 'audit.log'), 'utf8')}{"half\n`);
    log.record(entry({ target: 'after' }));
    expect(log.list().map((e) => e.target)).toEqual(['after', 'before']);
  });
});

describe('filters', () => {
  const d = dir();
  const log = new AuditLog({ dir: d });
  log.record(entry({ who: 'mohamed', action: 'login', target: 'console', at: 10 }));
  log.record(entry({ who: 'ci', action: 'run.start', target: 'hunt-1', at: 20 }));
  log.record(entry({ who: 'mohamed', action: 'run.terminate', target: 'hunt-1', at: 30 }));

  it('narrows by who', () => {
    expect(log.list({ who: 'mohamed' })).toHaveLength(2);
  });

  it('narrows by action', () => {
    expect(log.list({ action: 'run.start' }).map((e) => e.who)).toEqual(['ci']);
  });

  it('narrows by target across actions, which is how a Run is followed', () => {
    expect(log.list({ target: 'hunt-1' }).map((e) => e.action)).toEqual(['run.terminate', 'run.start']);
  });

  it('narrows by time', () => {
    expect(log.list({ since: 20 })).toHaveLength(2);
  });
});

describe('rotation, which is not trimming', () => {
  it('moves old entries aside instead of dropping them', () => {
    // THE DISTINCTION THIS CLASS EXISTS FOR. `secrets/audit.ts` trims to the newest N, which is
    // right for a ledger an operator skims. Here it would mean anybody who can generate audit
    // events can evict the record of what they did by generating more of them.
    const d = dir();
    const log = new AuditLog({ dir: d });
    log.record(entry({ target: 'the-thing-to-hide' }));

    // Push the live file past the rotation bound, then write again.
    const file = path.join(d, 'audit.log');
    writeFileSync(file, `${readFileSync(file, 'utf8')}${'#'.repeat(9 * 1024 * 1024)}\n`);
    log.record(entry({ target: 'after-rotation' }));

    const rotated = readdirSync(d).filter((n) => /^audit-.+\.log$/.test(n));
    expect(rotated).toHaveLength(1);
    // Both are still readable: the old one through the rotated file, the new one through the live.
    expect(log.list().map((e) => e.target)).toContain('the-thing-to-hide');
    expect(log.list().map((e) => e.target)).toContain('after-rotation');
  });

  it('deletes a rotated file only when it is older than the retention window', () => {
    const d = dir();
    const log = new AuditLog({ dir: d });
    const stale = path.join(d, 'audit-2020-01-01T00-00-00-000Z.log');
    writeFileSync(stale, `${JSON.stringify(entry({ target: 'ancient' }))}\n`);
    const old = (Date.now() - (retentionDays() + 1) * 24 * 60 * 60 * 1000) / 1000;
    utimesSync(stale, old, old);

    const fresh = path.join(d, 'audit-2026-01-01T00-00-00-000Z.log');
    writeFileSync(fresh, `${JSON.stringify(entry({ target: 'recent' }))}\n`);

    log.record(entry({ target: 'now' }));

    const left = readdirSync(d);
    expect(left).not.toContain(path.basename(stale));
    expect(left).toContain(path.basename(fresh));
  });
});

describe('the record must not become the leak', () => {
  it('has no field a credential lands in, swept on the WRITTEN file', () => {
    // Swept on disk, after a real write, rather than on the object — the same bar
    // `slotStore.test.ts` holds its resolution ledger to. An object can be shaped correctly and
    // still be serialised alongside something that is not.
    const d = dir();
    const log = new AuditLog({ dir: d });
    const SENTINEL = 'do_v1_THISWOULDBEACREDENTIAL';
    log.record(
      entry({
        action: 'secret.bind',
        target: 'desync/shodan-key',
        // The NAME of a secret is what a binding line carries. Nothing on this path has a value,
        // and `detail` is the only free-text field — so that is where a leak would arrive.
        detail: 'secret shodan-key',
      })
    );
    const written = readFileSync(path.join(d, 'audit.log'), 'utf8');
    expect(written).not.toContain(SENTINEL);
    expect(written).toContain('shodan-key');
    // And no key that could hold one.
    const parsed = JSON.parse(written.trim()) as Record<string, unknown>;
    expect(Object.keys(parsed).sort()).toEqual(
      ['action', 'at', 'detail', 'ip', 'machine', 'outcome', 'role', 'target', 'via', 'who'].sort()
    );
  });
});

describe('who is making this request', () => {
  const originals: string[] = [];
  beforeEach(() => {
    originals.length = 0;
  });
  afterEach(() => {
    for (const token of originals) sessions.revoke(token);
  });

  it('names the person behind a live console session', () => {
    const minted = sessions.mint('mohamed');
    originals.push(minted.token);
    const got = callerOf({ headers: { authorization: `Bearer ${minted.token}` }, ip: '10.0.0.4' });
    expect(got).toEqual({ who: 'mohamed', via: 'session', ip: '10.0.0.4' });
  });

  it('calls a service token a capability, not a person', () => {
    // `KONTRA_STATE_TOKEN` is shared by the CLI, CI and anything an operator scripted. Recording
    // it as a user would be a lie the trail cannot distinguish from a real one.
    const got = callerOf({ headers: { authorization: 'Bearer not-a-session' }, ip: '10.0.0.5' });
    expect(got.who).toBe('service-token');
    expect(got.via).toBe('token');
  });

  it('never puts any part of the token in the record', () => {
    const got = callerOf({ headers: { authorization: 'Bearer s3cr3t-token-value' }, ip: '' });
    expect(JSON.stringify(got)).not.toContain('s3cr3t');
  });

  it('records an unauthenticated caller rather than dropping them', () => {
    // A refused anonymous request on a privileged route is exactly the event worth seeing.
    expect(callerOf({ headers: {}, ip: '10.0.0.6' })).toEqual({
      who: 'anonymous',
      via: 'anonymous',
      ip: '10.0.0.6',
    });
  });
});
