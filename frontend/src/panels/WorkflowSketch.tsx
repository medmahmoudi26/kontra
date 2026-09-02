/**
 * The workflow's design tab: the sketch of what this workflow is meant to be made of.
 *
 * A DRAWING WITH A SUBJECT AT LAST. Scratch was a top-level surface holding a list of drawings
 * named by hand — a drawing surface about nothing, which is why nobody opened it. It is a tab here,
 * keyed to the workflow it is a sketch OF (`workflowScratchId`), and that single change is what
 * turns "what was this meant to be" from a search into an address. ADR 0026 survives unchanged and
 * is strengthened by the move: it was always a drawing that produces code, and it now has the one
 * thing it was missing.
 *
 * NOTHING ON THIS TAB RUNS, AND THERE IS NOTHING ON IT TO PRESS. No run, no play, no dispatch, on a
 * node or anywhere else — the canvas ADR 0023 §12 removed WAS the execution model, and a play
 * button on a node is the exact mistake the distinction exists to prevent. It is not left out by
 * discipline alone: this component is handed a document and two callbacks that edit it, and holds
 * no run id, no queue and no way to reach one. `workflowSketch.render.test.ts` asserts it over every
 * interactive element in the markup.
 *
 * THINGS THAT DO NOT EXIST YET ARE THE POINT. A workflow is designed before its parts are built:
 * you draw `nscheck.delegation → nscheck.ask`, look at the shape, and then go and write the two
 * Methods. So the Actor form takes free text rather than a picker — an Actor nobody has deployed
 * has no catalog row to pick a version off — and the catalog is offered as a `datalist`, which
 * helps and does not gate. What does not resolve is drawn in its own group, under
 * `resolveScratch`'s own sentence, which is the sentence the agent reading the spec will get.
 *
 * IT IS A BOARD AND NOT A CANVAS, and that is a deliberate trade rather than an unfinished one.
 * React Flow renders its nodes out of a store that only fills once the DOM has measured them, so
 * under this suite — node, no jsdom — a `<ReactFlow>` renders an empty `react-flow__nodes` div and
 * every assertion about what is on the tab becomes an assertion about nothing. The one property a
 * reviewer must be able to check first is that no node carries a run affordance, so the drawing is
 * laid out here and the NODE COMPONENTS ARE THE CANVAS'S OWN (`ScratchNodes.tsx`), reused rather
 * than rebuilt: the same four shapes, the same accents, the same handles, the same ports. The
 * `ReactFlowProvider` is borrowed for exactly one reason — `Handle` reads its store on the way to
 * rendering — which is the same thing `scratchNodes.render.test.ts` borrows it for.
 *
 * WHICH MEANS THE EDGES ARE SENTENCES HERE, not curves — `a.field → b.field`, with the author's own
 * word for the edge after it. Each end is named by `nodeLabel`, the words the canvas and the
 * inspector already share, so the tab is not a second renderer: the SPEC an agent reads is still
 * `renderScratch`'s and still only the server's (ADR 0026), and nothing here derives it a second
 * time.
 *
 * IT TOUCHES NONE OF THE DEAD EXECUTION-EDITOR STYLESHEET CLASSES — `.canvas`, `.actor-node`,
 * `.io-row`, `.mapping-table`, `.link-panel`, `.seed-table`, `.panel.inspector` — which are what
 * the interpreter left behind in `styles.css`, and reaching for one would be reaching for the
 * surface that ran things. Every utility used here is one this codebase already carries: the
 * stylesheet is a static compile, so a class nobody else uses is one the compiled bundle has no
 * rule for and which therefore silently does nothing. The one width that had no existing utility is
 * an inline style off `NODE_W`, which is where the node takes its own width from.
 */

