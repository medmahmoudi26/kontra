import { describe, expect, it } from 'vitest';

import {
  SPARK_WINDOW,
  areaPoints,
  greenStreak,
  push,
  rate,
  sparkPoints,
  streakBars,
  sumSeries,
} from './spark';

describe('sparkPoints', () => {
  // THE RULE THIS FILE EXISTS FOR. A sparkline with no samples must draw nothing, because a flat
  // line at the baseline is a measured zero and "we have not measured yet" is not that.
  it('draws nothing from fewer than two samples', () => {
    expect(sparkPoints([], 100, 16)).toBe('');
    expect(sparkPoints([5], 100, 16)).toBe('');
  });

  it('spans the full width, oldest left', () => {
    const pts = sparkPoints([0, 1, 2], 100, 16).split(' ');
    expect(pts).toHaveLength(3);
    expect(pts[0]!.startsWith('0.0,')).toBe(true);
    expect(pts[2]!.startsWith('100.0,')).toBe(true);
  });

  it('puts the maximum at the top and normalises to the series itself', () => {
    const small = sparkPoints([0, 3], 100, 16);
    const large = sparkPoints([0, 3000], 100, 16);
    // Same shape: a sparkline says direction, not magnitude. The number beside it says magnitude.
    expect(small).toBe(large);
  });

  it('draws an all-zero series flat along the bottom — that is a real reading', () => {
    expect(sparkPoints([0, 0, 0], 100, 16)).toBe('0.0,15.0 50.0,15.0 100.0,15.0');
  });

  it('clamps a negative sample to the baseline rather than off the viewBox', () => {
    const pts = sparkPoints([-5, 10], 100, 16).split(' ');
    expect(pts[0]).toBe('0.0,15.0');
  });
});

describe('areaPoints', () => {
  it('closes the polyline down to the baseline at both ends', () => {
    expect(areaPoints('0.0,1.0 100.0,2.0', 100, 16)).toBe('0,16 0.0,1.0 100.0,2.0 100,16');
  });

  it('stays empty when there is no line', () => {
    expect(areaPoints('', 100, 16)).toBe('');
  });
});

describe('push', () => {
  it('appends without mutating', () => {
    const a = [1, 2];
    expect(push(a, 3)).toEqual([1, 2, 3]);
    expect(a).toEqual([1, 2]);
  });

  it('caps the window, dropping the oldest', () => {
    let s: number[] = [];
    for (let i = 0; i < SPARK_WINDOW + 10; i++) s = push(s, i);
    expect(s).toHaveLength(SPARK_WINDOW);
    expect(s[s.length - 1]).toBe(SPARK_WINDOW + 9);
    expect(s[0]).toBe(10);
  });

  it('stores a non-finite sample as zero rather than poisoning the max', () => {
    expect(push([1], Number.NaN)).toEqual([1, 0]);
  });
});

describe('rate', () => {
  it('is per second, from two cumulative counts', () => {
    expect(rate(100, 300, 2000)).toBe(100);
  });

  // A catalog listing can go DOWN — compaction, an abandoned output, a listing that raced a write.
  // "-40 units/s" is never something that happened.
  it('never reports a negative rate', () => {
    expect(rate(300, 100, 2000)).toBe(0);
  });

  it('is zero across a zero-length interval rather than infinite', () => {
    expect(rate(0, 100, 0)).toBe(0);
  });
});

describe('sumSeries', () => {
  it('adds element-wise', () => {
    expect(sumSeries([[1, 2, 3], [10, 20, 30]])).toEqual([11, 22, 33]);
  });

  // A dataset that appeared mid-sweep has FEWER samples, and they are the most recent ones. Left
  // alignment would drag its activity backwards and show a spike before the run began.
  it('aligns short series at the newest end', () => {
    expect(sumSeries([[1, 2, 3, 4], [10, 20]])).toEqual([1, 2, 13, 24]);
  });

  it('is empty for no series', () => {
    expect(sumSeries([])).toEqual([]);
  });
});

describe('streakBars', () => {
  const bars = [
    { ms: 1000, status: 'completed' as const, label: 'a' },
    { ms: 2000, status: 'failed' as const, label: 'b' },
  ];

  it('is empty for no runs', () => {
    expect(streakBars([], 100, 17)).toEqual([]);
  });

  it('scales height to the longest run in the window', () => {
    const [first, second] = streakBars(bars, 100, 17);
    expect(second!.height).toBe(17);
    expect(first!.height).toBeCloseTo(8.5, 5);
  });

  it('gives a sub-second run a visible floor rather than no bar at all', () => {
    const [tiny] = streakBars([{ ms: 1, status: 'completed', label: 'x' }, ...bars], 100, 17);
    expect(tiny!.height).toBe(3);
  });

  it('lays bars out oldest-left, evenly', () => {
    const rects = streakBars(bars, 100, 17);
    expect(rects[0]!.x).toBeCloseTo(0.4, 5);
    expect(rects[1]!.x).toBeCloseTo(50.4, 5);
  });
});

describe('greenStreak', () => {
  it('counts from the newest end and stops at the first non-success', () => {
    expect(greenStreak(['completed', 'completed', 'failed', 'completed'])).toBe(2);
  });

  it('is zero when the newest run did not complete', () => {
    expect(greenStreak(['running', 'completed'])).toBe(0);
  });

  it('is zero for no runs', () => {
    expect(greenStreak([])).toBe(0);
  });
});
