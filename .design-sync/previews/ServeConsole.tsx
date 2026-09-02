import { ServeConsole } from '@kontra/frontend';

/**
 * The console sits under the editor and the worker's pane, in a `p-3` scroll region that is a
 * fraction of the workbench's width — never the full page. The cap keeps a card at the shape an
 * operator reads it in.
 */
function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground" style={{ maxWidth: 592 }}>
        {children}
      </div>
    </div>
  );
}

const PROBE = {
  id: 'actor:probe:1f3k',
  kind: 'actor' as const,
  name: 'probe',
  path: '/srv/checkout/examples/python/probe',
  version: '0.2.0',
  description: '',
  registeredAt: 1_752_000_000_000,
};

/** What `POST /api/sources/actor/:id/serve` answers with — `actorSession`'s own string, minted
 *  beside the session it names. tmux rewrites `.` to `_`, which is why the version reads `0_2_0`. */
const SERVED = {
  actor: 'probe',
  version: '0.2.0',
  path: PROBE.path,
  session: 'probe-0_2_0',
  attach: 'tmux attach -t probe-0_2_0',
};

const noop = () => {};

/**
 * Nothing has been served from here, and the session is named anyway — DERIVED from the folder, not
 * left blank. An Actor served an hour ago, or from a terminal with `kontra serve --actor … --tmux`,
 * has a worker and a pane and no result in this component's state; a console with no name beside an
 * empty pane leaves nothing to check with `tmux ls`.
 */
export function BeforeAnythingIsServed() {
  return (
    <Frame>
      <ServeConsole
        source={PROBE}
        session="probe-0_2_0"
        served={null}
        error={null}
        busy={null}
        onServe={noop}
      />
    </Frame>
  );
}

/**
 * After a serve. The session and the attach command are the whole report because they are what an
 * operator does next: the pane above is a SNAPSHOT (ADR 0020), so reading a long traceback or typing
 * into the worker means attaching in a terminal. `from` is there because two checkouts of `probe`
 * hold the same name and the same version, and the path is the only thing that says which one is
 * now running.
 */
export function Served() {
  return (
    <Frame>
      <ServeConsole
        source={PROBE}
        session={SERVED.session}
        served={SERVED}
        error={null}
        busy={null}
        onServe={noop}
      />
    </Frame>
  );
}

/**
 * The failure this whole surface exists for: the file saved, the serve returned, and the worker died
 * at import. The refusal carries the CLI's OWN last lines — "could not serve" would send an operator
 * to a terminal to run the same command and read the same output by hand.
 */
export function WorkerDiedAtImport() {
  return (
    <Frame>
      <ServeConsole
        source={PROBE}
        session="probe-0_2_0"
        served={null}
        error={
          'serve the actor failed: 400 Bad Request — serve failed (exit 1):\n' +
          '  File "/srv/checkout/examples/python/probe/actor.py", line 14, in <module>\n' +
          '    import httpx\n' +
          "ModuleNotFoundError: No module named 'httpx'"
        }
        busy={null}
        onServe={noop}
      />
    </Frame>
  );
}

/**
 * The registration outlived its folder. The fix is to put the directory back or forget the
 * registration, and neither is discoverable from "could not serve" — both are from the path, which
 * is why the server's sentence is printed verbatim and the button stays live.
 */
export function FolderWentAway() {
  return (
    <Frame>
      <ServeConsole
        source={PROBE}
        session="probe-0_2_0"
        served={null}
        error={
          'serve the actor failed: 400 Bad Request — /srv/checkout/examples/python/probe is not on ' +
          'this machine any more — put the folder back, or forget the registration'
        }
        busy={null}
        onServe={noop}
      />
    </Frame>
  );
}

/**
 * A SAVE is in flight, and it disables Serve — the button reads `Serve`, not `Serving…`, because
 * nothing is being served. A serve that overtook a save would start a worker on the file as it is on
 * DISK, the version the operator is in the middle of replacing, and the pane would then show a
 * traceback from code that is no longer in the editor.
 *
 * (`Serving…`, the sub-second state of this same button, cannot be captured statically any other
 * way than by holding `busy='serve'`; it is one word's difference from this cell.)
 */
export function BlockedByASaveInFlight() {
  return (
    <Frame>
      <ServeConsole
        source={PROBE}
        session="probe-0_2_0"
        served={null}
        error={null}
        busy="save"
        onServe={noop}
      />
    </Frame>
  );
}
