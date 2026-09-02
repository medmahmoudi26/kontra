/**
 * The TYPESCRIPT ARM of `conformance/queues.json` — all four derivations this package owns.
 *
 * WHY ONE FILE AND NOT FOUR. The Actor's task queue, its Nexus endpoint and its tmux session name
 * are three rules with three different answers to the same question ("what is this Actor called
 * here?"), and this package derives them in four places: `nexusRegistry.ts` (endpoint + shared
 * queue), `panels/pollers.ts` (shared queue), `panels/tmux.ts` (the Worker's session) and
 * `panels/discovery.ts` (a Machine's session). Each used to be pinned by its own hand-written
 * table of examples beside it, and the tables did not know about each other — so the one property
 * that matters most across them was untestable in any of the four files: A QUEUE NAME IS NOT
 * SANITISED AND AN ENDPOINT NAME IS, and a derivation that shared one rule between them routes to
 * a queue nobody polls.
 *
 * THE FAILURE THESE GUARD, in the code's own words: the actor registers, polls a queue nobody
 * schedules onto, and reports as a healthy idle Worker while every run hangs to StartToClose. For
 * the session name it is ADR 0020's forbidden tile — a Machine whose Worker is running perfectly,
 * drawn as one with NO SESSION.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import { endpointName, sharedQueue as endpointQueue } from './nexusRegistry';
import { sharedQueue as pollerQueue } from './panels/pollers';
import { actorSession } from './panels/tmux';
import { sessionNameFor } from './panels/discovery';

type QueueCase = { why: string; name: string; version: string; expect: string };
type TmuxCase = {
  why: string;
  actor: string;
  version: string;
  tag: string;
  worker: string;
  machine: string;
};

const corpus = JSON.parse(
  readFileSync(join(__dirname, '../../conformance/queues.json'), 'utf8')
) as {
  shared: { cases: QueueCase[] };
  endpoint: { servable: string; cases: QueueCase[] };
  tmux_session: { cases: TmuxCase[] };
};

describe('the corpus itself', () => {
  it('did not silently shrink, and still carries the inputs that break', () => {
    // A corpus of nothing passes every case below. The named rows are the ones the easy version of
    // this file would have omitted: an empty version is the DIY/CLI path's whole contract, and the
    // space and the emoji are what separate the unsanitised rule from the sanitised one.
    expect(corpus.shared.cases.length).toBeGreaterThanOrEqual(6);
    expect(corpus.endpoint.cases.length).toBeGreaterThanOrEqual(10);
    expect(corpus.tmux_session.cases.length).toBeGreaterThanOrEqual(8);
    const blob = JSON.stringify(corpus);
    for (const token of ['my actor', 'café', 'naïve', '📦', '-shared', 'fleet', 'actor']) {
      expect(blob, `the corpus no longer exercises ${token}`).toContain(token);
    }
    expect(corpus.shared.cases.some((c) => c.version === '')).toBe(true);
  });
});

describe('the shared task queue', () => {
  // TWO DERIVATIONS IN THIS PACKAGE, asserted against the same rows: `nexusRegistry.ts` points the
  // endpoint at this queue and `panels/pollers.ts` describes it to find out whether anything is
  // polling. If they disagree, the poller tile reports `none` for a queue that is being served.
  for (const c of corpus.shared.cases) {
    it(c.why, () => {
      expect(endpointQueue(c.name, c.version)).toBe(c.expect);
      expect(pollerQueue(c.name, c.version)).toBe(c.expect);
    });
  }

  it('is never the sessions queue the actor process itself polls', () => {
    // The handler serves its Nexus operation on the SHARED queue and polls `-sessions` for
    // RunBatch/Close. An endpoint aimed at the latter routes operations to a queue that does not
    // serve them — which is not an error, it is a timeout.
    for (const c of corpus.shared.cases) {
      expect(endpointQueue(c.name, c.version).endsWith('-sessions')).toBe(false);
    }
  });
});

describe('the Nexus endpoint name', () => {
  for (const c of corpus.endpoint.cases) {
    it(c.why, () => {
      expect(endpointName(c.name, c.version)).toBe(c.expect);
    });
  }

  it('is always a name this cluster will accept', () => {
    // The pattern is the corpus's, not this file's: the dev server enforces it, and a name that
    // fails it is refused at create time with a message about the NAME rather than about the
    // missing version.
    const servable = new RegExp(corpus.endpoint.servable);
    for (const c of corpus.endpoint.cases) {
      expect(servable.test(c.expect), `${c.why}: ${c.expect}`).toBe(true);
    }
  });
});

describe('a queue name is not sanitised and an endpoint name is', () => {
  it('passes a space through one rule and collapses it in the other', () => {
    // THE PROPERTY BEHIND THE ADVERSARIAL ROWS, asserted directly so that a future maintainer who
    // shares one sanitiser between the two reads one line instead of six mismatched strings.
    // Temporal accepts a space in a task queue name; the endpoint registry does not. Cleaning up
    // the queue routes to a queue nobody polls, which is silent; leaving the endpoint dirty fails
    // at create time, which is loud. Opposite rules, one pair of inputs.
    expect(endpointQueue('my actor', '0.1.0')).toBe('my actor-0.1.0');
    expect(endpointName('my actor', '0.1.0')).not.toContain(' ');
  });
});

describe('the tmux session name', () => {
  // TWO DOMAINS, TWO ANSWERS PER ROW. `worker` is an Actor's Worker — a name and a version and
  // nothing else. `machine` is a fleet Machine's session, which is also given the fleet's tag,
  // because a `fleet up` with no Actor placed on it still has Terminals. Their fallbacks differ on
  // purpose and the corpus says why; what must not differ is either one from its Go peer.
  for (const c of corpus.tmux_session.cases) {
    it(c.why, () => {
      expect(actorSession(c.actor, c.version)).toBe(c.worker);
      expect(sessionNameFor(c.actor, c.version, c.tag)).toBe(c.machine);
    });
  }

  it('records a reason for every row where the two answers differ', () => {
    // A case whose reason is not written down is one nobody can tell from a typo when it goes red
    // (conformance/README.md). These are the rows that would read as a bug on sight.
    const differ = corpus.tmux_session.cases.filter((c) => c.worker !== c.machine);
    expect(differ.length).toBeGreaterThan(0);
    for (const c of differ) expect(c.why.length).toBeGreaterThan(30);
  });
});
