/**
 * One React Flow node component per kind of thing a Scratch can name.
 *
 * FOUR SHAPES, NOT ONE BOX WITH A LABEL. An Actor's Method, a caller workflow, a Dataset and a note
 * are four different things to whoever reads the sketch back, and the surface they replaced drew
 * them as the same rectangle in three colours — which meant telling a Dataset from an Actor was
 * reading the text on it. Each carries its own icon, its own second line, and its own accent.
 *
 * THERE IS NO RUN AFFORDANCE HERE, and there never should be (ADR 0026). The canvas 0023 §12
 * removed WAS the execution model; this one is input to writing the program that is. A play button
 * on a node is the exact mistake that distinction exists to prevent.
 *
 * Typed with the base `NodeProps` and narrowed at the boundary, the way v1's ActorNode was: React
 * Flow's `nodeTypes` registry is a map of components over the wide data type, and a component
 * declared against a narrower one is not assignable to it.
 */

import { createContext, useContext, useEffect, type ReactNode } from 'react';
import { Handle, Position, useUpdateNodeInternals, type NodeProps, type NodeTypes } from '@xyflow/react';
import { Boxes, Database, FileCode2, Trash2 } from 'lucide-react';
import type { ScratchNode, ScratchNote } from '../run/api';
import {
  NODE_H,
  NODE_W,
  type ScratchNodeData,
  type ScratchNodeType,
  type ScratchNoteData,
} from './scratchFlow';
import { fieldHandle, inHandle, outHandle, type ScratchHandleKind } from './scratchHandles';
import { shortType, type NodePorts, type PortSide, type ScratchPort } from './scratchPorts';

/**
 * What a node can ask the page to do.
 *
 * THROUGH CONTEXT, NOT THROUGH `data`. A node's `data` is the document's own node — that is the
 * whole point of the projection — and hanging callbacks off it would put a new function identity in
 * the document's slot on every render, which React Flow reads as the node having changed and
 * re-measures.
 */
export interface ScratchNodeActions {
  onDelete(id: string): void;
  onEditNote(id: string, text: string): void;
}

/**
 * `null` UNTIL SOMEBODY PROVIDES THEM, and that is what makes a node drawable on a surface with
 * nowhere to send an edit.
 *
 * It used to be a no-op pair, so every node drew a bin whether or not anything was listening — a
 * control that does nothing, which is how an operator learns not to trust the controls. The rule is
 * the one `WorkflowThread.tsx` states for `onDrillTurn`: an absent handler leaves the affordance
 * OFF rather than dead. The workflow design tab reads these nodes without editing them
 * (`WorkflowSketch.tsx`), which is where it started mattering.
 */
const Actions = createContext<ScratchNodeActions | null>(null);
export const ScratchActionsProvider = Actions.Provider;

/**
 * Every node's fields, by node id — THROUGH CONTEXT, for the same reason the actions are.
 *
 * The ports are derived from the CATALOG (`scratchPorts.ts`), which arrives after the document and
 * changes without it: a poll answers, a worker registers a workflow, the lake schema lands. Putting
 * them in a node's `data` would change the node object every time any of that happened, and React
 * Flow re-measures a node whose object changed — during a drag, that is every frame of the drag of
 * every other node. Through context the node re-renders and the document's own node stays the
 * object it was.
 */
const Ports = createContext<ReadonlyMap<string, NodePorts>>(new Map());
export const ScratchPortsProvider = Ports.Provider;

/** A node the catalog has not answered for yet. Not "it has no fields" — the sentence says which. */
const UNREAD: NodePorts = {
  in: { ports: [], declared: false, why: 'the catalog has not answered yet' },
  out: { ports: [], declared: false, why: 'the catalog has not answered yet' },
};

/**
 * How a handle is drawn.
 *
 * SOLID FOR A FIELD SOMETHING DECLARES, DASHED FOR ONE ONLY AN EDGE NAMES. A Method change strands a
 * field — the port comes back undeclared rather than disappearing, because React Flow drops an edge
 * whose named handle is gone and the author's line would vanish off the canvas while staying in the
 * document. Mark, never refuse (ADR 0026).
 *
 * Ten pixels, not six. Six was fine while a handle was only an anchor for edges the link button
 * made; it is not a target anybody can grab, and connecting is a drag onto exactly this dot.
 */
