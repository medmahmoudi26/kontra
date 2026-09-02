/**
 * The inline row tail's arithmetic and its vocabulary (issue 25).
 *
 * WHAT IS PINNED HERE IS THE HONESTY, not the formatting. The preview route this reads takes a
 * `limit` and nothing else — no offset, no ordering — so "the last ten rows" is only true when the
 * read reached the END of the Dataset, and every case where it did not must come back labelled as
 * something else. A read that quietly showed the first ten rows of a 223,378-row Dataset under the
 * word "last" is the same class of lie as an `open` Dataset drawn as `sealed`, and it is the one
 * this module could most easily tell.
 *
 * Node, no jsdom, as everywhere else here: these are pure functions and the component that calls
 * them is asserted as markup in `panels/datasetTail.render.test.ts`.
 */

import { describe, expect, it } from 'vitest';

import {
  TAIL_ROWS,
  TAIL_START,
  TAIL_WINDOW,
  columnSignature,
  emptyTail,
  readTail,
  tailCell,
  tailFollows,
  tailPlan,
  tailReachNote,
  tailReduce,
  tailStateWords,
  tailSummaryWords,
  tailWords,
  type TailColumn,
} from './tailRows';
import { DATASET_STATES, type DatasetBadgeState } from './state';

const COLUMNS: TailColumn[] = [
  { name: 'host', type: 'VARCHAR' },
  { name: 'ok', type: 'BOOLEAN' },
];

/** `n` rows, each carrying its own index so a slice can be checked for WHICH rows it took. */
function rows(n: number, from = 0): unknown[][] {
  return Array.from({ length: n }, (_, i) => [`h${from + i}`, true]);
}

const AT = 1_787_084_868_000;

describe('planning the read', () => {
  it('asks for one row more than it needs, so a full answer is detectable', () => {
    // Without the extra row, a Dataset of exactly TAIL_WINDOW rows and one of ten times that come
    // back identical — and one of the two would be called a tail wrongly.
    expect(tailPlan(3).limit).toBe(TAIL_ROWS + 1);
    expect(tailPlan(400).limit).toBe(TAIL_WINDOW + 1);
    expect(tailPlan(TAIL_WINDOW).limit).toBe(TAIL_WINDOW + 1);
  });

  it('does not read five hundred rows to show ten of the middle', () => {
    // Past the window there is no tail to be had inline, so the plan stops pretending: ten rows,
    // called the first ten. Reading the whole thing for a mid-Dataset slice would be expensive AND
    // dishonest, which is a rare combination worth refusing twice.
    const plan = tailPlan(223_378);
    expect(plan.limit).toBe(TAIL_ROWS);
    expect(plan.attempt).toBe('head');
  });

  it('asks for nothing at all when the catalog counts no rows', () => {
    expect(tailPlan(0).limit).toBe(0);
    expect(tailPlan(-1).limit).toBe(0);
    expect(tailPlan(Number.NaN).limit).toBe(0);
  });
});

describe('reading the answer', () => {
  it('takes the LAST rows when the read reached the end', () => {
    const plan = tailPlan(42);
    const read = readTail({ columns: COLUMNS, rows: rows(42) }, plan, AT);
    expect(read.reach).toBe('tail');
    expect(read.rows).toHaveLength(TAIL_ROWS);
    // rows 32..41 — the end of the Dataset, not the beginning of the window.
    expect(read.rows[0]?.[0]).toBe('h32');
    expect(read.rows[9]?.[0]).toBe('h41');
    expect(tailWords(read)).toBe('the last 10 of 42 rows');
  });

  it('refuses to call a clipped read a tail, and takes the first rows instead', () => {
    // The catalog said 400 so a tail was attempted; 501 rows came back, which means the Dataset has
    // at least one row past the window. These are its beginning. Saying "the last 10" here is the
    // exact failure this module exists to prevent.
    const plan = tailPlan(400);
    const read = readTail({ columns: COLUMNS, rows: rows(TAIL_WINDOW + 1) }, plan, AT);
    expect(read.reach).toBe('head');
    expect(read.rows[0]?.[0]).toBe('h0');
    expect(tailWords(read)).toContain('the first 10 rows');
    expect(tailReachNote(read)).toContain('one click away in the console');
  });

  it('catches a Dataset that grew between the listing poll and the read', () => {
    // Planned from a count of 6, so it asked for 11 and got 11: the Dataset is longer than the
    // catalog said. A read that cannot see the end does not claim one.
    const plan = tailPlan(6);
    const read = readTail({ columns: COLUMNS, rows: rows(TAIL_ROWS + 1) }, plan, AT);
    expect(read.reach).toBe('head');
    expect(read.whole).toBe(false);
  });

  it('says a short Dataset is all of it, rather than implying more above', () => {
    const plan = tailPlan(3);
    const read = readTail({ columns: COLUMNS, rows: rows(3) }, plan, AT);
    expect(read.whole).toBe(true);
    expect(read.rows).toHaveLength(3);
    expect(tailWords(read)).toBe('all 3 rows — that is the whole of it');
    expect(tailReachNote(read)).toBeNull();
  });

  it('counts one row without pluralising it', () => {
    const read = readTail({ columns: COLUMNS, rows: rows(1) }, tailPlan(1), AT);
    expect(tailWords(read)).toBe('all 1 row — that is the whole of it');
  });

  it('measures the denominator itself when it reached the end', () => {
    // The catalog's count was taken earlier and can be stale; a tail read SAW the whole Dataset, so
    // its own count is the better number and the words use it.
    const read = readTail({ columns: COLUMNS, rows: rows(42) }, tailPlan(37), AT);
    expect(read.seen).toBe(42);
    expect(tailWords(read)).toContain('of 42 rows');
  });

  it('keeps the catalog total as the only honest denominator for a head read', () => {
    const read = readTail({ columns: COLUMNS, rows: rows(TAIL_ROWS) }, tailPlan(223_378), AT);
    expect(tailWords(read)).toContain('223,378 in all');
    // Never the number it saw: it saw ten, and "10 in all" would be a measurement it did not make.
    expect(tailWords(read)).not.toContain('10 in all');
  });

  it('answers a zero-row Dataset without a request', () => {
    const read = emptyTail(tailPlan(0), AT);
    expect(read.seen).toBe(0);
    expect(read.rows).toHaveLength(0);
    expect(tailWords(read)).toBe('no rows');
  });
});