import { createElement, useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { ReactFlowProvider } from '@xyflow/react';
import { Plus, StickyNote } from 'lucide-react';
import { workflowScratchId } from '@core/scratch';

import {
  fetchWorkflowSketch,
  saveWorkflowSketch,
  type ScratchDocument,
  type WorkflowDescriptor,
  type WorkflowFile,
} from '../run/api';
import { fetchSchema, type SchemaEntry } from '../run/query';
import { useAppStore } from '../state/store';
import { useRegisteredFolders } from './RegisteredFolders';
import { registeredActors } from './sourceFolders';
import { ScratchActionsProvider, ScratchPortsProvider, scratchNodeTypes } from './ScratchNodes';
import type { ScratchCatalogue } from './scratchInspect';
import {
  NODE_W,
  normaliseScratchDocument,
  withoutNode,
  type ScratchNodeData,
  type ScratchNodeType,
  type ScratchNoteData,
} from './scratchFlow';
import { documentPorts } from './scratchPorts';
import {
  mintSketchId,
  readSketch,
  sketchDocument,
  sketchSummary,
  withActor,
  withNote,
  withNoteText,
  type SketchLine,
  type SketchPiece,
  type SketchState,
} from './workflowSketch';

/**
 * One of the canvas's own node components, drawn outside a canvas.
 *
 * REACT FLOW HANDS A NODE COMPONENT A DOZEN PROPS OFF ITS STORE — `dragging`, `zIndex`,
 * `positionAbsoluteX`, and so on — and `NodeProps` requires all of them. The four `ScratchNode*`
 * components read `id`, `data` and `selected` and nothing else, which is exactly what makes them
 * drawable here at all; the rest are filled with what a node nobody is dragging has. The cast is the
 * one `scratchNodes.render.test.ts` already makes, for the same reason: the registry is typed over
 * React Flow's wide `NodeProps` and there is no narrower type to satisfy.
 *
 * `isConnectable: false` IS THE HONEST VALUE HERE. There is no canvas to draw a line on, so the
 * handles are what a field's name and type hang off rather than something to drag from.
 */
function drawNode(
  kind: ScratchNodeType,
  id: string,
  data: ScratchNodeData | ScratchNoteData
): JSX.Element {
  return createElement(scratchNodeTypes[kind], {
    id,
    data,
    type: kind,
    dragging: false,
    zIndex: 0,
    isConnectable: false,
    positionAbsoluteX: 0,
    positionAbsoluteY: 0,
  } as never);
}

export interface WorkflowSketchProps {
  /** The name the workflow is registered and opened under — the sketch's subject, and its key. */
  workflow: string;
  sketch: SketchState;
  /** The catalog, the workflow listing and the lake, as the page already holds them. Nothing here
   *  fetches: a second lookup would be a second opinion about what this installation has. */
  catalogue: ScratchCatalogue;
  /**
   * The document, edited.
   *
   * ABSENT LEAVES THE TAB READ-ONLY, which is what a surface with nowhere to send a change should
   * do rather than offer a control that does nothing. It is one channel for every gesture — place,
   * remove, retype a note — so the page owns the document and this component owns none of it.
   */
  onChange?: (next: ScratchDocument) => void;
  /** Write the sketch. Absent for the same reason as `onChange`. */
  onSave?: () => void;
  /** Edited since the last write. */
  dirty?: boolean;
  /** A write is in flight, so the button refuses a second click rather than sending a second save. */
  saving?: boolean;
  /** Why the last write did not land. Kept apart from `unreadable`: a sketch that could not be
   *  STORED is still on screen and still editable, and clearing it off would throw the drawing away
   *  to report that saving it failed. */
  saveError?: string | null;
}

export function WorkflowSketch({
  workflow,
  sketch,
  catalogue,
  onChange,
  onSave,
  dirty,
  saving,
  saveError,
}: WorkflowSketchProps): JSX.Element {
  const doc = sketchDocument(sketch);
  const reading = readSketch(doc, catalogue);
  const ports = documentPorts(doc, catalogue);

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden" data-testid="scratch-tab" data-workflow={workflow}>
      {/* WHAT THIS TAB IS ABOUT, AND IT IS NOT THE OPEN RUN. Every other reading on this page is
          scoped to the conversation the bar above names; a sketch is a property of the workflow and
          outlives every run of it, so the one line at the top says so rather than letting the run id
          overhead imply otherwise. */}
      <p className="m-0 shrink-0 border-b border-border px-4 py-1 text-[10.5px] text-muted-foreground">
        The sketch behind <code className="font-mono">{workflow}</code> — the Actors it is meant to be
        made of, including the ones nobody has built yet. About the workflow, not about the open run.
        Nothing on it runs (ADR 0026).
      </p>

      <SketchBar
        sketch={sketch}
        summary={sketchSummary(reading)}
        empty={reading.empty}
        {...(onSave ? { onSave } : {})}
        dirty={dirty ?? false}
        saving={saving ?? false}
        saveError={saveError ?? null}
      />

      <div className="min-h-0 flex-1 overflow-y-auto p-4" data-testid="scratch-board">
        {sketch.state === 'unreadable' ? (
          <p className="m-0 text-[12.5px]" data-testid="scratch-unreadable">
            {/* NOT "NO SKETCH". A read that failed and a workflow nobody has drawn one for are two
                different sentences, and drawing the second over the first invites somebody to start
                again on top of a document that is already stored. */}
            Could not read this workflow’s sketch: {sketch.detail}
          </p>
        ) : sketch.state === 'unread' ? (
          <p className="m-0 text-[12.5px] text-muted-foreground" data-testid="scratch-loading">
            Reading the sketch for <code className="font-mono">{workflow}</code>…
          </p>
        ) : (
          <ReactFlowProvider>
            <ScratchPortsProvider value={ports}>
              {/* THE ACTIONS ARE `null` WHEN THERE IS NOWHERE TO SEND ONE, and the node components
                  draw no bin and no editable note against a null (`ScratchNodes.tsx`). A no-op pair
                  here would put a remove button on every piece of a read-only drawing — a control
                  that does nothing, which is how an operator learns not to trust the controls. */}
              <ScratchActionsProvider
                value={
                  onChange
                    ? {
                        onDelete: (id) => onChange(withoutNode(doc, id)),
                        onEditNote: (id, text) => onChange(withNoteText(doc, id, text)),
                      }
                    : null
                }
              >
                {reading.empty ? (
                  <Nothing workflow={workflow} />
                ) : (
                  <>
                    {reading.uses.length > 0 && (
                      <Group
                        testid="scratch-uses"
                        title="What it uses"
                        hint="Deployed, listed, or in the lake — the pieces this installation already has."
                        pieces={reading.uses}
                      />
                    )}
                    {reading.unbuilt.length > 0 && (
                      <Group
                        testid="scratch-unbuilt"
                        title="Yet to be built"
                        hint="Drawn here and not found. Not a fault — this is how somebody works out what to write next, and the spec hands these to an agent under the same heading."
                        pieces={reading.unbuilt}
                        unbuilt
                      />
                    )}
                    <Flow lines={reading.lines} />
                    {reading.notes.length > 0 && (
                      <section className="mt-3" data-testid="scratch-notes">
                        <Heading
                          title="What the author said"
                          hint="Verbatim, to the agent. “page 200 at a time”, “isolate, do not fail the run” — the things a person knows and a schema does not."
                        />
                        <div className="flex flex-wrap items-start gap-3">
                          {reading.notes.map((n) => (
                            <div key={n.id}>{drawNode('note', n.id, { note: n })}</div>
                          ))}
                        </div>
                      </section>
                    )}
                    <HandOver workflow={workflow} />
                  </>
                )}
              </ScratchActionsProvider>
            </ScratchPortsProvider>
          </ReactFlowProvider>
        )}

        {onChange && sketch.state !== 'unread' && sketch.state !== 'unreadable' && (
          <AddPiece doc={doc} catalogue={catalogue} onChange={onChange} />
        )}
      </div>
    </div>
  );
}

