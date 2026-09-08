/**
 * How every Temporal connection in this process is configured — the ONE place, on purpose.
 *
 * WHAT WAS WRONG. Nothing in this repository could connect to a secured Temporal. Seven call sites
 * in this package plus the Go handler, the Go host, both Python hosts and six CLI commands all
 * spelled `connect({ address })` and stopped there, so a self-hoster who put Temporal behind mTLS —
 * the ordinary thing to do with a server that holds every Run's history — had no way to point kontra
 * at it. That is a gap in the open product, independent of anyone's hosting plans.
 *
 * WHY ONE FUNCTION AND A TEST THAT COUNTS. Eighteen sites is the whole difficulty. A change that
 * reaches fourteen of them does not fail loudly: it produces a deployment that mostly works and has
 * one process talking plaintext to a server that accepts both, which is the worst of the three
 * possible outcomes because it looks like the good one. So the options are built here, every site
 * calls this, and `temporalTls.test.ts` fails when a `connect(` appears that does not — a guard
 * that asserts it FOUND the call sites before asserting anything about them, because a sweep that
 * matches zero files reports success.
 *
 * The same shape as the claim-check codec, which had to reach both SDKs to be correct: a client
 * wired one way and a host wired the other fails only once a payload is big enough, so a demo passes
 * and a real run does not.
 *
 * ── THE CONTRACT ────────────────────────────────────────────────────────────────────────────────
 *
 *     KONTRA_ADDRESS                    host:port, default localhost:7233 (unchanged)
 *     KONTRA_TEMPORAL_TLS               1|true|yes|on — TLS with the system trust store
 *     KONTRA_TEMPORAL_TLS_CA            PEM path: the server's root CA, for a private CA
 *     KONTRA_TEMPORAL_TLS_CERT          PEM path: this client's certificate   ┐ both, or neither
 *     KONTRA_TEMPORAL_TLS_KEY           PEM path: this client's private key   ┘
 *     KONTRA_TEMPORAL_TLS_SERVER_NAME   SNI override, for a proxy in front of the server
 *
 * PLAINTEXT REMAINS THE DEFAULT, and that is a compatibility promise rather than a preference: with
 * none of these set this returns exactly `{ address }`, which is byte-for-byte what every call site
 * passed before. An existing deployment needs no new configuration and cannot be broken by this file
 * existing.
 *
 * ANY ONE OF THEM TURNS TLS ON. Setting `_CA` and forgetting `KONTRA_TEMPORAL_TLS=1` would otherwise
 * read a certificate, build nothing from it, and connect in the clear — a silent downgrade produced
 * by configuration that looks complete.
 *
 * A MISCONFIGURATION IS A REFUSAL. An unreadable file, or a cert without its key, throws and names
 * the variable and the path. It never falls back to plaintext: a security setting that degrades to
 * off when it cannot be satisfied is the failure `watchdog.sh` shipped for years in this repository,
 * and ADR 0039 refuses the same shape for cosign. Key MATERIAL never reaches the message — the path
 * is a filesystem location and the bytes are the secret.
 */

import { readFileSync } from 'node:fs';

/** Every variable this file reads. Exported so the test can assert none is consulted elsewhere. */
export const TLS_VARS = [
  'KONTRA_TEMPORAL_TLS',
  'KONTRA_TEMPORAL_TLS_CA',
  'KONTRA_TEMPORAL_TLS_CERT',
  'KONTRA_TEMPORAL_TLS_KEY',
  'KONTRA_TEMPORAL_TLS_SERVER_NAME',
] as const;

/** The address every client dials, default included. One reading, so a mismatch is impossible. */
export const DEFAULT_ADDRESS = 'localhost:7233';

/** The subset of Temporal's `TLSConfig` this builds. Declared structurally rather than imported so
 *  that `@temporalio/client` and `@temporalio/worker` — which take the same shape through two
 *  different type paths — are both satisfied by one return type. */
export interface TemporalTlsConfig {
  serverNameOverride?: string;
  serverRootCACertificate?: Buffer;
  clientCertPair?: { crt: Buffer; key: Buffer };
}

export interface TemporalConnectOptions {
  address: string;
  /** ABSENT, never `false`, when TLS is off. `Connection.connect` treats a falsy `tls` as plaintext,
   *  so both work — absent is what the call sites passed before this file, which keeps the
   *  no-configuration path byte-identical rather than merely equivalent. */
  tls?: TemporalTlsConfig;
}

type Env = Record<string, string | undefined>;

