/**
 * The sidecar down the left of the canvas: what the selected node IS.
 *
 * A BOX YOU DRAG CANNOT CARRY A PARAGRAPH. The node says `nscheck@0.1.0 .delegation()` and that is
 * the right thing for a node to say — but what `delegation` is FOR, and what it takes and emits, is
 * the reading that decides whether the edge being drawn makes sense, and it was reachable only by
 * leaving Scratch for the Actors page and coming back.
 *
 * AND THE METHOD IS PICKED HERE, which is the half the canvas had no answer for at all. The palette
 * lists one row per Method, so placing a node was the only moment the Method was ever chosen: place
 * the Actor before deciding, or grab the wrong row, and the node could only be deleted and drawn
 * again — taking its edges with it. `ScratchActorNode.method` was already allowed to be empty for
 * exactly that reason; nothing could write it back until this.
 *
 * WHAT A METHOD IS COMES FROM `MethodContract.tsx`, the same rows the Actors page draws. Two
 * renderings of "what this Method takes" would disagree eventually, and the one in Scratch is the
 * one nobody would notice going stale — nobody opens a sketch to audit a field table.
 *
 * THERE IS NO CLOSE BUTTON, and that is deliberate rather than missing. Which node is selected is
 * React Flow's own state (`onSelectionChange`), so a button here that only cleared the page's copy
 * would leave the node still selected on the canvas — and clicking it again would change nothing,
 * fire no selection event, and reopen nothing. Clicking the canvas closes this, which is the
 * gesture that actually deselects.
 *
 * SELECTION IS NOT IN THE DOCUMENT. A click must not dirty a drawing, and two people opening the
 * same Scratch do not share a cursor — so nothing this panel is handed is stored, and the only
 * thing it writes back is a Method the author picked.
 */

import type { ScratchNode } from '../run/api';
import { asOfText, countText } from '../datasets/scope';
import { dispatchText, versionText, wholeDataset } from '../datasets/grouped';
import { datasetBadge, datasetState } from '../datasets/state';
import { MethodRow } from './MethodContract';
import { WorkflowContract } from './WorkflowContract';
import type { ActorReading, DatasetReading, EdgeFallout, ScratchReading, WorkflowReading } from './scratchInspect';
import { SideDockControls, SideRail, SideResizer, useSideDock } from './chrome/SideDock';
import { Button } from '@/components/ui/button';

export function ScratchInspector({
  reading,
  fallout,
  onPickMethod,
}: {
  reading: ScratchReading;
  /** Edges the last Method change left to be checked, for the node being shown. Never a reason to
   *  remove one — the author drew them. */
  fallout: readonly EdgeFallout[];
  onPickMethod(method: string): void;
}): JSX.Element {
  const node = reading.node as ScratchNode;
  const dock = useSideDock('scratch-inspector', { width: 304, side: 'left', collapsed: false });

  if (dock.collapsed) {
    return (
      <div className="flex min-h-0" style={{ order: dock.side === 'left' ? -1 : 1 }}>
        <SideRail
          label="Inspector"
          testid="scratch-inspector-rail"
          onExpand={() => dock.setCollapsed(false)}
        />
      </div>
    );
  }

  const aside = (
    <aside
      /* THE WIDTH IS THE OPERATOR'S. A Method's field table and a Dataset's column list are not the
         same size, and 304 pixels was one number chosen for both. */
      style={{ flex: `0 0 ${dock.width}px`, width: dock.width }}
      className="flex min-h-0 flex-col overflow-y-auto bg-background"
      data-testid={`scratch-inspector-${node.id}`}
      data-side={dock.side}
    >
      <div className="flex shrink-0 items-center gap-1 border-b border-border px-2 py-1">
        <span className="text-[9.5px] uppercase tracking-wide text-muted-foreground">Inspector</span>
        <SideDockControls dock={dock} label="the inspector" testid="scratch-inspector-dock" />
      </div>
      {reading.kind === 'actor' ? (
        <ActorBody reading={reading} fallout={fallout} onPickMethod={onPickMethod} />
      ) : reading.kind === 'workflow' ? (
        <WorkflowBody reading={reading} />
      ) : (
        <DatasetBody reading={reading} />
      )}
    </aside>
  );

  /* The handle sits on the edge the panel is NOT docked to — see `SideDock.tsx`. */
  const handle = (
    <SideResizer
      width={dock.width}
      onWidth={dock.setWidth}
      side={dock.side}
      label="the inspector"
      testid="scratch-inspector-resizer"
    />
  );
  return (
    <div
      className={`flex min-h-0 ${dock.side === 'left' ? 'border-r' : 'border-l'} border-border`}
      style={{ order: dock.side === 'left' ? -1 : 1 }}
    >
      {dock.side === 'left' ? (
        <>
          {aside}
          {handle}
        </>
      ) : (
        <>
          {handle}
          {aside}
        </>
      )}
    </div>
  );
}

