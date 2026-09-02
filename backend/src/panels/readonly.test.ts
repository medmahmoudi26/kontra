import { readFileSync, readdirSync } from 'node:fs';
import * as path from 'node:path';
import { describe, expect, it } from 'vitest';

/**
 * The read-only boundary, pinned against the source itself.
 *
 * ADR 0020's finding (3) is the reason this test exists rather than a comment: through a
 * **read-only** (`-r`) tmux client, both `run-shell "touch …"` and `send-keys` into an interactive
 * shell EXECUTED, as the Machine's root, and tmux returned no error. `-r` gates a client's keystroke
 * handling, not tmux commands. So "read-only" can never be a property of tmux — it is a property of
 * this code, which means it is only true for as long as nobody adds a write.
 *
 * A grep is a blunt instrument, and that is the point: the invariant it guards ("no route, no
 * message type, no code path can put bytes into a session's channel") is not something a type can
 * express, and the next person to reach for `send-keys` should have to delete a test that explains
 * why they must not.
 *
 * `cli/fleet.go`'s own suite pins its program the same way, by reading the source.
 */
const HERE = __dirname;

/**
 * `ids.ts`, `tmux.ts` and `types.ts` now live in `@kontra/core` and what is left beside this file
 * is a one-line re-export of each. THE SWEEP HAS TO FOLLOW THEM: reading only this directory still
 * finds files, still greps them, and still passes — while examining three re-export lines instead
 * of the code the invariant is about. That is the failure this suite exists to prevent, arriving
 * as a refactor rather than as a `send-keys`.
 */
const CORE_PANELS = path.join(HERE, '..', '..', '..', 'core', 'src', 'panels');

function panelSources(): Array<{ file: string; source: string }> {
  const out: Array<{ file: string; source: string }> = [];
  const add = (file: string): void => out.push({ file, source: readFileSync(file, 'utf8') });
  for (const dir of [HERE, CORE_PANELS]) {
    for (const entry of readdirSync(dir)) {
      if (entry.endsWith('.test.ts')) continue;
      if (entry.endsWith('.ts')) add(path.join(dir, entry));
    }
  }
  add(path.join(HERE, '..', 'panels.ts'));
  add(path.join(HERE, '..', 'activities', 'panels.ts'));
  add(path.join(HERE, '..', 'workflows', 'tmuxSession.ts'));
  return out;
}

describe('nothing can write to a session (ADR 0020, finding 3)', () => {
  const sources = panelSources();

  it('reads every panels source, so a new file cannot slip past this test', () => {
    const names = sources.map((s) => path.basename(s.file));
    expect(names).toContain('server.ts');
    expect(names).toContain('ssh.ts');
    expect(names).toContain('panels.ts');
    expect(sources.length).toBeGreaterThan(8);
  });

  it('contains no tmux command that could inject into a pane', () => {
    // `run-shell` is arbitrary command execution as the Machine's root; `send-keys`, `paste-buffer`
    // and `load-buffer` are input. None of them may appear anywhere in the panels path.
    const forbidden = ['send-keys', 'run-shell', 'paste-buffer', 'load-buffer', 'set-buffer'];
    for (const { file, source } of sources) {
      for (const needle of forbidden) {
        // Comments are allowed to name what is forbidden — this very rule has to be explainable.
        const codeLines = source
          .split('\n')
          .filter((l) => !l.trim().startsWith('*') && !l.trim().startsWith('//') && !l.trim().startsWith('#'));
        expect(codeLines.join('\n'), `${path.basename(file)} must not use ${needle}`).not.toContain(
          needle
        );
      }
    }
  });

  it('never opens a writable stdin to a Machine', () => {
    const ssh = readFileSync(path.join(HERE, 'ssh.ts'), 'utf8');
    // stdin is explicitly 'ignore'. Not 'pipe' and not inherited: there must be no file descriptor
    // that bytes could be written to, even by accident.
    expect(ssh).toContain("stdio: ['ignore', 'pipe', 'pipe']");
    for (const { file, source } of sources) {
      expect(source, path.basename(file)).not.toMatch(/stdin\s*[.?]\s*write/);
      expect(source, path.basename(file)).not.toMatch(/stdin\s*\.\s*end\s*\(/);
    }
  });

  it('spawns ssh with an argv array and never through a shell', () => {
    const ssh = readFileSync(path.join(HERE, 'ssh.ts'), 'utf8');
    expect(ssh).toContain("spawn('ssh', args,");
    // `shell: true` would make every remote string a local shell command as well as a remote one,
    // and `exec`/`execSync` ARE a shell — `RegExp.prototype.exec` is not, hence the narrow patterns.
    for (const { file, source } of sources) {
      expect(source, path.basename(file)).not.toMatch(/shell:\s*true/);
      expect(source, path.basename(file)).not.toMatch(/\bexecSync\s*\(|\bexecFile\s*\(/);
      expect(source, path.basename(file)).not.toMatch(/(^|[^.\w])exec\s*\(\s*['"`]/);
    }
  });

  it('accepts no client message that carries a payload', () => {
    // The client→server union in types.ts is the whole surface a browser can reach. If a message
    // ever grows a `data` or `bytes` field, this fails — which is the moment to re-read finding (3).
    const types = readFileSync(path.join(CORE_PANELS, 'types.ts'), 'utf8');
    const at = types.indexOf('export type ClientMessage');
    // FOUND, BEFORE ANYTHING IS CONCLUDED FROM IT. `indexOf` returns -1 when the declaration moves,
    // `slice(-1)` then yields one character and the two negative assertions below pass against it —
    // a green test reporting on a file it did not read. This is how the extraction to @kontra/core
    // first went: `union` was the empty string.
    expect(at, 'ClientMessage union not found in core/src/panels/types.ts').toBeGreaterThan(-1);
    const union = types.slice(at, types.indexOf(';', at));
    expect(union).toContain("t: 'subscribe'");
    expect(union).not.toMatch(/\bdata\b|\bbytes\b|\bkeys\b|\binput\b|\bwrite\b/);
  });

  it('refuses binary frames from a client', () => {
    // The one path where bytes could arrive from a browser at all is answered with an error rather
    // than being routed anywhere.
    const server = readFileSync(path.join(HERE, 'server.ts'), 'utf8');
    expect(server).toContain('binary frames from a client are not accepted');
  });

  it('never hands the browser the wider token', () => {
    // KONTRA_STATE_TOKEN also authorises POST /api/infra/stacks/:fqn/:op. A credential in a page
    // must not be able to spend money.
    for (const { file, source } of sources) {
      expect(source.replace(/\/\*[^]*?\*\//g, ''), path.basename(file)).not.toContain(
        'KONTRA_STATE_TOKEN'
      );
    }
  });
});
