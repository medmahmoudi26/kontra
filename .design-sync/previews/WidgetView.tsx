import { WidgetView } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/**
 * A widget is a property of the CURRENT screen: a pane that prints the marker renders
 * as a document instead of a terminal, and stops being one on the next snapshot that
 * does not. WidgetView renders what it is given — the swap is the tile's decision.
 */
export function RunReport() {
  return (
    <Frame>
      <div className="h-[300px]">
        <WidgetView
          terminalId="kf-crawl-01"
          widget={
            {
              kind: 'markdown',
              title: 'Crawl report — campaign-apex-119',
              seq: '42',
              body: [
                '## batch 41',
                '',
                '| metric | value |',
                '| --- | --- |',
                '| pages | 1,204 |',
                '| cached | 318 |',
                '| errors | 2 |',
                '',
                'Two hosts returned `503` and were isolated, not dropped.',
              ].join('\n'),
            } as never
          }
        />
      </div>
    </Frame>
  );
}

/** With a `__FILE__` label — a label, not a link: the string never reaches a filesystem. */
export function WithFileLabel() {
  return (
    <Frame>
      <div className="h-[260px]">
        <WidgetView
          terminalId="kf-probe-01"
          widget={
            {
              kind: 'markdown',
              title: 'probe summary',
              file: '/var/lib/kontra/reports/probe-41.md',
              seq: '7',
              body: [
                '**12,880** hosts answered of **37,412** probed.',
                '',
                '- `200` — 9,410',
                '- `403` — 2,104',
                '- `503` — 1,366',
              ].join('\n'),
            } as never
          }
        />
      </div>
    </Frame>
  );
}

/** No title in the document: the tile's own name is the fallback. */
export function UntitledFallsBackToTile() {
  return (
    <Frame>
      <div className="h-[220px]">
        <WidgetView
          terminalId="kf-crawl-02"
          fallbackTitle="kf-crawl-02 · actor"
          widget={
            {
              kind: 'markdown',
              seq: '3',
              body: 'Waiting for the next batch — the queue is empty.',
            } as never
          }
        />
      </div>
    </Frame>
  );
}