/** `1`, `true`, `yes`, `on` — anything else is off. Case-insensitive, trimmed. */
function truthy(value: string | undefined): boolean {
  if (!value) return false;
  return ['1', 'true', 'yes', 'on'].includes(value.trim().toLowerCase());
}

/**
 * Read a PEM, or refuse.
 *
 * The variable AND the path are named: an operator chasing this needs to know which setting pointed
 * where, and a path is a filesystem location rather than a secret. The file's CONTENTS never reach
 * the message, and neither does a passphrase — this reads DER/PEM bytes and does not decrypt.
 */
function readPem(env: Env, variable: string): Buffer {
  const path = env[variable];
  if (!path) throw new Error(`${variable} is empty — set it to a PEM path or unset it entirely`);
  try {
    return readFileSync(path);
  } catch (err) {
    const why = err instanceof Error ? err.message : String(err);
    throw new Error(
      `${variable}=${path} could not be read (${why}). Temporal TLS is configured, so this is a ` +
        `refusal rather than a fall back to an unencrypted connection.`
    );
  }
}

/** Is any TLS setting present? Any one of them turns TLS on — see the header. */
export function tlsRequested(env: Env = process.env): boolean {
  return TLS_VARS.some((v) => (v === 'KONTRA_TEMPORAL_TLS' ? truthy(env[v]) : Boolean(env[v])));
}

/**
 * The options every `Connection.connect` / `NativeConnection.connect` / `Worker.create` in this
 * process is given.
 *
 * `env` is a parameter so the test can drive it without mutating the process, which is also what
 * lets the no-configuration case be asserted against an explicitly empty environment rather than
 * against whatever the suite happens to inherit.
 *
 * `address` overrides the environment for the one caller that is given one — `panels/converger.ts`
 * takes it as an option. It overrides only the ADDRESS: a caller that dials a different Temporal on
 * the same deployment still needs that deployment's TLS, and letting an override skip the TLS half
 * would be a per-call-site downgrade of exactly the kind this file exists to make impossible.
 */
export function temporalConnectOptions(
  opts: { env?: Env; address?: string } = {}
): TemporalConnectOptions {
  const env = opts.env ?? process.env;
  const address = opts.address ?? env.KONTRA_ADDRESS ?? DEFAULT_ADDRESS;
  if (!tlsRequested(env)) return { address };

  const tls: TemporalTlsConfig = {};

  if (env.KONTRA_TEMPORAL_TLS_SERVER_NAME) {
    tls.serverNameOverride = env.KONTRA_TEMPORAL_TLS_SERVER_NAME;
  }
  if (env.KONTRA_TEMPORAL_TLS_CA) {
    tls.serverRootCACertificate = readPem(env, 'KONTRA_TEMPORAL_TLS_CA');
  }

  // BOTH OR NEITHER. A cert without a key is not a partial configuration that could be completed at
  // connect time — it is mTLS that will not authenticate, and the server's rejection arrives as a
  // handshake failure that names neither variable.
  const hasCert = Boolean(env.KONTRA_TEMPORAL_TLS_CERT);
  const hasKey = Boolean(env.KONTRA_TEMPORAL_TLS_KEY);
  if (hasCert !== hasKey) {
    throw new Error(
      `KONTRA_TEMPORAL_TLS_CERT and KONTRA_TEMPORAL_TLS_KEY must be set together — ` +
        `${hasCert ? 'the key' : 'the certificate'} is missing. A client certificate without its ` +
        `key cannot authenticate, and the server would refuse the handshake without naming either.`
    );
  }
  if (hasCert && hasKey) {
    tls.clientCertPair = {
      crt: readPem(env, 'KONTRA_TEMPORAL_TLS_CERT'),
      key: readPem(env, 'KONTRA_TEMPORAL_TLS_KEY'),
    };
  }

  return { address, tls };
}

/**
 * One line describing the connection, for a boot log.
 *
 * NAMES THE MODE AND NEVER THE MATERIAL. "which Temporal, encrypted or not, with a client
 * certificate or not" is exactly what an operator needs when a worker is not polling, and it is the
 * whole of what is safe to print.
 */
export function describeConnection(options: TemporalConnectOptions): string {
  if (!options.tls) return `${options.address} (plaintext)`;
  const parts = ['TLS'];
  if (options.tls.serverRootCACertificate) parts.push('private CA');
  if (options.tls.clientCertPair) parts.push('client certificate');
  if (options.tls.serverNameOverride) parts.push(`SNI ${options.tls.serverNameOverride}`);
  return `${options.address} (${parts.join(', ')})`;
}
