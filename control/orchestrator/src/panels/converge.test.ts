import { describe, expect, it } from 'vitest';
import {
  convergeScript,
  DEFAULT_WINDOWS,
  GEOMETRY,
  HISTORY_LIMIT,
  killScript,
  parseConvergeReport,
  validateConvergeSpec,
} from './converge';

const SPEC = { session: 'kontra-webcrawl', windows: [...DEFAULT_WINDOWS] };

describe('the session converge script', () => {
  const script = convergeScript(SPEC);

  it('is idempotent: every mutation is guarded by its own existence check', () => {
    // A re-converge runs this again on a Machine that already has the session. Without the guards
    // it would add a second `actor` window on every visit to the Dashboard.
    expect(script).toContain("tmux has-session -t 'kontra-webcrawl'");
    expect(script).toMatch(/has-session[^\n]*\|\|/);
    expect(script).toContain(
      "tmux list-windows -t 'kontra-webcrawl' -F '#{window_name}' | grep -qx 'handler' ||"
    );
    // Creating the session creates its FIRST window; only the rest are added.
    expect(script.match(/tmux new-session/g)?.length).toBe(1);
    expect(script.match(/tmux new-window/g)?.length).toBe(1);
  });

  it('sets the three options ADR 0020 makes load-bearing', () => {
    // Scrollback: tmux's 2000 default is too short to seed a Terminal, and cannot be raised
    // retroactively for lines already lost.
    expect(script).toContain(`tmux set-option -t 'kontra-webcrawl' history-limit ${HISTORY_LIMIT}`);
    expect(HISTORY_LIMIT).toBeGreaterThan(2000);
    // Manual sizing: with tmux's default (`latest`), an operator running `tmux attach` on the box
    // reflows every browser tile mid-stream.
    expect(script).toContain("tmux set-option -t 'kontra-webcrawl' window-size manual");
    // …and the fixed geometry that makes a snapshot a rectangle of known size, per window.
    expect(script).toContain(
      `tmux resize-window -t 'kontra-webcrawl:actor' -x ${GEOMETRY.cols} -y ${GEOMETRY.rows}`
    );
    expect(script).toContain(
      `tmux resize-window -t 'kontra-webcrawl:handler' -x ${GEOMETRY.cols} -y ${GEOMETRY.rows}`
    );
  });

  it('installs tmux itself, non-interactively', () => {
    // machine.ts no longer does. An apt prompt inside a Temporal activity is a hang, not a prompt.
    expect(script).toContain('command -v tmux >/dev/null 2>&1 ||');
    expect(script).toContain('apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends tmux');
    expect(script).toContain('DEBIAN_FRONTEND=noninteractive');
  });

  it('puts only journals in panes, never the Worker', () => {
    // Finding (2): a stalled viewer can segfault a Machine's tmux server, destroying every session
    // on that socket. That is only survivable because the panes hold journals.
    for (const w of DEFAULT_WINDOWS) expect(w.command).toContain('journalctl -fu');
    expect(script).not.toContain('/opt/kontra/bin/handler');
    expect(script).not.toContain('actor.py');
  });

  it('is byte-stable for the same input', () => {
    expect(convergeScript(SPEC)).toBe(script);
  });

  it('refuses a hostile session, window or command rather than quoting it', () => {
    expect(() => convergeScript({ ...SPEC, session: "x'; id; '" })).toThrow(/not safe/);
    expect(() => convergeScript({ session: 'ok', windows: [{ name: 'a b', command: 'x' }] })).toThrow();
    expect(() =>
      convergeScript({ session: 'ok', windows: [{ name: 'a', command: 'journalctl `id`' }] })
    ).toThrow();
    expect(() => convergeScript({ session: 'ok', windows: [] })).toThrow(/at least one window/);
    expect(() =>
      convergeScript({
        session: 'ok',
        windows: [
          { name: 'a', command: 'journalctl -fu x.service' },
          { name: 'a', command: 'journalctl -fu y.service' },
        ],
      })
    ).toThrow(/duplicate window/);
    // Only a journal follower is a legal window command — a pane must never hold the Worker
    // itself (ADR 0020, finding 2).
    expect(() =>
      convergeScript({ session: 'ok', windows: [{ name: 'shell', command: 'bash' }] })
    ).toThrow(/not safe/);
    expect(() => validateConvergeSpec({ ...SPEC, cols: -1 })).toThrow(/out of range/);
  });

  it('kills a session idempotently — absent is a successful outcome', () => {
    expect(killScript('kontra-webcrawl')).toContain("kill-session -t 'kontra-webcrawl'");
    expect(killScript('kontra-webcrawl')).toContain('|| true');
    expect(() => killScript('a;id')).toThrow();
  });
});

describe('the converge report', () => {
  it('reads created and the windows the Machine actually has', () => {
    expect(parseConvergeReport('noise\nKONTRA_TMUX created=1 windows=actor,handler,\n')).toEqual({
      created: true,
      windows: ['actor', 'handler'],
    });
    expect(parseConvergeReport('KONTRA_TMUX created=0 windows=actor,')).toEqual({
      created: false,
      windows: ['actor'],
    });
  });

  it('tolerates apt and tmux chatter around it', () => {
    const noisy = [
      'Reading package lists...',
      'Setting up tmux (3.2a-4ubuntu0.2) ...',
      'KONTRA_TMUX created=1 windows=actor,handler,',
    ].join('\n');
    expect(parseConvergeReport(noisy).created).toBe(true);
  });

  it('treats a missing report as a failure, not as an empty session', () => {
    // A script killed halfway would otherwise report a session with no windows, which reads as a
    // successful converge of nothing.
    expect(() => parseConvergeReport('Reading package lists...\n')).toThrow(/did not run to completion/);
  });
});
