import { StyleControls } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/** The slice of TerminalStyleApi these controls actually read. */
const style = (palette: string, fontSize: number, canShrink = true, canGrow = true) =>
  ({
    palette,
    fontSize,
    canShrink,
    canGrow,
    setPalette: () => {},
    setFontSize: () => {},
    nudgeFontSize: () => {},
    toggleSidebar: () => {},
  }) as never;

/** Default: the palette follows the app theme, at the default 12px cell. */
export function Default() {
  return (
    <Frame>
      <StyleControls style={style('follow', 12)} />
    </Frame>
  );
}

/** Each palette the wall offers. `follow` resolves against light/dark at apply time. */
export function Palettes() {
  return (
    <Frame>
      <div className="flex flex-col gap-3">
        {['follow', 'night', 'paper'].map((p) => (
          <div key={p} className="flex items-center gap-3">
            <span className="w-14 font-mono text-xs text-muted-foreground">{p}</span>
            <StyleControls style={style(p, 12)} />
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * A stepper, not a slider — cell metrics are integral, and a slider emits fifty values,
 * which on a live tile is fifty re-`stty`s and re-attaches. At the clamps the step
 * that would leave the range is disabled.
 */
export function AtTheClamps() {
  return (
    <Frame>
      <div className="flex flex-col gap-3">
        <div className="flex items-center gap-3">
          <span className="w-24 font-mono text-xs text-muted-foreground">min (9)</span>
          <StyleControls style={style('night', 9, false, true)} />
        </div>
        <div className="flex items-center gap-3">
          <span className="w-24 font-mono text-xs text-muted-foreground">max (20)</span>
          <StyleControls style={style('night', 20, true, false)} />
        </div>
      </div>
    </Frame>
  );
}
