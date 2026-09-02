# 30. The code editors become read-only viewers: what is on this disk, at this digest

## Status

**Accepted.** Reverses commit `32c27b0 ui: the workflow editor is a code editor again` on the part
that made it *editable*, and keeps every part that made it *legible*. Extends **0020**'s decision —
a dashboard view of code the operator did not author here has no input path — from Terminals to the
two source viewers. Leans on **0011** (an actor's registered OCI digest) and GitHub #15 (a
workflow's derived `wf-<name>-<digest12>` queue) for the digest a viewer shows beside the source.
Does not touch **0023** §12's caller generation, which still writes — see the Decision. Supersedes no
ADR.

## Context

`FolderWorkbench.tsx` and `WorkflowsPage.tsx` each embedded a CodeMirror the operator could type into
and save back to disk with Cmd/Ctrl-S. That write is the problem. The premise of the whole
instrument-panel direction (`.scratch/instrument-panel/PRD.md`) is that **everybody already has a
code editor**, and the browser should stop competing with it and instead show the things an editor
cannot: what is registered, what is serving, what is running, and what a run produces. A dashboard
that also writes to disk introduces a class of bug the panel exists to remove — the file on disk and
the *registered* code can silently disagree, because the person editing in the browser is not the
task queue a worker is polling, and nothing on the screen says the two have diverged.

Three things were established against the real code before deciding.

1. **What `32c27b0` was actually solving was legibility, not authorship.** Its message argues the
   file "is PYTHON. Indentation is its syntax," and lists what a bare `<textarea>` lacked: syntax
   highlighting, **line numbers to read a traceback against**, bracket matching, and a real undo.
   Every one of those is a property of a *viewer*. The one thing on its list that is editing —
   `indentWithTab` and the four-space indent unit, guarding against a stray tab becoming a `TabError`
   the worker dies on at import — only matters when the browser is the thing writing the tab. Remove
   the write and that hazard leaves with it; keep the grammar and the line numbers and its real value
   stays. So a read-only viewer solves what `32c27b0` solved, and it does not reintroduce what
   `32c27b0` was reverting (a `<textarea>` with no highlighting) — it is a third position, not a
   round trip.

2. **CodeMirror's `editable={false}` is a viewer, not a disabled editor.** It sets
   `contenteditable=false`: input is refused, but the text stays **selectable, copyable and
   scrollable**, `basicSetup` still draws line numbers, and `python()` / `go()` still colour it.
   Measured against the suite: `workbenchEditor.test.ts` runs the real grammars and confirms
   `.go` parses as Go, markdown as markdown, and JSON gets no grammar rather than the wrong one —
   none of which depends on the editor being writable. The "keep everything else" requirement is
   therefore free: the same extension list, minus the two edit-only extensions.

3. **Only ONE of the two source-write routes is unused after this, and finding out which is the
   point.** There are two `PUT`s that write source from the browser:
   - `PUT /api/workflows/file/:name` (→ `workflowControl.saveWorkflow`) has exactly one caller, the
     Workflows editor's Save. Read-only ⇒ **unused ⇒ removed.**
   - `PUT /api/sources/:kind/:id/file` (→ `sources.writeInside`) is *also* the route the caller
     generator uses: `MethodCall.tsx` writes a **new** `workflow.py` into a registered workflow
     folder through it (ADR 0023 §12 — a Run is one execution of a caller's workflow, so `call`
     produces the caller file rather than dispatching). That is not an in-place edit of deployed
     code; it is the creation of an artefact the operator then serves. So this route **survives
     used**, and deleting it would break a feature the read-only decision has no quarrel with.

   A literal reading of "remove the write path" would have deleted both and taken caller generation
   with it. The write path being removed is the *editor's*, not every write of source anywhere.

## Decision

- **Both source viewers are read-only.** The CodeMirror in `WorkflowsPage.tsx` and the one in
  `FolderWorkbench.tsx` render with `editable={false}`. No `onChange`, no Cmd/Ctrl-S, no Save button.
  The workbench's "create `description.md`" button goes too — creating a file is a write, and the
  operator makes it in their own editor now; the markdown *preview* beside the file stays, because
  reading is what the viewer is for.

- **The extension list keeps the grammar and drops the edit.** `WorkflowsPage` goes from
  `[python(), indentUnit.of(PY_INDENT), keymap.of([indentWithTab])]` to `[python()]`. The workbench
  keeps `workbenchEditor.editorMode`'s per-file extensions unchanged — the indent extensions there
  are inert under a non-editable view, and rebuilding that shared, tested module to strip two inert
  entries would risk the `.go`/markdown highlighting this ADR is committed to keeping.

- **The panel shows the registered digest beside the source, in a new `SourceProvenance` strip.**
  The strip carries three things the old editor did not: the folder **path** with a copy affordance,
  an **"open in your editor"** hint (the premise of the direction, made actionable), and the
  **registered digest** — for a workflow the 12-hex suffix of its derived `wf-<name>-<digest12>`
  queue (`queueDigest`), for an actor its OCI image digest from the catalog (**0011**). Absent is its
  own state — "not served yet" for a workflow, "unpinned" for an actor — never a blank digest. This
  is where a disk that no longer matches what is serving becomes *visible*, which is the honest
  replacement for a write that would have made them silently agree.

- **`PUT /api/workflows/file/:name` and `saveWorkflow` are removed; `PUT /api/sources/:kind/:id/file`
  and `writeInside` stay.** The client `saveWorkflowSource` goes with its route; `saveSourceFile`
  stays because `MethodCall` still calls it. The state reducers in `folderWorkbench.ts` that model a
  buffer's edited/saved transitions are left as a pure, tested library — they are no longer wired to
  any control, and the component uses only their read half (open, select, list, the file rows).

- **The reason is the same one `0020` gave, and it is stronger than "we only write safe things."**
  A guarantee that rests on *there being nothing to write to* is more robust than one that rests on
  the write path being careful. `0020`: "A read-only guarantee that rests on 'we only ever write
  these two commands' is weaker than one that rests on 'there is nothing to write to.'" The Workflows
  route now has nothing to write to.

## Consequences

- **The browser can no longer make the file and the registered digest disagree** — it cannot change
  the file. When they *do* disagree (an edit in the operator's own editor that has not been
  re-served), the `SourceProvenance` strip shows the registered digest next to the newer bytes on
  disk, so the divergence reads as a fact on the screen rather than as a surprise at the next run.

- **Changing a workflow is now: edit in your editor → re-serve.** Re-serving re-derives the
  content-bound queue (GitHub #15), so the digest the strip shows moves exactly when the deployed
  code does. This is the loop the PRD names — "save in your own editor → the control plane notices →
  the form regenerates" — and slice 03's `--watch` re-registration is what makes the *form* current
  without a re-serve; the *running code* still changes only on an explicit serve.

- **A file changed on disk under the viewer is picked up by Reload, not by a keystroke.** The
  workbench's reload button already re-reads the folder from disk; its copy said edited buffers were
  kept across it, which no longer happens because nothing is edited. The button's help text now says
  what it is for: a file changed in the operator's own editor shows up here on reload.

- **Caller generation is unaffected and still writes.** `POST /api/sources/actor/:id/caller` returns
  source and `MethodCall` saves it through the surviving `PUT /api/sources/:kind/:id/file`. This ADR
  deliberately does not touch it; a future simplification that "removes the last source-write route"
  must not fold that write away with it.

- **Verification run, not asserted.** `PUT /api/workflows/file/nscheck` now returns `404`
  (`server.test.ts`); `queueDigest` extracts the trailing digest and reports empty for an unserved
  workflow (`workflowFile.test.ts`); `SourceProvenance` renders the path, the copy affordance, the
  "open in your editor" hint and the digest, and draws "not served yet"/"unpinned" when absent
  (`sourceProvenance.render.test.ts`); `.go` and markdown highlighting are unchanged
  (`workbenchEditor.test.ts`, still green).

## Alternatives considered

- **Keep the write, add a warning when disk ≠ registered digest.** Rejected: it keeps the class of
  bug and papers over it. The write is the only reason the two can diverge in the first place; a
  banner about a divergence the panel itself causes is weaker than not causing it, and it is the
  "we only write carefully" posture `0020` already argued against.

- **Delete both `PUT` source routes to satisfy "remove the write path" literally.** Rejected on
  finding 3 — it would break `0023` §12 caller generation, which writes a new file rather than
  editing deployed code. "The editor's write path" and "every write of source" are different sets.

- **Revert `32c27b0` wholesale back to a `<textarea>`.** Rejected on finding 1: that throws away the
  line numbers and highlighting a traceback is read against, which were the real value of the commit.
  Read-only keeps them; only the write is dropped.
