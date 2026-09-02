/**
 * What the `local` mode calls the machine it means.
 *
 * THE NAME IS READ, NOT JUST PARSED. It becomes the node segment of every `local:` id, so it is what
 * the Monitor's tree draws as the Machine holding every locally-served Worker, and what a saved
 * Dashboard still says months later about where a Terminal was.
 *
 * `os.hostname()` IS THE CONTAINER'S. Every deployment with a Dashboard runs this in one, so the
 * hostname is Docker's 12-hex default — and the tree drew `54af48ee4c6a` as the Machine, which
 * identifies nothing anybody can act on: not the host the tmux server actually runs on (that is the
 * machine outside), not stable across a `compose up`, not pingable, not ssh-able. The premise the
 * original rule was written for still stands — a real host name beats `localhost` — but a container
 * id is not a real host name.
 */

import { afterEach, describe, expect, it, vi } from 'vitest';

const saved = process.env.KONTRA_PANEL_LOCAL_HOST;

afterEach(() => {
  vi.restoreAllMocks();
  vi.resetModules();
  if (saved === undefined) delete process.env.KONTRA_PANEL_LOCAL_HOST;
  else process.env.KONTRA_PANEL_LOCAL_HOST = saved;
});

/** Re-imported per test because the hostname is read at CALL time but `os` is mocked per module. */
async function hostWith(hostname: string): Promise<string> {
  vi.resetModules();
  vi.doMock('node:os', () => ({ hostname: () => hostname, default: { hostname: () => hostname } }));
  const { localHost } = await import('./local');
  return localHost();
}

describe('the node name a local: id carries', () => {
  it('keeps a real host name, which is the whole point of not hard-coding localhost', () => {
    // `local:main-droplet/kontra-webcrawl/actor` says WHERE in a log where `local:localhost/…`
    // does not. Nothing about this change gives that up.
    delete process.env.KONTRA_PANEL_LOCAL_HOST;
    return expect(hostWith('main-droplet')).resolves.toBe('main-droplet');
  });

  it('refuses a 12-hex container id and says localhost instead', async () => {
    // The one the user caught: MACHINES → `local` → `54af48ee4c6a`, which is a lease, not a name.
    delete process.env.KONTRA_PANEL_LOCAL_HOST;
    await expect(hostWith('54af48ee4c6a')).resolves.toBe('localhost');
  });

  it('refuses the long form too — `--hostname` full ids are 64 hex', async () => {
    delete process.env.KONTRA_PANEL_LOCAL_HOST;
    await expect(hostWith('a'.repeat(64))).resolves.toBe('localhost');
  });

  it('does not mistake a real name that happens to be hex-ish', async () => {
    // `dead-beef-01` is somebody's machine. The rule is EXACTLY Docker's shape — bare hex, 12 or
    // 64 — because a heuristic that ate real host names would lose the information it protects.
    delete process.env.KONTRA_PANEL_LOCAL_HOST;
    await expect(hostWith('dead-beef-01')).resolves.toBe('dead-beef-01');
    await expect(hostWith('abcdef')).resolves.toBe('abcdef');
    await expect(hostWith('abcdef0123456')).resolves.toBe('abcdef0123456');
  });

  it('lets the operator name the machine outside, and that wins over everything', async () => {
    // The tmux server is the HOST's; this process is in a container and cannot learn its name. The
    // variable is the only party that knows, so it beats both the probe and the fallback.
    process.env.KONTRA_PANEL_LOCAL_HOST = 'main-droplet';
    await expect(hostWith('54af48ee4c6a')).resolves.toBe('main-droplet');
  });

  it('falls back to localhost for a name that could not be an id segment', async () => {
    // The id grammar is `<mode>:<node>/<session>/<window>`; a hostname with a slash in it would
    // mint an id that parses as a different Terminal.
    delete process.env.KONTRA_PANEL_LOCAL_HOST;
    await expect(hostWith('has/slash')).resolves.toBe('localhost');
  });
});
