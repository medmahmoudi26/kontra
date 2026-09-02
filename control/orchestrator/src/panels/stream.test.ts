import { describe, expect, it } from 'vitest';
import { FrameBudget, MAX_BYTES_PER_SEC, MAX_FRAMES_PER_SEC, Ring, RING_BYTES } from './stream';

describe('the per-Terminal ring', () => {
  it('holds 1 MiB and drops the oldest bytes past it', () => {
    expect(RING_BYTES).toBe(1024 * 1024);
    const ring = new Ring(16);
    ring.push(Buffer.from('aaaaaaaa'));
    ring.push(Buffer.from('bbbbbbbb'));
    expect(ring.bytes).toBe(16);
    ring.push(Buffer.from('cccc'));
    expect(ring.bytes).toBe(16);
    // The oldest four bytes went, not the newest four: a tile shows the END of a stream.
    expect(ring.concat().toString()).toBe('aaaabbbbbbbbcccc');
  });

  it('keeps the tail of a chunk that is bigger than the whole ring', () => {
    const ring = new Ring(8);
    ring.push(Buffer.from('0123456789'));
    expect(ring.concat().toString()).toBe('23456789');
  });

  it('is replaced wholesale by a snapshot', () => {
    // A snapshot is the entire screen, so appending would grow the tile forever.
    const ring = new Ring(1024);
    ring.push(Buffer.from('old screen'));
    ring.reset(Buffer.from('new screen'));
    expect(ring.concat().toString()).toBe('new screen');
  });
});

describe('the per-Terminal send budget', () => {
  const at = (t: { v: number }) => () => t.v;

  it('passes bytes straight through under the caps', () => {
    const t = { v: 1000 };
    const b = new FrameBudget(at(t));
    const first = b.offer(Buffer.from('screen'));
    expect(first.send?.toString()).toBe('screen');
    expect(b.takeElided()).toBe(0);
  });

  it('holds bytes once 15 frames have gone out inside one second', () => {
    const t = { v: 0 };
    const b = new FrameBudget(at(t));
    for (let i = 0; i < MAX_FRAMES_PER_SEC; i += 1) {
      t.v += 10;
      expect(b.offer(Buffer.from('x')).send, `frame ${i}`).not.toBeNull();
    }
    t.v += 10;
    expect(b.offer(Buffer.from('y')).send).toBeNull();
    expect(b.held).toBe(1);
    // …and releases them once the window has rolled.
    t.v += 1000;
    expect(b.drain().send?.toString()).toBe('y');
  });

  it('elides the OLDEST held bytes and reports exactly how many', () => {
    const t = { v: 0 };
    const b = new FrameBudget(at(t), 1, MAX_BYTES_PER_SEC);
    b.offer(Buffer.from('first frame')); // spends the single frame allowed this second
    b.offer(Buffer.alloc(MAX_BYTES_PER_SEC, 'a')); // held: fills the hold exactly
    b.offer(Buffer.from('newest')); // pushes 6 bytes of the oldest out
    const dropped = b.takeElided();
    expect(dropped).toBe(6);
    // Reported once, not forever.
    expect(b.takeElided()).toBe(0);
    // What survived ends with the newest bytes.
    t.v += 1000;
    const released = b.drain().send;
    expect(released?.subarray(released.length - 6).toString()).toBe('newest');
  });

  it('caps at 256 KiB per second, and the overflow is elided rather than queued', () => {
    const t = { v: 0 };
    const b = new FrameBudget(at(t));
    const sent = b.offer(Buffer.alloc(MAX_BYTES_PER_SEC + 4096, 'z'));
    // One second's worth leaves. The 4 KiB that did not fit the hold is DROPPED, not queued — a
    // queue that grows with the overflow is exactly the retention ADR 0020 measured on a Machine,
    // moved into this process.
    expect(sent.send?.length).toBe(MAX_BYTES_PER_SEC);
    expect(b.takeElided()).toBe(4096);
    expect(b.held).toBe(0);
    // The next second's budget is available again.
    t.v += 1001;
    expect(b.offer(Buffer.from('later')).send?.toString()).toBe('later');
  });
});
