import { describe, expect, it } from 'vitest';
import { assertSafe, parseTerminalId, terminalId, tmuxWorkflowId, tryParseTerminalId } from './ids';

/**
 * These ids come from a browser and end up inside a command that runs as root on a fleet Machine.
 * The whole file exists so that the answer to "what if someone sends
 * `fleet:kf-crawl-01/kontra-webcrawl/$(id)`" is a parse failure and not a shell.
 */
describe('Terminal ids', () => {
  it('round-trips the canonical form', () => {
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    expect(parseTerminalId(id)).toEqual({
      // `mode`, not `provider` (slice 6): the first segment is the EXECUTION MODE — the place a
      // Worker runs — and it is a selector facet with three values, not the name of the one provider
      // there used to be. A caller that omits it still gets `fleet`, so no id minted before slice 6
      // changes shape.
      mode: 'fleet',
      machine: 'kf-crawl-01',
      session: 'kontra-webcrawl',
      window: 'actor',
    });
    expect(terminalId({ machine: 'kf-crawl-01', session: 'kontra-webcrawl', window: 'actor' })).toBe(id);
  });

  it('carries all three modes, and holds each one to its own node shape', () => {
    // The prefix is a facet, so nothing may assume `fleet:` — and each mode's node is admitted by a
    // DIFFERENT whitelist, because a fleet node reaches a ControlPath and an `ssh` argument, a docker
    // node reaches an argv element of `docker exec`, and a local node reaches no command at all.
    expect(
      terminalId({ mode: 'local', machine: 'main-droplet', session: 'kontra-webcrawl', window: 'actor' })
    ).toBe('local:main-droplet/kontra-webcrawl/actor');
    expect(
      terminalId({
        mode: 'docker',
        machine: 'kontra-webcrawl-0.2.0-0',
        session: 'kontra-webcrawl',
        window: 'actor',
      })
    ).toBe('docker:kontra-webcrawl-0.2.0-0/kontra-webcrawl/actor');
    expect(parseTerminalId('local:main-droplet/kontra-webcrawl/actor').mode).toBe('local');
    expect(parseTerminalId('docker:kontra-webcrawl-0.2.0-0/kontra-webcrawl/actor').mode).toBe('docker');

    // A hostname is not a Machine name, and a Machine name is a fine hostname — so the node has to be
    // checked against the mode that claims it, not against the union of the three.
    expect(() => parseTerminalId('fleet:main-droplet/kontra-x/actor')).toThrow();
    expect(() => parseTerminalId('local:main droplet/kontra-x/actor')).toThrow();
    expect(() => parseTerminalId('docker:kontra worker/kontra-x/actor')).toThrow();
    // And an unknown mode is a parse failure, not a fourth transport.
    expect(() => parseTerminalId('podman:kf-crawl-01/kontra-x/actor')).toThrow();
    expect(tryParseTerminalId('podman:kf-crawl-01/kontra-x/actor')).toBeNull();
  });

  it('rejects every shell metacharacter, in every position', () => {
    const hostile = [
      '$(id)',
      '`id`',
      'a;id',
      'a&&id',
      'a|id',
      'a>out',
      "a'b",
      'a"b',
      'a b',
      'a\nb',
      'a\\b',
      'a$b',
      '../etc',
      '*',
      '~root',
      '\0',
    ];
    for (const bad of hostile) {
      expect(() => parseTerminalId(`fleet:${bad}/kontra-x/actor`), `machine ${bad}`).toThrow();
      expect(() => parseTerminalId(`fleet:kf-crawl-01/${bad}/actor`), `session ${bad}`).toThrow();
      expect(() => parseTerminalId(`fleet:kf-crawl-01/kontra-x/${bad}`), `window ${bad}`).toThrow();
      expect(() => assertSafe('command', `journalctl -fu x; ${bad}`), `command ${bad}`).toThrow();
    }
  });

  it('rejects a structure that could smuggle a segment', () => {
    for (const bad of [
      'fleet:kf-crawl-01/kontra-x/actor/extra',
      'fleet:kf-crawl-01/kontra-x',
      'fleet:kf-crawl-01',
      // `local:` is IMPLEMENTED since slice 6, so what used to be rejected as a reserved prefix is
      // now rejected for structure only — a second colon still cannot smuggle a second mode.
      'local:main-droplet/kontra-x/actor:more',
      'local:main-droplet/kontra-x',
      'fleet:kf-crawl-01/kontra-x/actor:more',
      'kf-crawl-01/kontra-x/actor',
      '',
      'fleet:',
      'fleet://kontra-x/actor',
    ]) {
      expect(() => parseTerminalId(bad), bad).toThrow();
      expect(tryParseTerminalId(bad), bad).toBeNull();
    }
  });

  it('holds the Machine name to the shape programs/fleet.ts mints', () => {
    // Deterministic names are what let a saved Dashboard slot point at a Machine at all.
    expect(() => parseTerminalId('fleet:kf-crawl-01/s/w')).not.toThrow();
    expect(() => parseTerminalId('fleet:kf-web-crawl-99/s/w')).not.toThrow();
    for (const bad of ['crawl-01', 'kf-crawl-1', 'kf-crawl-001', 'KF-CRAWL-01', 'kf--01', 'kf-crawl-01x']) {
      expect(() => parseTerminalId(`fleet:${bad}/s/w`), bad).toThrow();
    }
  });

  it('bounds length, so a 4 KiB id cannot reach a command line', () => {
    expect(() => parseTerminalId(`fleet:kf-crawl-01/${'s'.repeat(64)}/actor`)).not.toThrow();
    expect(() => parseTerminalId(`fleet:kf-crawl-01/${'s'.repeat(65)}/actor`)).toThrow();
    expect(() => parseTerminalId(`fleet:kf-crawl-01/s/${'w'.repeat(65)}`)).toThrow();
  });

  it('validates a Machine before naming its workflow', () => {
    expect(tmuxWorkflowId('kf-crawl-01')).toBe('tmux-kf-crawl-01');
    // A workflow id is a Temporal identifier, but it is also a log line and a CLI argument.
    expect(() => tmuxWorkflowId('kf-crawl-01; rm -rf /')).toThrow();
  });

  it('admits ONLY a journal follower as a window command', () => {
    // Narrower than "no shell metacharacters": a window's command runs as root on a Machine, and
    // ADR 0020's finding (2) makes "panes hold journals and nothing else" an invariant rather than a
    // preference — a stalled viewer can segfault a Machine's tmux server.
    expect(() => assertSafe('command', 'journalctl -fu kontra-actor.service')).not.toThrow();
    expect(() => assertSafe('command', 'journalctl -n 200 -fu kontra-handler.service')).not.toThrow();
    expect(() => assertSafe('command', 'journalctl -n 200 --no-pager -fu ssh.service')).not.toThrow();

    for (const bad of [
      "journalctl -fu x' ; id ; '",
      'journalctl -fu $(id)',
      'bash',
      '/opt/kontra/bin/handler', // the Worker itself — the invariant this exists to hold
      'python3 /opt/kontra/actor/webcrawl/actor.py',
      'journalctl --vacuum-size=1',
      'journalctl -fu kontra-actor.service; rm -rf /opt/kontra',
      'tail -f /var/log/syslog',
    ]) {
      expect(() => assertSafe('command', bad), bad).toThrow();
    }
  });
});
