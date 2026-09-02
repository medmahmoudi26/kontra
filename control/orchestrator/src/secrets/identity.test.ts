/**
 * The actor identity token: what it proves, and every way it must refuse.
 *
 * A token is the whole of "authenticated as itself", so the tests that matter are the forgeries —
 * a tampered subject, a signature from another store's key, an expired claim. Each is its own
 * test, because "it throws" tells you nothing about which forgery got in.
 */

import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { identityTtlSeconds, mintActorToken, verifyActorToken } from './identity';
import { SecretForbidden } from './types';

let dir: string;

beforeEach(() => {
  delete process.env.KONTRA_SECRETS_KEY;
  delete process.env.KONTRA_ACTOR_TOKEN_TTL;
  dir = mkdtempSync(join(tmpdir(), 'kontra-secrets-'));
});

describe('minting', () => {
  it('names one actor and expires', () => {
    const minted = mintActorToken('probe', { dir });
    expect(minted.identity).toBe('actor:probe');
    expect(minted.expiresAt).toBeGreaterThan(Date.now());
    expect(verifyActorToken(minted.token, { dir })).toBe('actor:probe');
  });

  it('defaults to a ttl measured in weeks, not hours', () => {
    // `@actor.load` runs every time the actor's resource opens — once per Session for as long as
    // the worker polls. An hour-long token would fail an actor that has worked all week.
    expect(identityTtlSeconds()).toBe(30 * 24 * 60 * 60);
    process.env.KONTRA_ACTOR_TOKEN_TTL = '60';
    expect(identityTtlSeconds()).toBe(60);
  });
});

describe('verifying', () => {
  it('refuses a token whose subject was edited', () => {
    const minted = mintActorToken('probe', { dir });
    const [prefix, body, sig] = minted.token.split('.');
    const claims = JSON.parse(Buffer.from(body!, 'base64url').toString('utf8')) as { sub: string };
    claims.sub = 'actor:victim';
    const forged = `${prefix}.${Buffer.from(JSON.stringify(claims)).toString('base64url')}.${sig}`;
    expect(() => verifyActorToken(forged, { dir })).toThrow(SecretForbidden);
  });

  it('refuses a token signed by another store', () => {
    const other = mkdtempSync(join(tmpdir(), 'kontra-secrets-'));
    const minted = mintActorToken('probe', { dir: other });
    expect(() => verifyActorToken(minted.token, { dir })).toThrow(SecretForbidden);
  });

  it('refuses an expired token, and says so — it is the one failure an operator can act on', () => {
    const minted = mintActorToken('probe', { dir, ttlSeconds: 60, now: Date.now() - 120_000 });
    expect(() => verifyActorToken(minted.token, { dir })).toThrow(/expired/);
  });

  it('refuses anything that is not a token of this format', () => {
    for (const junk of ['', 'Bearer x', 'kai1.only-two', 'kai9.a.b', '...', 'null']) {
      expect(() => verifyActorToken(junk, { dir })).toThrow(SecretForbidden);
    }
  });

  it('says the same thing about every forgery except expiry', () => {
    // Which of "wrong signature" and "not a token" happened is useful to somebody with a token
    // generator and to nobody else.
    const wrongKey = mintActorToken('probe', { dir: mkdtempSync(join(tmpdir(), 'kontra-secrets-')) });
    const a = String(((): unknown => { try { verifyActorToken(wrongKey.token, { dir }); } catch (e) { return e; } })());
    const b = String(((): unknown => { try { verifyActorToken('kai1.a.b', { dir }); } catch (e) { return e; } })());
    expect(a).toBe(b);
  });
});