/** The strip at the top: which of the three kinds this is, and its name. The same three accents the
 *  nodes carry, so the panel and the box on the canvas are visibly the same thing. */
function Head({
  kind,
  accent,
  title,
  badge,
  detail,
}: {
  kind: string;
  accent: string;
  title: string;
  badge?: string;
  detail: string;
}): JSX.Element {
  return (
    <div className="shrink-0 border-b border-border px-3.5 py-2.5">
      <div className="flex items-baseline gap-1.5">
        <span
          className={`rounded-full px-1.5 font-mono text-[8.5px] uppercase tracking-wider ${accent}`}
        >
          {kind}
        </span>
        <span className="min-w-0 truncate font-mono text-[12px] font-semibold">{title}</span>
        {badge && <span className="font-mono text-[9.5px] text-muted-foreground">{badge}</span>}
      </div>
      <div className="mt-0.5 font-mono text-[10px] text-muted-foreground">{detail}</div>
    </div>
  );
}

function ActorBody({
  reading,
  fallout,
  onPickMethod,
}: {
  reading: ActorReading;
  fallout: readonly EdgeFallout[];
  onPickMethod(method: string): void;
}): JSX.Element {
  const { node, methods, standing } = reading;
  return (
    <>
      <Head
        kind="actor"
        accent="bg-sky-500/15 text-sky-600 dark:text-sky-400"
        title={node.actor}
        badge={`v${node.version}`}
        detail={node.method ? `.${node.method}()` : 'no Method chosen'}
      />

      {standing.state === 'unknown-actor' ? (
        // NAMING WHAT IT LOOKED FOR, in the server's own sentence — this is what the agent reads in
        // the spec's "What does not resolve", against the node rather than in a list.
        <p
          className="m-0 border-b border-amber-500/40 bg-amber-500/5 px-3.5 py-2.5 text-[11px] leading-snug text-amber-700 dark:text-amber-400"
          data-testid={`inspect-unresolved-${node.id}`}
        >
          {standing.detail}
        </p>
      ) : (
        <>
          {standing.state === 'unchosen' && (
            // NOT AN ERROR. A half-drawn sketch is a legitimate thing to save; this is the surface
            // where the Method gets decided, so the sentence says what to do rather than what is
            // wrong.
            <p
              className="m-0 border-b border-border bg-muted/40 px-3.5 py-2 text-[11px] leading-snug text-muted-foreground"
              data-testid={`inspect-unchosen-${node.id}`}
            >
              No Method chosen yet — pick one below. The sketch saves fine without one, and the spec
              tells the agent it is missing.
            </p>
          )}

          {standing.state === 'unknown-method' && (
            <p
              className="m-0 border-b border-amber-500/40 bg-amber-500/5 px-3.5 py-2.5 text-[11px] leading-snug text-amber-700 dark:text-amber-400"
              data-testid={`inspect-unresolved-${node.id}`}
            >
              {standing.detail}
            </p>
          )}

          {fallout.length > 0 && <Fallout nodeId={node.id} fallout={fallout} />}

          {methods.length === 0 ? (
            <p className="m-0 px-3.5 py-2.5 text-[11px] italic text-muted-foreground">
              this version registered no Methods — a load-only Actor is still a real deployment, but
              there is nothing here to dispatch
            </p>
          ) : (
            <div data-testid={`inspect-methods-${node.id}`}>
              <div className="flex items-baseline justify-between bg-muted px-3.5 py-1.5 text-[9px] uppercase tracking-wider text-muted-foreground">
                <span>methods it declares</span>
                <span className="font-mono tabular-nums">{methods.length}</span>
              </div>
              {methods.map((op) => {
                const inUse = op.name === node.method;
                return (
                  <div
                    key={op.name}
                    className={inUse ? 'border-l-2 border-sky-500 bg-sky-500/5' : ''}
                    data-testid={`inspect-method-${node.id}-${op.name}`}
                  >
                    <div className="flex items-center gap-1.5 px-3.5 pt-1.5">
                      {inUse ? (
                        <span
                          className="rounded bg-sky-500/15 px-1.5 font-mono text-[8.5px] uppercase tracking-wider text-sky-600 dark:text-sky-400"
                          data-testid={`inspect-inuse-${node.id}`}
                          title="this node dispatches this Method"
                        >
                          in use
                        </span>
                      ) : (
                        // PICKING REWRITES `method` AND NOTHING ELSE (`withMethod`). It is a button
                        // per row rather than a select, because what you are choosing between is the
                        // descriptions and signatures under each row — the reason to open this panel.
                        <Button
                          variant="ghost"
                          size="sm"
                          className="h-5 px-1.5 text-[10px]"
                          data-testid={`inspect-pick-${node.id}-${op.name}`}
                          title={`dispatch ${op.name}() from this node — the edges you drew are left exactly as they are`}
                          onClick={() => onPickMethod(op.name)}
                        >
                          use this
                        </Button>
                      )}
                    </div>
                    {/* The field tables open on the Method in use: that is the one whose types the
                        edges either fit or do not. The others keep their description and their
                        `takes → emits` summary, which is what you choose BETWEEN. */}
                    <MethodRow actor={node.actor} op={op} expanded={inUse} />
                  </div>
                );
              })}
            </div>
          )}
        </>
      )}
    </>
  );
}

