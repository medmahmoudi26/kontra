/**
 * Settings → Credential bindings: where an operator GRANTS, and where they read back who used it.
 *
 * TWO HALVES, AND THE SECOND ONE IS THE POINT. Binding a slot is a small act; being able to answer
 * "which actor read my key, and when" months later is what turns that act into a decision the
 * operator made rather than one they discovered. The ledger is therefore on this page, under the
 * grants, not tucked behind a filter somewhere else.
 *
 * IT IS BESIDE THE SECRETS, NOT ON THE ACTORS PAGE, and that is a placement decision rather than a
 * layout one: binding needs the operator's whole secret inventory in a picker, and putting a list
 * of credential names on the page opened to read an actor's code is how a screenshot of a
 * debugging session ends up carrying it. The Actors page states the situation (`SlotStrip.tsx`)
 * and points here.
 *
 * PROPS IN, MARKUP OUT for everything with a state worth pinning: {@link SlotBindingSurface} draws
 * whatever groups it is handed, so unbound, bound, revoked, destroyed, unused-grant and
 * nothing-declared are renders rather than mocks. The fetching lives in {@link useSlotBindings}.
 *
 * AND NOTHING HERE CAN SHOW A VALUE. There is no route that returns one to a browser — resolution
 * answers an ACTOR authenticated as itself, and its answer carries the slot and the value and not
 * even the secret's name. What this page holds are names, states, dates and outcomes.
 */

import { useCallback, useEffect, useState } from 'react';
import { Trash2 } from 'lucide-react';

import {
  bindSlot,
  fetchResolutions,
  fetchSlots,
  unbindSlot,
  type ActorSlots,
  type BindableSecret,
  type Resolution,
  type SlotBinding,
} from '../run/api';
import {
  SLOT_WORD,
  bindingGroups,
  isoDate,
  resolutionRows,
  unbindConfirm,
  type BindingGroup,
  type ResolutionRow,
} from './slots';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';

const STATE_BADGE: Record<string, string> = {
  bound: 'bg-emerald-500/15 text-emerald-300',
  unbound: 'bg-amber-500/15 text-amber-300',
  revoked: 'bg-rose-500/15 text-rose-300',
  missing: 'bg-rose-500/15 text-rose-300',
};

export interface SlotBindingSurfaceProps {
  groups: readonly BindingGroup[];
  /** The names an operator can bind to. Empty means there is nothing in the store yet. */
  secrets: readonly BindableSecret[];
  /** Newest first. Empty is a real state — nothing has resolved anything yet. */
  resolutions: readonly ResolutionRow[];
  /** Why the list is empty, when it is empty because something failed. Never the same as "none". */
  error: string | null;
  /** What the last grant did. Never a value; there is none on this path. */
  notice: string | null;
  /** `actor/slot` of a grant in flight, so its controls can say so. */
  busy?: string | null;
  onBind(actor: string, slot: string, secret: string): void;
  onUnbind(actor: string, slot: string, secret: string): void;
}

