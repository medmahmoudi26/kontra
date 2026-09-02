/**
 * The serve console, drawn — what an operator reads before and after pressing Serve.
 *
 * `renderToStaticMarkup` for the reason `HealthChips.test.ts` records: this suite runs in node with
 * no jsdom and no testing-library, and every assertion here is about text and `data-testid`, both of
 * which are in the markup.
 *
 * TWO OF THESE ARE ABOUT WHAT IS *NOT* THERE. A page that could serve an Actor to managed
 * containers or to a fleet from beside its editor would make "try this edit" and "deploy this" one
 * selection apart, and the second of those costs money and outlives the tab
 * (`backend/src/actorControl.ts`). An absent affordance leaves no trace in a diff, so it is
 * asserted rather than remembered.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { ServeConsole } from './WorkbenchPanes';
import { actorSession } from '@core/panels/tmux';
import type { ActorServeResult, Source } from '../run/api';

const PROBE: Source = {
  id: 'actor:probe:1f3k',
  kind: 'actor',
  name: 'probe',
  path: '/srv/checkout/examples/python/probe',
  version: '0.1.0',
  description: '',
  registeredAt: 1_700_000_000_000,
};

const SERVED: ActorServeResult = {
  actor: 'probe',
  version: '0.1.0',
  path: PROBE.path,
  session: 'probe-0_1_0',
  attach: 'tmux attach -t probe-0_1_0',
};

function draw(
  over: {
    served?: ActorServeResult | null;
    error?: string | null;
    serving?: string | null;
    busy?: string | null;
  } = {}
): string {
  const served = over.served ?? null;
  return renderToStaticMarkup(
    createElement(ServeConsole, {
      source: PROBE,
      session: served?.session ?? actorSession(PROBE.name, PROBE.version),
      served,
      error: over.error ?? null,
      serving: over.serving ?? null,
      busy: over.busy ?? null,
      onServe: () => {},
      onRestart: () => {},
    })
  );
}

describe('before anything has been served', () => {
  it('offers the control, and names the command it is', () => {
    const html = draw();
    expect(html).toContain('data-testid="actor-serve-button"');
    expect(html).toContain('kontra serve --actor /srv/checkout/examples/python/probe --mode local --tmux');
  });

  it('names the session the pane is watching, so an empty pane is readable', () => {
    // An Actor served an hour ago or from a terminal has a pane and no serve result here. Without
    // the name, "no pane" is unactionable: there is nothing to check with `tmux ls`.
    expect(draw()).toContain('probe-0_1_0');
  });
});

describe('after a serve', () => {
  const html = draw({ served: SERVED });

  it('reports the session and the attach command', () => {
    expect(html).toContain('data-testid="actor-session"');
    expect(html).toContain('data-testid="actor-attach"');
    expect(html).toContain('tmux attach -t probe-0_1_0');
  });

  it('shows the pane and the attach command as ONE session', () => {
    // THE CONTRACT THIS WHOLE SURFACE RESTS ON. The console prints the server's own answer back
    // and the pane is looked up by the same value, so a disagreement means the console names a
    // session the operator attaches to while the pane draws a different worker. The browser used
    // to derive its own copy of this rule; it imports the server's now (`@core/panels/tmux`), and
    // what still crosses a real boundary — `cli/tmux.go` and `cli/fleet.go` — is held by
    // conformance/queues.json.
    expect(SERVED.session).toBe(actorSession(PROBE.name, PROBE.version));
    expect(SERVED.attach).toContain(SERVED.session);
    expect(html).toContain(`tmux attach -t ${SERVED.session}`);
  });

  it('says where the code was served from', () => {
    // Two checkouts of `probe` have the same name and the same version. The path is the only thing
    // that says which one is now running.
    expect(html).toContain('/srv/checkout/examples/python/probe');
  });
});

/**
 * A WORKER IS ALREADY THERE — the state that is neither success nor failure.
 *
 * The trap it exists for: a worker holds the code it imported at BOOT. An operator edits `actor.py`,
 * saves, presses Serve, and is told "already serving" — which is true, and the most misleading true
 * sentence on this surface, because their edit is not running and everything on screen says the
 * Actor is up. So it is drawn as a question with the answer attached, not as an error, and the
 * answer says what it costs.
 */
