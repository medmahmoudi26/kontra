/**
 * The TYPESCRIPT ARM of `shared/conformance/queues.json` — all four derivations this package owns.
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
import {
  actorSession,
  actorSessionTag,
  isKnownSessionKind,
  kontraSessionKind,
  SESSION_KINDS,
  watchSessionTag,
  workflowSessionTag,
} from './panels/tmux';
import { sessionNameFor } from './panels/discovery';
import { isKontraSession } from './panels/local';

type QueueCase = { why: string; name: string; version: string; expect: string };
type TmuxCase = {
  why: string;
  actor: string;
  version: string;
  tag: string;
  worker: string;
  machine: string;
};
type SessionKindCase = { why: string; tag: string; kind: string; known: boolean };

const corpus = JSON.parse(
  readFileSync(join(__dirname, '../../../shared/conformance/queues.json'), 'utf8')
) as {
  shared: { cases: QueueCase[] };
  endpoint: { servable: string; cases: QueueCase[] };
  tmux_session: { cases: TmuxCase[] };
  session_kind: { cases: SessionKindCase[] };
};

describe('the corpus itself', () => {
  it('did not silently shrink, and still carries the inputs that break', () => {
    // A corpus of nothing passes every case below. The named rows are the ones the easy version of
    // this file would have omitted: an empty version is the DIY/CLI path's whole contract, and the
    // space and the emoji are what separate the unsanitised rule from the sanitised one.
    expect(corpus.shared.cases.length).toBeGreaterThanOrEqual(6);
    expect(corpus.endpoint.cases.length).toBeGreaterThanOrEqual(10);
    expect(corpus.tmux_session.cases.length).toBeGreaterThanOrEqual(8);
    expect(corpus.session_kind.cases.length).toBeGreaterThanOrEqual(9);
    const blob = JSON.stringify(corpus);
    // `agent:claude` and `Actor:probe` are §session_kind's refusals that LOOK like acceptances. A
    // corpus that lost them would pass every row it kept while admitting the thing ADR 0043 refuses.
    for (const token of ['my actor', 'café', 'naïve', '📦', '-shared', 'fleet', 'actor',
                         'agent:claude', 'Actor:probe', 'watch:repl']) {
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
    // (shared/conformance/README.md). These are the rows that would read as a bug on sight.
    const differ = corpus.tmux_session.cases.filter((c) => c.worker !== c.machine);
    expect(differ.length).toBeGreaterThan(0);
    for (const c of differ) expect(c.why.length).toBeGreaterThan(30);
  });
});

describe('the session kind vocabulary (ADR 0043)', () => {
  // THE READER'S ARM. The CLI writes `@kontra`; this side decides whether the wall shows what it
  // finds. A kind one writes and the other does not know is a Worker running perfectly whose tile is
  // absent — ADR 0020's one forbidden failure — and a kind this side admits that nothing writes is a
  // door nobody meant to leave open.
  for (const c of corpus.session_kind.cases) {
    it(c.why, () => {
      expect(kontraSessionKind(c.tag)).toBe(c.kind);
      expect(isKnownSessionKind(c.tag)).toBe(c.known);
    });
  }

  it('is the gate discovery actually consults, not a parallel opinion', () => {
    // `isKnownSessionKind` being right buys nothing if `isKontraSession` does not call it. A tagged
    // session is admitted EXACTLY when its kind is known; the session NAME is held constant and
    // deliberately not `kontra-`, so the legacy prefix cannot be what answers.
    for (const c of corpus.session_kind.cases) {
      if (c.tag === '') continue; // untagged is the prefix's business, asserted below
      expect(isKontraSession({ session: 'some-session', kontra: c.tag }), c.tag).toBe(c.known);
    }
  });

  it('still admits an untagged legacy Worker by name, and nothing else', () => {
    // ADR 0020: a live Worker drawn as absent is the one thing a tile may never say. A Worker that
    // predates tagging has a `kontra-` name and no tag, so the prefix stays — and it is the NARROWER
    // door, which is the half worth pinning.
    expect(isKontraSession({ session: 'kontra-webcrawl', kontra: '' })).toBe(true);
    expect(isKontraSession({ session: 'kontra-webcrawl' })).toBe(true);
    expect(isKontraSession({ session: 'my-own-session', kontra: '' })).toBe(false);
    expect(isKontraSession({ session: 'my-own-session' })).toBe(false);
  });

  it('admits every tag kontra itself writes', () => {
    // The vocabulary is only useful if the writers stay inside it. These are the TypeScript writers;
    // `cli/queues_conformance_test.go` holds the Go ones against the same set.
    const written = [actorSessionTag('probe', '0.1.0'), workflowSessionTag('hunt'), watchSessionTag('repl')];
    expect(written).toHaveLength(SESSION_KINDS.length);
    expect(new Set(written.map(kontraSessionKind))).toEqual(new Set(SESSION_KINDS));
    for (const tag of written) expect(isKnownSessionKind(tag), tag).toBe(true);
  });

  it('carries the refusals that look like acceptances', () => {
    // A corpus of only-valid rows passes a function that returns true unconditionally — which is
    // precisely the pre-0043 behaviour this change removes. These are the rows that catch it.
    const refused = corpus.session_kind.cases.filter((c) => !c.known).map((c) => c.tag);
    expect(refused).toContain('agent:claude'); // a plausible kind that is not ours
    expect(refused).toContain('Actor:probe:0.1.0'); // case-folded
    expect(refused).toContain('anything'); // no colon at all
    expect(corpus.session_kind.cases.filter((c) => c.known).length).toBeGreaterThanOrEqual(3);
  });
});
