/**
 * The Temporal connection is configured in ONE place, and this is what keeps that true.
 *
 * Two halves, and the second is the one that matters. The first asserts the options are built
 * correctly; the second walks this package's sources and fails when a `connect(` appears that does
 * not go through {@link temporalConnectOptions}. Sixteen client connections across three languages is the
 * whole difficulty of this change: a version that reaches most of them produces a deployment which
 * mostly works and has one process talking plaintext to a server that accepts both — the worst
 * outcome, because it looks like the good one.
 */

import { readdirSync, readFileSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  DEFAULT_ADDRESS,
  describeConnection,
  temporalConnectOptions,
  tlsRequested,
  TLS_VARS,
} from './temporalTls';

/** A PEM on disk. Contents are never parsed by this module — it reads bytes and hands them over. */
function pem(name: string, body = 'not-a-real-certificate'): string {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-tls-'));
  const path = join(dir, name);
  writeFileSync(path, body);
  return path;
}

describe('the no-configuration path', () => {
  it('is byte-identical to what every call site passed before this file existed', () => {
    // THE COMPATIBILITY PROMISE. An existing deployment sets none of these, and must be unable to
    // notice this file exists. `tls` ABSENT rather than `false` is the point: both connect in the
    // clear, and absent is what was passed before.
    const options = temporalConnectOptions({ env: {} });
    expect(options).toEqual({ address: DEFAULT_ADDRESS });
    expect('tls' in options).toBe(false);
  });

  it('still honours KONTRA_ADDRESS, and an explicit override beats it', () => {
    expect(temporalConnectOptions({ env: { KONTRA_ADDRESS: 'temporal:7233' } }).address).toBe('temporal:7233');
    expect(
      temporalConnectOptions({ env: { KONTRA_ADDRESS: 'temporal:7233' }, address: 'other:7233' }).address
    ).toBe('other:7233');
  });
});

describe('turning TLS on', () => {
  it('needs only the switch, and uses the system trust store', () => {
    const options = temporalConnectOptions({ env: { KONTRA_TEMPORAL_TLS: '1' } });
    expect(options.tls).toEqual({});
    expect(describeConnection(options)).toContain('TLS');
  });

  it.each(['1', 'true', 'TRUE', 'yes', ' on '])('accepts %o as on', (value) => {
    expect(tlsRequested({ KONTRA_TEMPORAL_TLS: value })).toBe(true);
  });

  it.each(['0', 'false', 'no', 'off', '', undefined])('treats %o as off', (value) => {
    expect(tlsRequested({ KONTRA_TEMPORAL_TLS: value })).toBe(false);
  });

  it('is turned on by ANY setting, not only the switch', () => {
    // THE SILENT DOWNGRADE THIS CLOSES. Setting a CA and forgetting the switch would otherwise read
    // the certificate, build nothing from it, and connect in the clear — a plaintext connection
    // produced by configuration that looks complete.
    for (const v of TLS_VARS) {
      if (v === 'KONTRA_TEMPORAL_TLS') continue;
      expect(tlsRequested({ [v]: 'x' }), `${v} alone must turn TLS on`).toBe(true);
    }
  });

  it('reads a private CA and a client pair off disk', () => {
    const options = temporalConnectOptions({
      env: {
        KONTRA_TEMPORAL_TLS_CA: pem('ca.pem', 'ca-bytes'),
        KONTRA_TEMPORAL_TLS_CERT: pem('crt.pem', 'crt-bytes'),
        KONTRA_TEMPORAL_TLS_KEY: pem('key.pem', 'key-bytes'),
        KONTRA_TEMPORAL_TLS_SERVER_NAME: 'temporal.internal',
      },
    });
    expect(options.tls?.serverRootCACertificate?.toString()).toBe('ca-bytes');
    expect(options.tls?.clientCertPair?.crt.toString()).toBe('crt-bytes');
    expect(options.tls?.clientCertPair?.key.toString()).toBe('key-bytes');
    expect(options.tls?.serverNameOverride).toBe('temporal.internal');
    expect(describeConnection(options)).toBe(
      `${DEFAULT_ADDRESS} (TLS, private CA, client certificate, SNI temporal.internal)`
    );
  });
});

