/**
 * The Monitor's filter row: three menus, a box, a toggle, and a count.
 *
 * ONE ROW, and the order is the order the questions get asked: which actor, which address, which
 * session, then anything else. The count on the right is the honest part — it says how many of how
 * many, so a filter that quietly hides eight Machines cannot be mistaken for a Fleet that has eight
 * fewer Machines than it did a minute ago. That confusion is the only real risk a filter introduces
 * on this surface, and it is why the count is never conditional.
 *
 * The menus are built from the inventory (see `paneFilter.ts`), so they never offer a value nothing
 * has and never omit one something does.
 */

import { ANY, filterActive, type PaneFilter } from '../paneFilter';

export interface PaneFilterBarProps {
  filter: PaneFilter;
  options: { actors: string[]; ips: string[]; sessions: string[] };
  onChange(next: PaneFilter): void;
  shown: number;
  total: number;
}

export default function PaneFilterBar({
  filter,
  options,
  onChange,
  shown,
  total,
}: PaneFilterBarProps): JSX.Element {
  const active = filterActive(filter);
  const set = <K extends keyof PaneFilter>(key: K, value: PaneFilter[K]): void =>
    onChange({ ...filter, [key]: value });

  return (
    <div className="flex flex-wrap items-center gap-1.5" data-testid="pane-filter">
      <span className="whitespace-nowrap font-mono text-[10px] uppercase tracking-widest text-muted-foreground">
        Show
      </span>

      <Menu
        testid="filter-actor"
        label="all actors"
        value={filter.actor}
        options={options.actors}
        onChange={(v) => set('actor', v)}
      />
      <Menu
        testid="filter-ip"
        label="all addresses"
        value={filter.ip}
        options={options.ips}
        onChange={(v) => set('ip', v)}
      />
      <Menu
        testid="filter-session"
        label="all sessions"
        value={filter.session}
        options={options.sessions}
        onChange={(v) => set('session', v)}
      />

      <input
        data-testid="filter-query"
        value={filter.query}
        onChange={(e) => set('query', e.target.value)}
        placeholder="host, ip, session…"
        spellCheck={false}
        aria-label="filter terminals"
        className="w-44 rounded-lg border border-border bg-muted px-2.5 py-1 font-mono text-[11px] outline-none focus:border-primary/60"
      />

      <button
        type="button"
        data-testid="filter-live-only"
        data-active={filter.liveOnly ? 'true' : undefined}
        onClick={() => set('liveOnly', !filter.liveOnly)}
        title="only Terminals holding a real PTY attach — the expensive ones, and the only ones whose content is moving"
        className={`whitespace-nowrap rounded-full border border-border px-2.5 py-1 font-mono text-[10px] uppercase tracking-wide ${
          filter.liveOnly ? 'bg-emerald-500/15 text-emerald-500' : 'text-muted-foreground'
        }`}
      >
        live only
      </button>

      {active && (
        <button
          type="button"
          data-testid="filter-clear"
          onClick={() =>
            onChange({ actor: ANY, ip: ANY, session: ANY, query: '', liveOnly: false })
          }
          className="whitespace-nowrap rounded-full border border-border px-2.5 py-1 font-mono text-[10px] text-muted-foreground hover:text-foreground"
        >
          clear ✕
        </button>
      )}

      <span
        className="ml-auto whitespace-nowrap font-mono text-[10.5px] tabular-nums text-muted-foreground"
        data-testid="filter-count"
        data-shown={shown}
        data-total={total}
      >
        {/* Never conditional. A filter that hides eight Machines must not read like a Fleet that
            lost eight Machines, and the only defence against that is saying both numbers always. */}
        showing {shown} of {total} panes
      </span>
    </div>
  );
}

function Menu({
  testid,
  label,
  value,
  options,
  onChange,
}: {
  testid: string;
  /** What the "no narrowing" option reads as — `all actors`, not `any` or a blank. */
  label: string;
  value: string;
  options: string[];
  onChange(v: string): void;
}): JSX.Element {
  return (
    <select
      data-testid={testid}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      aria-label={label}
      className="max-w-48 rounded-lg border border-border bg-muted px-2 py-1 font-mono text-[11px] outline-none"
    >
      <option value={ANY}>{label}</option>
      {options.map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
    </select>
  );
}
