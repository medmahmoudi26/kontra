/**
 * What a card says about where an Actor's code is — the join between the two inventories on this
 * page, rendered.
 *
 * `renderToStaticMarkup` for the reason `HealthChips.test.ts` records: this suite runs in node with
 * no jsdom and no testing-library, and every assertion here is about text and `data-testid`, both of
 * which are in the markup.
 *
 * A CARD IS A REGISTERED FOLDER, so `folder` is required and there is no "no registered folder"
 * case left to test. The two states that remain are different sentences and collapsing them is the
 * bug: "not on disk" is fixable by putting the directory back, and it is not "here is an editor
 * over nothing".
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { Source } from '../run/api';
import type { CatalogActor } from '../types';
import { ActorCard } from './ActorCard';

const PROBE: CatalogActor = {
  key: 'probe@0.1.0',
  name: 'probe',
  version: '0.1.0',
  schemaVersion: '1',
  operations: [{ name: 'head', description: 'GET each target.' }],
  source: '/srv/checkout/examples/python/probe',
};

function folder(over: Partial<Source> = {}): Source {
  return {
    id: 'actor:probe:1f3k',
    kind: 'actor',
    name: 'probe',
    path: '/srv/checkout/examples/python/probe',
    version: '0.1.0',
    description: '',
    registeredAt: 1_700_000_000_000,
    ...over,
  };
}

const draw = (found: Source, actor: CatalogActor = PROBE): string =>
  renderToStaticMarkup(
    createElement(ActorCard, {
      actor,
      machines: [],
      folder: found,
      open: false,
      onToggle: () => {},
      onEdit: () => {},
      onCall: () => {},
    })
  );

describe('an Actor whose folder is registered', () => {
  it('offers the editor, and names the folder it opens', () => {
    const html = draw(folder());
    expect(html).toContain('data-testid="actor-edit-probe"');
    expect(html).toContain('/srv/checkout/examples/python/probe');
  });

  it('draws the REGISTERED PATH, which is the one the reader controls', () => {
    // Two checkouts of `probe` is the situation this page is opened in, and the answer to "which
    // one is this console holding" cannot be behind a hover.
    const html = draw(folder({ path: '/srv/other/probe' }));
    expect(html).toContain('data-testid="actor-source-probe"');
    expect(html).toContain('/srv/other/probe');
  });

  it('stays quiet about the worker’s path when it is the same path', () => {
    // Printing one directory twice under two labels is the duplication the whole inversion removed.
    expect(draw(folder())).not.toContain('data-testid="actor-loaded-probe"');
  });

  it('says where the RUNNING copy came from when the two disagree', () => {
    // On a fleet Machine `actor.source` is `/opt/kontra/actor/probe` — a path on the Machine. The
    // two differing means the code being edited is not the code that ran, which is a real finding
    // and invisible if the card only ever prints one of them.
    const html = draw(folder(), { ...PROBE, source: '/opt/kontra/actor/probe' });
    expect(html).toContain('data-testid="actor-loaded-probe"');
    expect(html).toContain('/opt/kontra/actor/probe');
  });

  it('says nothing about a worker path that was never recorded', () => {
    // An entry from before registration carried `source`. Absent is "not recorded", and the folder
    // above already answers where the code is — a second sentence would only be noise.
    const { source: _drop, ...noSource } = PROBE;
    expect(draw(folder(), noSource)).not.toContain('data-testid="actor-loaded-probe"');
  });
});

describe('an Actor whose registered folder is gone', () => {
  it('says the folder is missing rather than offering an editor that 400s', () => {
    const html = draw(folder({ absent: true }));
    expect(html).toContain('data-testid="actor-folder-absent-probe"');
    expect(html).not.toContain('data-testid="actor-edit-probe"');
    // The registration is still the operator's, so the path is still named — the fix is putting
    // that directory back, and a sentence without it names nothing to put back.
    expect(html).toContain('/srv/checkout/examples/python/probe');
  });
});

describe('a version that broke a caller', () => {
  /** `probe@0.2.0` as the catalog holds it after the cross-version check found two things. */
  const BROKEN: CatalogActor = {
    ...PROBE,
    key: 'probe@0.2.0',
    version: '0.2.0',
    incompatibilities: [
      {
        method: 'head',
        field: 'input',
        rule: 'BACKWARD',
        previous: '0.1.0',
        detail: 'required field "timeout" added — data shaped for the older schema does not carry it',
      },
      {
        method: 'head',
        field: 'output',
        rule: 'FORWARD',
        previous: '0.1.0',
        detail: 'field "status" removed — a caller reading it gets nothing',
      },
    ],
  };

  it('names the Method, the direction and the version it was compared against', () => {
    // The whole point of storing the finding: registration succeeded, so this strip is the only
    // place the break is visible before a run meets it as data that does not fit. "0.2.0 is
    // incompatible" would send a reader to diff two descriptors; the Method and the direction are
    // what they can act on.
    const html = draw(folder(), BROKEN);
    expect(html).toContain('data-testid="actor-incompat-probe"');
    expect(html).toContain('data-testid="actor-incompat-probe-head-input"');
    expect(html).toContain('data-testid="actor-incompat-probe-head-output"');
    expect(html).toContain('BACKWARD');
    expect(html).toContain('FORWARD');
    expect(html).toContain('breaks a caller of 0.1.0');
    expect(html).toContain('required field &quot;timeout&quot; added');
  });

  it('says registration was not refused, and that this is structural', () => {
    // Both halves matter to whoever reads it: nothing is broken about the deploy (the version
    // registered), and the check has not proved substitutability — it compared two documents.
    const html = draw(folder(), BROKEN);
    expect(html).toContain('Registration was NOT refused');
    expect(html).toContain('not a proof');
  });

  it('draws nothing at all when there is no finding', () => {
    // Absent is "nothing was reported" — a first version, a new Method, a schema with no fields.
    // A card that drew a compatible badge here would be claiming a check that never ran.
    const html = draw(folder());
    expect(html).not.toContain('data-testid="actor-incompat-probe"');
    expect(html).not.toContain('BACKWARD');
    expect(html.toLowerCase()).not.toContain('compatible');
  });
});