/* ───────────────────────────── the strip ───────────────────────────── */

/**
 * What the sketch is, and the one write there is.
 *
 * `SAVE` IS THE ONLY VERB ON THIS SURFACE. It stores a document; it starts nothing. The bar names
 * what would be written and when it last was, because a drawing an author edits for ten minutes and
 * never saves is the failure this feature can least afford — the whole value is that an agent reads
 * it back later.
 */
function SketchBar({
  sketch,
  summary,
  empty,
  onSave,
  dirty,
  saving,
  saveError,
}: {
  sketch: SketchState;
  summary: string;
  empty: boolean;
  onSave?: () => void;
  dirty: boolean;
  saving: boolean;
  saveError: string | null;
}): JSX.Element {
  return (
    <div
      className="flex shrink-0 flex-wrap items-baseline gap-2 border-b border-border bg-muted/40 px-4 py-1.5"
      data-testid="scratch-scope"
      data-state={sketch.state}
    >
      <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Sketch</span>
      <span className="font-mono text-[11.5px]" data-testid="scratch-summary">
        {sketch.state === 'unread'
          ? '…'
          : sketch.state === 'unreadable'
            ? 'unreadable'
            : empty
              ? 'none drawn'
              : summary}
      </span>
      {sketch.state === 'drawn' && sketch.updatedAt !== null && (
        <span className="font-mono text-[9.5px] text-muted-foreground" data-testid="scratch-saved-at">
          saved {new Date(sketch.updatedAt).toLocaleString()}
        </span>
      )}
      {dirty && (
        <span className="font-mono text-[9.5px] text-amber-500" data-testid="scratch-dirty">
          unsaved
        </span>
      )}
      {saveError && (
        // THE DRAWING STAYS ON SCREEN. A save that did not land is a reason to try again, not a
        // reason to lose what was drawn — clearing the board to report the failure would destroy
        // the only copy of it.
        <span className="text-[10.5px] text-destructive" data-testid="scratch-save-error">
          not saved: {saveError}
        </span>
      )}
      {onSave && (
        <button
          type="button"
          data-testid="scratch-save"
          disabled={saving || !dirty}
          title="store this drawing against this workflow"
          onClick={onSave}
          className="ml-auto rounded border border-border px-1.5 text-[10.5px] outline-none hover:bg-accent disabled:opacity-50 focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          {saving ? '…' : 'save'}
        </button>
      )}
    </div>
  );
}

