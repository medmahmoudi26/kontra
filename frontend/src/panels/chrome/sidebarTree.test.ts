/**
 * The sidebar tree's KEYBOARD, and the promise a click has to keep.
 *
 * `tree.ts` decides the shape and the rollups and is tested beside itself; what it cannot say is
 * what the component does with a key press, and that is the whole of this file's subject. The tree
 * has ONE tab-stop and a cursor that lives in `useState` — arrows to move, Left/Right to fold,
 * Enter to reveal — none of which is expressible in a static render, and all of which is what an
 * operator navigating a fleet of twelve Machines actually touches.
 *
 * THE RULE WITH ADR 0020'S WEIGHT ON IT is the second describe: clicking a leaf REVEALS and must
 * never promote. A live tile is an `sshd` session, a PTY and a per-viewer tmux session on an
 * `s-1vcpu-2gb` Machine, and slice 2 amendment 9 states the rule as "never a click, never a stray
 * tab-stop" — a tree of clickable Machine names being the easiest place in this UI to break it by
 * accident. A test that only rendered markup could confirm the tooltip SAYS so; only a fired
 * handler can confirm it does so.
 */

import { createElement } from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import SidebarTree from './SidebarTree';
import type { Terminal, TerminalHealth } from '../panelsClient';

const OK: TerminalHealth = {
  reachable: 'ok',
  session: 'present',
  process: 'running',
  poller: 'live',
  loads: 'ok',
};

/** `mode:node/session/window`, the shape `parseTileRef` reads. The fields are derived from the id so
 *  the fixture cannot claim a machine the id does not name. */
function terminal(id: string, health: TerminalHealth = OK): Terminal {
  const [, rest = ''] = id.split(':');
  const [machine = '', , window = ''] = rest.split('/');
  return {
    id,
    machine,
    host: '10.124.0.3',
    publicIp: '',
    tag: 'crawl',
    fleet: 'webcrawl',
    actor: 'crawler',
    version: '1.2.3',
    window,
    health,
  };
}

const INVENTORY = [
  terminal('fleet:kf-01/kontra-webcrawl/actor'),
  terminal('fleet:kf-01/kontra-webcrawl/handler'),
  terminal('fleet:kf-02/kontra-webcrawl/actor'),
];

const onReveal = vi.fn();
const onSelectNode = vi.fn();
const onToggle = vi.fn();

function draw(over: Partial<Parameters<typeof SidebarTree>[0]> = {}): ReturnType<typeof render> {
  return render(
    createElement(SidebarTree, {
      inventory: INVENTORY,
      onWall: new Set(INVENTORY.map((t) => t.id)),
      live: new Set<string>(),
      onReveal,
      onSelectNode,
      open: true,
      onToggle,
      ...over,
    })
  );
}

/** The one tab-stop. Every key in this file goes through it, exactly as an operator's would. */
function tree(): HTMLElement {
  return screen.getByRole('tree');
}

function cursorKey(): string | null {
  const row = document.querySelector('[data-cursor="true"]');
  return row?.getAttribute('data-testid')?.replace('tree-row-', '') ?? null;
}

beforeEach(() => {
  localStorage.clear();
  vi.clearAllMocks();
});

describe('the tree it draws', () => {
  it('is closed by default, and counts MACHINES on the button rather than Terminals', () => {
    // Two Machines, three Terminals. The button says Machines because that is what the tree is a
    // tree of; the tooltip is where the Terminal count is, and both are on the closed control so
    // the tree is worth opening before it is opened.
    draw({ open: false });
    const button = screen.getByTestId('sidebar-open');
    expect(button.textContent).toContain('Machines 2');
    expect(button.getAttribute('title')).toContain('3 Terminals');
    expect(screen.queryByRole('tree')).toBeNull();
  });

  it('opens on the toggle rather than by remembering its own state', () => {
    draw({ open: false });
    fireEvent.click(screen.getByTestId('sidebar-open'));
    // The PAGE owns whether the tree is open — the wall re-fits every tile when it changes.
    expect(onToggle).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('tree')).toBeNull();
  });

  it('draws a row for every Terminal, whether or not it is on the wall', () => {
    draw({ onWall: new Set(['fleet:kf-01/kontra-webcrawl/actor']) });
    for (const t of INVENTORY) expect(screen.getByTestId(`tree-row-${t.id}`)).toBeTruthy();
    expect(screen.getByTestId('tree-row-fleet:kf-02/kontra-webcrawl/actor').dataset.onWall).toBe(
      'false'
    );
  });

  it('says an empty inventory means no Machines, not a broken tree', () => {
    draw({ inventory: [] });
    expect(screen.getByText(/an empty tree means no/i)).toBeTruthy();
  });
});

