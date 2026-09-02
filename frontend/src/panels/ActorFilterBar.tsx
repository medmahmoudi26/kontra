/**
 * The Actors page's filter row: a menu, a box, two toggles, and a count.
 *
 * ONE ROW, in the order the questions get asked: which Actor, then anything else, then the two
 * narrowings that are about STATE rather than identity. It is the Monitor's bar with this page's
 * nouns — deliberately, because an operator who learns one filter should not have to learn a
 * second grammar on the next surface.
 *
 * THE COUNT IS NEVER CONDITIONAL. It says how many of how many even when nothing is filtered,
 * because the risk a filter introduces on a catalog is that hiding eight Actors reads as a catalog
 * that lost eight Actors — and saying both numbers always is the only defence.
 */

import { ANY, actorFilterActive, EMPTY_ACTOR_FILTER, type ActorFilter } from './actorFilter';

export interface ActorFilterBarProps {
  filter: ActorFilter;
  /** Actor names, deduped across versions — see `actorNames`. */
  names: string[];
  onChange(next: ActorFilter): void;
  shown: number;
  total: number;
}

export function ActorFilterBar({
  filter,
  names,
  onChange,
  shown,
  total,
}: ActorFilterBarProps): JSX.Element {
  const set = <K extends keyof ActorFilter>(key: K, value: ActorFilter[K]): void =>
    onChange({ ...filter, [key]: value });

  return (
    // `font-mono text-[11px]` on the ROW, not on the controls. An unlayered
    // `input[type=…], select, textarea { font: inherit }` rule in styles.css outranks Tailwind, so
    // a font set directly on the `<select>` does nothing — it has to be inherited from a parent.
    <div
      className="flex flex-wrap items-center gap-1.5 font-mono text-[11px]"
      data-testid="actor-filter"
    >
      <span className="whitespace-nowrap text-[10px] uppercase tracking-widest text-muted-foreground">
        Show
      </span>

      <select
        data-testid="actor-filter-name"
        value={filter.name}
        onChange={(e) => set('name', e.target.value)}
        aria-label="all actors"
        className="max-w-48 rounded-lg border border-border bg-muted px-2 py-1 outline-none"
      >
        <option value={ANY}>all actors</option>
        {names.map((n) => (
          <option key={n} value={n}>
            {n}
          </option>
        ))}
      </select>

      <input
        data-testid="actor-filter-query"
        value={filter.query}
        onChange={(e) => set('query', e.target.value)}
        placeholder="name, version, method, path…"
        spellCheck={false}
        aria-label="filter actors"
        className="w-56 rounded-lg border border-border bg-muted px-2.5 py-1 outline-none focus:border-primary/60"
      />

      <Toggle
        testid="actor-filter-serving"
        on={filter.servingOnly}
        onClick={() => set('servingOnly', !filter.servingOnly)}
        title="only Actors a Machine is actually polling for — a Machine whose health probe is unhappy is not serving, whatever the catalog says"
        tone="bg-emerald-500/15 text-emerald-500"
      >
        serving
      </Toggle>

      <Toggle
        testid="actor-filter-ondisk"
        on={filter.onDiskOnly}
        onClick={() => set('onDiskOnly', !filter.onDiskOnly)}
        title="only Actors whose code is on this disk — the ones the workbench can open and serve"
        tone="bg-sky-500/15 text-sky-500"
      >
        on this disk
      </Toggle>

      {actorFilterActive(filter) && (
        <button
          type="button"
          data-testid="actor-filter-clear"
          onClick={() => onChange(EMPTY_ACTOR_FILTER)}
          className="whitespace-nowrap rounded-full border border-border px-2.5 py-1 text-[10px] text-muted-foreground hover:text-foreground"
        >
          clear ✕
        </button>
      )}

      <span
        className="ml-auto whitespace-nowrap text-[10.5px] tabular-nums text-muted-foreground"
        data-testid="actor-filter-count"
        data-shown={shown}
        data-total={total}
      >
        showing {shown} of {total} {total === 1 ? 'actor' : 'actors'}
      </span>
    </div>
  );
}

function Toggle({
  testid,
  on,
  onClick,
  title,
  tone,
  children,
}: {
  testid: string;
  on: boolean;
  onClick(): void;
  title: string;
  /** The colour it takes when it is ON. Off is always muted — a toggle that looked engaged while
   *  it was not would be a filter nobody knew was applied. */
  tone: string;
  children: React.ReactNode;
}): JSX.Element {
  return (
    <button
      type="button"
      data-testid={testid}
      data-active={on ? 'true' : undefined}
      onClick={onClick}
      title={title}
      className={`whitespace-nowrap rounded-full border border-border px-2.5 py-1 text-[10px] uppercase tracking-wide ${
        on ? tone : 'text-muted-foreground'
      }`}
    >
      {children}
    </button>
  );
}