/**
 * The edges a Method change left to be reckoned with.
 *
 * IT SAYS NOTHING WAS REMOVED, because that is the question an amber strip raises. The author drew
 * those edges and they are all still there; what changed is what the node takes or emits, and only
 * a person can say whether the line still means what they meant by it.
 */
function Fallout({ nodeId, fallout }: { nodeId: string; fallout: readonly EdgeFallout[] }): JSX.Element {
  return (
    <div
      className="border-b border-amber-500/40 bg-amber-500/5 px-3.5 py-2"
      data-testid={`inspect-fallout-${nodeId}`}
    >
      <div className="text-[9px] uppercase tracking-wider text-amber-500">
        {fallout.length} edge{fallout.length === 1 ? '' : 's'} to check — nothing was removed
      </div>
      <ul className="m-0 mt-1 list-none p-0">
        {fallout.map((f) => (
          <li
            key={`${f.edgeId}:${f.side}`}
            className="text-[10.5px] leading-snug"
            data-testid={`inspect-fallout-edge-${f.edgeId}`}
          >
            {f.detail}
          </li>
        ))}
      </ul>
    </div>
  );
}

function WorkflowBody({ reading }: { reading: WorkflowReading }): JSX.Element {
  const { node, file, listed, standing } = reading;
  return (
    <>
      <Head
        kind="workflow"
        accent="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
        title={node.file}
        detail="caller workflow"
      />

      {/* THE FILE AND THE CONTRACT ARE TWO DIFFERENT ABSENCES. A file this installation does not
          have is a sketch of one somebody is about to write; a file that exists and has registered
          nothing is one no worker has served yet. Only the first is a claim the listing can make,
          and only once it has answered. */}
      {listed && !file && (
        <p
          className="m-0 border-b border-amber-500/40 bg-amber-500/5 px-3.5 py-2.5 text-[11px] leading-snug text-amber-700 dark:text-amber-400"
          data-testid={`inspect-unresolved-${node.id}`}
        >
          no workflow file named <code className="font-mono">{node.file}</code> in{' '}
          <code className="font-mono">.kontra/workflows/</code> — this is a sketch of one that does
          not exist yet, or a file that has been renamed since.
        </p>
      )}

      {file?.description && (
        <p className="m-0 border-b border-border px-3.5 py-2 text-[11px] leading-snug">
          {file.description}
        </p>
      )}

      <div className="px-3.5 py-2.5">
        {standing.state === 'ambiguous' ? (
          <p className="m-0 text-[11px] leading-snug text-muted-foreground">
            {standing.types.length} registered workflow types share this file&rsquo;s name (
            {standing.types.join(', ')}), so which contract belongs to this node cannot be told from
            the name alone. Open it on the Workflows page, which reads the type out of the source.
          </p>
        ) : (
          <>
            {/* THE SAME CONTRACT THE WORKFLOWS PAGE DRAWS — its input and output readings keep
                `not declared` apart from `declares no fields`, which is the distinction a `dict`
                annotation makes and an empty table destroys. */}
            <WorkflowContract
              type={node.file}
              descriptor={standing.state === 'described' ? standing.descriptor : undefined}
            />
            {/* AND AN UNDESCRIBED WORKFLOW SAYS SO, HERE. The Workflows page draws nothing for a
                missing description because the source is on the screen beside it; in a sidecar whose
                whole job is "what is this node", silence reads as a panel that failed to load. Both
                SDKs omit the key rather than sending "", so this is an author who wrote none. */}
            {standing.state === 'described' && !standing.descriptor.description && (
              <p
                className="m-0 mt-1.5 text-[10.5px] italic text-muted-foreground/70"
                data-testid={`inspect-undescribed-${node.id}`}
              >
                no description — add a docstring to the workflow class
              </p>
            )}
          </>
        )}
      </div>
    </>
  );
}

