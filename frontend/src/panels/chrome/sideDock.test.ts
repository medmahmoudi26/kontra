/**
 * The two rules a movable sidecar lives or dies by.
 *
 * THE SIDE DECIDES THE DIRECTION. A panel docked left grows as the pointer moves right; docked right
 * the same movement shrinks it. Backwards, the panel runs away from the cursor — obvious the first
 * time anyone drags it and invisible in review, which is why it is arithmetic with a test rather
 * than a sign buried in a component.
 *
 * A CORRUPT STORED LAYOUT COSTS THE DEFAULT, NEVER THE PAGE. And never HALF the default: a width
 * that survives with a side that does not is how a panel comes back at the right size on the wrong
 * edge, which reads as the app having moved it by itself.
 */

import { describe, expect, it } from 'vitest';
import {
  clampSideWidth,
  dragWidth,
  flipSide,
  keyWidth,
  parseSideDock,
  sideDockKey,
  SIDE_BOUNDS,
  SIDE_STEP,
  type SideDockState,
} from './sideDock';

const FALLBACK: SideDockState = { width: 264, side: 'left', collapsed: false };

describe('clampSideWidth', () => {
  it('holds the panel between narrow-but-a-panel and wide-but-still-a-page', () => {
    expect(clampSideWidth(10)).toBe(SIDE_BOUNDS.min);
    expect(clampSideWidth(5000)).toBe(SIDE_BOUNDS.max);
    expect(clampSideWidth(300)).toBe(300);
  });

  it('answers the minimum for a value that is not a number', () => {
    // A stored NaN must not become a width; a zero-width panel is one an operator has to know to
    // drag back from an invisible edge.
    expect(clampSideWidth(Number.NaN)).toBe(SIDE_BOUNDS.min);
    expect(clampSideWidth(Number.POSITIVE_INFINITY)).toBe(SIDE_BOUNDS.min);
  });
});

describe('dragging by the handle', () => {
  it('widens a left-docked panel as the pointer moves right', () => {
    expect(dragWidth(264, 40, 'left')).toBe(304);
    expect(dragWidth(264, -40, 'left')).toBe(224);
  });

  it('widens a right-docked panel as the pointer moves LEFT', () => {
    // The mirror. Same gesture, opposite meaning, because the handle is on the panel's other edge.
    expect(dragWidth(264, -40, 'right')).toBe(304);
    expect(dragWidth(264, 40, 'right')).toBe(224);
  });

  it('measures from the start of the gesture, so a pinned drag comes straight back', () => {
    // Pinned at min by a long drag left; one pixel back right is one pixel wider, not the start of
    // a journey back through everything the pointer already travelled.
    const pinned = dragWidth(264, -1000, 'left');
    expect(pinned).toBe(SIDE_BOUNDS.min);
    expect(dragWidth(264, -(264 - SIDE_BOUNDS.min) + 1, 'left')).toBe(SIDE_BOUNDS.min + 1);
  });
});

describe('the arrow keys', () => {
  it('reads the key as a direction ON SCREEN, not on the panel', () => {
    // ArrowRight moves the handle right: it widens a left panel and narrows a right one. Any other
    // reading makes one key do opposite things to two panels on one page.
    expect(keyWidth(264, 'ArrowRight', 'left')).toBe(264 + SIDE_STEP);
    expect(keyWidth(264, 'ArrowRight', 'right')).toBe(264 - SIDE_STEP);
    expect(keyWidth(264, 'ArrowLeft', 'left')).toBe(264 - SIDE_STEP);
  });

  it('answers null for a key that is not a resize, so the page keeps it', () => {
    expect(keyWidth(264, 'Enter', 'left')).toBeNull();
    expect(keyWidth(264, 'ArrowUp', 'left')).toBeNull();
  });
});

describe('flipSide', () => {
  it('is its own inverse', () => {
    expect(flipSide('left')).toBe('right');
    expect(flipSide(flipSide('left'))).toBe('left');
  });
});

describe('reading a stored layout', () => {
  it('takes the caller’s defaults when nothing is stored', () => {
    expect(parseSideDock(null, FALLBACK)).toEqual(FALLBACK);
  });

  it('reads a whole layout back', () => {
    const raw = JSON.stringify({ width: 320, side: 'right', collapsed: true });
    expect(parseSideDock(raw, FALLBACK)).toEqual({ width: 320, side: 'right', collapsed: true });
  });

  it('falls back per FIELD, so a half-written layout cannot move the panel', () => {
    // A width with no side used to be the interesting case: the panel comes back the right size on
    // the wrong edge, and nobody moved it.
    expect(parseSideDock(JSON.stringify({ width: 320 }), FALLBACK)).toEqual({
      width: 320,
      side: 'left',
      collapsed: false,
    });
    expect(parseSideDock(JSON.stringify({ side: 'nowhere' }), FALLBACK)).toEqual(FALLBACK);
  });

  it('clamps a stored width that is out of bounds', () => {
    expect(parseSideDock(JSON.stringify({ width: 9000 }), FALLBACK).width).toBe(SIDE_BOUNDS.max);
  });

  it('costs the default and not the page on anything unreadable', () => {
    expect(parseSideDock('{oh no', FALLBACK)).toEqual(FALLBACK);
    expect(parseSideDock('null', FALLBACK)).toEqual(FALLBACK);
    expect(parseSideDock('[]', FALLBACK)).toEqual(FALLBACK);
    expect(parseSideDock('7', FALLBACK)).toEqual(FALLBACK);
  });
});

describe('sideDockKey', () => {
  it('namespaces per panel, so two panels do not share one layout', () => {
    expect(sideDockKey('workflows')).not.toBe(sideDockKey('scratch-inspector'));
    expect(sideDockKey('workflows')).toContain('workflows');
  });
});
