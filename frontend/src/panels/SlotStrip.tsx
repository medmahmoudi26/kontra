/**
 * What an Actor will ask for, on its card — BEFORE it runs.
 *
 * This is half the point of the whole slice. An actor's Methods and their schemas say what it
 * takes; nothing on this page said what it will ASK FOR, so the first time an operator learned
 * that a third-party actor wanted a credential was when a run failed inside `@actor.load`. The
 * strip below is that fact, on the surface where somebody is already deciding whether to serve
 * this code.
 *
 * PROPS IN, MARKUP OUT. It takes a view and draws it, so unbound / bound / revoked / gone are four
 * renders and no mocks (`slots.render.test.ts`), and the card that hosts it needs no fetch.
 *
 * NOTHING HERE IS A CONTROL. Binding happens in Settings, beside the secrets it binds to — a
 * picker on this card would need the operator's whole secret inventory on the Actors page, which
 * is a list of credential names on a screen opened to read code. The strip states the situation
 * and says where the fix is.
 */

import type { ActorSlots, SlotState } from '../run/api';
import { SLOT_WORD, blocking, summariseSlots, versionDiff } from './slots';

const STATE_BADGE: Record<SlotState, string> = {
  bound: 'bg-emerald-500/15 text-emerald-300',
  unbound: 'bg-amber-500/15 text-amber-300',
  revoked: 'bg-rose-500/15 text-rose-300',
  missing: 'bg-rose-500/15 text-rose-300',
};

export function SlotStrip({ view }: { view: ActorSlots }): JSX.Element | null {
  /* NO SLOTS, NO STRIP. Most actors ask for no credentials, and "asks for 0 credentials" on every
     card would be a line of noise on the page this system's inventory is read from. The absence is
     unambiguous here in a way it is not for a schema: a Method with no declared input might be
     undeclared, whereas an actor that declared no slot cannot ask for one at all — the resolve
     route refuses it. */
  if (view.slots.length === 0) return null;

  const bad = blocking(view.slots);
  const diff = versionDiff(view);

  return (
    <div
      className="flex flex-col gap-1 rounded-md border border-border bg-background/40 p-2"
      data-testid={`slots-${view.actor}`}
      data-blocking={bad.length}
    >
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-[11px] font-semibold">Credentials</span>
        <span className="text-[10px] text-muted-foreground">{view.version}</span>
      </div>

      <p className="m-0 text-[11px] text-muted-foreground" data-testid={`slots-${view.actor}-summary`}>
        {summariseSlots(view)}
      </p>

      {diff && (
        /* A NEW SLOT IN A NEW VERSION IS A CHANGE, drawn on the version that made it — the same
           place, and for the same reason, that a cross-version schema finding is drawn. An actor
           that quietly grew an appetite between builds is exactly the change nobody notices. */
        <p className="m-0 text-[11px] text-amber-300" data-testid={`slots-${view.actor}-diff`}>
          {diff}
        </p>
      )}

      <ul className="m-0 flex list-none flex-col gap-1 p-0" data-testid={`slots-${view.actor}-list`}>
        {view.slots.map((s) => (
          <li
            key={s.slot}
            data-testid={`slot-${view.actor}-${s.slot}`}
            data-state={s.state}
            className="flex flex-col gap-0.5"
          >
            <span className="flex items-baseline gap-2">
              <span className="font-mono text-[11px]">{s.slot}</span>
              <span className={`rounded-full px-2 py-0.5 text-[10px] ${STATE_BADGE[s.state]}`}>
                {SLOT_WORD[s.state]}
              </span>
              {s.secret && (
                /* THE OPERATOR SEES WHICH SECRET, and the ACTOR never does — the resolve route
                   answers with the slot and the value alone. This is the operator's console. */
                <span className="font-mono text-[10px] text-muted-foreground">
                  → {s.secret}
                  {s.secretVersion ? ` v${s.secretVersion}` : ''}
                </span>
              )}
            </span>
            {s.description && (
              <span className="text-[10px] text-muted-foreground">{s.description}</span>
            )}
            {s.state !== 'bound' && (
              <span className="text-[10px] text-amber-300" data-testid={`slot-${view.actor}-${s.slot}-detail`}>
                {s.detail}
              </span>
            )}
          </li>
        ))}
      </ul>

      {bad.length > 0 && (
        <p className="m-0 text-[10px] text-muted-foreground" data-testid={`slots-${view.actor}-fix`}>
          Bind these in Settings → Credential bindings.
        </p>
      )}
    </div>
  );
}
