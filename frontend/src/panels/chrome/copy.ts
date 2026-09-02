/**
 * Copying a tile's visible text OUT (slice 7a).
 *
 * COPY IS THE ONE DIRECTION THAT IS ALLOWED. ADR 0020's guarantee is that no route, message type or
 * code path can write to a session's channel; it says nothing about reading, and reading is most of what
 * an operator wants from a wall — a stack trace to paste into an issue, a request id to grep the lake
 * for, the exact line that made them go and look. Nothing in this file touches the socket: it reads
 * xterm's own buffer, which is a client-side copy of bytes that already arrived, and hands the result to
 * the clipboard.
 *
 * VISIBLE TEXT, NOT SCROLLBACK, and the distinction is honesty rather than laziness. A Terminal is not a
 * record (ADR 0020 finding (5): snapshots are a screen, a live attach is lossier still, and the Manifest
 * and the lake are the record). "Copy everything this tile has" would hand someone 2000 lines that look
 * like a log and are not one — with silent gaps where the byte cap elided output. What the operator can
 * see is a claim this page can actually stand behind, so that is what the menu offers, and the caller
 * appends the elided-bytes count when there is one.
 *
 * WHY A `document.execCommand` FALLBACK IN 2026. `navigator.clipboard` requires a secure context.
 * `localhost` counts; a Controller reached over its VPC address on plain HTTP — which is exactly the
 * compose topology `panelsClient.ts` describes — does NOT, and there `navigator.clipboard` is
 * `undefined`. Without the fallback, "copy visible text" would work on the developer's laptop and be a
 * dead button on the box an operator actually uses.
 */

/** The part of an xterm buffer this needs. An interface so the extraction is testable without a DOM or
 * a terminal — the trailing-blank rule below is the part with a bug in it if anything is. */
export interface BufferLike {
  /** The scrollback offset of the top visible row. */
  viewportY: number;
  /** Total rows including scrollback. */
  length: number;
  getLine(index: number): { translateToString(trimRight?: boolean): string } | undefined;
}

/**
 * The rows currently on screen, as text.
 *
 * Each line is right-trimmed, because `capture-pane` pads a screen to its full width with spaces and
 * pasting 80-column-wide trailing whitespace into an issue is worse than useless. Trailing BLANK lines
 * go too — a 50-row pane showing four lines of output would otherwise copy as four lines and
 * forty-six empty ones — but blank lines in the MIDDLE are kept, because a Worker that printed a gap
 * printed a gap.
 */
export function visibleText(buffer: BufferLike, rows: number): string {
  const start = Math.max(0, buffer.viewportY);
  const end = Math.min(buffer.length, start + Math.max(0, rows));
  const lines: string[] = [];
  for (let i = start; i < end; i += 1) {
    lines.push(buffer.getLine(i)?.translateToString(true) ?? '');
  }
  while (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
  return lines.join('\n');
}

/**
 * Put text on the clipboard. Resolves `true` on success.
 *
 * Never throws: a rejected clipboard permission is a thing the caller reports as a notice, not an
 * unhandled rejection that trips the browser suite's page-error listener.
 */
export async function copyText(text: string): Promise<boolean> {
  if (text === '') return false;
  try {
    if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Fall through: a denied permission on a secure origin is still worth trying the legacy path for.
  }
  return legacyCopy(text);
}

/**
 * The insecure-origin path: a detached textarea, selected, `execCommand('copy')`, removed.
 *
 * ON `document.body`, NEVER INSIDE A TILE. xterm owns the contents of its container and clears children
 * it did not create — the same rule that makes `tile-empty-<id>` a SIBLING of the mount rather than a
 * child — so a textarea parked in there would be both fragile and, briefly, a real focusable text input
 * inside a read-only terminal. Off-screen rather than `display: none`, because a hidden element cannot
 * be selected and the copy silently does nothing.
 */
function legacyCopy(text: string): boolean {
  if (typeof document === 'undefined') return false;
  const area = document.createElement('textarea');
  area.value = text;
  area.setAttribute('readonly', 'true');
  area.setAttribute('aria-hidden', 'true');
  area.style.position = 'fixed';
  area.style.top = '-1000px';
  area.style.left = '-1000px';
  area.style.opacity = '0';
  document.body.appendChild(area);
  try {
    area.select();
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    area.remove();
  }
}