describe('a misconfiguration is a refusal, never a fall back to plaintext', () => {
  it('throws when a certificate file cannot be read, naming the variable and the path', () => {
    // A security setting that degrades to off when it cannot be satisfied is the failure
    // `watchdog.sh` shipped for years here, and the shape ADR 0039 refuses for cosign.
    expect(() =>
      temporalConnectOptions({ env: { KONTRA_TEMPORAL_TLS_CA: '/nope/missing-ca.pem' } })
    ).toThrow(/KONTRA_TEMPORAL_TLS_CA=\/nope\/missing-ca\.pem could not be read/);
  });

  it('throws when a client certificate is set without its key, and the other way round', () => {
    const crt = pem('crt.pem');
    const key = pem('key.pem');
    expect(() => temporalConnectOptions({ env: { KONTRA_TEMPORAL_TLS_CERT: crt } })).toThrow(/the key is missing/);
    expect(() => temporalConnectOptions({ env: { KONTRA_TEMPORAL_TLS_KEY: key } })).toThrow(
      /the certificate is missing/
    );
  });

  it('never puts key material in a message', () => {
    // The PATH is a filesystem location and is named on purpose — an operator needs to know which
    // setting pointed where. The BYTES are the secret and never appear.
    const key = pem('key.pem', 'SUPER-SECRET-KEY-MATERIAL');
    try {
      temporalConnectOptions({ env: { KONTRA_TEMPORAL_TLS_CERT: '/nope/x.pem', KONTRA_TEMPORAL_TLS_KEY: key } });
      expect.unreachable('should have thrown on the unreadable certificate');
    } catch (err) {
      expect(String(err)).not.toContain('SUPER-SECRET-KEY-MATERIAL');
      expect(String(err)).toContain('KONTRA_TEMPORAL_TLS_CERT');
    }
  });

  it('says plaintext plainly, so a boot log distinguishes the two', () => {
    expect(describeConnection({ address: 'a:1' })).toBe('a:1 (plaintext)');
  });
});

/** Every `.ts` under `src/`, excluding tests and this module's own subject. */
function sources(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name === 'node_modules' || entry.name === '__pycache__') continue;
      sources(path, out);
    } else if (entry.name.endsWith('.ts') && !entry.name.includes('.test.')) {
      out.push(path);
    }
  }
  return out;
}

