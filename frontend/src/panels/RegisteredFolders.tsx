/**
 * The folders an operator registered: the hook that holds them, and what can be done to one.
 *
 * ONE HOOK FOR BOTH PAGES, for the same reason `RegisterFolder` is one form: an Actor folder and a
 * Workflow folder differ in their marker file and in nothing else this owns, so two implementations
 * would drift on `actor.json` vs `workflow.py` and on which of them remembered that a discovered
 * folder cannot be forgotten.
 *
 * THE LIST THAT USED TO LIVE HERE IS GONE. It drew a shelf of folder rows above the Actors grid and
 * below the Workflows list — which, on an ordinary installation, is the folders those surfaces were
 * already naming, printed a second time in a second vocabulary. The folder is now a line on the row
 * that names it, and what is left here is the part that must not be written twice.
 *
 * THE PATH IS ON THE ROW, not a tooltip on it. Registration records WHERE the code is, and an
 * operator with three checkouts of the same actor is looking to find out which one this console is
 * serving. A name they already know, above a path they have to hover for, answers the question they
 * did not ask. It wraps rather than truncates for the same reason — the tail of a path is the half
 * that distinguishes two checkouts.
 *
 * A DISCOVERED FOLDER GETS NO FORGET BUTTON. `SourceStore.forget` refuses an `at:` id — there is no
 * registration to remove, the folder is simply in the default root — so a button would be an
 * affordance that exists only to 400. That is the rule two copies would have disagreed about, and
 * it is why `FolderActions` is one component rather than a shape each surface draws for itself.
 */

import { useCallback, useEffect, useState } from 'react';
import { Trash2 } from 'lucide-react';
import {
  fetchSources,
  forgetSource,
  type Source,
  type SourceKind,
} from '../run/api';
import { canForget, withRegistered, withoutSource } from './sourceFolders';
import { Button } from '@/components/ui/button';

/** What a page holds about its registered folders, and the three things it can do to them. */
export interface FolderShelf {
  kind: SourceKind;
  /** `~/.kontra/actors` or `~/.kontra/workflows`, as the SERVER resolves it — the form prefills it
   *  and must not guess it: `KONTRA_HOME` moves it, and the browser cannot read that. */
  defaultRoot: string;
  sources: Source[];
  /** Why the list is empty, when it is empty because something failed rather than because nothing
   *  is registered. The two are drawn differently; collapsing them is how a broken orchestrator
   *  reads as a clean install. */
  error: string | null;
  /** Fold in what `POST /api/sources/:kind` answered. */
  registered(source: Source): void;
  forget(source: Source): void;
  reload(): void;
  forgetting: string | null;
}

export function useRegisteredFolders(kind: SourceKind): FolderShelf {
  const [defaultRoot, setDefaultRoot] = useState('');
  const [sources, setSources] = useState<Source[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [forgetting, setForgetting] = useState<string | null>(null);

  const reload = useCallback(() => {
    void fetchSources(kind)
      .then((got) => {
        // A 200 that is not this shape is treated as an empty list rather than trusted: anything in
        // front of the orchestrator that answers `{}` — a proxy, a stub, a server too old for this
        // route — would otherwise put `undefined` where the list goes and take the whole page down
        // on the first `.map`, for a section that is a footnote on it.
        setDefaultRoot(typeof got.defaultRoot === 'string' ? got.defaultRoot : '');
        setSources(Array.isArray(got.sources) ? got.sources : []);
        setError(null);
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)));
  }, [kind]);

  useEffect(reload, [reload]);

  const forget = useCallback(
    (source: Source) => {
      setForgetting(source.id);
      void forgetSource(kind, source.id)
        .then(() => {
          setSources((prev) => withoutSource(prev, source.id));
          setError(null);
        })
        .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
        .finally(() => setForgetting(null));
    },
    [kind]
  );

  const registered = useCallback((source: Source) => {
    setSources((prev) => withRegistered(prev, source));
  }, []);

  return { kind, defaultRoot, sources, error, registered, forget, reload, forgetting };
}

/**
 * What can be DONE to one registered folder, wherever it is drawn.
 *
 * THE LIST THIS FILE USED TO EXPORT IS GONE, and this is what survived it. `FolderList` was a shelf
 * of folder rows drawn above the Actors grid and below the Workflows list, which on an ordinary
 * installation was the same folders those surfaces were already naming — the same workflow twice on
 * one screen, in two vocabularies. The folders moved onto the row that names them; the affordances
 * moved here.
 *
 * ONE IMPLEMENTATION, FOR THE REASON THE SHELF HAD ONE. An Actor folder and a Workflow folder differ
 * in their marker file and in nothing this draws, and the fact two copies would eventually disagree
 * about is the one that costs a 400: a DISCOVERED folder cannot be forgotten. `SourceStore.forget`
 * refuses an `at:` id — the folder is simply sitting in the default root, so there is no
 * registration to remove — and a surface that offered the button anyway would be an affordance that
 * exists only to fail.
 */
export function FolderActions({
  source,
  busy,
  onForget,
  testid,
}: {
  source: Source;
  /** True while any forget on this shelf is in flight — the whole group disables, because two in
   *  flight against one list is a race the shelf's own state cannot represent. */
  busy: boolean;
  /** Absent means no forget affordance at all, for a surface that only reports. */
  onForget?(source: Source): void;
  /** Prefix for the test ids, so a card and a list row are addressable apart. */
  testid: string;
}): JSX.Element | null {
  if (!canForget(source)) {
    return (
      <span
        className="shrink-0 rounded bg-muted px-1 font-mono text-[9px] uppercase tracking-wider text-muted-foreground"
        data-testid={`${testid}-discovered`}
        title="found in the default directory rather than registered — there is no registration to remove. Move the folder to un-list it."
      >
        discovered
      </span>
    );
  }
  if (!onForget) return null;
  return (
    <Button
      variant="ghost"
      size="sm"
      className="h-5 shrink-0 px-1 text-[9.5px]"
      data-testid={`${testid}-forget`}
      disabled={busy}
      title={`forget this registration — the folder at ${source.path} is not touched`}
      onClick={() => onForget(source)}
    >
      <Trash2 size={10} className="mr-0.5" />
      {busy ? '…' : 'forget'}
    </Button>
  );
}

/**
 * THE FOLDER IS GONE, and the row is still here on purpose — the registration is the operator's to
 * keep or forget. Saying so is what stops it reading like a healthy one until `serve` fails.
 */
export function FolderAbsent({ source, testid }: { source: Source; testid: string }): JSX.Element | null {
  if (!source.absent) return null;
  return (
    <span
      className="shrink-0 rounded bg-rose-500/15 px-1 font-mono text-[9px] uppercase tracking-wider text-rose-500"
      data-testid={`${testid}-absent`}
      title="the path is not on the orchestrator's disk any more — a rename, a checkout, an unmounted volume. The registration is kept; forget it or put the folder back."
    >
      absent
    </span>
  );
}
