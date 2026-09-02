/**
 * The Batch a Method is called with, and what the call it makes is said to have found.
 *
 * TWO SUBJECTS, ONE MODULE, and they are the same value seen twice: the Batch this collects is
 * what the probe dispatches (ADR 0033), and `probeVerdict` is how what came back is read. A drop
 * and an empty result both return zero rows, so the distinction between them is a function with
 * tests rather than a colour choice in a component.
 */

import { describe, expect, it } from 'vitest';
import {
  addUnit,
  coerceField,
  draftFor,
  probeVerdict,
  removeUnit,
  setCell,
  setJson,
  setUnitValues,
  unitsOf,
  type BatchDraft,
} from './methodCall';
import { addRow } from './formFields';

/** Press "add" on one Unit's list — the reducers hand back the whole record, so this puts it back. */
function addListRow(draft: BatchDraft, unit: number, list: string): BatchDraft {
  if (draft.kind !== 'fields') return draft;
  const node = draft.fields.find((f) => f.name === list)!;
  return setUnitValues(draft, unit, addRow(node, draft.units[unit] ?? {}));
}

/** The Batch, or the reason there is not one — flattened so a test can say which it expected. */
function batch(draft: BatchDraft): unknown {
  const got = unitsOf(draft);
  return 'error' in got ? `REFUSED: ${got.error}` : got.units;
}

const HEAD = {
  type: 'object',
  properties: {
    url: { type: 'string' },
    depth: { type: 'integer' },
    follow: { type: 'boolean' },
    headers: { type: 'object' },
    tags: { type: 'array', items: { type: 'string' } },
    note: { type: ['string', 'null'] },
  },
  required: ['url'],
};

describe('a Method that declares its input', () => {
  it('becomes a field per property, in the order the card lists them', () => {
    const draft = draftFor(HEAD);
    expect(draft.kind).toBe('fields');
    if (draft.kind !== 'fields') return;
    expect(draft.fields.map((f) => f.name)).toEqual([
      'url',
      'depth',
      'follow',
      'headers',
      'tags',
      'note',
    ]);
    // One Unit to start: a Batch with no rows has nothing to type into.
    expect(draft.units).toHaveLength(1);
  });

  it('coerces every value by its declared type, because an input only holds strings', () => {
    // Sending `"8080"` where the Actor's model wants an int is a dispatch that fails at the far end
    // of a serve, in a tmux pane, minutes later.
    let draft = draftFor(HEAD);
    draft = setCell(draft, 0, 'url', 'https://a.test');
    draft = setCell(draft, 0, 'depth', '2');
    draft = setCell(draft, 0, 'follow', 'true');
    draft = setCell(draft, 0, 'headers', '{"accept": "*/*"}');
    // `tags` is a LIST now, so it is two rows rather than a bracket typed by hand. `headers` is not:
    // `{"type":"object"}` declares an open shape and stays the JSON box it always was.
    draft = addListRow(draft, 0, 'tags');
    draft = setCell(draft, 0, 'tags.0', 'a');
    draft = addListRow(draft, 0, 'tags');
    draft = setCell(draft, 0, 'tags.1', 'b');
    expect(batch(draft)).toEqual([
      {
        url: 'https://a.test',
        depth: 2,
        follow: true,
        headers: { accept: '*/*' },
        tags: ['a', 'b'],
      },
    ]);
  });

  it('leaves an untyped optional field OUT rather than sending an empty string', () => {
    // An Actor that reads a missing key as "use the default" and `""` as "the empty string" is the
    // ordinary case, and only one of those is what a blank input means.
    const draft = setCell(draftFor(HEAD), 0, 'url', 'https://a.test');
    expect(batch(draft)).toEqual([{ url: 'https://a.test' }]);
  });

  it('refuses a required field nobody filled in, and names it', () => {
    expect(batch(draftFor(HEAD))).toBe('REFUSED: unit 1 · url is required');
  });

  it('names the Unit as well as the field, because a Batch has more than one', () => {
    let draft = setCell(draftFor(HEAD), 0, 'url', 'https://a.test');
    draft = addUnit(draft);
    draft = setCell(draft, 1, 'url', 'https://b.test');
    draft = setCell(draft, 1, 'depth', 'deep');
    expect(batch(draft)).toBe('REFUSED: unit 2 · depth: "deep" is not a number');
  });

  it('adds and drops Units, because the Batch is the list', () => {
    let draft = setCell(draftFor(HEAD), 0, 'url', 'https://a.test');
    draft = addUnit(draft);
    draft = setCell(draft, 1, 'url', 'https://b.test');
    draft = addUnit(draft);
    draft = setCell(draft, 2, 'url', 'https://c.test');
    draft = removeUnit(draft, 1);
    expect(batch(draft)).toEqual([{ url: 'https://a.test' }, { url: 'https://c.test' }]);
  });

  it('keeps one empty Unit when the last one is dropped', () => {
    // A table with no rows has no inputs, and the only control left would be the one that removed
    // them — clearing it is what "remove" means when there is one.
    const draft = removeUnit(setCell(draftFor(HEAD), 0, 'url', 'https://a.test'), 0);
    expect(draft.kind === 'fields' && draft.units).toHaveLength(1);
    expect(batch(draft)).toBe('REFUSED: unit 1 · url is required');
  });

  it('takes `null` for a nullable field, which blank cannot say', () => {
    // Blank already means "leave the key out". An explicit null is a different fact and the only
    // way to type it.
    let draft = setCell(draftFor(HEAD), 0, 'url', 'https://a.test');
    draft = setCell(draft, 0, 'note', 'null');
    expect(batch(draft)).toEqual([{ url: 'https://a.test', note: null }]);
  });
});

