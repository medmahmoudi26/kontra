import { describe, it, expect } from 'vitest';
import { registryReadAuth, registryWriteAuth } from './registryAuth';

// WHAT THESE PIN. `docker-compose.yml` renders zot's accessControl with `"defaultPolicy": []` on
// every repository tree, so with the three passwords set there is NO anonymous read — and every
// registry read in this orchestrator swallows its failures on purpose. The symptom of sending no
// credential is therefore not an error anywhere: it is an Images page that reports the registry as
// unreadable, and an `inuse-` reconciler that protects nothing while retention is armed.

const decode = (h: Record<string, string>): string =>
  Buffer.from((h.authorization ?? '').replace(/^Basic /, ''), 'base64').toString();

describe('reading', () => {
  it('sends nothing when no account is configured, because that install is anonymous', () => {
    expect(registryReadAuth({})).toEqual({});
  });

  it('prefers the account that may only read', () => {
    const h = registryReadAuth({
      KONTRA_REGISTRY_PULL_PASSWORD: 'readonly',
      KONTRA_REGISTRY_PUSH_ACTORS_PASSWORD: 'canwrite',
    });
    expect(decode(h)).toBe('pull:readonly');
  });

  it('falls back to a push account, because a service may only have been given one', () => {
    const h = registryReadAuth({ KONTRA_REGISTRY_PUSH_ACTORS_PASSWORD: 'canwrite' });
    expect(decode(h)).toBe('push-actors:canwrite');
  });

  it('honours a renamed account', () => {
    const h = registryReadAuth({
      KONTRA_REGISTRY_PULL_USER: 'reader',
      KONTRA_REGISTRY_PULL_PASSWORD: 'pw',
    });
    expect(decode(h)).toBe('reader:pw');
  });
});

describe('writing', () => {
  const env = {
    KONTRA_REGISTRY_PUSH_ACTORS_PASSWORD: 'actpw',
    KONTRA_REGISTRY_PUSH_RUNTIMES_PASSWORD: 'rtpw',
    KONTRA_REGISTRY_PULL_PASSWORD: 'pullpw',
  };

  // THE NAMESPACE PICKS THE ACCOUNT, and that is the security property rather than a detail: neither
  // push account may write the other's tree, so one credential cannot cover both.
  it('tags an actor repository as the actors account', () => {
    expect(decode(registryWriteAuth('actors/probe', env))).toBe('push-actors:actpw');
  });

  it('tags a runtime repository as the runtimes account', () => {
    expect(decode(registryWriteAuth('kontra-runtimes/python', env))).toBe('push-runtimes:rtpw');
  });

  it('never writes with the read-only account', () => {
    for (const repo of ['actors/probe', 'kontra-runtimes/python', 'bundles/hello']) {
      expect(decode(registryWriteAuth(repo, env))).not.toContain('pull:');
    }
  });

  it('sends nothing on an anonymous install, so the write is attempted rather than refused here', () => {
    expect(registryWriteAuth('actors/probe', {})).toEqual({});
  });
});
