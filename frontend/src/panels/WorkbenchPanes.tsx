/**
 * The workbench's two drawn parts that are not the editor: the folder's files, and the serve
 * console under it.
 *
 * WHY THEY ARE NOT IN `FolderWorkbench.tsx`. That file imports `WorkerPane`, which imports
 * `TerminalTile`, which imports xterm — and xterm touches `self` at MODULE LOAD, so importing
 * anything from the workbench in this suite (node, no jsdom) fails before a single test runs with
 * `ReferenceError: self is not defined`. These two are the parts with a contract worth drawing in a
 * test, so they live where a test can reach them. Nothing in here talks to a server or to a
 * terminal: props in, markup out.
 */

import { TerminalSquare } from 'lucide-react';
import type { ActorServeResult, Source } from '../run/api';
import { fmtBytes, type FileRow } from './folderWorkbench';
import { Button } from '@/components/ui/button';

/**
 * The folder's files, one row each.
 *
 * Its own component because it is the part with a contract: a row per file, the unsaved dot on the
 * ones that have edits nobody has written, and `new` on a file that is not on disk yet. Exported so
 * `folderWorkbench.render.test.ts` can draw it without a server.
 */
export function WorkbenchFiles({
  rows,
  selected,
  onSelect,
}: {
  rows: FileRow[];
  selected: string;
  onSelect(name: string): void;
}): JSX.Element {
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-0.5 overflow-y-auto p-1.5">
      {rows.length === 0 ? (
        <p className="m-0 p-2 text-[10.5px] text-muted-foreground" data-testid="workbench-no-files">
          nothing readable in this folder — a file must be a plain name (no dotfiles, no
          subdirectories) for the editor to open it
        </p>
      ) : (
        rows.map((f) => (
          <button
            key={f.name}
            className={`flex w-full items-baseline gap-1.5 rounded px-2 py-1.5 text-left ${
              f.name === selected ? 'bg-accent' : 'hover:bg-accent/50'
            }`}
            data-testid={`workbench-file-${f.name}`}
            data-dirty={f.dirty ? 'true' : undefined}
            onClick={() => onSelect(f.name)}
            title={f.bytes === null ? `${f.name} — not written yet` : f.name}
          >
            <span className="min-w-0 flex-1 truncate font-mono text-[11.5px]">{f.name}</span>
            {/* The dot is the whole unsaved story on this row: switching files keeps the buffer, so
                a file can sit here edited and unwritten while another one is open. */}
            {f.dirty && <span className="shrink-0 text-[13px] leading-none text-amber-500">•</span>}
            <span className="shrink-0 font-mono text-[9px] tabular-nums text-muted-foreground">
              {f.bytes === null ? 'new' : fmtBytes(f.bytes)}
            </span>
          </button>
        ))
      )}
    </div>
  );
}

/**
 * Serve this Actor, and what serving it answered.
 *
 * THE SESSION AND THE ATTACH COMMAND ARE THE WHOLE REPORT, because they are what an operator does
 * next: the pane above is a SNAPSHOT (ADR 0020 — a live attach is an sshd session and a PTY, and it
 * is promoted on the Monitor, not here), so reading a long traceback or typing into the worker means
 * attaching in a terminal. A "served ✓" with no name would leave them to guess it, and the guess —
 * `probe-0.1.0` — is exactly the one tmux rewrites out from under them.
 *
 * THERE IS NO MODE CONTROL, and that is the decision this component is built around rather than a
 * feature nobody got to. `kontra serve --actor` can also start managed containers or place the Worker
 * on Machines that keep billing after the tab is closed; a picker here would make "try this edit"
 * and "deploy this" the same gesture, one selection apart, on the one surface whose entire value is
 * that starting and stopping is cheap and reversible. `backend/src/actorControl.ts` holds the
 * reasoning; this is where it has to keep being true.
 */