describe('every connection in this package goes through one function', () => {
  // `Connection.connect(` and `NativeConnection.connect(`, however the module was imported —
  // `panels/pollers.ts` reaches it through a lazy `require` as `client.Connection.connect`, which a
  // sweep for the bare identifier misses. That site was found by this rule, not by the change.
  const CONNECT = /(?:^|[^\w.])(?:\w+\.)?(?:Native)?Connection\.connect\(/g;

  const files = sources(join(__dirname));

  it('found the sources it is about to judge', () => {
    // NON-VACUOUS. A walk that returns nothing passes every assertion below, which is how a guard
    // reports success for a tree it never read — this repository's most repeated failure.
    expect(files.length).toBeGreaterThan(50);
    expect(files.some((f) => f.endsWith('temporalClient.ts'))).toBe(true);
  });

  it('found the call sites, and every one of them is wired', () => {
    const sites: string[] = [];
    const bare: string[] = [];
    for (const file of files) {
      if (file.endsWith('temporalTls.ts')) continue; // the definition, not a call site
      const text = readFileSync(file, 'utf8');
      for (const line of text.split('\n')) {
        CONNECT.lastIndex = 0;
        if (!CONNECT.test(line)) continue;
        sites.push(`${file}: ${line.trim()}`);
        if (!line.includes('temporalConnectOptions(')) bare.push(`${file}: ${line.trim()}`);
      }
    }
    // The count is asserted before the content: a regex that stopped matching would otherwise
    // report a clean sweep over nothing.
    expect(sites.length, 'no Temporal connect sites found — the pattern stopped matching').toBeGreaterThanOrEqual(8);
    expect(bare, 'a Temporal connection that bypasses temporalConnectOptions').toEqual([]);
  });

  it('leaves no second reading of the TLS environment', () => {
    // A READ, not a mention. One function, one reading: a call site that RESOLVED
    // `KONTRA_TEMPORAL_TLS_*` itself would be a second policy that agrees today and drifts later.
    // Naming the variables in documentation is fine and wanted — the Go arm's version of this guard
    // was `includes()` and failed on the `.kontra/config.yaml` template that documents them, which
    // is a guard making the codebase worse in order to be satisfied.
    const READ = /process\.env(?:\.|\[')(KONTRA_TEMPORAL_TLS[A-Z_]*)/g;
    const offenders: string[] = [];
    for (const file of files) {
      if (file.endsWith('temporalTls.ts')) continue;
      const text = readFileSync(file, 'utf8');
      for (const m of text.matchAll(READ)) offenders.push(`${file} reads ${m[1]}`);
    }
    expect(offenders).toEqual([]);
    // NON-VACUOUS: the pattern must match a real read, or the sweep above proves nothing.
    expect("process.env.KONTRA_TEMPORAL_TLS_CA".match(READ)).not.toBeNull();
    expect(TLS_VARS.length).toBeGreaterThan(0);
  });
});

describe('the shared contract, executed from shared/conformance/temporal_tls.json', () => {
  // THE THIRD ARM. Go and Python drive the same rows; what the corpus pins is the DECISION — TLS on
  // or off, which material is present, which misconfigurations are refusals — because that is what
  // an operator configures and what must not disagree across sixteen client connections.
  type Case = {
    why: string;
    env: Record<string, string>;
    tls: boolean;
    ca?: boolean;
    client_pair?: boolean;
    server_name?: string;
  };
  type Refusal = { why: string; env: Record<string, string>; names: string[] };

  const corpus = JSON.parse(
    readFileSync(join(__dirname, '../../../shared/conformance/temporal_tls.json'), 'utf8')
  ) as { truthy: string[]; falsy: string[]; cases: Case[]; refusal_cases: Refusal[] };

  // The placeholders the corpus uses. This arm does not parse the bytes — it hands them to the SDK —
  // so any readable file will do; Go validates the pair and needs a real one, which is why the corpus
  // carries paths rather than material.
  const paths = { '@ca': pem('ca.pem', 'ca'), '@crt': pem('crt.pem', 'crt'), '@key': pem('key.pem', 'key') };
  const resolve = (env: Record<string, string>): Record<string, string> =>
    Object.fromEntries(
      Object.entries(env).map(([k, v]) => [k, (paths as Record<string, string>)[v] ?? v])
    );

  it('did not silently shrink', () => {
    expect(corpus.cases.length).toBeGreaterThanOrEqual(8);
    expect(corpus.refusal_cases.length).toBeGreaterThanOrEqual(3);
    const blob = JSON.stringify(corpus);
    for (const marker of ['OFF DOES NOT OVERRIDE A CA', 'SILENT DOWNGRADE']) {
      expect(blob, `the corpus no longer carries ${marker}`).toContain(marker);
    }
  });

  for (const c of corpus.cases) {
    it(c.why, () => {
      const env = resolve(c.env);
      expect(tlsRequested(env)).toBe(c.tls);
      const options = temporalConnectOptions({ env });
      if (!c.tls) {
        expect(options.tls).toBeUndefined();
        return;
      }
      expect(options.tls).toBeDefined();
      expect(Boolean(options.tls?.serverRootCACertificate)).toBe(c.ca);
      expect(Boolean(options.tls?.clientCertPair)).toBe(c.client_pair);
      expect(options.tls?.serverNameOverride ?? '').toBe(c.server_name ?? '');
    });
  }

  for (const c of corpus.refusal_cases) {
    it(c.why, () => {
      let message = '';
      try {
        temporalConnectOptions({ env: resolve(c.env) });
        expect.unreachable('a misconfiguration returned no error — this is the silent fall back');
      } catch (err) {
        message = String(err);
      }
      for (const name of c.names) expect(message, `does not name ${name}`).toContain(name);
    });
  }

  it('reads the switch the same way the other two arms do', () => {
    for (const v of corpus.truthy) expect(tlsRequested({ KONTRA_TEMPORAL_TLS: v }), v).toBe(true);
    for (const v of corpus.falsy) expect(tlsRequested({ KONTRA_TEMPORAL_TLS: v }), v).toBe(false);
  });
});
