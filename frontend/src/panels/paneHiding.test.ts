/**
 * Hiding a pane — the document, and the claim that it stops costing something.
 *
 * WHAT THIS FILE IS DEFENDING. "Hide" is the easiest feature in this codebase to implement wrongly
 * and have it look right: `display: none` on a tile passes every screenshot, and leaves the streamer
 * taking a `capture-pane` on a 2 GB Machine every three seconds for a rectangle nobody is looking at.
 * So the assertions here are about the two things a screenshot cannot show — what falls out of the
 * subscription set, and what survives a reload — plus the one thing that must NOT happen, which is a
 * pane disappearing with nothing left on the page to say it was hidden (that one is
 * `hiddenPanes.render.test.ts`, because it is about what is drawn).
 */

import { describe, expect, it } from 'vitest';

import {
  HIDDEN_STORAGE_KEY,
  HIDDEN_VERSION,
  parseHidden,
  serializeHidden,
  toggleHidden,
  wantedSubscriptions,
} from './paneHiding';
import { resubscribePlan } from './reconnect';

const ACTOR = 'local:localhost/nscheck-0_1_0/actor';
const HANDLER = 'local:localhost/nscheck-0_1_0/handler';
const CLAUDE = 'local:localhost/kontra-0/claude';

describe('the document', () => {
  it('round-trips through storage', () => {
    const { ids, warning } = parseHidden(serializeHidden([ACTOR, HANDLER]));
    expect(ids).toEqual([ACTOR, HANDLER]);
    expect(warning).toBeUndefined();
  });

  it('names one key and versions the VALUE, so clearing a key by hand is not the migration path', () => {
    expect(HIDDEN_STORAGE_KEY).toBe('kontra-monitor-hidden');
    expect(JSON.parse(serializeHidden([ACTOR]))).toEqual({ version: HIDDEN_VERSION, ids: [ACTOR] });
  });

  it('FAILS OPEN on anything it cannot read, and says so', () => {
    // The safe direction, and the reason it is stated: a hidden set that guesses shows a wall with
    // panes missing and nothing on the page to explain them. Every one of these has to produce a
    // wall where everything is visible.
    for (const raw of ['{', 'null', '[]', '"nope"', '{"version":99,"ids":["x"]}', '{"version":1}']) {
      const { ids, warning } = parseHidden(raw);
      expect(ids).toEqual([]);
      expect(warning).toBeTruthy();
    }
    expect(parseHidden(null)).toEqual({ ids: [] });
    expect(parseHidden('   ')).toEqual({ ids: [] });
  });

  it('drops entries it cannot read rather than the whole set, and reports how many', () => {
    const { ids, warning } = parseHidden(
      JSON.stringify({ version: HIDDEN_VERSION, ids: [ACTOR, '', 7, ACTOR, HANDLER] })
    );
    expect(ids).toEqual([ACTOR, HANDLER]);
    expect(warning).toContain('3 entries');
  });

  it('toggles, and keeps the order they were hidden in', () => {
    // Insertion order is what makes the affordance readable: the pane you just hid is the last one
    // in the list, so undoing reads backwards.
    let ids = toggleHidden([], ACTOR);
    ids = toggleHidden(ids, HANDLER);
    expect(ids).toEqual([ACTOR, HANDLER]);
    expect(toggleHidden(ids, ACTOR)).toEqual([HANDLER]);
  });
});

/**
 * THE PROPERTY THE WHOLE FEATURE IS FOR: hiding UNSUBSCRIBES.
 *
 * `DashboardPage.sync` is `resubscribePlan(wantedSubscriptions(…), conn.subscribed)`, so driving
 * those two functions together is driving the real thing. What the streamer does with the
 * `unsubscribe` is read out in `sync`'s own comment (`snapshotRound` builds its `capture-pane`
 * window list from live subscriptions, `fanOut` delivers only to subscribers, `announceHealth` walks
 * them) — and what it does NOT stop is the 30 s discovery-and-probe pass, which is per Machine and
 * owes nothing to any subscription.
 */
describe('what hiding takes off the socket', () => {
  const tiles = [{ id: ACTOR }, { id: HANDLER }, { id: CLAUDE }];
  const measured = new Set([ACTOR, HANDLER, CLAUDE]);

  it('drops a hidden pane out of the wanted set', () => {
    expect(wantedSubscriptions(tiles, measured, new Set())).toEqual([ACTOR, HANDLER, CLAUDE]);
    expect(wantedSubscriptions(tiles, measured, new Set([HANDLER]))).toEqual([ACTOR, CLAUDE]);
  });

  it('turns that into an UNSUBSCRIBE on a socket that already had it', () => {
    const subscribed = new Set([ACTOR, HANDLER, CLAUDE]);
    const plan = resubscribePlan(wantedSubscriptions(tiles, measured, new Set([HANDLER])), subscribed);
    // The one line that separates this feature from `display: none`.
    expect(plan.unsubscribe).toEqual([HANDLER]);
    expect(plan.subscribe).toEqual([]);
  });

  it('re-subscribes it when it is restored, exactly once', () => {
    const subscribed = new Set([ACTOR, CLAUDE]); // HANDLER was hidden and unsubscribed
    const plan = resubscribePlan(wantedSubscriptions(tiles, measured, new Set()), subscribed);
    expect(plan.subscribe).toEqual([HANDLER]);
    expect(plan.unsubscribe).toEqual([]);
  });

  it('still refuses to subscribe a tile that has not measured itself, hidden or not', () => {
    // A 0×0 subscribe asks a node for a zero-sized screen. Hiding must not become a way around the
    // one guard that stops that.
    expect(wantedSubscriptions(tiles, new Set([ACTOR]), new Set())).toEqual([ACTOR]);
  });

  it('hides ONE WINDOW, not the session it lives in', () => {
    // The grain, asserted rather than described: `nscheck-0_1_0` has an actor window and a handler
    // window, and an operator silencing the noisy one keeps the other. A control that took the
    // session would be doing more than the tile it sits on says it does.
    expect(wantedSubscriptions(tiles, measured, new Set([HANDLER]))).toContain(ACTOR);
  });
});
