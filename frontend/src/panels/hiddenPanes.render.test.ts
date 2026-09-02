/**
 * "3 panes hidden" — the affordance, asserted as the line an operator reads.
 *
 * HIDDEN MUST NOT LOOK LIKE GONE. That is the first property `paneHiding.ts` owes, and it is the one
 * that cannot be proven by any test of the hidden SET: a set with three ids in it and a page that
 * renders nothing about them is a wall with three holes, indistinguishable from three Workers that
 * died — the exact confusion this dashboard has produced before and the reason `TileWall` spends a
 * row on ghosts. So this file reads the rendered strip, the way `HealthChips.test.ts`'s `row()` reads
 * the chip row.
 */

import { describe, expect, it } from 'vitest';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';

import HiddenPanes, { hiddenLabel } from './chrome/HiddenPanes';

const ACTOR = 'local:localhost/nscheck-0_1_0/actor';
const HANDLER = 'local:localhost/nscheck-0_1_0/handler';
const CRAWLER = 'fleet:kf-crawl-07/kontra-webcrawl/actor';

function render(ids: string[], known: string[] = ids): string {
  return renderToStaticMarkup(
    createElement(HiddenPanes, {
      ids,
      known: new Set(known),
      onShow: () => {},
      onShowAll: () => {},
    })
  );
}

/** The strip as text, tags stripped, one space between words. */
function strip(html: string): string {
  return html
    .replace(/<[^>]*>/g, ' ')
    .replace(/&#x27;/g, "'")
    .replace(/&quot;/g, '"')
    .replace(/&amp;/g, '&')
    .replace(/\s+/g, ' ')
    .trim();
}

describe('what a wall with hidden panes says about them', () => {
  it('renders nothing at all when nothing is hidden', () => {
    // The one case where silence is right: there is no absence to explain, and a permanent
    // "0 hidden" row is a line of the wall spent on a fact about nothing.
    expect(render([])).toBe('');
  });

  it('says how many, says WHAT, and says that they stopped costing anything', () => {
    // Was: nothing. A tile left the wall and the page said nothing anywhere, which is what a dead
    // Worker looks like.
    expect(strip(render([ACTOR, HANDLER, CRAWLER]))).toBe(
      '3 panes hidden — one tmux window each, not drawn and not subscribed: no snapshot exec, no ' +
        'frames, no live attach. Click one to put it back. ' +
        'localhost nscheck-0_1_0:actor ✕ ' +
        'localhost nscheck-0_1_0:handler ✕ ' +
        'kf-crawl-07 kontra-webcrawl:actor ✕ ' +
        'Show all'
    );
  });

  it('names the WINDOW, because that is the grain the control hides', () => {
    // `<node> <session>:<window>` — the same string the status line prints and `tmux attach -t`
    // takes. An operator who hid the handler still has the actor, and this list is where that is
    // visible rather than inferred.
    expect(hiddenLabel(HANDLER)).toBe('localhost nscheck-0_1_0:handler');
    const html = render([HANDLER]);
    expect(html).toContain('nscheck-0_1_0:handler');
    expect(html).not.toContain('nscheck-0_1_0:actor');
  });

  it('agrees with itself about the count and the plural', () => {
    expect(strip(render([ACTOR]))).toContain('1 pane hidden');
    expect(render([ACTOR])).toContain('data-count="1"');
    expect(strip(render([ACTOR, HANDLER]))).toContain('2 panes hidden');
  });

  it('every name is the way back, and there is a way back for all of them at once', () => {
    const html = render([ACTOR, HANDLER]);
    expect(html).toContain(`data-testid="unhide-${ACTOR}"`);
    expect(html).toContain(`data-testid="unhide-${HANDLER}"`);
    expect(html).toContain('data-testid="unhide-all"');
  });

  it('still lists a hidden pane the streamer has stopped reporting, and marks it', () => {
    /*
      A hidden pane on a Machine that left the Fleet. Dropping it from this list would leave an
      invisible entry that silently re-hides the tile if that id ever comes back — a wall with a hole
      in it and nothing on the page to explain the hole — and naming it is the only way to clear it.
    */
    const html = render([ACTOR, CRAWLER], [ACTOR]);
    expect(html).toContain(`data-testid="unhide-${CRAWLER}"`);
    expect(html).toMatch(new RegExp(`unhide-${CRAWLER.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}"[^>]*data-absent="true"`));
    expect(strip(html)).toContain('kf-crawl-07 kontra-webcrawl:actor (not in the inventory)');
    // …and the one that IS reported is not marked.
    expect(html).toMatch(new RegExp(`unhide-${ACTOR.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}"(?![^>]*data-absent)`));
  });

  it('reads as a qualifier, not as a failure', () => {
    // Amber, dashed — the same register as the held-repaint badge and the stale badge. Hiding a pane
    // is something an operator DID; it is not a fault and must not be styled as one, or a wall with
    // one tidied-away tile looks like a wall with a problem.
    const html = render([ACTOR]);
    expect(html).toContain('border-dashed');
    expect(html).toContain('amber');
    expect(html).not.toContain('destructive');
    expect(html).not.toContain('red-');
  });
});
