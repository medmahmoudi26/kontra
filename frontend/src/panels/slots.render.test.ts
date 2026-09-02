/**
 * The two slot surfaces, drawn in every state a slot can be in.
 *
 * Node, no jsdom, no testing-library — both take rows as props, so unbound / bound / revoked /
 * destroyed / undeclared-refusal are renders and not mocks.
 *
 * THE ASSERTION THAT MATTERS MOST IS AGAIN A NEGATIVE ONE, for `secretsSection.render.test.ts`'s
 * reason: nothing on either surface may carry a credential. There is no route that would hand one
 * to a browser, so a sentinel appearing in this markup would mean somebody added a field to a type
 * that exists specifically not to have one.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import type { ActorSlots, BindableSecret, Resolution, SlotBinding, SlotStatus } from '../run/api';
import { SlotBindingSurface, type SlotBindingSurfaceProps } from './SlotBindings';
import { SlotStrip } from './SlotStrip';
import { bindingGroups, resolutionRows } from './slots';

const SENTINEL = 'sk_live_SENTINEL_never_in_a_browser_c0de';
const AUG = (day: number): number => Date.UTC(2026, 7, day, 9, 14, 0);

/** The four states, as the server derives them (`secrets/slots.ts:slotStatuses`). */
const UNBOUND: SlotStatus = {
  slot: 'api_key',
  description: 'the vendor key this actor calls with',
  state: 'unbound',
  detail: 'declared, and nothing is bound to it — this actor will ask and there is no answer',
};
const BOUND: SlotStatus = {
  slot: 'api_key',
  state: 'bound',
  secret: 'stripe-prod',
  secretVersion: 3,
  boundAt: AUG(20),
  detail: 'bound to "stripe-prod", version 3',
};
const REVOKED: SlotStatus = {
  slot: 'api_key',
  state: 'revoked',
  secret: 'stripe-prod',
  boundAt: AUG(20),
  detail: 'bound to "stripe-prod", whose every version is revoked — write a new version to make it usable',
};
const MISSING: SlotStatus = {
  slot: 'api_key',
  state: 'missing',
  secret: 'stripe-prod',
  boundAt: AUG(20),
  detail: 'bound to "stripe-prod", which no longer exists — the grant stands, the secret was destroyed',
};

const view = (slots: SlotStatus[], over: Partial<ActorSlots> = {}): ActorSlots => ({
  actor: 'probe',
  version: '0.2.0',
  slots,
  added: [],
  ...over,
});

const strip = (v: ActorSlots): string => renderToStaticMarkup(createElement(SlotStrip, { view: v }));

// ── the Actors page's strip ──────────────────────────────────────────────────────────────────

describe('what an Actor will ask for, on its card', () => {
  it('draws DECLARED-AND-UNBOUND, with the author’s sentence and where the fix is', () => {
    const html = strip(view([UNBOUND]));
    expect(html).toContain('data-state="unbound"');
    expect(html).toContain('api_key');
    expect(html).toContain('the vendor key this actor calls with');
    expect(html).toContain('not bound');
    expect(html).toMatch(/Settings/);
    expect(html).toContain('data-blocking="1"');
  });

  it('draws BOUND, naming the secret and the version that answers', () => {
    const html = strip(view([BOUND]));
    expect(html).toContain('data-state="bound"');
    expect(html).toContain('stripe-prod');
    expect(html).toContain('v3');
    expect(html).toContain('data-blocking="0"');
    // A bound slot's detail is not a warning, so the fix line is not drawn.
    expect(html).not.toMatch(/Bind these in Settings/);
  });

  it('draws REVOKED as revoked — not as unbound', () => {
    const html = strip(view([REVOKED]));
    expect(html).toContain('data-state="revoked"');
    expect(html).toContain('revoked');
    expect(html).toContain('write a new version');
    expect(html).not.toContain('data-state="unbound"');
  });

  it('draws a DESTROYED secret as its own state', () => {
    const html = strip(view([MISSING]));
    expect(html).toContain('data-state="missing"');
    expect(html).toContain('secret gone');
    expect(html).toContain('no longer exists');
  });

  it('SURFACES A NEW VERSION’S NEW SLOT AS A CHANGE, on the version that made it', () => {
    const html = strip(view([BOUND, { ...UNBOUND, slot: 'webhook_secret' }], {
      added: ['webhook_secret'],
      comparedWith: '0.1.0',
    }));
    expect(html).toContain('data-testid="slots-probe-diff"');
    expect(html).toContain('webhook_secret');
    expect(html).toContain('0.1.0');
  });

  it('draws NO change strip for a first version — there was nothing to compare against', () => {
    const html = strip(view([UNBOUND], { added: ['api_key'] }));
    expect(html).not.toContain('data-testid="slots-probe-diff"');
  });

  it('draws NOTHING for an actor that asks for no credentials', () => {
    expect(strip(view([]))).toBe('');
  });
});

// ── Settings' binding view ───────────────────────────────────────────────────────────────────

const SECRETS: BindableSecret[] = [
  { name: 'stripe-prod', usable: true },
  { name: 'probe-own', owner: 'actor:probe', usable: true },
];

function bindings(
  actors: ActorSlots[],
  grants: SlotBinding[] = [],
  resolutions: Resolution[] = [],
  over: Partial<SlotBindingSurfaceProps> = {}
): string {
  const props: SlotBindingSurfaceProps = {
    groups: bindingGroups(actors, grants),
    secrets: SECRETS,
    resolutions: resolutionRows(resolutions),
    error: null,
    notice: null,
    onBind: () => undefined,
    onUnbind: () => undefined,
    ...over,
  };
  return renderToStaticMarkup(createElement(SlotBindingSurface, props));
}

