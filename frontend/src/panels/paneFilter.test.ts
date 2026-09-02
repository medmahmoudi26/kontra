import { describe, expect, it } from 'vitest';

import type { Terminal } from './panelsClient';
import {
  ANY,
  EMPTY_FILTER,
  filterActive,
  hiddenPanes,
  matchPane,
  paneActor,
  paneAddress,
  paneOptions,
  paneSession,
} from './paneFilter';

function pane(over: Partial<Terminal> & { id: string }): Terminal {
  return {
    machine: 'node-1',
    host: 'node-1.kontra.internal',
    publicIp: '10.124.0.5',
    tag: 'worker',
    fleet: 'apex',
    actor: 'nscheck',
    version: '0.1.0',
    window: '0',
    health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
    ...over,
  };
}

const fleet = pane({ id: 'fleet:node-1/kontra-nscheck/0' });
const other = pane({
  id: 'fleet:node-2/kontra-nscheck/0',
  machine: 'node-2',
  host: 'node-2.kontra.internal',
  publicIp: '10.124.0.6',
});
const local = pane({
  id: 'local:host/kontra-serve-nscheck/0',
  machine: 'host',
  host: 'controller',
  publicIp: '',
  actor: '',
  version: '',
});

describe('reading a Terminal', () => {
  // A local `kontra workflow serve … --tmux` session has no public address. That is a CATEGORY,
  // not missing data, and an empty cell in a filter menu is unselectable.
  it('calls an addressless Terminal local', () => {
    expect(paneAddress(local)).toBe('local');
    expect(paneAddress(fleet)).toBe('10.124.0.5');
  });

  it('calls an actorless Terminal unregistered rather than blank', () => {
    expect(paneActor(local)).toBe('unregistered');
    expect(paneActor(fleet)).toBe('nscheck');
  });

  it('takes the session from the id, which is where the grammar puts it', () => {
    expect(paneSession(fleet)).toBe('kontra-nscheck');
    expect(paneSession(local)).toBe('kontra-serve-nscheck');
  });
});

describe('paneOptions', () => {
  it('offers exactly the values the inventory has, sorted and deduped', () => {
    const opts = paneOptions([fleet, other, local]);
    expect(opts.actors).toEqual(['nscheck', 'unregistered']);
    expect(opts.ips).toEqual(['10.124.0.5', '10.124.0.6', 'local']);
    expect(opts.sessions).toEqual(['kontra-nscheck', 'kontra-serve-nscheck']);
  });

  it('is empty for an empty wall rather than offering a stale menu', () => {
    expect(paneOptions([])).toEqual({ actors: [], ips: [], sessions: [] });
  });
});

describe('filterActive', () => {
  it('is false for the resting state', () => {
    expect(filterActive(EMPTY_FILTER)).toBe(false);
  });

  it('is true for each control on its own', () => {
    expect(filterActive({ ...EMPTY_FILTER, actor: 'nscheck' })).toBe(true);
    expect(filterActive({ ...EMPTY_FILTER, ip: 'local' })).toBe(true);
    expect(filterActive({ ...EMPTY_FILTER, session: 'kontra-nscheck' })).toBe(true);
    expect(filterActive({ ...EMPTY_FILTER, liveOnly: true })).toBe(true);
    expect(filterActive({ ...EMPTY_FILTER, query: 'node' })).toBe(true);
  });

  it('treats whitespace as no query', () => {
    expect(filterActive({ ...EMPTY_FILTER, query: '   ' })).toBe(false);
  });
});

describe('matchPane', () => {
  const none = new Set<string>();

  it('keeps everything with no filter', () => {
    expect(matchPane(fleet, EMPTY_FILTER, none)).toBe(true);
    expect(matchPane(local, EMPTY_FILTER, none)).toBe(true);
  });

  it('narrows on each menu exactly', () => {
    expect(matchPane(fleet, { ...EMPTY_FILTER, actor: 'nscheck' }, none)).toBe(true);
    expect(matchPane(local, { ...EMPTY_FILTER, actor: 'nscheck' }, none)).toBe(false);
    expect(matchPane(other, { ...EMPTY_FILTER, ip: '10.124.0.5' }, none)).toBe(false);
    expect(matchPane(fleet, { ...EMPTY_FILTER, session: 'kontra-nscheck' }, none)).toBe(true);
  });

  // Three menus and a box on one row read as "all of these at once".
  it('ANDs the clauses', () => {
    const f = { ...EMPTY_FILTER, actor: 'nscheck', ip: '10.124.0.6' };
    expect(matchPane(fleet, f, none)).toBe(false);
    expect(matchPane(other, f, none)).toBe(true);
  });

  it('reads live from the page, not from the inventory', () => {
    const f = { ...EMPTY_FILTER, liveOnly: true };
    expect(matchPane(fleet, f, none)).toBe(false);
    expect(matchPane(fleet, f, new Set([fleet.id]))).toBe(true);
  });

  it('searches every field, because a fragment does not know which box it belongs in', () => {
    expect(matchPane(fleet, { ...EMPTY_FILTER, query: '10.124' }, none)).toBe(true);
    expect(matchPane(fleet, { ...EMPTY_FILTER, query: 'internal' }, none)).toBe(true);
    expect(matchPane(fleet, { ...EMPTY_FILTER, query: '0.1.0' }, none)).toBe(true);
    expect(matchPane(fleet, { ...EMPTY_FILTER, query: 'nope' }, none)).toBe(false);
  });

  it('is case-insensitive', () => {
    expect(matchPane(fleet, { ...EMPTY_FILTER, query: 'NSCHECK' }, none)).toBe(true);
  });
});

describe('hiddenPanes', () => {
  const live = new Set<string>();

  // The wall keeps every tile in its saved document; only what is drawn shrinks. An unfiltered wall
  // must therefore hide NOTHING, or every tile would take the filtered code path for no reason.
  it('hides nothing at rest', () => {
    expect(hiddenPanes([fleet, other, local], EMPTY_FILTER, live).size).toBe(0);
  });

  it('returns the complement of the matches', () => {
    const hidden = hiddenPanes([fleet, other, local], { ...EMPTY_FILTER, ip: 'local' }, live);
    expect([...hidden].sort()).toEqual([fleet.id, other.id].sort());
  });

  it('can hide everything — which the wall must say differently from an empty Fleet', () => {
    expect(hiddenPanes([fleet, other], { ...EMPTY_FILTER, query: 'zzz' }, live).size).toBe(2);
  });
});

describe('ANY', () => {
  it('is not the empty string, so an unregistered actor stays selectable', () => {
    expect(ANY).not.toBe('');
    expect(matchPane(local, { ...EMPTY_FILTER, actor: 'unregistered' }, new Set())).toBe(true);
  });
});