describe('a Method that declares nothing', () => {
  it('gets a JSON box, not an empty field table', () => {
    // `examples/go/dnsfacts` and `examples/go/nscheck` declare no types at all. A form derived from
    // nothing would be an empty box claiming the Method takes nothing — the exact confusion the
    // Actors page exists to prevent.
    const draft = draftFor(undefined);
    expect(draft.kind).toBe('json');
    expect(batch(draft)).toEqual([{}]);
  });

  it('does the same for a schema that declares no properties', () => {
    // `{"type": "object"}` is an author who declared the shape and left it open. A field table with
    // zero columns has nothing to type into.
    expect(draftFor({ type: 'object' }).kind).toBe('json');
  });

  it('takes the Batch as typed, whatever is in the Units', () => {
    const draft = setJson(draftFor(undefined), '[{"host": "a.test"}, {"host": "b.test"}]');
    expect(batch(draft)).toEqual([{ host: 'a.test' }, { host: 'b.test' }]);
  });

  it('refuses a bare object rather than wrapping it, because a Batch is a list', () => {
    // The generated file reports `len(batch)`, which for a dict is its number of KEYS. Wrapping
    // silently would teach the shape wrong on the surface whose job is to teach the shape.
    expect(batch(setJson(draftFor(undefined), '{"host": "a.test"}'))).toBe(
      'REFUSED: a Batch is a list of Units — wrap it in [ ]'
    );
  });

  it('says it is not JSON, with the parser’s own complaint', () => {
    expect(String(batch(setJson(draftFor(undefined), '[{host: 1}]')))).toContain(
      'the Batch is not JSON'
    );
  });

  it('reads an empty box as an empty Batch', () => {
    // `BATCH = []` is a legal generated file, and it is what an operator who means to type the Units
    // into the file itself wants.
    expect(batch(setJson(draftFor(undefined), '   '))).toEqual([]);
  });
});

