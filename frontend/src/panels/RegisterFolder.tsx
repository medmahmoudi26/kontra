/**
 * Register a folder — the form both the Actors and the Workflows pages use.
 *
 * ONE FIELD, AND IT IS A PATH. Registration records where your code lives; there is nothing to
 * upload and nothing to name, because the folder already says what it is (`actor.json` gives an
 * Actor its name and version, the directory name gives a Workflow its own).
 *
 * NO FILE PICKER, and that is not a shortcut. A browser's directory picker reads the CLIENT's
 * filesystem, and the folder being registered has to exist on the machine the ORCHESTRATOR runs
 * on — usually the same box, but not when the console is open against a controller over the VPC.
 * A picker would silently register a path that resolves to something else there, or to nothing.
 * A typed path is the honest input, and the server answers with the actual failure: the directory
 * that is not there, or the marker file that is missing.
 *
 * THE DEFAULT ROOT IS PREFILLED because a path field with no default is a guess about a
 * convention the operator has not read yet.
 */

import { useState, type FormEvent } from 'react';
import { FolderPlus } from 'lucide-react';
import { registerSource, type Source, type SourceKind } from '../run/api';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';

export function RegisterFolder({
  kind,
  defaultRoot,
  onRegistered,
}: {
  kind: SourceKind;
  /** `~/.kontra/actors` or `~/.kontra/workflows`, from the server. */
  defaultRoot: string;
  onRegistered(source: Source): void;
}): JSX.Element {
  const [open, setOpen] = useState(false);
  const [path, setPath] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const marker = kind === 'actor' ? 'actor.json' : 'workflow.py';

  async function submit(e: FormEvent): Promise<void> {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const source = await registerSource(kind, path.trim());
      onRegistered(source);
      setPath('');
      setOpen(false);
    } catch (err) {
      // The server's message names the path and the file it wanted. Showing it verbatim is the
      // entire content of the answer — a generic "registration failed" would send the operator
      // back to guess which of the two is wrong.
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  if (!open) {
    return (
      <Button
        size="sm"
        variant="ghost"
        className="h-6 px-1.5 text-[10px]"
        data-testid={`register-${kind}-open`}
        onClick={() => {
          setOpen(true);
          // An EMPTY root prefills nothing, not `/`. The root arrives from the server and the form
          // can be opened before that answer lands; `${''}/` put a lone slash in the field, which
          // registers as "/ has no actor.json" — a refusal about a path the operator never typed.
          setPath(defaultRoot ? `${defaultRoot}/` : '');
        }}
      >
        <FolderPlus size={11} className="mr-1" />
        register
      </Button>
    );
  }

  return (
    <form className="flex flex-col gap-1" onSubmit={submit} data-testid={`register-${kind}-form`}>
      <div className="flex items-center gap-1">
        <Input
          autoFocus
          className="h-6 flex-1 font-mono text-[10.5px]"
          value={path}
          onChange={(e) => setPath(e.target.value)}
          placeholder={`${defaultRoot}/my-${kind}`}
          spellCheck={false}
          data-testid={`register-${kind}-path`}
          onKeyDown={(e) => {
            if (e.key === 'Escape') setOpen(false);
          }}
        />
        <Button
          type="submit"
          size="sm"
          className="h-6 px-2 text-[10px]"
          disabled={busy || path.trim() === ''}
          data-testid={`register-${kind}-submit`}
        >
          {busy ? '…' : 'add'}
        </Button>
      </div>
      {error ? (
        <p className="m-0 px-0.5 text-[10px] text-destructive" data-testid={`register-${kind}-error`}>
          {error}
        </p>
      ) : (
        <p className="m-0 px-0.5 text-[9.5px] text-muted-foreground">
          a folder on the orchestrator's disk holding {marker}
        </p>
      )}
    </form>
  );
}
