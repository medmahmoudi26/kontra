/**
 * Reading a parked run's asks, and answering one.
 *
 * PURE, except for the two cases that are about a signal — everything else here is a function over
 * a memo bag, which is exactly what the choice to EMIT an ask rather than expose a query bought:
 * the reader has no worker, no cluster and no clock it was not handed, so every state of a parked
 * run can be asserted from a literal object.
 *
 * The two literals this module shares with `sdk/python/kontra/hitl.py` are pinned against that
 * file's own bytes below. A drift there is not a type error — it is a run that publishes an ask
 * nothing reads, and an answer signalled to a name nothing handles.
 */

import { readFileSync } from 'node:fs';
import * as path from 'node:path';
import { describe, expect, it, vi } from 'vitest';

import { describe as describeEvent, mapHistory, type RawHistoryEvent } from './history';
import type { Ask } from './transcript';

import {
  ANSWER_SIGNAL_PREFIX,
  ASK_MEMO_PREFIX,
  AnswerRefused,
  AskNotFound,
  AskNotPending,
  answerAsk,
  defaultOperator,
  pendingAsks,
  readAsks,
  validateAnswer,
  type RunAsk,
} from './hitl';

const T0 = 1_786_831_339_000;
const APPROVAL = {
  type: 'object',
  properties: { approve: { type: 'boolean' }, note: { type: 'string' } },
  required: ['approve'],
};

/** One memo entry, as `upsert_memo` writes it. */
function ask(id: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    [`${ASK_MEMO_PREFIX}${id}`]: {
      id,
      prompt: `Approve ${id}?`,
      askedAt: T0,
      state: 'pending',
      ...over,
    },
  };
}

describe('reading a parked run', () => {
  it('reads every ask a run published, oldest first', () => {
    const memo = {
      ...ask('ask-2', { askedAt: T0 + 5_000 }),
      ...ask('ask-1'),
      // Not an ask. A run's memo is the author's too, and a reader that swallowed every key would
      // render whatever somebody happened to put there as a question to a human.
      run: 'nightly',
    };
    const asks = readAsks(memo, T0 + 10_000);
    expect(asks.map((a) => a.id)).toEqual(['ask-1', 'ask-2']);
  });

  it('answers with a LIST even when a run has exactly one ask', () => {
    // The decision this pins: parallel branches each needing judgement is normal, and a singular
    // shape could not have been widened later without breaking its own contract.
    expect(readAsks(ask('ask-1'), T0)).toHaveLength(1);
    expect(Array.isArray(readAsks(ask('ask-1'), T0))).toBe(true);
  });

  it('says how long the run has been parked, and how long is left', () => {
    const memo = ask('ask-1', { deadlineAt: T0 + 4 * 3_600_000 });
    const [a] = readAsks(memo, T0 + 600_000);
    expect(a!.waitedMs).toBe(600_000);
    expect(a!.remainingMs).toBe(4 * 3_600_000 - 600_000);
  });

  it('measures the wait to the ANSWER on an ask that is no longer pending', () => {
    const memo = ask('ask-1', { state: 'answered', answeredAt: T0 + 90_000, by: 'mo' });
    const [a] = readAsks(memo, T0 + 10 * 3_600_000);
    // NOT to `now`. An answered ask's wait is a fact about the past, and letting it keep climbing
    // would report a decision taken in 90 seconds as one that took ten hours.
    expect(a!.waitedMs).toBe(90_000);
    expect(a!.by).toBe('mo');
  });

  it('leaves an ask with no deadline WITHOUT a countdown rather than with a blank one', () => {
    // Waiting indefinitely is a choice the author made, not a missing value. A `remainingMs` of 0
    // or of Infinity would both render as a number nobody meant.
    const [a] = readAsks(ask('ask-1'), T0 + 600_000);
    expect(a!.deadlineAt).toBeUndefined();
    expect(a!.remainingMs).toBeUndefined();
  });

  it('keeps the four endings apart', () => {
    const memo = {
      ...ask('ask-1', { state: 'answered', answeredAt: T0 + 1 }),
      ...ask('ask-2', { askedAt: T0 + 1, state: 'expired', expiredAt: T0 + 2 }),
      ...ask('ask-3', { askedAt: T0 + 2, state: 'abandoned', abandonedAt: T0 + 3 }),
      ...ask('ask-4', { askedAt: T0 + 3 }),
    };
    const asks = readAsks(memo, T0 + 100);
    expect(asks.map((a) => a.state)).toEqual(['answered', 'expired', 'abandoned', 'pending']);
    // A deadline nobody met and a run somebody stopped are different endings, and only one of the
    // four is still waiting on a human.
    expect(pendingAsks(asks).map((a) => a.id)).toEqual(['ask-4']);
  });

  it('degrades an unreadable ask to a legible row rather than dropping or throwing', () => {
    // The run IS parked whether or not this server can read what it asked. A row that vanished
    // would leave an operator with a stuck run and nothing to click, which is the one outcome
    // that makes it unrecoverable from the UI.
    const asks = readAsks({ [`${ASK_MEMO_PREFIX}ask-1`]: 'not an ask at all' }, T0);
    expect(asks).toHaveLength(1);
    expect(asks[0]!.malformed).toBe(true);
    expect(asks[0]!.prompt).toMatch(/could not be read/);
    expect(asks[0]!.id).toBe('ask-1');
  });

  it('reads a run with no memo at all as a run that asked nobody anything', () => {
    expect(readAsks(undefined, T0)).toEqual([]);
    expect(readAsks({}, T0)).toEqual([]);
    expect(readAsks('nonsense', T0)).toEqual([]);
  });
});

