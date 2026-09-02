/**
 * A ```mermaid fence, rendered — or its source, never a blank box (ADR 0020, slice 7b).
 *
 * MERMAID IS LAZY, AND THAT IS A BUDGET DECISION. The library is the largest thing this app could
 * import (roughly the weight of everything else on the Dashboard put together), and a wall of
 * journal followers must not pay for it. So it is loaded by `import('mermaid')` inside the effect
 * below — the same shape `DatasetPage` uses for AG Grid — which means the Dashboard chunk contains
 * this file and not the renderer, and a Dashboard that never shows a diagram never fetches it.
 *
 * A CHART'S SOURCE CAN COME FROM PANE TEXT, so this is an untrusted-input path:
 *
 *  - `securityLevel: 'strict'` — no HTML labels, no `click` handlers, no `%%{init}%%` raising the
 *    level (mermaid keeps `securityLevel` in its own `secure` list, which is exactly why the ADR
 *    mandates strict rather than `antiscript`).
 *  - `startOnLoad: false` — nothing scans the document for diagrams; this component renders the one
 *    string it was given.
 *  - **The source is capped** ({@link MERMAID_SOURCE_CAP}). A parser is the one thing on this page
 *    that a pane can make do unbounded work, and mermaid parses on the main thread.
 *  - **The SVG is parsed as XML and imported, never assigned as HTML.** No `innerHTML`, no
 *    `dangerouslySetInnerHTML` — `DOMParser` with `image/svg+xml` plus `importNode` gives a document
 *    that either parses or reports an error, instead of an HTML parser's best effort. On top of that
 *    {@link scrubSvg} removes script elements and event-handler attributes: mermaid at strict level
 *    emits neither, so this is defence in depth against a mermaid regression, NOT the control that
 *    makes the path safe.
 *  - **A chart that fails to parse renders its source as code** with the error underneath. A blank
 *    box on a wall of terminals reads as a broken stream, and the source is what the operator needs
 *    to see anyway.
 */

import { useEffect, useRef, useState } from 'react';

/**
 * The most mermaid source this will hand to the parser.
 *
 * A snapshot is one screen (~10 KiB at 200×50), so 32 KiB is far above anything a pane can emit in
 * one frame and far below the size at which parsing costs a visible pause.
 */
export const MERMAID_SOURCE_CAP = 32 * 1024;

/**
 * The initialisation, exported so a test can assert the two flags the ADR requires rather than
 * trusting a comment. `flowchart.htmlLabels: false` is redundant at strict level and stated anyway:
 * it is the specific behaviour ("labels are markup") that would matter if the level ever moved.
 */
export const MERMAID_CONFIG = {
  startOnLoad: false,
  securityLevel: 'strict',
  theme: 'dark',
  fontFamily: 'inherit',
  flowchart: { htmlLabels: false },
  themeVariables: { background: '#0b0b0e', fontSize: '12px' },
} as const;

/** One module fetch per tab, however many diagrams the page shows. */
let modulePromise: Promise<typeof import('mermaid')> | null = null;
let initialised = false;
let renderCounter = 0;

async function mermaidApi(): Promise<typeof import('mermaid').default> {
  modulePromise ??= import('mermaid');
  const mod = await modulePromise;
  const api = mod.default;
  if (!initialised) {
    initialised = true;
    api.initialize(MERMAID_CONFIG);
  }
  return api;
}

/** Reset the module-level memo. For tests only — nothing in the app has a reason to re-initialise. */
export function resetMermaidForTest(): void {
  modulePromise = null;
  initialised = false;
}

/**
 * Remove the two things an SVG must never carry into this page: script elements and event-handler
 * attributes. A string operation on purpose — it is testable without a DOM, and it runs before the
 * XML parser rather than after, so nothing it removes ever exists as a node.
 *
 * NOT A SANITISER, and must not be read as one. Pattern-matching markup is a losing game in the
 * general case; the reason this is sound here is that the input is mermaid's own serialiser at
 * `securityLevel: 'strict'`, and this only has to survive that guarantee being broken by a
 * dependency bump. `foreignObject` goes too: it is the element through which HTML re-enters an SVG,
 * and strict mode does not use it.
 */
