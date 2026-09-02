/**
 * The frame every surface mounts into: the rail, and the column beside it.
 *
 * IT IS A COMPONENT RATHER THAN FOUR LINES IN `App.tsx` FOR ONE REASON — `App` holds five
 * `React.lazy` surfaces, and rendering it in a test starts five dynamic imports into CodeMirror,
 * xterm and AG Grid before it can draw anything. The frame is the part of this slice that has
 * states worth pinning (which surface is marked, what a surface that has not resolved yet looks
 * like), so it takes its content as a prop and can be drawn in node with nothing heavy behind it.
 * That is the house split: props in, markup out.
 *
 * THE SUSPENSE BOUNDARY LIVES HERE, WITH THE RAIL ALREADY DRAWN. A fallback that replaced the whole
 * page would blank the navigation every time an operator changed surface, which reads as the app
 * reloading; keeping the rail up means the only thing that flickers is the thing that is actually
 * arriving.
 *
 * `.app` IS A ROW WITH `overflow: hidden` (`styles.css`), and every surface is `flex: 1; min-width:
 * 0` inside it. That `min-width` is load-bearing rather than tidy: without it a flex item refuses
 * to shrink below its content's intrinsic width, so a wide table or a long line of terminal output
 * pushes the surface past the viewport instead of scrolling inside itself.
 */

import { Suspense, type ReactNode } from 'react';

import { SideNav } from './SideNav';

/**
 * What a surface looks like before its chunk has arrived.
 *
 * A SENTENCE, NOT A SPINNER. This is on screen for the length of one chunk fetch on a local
 * appliance, and the house rule is that nothing blinks — an animation here would be the busiest
 * thing on a page whose whole point is calm. It carries `.dataset-page`'s padding so the text lands
 * where the surface's own first line will.
 */
export function SurfacePending(): JSX.Element {
  return (
    <div className="dataset-page text-muted-foreground" data-testid="surface-pending">
      Loading…
    </div>
  );
}

export function Shell({ children }: { children: ReactNode }): JSX.Element {
  return (
    <div className="app" data-testid="app-shell">
      <SideNav />
      <Suspense fallback={<SurfacePending />}>{children}</Suspense>
    </div>
  );
}
