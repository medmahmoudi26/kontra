import { MermaidBlock } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/**
 * KNOWN LIMITATION — use sequence diagrams.
 *
 * `scrubSvg` strips `<foreignObject>` (it is how HTML re-enters an SVG). Mermaid v11 renders
 * flowchart and state-diagram NODE labels inside a `foreignObject` even at
 * `securityLevel: 'strict'` with `flowchart.htmlLabels: false`, so those diagrams render as
 * correctly-shaped but EMPTY boxes — edge labels survive, node labels do not. Sequence
 * diagrams label with `<text>` and are unaffected. Both cards here are sequence diagrams
 * for that reason.
 *
 * Mermaid also renders asynchronously, so a card captured too early can show an empty box.
 */

/** A dispatch, end to end: who calls whom, and what is durable when. */
export function DispatchSequence() {
  return (
    <Frame>
      <div className="min-h-[260px]">
        <MermaidBlock
          chart={[
            'sequenceDiagram',
            '  participant C as Caller workflow',
            '  participant Q as kontra-subfinder',
            '  participant W as Worker',
            '  C->>Q: Batch (200 units)',
            '  Q->>W: poll',
            '  W-->>C: Batch out',
            '  Note over W: output durable at emit time',
          ].join('\n')}
        />
      </div>
    </Frame>
  );
}

/**
 * The failure path an operator actually needs drawn: a worker dies mid-Batch, the units
 * are isolated rather than committed, and the Dataset stays `open` because nobody sealed it.
 */
export function IsolatedBatch() {
  return (
    <Frame>
      <div className="min-h-[300px]">
        <MermaidBlock
          chart={[
            'sequenceDiagram',
            '  participant C as Caller workflow',
            '  participant W as Worker',
            '  participant L as Lake',
            '  C->>W: Batch 41 (200 units)',
            '  W->>L: emit 118 rows',
            '  Note over W: worker dies',
            '  W--xC: no Batch out',
            '  C->>C: isolate 82 units',
            '  Note over L: Dataset stays open — never sealed',
          ].join('\n')}
        />
      </div>
    </Frame>
  );
}
