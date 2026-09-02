import { PaneFilterBar } from '@kontra/frontend';

/**
 * `font-mono text-[11px]` sits on the PARENT, not on the controls. An unlayered
 * `select { font: inherit }` rule in `styles.css` outranks Tailwind, so a family set on a
 * `<select>` does nothing and the menus fall back to whatever the page inherits.
 */
function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 font-mono text-[11px] text-foreground">
        {children}
      </div>
    </div>
  );
}

/** The sentinel `ANY` from `paneFilter.ts` — "do not narrow on this", not the empty string. */
const ANY = '*';

/**
 * The menus are built from the inventory by `paneOptions`, so they offer exactly the values
 * something on the wall has: three actors (one Terminal holds no placement yet), the four
 * fleet addresses plus `local` for a `kontra actor serve … --tmux` on this host, and the tmux
 * sessions the ids carry.
 */
const OPTIONS = {
  actors: ['probe', 'subfinder', 'unregistered'],
  ips: ['10.124.0.4', '10.124.0.5', '10.124.0.6', '10.124.0.7', 'local'],
  sessions: ['kontra-probe', 'kontra-serve-probe', 'kontra-subfinder'],
};

const noop = () => {};

/** A ten-Machine sweep, one window per Machine per actor. */
const TOTAL = 34;

/**
 * Nothing narrowed — the row an operator meets. Every menu reads `all actors` / `all
 * addresses` / `all sessions` rather than blank or `any`, and there is no clear button,
 * because there is nothing to clear. The count still says both numbers.
 */
export function NothingNarrowed() {
  return (
    <Frame>
      <PaneFilterBar
        filter={{ actor: ANY, ip: ANY, session: ANY, query: '', liveOnly: false }}
        options={OPTIONS}
        onChange={noop}
        shown={TOTAL}
        total={TOTAL}
      />
    </Frame>
  );
}

/**
 * The first question the row exists for: which Machines are running THIS actor. One menu
 * moved, the clear button has appeared, and the count went to 11 of 34 — the reading that
 * stops a filtered wall from looking like a Fleet that lost 23 panes.
 */
export function NarrowedToOneActor() {
  return (
    <Frame>
      <PaneFilterBar
        filter={{ actor: 'subfinder', ip: ANY, session: ANY, query: '', liveOnly: false }}
        options={OPTIONS}
        onChange={noop}
        shown={11}
        total={TOTAL}
      />
    </Frame>
  );
}

/**
 * `live only` is a toggle, not a menu, and it is the only control here that is about the
 * PAGE rather than the inventory: which Terminals hold a real PTY attach. Three of 34, which
 * is what a wall inside the live budget looks like.
 */
export function LiveOnly() {
  return (
    <Frame>
      <PaneFilterBar
        filter={{ actor: ANY, ip: ANY, session: ANY, query: '', liveOnly: true }}
        options={OPTIONS}
        onChange={noop}
        shown={3}
        total={TOTAL}
      />
    </Frame>
  );
}

/**
 * Every clause at once, which is the reading one row of controls implies: actor AND address
 * AND session AND the free-text box AND live. The box is the fifth question nobody
 * anticipated — it searches every field, so `apex` matches the fleet tag without the operator
 * having to know which field their fragment lives in. This is also the row at its widest: it
 * wraps rather than truncating, and the count stays pinned right.
 */
export function EveryClauseAtOnce() {
  return (
    <Frame>
      <PaneFilterBar
        filter={{
          actor: 'subfinder',
          ip: '10.124.0.6',
          session: 'kontra-subfinder',
          query: 'apex',
          liveOnly: true,
        }}
        options={OPTIONS}
        onChange={noop}
        shown={1}
        total={TOTAL}
      />
    </Frame>
  );
}

/**
 * A filter that matches nothing. THE COUNT IS NEVER CONDITIONAL, and this is the case that
 * pays for it: an empty wall reading `showing 0 of 34 panes` is a filter with no matches,
 * which is fixed by the clear button beside it. The same wall with no count would read as a
 * Fleet that went away.
 */
export function MatchesNothing() {
  return (
    <Frame>
      <PaneFilterBar
        filter={{ actor: 'probe', ip: ANY, session: 'kontra-subfinder', query: '', liveOnly: false }}
        options={OPTIONS}
        onChange={noop}
        shown={0}
        total={TOTAL}
      />
    </Frame>
  );
}