describe('the four lifecycle states survive the expander', () => {
  it('follows only an open Dataset', () => {
    expect(tailFollows('open')).toBe(true);
    for (const s of ['sealed', 'abandoned', 'none'] as DatasetBadgeState[]) {
      expect(tailFollows(s)).toBe(false);
    }
  });

  it('gives each of the four its own sentence about THESE rows', () => {
    // A table is the most finished-looking thing a surface can draw, so the open case must not read
    // as a result — and `no lifecycle` must not be folded into `sealed`, which is §11's one rule.
    const said = new Set<string>();
    for (const s of [...DATASET_STATES, 'none'] as DatasetBadgeState[]) {
      said.add(tailStateWords(s));
    }
    expect(said.size).toBe(4);
    expect(tailStateWords('open')).toContain('follows');
    expect(tailStateWords('open')).not.toContain('will not change');
    expect(tailStateWords('sealed')).toContain('will not change');
    expect(tailStateWords('none')).toContain('No lifecycle');
  });
});

describe('what the twisty promises before it is opened', () => {
  it('promises the first rows, not the last, for a Dataset it cannot tail', () => {
    // The lie would only be discovered after clicking, which is the worst place to put one.
    expect(tailSummaryWords(223_378)).toBe('the first 10 rows');
    expect(tailSummaryWords(400)).toBe('the last 10 rows');
    expect(tailSummaryWords(4)).toBe('all 4 rows');
    expect(tailSummaryWords(1)).toBe('all 1 row');
    expect(tailSummaryWords(0)).toBe('no rows to show');
  });
});

describe('the header is keyed on the schema, not on the read', () => {
  it('is unchanged when only the rows change', () => {
    // ISSUE #14's RULE, on this table. An open Dataset re-reads every time its count moves and every
    // one of those reads returns the same columns; a header rebuilt off the read object would be
    // handed new cells ten times a minute for no change at all.
    const before = columnSignature(COLUMNS);
    const after = columnSignature([...COLUMNS.map((c) => ({ ...c }))]);
    expect(after).toBe(before);
  });

  it('changes when a column is added, renamed or retyped', () => {
    const base = columnSignature(COLUMNS);
    expect(columnSignature([...COLUMNS, { name: 'ns', type: 'VARCHAR' }])).not.toBe(base);
    expect(columnSignature([{ name: 'ip', type: 'VARCHAR' }, COLUMNS[1]!])).not.toBe(base);
    expect(columnSignature([COLUMNS[0]!, { name: 'ok', type: 'VARCHAR' }])).not.toBe(base);
  });

  it('does not collide two schemas onto one signature', () => {
    // A DuckDB type carries spaces, commas and parens (`DECIMAL(10, 2)`), so the join uses control
    // characters no identifier or type can contain.
    const a = columnSignature([{ name: 'a', type: 'INT b' }]);
    const b = columnSignature([{ name: 'a', type: 'INT' }, { name: 'b', type: '' }]);
    expect(a).not.toBe(b);
  });
});

describe('cells speak DuckDB', () => {
  it('prints a struct the way every other window onto these rows does', () => {
    expect(tailCell({ ns: 'a.example.com', ok: true }).text).toBe("{'ns': a.example.com, 'ok': true}");
    expect(tailCell(null).text).toBe('NULL');
  });

  it('clips a body to a row and keeps the whole of it for the title', () => {
    const body = 'x'.repeat(400);
    const cell = tailCell(body, 20);
    expect(cell.clipped).toBe(true);
    expect(cell.text).toHaveLength(21); // 20 + the ellipsis
    expect(cell.full).toBe(body);
  });
});

describe('the read state machine', () => {
  it('keeps the rows it has across a re-read', () => {
    // An open Dataset re-reads whenever its count moves. Blanking the table for the duration would
    // turn "a row landed" into a flicker, which is the animation this surface may not have.
    const ready = tailReduce(TAIL_START, {
      type: 'rows',
      read: readTail({ columns: COLUMNS, rows: rows(12) }, tailPlan(12), AT),
    });
    const rereading = tailReduce(ready, { type: 'read' });
    expect(rereading.phase).toBe('reading');
    expect(rereading.read).toBe(ready.read);
  });

  it('keeps them across a failure too, and carries the server’s own sentence', () => {
    const ready = tailReduce(TAIL_START, {
      type: 'rows',
      read: readTail({ columns: COLUMNS, rows: rows(12) }, tailPlan(12), AT),
    });
    const failed = tailReduce(ready, { type: 'failed', error: 'no output dataset named "lame"' });
    expect(failed.phase).toBe('failed');
    expect(failed.read).toBe(ready.read);
    expect(failed.error).toContain('no output dataset named');
    // A retry clears the error but not the rows.
    const retry = tailReduce(failed, { type: 'read' });
    expect(retry.error).toBeNull();
    expect(retry.read).toBe(ready.read);
  });

  it('starts having read nothing, which is not the same as having read no rows', () => {
    expect(TAIL_START.phase).toBe('unopened');
    expect(TAIL_START.read).toBeNull();
  });
});