export function ServeConsole({
  source,
  session,
  served,
  error,
  serving,
  busy,
  onServe,
  onRestart,
}: {
  source: Source;
  /** The session the pane beside the editor is watching. Passed in rather than derived again here,
   *  because a console naming one session while the pane draws another is two workers on one
   *  screen and no way to tell which is which. */
  session: string;
  served: ActorServeResult | null;
  /** The serve's refusal, verbatim — it carries the CLI's own last lines. */
  error: string | null;
  /**
   * A worker is ALREADY serving this Actor — the 409, which is a question rather than a failure.
   *
   * Its own prop rather than a shape read out of `error`, because it is answered with a button and
   * drawn in a different colour. And it must be answerable: a worker holds the code it imported at
   * boot, so an operator who edits a file and presses Serve on a live session is told "already
   * serving" — true, and the most misleading sentence available, because their edit is not running
   * and nothing on screen says so.
   */
  serving: string | null;
  onRestart(): void;
  /**
   * What the workbench is doing, if anything — the same value the Save button reads.
   *
   * ANY work disables the button, not just a serve. A serve that overtook a save in flight would
   * start a worker on the file as it is on DISK, which is the version the operator is in the middle
   * of replacing — and the pane would then show a traceback from code that is no longer in the
   * editor.
   */
  busy: string | null;
  onServe(): void;
}): JSX.Element {
  const restarting = busy === 'restart';
  return (
    <div className="flex flex-col gap-2.5" data-testid="actor-serve">
      <div className="flex flex-wrap items-center gap-2.5">
        <Button
          size="sm"
          variant="outline"
          className="h-7 px-2.5 text-[11.5px]"
          data-testid="actor-serve-button"
          disabled={busy !== null}
          title={`start ${source.name}'s worker on this machine, in a tmux session called ${session}`}
          onClick={onServe}
        >
          <TerminalSquare size={13} className="mr-1.5" />
          {busy === 'serve' ? 'Serving…' : 'Serve'}
        </Button>
        <span className="text-[11.5px] text-muted-foreground">
          starts this Actor&rsquo;s worker <strong className="font-semibold">on this machine</strong>
          , in tmux — the pane beside the editor is that worker.
        </span>
      </div>

      {/* SAID, not merely absent. An operator who has run `kontra serve --actor` knows there are
          other placements and will go looking for the control; a surface that silently lacks one
          reads as unfinished rather than as decided. */}
      <p className="m-0 text-[10.5px] leading-snug text-muted-foreground">
        Local only. Putting this Actor on Machines that keep costing money after the tab is closed
        is a command you type, with the flags in front of you — not the neighbour of the button you
        press to try an edit.
      </p>

      <dl className="m-0 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1" data-testid="actor-serve-report">
        <dt className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">
          session
        </dt>
        <dd className="m-0 min-w-0 break-all font-mono text-[11.5px]" data-testid="actor-session">
          {session}
          {served === null && (
            <span className="ml-2 text-[10.5px] text-muted-foreground">
              — the name this Actor&rsquo;s worker runs under, served from here or from a terminal
            </span>
          )}
        </dd>
        <dt className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">
          attach
        </dt>
        {/* THE SERVER'S OWN STRING when there is one. It is minted beside the session it names, so
            printing it back is the proof that the pane above and the terminal an operator opens are
            the same tmux session and not two spellings of one intention. */}
        <dd className="m-0 min-w-0 break-all font-mono text-[11.5px]" data-testid="actor-attach">
          {served ? served.attach : `tmux attach -t ${session}`}
        </dd>
        {served && (
          <>
            <dt className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">
              from
            </dt>
            <dd className="m-0 min-w-0 break-all font-mono text-[11.5px] text-muted-foreground">
              {served.path}
            </dd>
          </>
        )}
      </dl>

      {/* AMBER, NOT RED, AND WITH THE ANSWER ON IT. Nothing went wrong: there is a worker there.
          The button says what it costs — the running worker dies, and anything in flight on it dies
          too — because "Restart" alone reads as free on a page where every other control is. */}
      {serving && (
        <div
          className="flex flex-wrap items-center gap-2.5 rounded border border-amber-500/40 bg-amber-500/10 p-2"
          data-testid="actor-serve-serving"
        >
          <p className="m-0 min-w-0 flex-1 text-[11.5px] leading-snug text-amber-500">{serving}</p>
          <Button
            size="sm"
            variant="outline"
            className="h-7 shrink-0 px-2.5 text-[11.5px]"
            data-testid="actor-restart-button"
            disabled={busy !== null}
            title={`kill the worker in ${session} and start a new one from the code on disk — anything running on it now is lost`}
            onClick={onRestart}
          >
            <TerminalSquare size={13} className="mr-1.5" />
            {restarting ? 'Restarting…' : 'Restart it'}
          </Button>
        </div>
      )}

      {error && (
        <p
          className="m-0 whitespace-pre-wrap rounded border border-destructive/40 bg-destructive/10 p-2 font-mono text-[11.5px] text-destructive"
          data-testid="actor-serve-error"
        >
          {error}
        </p>
      )}

      {/* THE COMMAND THIS BUTTON IS, printed. It is what the server runs (`serveActor`), so an
          operator who needs a flag it does not pass can take it to a terminal and start from here
          rather than from the manual. */}
      <pre className="m-0 overflow-x-auto rounded bg-muted/50 px-2 py-1.5 font-mono text-[11px] text-muted-foreground">
        {`kontra serve --actor ${source.path} --mode local --tmux`}
      </pre>
    </div>
  );
}