describe('Settings — credential bindings', () => {
  it('says what a slot IS before there is a single one', () => {
    const html = bindings([]);
    expect(html).toContain('data-testid="slot-bindings-empty"');
    // EMPTY IS A STATE, NOT A BLANK: the page has to teach the concept, because an operator who
    // does not know what a slot is cannot tell "none declared" from "broken".
    expect(html).toMatch(/declared a credential slot/);
    expect(html).toMatch(/api_key/);
  });

  it('offers a picker over the operator’s secrets, per slot', () => {
    const html = bindings([view([UNBOUND])]);
    expect(html).toContain('data-testid="binding-probe-api_key-select"');
    expect(html).toContain('stripe-prod');
    expect(html).toContain('actor:probe');
  });

  it('draws all four states with their own detail', () => {
    for (const [state, row] of [
      ['unbound', UNBOUND],
      ['bound', BOUND],
      ['revoked', REVOKED],
      ['missing', MISSING],
    ] as const) {
      const html = bindings([view([row])]);
      expect(html).toContain(`data-state="${state}"`);
      expect(html).toContain('data-testid="binding-probe-api_key-detail"');
    }
  });

  it('offers a withdrawal only where there is a grant to withdraw', () => {
    expect(bindings([view([BOUND])])).toContain('data-testid="binding-probe-api_key-unbind"');
    expect(bindings([view([UNBOUND])])).not.toContain('data-testid="binding-probe-api_key-unbind"');
  });

  it('DRAWS A GRANT NO REGISTERED VERSION ASKS FOR, rather than keeping it invisible', () => {
    const html = bindings(
      [view([BOUND])],
      [{ actor: 'probe', slot: 'gone_in_0_2', secret: 'stripe-prod', boundAt: AUG(20) }]
    );
    expect(html).toContain('data-testid="slot-actor-probe-unused"');
    expect(html).toContain('gone_in_0_2');
  });

  it('shows an actor that has grants and has never registered', () => {
    const html = bindings([], [{ actor: 'probe', slot: 'api_key', secret: 'stripe-prod', boundAt: AUG(20) }]);
    expect(html).toContain('data-testid="slot-actor-probe"');
    expect(html).toContain('not registered');
  });

  it('says there are no secrets to bind to, rather than offering an empty picker', () => {
    const html = bindings([view([UNBOUND])], [], [], { secrets: [] });
    expect(html).toContain('no secrets yet');
  });

  it('answers "which actor read my key, and when" — and marks the refusals', () => {
    const html = bindings(
      [view([BOUND])],
      [],
      [
        {
          at: AUG(25),
          actor: 'probe',
          version: '0.2.0',
          slot: 'api_key',
          run: 'nscheck-17',
          secret: 'stripe-prod',
          secretVersion: 3,
          outcome: 'resolved',
        },
        {
          at: AUG(24),
          actor: 'scanner',
          version: '0.1.0',
          slot: 'sneaky',
          run: '',
          secret: '',
          secretVersion: 0,
          outcome: 'undeclared',
        },
      ]
    );
    expect(html).toContain('data-outcome="resolved"');
    expect(html).toContain('2026-08-25 09:14');
    expect(html).toContain('nscheck-17');
    // THE REFUSAL IS THE HALF WORTH HAVING: an actor asking for a slot it never declared is the
    // event this whole design exists to refuse, and a ledger of successes only goes quiet exactly
    // when it matters.
    expect(html).toContain('data-outcome="undeclared"');
    expect(html).toMatch(/not declared/);
  });

  it('has a sentence for a ledger nothing has written to yet', () => {
    expect(bindings([view([BOUND])])).toContain('data-testid="slot-ledger-empty"');
  });
});

// ── the sweep ────────────────────────────────────────────────────────────────────────────────

describe('THE SWEEP: no credential can reach either surface', () => {
  it('renders every state with no value anywhere, and offers no field one could be typed into', () => {
    // The honest form of this check. Neither surface has a route to a value — the resolve route
    // answers an ACTOR authenticated as itself, and its answer does not even carry the secret's
    // NAME — so what is asserted is that a full render of every state carries no credential and
    // no control that would hold one. A "reveal" affordance added later has to delete this.
    const html = [
      strip(view([BOUND, REVOKED, MISSING, UNBOUND], { added: ['api_key'], comparedWith: '0.1.0' })),
      bindings(
        [view([BOUND, UNBOUND])],
        [{ actor: 'probe', slot: 'gone', secret: 'stripe-prod', boundAt: AUG(20) }],
        [
          {
            at: AUG(25),
            actor: 'probe',
            version: '0.2.0',
            slot: 'api_key',
            run: 'r-1',
            secret: 'stripe-prod',
            secretVersion: 3,
            outcome: 'resolved',
          },
          {
            at: AUG(24),
            actor: 'probe',
            version: '0.2.0',
            slot: 'api_key',
            run: 'r-0',
            secret: 'stripe-prod',
            secretVersion: 0,
            outcome: 'revoked',
          },
        ],
        { notice: 'probe\u2019s "api_key" now resolves "stripe-prod"', error: 'could not bind' }
      ),
    ].join('\n');
    expect(html).not.toContain(SENTINEL);
    expect(html).not.toMatch(/type="password"/);
    // No text input at all: everything on the binding surface is a picker over names that already
    // exist, so there is no box a credential could be pasted into by an operator who misread it.
    expect(html).not.toMatch(/<input/);
  });
});