describe('calling a Method from its row', () => {
  it('offers `call`, and the title says what one call is', () => {
    /* THE TITLE USED TO PROMISE THE OPPOSITE — "this generates code, it does not start a run" —
       and that was right for a button that handed over a file. The page calls the Method now
       (ADR 0033), so the title has to say what the call IS instead, and what bounds it: one
       Method, one call, into a Dataset. A title still claiming nothing is started would be the
       one sentence in the feature contradicting the button beside it. */
    const html = draw(folder());
    expect(html).toContain('data-testid="method-call-probe-head"');
    expect(html).toContain('one Method, one call, into a Dataset');
    expect(html).not.toContain('it does not start a run');
    expect(html).toContain('call</button>');
  });

  it('draws no call button when the Actor’s code is not on this disk', () => {
    // BOTH routes behind `call` are keyed by the registered folder — the caller
    // (`POST /api/sources/actor/:id/caller`) and the probe (`…/probe`) — and both 400 on a
    // directory that is gone. The same absence, for the same reason, as the missing edit button,
    // and the card's own sentence above is the explanation for all of them.
    expect(draw(folder({ absent: true }))).not.toContain('data-testid="method-call-probe-head"');
  });
});

/**
 * FORGET IS ON THE CARD NOW, because the folder shelf that used to hold it is gone.
 *
 * The page drew most registered folders twice — once as their own card above the grid, once as the
 * Actor card's `edit code` line — in two vocabularies, leaving the reader to join them by eye. One
 * card carries both facts, so the actions that belonged to the shelf belong here.
 */
describe('forgetting a registration from the card', () => {
  const drawWith = (found: Source, over: Record<string, unknown> = {}): string =>
    renderToStaticMarkup(
      createElement(ActorCard, {
        actor: PROBE,
        machines: [],
        folder: found,
        open: false,
        onToggle: () => {},
        onEdit: () => {},
        onCall: () => {},
        onForget: () => {},
        ...over,
      })
    );

  it('offers forget beside the editor on a registered folder', () => {
    const html = drawWith(folder());
    expect(html).toContain('data-testid="actor-probe-forget"');
    expect(html).toContain('data-testid="actor-edit-probe"');
  });

  it('offers forget on a folder that is GONE, which is when it is wanted most', () => {
    // The directory vanished; the registration is still the operator's to drop. Drawing only the
    // sentence would leave a dead row that nothing on the page can remove.
    const html = drawWith(folder({ absent: true }));
    expect(html).toContain('data-testid="actor-folder-absent-probe"');
    expect(html).toContain('data-testid="actor-probe-forget"');
    expect(html).not.toContain('data-testid="actor-edit-probe"');
  });

  it('says `discovered` instead of a button for a folder found in the default root', () => {
    // `SourceStore.forget` refuses an `at:` id — there is no registration to remove — so a button
    // here would be an affordance that exists only to 400. Same word the folder list used.
    const html = drawWith(folder({ id: 'at:/home/med/.kontra/actors/probe' }));
    expect(html).toContain('data-testid="actor-probe-discovered"');
    expect(html).not.toContain('data-testid="actor-probe-forget"');
  });

  it('draws no forget affordance at all when the page passes no handler', () => {
    expect(draw(folder())).not.toContain('data-testid="actor-probe-forget"');
  });
});