/* ───────────────────────────── the drawing ───────────────────────────── */

/** A workflow nobody has sketched. NOT an error, and it says what the tab is for rather than that
 *  something is missing — an absence drawn as a fault is how an operator learns to distrust a
 *  surface, and most workflows will never have one. */
function Nothing({ workflow }: { workflow: string }): JSX.Element {
  return (
    <div data-testid="scratch-none">
      <p className="m-0 text-[12.5px]">
        No sketch has been drawn for <code className="font-mono">{workflow}</code>.
      </p>
      <p className="m-0 mt-1.5 text-[11.5px] text-muted-foreground">
        A sketch is where the SHAPE of a run gets worked out before fifty lines of Temporal have
        to be right — which Actors, in what order, and what the notes say that no schema can. Place
        the pieces below; an Actor that has not been built yet is a perfectly ordinary thing to draw,
        and is how you work out what to write next.
      </p>
    </div>
  );
}

/** One group of pieces, drawn with the canvas's own node components. */
function Group({
  testid,
  title,
  hint,
  pieces,
  unbuilt,
}: {
  testid: string;
  title: string;
  hint: string;
  pieces: SketchPiece[];
  unbuilt?: boolean;
}): JSX.Element {
  return (
    <section className="mt-1" data-testid={testid}>
      <Heading title={title} hint={hint} />
      <div className="flex flex-wrap items-start gap-3">
        {pieces.map((p) => (
          <Piece key={p.node.id} piece={p} unbuilt={unbuilt ?? false} />
        ))}
      </div>
    </section>
  );
}

/**
 * One piece, and what is true about it.
 *
 * THE NODE IS THE CANVAS'S OWN COMPONENT, UNTOUCHED. What tells a built Actor from one that is not
 * is the frame around it and the sentence under it, because the difference is not a property of the
 * node — it is a property of what this installation happens to have this morning, and a node that
 * redrew itself amber would be a drawing that changed when somebody deployed something.
 */
function Piece({ piece, unbuilt }: { piece: SketchPiece; unbuilt: boolean }): JSX.Element {
  return (
    <div
      className={`rounded-md ${unbuilt ? 'border border-dashed border-amber-500/40 p-1' : ''}`}
      data-testid={`scratch-piece-${piece.node.id}`}
      data-standing={piece.standing}
    >
      {drawNode(piece.node.kind, piece.node.id, { node: piece.node })}
      {piece.detail && (
        <p
          // WIDTH BY INLINE STYLE, from the node's own constant, so the sentence wraps to the box it
          // is about rather than to whatever the flex row happened to give it — and so this file
          // adds no arbitrary Tailwind value the compiled stylesheet would have to grow for.
          style={{ maxWidth: NODE_W }}
          className={`m-0 mt-1 text-[9.5px] leading-tight ${
            unbuilt ? 'text-amber-500' : 'text-muted-foreground'
          }`}
          data-testid={`scratch-detail-${piece.node.id}`}
        >
          {piece.detail}
        </p>
      )}
    </div>
  );
}

/**
 * How it flows, in the sentences the spec uses.
 *
 * THE SAME WORDS `renderScratch` PUTS IN FRONT OF THE AGENT, so the author reads what the agent
 * reads. An empty list is a real drawing and says so: the pieces were placed and the order was not
 * drawn, which is a thing to know about a sketch rather than a hole in it.
 */
