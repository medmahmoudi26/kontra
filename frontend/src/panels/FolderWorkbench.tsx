/**
 * A registered folder, opened: its files on the left, one of them in a READ-ONLY viewer on the
 * right — what is actually on this disk, at this digest (ADR 0030).
 *
 * THE SAME VIEWER THE WORKFLOWS PAGE HAS, and deliberately not a second one — CodeMirror, per-file
 * grammar from `workbenchEditor`, `editable={false}`. It used to write: CodeMirror with the
 * `PY_INDENT` unit, `indentWithTab` and Cmd/Ctrl-S. That was reversed because a dashboard that
 * writes to disk lets the file and the registered digest disagree, and the browser's job here is to
 * SHOW what is deployed, not to compete with the editor the operator already has. ADR 0020 made the
 * same call for Terminals. The grammar stays — highlighting, `.go` support, scrolling — because the
 * viewer is the point; the write is what goes.
 *
 * WHAT IS DIFFERENT IS THE FILE LIST, because an Actor is a FOLDER and a workflow file was a file.
 * `actor.py` is the code, `actor.json` is the manifest the SDKs read, and `description.md` is where
 * a Method's documentation comes from — all three matter, and a viewer that could open only the
 * marker (which is what `GET …/file` with no name does) would be a viewer for the least interesting
 * of them. `description.md` still renders its markdown preview beside itself when it is open; what
 * went with the write is the button that CREATED one, since creating a file is a write and the
 * operator makes it in their own editor now.
 *
 * THE ONE THING THAT EXECUTES IS SERVE, and it starts a worker HERE — on the machine the
 * orchestrator is on, in tmux, and nowhere else. `serveActor` records why that is a decision and
 * not a first step; what this file adds is the pane beside the viewer, because a worker that starts
 * and registers nothing is this system's hardest failure to see. Serve returns, the catalog says
 * what it always said, and the traceback is in a pane an operator had to know to go and attach to.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { ArrowLeft, Columns2, RefreshCw, Rows2 } from 'lucide-react';
import CodeMirror from '@uiw/react-codemirror';
import {
  AlreadyServingError,
  fetchSourceFile,
  fetchSourceFiles,
  serveActorSource,
  type ActorServeResult,
  type Source,
} from '../run/api';
import { actorSession } from '@core/panels/tmux';
import { WorkerPane } from './WorkerPane';
import { SourceProvenance } from './SourceProvenance';
import { PaneResizer, usePaneHeight } from './chrome/PaneResizer';
import { PaneStack } from './chrome/PaneStack';
import { SideDockControls, SideRail, SideResizer, useSideDock } from './chrome/SideDock';
import {
  bufferOf,
  emptyWorkbench,
  fileRows,
  languageOf,
  listed,
  opened,
  refused,
  selectFile,
  type WorkbenchState,
} from './folderWorkbench';
import { ServeConsole, WorkbenchFiles } from './WorkbenchPanes';
import { editorMode } from './workbenchEditor';
import Markdown from './widgets/Markdown';
import { useAppStore } from '../state/store';
import { Button } from '@/components/ui/button';

/** The file list's range. Wider at the top than the shared default, because the thing it has to be
 *  able to show in full is an absolute path in a deep worktree. */
const FILES_BOUNDS = { min: 150, max: 520 };

/** The editor column's range when the split is a row. A Python file wants ~90 columns and the pane
 *  beside it needs enough left over to be worth having, so neither end is the shared default. */
const SPLIT_BOUNDS = { min: 320, max: 1400 };

