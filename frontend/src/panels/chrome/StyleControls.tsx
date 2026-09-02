/**
 * Palette and font size, in the wall's control bar (slice 7a).
 *
 * FONT SIZE IS LABELLED WITH WHAT IT COSTS, and it is the only appearance control on this page that is
 * not free. Cell metrics decide cols/rows; cols/rows are what the streamer puts into `stty` before
 * `tmux attach`; so a size change re-measures every tile and re-attaches every LIVE one. `TerminalTile`
 * coalesces the storm (its `SETTLE_MS`) so a three-step change is one re-attach rather than three, but
 * the operator should still be told, because "why did my four live tiles blink" has an answer and it is
 * this control.
 *
 * A STEPPER, NOT A SLIDER, for the same reason: a slider is a gesture that emits fifty values, and fifty
 * values is fifty settle windows. Whole points only, which is also all xterm's cell metrics can express.
 */

import { Button } from '@/components/ui/button';
import { MAX_FONT_SIZE, MIN_FONT_SIZE, PALETTES, PALETTE_ORDER, type PaletteName } from '../theme';
import type { TerminalStyleApi } from './useTerminalStyle';

export default function StyleControls({ style }: { style: TerminalStyleApi }): JSX.Element {
  return (
    <span className="flex items-center gap-1" data-testid="style-controls">
      <select
        data-testid="palette-picker"
        data-palette={style.palette}
        // `pr-5` leaves room for the native chevron: without it the longest option name renders
        // underneath the arrow, which reads as a truncated control rather than a full one.
        className="h-7 rounded border bg-background pl-1.5 pr-5 text-xs"
        value={style.palette}
        title={PALETTES[style.palette]?.hint}
        aria-label="terminal palette"
        onChange={(e) => style.setPalette(e.target.value as PaletteName)}
      >
        {PALETTE_ORDER.map((name) => (
          <option key={name} value={name} title={PALETTES[name].hint}>
            {PALETTES[name].label}
          </option>
        ))}
      </select>

      <span
        className="flex items-center gap-0.5"
        data-testid="font-size"
        data-size={style.fontSize}
        title={
          `Terminal font size, ${MIN_FONT_SIZE}–${MAX_FONT_SIZE} px. It changes the cell size, so it ` +
          'changes the cols/rows every tile reports — and the streamer stty’s those before a live ' +
          'attach, so every LIVE tile re-attaches once the size settles.'
        }
      >
        <Button
          variant="ghost"
          size="sm"
          data-testid="font-smaller"
          aria-label="smaller terminal font"
          disabled={!style.canShrink}
          onClick={() => style.nudgeFontSize(-1)}
        >
          A−
        </Button>
        <span className="w-6 text-center text-[11px] text-muted-foreground">{style.fontSize}</span>
        <Button
          variant="ghost"
          size="sm"
          data-testid="font-bigger"
          aria-label="larger terminal font"
          disabled={!style.canGrow}
          onClick={() => style.nudgeFontSize(1)}
        >
          A+
        </Button>
      </span>
    </span>
  );
}