describe('an answer is checked against the schema its own ask declared', () => {
  it('accepts an answer that fits', () => {
    expect(() => validateAnswer(APPROVAL, { approve: true, note: 'checked' })).not.toThrow();
  });

  it('names the field it objected to', () => {
    // "Invalid" without a field is a form an operator has to guess their way around.
    try {
      validateAnswer(APPROVAL, { approve: 'yes' });
      throw new Error('should have refused');
    } catch (err) {
      expect(err).toBeInstanceOf(AnswerRefused);
      expect((err as AnswerRefused).field).toBe('/approve');
      expect((err as AnswerRefused).message).toMatch(/boolean/);
    }
  });

  it('accepts anything when the ask declared no shape', () => {
    // The author chose not to constrain the answer. Inventing a constraint here would refuse
    // answers to their own question.
    expect(() => validateAnswer(undefined, { anything: [1, 2, 3] })).not.toThrow();
    expect(() => validateAnswer(null, 'a bare string')).not.toThrow();
  });

  it('refuses rather than waves through a schema it cannot compile', () => {
    // A workflow asked for a shape and nothing checked it. Signalling anyway would hand the run a
    // value it never agreed to take.
    expect(() => validateAnswer({ type: 'object', required: 'not-an-array' }, {})).toThrow(
      AnswerRefused
    );
    expect(() => validateAnswer('a string is not a schema', {})).toThrow(AnswerRefused);
  });

  it('compiles one schema twice without colliding on its own $id', () => {
    // The bug the dispatch path already hit: compiling registers `$id` globally in an ajv
    // instance, and a second registration throws. An ask is answered more than once across a
    // fleet's worth of runs, so this has to be survivable.
    const withId = { $id: 'https://kontra/ask/approval', ...APPROVAL };
    expect(() => validateAnswer(withId, { approve: true })).not.toThrow();
    expect(() => validateAnswer(withId, { approve: true })).not.toThrow();
  });
});