function DatasetBody({ reading }: { reading: DatasetReading }): JSX.Element {
  const { node, standing, columns, measuredAt } = reading;
  return (
    <>
      <Head
        kind="dataset"
        accent="bg-violet-500/15 text-violet-600 dark:text-violet-400"
        title={node.name}
        detail={node.direction === 'out' ? 'written to →' : '→ read from'}
      />

      {/* THE DIRECTION IS READ OFF THE DRAWING, not declared. The palette used to offer `IN` and
          `OUT` and the author picked before drawing anything; now an edge onto this node's input is
          a write and an edge out of its output is a read, and the canvas derives it
          (`scratchFlow.ts:withDerivedDirections`). It is still said in words here because reading a
          Dataset and writing one are the same call up to `.writer()` — the distinction that once
          announced a thousand committed rows a second after a run started. */}
      <p className="m-0 border-b border-border px-3.5 py-2 text-[11px] leading-snug text-muted-foreground">
        {node.direction === 'out'
          ? 'Drawn as WRITTEN TO: an edge in this sketch lands on it, so something here produces it.'
          : 'Drawn as READ FROM: nothing here writes it — connect a step to its input side and this becomes written to.'}
      </p>

      {/* THE COLUMNS, WHICH ARE ALSO THE NODE'S PORTS (`scratchPorts.ts`). Same reading, drawn twice
          on purpose: the handles on the canvas are too small to read and the panel is where the
          field you are about to connect gets its name checked. */}
      <div className="border-b border-border px-3.5 py-2" data-testid={`inspect-columns-${node.id}`}>
        <div className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">
          columns
        </div>
        {columns.state === 'unread' ? (
          <div className="text-[11px] italic text-muted-foreground">
            not read yet — the lake has not answered with this Dataset&rsquo;s columns
          </div>
        ) : columns.state === 'absent' ? (
          <div className="text-[11px] italic text-muted-foreground">
            none — nothing in the lake has written this name, so it has no columns to connect to yet
          </div>
        ) : (
          <ul className="m-0 list-none p-0">
            {columns.columns.map((c) => (
              <li key={c.name} className="flex gap-2 font-mono text-[11px]">
                <span>{c.name}</span>
                <span className="truncate text-muted-foreground" title={c.type}>
                  {c.type}
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>

      {standing.state === 'unread' ? (
        <p
          className="m-0 px-3.5 py-2.5 text-[11px] italic leading-snug text-muted-foreground"
          data-testid={`inspect-unread-${node.id}`}
        >
          the lake listing has not answered yet — nothing here is a claim about this name
        </p>
      ) : standing.state === 'absent' ? (
        <p
          className="m-0 px-3.5 py-2.5 text-[11px] leading-snug text-amber-700 dark:text-amber-400"
          data-testid={`inspect-unresolved-${node.id}`}
        >
          no Dataset named <code className="font-mono">{node.name}</code> is in the lake — this is a
          sketch of one nothing has written yet, or a name spelled differently there.
        </p>
      ) : (
        <div data-testid={`inspect-dataset-${node.id}`}>
          {standing.groups.map((g) => {
            // `wholeDataset` is the group as one row — including the rule that a lifecycle is
            // recorded per NAME, so any dispatch's is the group's. Reading `dispatches[0].state`
            // here would be that rule stated a second time.
            const badge = datasetBadge(datasetState(wholeDataset(g)));
            return (
              <div key={`${g.kind}:${g.name}`} className="border-b border-border px-3.5 py-2">
                <div className="flex flex-wrap items-baseline gap-1.5">
                  <span className="rounded bg-muted px-1.5 font-mono text-[8.5px] uppercase tracking-wider text-muted-foreground">
                    {g.kind}
                  </span>
                  <span
                    className={`rounded border px-1.5 font-mono text-[8.5px] uppercase tracking-wider ${badge.className}`}
                    title={badge.title}
                  >
                    {badge.label}
                  </span>
                  <span className="font-mono text-[9.5px] text-muted-foreground">
                    {dispatchText(g)}
                  </span>
                </div>
                {/* THE COUNT CARRIES ITS SCOPE, everywhere (`datasets/scope.ts`): a number whose
                    scope is unstated is the defect the Datasets surface was built to undo, and a
                    total read from a poll is a fact about the moment that poll answered. */}
                <div className="mt-1 font-mono text-[11px]">
                  {countText(g.total.total.rows, 'dataset', g.kind)}
                </div>
                <div className="font-mono text-[9.5px] text-muted-foreground">
                  {asOfText(measuredAt)}
                  {versionText(g) ? ` · ${versionText(g)}` : ''}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </>
  );
}