function handleClass(declared: boolean): string {
  return declared
    ? '!size-2.5 !border-0 !bg-muted-foreground/70'
    : '!size-2.5 !border !border-dashed !border-amber-500 !bg-transparent';
}

/** What the whole-node handle says it is for. It is never a fault: a Method that declares neither
 *  `takes=` nor `emits=`, a workflow annotated `dict`, and a node whose Method is not picked yet all
 *  have nothing finer to offer, and this is how they stay connectable. */
function wholeTitle(side: 'in' | 'out', why: string): string {
  const what = side === 'in' ? 'takes' : 'emits';
  return why
    ? `${what}: ${why} — an edge here means the whole node`
    : `an edge here means the whole node rather than one field`;
}

/**
 * One field, on one edge of the node.
 *
 * THE HANDLE IS INSIDE THE ROW, and the row is `relative`, so React Flow's own
 * `.react-flow__handle-left { left: -4px; top: 50% }` puts the dot on the node's border beside THIS
 * field. Computing a pixel offset per index instead would be a second copy of the row height, wrong
 * the first time a font renders differently.
 */
function PortRow({
  nodeId,
  side,
  port,
}: {
  nodeId: string;
  side: 'in' | 'out';
  port: ScratchPort;
}): JSX.Element {
  return (
    <div
      className={`relative flex items-baseline gap-1 px-2 ${side === 'out' ? 'justify-end' : ''}`}
      data-testid={`port-${side}-${nodeId}-${port.name}`}
    >
      <Handle
        type={side === 'in' ? 'target' : 'source'}
        position={side === 'in' ? Position.Left : Position.Right}
        id={fieldHandle(side, port.name)}
        className={handleClass(port.declared)}
        title={
          port.declared
            ? `${port.name}: ${port.type}`
            : `${port.name} is not declared here — an edge attaches to it, and nothing this node is says it exists`
        }
      />
      <span className="truncate font-mono text-[9.5px] leading-tight">{port.name}</span>
      <span
        className={`shrink-0 font-mono text-[8.5px] leading-tight ${
          port.declared ? 'text-muted-foreground' : 'text-amber-500'
        }`}
        title={port.type}
      >
        {port.declared ? shortType(port.type) : 'not declared'}
      </span>
    </div>
  );
}

/**
 * The frame every catalogued node shares.
 *
 * THE HANDLES ARE WHERE EDGES ARE MADE, and where the ones the document already holds land. A field
 * has its own (`out:field:body`), so an edge names in the DOM which value it carries — and
 * `scratchFlow.ts` projects a stored edge onto the very same ids, which is what keeps a loaded edge
 * and one drawn a second ago the same object.
 *
 * AND EVERY NODE KEEPS A WHOLE-NODE HANDLE PER SIDE. Three ordinary states have no field to offer,
 * and a node that could not be connected until its types were known would refuse the exact state
 * people draw in.
 */
