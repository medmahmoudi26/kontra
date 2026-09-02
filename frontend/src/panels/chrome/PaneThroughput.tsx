/**
 * A tile's own output rate, as a sparkline in its banner.
 *
 * WHAT IT MEASURES IS BYTES THAT ARRIVED FOR THIS TERMINAL. Not a synthesised curve, and not a
 * proxy: the byte path in `TerminalTile` adds each frame's length to a counter, and this component
 * drains that counter once a second. So a flat line means the pane printed nothing in that second,
 * a spike means it printed a screen, and an empty chart means we have not been watching long enough
 * to have two samples. All three are things that are true.
 *
 * IT OWNS ITS OWN CLOCK, AND THAT IS THE WHOLE REASON IT IS A COMPONENT. The alternative — state on
 * the tile, ticked once a second — would re-render every tile on the wall every second, and each of
 * those renders reaches an xterm. `StatusBar` is built the same way and for the same reason. Only
 * this seven-pixel svg re-renders.
 *
 * A COUNTER REF, NOT A PROP, for the same reason: a prop that changed on every frame would re-render
 * the banner at the frame rate of the busiest Machine on the wall.
 */

import { memo, useEffect, useState, type MutableRefObject } from 'react';

import { Spark } from '../../components/Spark';
import { push } from '../../components/spark';

/** One second. Slower and a burst of output is averaged into invisibility; faster and the chart is
 *  measuring the poll rather than the pane. */
const SAMPLE_MS = 1000;

export default memo(function PaneThroughput({
  counter,
  id,
}: {
  /** Bytes received since the last drain. Mutated by the tile's byte path; reset here. */
  counter: MutableRefObject<number>;
  id: string;
}): JSX.Element | null {
  const [series, setSeries] = useState<number[]>([]);

  useEffect(() => {
    const timer = setInterval(() => {
      const bytes = counter.current;
      counter.current = 0;
      setSeries((prev) => push(prev, bytes));
    }, SAMPLE_MS);
    return () => clearInterval(timer);
  }, [counter]);

  return (
    <span
      className="h-3 w-11 shrink-0 text-muted-foreground"
      data-testid={`tile-throughput-${id}`}
      data-samples={series.length}
    >
      <Spark
        series={series}
        color="#6ee7b7"
        width={46}
        height={13}
        title="bytes this pane printed per second, measured from the frames this tile received"
      />
    </span>
  );
});
