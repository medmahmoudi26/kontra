/**
 * Widget detection, against screens a scanner could have printed (ADR 0020, slice 7b).
 *
 * The three rules with security weight are the first three describes: the marker's POSITION, where
 * metadata may come from, and the cap. Each is a narrowing of tmuxy's behaviour, and each is here
 * because the input is a pane full of crawler output.
 */

import { describe, expect, it } from 'vitest';
import {
  describeWidgetFallback,
  fileLabel,
  paneText,
  parseWidget,
  parseWidgetFrame,
  stripAnsi,
  WIDGET_MARKER,
  WIDGET_TEXT_CAP,
  WIDGET_TITLE_CAP,
} from './parseWidget';

/** ESC as a code, never as a literal: a source file holding a real control character is a file every
 * tool treats as binary. */
const ESC = String.fromCharCode(0x1b);
const CLEAR_HOME = `${ESC}[H${ESC}[2J`;

const WIDGET = [`${WIDGET_MARKER}markdown`, '# Crawl', '', '- one', '- two'].join('\n');

describe('the marker must be the first non-empty line', () => {
  it('accepts a screen that opens with it, after blank lines', () => {
    const widget = parseWidget(`\n\n${WIDGET}`);
    expect(widget?.kind).toBe('markdown');
    expect(widget?.body).toContain('# Crawl');
  });

  it('REFUSES a marker further down the screen', () => {
    // THE CASE THIS EXISTS FOR: a crawler logs a page it fetched, and the page's title is the marker.
    // tmuxy scans every line (its panes are shells with prompts above the marker); we must not, or a
    // scanned site chooses whether its own output is rendered as a document.
    const hostile = [
      '2026-08-12T04:00:01Z crawled https://target.example/ ok',
      `2026-08-12T04:00:02Z title=${WIDGET_MARKER}markdown`,
      '# Not a heading, a log line',
    ].join('\n');
    expect(parseWidget(hostile)).toBeNull();
  });

  it('refuses a marker that is merely a prefix of the line', () => {
    expect(parseWidget(`prefix ${WIDGET_MARKER}markdown\nbody`)).toBeNull();
  });

  it('refuses an unknown kind, and says why', () => {
    const text = `${WIDGET_MARKER}image\nnot rendered`;
    expect(parseWidget(text)).toBeNull();
    expect(describeWidgetFallback(text)).toContain('unknown widget kind');
  });

  it('refuses a marker with no content under it', () => {
    expect(parseWidget(`${WIDGET_MARKER}markdown\n\n   \n`)).toBeNull();
    expect(describeWidgetFallback(`${WIDGET_MARKER}markdown\n`)).toContain('no content under its marker');
  });

  it('is not a widget at all when there is no marker', () => {
    expect(parseWidget('systemd[1]: Started kontra-actor.service.')).toBeNull();
    expect(describeWidgetFallback('systemd[1]: Started kontra-actor.service.')).toBeNull();
  });
});

describe('metadata comes from the header block only', () => {
  it('honours __TITLE__, __FILE__ and __SEQ__ immediately after the marker', () => {
    const widget = parseWidget(
      [
        `${WIDGET_MARKER}markdown`,
        '__TITLE__: Crawl of target.example ',
        '__FILE__:/var/lib/kontra/reports/crawl.md',
        '__SEQ__:7',
        '',
        'body text',
      ].join('\n')
    );
    expect(widget?.title).toBe('Crawl of target.example');
    expect(widget?.file).toBe('/var/lib/kontra/reports/crawl.md');
    expect(widget?.seq).toBe('7');
    expect(widget?.body).toBe('body text\n');
  });

  it('falls back to __FILE__\u2019s basename for the title', () => {
    const widget = parseWidget([`${WIDGET_MARKER}markdown`, '__FILE__:/tmp/a/b/report.md', 'x'].join('\n'));
    expect(widget?.title).toBe('report.md');
  });

  it('IGNORES a __TITLE__ that appears in the body', () => {
    // A crawled string inside the document must not be able to choose the tile's header — that is
    // chrome an operator reads without questioning it.
    const widget = parseWidget(
      [`${WIDGET_MARKER}markdown`, 'first line of body', '__TITLE__:Production — enter your password', 'more'].join(
        '\n'
      )
    );
    expect(widget?.title).toBeUndefined();
    expect(widget?.body).toContain('__TITLE__:Production');
  });

  it('flattens and caps a hostile title', () => {
    const widget = parseWidget(
      [`${WIDGET_MARKER}markdown`, `__TITLE__:${'A'.repeat(400)}`, 'body'].join('\n')
    );
    expect(widget?.title?.length).toBe(WIDGET_TITLE_CAP);
    expect(widget?.title?.endsWith('…')).toBe(true);
  });

  it('swallows unknown protocol lines rather than rendering them', () => {
    const widget = parseWidget([`${WIDGET_MARKER}markdown`, '__PRIVATE__:secret', 'body'].join('\n'));
    expect(widget?.body).toBe('body\n');
  });
});