function NodeFrame({
  id,
  kind,
  tone,
  accent,
  icon,
  title,
  badge,
  detail,
  selected,
}: {
  id: string;
  kind: ScratchHandleKind;
  tone: string;
  accent: string;
  icon: ReactNode;
  title: string;
  badge?: string;
  detail: string;
  selected?: boolean;
}): JSX.Element {
  const actions = useContext(Actions);
  const ports = useContext(Ports).get(id) ?? UNREAD;
  const fields = ports.in.ports.length + ports.out.ports.length;

  // RE-MEASURE WHEN THE HANDLE IDS CHANGE, which is v1's `ActorNode` doing the same thing for the
  // same reason. React Flow caches a node's handle bounds and carries them across a new node object
  // with the same id (`parseHandles` keeps the previous ones for anything already measured) — so a
  // node whose handles are suddenly called something else keeps the old names, and since
  // `scratchFlow.ts` names the handle every edge lands on, every edge touching that node silently
  // stops being drawn. Picking a different Method in the inspector is exactly that: `fetch` emits
  // `body` and `title` emits `html`, so every handle on the right is renamed at once.
  const handles = handleKey(kind, ports);
  const updateNodeInternals = useUpdateNodeInternals();
  useEffect(() => {
    updateNodeInternals(id);
  }, [id, handles, updateNodeInternals]);

  return (
    <div
      // NO HORIZONTAL PADDING ON THE BOX: every row inside carries its own, so a row is full width
      // and a handle sitting at its left edge sits on the node's border rather than 8px inside it.
      // The height is a MINIMUM, not a size — a node is as tall as the fields it declares, and the
      // alternative is a scrollbar inside a box somebody is dragging around.
      className={`flex flex-col justify-center rounded-md border py-1 ${tone} ${
        selected ? 'ring-2 ring-primary' : ''
      }`}
      style={{ width: NODE_W, minHeight: NODE_H }}
      data-testid={`scratch-node-${id}`}
      data-kind={kind}
    >
      <div className="relative flex min-w-0 items-baseline gap-1 px-2">
        <Handle
          type="target"
          position={Position.Left}
          id={inHandle(kind)}
          className={handleClass(true)}
          data-testid={`port-whole-in-${id}`}
          title={wholeTitle('in', ports.in.why)}
        />
        <span className={`shrink-0 self-center ${accent}`}>{icon}</span>
        <span className="truncate font-mono text-[11.5px] font-semibold">{title}</span>
        {badge && <span className="shrink-0 font-mono text-[9px] text-muted-foreground">{badge}</span>}
        <Handle
          type="source"
          position={Position.Right}
          id={outHandle(kind)}
          className={handleClass(true)}
          data-testid={`port-whole-out-${id}`}
          title={wholeTitle('out', ports.out.why)}
        />
      </div>

      <div className="flex items-baseline gap-1 px-2">
        <span className="truncate font-mono text-[10px] text-muted-foreground">{detail}</span>
        {/* `nodrag`, or a pointer-down on the bin drags the node instead of pressing it. Drawn only
            where something is listening — see the Actions context. */}
        {actions && (
          <button
            className="nodrag ml-auto shrink-0 text-muted-foreground hover:text-destructive"
            title="remove"
            data-testid={`delete-${id}`}
            onClick={() => actions.onDelete(id)}
          >
            <Trash2 size={11} />
          </button>
        )}
      </div>

      {fields > 0 ? (
        <div className="mt-1 flex border-t border-border/60 pt-1">
          <div className="flex min-w-0 flex-1 flex-col">
            {ports.in.ports.map((p) => (
              <PortRow key={p.name} nodeId={id} side="in" port={p} />
            ))}
          </div>
          <div className="flex min-w-0 flex-1 flex-col">
            {ports.out.ports.map((p) => (
              <PortRow key={p.name} nodeId={id} side="out" port={p} />
            ))}
          </div>
        </div>
      ) : (
        // NOT AN EMPTY TABLE AND NOT SILENCE. A node with nothing to connect per field is a normal
        // node — the sentence is the difference between "this declares no fields" and "this failed
        // to load", and only one of them is a reason to delete the node and draw it again.
        <div
          className="px-2 text-[9px] italic leading-tight text-muted-foreground"
          data-testid={`ports-none-${id}`}
        >
          {ports.in.why === ports.out.why ? ports.in.why : `in: ${ports.in.why} · out: ${ports.out.why}`}
        </div>
      )}
    </div>
  );
}

/** Every handle id this node draws, as one string. What `useUpdateNodeInternals` has to fire on:
 *  a node keeping a renamed handle is every edge touching it silently not drawn. */
function handleKey(kind: ScratchHandleKind, ports: NodePorts): string {
  const names = (s: PortSide): string => s.ports.map((p) => p.name).join(',');
  return `${kind}|${names(ports.in)}|${names(ports.out)}`;
}

/** `nscheck@0.1.0` and the Method it dispatches. The Method is the unit, not the Actor: an Actor
 *  holds many (ADR 0023 §9), and which one this step is IS the drawing. */