describe('one value, by its declared type', () => {
  it('keeps a string a string, digits and all', () => {
    expect(coerceField('string', '8080')).toEqual({ value: '8080' });
  });

  it('refuses a fraction where a whole number was declared', () => {
    expect(coerceField('integer', '1.5')).toEqual({ error: '"1.5" is not a whole number' });
  });

  it('refuses a boolean that is not one of the two words', () => {
    expect(coerceField('boolean', 'yes')).toEqual({ error: '"yes" is not true or false' });
  });

  it('refuses a list typed as prose, and says what to type instead', () => {
    const got = coerceField('string[]', 'a,b');
    expect(got).toEqual({ error: '"a,b" is not JSON — a string[] is typed as JSON' });
  });

  it('takes a bare word for a field the author left untyped', () => {
    // `any` is a field with no declared type. Refusing a bare word would be this form inventing a
    // constraint the Actor does not have.
    expect(coerceField('any', 'whatever')).toEqual({ value: 'whatever' });
    expect(coerceField('any', '42')).toEqual({ value: 42 });
  });
});

describe('what the probe found', () => {
  /* FOUR FINDINGS, AND THREE OF THEM ARE ZERO ROWS. ADR 0028 §4 made `(results, dropped)`
     undestructurable-around so a caller could not skip past the drops, and ADR 0033's own
     consequence list says isolated Units are a probe's most useful output and must be drawn. This
     is where "0 results" stops being one sentence. */

  it('says a drop is a drop, and names the count against the Batch it came from', () => {
    const said = probeVerdict({ units: 12, results: 9, isolated: 3, done: true });
    expect(said.tone).toBe('dropped');
    expect(said.headline).toBe('3 of 12 Units were dropped');
    expect(said.detail).toContain('9 rows');
    // The lost rows are fetchable, and saying so is the difference between a report and a dead end.
    expect(said.detail).toContain('dropped.rows()');
  });

  it('DOES NOT render "everything was dropped" the way it renders "nothing was found"', () => {
    // The failure this exists for: a 15,814-target run reported `completed` in seven minutes having
    // scanned almost nothing, because zero rows was drawn one way.
    const dropped = probeVerdict({ units: 12, results: 0, isolated: 12, done: false });
    const empty = probeVerdict({ units: 12, results: 0, isolated: 0, done: true });
    expect(dropped.tone).toBe('dropped');
    expect(empty.tone).toBe('empty');
    expect(dropped.headline).not.toBe(empty.headline);
  });

  it('tells an early return apart from a drop, because nothing was lost in one of them', () => {
    const partial = probeVerdict({ units: 12, results: 4, isolated: 0, done: false });
    expect(partial.tone).toBe('partial');
    expect(partial.headline).toContain('before covering its input');
    expect(partial.detail).toContain('nothing was dropped');
  });

  it('reads an empty result as an ANSWER, not a failure', () => {
    const empty = probeVerdict({ units: 12, results: 0, isolated: 0, done: true });
    expect(empty.headline).toContain('every one of the 12 Units was covered');
    expect(empty.detail).toContain('not a failure');
  });

  it('says the ordinary thing when rows came back and nothing was lost', () => {
    const ok = probeVerdict({ units: 12, results: 12, isolated: 0, done: true });
    expect(ok.tone).toBe('ok');
    expect(ok.headline).toBe('12 rows from 12 Units');
  });

  it('counts one of anything in the singular, because a probe is often one Unit', () => {
    expect(probeVerdict({ units: 1, results: 1, isolated: 0, done: true }).headline).toBe(
      '1 row from 1 Unit'
    );
    expect(probeVerdict({ units: 1, results: 0, isolated: 1, done: true }).headline).toBe(
      '1 of 1 Unit was dropped'
    );
    expect(probeVerdict({ units: 1, results: 0, isolated: 0, done: true }).headline).toBe(
      'no rows — the one Unit was covered and produced none'
    );
  });

  it('lets a drop outrank an early return, because a lost Unit is the bigger finding', () => {
    // Both are true of the same call; only one can be the sentence. The counts are drawn beside it
    // either way, which is what keeps the choice from hiding the other fact.
    expect(probeVerdict({ units: 9, results: 0, isolated: 9, done: false }).tone).toBe('dropped');
  });
});