describe('the cap is a fallback to the terminal, not a truncation', () => {
  it('returns null past the cap so the tile keeps painting into xterm', () => {
    const huge = `${WIDGET_MARKER}markdown\n${'x'.repeat(WIDGET_TEXT_CAP)}`;
    expect(huge.length).toBeGreaterThan(WIDGET_TEXT_CAP);
    expect(parseWidget(huge)).toBeNull();
  });

  it('explains the refusal, so a producer is not left guessing', () => {
    const huge = `${WIDGET_MARKER}markdown\n${'x'.repeat(WIDGET_TEXT_CAP)}`;
    expect(describeWidgetFallback(huge)).toContain('the cap is');
  });

  it('accepts a document just under the cap', () => {
    const head = `${WIDGET_MARKER}markdown\n`;
    const body = 'y'.repeat(WIDGET_TEXT_CAP - head.length);
    expect(parseWidget(head + body)?.body.startsWith('y')).toBe(true);
  });
});

describe('terminal control sequences are not content', () => {
  it('strips the repaint prefix, SGR, alternate screen and OSC', () => {
    const text = `${CLEAR_HOME}${ESC}[1;32m${WIDGET_MARKER}markdown${ESC}[0m\n${ESC}]0;title${String.fromCharCode(7)}# H\nbody`;
    const widget = parseWidget(text);
    expect(widget?.body).toBe('# H\nbody\n');
  });

  it('does not let an unterminated OSC swallow the screen twice', () => {
    // A malformed sequence ends the string once. The assertion is bounded output, not a specific
    // rendering: a regex that backtracked here would be the cost.
    const text = `${ESC}]0;${'a'.repeat(50)}`;
    expect(stripAnsi(text)).toBe('');
  });

  it('keeps tabs and newlines, drops carriage returns', () => {
    expect(stripAnsi('a\tb\r\nc')).toBe('a\tb\nc');
  });

  it('parses straight off a binary frame, the way a tile holds it', () => {
    const bytes = new TextEncoder().encode(`${CLEAR_HOME}${WIDGET}`);
    expect(parseWidgetFrame(bytes)?.body).toContain('# Crawl');
    expect(paneText(new TextEncoder().encode(`${ESC}[2Jhi`))).toBe('hi');
  });
});

describe('trailing whitespace and blank lines', () => {
  it('trims the padding capture-pane leaves and normalises the tail', () => {
    const widget = parseWidget([`${WIDGET_MARKER}markdown`, 'line   ', '', '', ''].join('\n'));
    expect(widget?.body).toBe('line\n');
  });
});

describe('fileLabel', () => {
  it('takes the last segment of either separator, and never touches a filesystem', () => {
    expect(fileLabel('/var/lib/x/report.md')).toBe('report.md');
    expect(fileLabel('C:\\reports\\x.md')).toBe('x.md');
    expect(fileLabel('report.md')).toBe('report.md');
    expect(fileLabel('/')).toBe('/');
  });
});

describe('describeWidgetFallback is total', () => {
  it('says nothing about a screen that parsed fine', () => {
    // A caller must not have to remember "only ask after a null" — asking about a good widget gets
    // `null`, never a sentence explaining a refusal that did not happen.
    expect(describeWidgetFallback(WIDGET)).toBeNull();
  });
});