/**
 * A FOLDER NOBODY HAS SERVED, and why it is not "load-only".
 *
 * Two empty Method lists mean opposite things: a catalogued Actor that declared none is a real
 * load-only deployment, and a folder nothing has run has no Methods because nothing has ever looked.
 * The one card that now draws both has to keep saying which — reporting a deployment there is no
 * evidence for is the confident-green-label failure ADR 0017 exists to prevent, one surface over.
 */
describe('an unserved folder', () => {
  const BARE: CatalogActor = {
    key: 'at:/srv/checkout/examples/python/probe',
    name: 'probe',
    version: '0.1.0',
    schemaVersion: '',
    operations: [],
    source: '/srv/checkout/examples/python/probe',
  };

  const drawBare = (): string =>
    renderToStaticMarkup(
      createElement(ActorCard, {
        actor: BARE,
        machines: [],
        folder: folder(),
        open: false,
        catalogued: false,
        onToggle: () => {},
        onEdit: () => {},
        onCall: () => {},
        onForget: () => {},
      })
    );

  it('is marked unserved rather than load-only', () => {
    const html = drawBare();
    expect(html).toContain('unserved');
    expect(html).not.toContain('load-only');
  });

  it('says its Methods are unknown, not that it declared none', () => {
    const html = drawBare();
    expect(html).toContain('nothing has served this folder yet');
    expect(html).not.toContain('a load-only Actor is still a real deployment');
  });

  it('still reaches the editor, which is the whole reason it is drawn', () => {
    expect(drawBare()).toContain('data-testid="actor-edit-probe"');
  });
});

/**
 * WHAT THIS ACTOR WILL ASK FOR (issue 20), on the card rather than in a run's traceback.
 *
 * The card's own contract here is small and worth pinning separately from `SlotStrip`'s: a card
 * draws the strip when there is something to draw and NOTHING when there is not. Most actors ask
 * for no credential, and a "0 credentials" line on every card would be noise on the page this
 * system's inventory is read from.
 */
describe('an Actor’s declared credential slots', () => {
  const slots = (state: 'unbound' | 'bound') => ({
    actor: 'probe',
    version: '0.1.0',
    slots: [
      {
        slot: 'api_key',
        description: 'the vendor key',
        state,
        detail: state === 'bound' ? 'bound to "stripe-prod", version 1' : 'declared, and nothing is bound to it',
        ...(state === 'bound' ? { secret: 'stripe-prod', secretVersion: 1 } : {}),
      },
    ],
    added: [],
  });

  const card = (over: Record<string, unknown>): string =>
    renderToStaticMarkup(
      createElement(ActorCard, {
        actor: PROBE,
        machines: [],
        folder: folder(),
        open: false,
        onToggle: () => {},
        onEdit: () => {},
        onCall: () => {},
        ...over,
      })
    );

  it('says an actor will ask for a credential nobody has granted', () => {
    const html = card({ slots: slots('unbound') });
    expect(html).toContain('data-testid="slots-probe"');
    expect(html).toContain('api_key');
    expect(html).toContain('data-blocking="1"');
  });

  it('names the secret once it is bound', () => {
    expect(card({ slots: slots('bound') })).toContain('stripe-prod');
  });

  it('draws no strip at all when nothing has declared a slot', () => {
    // `null` is the state every card is in before the credential surface answers, and the state
    // every actor that needs no credential stays in.
    expect(card({ slots: null })).not.toContain('data-testid="slots-probe"');
    expect(card({})).not.toContain('data-testid="slots-probe"');
  });
});