describe('answering one', () => {
  const pending: RunAsk = {
    id: 'ask-1',
    prompt: 'Approve these 12 hosts?',
    askedAt: T0,
    schema: APPROVAL,
    state: 'pending',
    waitedMs: 0,
  };

  const deps = (asks: RunAsk[] | undefined) => {
    const signal = vi.fn(async () => undefined);
    return { signal, deps: { asks: async () => asks, signal } };
  };

  it('validates BEFORE it signals, and signals the name that carries the ask id', async () => {
    const { signal, deps: d } = deps([pending]);
    const answered = await answerAsk(d, 'sweep-1', 'ask-1', { value: { approve: true }, by: 'mo' });
    expect(signal).toHaveBeenCalledWith('sweep-1', `${ANSWER_SIGNAL_PREFIX}ask-1`, {
      value: { approve: true },
      by: 'mo',
    });
    expect(answered.state).toBe('answered');
    expect(answered.by).toBe('mo');
  });

  it('refuses an answer that does not fit and leaves the run exactly as parked', async () => {
    // THE ORDERING IS THE POINT. A signal is durable and unacknowledged: once it is in the run's
    // history the workflow acts on it and there is no taking it back.
    const { signal, deps: d } = deps([pending]);
    await expect(answerAsk(d, 'sweep-1', 'ask-1', { value: { approve: 'yes' } })).rejects.toThrow(
      AnswerRefused
    );
    expect(signal).not.toHaveBeenCalled();
  });

  it('answering one ask leaves its siblings pending', async () => {
    const sibling: RunAsk = { ...pending, id: 'ask-2', schema: undefined };
    const { signal, deps: d } = deps([pending, sibling]);
    await answerAsk(d, 'sweep-1', 'ask-2', { value: 'go' });
    expect(signal).toHaveBeenCalledTimes(1);
    expect(signal).toHaveBeenCalledWith('sweep-1', `${ANSWER_SIGNAL_PREFIX}ask-2`, {
      value: 'go',
      by: '',
    });
  });

  it('tells a typo apart from an answer that arrived too late', async () => {
    const { signal, deps: d } = deps([{ ...pending, state: 'answered', by: 'alice' }]);
    await expect(answerAsk(d, 'sweep-1', 'ask-9', { value: {} })).rejects.toThrow(AskNotFound);
    // Not "invalid": the request was well formed and somebody got there first. An operator told
    // "invalid" goes looking at their own form.
    await expect(answerAsk(d, 'sweep-1', 'ask-1', { value: { approve: true } })).rejects.toThrow(
      AskNotPending
    );
    expect(signal).not.toHaveBeenCalled();
  });

  it('answers nothing on a run neither authority has heard of', async () => {
    const { signal, deps: d } = deps(undefined);
    await expect(answerAsk(d, 'no-such-run', 'ask-1', { value: {} })).rejects.toThrow(AskNotFound);
    expect(signal).not.toHaveBeenCalled();
  });

  it('records an unlabelled answer as unlabelled rather than inventing an operator', async () => {
    // SELF-ASSERTED MEANS OPTIONAL. There is no authenticated identity to fall back to, and a `by`
    // of "operator" or of a hostname would be kontra asserting something no human said.
    const { signal, deps: d } = deps([{ ...pending, schema: undefined }]);
    const answered = await answerAsk(d, 'sweep-1', 'ask-1', { value: 'go' });
    expect(answered.by).toBeUndefined();
    expect(signal).toHaveBeenCalledWith('sweep-1', `${ANSWER_SIGNAL_PREFIX}ask-1`, {
      value: 'go',
      by: '',
    });
  });

  it('takes the operator label from local config, and nothing at all when there is none', () => {
    expect(defaultOperator({ KONTRA_OPERATOR: '  mo  ' } as NodeJS.ProcessEnv)).toBe('mo');
    expect(defaultOperator({} as NodeJS.ProcessEnv)).toBe('');
  });
});

describe('the literals the two SDKs share', () => {
  /**
   * PINNED AGAINST THE PYTHON SOURCE, not against a copy of it. There is no code sharing across
   * this boundary and there cannot be — the same rule as the task-queue derivations — so the only
   * way a rename stays honest is a test that reads the peer's own bytes.
   */
  it('spells the memo prefix and the answer signal exactly as actorkit does', () => {
    const py = readFileSync(
      path.join(__dirname, '..', '..', '..', 'sdk', 'python', 'kontra', 'hitl.py'),
      'utf8'
    );
    expect(py).toContain(`ASK_MEMO_PREFIX = "${ASK_MEMO_PREFIX}"`);
    expect(py).toContain(`ANSWER_SIGNAL_PREFIX = "${ANSWER_SIGNAL_PREFIX}"`);
  });
});