export function SlotBindingSurface({
  groups,
  secrets,
  resolutions,
  error,
  notice,
  busy,
  onBind,
  onUnbind,
}: SlotBindingSurfaceProps): JSX.Element {
  return (
    <div className="flex flex-col gap-3" data-testid="slot-bindings">
      <p className="m-0 text-[11px] text-muted-foreground" data-testid="slot-bindings-blurb">
        An actor declares a <strong>slot</strong> — a name of its own, like <code>api_key</code> —
        and you bind it to one of your secrets. Its author never learns which: a resolution answers
        with the slot and the value, never the name you bound. Everything an actor asks for is
        listed here before it runs, and a run whose actor has an unbound slot is refused at the
        start rather than mid-run.
      </p>

      {error && (
        <p className="m-0 text-[11px] text-rose-300" data-testid="slot-bindings-error">
          {error}
        </p>
      )}
      {notice && (
        <p className="m-0 text-[11px] text-emerald-300" data-testid="slot-bindings-notice">
          {notice}
        </p>
      )}

      {groups.length === 0 ? (
        /* EMPTY IS A STATE, NOT A BLANK — `SecretsSection.tsx`'s rule. An operator arriving here
           has to learn that nothing has declared a slot yet, which is a fact about their actors
           rather than a page that failed to load. */
        <p className="m-0 text-[11px] text-muted-foreground" data-testid="slot-bindings-empty">
          No actor has declared a credential slot yet. Serve one that does and it appears here,
          with what it asks for, before it runs.
        </p>
      ) : (
        <ul className="m-0 flex list-none flex-col gap-2 p-0" data-testid="slot-bindings-list">
          {groups.map((group) => (
            <li
              key={group.actor}
              data-testid={`slot-actor-${group.actor}`}
              data-blocking={group.blocking}
              className="flex flex-col gap-1 rounded-md border border-border bg-background/40 p-2"
            >
              <div className="flex items-baseline justify-between gap-2">
                <span className="font-mono text-xs">{group.actor}</span>
                <Badge variant="outline">{group.version || 'not registered'}</Badge>
              </div>

              {group.diff && (
                <p className="m-0 text-[11px] text-amber-300" data-testid={`slot-actor-${group.actor}-diff`}>
                  {group.diff}
                </p>
              )}

              {group.slots.length === 0 && (
                <p className="m-0 text-[11px] text-muted-foreground">
                  Nothing has registered what this actor asks for — the grants below are yours,
                  waiting for it.
                </p>
              )}

              {group.slots.map((s) => {
                const key = `${group.actor}/${s.slot}`;
                return (
                  <div
                    key={s.slot}
                    data-testid={`binding-${group.actor}-${s.slot}`}
                    data-state={s.state}
                    className="flex flex-col gap-1 border-t border-border pt-1"
                  >
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-[11px]">{s.slot}</span>
                      <span className={`rounded-full px-2 py-0.5 text-[10px] ${STATE_BADGE[s.state]}`}>
                        {SLOT_WORD[s.state]}
                      </span>
                      <select
                        className="h-7 rounded-md border border-border bg-background px-1 text-xs"
                        data-testid={`binding-${group.actor}-${s.slot}-select`}
                        disabled={busy === key || secrets.length === 0}
                        value={s.secret ?? ''}
                        onChange={(e) => e.target.value && onBind(group.actor, s.slot, e.target.value)}
                      >
                        <option value="">
                          {secrets.length === 0 ? 'no secrets yet' : 'bind to…'}
                        </option>
                        {secrets.map((sec) => (
                          <option key={sec.name} value={sec.name}>
                            {sec.name}
                            {sec.owner ? ` (${sec.owner})` : ''}
                            {sec.usable ? '' : ' — every version revoked'}
                          </option>
                        ))}
                      </select>
                      {s.secret && (
                        <Button
                          size="sm"
                          variant="ghost"
                          data-testid={`binding-${group.actor}-${s.slot}-unbind`}
                          disabled={busy === key}
                          aria-label={`unbind ${s.slot}`}
                          onClick={() => onUnbind(group.actor, s.slot, s.secret ?? '')}
                        >
                          <Trash2 className="h-3 w-3" />
                        </Button>
                      )}
                    </div>
                    {s.description && (
                      <span className="text-[10px] text-muted-foreground">{s.description}</span>
                    )}
                    <span
                      className={`text-[10px] ${s.state === 'bound' ? 'text-muted-foreground' : 'text-amber-300'}`}
                      data-testid={`binding-${group.actor}-${s.slot}-detail`}
                    >
                      {s.detail}
                    </span>
                  </div>
                );
              })}

              {group.unused.length > 0 && (
                /* DRAWN, NOT HIDDEN. A grant for a slot no declared version asks for is harmless
                   and invisible, which is exactly why it never gets withdrawn — an actor that
                   dropped a slot leaves one behind on every install. */
                <div className="border-t border-border pt-1" data-testid={`slot-actor-${group.actor}-unused`}>
                  <p className="m-0 text-[10px] text-muted-foreground">
                    Granted, and no registered version asks for it:
                  </p>
                  {group.unused.map((b: SlotBinding) => (
                    <div key={b.slot} className="flex items-center gap-2">
                      <span className="font-mono text-[11px]">{b.slot}</span>
                      <span className="font-mono text-[10px] text-muted-foreground">→ {b.secret}</span>
                      <span className="text-[10px] text-muted-foreground">{isoDate(b.boundAt)}</span>
                      <Button
                        size="sm"
                        variant="ghost"
                        data-testid={`binding-${group.actor}-${b.slot}-unbind`}
                        disabled={busy === `${group.actor}/${b.slot}`}
                        aria-label={`unbind ${b.slot}`}
                        onClick={() => onUnbind(group.actor, b.slot, b.secret)}
                      >
                        <Trash2 className="h-3 w-3" />
                      </Button>
                    </div>
                  ))}
                </div>
              )}
            </li>
          ))}
        </ul>
      )}

      <div className="flex flex-col gap-1 border-t border-border pt-2" data-testid="slot-ledger">
        <span className="text-[11px] font-semibold">Which actor read my credential, and when</span>
        {resolutions.length === 0 ? (
          <p className="m-0 text-[11px] text-muted-foreground" data-testid="slot-ledger-empty">
            Nothing has resolved a credential yet. Every read and every refusal lands here — the
            actor, its version, the slot, the run, and which secret answered. Never the value.
          </p>
        ) : (
          <ul className="m-0 flex list-none flex-col gap-0.5 p-0" data-testid="slot-ledger-list">
            {resolutions.map((r, i) => (
              <li
                key={`${r.at}-${r.actor}-${r.slot}-${i}`}
                data-testid={`resolution-${i}`}
                data-outcome={r.outcome}
                className={`flex items-baseline gap-2 text-[10px] ${r.refused ? 'text-amber-300' : 'text-muted-foreground'}`}
              >
                <span className="font-mono">{r.when}</span>
                <span>{r.sentence}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

/** What the section holds, and the two things it can do. Its own hook so the surface stays pure. */
export function useSlotBindings(): SlotBindingSurfaceProps {
  const [actors, setActors] = useState<ActorSlots[]>([]);
  const [bindings, setBindings] = useState<SlotBinding[]>([]);
  const [secrets, setSecrets] = useState<BindableSecret[]>([]);
  const [resolutions, setResolutions] = useState<Resolution[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const reload = useCallback(() => {
    // A 200 that is not this shape is treated as empty rather than trusted — the guard
    // `useSecrets` takes, and for its reason: `undefined.map` would take the whole Settings page
    // down over a section of it.
    void fetchSlots()
      .then((got) => {
        setActors(Array.isArray(got.actors) ? got.actors : []);
        setBindings(Array.isArray(got.bindings) ? got.bindings : []);
        setSecrets(Array.isArray(got.secrets) ? got.secrets : []);
        setError(null);
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)));
    // THE LEDGER IS ITS OWN FETCH AND ITS OWN FAILURE. An audit trail that could not be read must
    // not blank the grants above it — the two answer different questions, and the page is more
    // useful with one of them than with neither.
    void fetchResolutions()
      .then((got) => setResolutions(Array.isArray(got) ? got : []))
      .catch(() => setResolutions([]));
  }, []);

  useEffect(reload, [reload]);

  const onBind = useCallback(
    (actor: string, slot: string, secret: string) => {
      setBusy(`${actor}/${slot}`);
      void bindSlot(actor, slot, secret)
        .then(() => {
          setNotice(`${actor}'s "${slot}" now resolves "${secret}"`);
          setError(null);
          reload();
        })
        .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
        .finally(() => setBusy(null));
    },
    [reload]
  );

  const onUnbind = useCallback(
    (actor: string, slot: string, secret: string) => {
      if (!window.confirm(unbindConfirm({ actor, slot, secret }))) return;
      setBusy(`${actor}/${slot}`);
      void unbindSlot(actor, slot)
        .then(() => {
          setNotice(`${actor}'s "${slot}" is no longer bound — the secret itself is untouched`);
          setError(null);
          reload();
        })
        .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
        .finally(() => setBusy(null));
    },
    [reload]
  );

  return {
    groups: bindingGroups(actors, bindings),
    secrets,
    resolutions: resolutionRows(resolutions),
    error,
    notice,
    busy,
    onBind,
    onUnbind,
  };
}

export default function SlotBindingsSection(): JSX.Element {
  return <SlotBindingSurface {...useSlotBindings()} />;
}