export function FolderWorkbench({
  source,
  digest,
  onClose,
}: {
  source: Source;
  /** The digest of the code a worker REGISTERED for this folder — the actor's OCI image digest (ADR
   *  0011), threaded from the catalog so the read-only viewer (ADR 0030) shows what is deployed
   *  beside the files on disk. Absent until a worker has served it. */
  digest?: string;
  onClose(): void;
}): JSX.Element {
  const theme = useAppStore((s) => s.theme);
  const [state, setState] = useState<WorkbenchState>(emptyWorkbench);
  const [busy, setBusy] = useState<string | null>(null);
  /** What the serve THIS visit performed answered, and what it said when it refused. Kept apart
   *  from `state.error` — a refused read of `actor.json` and a worker that died at import are two
   *  different things to fix, and the second one's evidence is four lines of traceback. */
  const [served, setServed] = useState<ActorServeResult | null>(null);
  const [serveError, setServeError] = useState<string | null>(null);
  /** "A worker is already there" — the 409, held apart from `serveError` because it is answered
   *  with a button rather than read as a failure. */
  const [serving, setServing] = useState<string | null>(null);
  const [editorHeight, setEditorHeight] = usePaneHeight('actor-workbench-editor', 420);
  /** The file list: its own width, its own edge, its own collapsed state — all persisted. */
  const filesDock = useSideDock(
    'actor-workbench-files',
    { width: 228, side: 'left', collapsed: false },
    FILES_BOUNDS
  );
  /**
   * The editor/panes split. `collapsed` is reused as the ORIENTATION rather than adding a second
   * storage key for one boolean: `false` is the row (side by side, the default this was asked for),
   * `true` is the stack. `width` is the editor's column when it is a row.
   */
  const splitDock = useSideDock(
    'actor-workbench-split',
    { width: 720, side: 'left', collapsed: false },
    SPLIT_BOUNDS
  );
  const splitRow = !splitDock.collapsed;

  const kind = source.kind;
  const id = source.id;

  const load = useCallback(() => {
    let live = true;
    void fetchSourceFiles(kind, id)
      .then((got) => {
        if (live) setState((s) => listed(s, got.files, kind));
      })
      .catch((err: unknown) => {
        if (live) setState((s) => refused(s, message(err)));
      });
    return () => {
      live = false;
    };
  }, [kind, id]);

  // A DIFFERENT FOLDER IS A DIFFERENT WORKBENCH. Keeping the buffers across the switch would show
  // `probe`'s actor.py under `beacon`'s name until the read landed — and keeping the serve report
  // would name `probe`'s session beside `beacon`'s code, which is the one thing on this screen an
  // operator is going to copy into a `tmux attach`.
  useEffect(() => {
    setState(emptyWorkbench());
    setServed(null);
    setServeError(null);
    setServing(null);
    return load();
  }, [load]);

  const selected = state.selected;
  const held = state.buffers[selected];
  useEffect(() => {
    if (!selected || held) return;
    let live = true;
    void fetchSourceFile(kind, id, selected)
      .then((got) => {
        if (live) setState((s) => opened(s, selected, got.source));
      })
      .catch((err: unknown) => {
        if (live) setState((s) => refused(s, message(err)));
      });
    return () => {
      live = false;
    };
  }, [kind, id, selected, held]);

  const rows = useMemo(() => fileRows(state), [state]);
  const buffer = bufferOf(state);
  const language = languageOf(selected);
  const mode = editorMode(language.id);

  /**
   * Serve this Actor's worker — and, with `restart`, replace the one that is already there.
   *
   * THREE OUTCOMES, NOT TWO. It served; it refused; or a worker is already serving, which is
   * neither. The third arrives as `AlreadyServingError` (the route's 409) and lands in its own
   * state, because the only useful response to it is a button — and because reporting it in red
   * beside the others would call the desired end state a failure.
   */
  const serve = useCallback(
    (restart = false) => {
      setBusy(restart ? 'restart' : 'serve');
      setServeError(null);
      setServing(null);
      void serveActorSource(id, { restart })
        .then((result) => {
          setServed(result);
          setServeError(null);
          setServing(null);
        })
        // THE CLI's OWN LAST LINES, minus its banner. `serveActor` puts the final four MEANINGFUL
        // lines of the command's output in its refusal, and `asError` unwraps the `{"error":…}`
        // envelope — so what lands here is `serve failed (exit 1): <what kontra printed>`. "Could
        // not serve" would send an operator to the terminal to run the command again and read the
        // same output by hand.
        .catch((err: unknown) => {
          if (err instanceof AlreadyServingError) {
            // NOT cleared: a worker IS serving, and whatever this visit last served is still true.
            setServing(err.message);
            return;
          }
          setServed(null);
          setServeError(message(err));
        })
        .finally(() => setBusy(null));
    },
    [id]
  );

  /**
   * The session this Actor's worker runs in: the serve's own answer while there is one, and the
   * name derived from the folder otherwise.
   *
   * THE DERIVED NAME IS NOT A FALLBACK FOR A FAILED SERVE — it is the case where nothing was served
   * from HERE. An Actor served an hour ago, or from a terminal with `kontra serve --actor … --tmux`,
   * has a worker and a pane and no result in this component's state; without the derivation the
   * workbench would draw an empty rectangle beside a worker that is polling.
   *
   * ONE VALUE REACHES BOTH the pane and the console, so the session an operator reads off the
   * console and the pane they are looking at cannot be two different workers.
   */
  const derived = actorSession(source.name, source.version);
  const session = served?.session ?? derived;

  /* THE EDITOR, in one place. It is drawn on its own for a Workflow folder and beside the worker's
     pane for an Actor, and two copies of a CodeMirror configuration is exactly the drift this file's
     header says it exists to avoid. */
  const editor = (
    <div className="min-h-0 flex-1 overflow-hidden p-3">
      {buffer === undefined ? (
        <p className="m-0 text-[12px] text-muted-foreground" data-testid="workbench-empty">
          {selected ? `reading ${selected}…` : 'pick a file to view.'}
        </p>
      ) : (
        <div className="flex h-full min-h-0 flex-col overflow-hidden rounded border border-border">
          <div className="flex h-[28px] shrink-0 items-center border-b border-border bg-muted/60 px-2.5">
            <span className="font-mono text-[10px] text-muted-foreground">
              {buffer.text.split('\n').length} lines · {language.label} · read-only
            </span>
          </div>
          {/* WHAT IS ON THIS DISK, AT THIS DIGEST (ADR 0030). The folder's path — to open in the
              operator's own editor — beside the digest a worker registered for this actor (ADR
              0011). The viewer stopped writing, so this is where "the file and the deployed digest
              disagree" becomes visible instead of a silent overwrite. */}
          <SourceProvenance
            path={source.path}
            digest={digest}
            digestAbsent="unpinned"
            testid="workbench-source-provenance"
          />
          <div className="flex min-h-0 flex-1 overflow-hidden">
            <div className="min-h-0 flex-1 overflow-hidden">
              {/* READ-ONLY (ADR 0030). `editable={false}` refuses input while keeping the text
                  selectable, scrollable and syntax-highlighted — the viewer, minus the write. The
                  grammar and `.go` support in `mode.extensions` are exactly what stays. */}
              <CodeMirror
                value={buffer.text}
                height="100%"
                className="workflow-editor h-full text-[12px]"
                theme={theme === 'dark' ? 'dark' : 'light'}
                extensions={mode.extensions}
                editable={false}
              />
            </div>
            {/* MARKDOWN IS RENDERED BESIDE ITSELF, and the reason is not decoration: only the
                FIRST PARAGRAPH of description.md becomes the description every list on this
                page shows, and `firstParagraph` SKIPS heading lines — so an author who opens
                with `# probe` and expects that to be the summary has written a description of
                nothing. Seeing the structure is how they find that out here rather than from a
                row that says less than they wrote.

                The renderer is `widgets/Markdown` — the hardened one (no raw HTML, no remote
                images) — because there is exactly one markdown renderer in this app on
                purpose, and a second "but this file is ours" one is how the first becomes
                optional. */}
            {language.id === 'markdown' && (
              <div
                className="min-h-0 w-[42%] shrink-0 overflow-y-auto border-l border-border bg-muted/20 px-3 py-1.5"
                data-testid="workbench-preview"
              >
                <Markdown source={buffer.text} testId="workbench-markdown" />
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );

  const files = (
    <aside
      /* THE WIDTH IS THE OPERATOR'S. It was `w-[228px]`, and a registered path like
         `/root/kontra-local/.claude/worktrees/workflows-client/examples/python/probe` wraps to five
         lines inside it — on the one panel whose job is to say WHICH checkout is open. */
      style={{ flex: `0 0 ${filesDock.width}px`, width: filesDock.width, order: filesDock.side === 'left' ? -1 : 1 }}
      className="flex min-h-0 flex-col border-r border-border"
      data-testid="workbench-files-aside"
      data-side={filesDock.side}
    >
      <div className="shrink-0 border-b border-border px-3 py-2.5">
          <div className="flex items-center gap-1">
            <Button
              variant="ghost"
              size="sm"
              className="h-6 px-1.5 text-[10.5px]"
              data-testid="workbench-close"
              onClick={onClose}
            >
              <ArrowLeft size={11} className="mr-1" />
              {kind === 'actor' ? 'Actors' : 'Workflows'}
            </Button>
            <SideDockControls dock={filesDock} label="the file list" testid="workbench-files-dock" />
          </div>
          <div className="mt-1.5 flex items-baseline gap-1.5">
            <span className="min-w-0 truncate font-mono text-[12.5px] font-semibold">
              {source.name}
            </span>
            {source.version && (
              <span className="font-mono text-[9.5px] text-muted-foreground">v{source.version}</span>
            )}
          </div>
          {/* THE PATH, as text. Two checkouts of one actor hold the same filenames; the path is the
              only thing on this screen that says which one is being edited. */}
          <p
            className="m-0 mt-0.5 break-all font-mono text-[9.5px] leading-snug text-muted-foreground"
            data-testid="workbench-path"
          >
            {source.path}
          </p>
        </div>

        <WorkbenchFiles
          rows={rows}
          selected={selected}
          onSelect={(name) => setState((s) => selectFile(s, name))}
        />
    </aside>
  );

  return (
    <main className="flex min-h-0 min-w-0 flex-1 overflow-hidden" data-testid="folder-workbench">
      {filesDock.collapsed ? (
        <div className="flex min-h-0" style={{ order: filesDock.side === 'left' ? -1 : 1 }}>
          <SideRail
            label="Files"
            badge={rows.length}
            testid="workbench-files-rail"
            onExpand={() => filesDock.setCollapsed(false)}
          />
        </div>
      ) : (
        <>
          {files}
          <SideResizer
            width={filesDock.width}
            onWidth={filesDock.setWidth}
            side={filesDock.side}
            label="the file list"
            bounds={FILES_BOUNDS}
            testid="workbench-files-resizer"
          />
        </>
      )}

      <section className="flex min-w-0 flex-1 flex-col overflow-hidden">
        <header className="flex shrink-0 items-center gap-2 border-b border-border px-4 py-2">
          <span className="min-w-0 truncate font-mono text-[13px]" data-testid="workbench-open-file">
            {selected || '—'}
          </span>
          <span className="shrink-0 font-mono text-[9.5px] text-muted-foreground">
            {language.label}
          </span>
          <div className="ml-auto flex shrink-0 gap-1.5">
            {/* THE ORIENTATION, on the editor's own header, because it is a property of this pair
                and not of the page. Only for an Actor — a Workflow folder has no pane to sit
                beside, so the control would move nothing. */}
            {kind === 'actor' && (
              <Button
                variant="ghost"
                size="sm"
                className="h-6 px-1.5"
                data-testid="workbench-split-toggle"
                title={
                  splitRow
                    ? 'Stack the worker pane under the editor — a traceback is wide and wraps badly in half a screen'
                    : 'Put the worker pane beside the editor'
                }
                onClick={() => splitDock.setCollapsed(splitRow)}
              >
                {splitRow ? <Rows2 size={12} /> : <Columns2 size={12} />}
              </Button>
            )}
            <Button
              variant="ghost"
              size="sm"
              className="h-6 px-1.5"
              data-testid="workbench-reload"
              title="re-read the folder from disk — the viewer is read-only, so a file changed in your own editor shows up here on reload"
              onClick={load}
            >
              <RefreshCw size={12} />
            </Button>
          </div>
        </header>

        {state.error && (
          <p
            className="m-3 mb-0 shrink-0 whitespace-pre-wrap rounded border border-destructive/40 bg-destructive/10 p-2 font-mono text-[11.5px] text-destructive"
            data-testid="workbench-error"
          >
            {state.error}
          </p>
        )}

        {/* THE EDITOR AND THE WORKER'S PANE, side by side, and the console under them — the same
            three-part pairing the Workflows page has, for the same reason. A worker that boots and
            dies leaves the folder saved, the serve successful and every count on the Actors grid
            unchanged; the traceback is over there.

            ONLY FOR AN ACTOR. There is no `POST /api/sources/workflow/:id/serve` — a caller's
            workflow is served from the Workflows page, which owns its queue and its type — so a
            pane here for a Workflow folder would be a rectangle that can never fill. */}
        {kind === 'actor' ? (
          /* SIDE BY SIDE OR STACKED, AND THE OPERATOR PICKS. Neither is right for both jobs: code
             next to a worker pane is what you want while writing a Method (a Python file is narrow
             and a traceback is short), and stacked is what you want while reading one (a traceback
             is WIDE — paths, then a caret line — and wraps to mush in half a screen). This was
             pinned to stacked, with a comment claiming side by side, which is how it read as broken
             rather than as chosen. */
          <div
            className={`flex min-h-0 flex-1 overflow-hidden border-t border-border ${
              splitRow ? 'flex-row' : 'flex-col'
            }`}
            data-testid="workbench-split"
            data-orientation={splitRow ? 'row' : 'column'}
          >
            {splitRow ? (
              <>
                {/* `flex flex-col`, AND THAT IS THE WHOLE BUG. This wrapper was `display: block`
                    while the element inside it is `min-h-0 flex-1` — flex properties on a child of
                    a block parent do nothing, so the editor sized to its CONTENT. MEASURED opening
                    `main.go`: the editor was **4006px tall** inside a wrapper the row had stretched
                    to 758, with `scrollHeight === clientHeight` and therefore `canScroll: false`.
                    Nothing was scrollable because nothing overflowed — CodeMirror had simply grown
                    to the whole file, and this wrapper's `overflow-hidden` clipped it at the fold.
                    A definite height on the wrapper is not enough on its own; the chain from here
                    down to `.cm-scroller` has to be flex the whole way, or `height: 100%` resolves
                    against `auto` and means nothing. */}
                <div
                  className="flex min-h-0 flex-col overflow-hidden"
                  style={{ flex: `0 0 ${splitDock.width}px`, width: splitDock.width }}
                >
                  {editor}
                </div>
                <SideResizer
                  width={splitDock.width}
                  onWidth={splitDock.setWidth}
                  side="left"
                  label="the editor"
                  bounds={SPLIT_BOUNDS}
                  testid="workbench-split-resizer"
                />
              </>
            ) : (
              <>
                {/* `flex flex-col` for the reason the split branch above spells out, and
                    shrinkable for the reason WorkflowsPage's editor container records: a fixed
                    height that will not shrink overflows a column whose ancestors are all
                    `overflow-hidden`, and the page cannot be scrolled to get it back. */}
                <div
                  className="flex min-h-0 flex-col overflow-hidden"
                  style={{ height: editorHeight }}
                >
                  {editor}
                </div>

                <PaneResizer
                  height={editorHeight}
                  onHeight={setEditorHeight}
                  grow="down"
                  label="the editor"
                  testid="workbench-editor-resizer"
                />
              </>
            )}

            {/* THE TWO PANES UNDER THE EDITOR REORDER AND FOLD, and which order is right depends
                entirely on what the operator is doing. Serving a folder for the first time, the
                console is the thing being read; chasing a worker that booted and died, the pane
                holding the traceback is — and it was pinned beside the editor at 85% of its width,
                which is neither. Both are still on this screen with the code, which is the pairing
                that matters (the traceback is invisible on every other surface). */}
            <PaneStack
              stack="actor-workbench"
              className="min-h-0 flex-1 overflow-hidden"
              panes={[
                {
                  key: 'worker',
                  title: 'worker pane',
                  meta: served?.session ?? undefined,
                  body: (
                    <WorkerPane
                      session={served?.session ?? null}
                      derived={derived}
                      subject="actor"
                      className="min-h-0 flex-1"
                    />
                  ),
                },
                {
                  key: 'serve',
                  title: 'serve',
                  body: (
                    <div className="min-h-0 flex-1 overflow-y-auto p-3">
                      <ServeConsole
                        source={source}
                        session={session}
                        served={served}
                        error={serveError}
                        serving={serving}
                        busy={busy}
                        onServe={() => serve(false)}
                        onRestart={() => serve(true)}
                      />
                    </div>
                  ),
                },
              ]}
            />
          </div>
        ) : (
          editor
        )}
      </section>
    </main>
  );
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