describe('when a worker is already serving', () => {
  const ALREADY =
    'a worker is already serving probe in tmux session probe-0_1_0. Restart it to pick up code you have edited';

  it('offers restart rather than reporting a failure', () => {
    const html = draw({ serving: ALREADY });
    expect(html).toContain('data-testid="actor-serve-serving"');
    expect(html).toContain('data-testid="actor-restart-button"');
    expect(html).not.toContain('data-testid="actor-serve-error"');
  });

  it('says the edit is the reason to press it', () => {
    // Without this the button reads as a pointless restart of something already working.
    expect(draw({ serving: ALREADY })).toContain('pick up code you have edited');
  });

  it('draws nothing when no worker is in the way', () => {
    expect(draw()).not.toContain('data-testid="actor-serve-serving"');
    expect(draw()).not.toContain('data-testid="actor-restart-button"');
  });

  it('disables the restart while any work is in flight', () => {
    // A restart racing a save would kill the worker and start a new one on the file as it is on
    // disk — the version being replaced.
    expect(draw({ serving: ALREADY, busy: 'save' })).toContain('disabled');
  });
});

describe('when the serve is refused', () => {
  it('shows the CLI’s own last lines', () => {
    const html = draw({
      error:
        'serve the actor failed: 400 Bad Request — serve failed (exit 1): ' +
        "ModuleNotFoundError: No module named 'httpx'",
    });
    expect(html).toContain('data-testid="actor-serve-error"');
    expect(html).toContain('ModuleNotFoundError');
  });

  it('shows a folder that went away as that, and keeps the button', () => {
    // The fix is to put the directory back or forget the registration — neither is discoverable
    // from "could not serve", and both are from the path.
    const html = draw({
      error:
        'serve the actor failed: 400 Bad Request — /srv/checkout/examples/python/probe is not on ' +
        'this machine any more — put the folder back, or forget the registration',
    });
    expect(html).toContain('is not on this machine any more');
    expect(html).toContain('data-testid="actor-serve-button"');
  });
});

describe('while the workbench is working', () => {
  it('says a serve is running', () => {
    expect(draw({ busy: 'serve' })).toContain('Serving…');
  });

  it('is disabled by a SAVE too, not only by its own serve', () => {
    // A serve that overtook a save in flight would start a worker on the file as it is on disk —
    // the version the operator is in the middle of replacing — and the pane would then show a
    // traceback from code that is no longer in the editor.
    const html = draw({ busy: 'save' });
    expect(html).toContain('disabled');
    expect(html).not.toContain('Serving…');
  });
});

describe('what this console cannot do', () => {
  it('has no control that chooses where the worker runs', () => {
    // No mode picker in any of its states: not a select, not a set of radios, not a second button.
    for (const html of [draw(), draw({ served: SERVED }), draw({ error: 'serve failed (exit 1)' })]) {
      expect(html).not.toContain('<select');
      expect(html).not.toContain('<option');
      expect(html).not.toContain('type="radio"');
      // One button, and it is Serve.
      expect(html.match(/<button/g) ?? []).toHaveLength(1);
    }
  });

  it('never offers a placement that costs money', () => {
    // The words are the giveaway: `--mode docker` and `--mode fleet` are the two placements this
    // page must not reach, and the only `--mode` on it is the local one the server passes.
    const html = draw({ served: SERVED });
    expect(html).not.toMatch(/docker/i);
    expect(html).not.toMatch(/fleet/i);
    expect(html.match(/--mode/g) ?? []).toHaveLength(1);
    expect(html).toContain('--mode local');
  });
});
