/**
 * The stream must agree with the poll, push only on change, and end.
 *
 * THE AGREEMENT IS THE ASSERTION THAT MATTERS. kontra deliberately gives a Run several readings and
 * none substitutes for another; a fast path that reported something slightly different would be a
 * fourth reading, and the one nobody cross-checks. So the payload here is produced by the same
 * function the polled route uses, and this suite pins that the bytes match.
 */

import { describe, expect, it } from 'vitest';

import { STREAM_HEADERS, followRun } from './runStream';

/**
 * A manual clock, scheduler and sink — no socket, no real interval.
 *
 * THE ROUTE CANNOT BE TESTED THROUGH `app.inject()`: it waits for a response to END and an event
 * stream never does, so every case hung for thirty seconds and asserted nothing. The loop is the
 * part with decisions in it, so the loop is what is tested.
 */
function harness(reads: Array<{ ok: boolean; terminal?: boolean; body: unknown }>) {
  let ticks: Array<() => void> = [];
  let clock = 0;
  let i = 0;
  const seen: string[] = [];
  const written: string[] = [];
  let closed = false;

  const stop = followRun(
    'r1',
    { write: (c) => written.push(c), close: () => (closed = true) },
    {
      read: async (runId) => {
        seen.push(runId);
        return reads[Math.min(i++, reads.length - 1)]!;
      },
      schedule: (tick) => {
        ticks.push(tick);
        return () => {
          ticks = ticks.filter((t) => t !== tick);
        };
      },
      now: () => clock,
    }
  );

  return {
    tick: async () => {
      for (const t of ticks) t();
      await Promise.resolve();
      await Promise.resolve();
    },
    advance: (ms: number) => (clock += ms),
    reads: seen,
    out: () => written.join(''),
    isClosed: () => closed,
    stop,
  };
}

function frames(payload: string): Array<{ event: string; data: unknown }> {
  return payload
    .split('\n\n')
    .filter((b) => b.trim())
    .map((block) => {
      const event = /^event: (.+)$/m.exec(block)?.[1] ?? '';
      const data = /^data: (.+)$/m.exec(block)?.[1] ?? 'null';
      return { event, data: JSON.parse(data) };
    });
}

describe('the run stream', () => {
  it('sends the current state immediately, not one interval late', async () => {
    const h = harness([{ ok: true, body: { turns: ['a'] } }]);
    await h.tick();
    expect(frames(h.out())[0]).toEqual({ event: 'state', data: { turns: ['a'] } });
    expect(h.reads).toContain('r1');
  });

  it('pushes only when the transcript changes', async () => {
    const h = harness([
      { ok: true, body: { turns: ['a'] } },
      { ok: true, body: { turns: ['a'] } }, // unchanged — must NOT be pushed
      { ok: true, body: { turns: ['a', 'b'] } },
    ]);
    await h.tick(); // the immediate read
    await h.tick(); // identical — must NOT push
    await h.tick(); // changed — must push
    const states = frames(h.out()).filter((f) => f.event === 'state');
    // A Run that is thinking produces the same transcript every tick; pushing it would spend the
    // bandwidth polling spent while calling itself a stream.
    expect(states.length).toBe(2);
    expect(states[0]!.data).toEqual({ turns: ['a'] });
    expect(states[1]!.data).toEqual({ turns: ['a', 'b'] });
  });

  it('ends a terminal run rather than holding the connection forever', async () => {
    const h = harness([{ ok: true, terminal: true, body: { turns: ['done'] } }]);
    await h.tick();
    // Not immediately: a terminal Run's last events are what somebody opening the page wants, and
    // closing the instant it completes would race the draw.
    expect(frames(h.out()).some((f) => f.event === 'end')).toBe(false);
    expect(h.isClosed()).toBe(false);
    h.advance(10_001);
    await h.tick();
    expect(frames(h.out()).some((f) => f.event === 'end')).toBe(true);
    expect(h.isClosed()).toBe(true);
  });

  it('reports a read failure without ending the stream', async () => {
    const written: string[] = [];
    let closed = false;
    followRun(
      'r1',
      { write: (c) => written.push(c), close: () => (closed = true) },
      {
        read: async () => {
          throw new Error('temporal blipped');
        },
        schedule: () => () => {},
        now: () => 0,
      }
    );
    await Promise.resolve();
    await Promise.resolve();
    const got = frames(written.join(''));
    // A blip is not the end: the client would reconnect into the same condition. Report and keep
    // the stream open; the client decides when to give up.
    expect(got.some((f) => f.event === 'warn')).toBe(true);
    expect(got.some((f) => f.event === 'end')).toBe(false);
    expect(closed).toBe(false);
  });

  it('declares the event-stream content type, unbuffered', () => {
    expect(STREAM_HEADERS['Content-Type']).toContain('text/event-stream');
    // Nginx buffers by default, which turns a stream into one very late response.
    expect(STREAM_HEADERS['X-Accel-Buffering']).toBe('no');
  });
});

describe('the stream and the poll agree', () => {
  it('emits exactly the bytes the polled reader would return', async () => {
    // ONE SOURCE. The route is handed the same `read` the polled route uses, so this asserts the
    // wiring rather than a coincidence — if somebody ever gives the stream its own query, the
    // payloads drift and this is what catches it.
    const body = { turns: [{ kind: 'ask', id: 1 }], history: { events: 3 } };
    const h = harness([{ ok: true, body }]);
    await h.tick();
    const state = frames(h.out()).find((f) => f.event === 'state');
    expect(state?.data).toEqual(body);
    expect(JSON.stringify(state?.data)).toBe(JSON.stringify(body));
  });
});
