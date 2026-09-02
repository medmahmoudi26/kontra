import { describe, expect, it } from 'vitest';
import { SSH_FLAGS, knownHostsPath, sshArgs, type SshTarget } from './ssh';
import { interpretProbe } from './probe';
import { looksLikeHostKeyChanged } from './tmux';
import type { MachineTarget } from './discovery';

/**
 * The fleet transport's argv, and the one property of it this deployment learned the hard way.
 *
 * THE MEASURED FAILURE. The streamer's shared known-hosts file held learned keys for `10.124.0.3`
 * through `10.124.0.12` — the DigitalOcean VPC's whole fleet range. A run's Machines are
 * destroyed when it ends and the next run's are NEW hosts at those SAME addresses, so
 * `StrictHostKeyChecking=accept-new` refused every one of them with `Host key verification failed`
 * and EVERY fleet tile on the Monitor showed an SSH error, on every fleet after the first.
 *
 * These tests pin the shape that makes that impossible rather than the sentence that reports it.
 */

const M1: SshTarget = { machine: 'kf-dns-01', host: '10.124.0.4', publicIp: '64.23.134.174' };

/** The SAME name at the SAME VPC address, next run — a different Droplet with a new host key.
 *  This is the pair the old shared file could not tell apart. */
const NEXT_FLEET: SshTarget = { machine: 'kf-dns-01', host: '10.124.0.4', publicIp: '143.198.1.9' };

function flagValue(args: readonly string[], name: string): string | undefined {
  const i = args.findIndex((a) => a.startsWith(`${name}=`));
  return i < 0 ? undefined : args[i]!.slice(name.length + 1);
}

describe('the known-hosts file is scoped to one Machine', () => {
  it('gives two Fleets of the same Machine name different files', () => {
    // The bug, stated as an inequality. Same name, same address, different Droplet: if these two
    // share a file, the second Fleet's every tile reports an SSH error and no operator action
    // fixes it.
    expect(knownHostsPath(M1)).not.toBe(knownHostsPath(NEXT_FLEET));
  });

  it('gives the same Machine the same file on every poll', () => {
    // The other half. A file that varied per call would re-learn the key every three seconds,
    // which is TOFU that has been switched off while still looking switched on.
    expect(knownHostsPath(M1)).toBe(knownHostsPath({ ...M1 }));
  });

  it('names the Machine in the path, so a stale file can be attributed', () => {
    expect(knownHostsPath(M1)).toContain('kf-dns-01');
  });

  it('refuses a machine name that is not a Machine name', () => {
    // The path reaches the filesystem. `machine` is the one part of it that is not hashed.
    expect(() => knownHostsPath({ machine: '../../etc/ssh', host: '10.0.0.1' })).toThrow();
  });

  it('does not let an address out of the hash and into the path', () => {
    // An address arrives from a stack output, so it is never a path segment — only ever hashed.
    const evil = knownHostsPath({ machine: 'kf-dns-01', host: '10.0.0.1', publicIp: '../../root' });
    expect(evil).not.toContain('..');
    expect(evil).not.toContain('root');
  });
});

describe('sshArgs', () => {
  it('points ssh at the scoped file and at no other', () => {
    const args = sshArgs(M1, 'tmux list-panes -a');
    expect(flagValue(args, 'UserKnownHostsFile')).toBe(knownHostsPath(M1));
    // `/etc/ssh/ssh_known_hosts` is the other file `accept-new` consults, and a key in it would
    // defeat the scoping entirely.
    expect(flagValue(args, 'GlobalKnownHostsFile')).toBe('/dev/null');
  });

  it('still REFUSES a changed key rather than turning checking off', () => {
    // The fix is where the keys are written, not whether they are checked. `no` here would trade a
    // real detection for a config change, which is the shortcut this file exists to not have taken.
    expect(SSH_FLAGS).toContain('StrictHostKeyChecking=accept-new');
    expect(sshArgs(M1, 'true')).not.toContain('StrictHostKeyChecking=no');
  });

  it('keeps the remote command as one argv element', () => {
    const remote = 'tmux capture-pane -p -e -S -50 -t \'kontra-dns:actor\'';
    const args = sshArgs(M1, remote);
    expect(args[args.length - 1]).toBe(remote);
    expect(args[args.length - 2]).toBe('root@10.124.0.4');
  });
});

describe('a refused host key is its own tile', () => {
  const machine: MachineTarget = {
    mode: 'fleet',
    machine: 'kf-dns-01',
    host: '10.124.0.4',
    publicIp: '64.23.134.174',
    tag: 'dns',
    fleet: 'dns',
    actor: 'nscheck',
    version: '0.1.0',
    session: 'kontra-nscheck',
    windows: ['actor'],
  };

  // Verbatim from OpenSSH 9.2, which is what the streamer image ships.
  const STDERR = [
    '@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@',
    '@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @',
    '@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@',
    'Host key verification failed.',
  ].join('\n');

  it('recognises the refusal', () => {
    expect(looksLikeHostKeyChanged(255, STDERR)).toBe(true);
    // Not every exit-255 — those are unreachable Machines, and the operator's answer is different.
    expect(looksLikeHostKeyChanged(255, 'ssh: connect to host 10.124.0.4 port 22: No route')).toBe(
      false
    );
    expect(looksLikeHostKeyChanged(0, STDERR)).toBe(false);
  });

  it('says the Machine answered, rather than printing an exit code', () => {
    const probe = interpretProbe(machine, 255, '', STDERR);
    expect(probe.reachable).toBe('fail');
    expect(probe.detail).toContain('kf-dns-01');
    expect(probe.detail).toContain('10.124.0.4');
    // The distinguishing claim: the Machine is fine. A tile that said "unreachable" would send an
    // operator to look at a Droplet that is up and serving.
    expect(probe.detail).toMatch(/host key/i);
    expect(probe.detail).not.toMatch(/exited 255/);
  });
});
