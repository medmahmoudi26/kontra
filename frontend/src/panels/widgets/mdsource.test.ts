/**
 * Building markdown out of values from elsewhere (ADR 0020, slice 7b).
 *
 * Each case here is an injection into a GENERATED document — the other direction from
 * `markdown.test.ts`, which is about rendering one. The renderer cannot help with these: a value that
 * closes a fence produces perfectly valid markdown, and the document simply says something else.
 */

import { describe, expect, it } from 'vitest';
import { cell, definitionTable, fence, fenceBody, inlineCode, line, VALUE_CAP } from './mdsource';

describe('line', () => {
  it('flattens newlines, tabs and control characters', () => {
    expect(line('a\nb\tc')).toBe('a b c');
    expect(line(`a${String.fromCharCode(0)}b`)).toBe('a b');
    expect(line(`${String.fromCharCode(0x1b)}[31mred`)).toBe('[31mred');
  });

  it('caps, and marks that it did', () => {
    const out = line('x'.repeat(VALUE_CAP * 2));
    expect(out.length).toBe(VALUE_CAP);
    expect(out.endsWith('…')).toBe(true);
  });

  it('takes non-strings without throwing', () => {
    expect(line(7)).toBe('7');
    expect(line(undefined)).toBe('');
    expect(line(null)).toBe('');
  });
});

describe('cell', () => {
  it('escapes a pipe, so one value cannot shift every column after it', () => {
    // Not hypothetical: a materialisation error is a stringified exception, and `KeyError: 'a|b'` is
    // enough to split a row.
    expect(cell("KeyError: 'a|b'")).toBe("KeyError: 'a\\|b'");
  });

  it('escapes backslashes first, so a trailing one cannot escape our escape', () => {
    expect(cell('c:\\path\\')).toBe('c:\\\\path\\\\');
  });

  it('cannot add a row', () => {
    expect(cell('one\n| two | three |')).toBe('one \\| two \\| three \\|');
  });

  it('renders an empty value as an em dash rather than nothing', () => {
    expect(cell('')).toBe('—');
    expect(cell(undefined)).toBe('—');
  });
});

describe('inlineCode', () => {
  it('neutralises a backtick, so a command cannot end its own span', () => {
    expect(inlineCode('a`b')).toBe('`aˋb`');
  });
});

describe('fenceBody and fence', () => {
  it('keeps newlines — a fence is lines', () => {
    expect(fenceBody('a\nb')).toBe('a\nb');
  });

  it('NEUTRALISES A CLOSING FENCE, which is how a label escapes a ```mermaid block', () => {
    const hostile = 'flowchart LR\n```\n# owned\n```mermaid\nflowchart LR\n  x --> y';
    const body = fenceBody(hostile);
    expect(body).not.toMatch(/^\s*```/m);
    expect(body).toContain('ˋˋˋ');
    // And through the fence builder, the block still has exactly one opening and one closing line.
    const block = fence('mermaid', hostile);
    expect(block.split('\n').filter((l) => /^\s*```/.test(l))).toEqual(['```mermaid', '```']);
  });

  it('neutralises a tilde fence too', () => {
    expect(fenceBody('~~~~\nx')).not.toMatch(/^~~~/m);
  });

  it('neutralises an indented closing fence', () => {
    // Up to three spaces of indent still closes a fence in CommonMark.
    expect(fenceBody('   ```')).toBe('   ˋˋˋ');
  });

  it('drops control characters that would reach the mermaid parser', () => {
    expect(fenceBody(`a${String.fromCharCode(0)}b`)).toBe('a b');
    expect(fenceBody('a\tb')).toBe('a\tb');
  });
});

describe('definitionTable', () => {
  it('drops absent rows rather than printing "undefined"', () => {
    const table = definitionTable([
      ['a', 1],
      ['b', undefined],
      ['c', ''],
      ['d', null],
      ['e', 'x'],
    ]);
    expect(table).toContain('| a | 1 |');
    expect(table).toContain('| e | x |');
    expect(table).not.toContain('| b |');
    expect(table).not.toContain('undefined');
  });

  it('is empty when nothing is known, so a caller can drop the section', () => {
    expect(definitionTable([['a', undefined]])).toBe('');
  });

  it('escapes both columns', () => {
    expect(definitionTable([['a|b', 'c|d']])).toContain('| a\\|b | c\\|d |');
  });
});