describe('what a click is allowed to do', () => {
  it('reveals the Terminal and promotes NOTHING', () => {
    draw();
    fireEvent.click(screen.getByTestId('tree-row-fleet:kf-01/kontra-webcrawl/handler'));
    expect(onReveal).toHaveBeenCalledWith('fleet:kf-01/kontra-webcrawl/handler');
    // The reveal is the whole of it: no second callback exists on this component that could attach.
    expect(onReveal).toHaveBeenCalledTimes(1);
  });

  it('folds a branch instead of revealing, because a branch is not a Terminal', () => {
    draw();
    fireEvent.click(screen.getByTestId('tree-row-fleet:kf-01'));
    expect(onReveal).not.toHaveBeenCalled();
    expect(screen.queryByTestId('tree-row-fleet:kf-01/kontra-webcrawl/actor')).toBeNull();
    // The other Machine is untouched — folding is per branch, not per tree.
    expect(screen.getByTestId('tree-row-fleet:kf-02/kontra-webcrawl/actor')).toBeTruthy();
  });

  it('narrows to one Machine without revealing anything', () => {
    draw();
    fireEvent.click(screen.getByTestId('tree-only-fleet:kf-01'));
    expect(onSelectNode).toHaveBeenCalledWith('kf-01');
    // The `only` button is inside a clickable row; without `stopPropagation` it would also fold it.
    expect(screen.getByTestId('tree-row-fleet:kf-01/kontra-webcrawl/actor')).toBeTruthy();
  });

  it('folds everything from one control', () => {
    draw();
    fireEvent.click(screen.getByTestId('sidebar-collapse-all'));
    expect(screen.queryByTestId('tree-row-fleet:kf-01')).toBeNull();
    expect(screen.getByTestId('tree-row-fleet')).toBeTruthy();
  });
});

describe('the keyboard', () => {
  it('starts the cursor at the top on the first key, wherever the tree is scrolled', () => {
    draw();
    expect(cursorKey()).toBeNull();
    fireEvent.keyDown(tree(), { key: 'ArrowDown' });
    expect(cursorKey()).toBe('fleet');
  });

  it('walks down and back up the flattened rows', () => {
    draw();
    fireEvent.keyDown(tree(), { key: 'ArrowDown' });
    fireEvent.keyDown(tree(), { key: 'ArrowDown' });
    expect(cursorKey()).toBe('fleet:kf-01');
    fireEvent.keyDown(tree(), { key: 'ArrowUp' });
    expect(cursorKey()).toBe('fleet');
  });

  it('stops at both ends rather than wrapping', () => {
    draw();
    fireEvent.keyDown(tree(), { key: 'Home' });
    fireEvent.keyDown(tree(), { key: 'ArrowUp' });
    expect(cursorKey()).toBe('fleet');
    fireEvent.keyDown(tree(), { key: 'End' });
    const last = cursorKey();
    fireEvent.keyDown(tree(), { key: 'ArrowDown' });
    expect(cursorKey()).toBe(last);
  });

  it('takes j and k as well, because this is a tree over tmux', () => {
    draw();
    fireEvent.keyDown(tree(), { key: 'j' });
    fireEvent.keyDown(tree(), { key: 'j' });
    expect(cursorKey()).toBe('fleet:kf-01');
    fireEvent.keyDown(tree(), { key: 'k' });
    expect(cursorKey()).toBe('fleet');
  });

  it('folds with Left and expands with Right, and never on a leaf', () => {
    draw();
    fireEvent.keyDown(tree(), { key: 'ArrowDown' });
    fireEvent.keyDown(tree(), { key: 'ArrowDown' });
    expect(cursorKey()).toBe('fleet:kf-01');
    fireEvent.keyDown(tree(), { key: 'ArrowLeft' });
    expect(screen.queryByTestId('tree-row-fleet:kf-01/kontra-webcrawl')).toBeNull();
    fireEvent.keyDown(tree(), { key: 'ArrowRight' });
    expect(screen.getByTestId('tree-row-fleet:kf-01/kontra-webcrawl')).toBeTruthy();
  });

  it('reveals with Enter, and only from a leaf', () => {
    draw();
    fireEvent.keyDown(tree(), { key: 'End' });
    fireEvent.keyDown(tree(), { key: 'Enter' });
    expect(onReveal).toHaveBeenCalledWith('fleet:kf-02/kontra-webcrawl/actor');

    onReveal.mockClear();
    fireEvent.keyDown(tree(), { key: 'Home' });
    fireEvent.keyDown(tree(), { key: 'Enter' });
    expect(onReveal).not.toHaveBeenCalled();
  });

  it('keeps the cursor on a row that still exists when a Machine leaves the Fleet', () => {
    // The cursor is a row KEY and not an index precisely so the 30-second inventory refresh cannot
    // move it under the operator — and a cursor pointing at nothing presents as a tree that has
    // stopped responding to the arrow keys.
    const view = draw();
    fireEvent.keyDown(tree(), { key: 'End' });
    expect(cursorKey()).toBe('fleet:kf-02/kontra-webcrawl/actor');
    view.rerender(
      createElement(SidebarTree, {
        inventory: INVENTORY.slice(0, 2),
        onWall: new Set(INVENTORY.slice(0, 2).map((t) => t.id)),
        live: new Set<string>(),
        onReveal,
        onSelectNode,
        open: true,
        onToggle,
      })
    );
    expect(cursorKey()).toBe('fleet');
    fireEvent.keyDown(tree(), { key: 'ArrowDown' });
    expect(cursorKey()).toBe('fleet:kf-01');
  });
});