export function scrubSvg(svg: string): string {
  return svg
    .replace(/<script[\s\S]*?<\/script\s*>/gi, '')
    .replace(/<script[^>]*\/>/gi, '')
    .replace(/<foreignObject[\s\S]*?<\/foreignObject\s*>/gi, '')
    .replace(/\son[a-z]+\s*=\s*"[^"]*"/gi, '')
    .replace(/\son[a-z]+\s*=\s*'[^']*'/gi, '')
    .replace(/\son[a-z]+\s*=\s*[^\s>]+/gi, '');
}

/** Does this look like an SVG document at all? The cheapest possible check that what we are about to
 * insert is what mermaid claims it is. */
export function looksLikeSvg(svg: string): boolean {
  return /^\s*(?:<\?xml[^>]*\?>\s*)?(?:<!DOCTYPE[^>]*>\s*)?<svg[\s>]/i.test(svg);
}

export interface MermaidBlockProps {
  chart: string;
  className?: string;
}

type Phase = 'source' | 'rendered';

export default function MermaidBlock({ chart, className }: MermaidBlockProps): JSX.Element {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const [phase, setPhase] = useState<Phase>('source');
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    // Back to source on every new chart: a previous diagram left in place under new source is worse
    // than no diagram, because it is plausible.
    setPhase('source');
    setError(null);

    if (chart.length > MERMAID_SOURCE_CAP) {
      setError(
        `this diagram is ${chart.length} characters and the cap is ${MERMAID_SOURCE_CAP} — ` +
          'showing its source instead'
      );
      return;
    }

    void (async () => {
      try {
        const api = await mermaidApi();
        // `parse` first: it throws on invalid syntax without touching the DOM, so a bad chart costs
        // a rejected promise rather than a half-drawn diagram.
        await api.parse(chart);
        renderCounter += 1;
        const { svg } = await api.render(`kontra-mermaid-${renderCounter}`, chart);
        if (cancelled) return;
        const host = hostRef.current;
        if (!host) return;
        const scrubbed = scrubSvg(svg);
        if (!looksLikeSvg(scrubbed)) throw new Error('mermaid did not return an SVG document');
        const doc = new DOMParser().parseFromString(scrubbed, 'image/svg+xml');
        if (doc.getElementsByTagName('parsererror').length > 0) {
          throw new Error('mermaid returned SVG that does not parse as XML');
        }
        host.replaceChildren(document.importNode(doc.documentElement, true));
        setPhase('rendered');
      } catch (e) {
        if (cancelled) return;
        // The message is rendered as TEXT by React below. It routinely quotes the offending line of
        // the chart, which is the most useful thing it could say and is why it is not swallowed.
        setError(e instanceof Error ? e.message : String(e));
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [chart]);

  return (
    <div className={className} data-testid="mermaid" data-phase={phase}>
      {/* The host is always in the DOM: the effect needs somewhere to put the diagram, and hiding it
          until then keeps the source and the diagram from both being visible for a frame. */}
      <div ref={hostRef} className="overflow-x-auto" hidden={phase !== 'rendered'} />

      {phase !== 'rendered' && (
        // NEVER A BLANK BOX (ADR 0020, slice 7b). Before the lazy import resolves, and forever if the
        // chart does not parse, the source is the content.
        <pre
          data-testid="mermaid-source"
          className="overflow-x-auto rounded border border-zinc-700/60 bg-black/40 p-2 text-[11px] leading-snug"
        >
          <code>{chart}</code>
        </pre>
      )}

      {error !== null && (
        <p data-testid="mermaid-error" className="mt-1 text-[11px] text-amber-300">
          this diagram did not render: {error}
        </p>
      )}
    </div>
  );
}
