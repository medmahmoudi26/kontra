import { describe, expect, it } from 'vitest';

import { deletionConfirm } from './DatasetPage';

/**
 * The confirmation before a temporary Dataset is dropped (temp-datasets slice 04).
 *
 * Deletion is a real destructive act on data, so the dialog NAMES what goes rather than asking a
 * bare "are you sure?"; and an `open` temp — one that may belong to a Run still in flight — gets an
 * extra sentence, because pulling the destination out from under a live run must be a
 * deliberate act, never a single silent click. Pure text, so it is pinned here without a DOM.
 */
describe('the deletion confirmation names what it will remove', () => {
  it('names the Dataset, its rows, its size and its owner Run', () => {
    const msg = deletionConfirm({
      name: 'tmp_a7f3',
      rows: 1234,
      bytes: 4_718_592,
      owner: 'run-42',
      state: 'sealed',
    });
    expect(msg).toContain('"tmp_a7f3"');
    expect(msg).toContain('1,234 rows');
    expect(msg).toContain('4.5 MB');
    expect(msg).toContain('owned by run run-42');
    expect(msg).toContain('cannot be undone');
  });

  it('spells out the consequence ONLY for an open temp', () => {
    const open = deletionConfirm({ name: 'tmp_x', rows: 5, bytes: 10, owner: 'r', state: 'open' });
    const sealed = deletionConfirm({ name: 'tmp_x', rows: 5, bytes: 10, owner: 'r', state: 'sealed' });
    // The open temp warns that a Run may still be appending; the sealed one has no live run to
    // pull the destination out from under, so it must not cry wolf.
    expect(open).toContain('still OPEN');
    expect(open).toContain('pulls the destination out from under');
    expect(sealed).not.toContain('still OPEN');
  });

  /**
   * A TAGGED TEMP IS THE CASE WHERE THE TWO DELETERS COULD DISAGREE, and this is where they are
   * reconciled in front of the person deciding.
   *
   * A tag means KEEP (ADR 0029 §3) and the page draws it as a chip. It does not, and must not, stop
   * this button: a temporary Dataset is removed by asking (ADR 0028), the record is keyed by the
   * owning **Run**, and the retention sweep never takes a temp on a clock either way. So the delete
   * NAMES the tags instead of silently destroying something the page has been calling kept — the
   * "two things that can delete a Dataset disagreeing about one tag" shape §5 exists to prevent.
   */
  it('names the tags on a tagged temp, and says the tag does not stop the delete', () => {
    const msg = deletionConfirm({
      name: 'tmp_a7f3',
      rows: 10,
      bytes: 1024,
      owner: 'nscheck-1',
      state: 'sealed',
      tags: ['keep-this', 'prod'],
    });
    expect(msg).toContain('TAGGED "keep-this", "prod"');
    expect(msg).toContain('does not stop this delete');
    // And what the tag DOES keep is stated, because that is the reason it was worth putting on.
    expect(msg).toContain('durable output keeps the tag');
  });

  it('says nothing about tags on an untagged temp', () => {
    const msg = deletionConfirm({ name: 'tmp_a7f3', rows: 10, bytes: 1024, owner: 'r', state: 'sealed', tags: [] });
    expect(msg).not.toContain('TAGGED');
    expect(deletionConfirm({ name: 'tmp_a7f3', rows: 10, bytes: 1024, owner: 'r', state: 'sealed' })).not.toContain(
      'TAGGED'
    );
  });

  it('reads singular for a one-row temp and tolerates an owner-less one', () => {
    const msg = deletionConfirm({ name: 'tmp_x', rows: 1, bytes: 0, state: 'abandoned' });
    expect(msg).toContain('1 row ');
    expect(msg).not.toContain('1 rows');
    // No owner marker survived (the safe-durable direction, slice 01): no dangling "owned by".
    expect(msg).not.toContain('owned by');
    // And no data yet is said plainly rather than as an empty size.
    expect(msg).toContain('no data yet');
  });
});