function ActorNode({ id, data, selected }: NodeProps): JSX.Element {
  const node = (data as ScratchNodeData).node as Extract<ScratchNode, { kind: 'actor' }>;
  return (
    <NodeFrame
      id={id}
      kind="actor"
      tone="border-sky-500/50 bg-sky-500/10"
      accent="text-sky-500"
      icon={<Boxes size={11} />}
      title={node.actor}
      badge={`v${node.version}`}
      detail={`.${node.method || '?'}()`}
      selected={selected}
    />
  );
}

/** A caller workflow that already exists, by the file that defines it. */
function WorkflowNode({ id, data, selected }: NodeProps): JSX.Element {
  const node = (data as ScratchNodeData).node as Extract<ScratchNode, { kind: 'workflow' }>;
  return (
    <NodeFrame
      id={id}
      kind="workflow"
      tone="border-emerald-500/50 bg-emerald-500/10"
      accent="text-emerald-500"
      icon={<FileCode2 size={11} />}
      title={node.file}
      detail="caller workflow"
      selected={selected}
    />
  );
}

/** A Dataset, its columns, and which of the two things it is. `direction` is derived from the side
 *  the edges land on now (`withDerivedDirections`) rather than chosen in the palette, and it is said
 *  in words here because reading a Dataset and writing one are the same call up to `.writer()` — the
 *  distinction that once put a thousand rows on a screen that nothing had written. */
function DatasetNode({ id, data, selected }: NodeProps): JSX.Element {
  const node = (data as ScratchNodeData).node as Extract<ScratchNode, { kind: 'dataset' }>;
  return (
    <NodeFrame
      id={id}
      kind="dataset"
      tone="border-violet-500/50 bg-violet-500/10"
      accent="text-violet-500"
      icon={<Database size={11} />}
      title={node.name}
      detail={node.direction === 'out' ? 'written to →' : '→ read from'}
      selected={selected}
    />
  );
}

/**
 * A note. No handles, deliberately: the server's parse builds its edge endpoints out of NODE ids
 * only, so an edge drawn to a note is dropped on the way in — a handle here would offer a
 * connection that silently does not survive the save.
 */
function NoteNode({ id, data, selected }: NodeProps): JSX.Element {
  const note = (data as ScratchNoteData).note as ScratchNote;
  const actions = useContext(Actions);

  return (
    <div
      className={`w-[188px] rounded border border-amber-500/40 bg-amber-500/10 p-1.5 ${
        selected ? 'ring-2 ring-primary' : ''
      }`}
      data-testid={`scratch-note-${id}`}
      data-kind="note"
    >
      <div className="mb-1 text-[9px] uppercase tracking-wider text-amber-600/80 dark:text-amber-400/80">
        note
        {actions && (
          <button
            className="nodrag float-right text-muted-foreground hover:text-destructive"
            title="delete this note"
            data-testid={`delete-${id}`}
            onClick={() => actions.onDelete(id)}
          >
            <Trash2 size={10} />
          </button>
        )}
      </div>
      {/* A textarea, edited in place. These words go to the agent verbatim, so what is typed is
          what is sent — no formatting step in between to be surprised by.

          `nodrag` and `nowheel` are not decoration: without them a pointer-down inside the textarea
          starts a node drag, so selecting a word moves the note instead, and a wheel over it zooms
          the canvas rather than scrolling the text. */}
      <textarea
        className="nodrag nowheel w-full resize-none bg-transparent text-[10.5px] leading-snug outline-none"
        rows={3}
        value={note.text}
        // READ-ONLY WHERE NOTHING IS LISTENING, rather than an input that swallows what is typed.
        readOnly={!actions}
        onChange={(e) => actions?.onEditNote(id, e.target.value)}
        spellCheck={false}
        data-testid={`note-text-${id}`}
      />
    </div>
  );
}

/**
 * The registry React Flow looks a node's `type` up in.
 *
 * Keyed by `ScratchNodeType`, so a kind added to `SCRATCH_NODE_TYPES` without a component here
 * fails the typecheck. React Flow's own answer to an unregistered type is its default node — an
 * empty rectangle with no label, which is draggable and savable and says nothing about what it is.
 */
export const scratchNodeTypes: Record<ScratchNodeType, NodeTypes[string]> = {
  actor: ActorNode,
  workflow: WorkflowNode,
  dataset: DatasetNode,
  note: NoteNode,
};
