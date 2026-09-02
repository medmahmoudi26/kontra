/**
 * A tile showing a widget instead of a screen (ADR 0020, slice 7b).
 *
 * This is what slice 7a's `TerminalTile` mounts in place of the xterm container when
 * {@link parseWidget} recognises the captured text. It is deliberately thin: a header, a scroll
 * container, and `Markdown`. Everything about what is safe to render lives in `Markdown`, and
 * everything about what counts as a widget lives in `parseWidget`.
 *
 * IT SAYS WHERE THE CONTENT CAME FROM, and that is a security control rather than decoration. A
 * rendered document looks authored — headings, tables, links — while this one was PRINTED BY A PANE,
 * which on this wall means it may be quoting a crawled page. The badge is how an operator knows the
 * difference between the Dashboard telling them something and a target telling them something. The
 * same reason `DashboardPage` says a Terminal is not a record.
 *
 * IT DOES NOT REPLACE THE TERMINAL PERMANENTLY. A widget is a property of the CURRENT screen, so a
 * pane that stops printing the marker goes back to being a terminal on the next snapshot. That is the
 * tile's decision, not this component's — it renders what it is given.
 */

import Markdown from './Markdown';
import { getWidgetTitle } from './getWidgetTitle';
import type { Widget } from './parseWidget';

export interface WidgetViewProps {
  widget: Widget;
  /** The Terminal this widget replaces, so the hooks are unique on a wall — the same convention
   * `HealthChips` uses for `chip-<signal>-<id>`. */
  terminalId?: string;
  /** What the tile would have called itself: `<machine> · <window>`. Used when the document names
   * nothing. */
  fallbackTitle?: string;
  className?: string;
}

export default function WidgetView({
  widget,
  terminalId,
  fallbackTitle,
  className,
}: WidgetViewProps): JSX.Element {
  const suffix = terminalId ? `-${terminalId}` : '';
  const title = getWidgetTitle(widget, fallbackTitle);

  return (
    <div
      data-testid={`widget${suffix}`}
      data-widget-kind={widget.kind}
      // The producer's version stamp, exposed so a spec (and a human) can tell "the same document
      // again" from "a new document that happens to look the same".
      data-widget-seq={widget.seq ?? ''}
      className={`flex min-h-0 flex-col overflow-hidden rounded border ${className ?? ''}`}
      style={{ background: '#0b0b0e' }}
    >
      <div className="flex min-w-0 items-center gap-2 border-b border-zinc-800 px-2 py-1">
        <span className="truncate text-[11px] font-medium" title={title}>
          {title ?? 'markdown widget'}
        </span>
        {widget.file !== undefined && (
          // A LABEL, NOT A LINK, and nothing reads it: `parseWidget` explains why this string never
          // reaches a filesystem.
          <code
            data-testid={`widget-file${suffix}`}
            className="truncate text-[10px] text-muted-foreground"
            title={widget.file}
          >
            {widget.file}
          </code>
        )}
        <span
          data-testid={`widget-provenance${suffix}`}
          className="ml-auto shrink-0 rounded border border-amber-500/50 px-1 text-[10px] text-amber-300"
          title={
            'This is markdown printed by the pane itself. Pane output can quote a crawled page, so ' +
            'treat it as data, not as the Dashboard speaking. Remote images are blocked and links ' +
            'are not followed.'
          }
        >
          from pane output
        </span>
      </div>

      <div className="min-h-0 flex-1 overflow-auto px-2 py-1">
        <Markdown source={widget.body} testId={`widget-markdown${suffix}`} />
      </div>
    </div>
  );
}