describe('what the archived reduced log records about an ask', () => {
  /**
   * THE AUDIT PAIRING, and the reason an ask is emitted at all. The reduced log is what survives
   * Temporal's retention (ADR 0025), and it is payload-free by construction — so what it can say
   * about an ask has to come from event METADATA. Two fields carry it: the KEYS of a memo upsert
   * (a map's keys are structure, its values are the payloads nothing here decodes), and a signal's
   * NAME, which `kontra.hitl` builds to carry the ask id for exactly this reason.
   */
  it('names the ask a memo upsert published, without decoding its value', () => {
    const detail = describeEvent('WorkflowPropertiesModified', {
      upsertedMemo: {
        fields: {
          [`${ASK_MEMO_PREFIX}ask-1`]: { metadata: {}, data: 'BASE64-THE-LOG-NEVER-READS' },
        },
      },
    });
    expect(detail).toBe(`memo=${ASK_MEMO_PREFIX}ask-1`);
    expect(detail).not.toContain('BASE64');
  });

  it('names the ask an answer answers', () => {
    // `signal` is already a reduced-log category, so the ANSWER reached the archive for free — but
    // as an anonymous row. With the id in the name, the archive pairs it with the question above.
    expect(
      describeEvent('WorkflowExecutionSignaled', { signalName: `${ANSWER_SIGNAL_PREFIX}ask-1` })
    ).toBe(`signal=${ANSWER_SIGNAL_PREFIX}ask-1`);
  });

  it('keeps a run\'s question and its answer together in one reduced log', () => {
    const raw: RawHistoryEvent[] = [
      {
        eventId: 1,
        eventTime: { seconds: 100 },
        workflowExecutionStartedEventAttributes: { identity: 'mo' },
      },
      {
        eventId: 2,
        eventTime: { seconds: 110 },
        workflowPropertiesModifiedEventAttributes: {
          upsertedMemo: { fields: { [`${ASK_MEMO_PREFIX}ask-1`]: {} } },
        },
      },
      {
        eventId: 3,
        eventTime: { seconds: 400 },
        workflowExecutionSignaledEventAttributes: { signalName: `${ANSWER_SIGNAL_PREFIX}ask-1` },
      },
    ];
    const log = mapHistory(raw);
    expect(log.events.map((e) => e.detail)).toEqual([
      'identity=mo',
      `memo=${ASK_MEMO_PREFIX}ask-1`,
      `signal=${ANSWER_SIGNAL_PREFIX}ask-1`,
    ]);
    // The answer is a `signal` and the ask is a `workflow` event — both already reduced-log
    // categories, so neither needed a new one to reach the archive.
    expect(log.events.map((e) => e.cat)).toEqual(['workflow', 'workflow', 'signal']);
  });

  it('does not dump a whole memo into the one line an operator reads', () => {
    const fields: Record<string, unknown> = {};
    for (let i = 1; i <= 9; i++) fields[`${ASK_MEMO_PREFIX}ask-${i}`] = {};
    const detail = describeEvent('WorkflowPropertiesModified', { upsertedMemo: { fields } });
    expect(detail).toContain('+5 more');
    expect(detail.length).toBeLessThan(120);
  });

  it('says nothing about an event that carries no memo at all', () => {
    expect(describeEvent('WorkflowTaskScheduled', {})).toBe('WorkflowTaskScheduled');
    expect(describeEvent('WorkflowPropertiesModified', { upsertedMemo: {} })).toBe(
      'WorkflowPropertiesModified'
    );
  });
});

describe('the shape the transcript reads', () => {
  it('is what this route serves, with nothing in between to translate', () => {
    // A COMPILE-TIME ASSERTION as much as a runtime one. `transcript.ts` declares what a reader of
    // an ask expects and says the reader "gains a second source" when the ask becomes a history
    // event — this is that source. If `RunAsk` ever stops satisfying `Ask`, this line stops
    // compiling, which is the failure a mapping layer would have turned into a blank form.
    const asks: Ask[] = readAsks(
      ask('ask-1', { deadlineAt: T0 + 1_000, schema: APPROVAL, context: { n: 12 } }),
      T0
    );
    expect(asks[0]).toMatchObject({
      id: 'ask-1',
      prompt: 'Approve ask-1?',
      askedAt: T0,
      deadlineAt: T0 + 1_000,
    });
  });
});