function Flow({ lines }: { lines: SketchLine[] }): JSX.Element {
  return (
    <section className="mt-3" data-testid="scratch-flow">
      <Heading
        title="How it flows"
        hint="An edge written a.field → b.field names the fields it carries; one with no fields says only that the first step feeds the second."
      />
      {lines.length === 0 ? (
        <p className="m-0 text-[11.5px] text-muted-foreground" data-testid="scratch-flow-none">
          Nothing is connected — the pieces are placed and the order is not drawn.
        </p>
      ) : (
        <ul className="m-0 list-none p-0">
          {lines.map((l) => (
            <li key={l.id} className="font-mono text-[11px]" data-testid={`scratch-line-${l.id}`}>
              {l.from} → {l.to}
              {l.label && <span className="ml-1.5 text-muted-foreground">({l.label})</span>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/**
 * How the drawing gets read back, which is the only reason to draw one.
 *
 * A SENTENCE, NOT A BUTTON. The spec is `renderScratch`'s and is served at
 * `/api/scratch/<id>/spec`; the `get_scratch` MCP tool fetches exactly that and hands it to an agent
 * with the Methods' descriptions joined in and everything the catalog does not have called out. What
 * an author could not know without being told is the ID — it is derived from the workflow now
 * (`workflowScratchId`) rather than typed, which is what makes the sketch findable and also what
 * makes it invisible. So the tab says it.
 *
 * AND IT IS TEXT BECAUSE THERE IS NOTHING HERE TO PRESS. Reading the sketch back is the agent's
 * side of the loop, not this surface's, and a control on this tab that reached out to an agent
 * would be the first step back towards a canvas that does things.
 */
function HandOver({ workflow }: { workflow: string }): JSX.Element {
  return (
    <p className="m-0 mt-3 text-[10.5px] text-muted-foreground" data-testid="scratch-handover">
      An agent reads this back with{' '}
      <code className="font-mono">get_scratch {workflowScratchId(workflow)}</code> — the same
      drawing, resolved against the catalog, with everything it names that does not exist called out
      by name. Nothing in it runs; what the agent writes from it is what does.
    </p>
  );
}

function Heading({ title, hint }: { title: string; hint: string }): JSX.Element {
  return (
    <>
      <h3 className="m-0 text-[10px] uppercase tracking-wide text-muted-foreground">{title}</h3>
      <p className="m-0 mb-1.5 text-[10.5px] text-muted-foreground">{hint}</p>
    </>
  );
}

/* ───────────────────────────── placing something ───────────────────────────── */

/**
 * Put an Actor, or a note, on the sketch.
 *
 * FREE TEXT, WITH THE CATALOG OFFERED BESIDE IT. A picker would be a surface that can only draw
 * what is already deployed, which is exactly the sketch nobody needs — the interesting one names
 * the Method you are about to go and write. So the catalog fills a `datalist`: it makes the built
 * case two keystrokes and refuses nothing, which is `resolveScratch`'s own rule (reported, never
 * corrected, never refused) expressed as an input.
 *
 * THERE IS NO RUN BUTTON HERE EITHER, and there is nothing for one to act on: this form produces a
 * node in a document. What starts a workflow is the input form and the queue on the page that knows
 * which folder is registered, and it stays there.
 */
function AddPiece({
  doc,
  catalogue,
  onChange,
}: {
  doc: ScratchDocument;
  catalogue: ScratchCatalogue;
  onChange: (next: ScratchDocument) => void;
}): JSX.Element {
  const [actor, setActor] = useState('');
  const [version, setVersion] = useState('');
  const [method, setMethod] = useState('');

  const place = (): void => {
    // AN ACTOR WITH NO NAME IS NOT A PIECE. Everything else about it may be blank — a version
    // nobody has cut and a Method nobody has written are the design case — but a node with no name
    // says nothing to the author or to the agent, and would have to be deleted to be fixed.
    if (!actor.trim()) return;
    onChange(withActor(doc, mintSketchId(), { actor, version, method }));
    setActor('');
    setVersion('');
    setMethod('');
  };

  /** Enter places, from any of the three fields. Not a `<form>`: this section sits inside the
   *  Workflows page, and a nested form is invalid markup whose submit would reload the app. */
  const onEnter = (e: KeyboardEvent<HTMLInputElement>): void => {
    if (e.key === 'Enter') place();
  };

  return (
    <section className="mt-3 border-t border-border pt-3" data-testid="scratch-add">
      <Heading
        title="Place a piece"
        hint="An Actor this workflow uses. One that does not exist yet is a legitimate thing to draw — the sketch reports it and never refuses it."
      />
      <div className="flex flex-wrap items-baseline gap-1.5">
        <input
          className="rounded border border-border bg-transparent px-1.5 py-0.5 font-mono text-[11px] outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          placeholder="actor"
          list="scratch-actor-names"
          value={actor}
          onChange={(e) => setActor(e.target.value)}
          onKeyDown={onEnter}
          data-testid="scratch-add-actor"
          spellCheck={false}
        />
        <input
          className="w-16 rounded border border-border bg-transparent px-1.5 py-0.5 font-mono text-[11px] outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          placeholder="version"
          list="scratch-actor-versions"
          value={version}
          onChange={(e) => setVersion(e.target.value)}
          onKeyDown={onEnter}
          data-testid="scratch-add-version"
          spellCheck={false}
        />
        <input
          className="rounded border border-border bg-transparent px-1.5 py-0.5 font-mono text-[11px] outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          placeholder="method"
          list="scratch-actor-methods"
          value={method}
          onChange={(e) => setMethod(e.target.value)}
          onKeyDown={onEnter}
          data-testid="scratch-add-method"
          spellCheck={false}
        />
        <button
          type="button"
          data-testid="scratch-add-place"
          title="draw this Actor on the sketch"
          onClick={place}
          className="flex items-baseline gap-1 rounded border border-border px-1.5 py-0.5 text-[10.5px] outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          <Plus size={11} />
          place
        </button>
        <button
          type="button"
          data-testid="scratch-add-note"
          title="add a note — it travels to the agent verbatim"
          onClick={() => onChange(withNote(doc, mintSketchId(), ''))}
          className="flex items-baseline gap-1 rounded border border-border px-1.5 py-0.5 text-[10.5px] outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          <StickyNote size={11} />
          note
        </button>
      </div>

      {/* WHAT IS DEPLOYED, OFFERED AND NOT ENFORCED. A `datalist` is a suggestion list the browser
          draws under a free-text input: typing a name that is not in it is allowed and is the whole
          design case. */}
      <datalist id="scratch-actor-names">
        {[...new Set(catalogue.actors.map((a) => a.name))].map((n) => (
          <option key={n} value={n} />
        ))}
      </datalist>
      <datalist id="scratch-actor-versions">
        {[...new Set(catalogue.actors.map((a) => a.version))].map((v) => (
          <option key={v} value={v} />
        ))}
      </datalist>
      <datalist id="scratch-actor-methods">
        {[...new Set(catalogue.actors.flatMap((a) => a.operations.map((o) => o.name)))].map((m) => (
          <option key={m} value={m} />
        ))}
      </datalist>
    </section>
  );
}

/* ───────────────────────────── wired to the workflow ───────────────────────────── */

/**
 * The tab's state, read and written: the sketch for one workflow.
 *
 * A HOOK, HELD BY THE PAGE, AND THAT IS NOT A STYLE CHOICE. `WorkflowThread` renders ONE tab at a
 * time, so a component that owned this state would be unmounted the moment somebody looked at the
 * Transcript
 * — and the four pieces they had just placed and not yet saved would be gone when they came back,
 * silently, with the tab redrawing as though nothing had been drawn. Scratch is the only one of the
 * five readings that holds anything a person typed, so it is the only one that has to outlive its
 * own tab. The page owns it; the view is pure.
 *
 * IT IS KEYED TO THE WORKFLOW AND TO NOTHING ELSE. `workflowScratchId` derives the store key from
 * the name the folder is registered under, so opening a workflow opens its sketch and switching
 * workflows switches it — there is no list to pick from, which is the whole of what was wrong with
 * the surface this replaces.
 *
 * NOTHING IS FETCHED THAT THE PAGE ALREADY HOLDS. The Actor catalog, the workflow listing and the
 * lake come in as they are, because a second lookup would be a second opinion about what this
 * installation has, arriving at a different moment and with nothing to say which was right
 * (`scratchInspect.ts` states the same rule). The one call made here is the column schema, which is
 * catalog metadata rather than a scan — `querySchema` runs `DESCRIBE`, which opens no data file.
 *
 * A SAVE THAT ARRIVES AFTER A SWITCH IS DISCARDED, and so is a read. Both carry the workflow they
 * were asked for; painting either onto another workflow's tab is the same bug the run thread refuses
 * one layer up, and here it would put one run's design under another run's name.
 */
export function useWorkflowSketch(
  workflow: string,
  /** `.kontra/workflows/` as the page last listed it; `null` before the listing answered. */
  workflows: readonly WorkflowFile[] | null,
  /** What workers described when they SERVED a workflow, keyed by `@workflow.defn` type. */
  registered: readonly WorkflowDescriptor[]
): WorkflowSketchProps {
  const allActors = useAppStore((s) => s.catalog);
  const folders = useRegisteredFolders('actor');
  /** NO FOLDER, NO ACTOR — the same narrowing the palette made. The raw catalog is everything any
   *  worker ever registered on any machine, most of it code that is not on this disk, and a sketch
   *  is about what somebody can actually build against here. */
  const actors = useMemo(
    () => registeredActors(folders.sources, allActors),
    [folders.sources, allActors]
  );
  const datasets = useAppStore((s) => s.datasets);
  /** WHEN the lake listing was measured. `0` means nothing has answered — which is not an empty
   *  lake, and `readSketch` refuses to call a Dataset unbuilt on the strength of it. */
  const datasetsAt = useAppStore((s) => s.datasetsAt);
  const [columns, setColumns] = useState<SchemaEntry[] | null>(null);

  const [sketch, setSketch] = useState<SketchState>({ state: 'unread' });
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  useEffect(() => {
    void fetchSchema()
      .then(setColumns)
      // `null` ON A FAILURE, not `[]`: a Dataset node may not say the lake has no such columns on
      // the strength of a question nobody got an answer to.
      .catch(() => undefined);
  }, []);

  useEffect(() => {
    let live = true;
    setSketch({ state: 'unread' });
    setDirty(false);
    setSaveError(null);
    if (!workflow) {
      setSketch({ state: 'none' });
      return;
    }
    void fetchWorkflowSketch(workflow)
      .then((rec) => {
        if (!live) return;
        setSketch(
          rec
            ? // NORMALISED ON THE WAY IN. A stored node with no position, or with `NaN` for one,
              // is a document the server's own parse accepts (`isPoint` takes any two numbers) —
              // and `at` is what a later canvas lays out from, so repairing it here is what keeps
              // this tab and any drawing surface reading the same document.
              { state: 'drawn', document: normaliseScratchDocument(rec.document), updatedAt: rec.updatedAt }
            : { state: 'none' }
        );
      })
      .catch((err: unknown) => {
        if (live) setSketch({ state: 'unreadable', detail: err instanceof Error ? err.message : String(err) });
      });
    return () => {
      live = false;
    };
  }, [workflow]);

  const change = useCallback((next: ScratchDocument) => {
    // EDITED IS `drawn`, WHATEVER IT WAS. Placing the first piece on a workflow that had no sketch
    // is the same gesture as placing the ninth, so there is no "create" to press first — and
    // `updatedAt` stays null until something has actually been stored.
    setSketch((was) => ({
      state: 'drawn',
      document: next,
      updatedAt: was.state === 'drawn' ? was.updatedAt : null,
    }));
    setDirty(true);
    setSaveError(null);
  }, []);

  const wanted = useRef(workflow);
  wanted.current = workflow;

  const save = useCallback(() => {
    if (sketch.state !== 'drawn' || saving) return;
    const forWorkflow = workflow;
    setSaving(true);
    setSaveError(null);
    void saveWorkflowSketch(forWorkflow, sketch.document)
      .then((rec) => {
        // A REPLY FOR A WORKFLOW THIS TAB HAS LEFT IS DROPPED. It is the store's own narrowing of a
        // document that belongs to another subject, and painting it here would put one run's
        // design under another run's name.
        if (wanted.current !== forWorkflow) return;
        setSketch({ state: 'drawn', document: rec.document, updatedAt: rec.updatedAt });
        setDirty(false);
      })
      .catch((err: unknown) => {
        if (wanted.current !== forWorkflow) return;
        setSaveError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (wanted.current === forWorkflow) setSaving(false);
      });
  }, [saving, sketch, workflow]);

  const catalogue = useMemo<ScratchCatalogue>(
    () => ({ actors, workflows, registered, datasets, datasetsAt, columns }),
    [actors, workflows, registered, datasets, datasetsAt, columns]
  );

  return {
    workflow,
    sketch,
    catalogue,
    onChange: change,
    onSave: save,
    dirty,
    saving,
    saveError,
  };
}
