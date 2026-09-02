/**
 * What is on this disk, at this digest — the strip that pairs a READ-ONLY source view with the two
 * facts it exists to carry (ADR 0030).
 *
 * THE VIEWER STOPPED WRITING, so the panel's job changed: it is no longer where an operator edits
 * their workflow, it is where they confirm what is deployed. Two things make that confirmation
 * possible and neither was on the old editor:
 *
 *   - THE REGISTERED DIGEST, beside the source. A served workflow polls `wf-<name>-<digest12>`,
 *     bound to the folder's content (GitHub #15); an actor carries its OCI image digest (ADR 0011).
 *     Showing it next to the bytes on disk is the whole point of losing the write — a dashboard that
 *     could write let the file and the registered digest disagree with nothing on screen saying so,
 *     and this is the screen that now says so.
 *   - THE PATH, copyable, and a plain "open in your editor" hint. The premise of the direction is
 *     that the operator already has an editor; the fastest way back to it is the absolute path, so
 *     the strip hands it over rather than making them read it off a list and retype it.
 *
 * IT LIVES IN ITS OWN FILE so a test can draw it: both `WorkflowsPage.tsx` and `FolderWorkbench.tsx`
 * import CodeMirror (and the workbench, xterm) at module load, and the web suite runs in node with
 * no DOM — the same reason `WorkflowContract` and `ActorCard` are their own files.
 */

import { useState } from 'react';
import { Check, Copy, SquarePen } from 'lucide-react';

export function SourceProvenance({
  path,
  digest,
  digestLabel = 'registered digest',
  digestAbsent = 'not served yet',
  testid = 'source-provenance',
  className = '',
}: {
  /** The absolute folder path the source lives in — what the operator opens in their own editor. */
  path: string;
  /**
   * The digest of the code that is REGISTERED, shown beside the source on disk. Empty/absent means
   * no worker has registered this yet, which is drawn as its own state and never as a blank digest.
   */
  digest?: string;
  digestLabel?: string;
  /** What to draw where the digest would be when nothing has registered — "not served yet" for a
   *  workflow, "unpinned" for an actor. */
  digestAbsent?: string;
  testid?: string;
  className?: string;
}): JSX.Element {
  // A momentary "copied" tick, not a toast: the affordance is a path an operator pastes into their
  // own editor, and the only feedback it needs is that the clipboard now holds it.
  const [copied, setCopied] = useState(false);
  const copy = (): void => {
    void navigator.clipboard?.writeText(path).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    });
  };

  return (
    <div
      className={`flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-border bg-muted/40 px-2.5 py-1 text-[10px] text-muted-foreground ${className}`}
      data-testid={testid}
    >
      <button
        type="button"
        className="flex min-w-0 items-center gap-1 rounded font-mono hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
        onClick={copy}
        data-testid={`${testid}-copy`}
        title="copy this path — it is what you open in your own editor"
      >
        {copied ? (
          <Check size={11} className="shrink-0 text-emerald-500" />
        ) : (
          <Copy size={11} className="shrink-0" />
        )}
        <span className="min-w-0 truncate">{path}</span>
      </button>

      {/* NO WRITE PATH, AND THE HINT SAYS WHERE ONE IS. The viewer is read-only on purpose (ADR
          0030); the operator already has an editor, so this points at it rather than pretending to
          be one. */}
      <span
        className="flex shrink-0 items-center gap-1"
        data-testid={`${testid}-open`}
        title="this view is read-only — edit in your own editor, then re-serve to change what is deployed"
      >
        <SquarePen size={11} />
        open in your editor
      </span>

      <span className="ml-auto flex shrink-0 items-center gap-1" data-testid={`${testid}-digest`}>
        <span className="uppercase tracking-wide opacity-70">{digestLabel}</span>
        <code
          className="font-mono text-foreground"
          title={
            digest
              ? 'the digest of the code currently registered and serving — a source on disk that no longer matches it reads as a mismatch here, not as a silent write'
              : 'no worker has registered this yet, so there is no deployed digest to compare the source against'
          }
        >
          {digest || digestAbsent}
        </code>
      </span>
    </div>
  );
}
