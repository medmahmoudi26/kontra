/**
 * The Secrets section of Settings: the whole of what an operator can do to a credential here.
 *
 * WRITE-ONLY IS DRAWN, NOT ONLY DOCUMENTED. There is no field on this page that ever holds a
 * stored value, no reveal control, and no "copy" button — because there is nothing to reveal: the
 * API has no route that returns one (`backend/src/secrets/routes.ts`). What replaces reading
 * it back is the version line: writing again makes version N+1, and the page says so.
 *
 * THE FORM IS ONE FORM FOR CREATE AND ROTATE, matching the store's one verb. A separate "rotate"
 * form would be the same three fields with a different button, and the difference between them —
 * whether this name exists — is a fact the page already knows and states in the button's own label.
 *
 * PROPS IN, MARKUP OUT for everything that has a state worth pinning: {@link SecretsSurface} draws
 * whatever rows it is handed, so empty, set, rotated and revoked are four renders and no mocks.
 * The fetching lives in {@link SecretsSection}, which is the only part a node test cannot draw.
 */

import { useCallback, useEffect, useState } from 'react';
import { Trash2 } from 'lucide-react';

import {
  destroySecret,
  fetchSecrets,
  revokeSecretVersion,
  writeSecret,
  type Secret,
} from '../run/api';
import { destroyConfirm, nameHint, revocationConfirm, toRows, type SecretRow } from './secrets';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';

export interface SecretsSurfaceProps {
  /** `file`, `kms`, … — where these values rest. */
  backend: string;
  location?: string;
  rows: readonly SecretRow[];
  /** Why the list is empty, when it is empty because something failed. Never the same as "none". */
  error: string | null;
  /** What the last write did — "shodan-key is now at version 2". Never the value. */
  notice: string | null;
  /** The name of the secret a mutation is in flight for, so its buttons can say so. */
  busy?: string | null;
  onWrite(name: string, value: string, owner: string): void;
  onRevoke(row: SecretRow, version: number): void;
  onDestroy(row: SecretRow): void;
}

const VERSION_BADGE: Record<string, string> = {
  current: 'bg-emerald-500/15 text-emerald-300',
  superseded: 'bg-muted text-muted-foreground',
  revoked: 'bg-rose-500/15 text-rose-300 line-through',
};

export function SecretsSurface({
  backend,
  location,
  rows,
  error,
  notice,
  busy,
  onWrite,
  onRevoke,
  onDestroy,
}: SecretsSurfaceProps): JSX.Element {
  const [name, setName] = useState('');
  const [value, setValue] = useState('');
  const [owner, setOwner] = useState('');

  const hint = nameHint(name);
  const existing = rows.find((r) => r.name === name);
  const canWrite = Boolean(name) && !hint && Boolean(value);

  return (
    <div className="flex flex-col gap-3" data-testid="secrets-section">
      <p className="m-0 text-[11px] text-muted-foreground" data-testid="secrets-backend">
        Stored by the <strong>{backend}</strong> backend{location ? ` at ${location}` : ''}. A value
        is never returned by this console, this API, or a log line — write a new version instead of
        reading one back.
      </p>

      {error && (
        <p className="m-0 text-[11px] text-rose-300" data-testid="secrets-error">
          {error}
        </p>
      )}
      {notice && (
        <p className="m-0 text-[11px] text-emerald-300" data-testid="secrets-notice">
          {notice}
        </p>
      )}

      {rows.length === 0 ? (
        /* EMPTY IS A STATE, NOT A BLANK. An operator arriving here has to learn that the store
           exists and is working — an absent list reads as a broken page, and "no secrets" plus a
           form that visibly works is the difference. */
        <p className="m-0 text-[11px] text-muted-foreground" data-testid="secrets-empty">
          No secrets yet. Whatever you write here is named, versioned and write-only.
        </p>
      ) : (
        <ul className="m-0 flex list-none flex-col gap-2 p-0" data-testid="secrets-list">
          {rows.map((row) => (
            <li
              key={row.name}
              data-testid={`secret-${row.name}`}
              data-state={row.state}
              data-scope={row.scope}
              className="flex flex-col gap-1 rounded-md border border-border bg-background/40 p-2"
            >
              <div className="flex items-baseline justify-between gap-2">
                <span className="font-mono text-xs">{row.name}</span>
                <span className="flex items-center gap-2">
                  {row.owner ? (
                    /* WHO RESOLVES IT is the fact most worth a badge: an actor secret is fetched by
                       that actor authenticated as itself, and an operator one is resolved at the
                       last hop and can never be fetched over HTTP. */
                    <Badge variant="outline" data-testid={`secret-${row.name}-owner`}>
                      {row.owner}
                    </Badge>
                  ) : (
                    <Badge variant="outline">operator</Badge>
                  )}
                  <Button
                    size="sm"
                    variant="ghost"
                    data-testid={`secret-${row.name}-destroy`}
                    disabled={busy === row.name}
                    onClick={() => onDestroy(row)}
                    aria-label={`delete ${row.name}`}
                  >
                    <Trash2 className="h-3 w-3" />
                  </Button>
                </span>
              </div>
              <span className="text-[11px] text-muted-foreground" data-testid={`secret-${row.name}-detail`}>
                {row.detail}
              </span>
              <div className="flex flex-wrap gap-1">
                {row.versions.map((v) => (
                  <span
                    key={v.version}
                    data-testid={`secret-${row.name}-v${v.version}`}
                    data-version-state={v.state}
                    className={`flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] ${VERSION_BADGE[v.state]}`}
                  >
                    v{v.version} · {v.created}
                    {v.state !== 'revoked' && (
                      <button
                        type="button"
                        className="underline"
                        data-testid={`secret-${row.name}-v${v.version}-revoke`}
                        disabled={busy === row.name}
                        onClick={() => onRevoke(row, v.version)}
                      >
                        revoke
                      </button>
                    )}
                  </span>
                ))}
              </div>
            </li>
          ))}
        </ul>
      )}

      <form
        className="flex flex-col gap-2 border-t border-border pt-2"
        data-testid="secret-form"
        onSubmit={(e) => {
          e.preventDefault();
          if (!canWrite) return;
          onWrite(name, value, owner);
          // THE VALUE FIELD IS CLEARED THE MOMENT IT IS SUBMITTED. Leaving it filled would put a
          // credential in a DOM node, in an autofill store and in a screenshot, for a page whose
          // whole contract is that the value is not readable here.
          setValue('');
        }}
      >
        <div className="flex flex-wrap items-center gap-2">
          <Input
            className="h-7 w-48 text-xs"
            placeholder="name (e.g. shodan-key)"
            data-testid="secret-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <Input
            className="h-7 w-64 text-xs"
            /* A password field, and `autoComplete="off"`: the browser must not offer to remember
               it, and a shoulder over the keyboard must not read it. */
            type="password"
            autoComplete="off"
            placeholder="value — write-only"
            data-testid="secret-value"
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
          <Input
            className="h-7 w-40 text-xs"
            placeholder="owner actor (optional)"
            data-testid="secret-owner"
            value={owner}
            disabled={Boolean(existing)}
            onChange={(e) => setOwner(e.target.value)}
          />
          <Button size="sm" type="submit" data-testid="secret-write" disabled={!canWrite}>
            {existing ? `rotate to v${(existing.versions[0]?.version ?? 0) + 1}` : 'create'}
          </Button>
        </div>
        {hint && (
          <p className="m-0 text-[11px] text-amber-300" data-testid="secret-name-hint">
            {hint}
          </p>
        )}
        {existing && (
          <p className="m-0 text-[11px] text-muted-foreground" data-testid="secret-rotate-note">
            {`"${name}" exists — writing adds a version. The one before it keeps working until you revoke it, which is what makes a rotation safe.`}
          </p>
        )}
      </form>
    </div>
  );
}

/** What the section holds, and the three things it can do. Its own hook so the surface stays pure. */
export function useSecrets(): SecretsSurfaceProps {
  const [secrets, setSecrets] = useState<Secret[]>([]);
  const [backend, setBackend] = useState('');
  const [location, setLocation] = useState<string | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const reload = useCallback(() => {
    void fetchSecrets()
      .then((got) => {
        // A 200 that is not this shape is treated as an empty store rather than trusted — the same
        // guard `useRegisteredFolders` takes, and for the same reason: `undefined.map` would take
        // the whole Settings page down over a section of it.
        setBackend(typeof got.backend === 'string' ? got.backend : 'unknown');
        setLocation(typeof got.location === 'string' ? got.location : undefined);
        setSecrets(Array.isArray(got.secrets) ? got.secrets : []);
        setError(null);
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)));
  }, []);

  useEffect(reload, [reload]);

  const onWrite = useCallback(
    (name: string, value: string, owner: string) => {
      setBusy(name);
      void writeSecret(name, value, owner || undefined)
        .then((written) => {
          // The confirmation an operator gets INSTEAD of reading the value back.
          setNotice(
            `${name} is now at version ${written.version}${written.rotated ? ' — the version before it still works until you revoke it' : ''}${
              written.trimmed ? '. Surrounding whitespace was trimmed.' : ''
            }`
          );
          setError(null);
          reload();
        })
        .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
        .finally(() => setBusy(null));
    },
    [reload]
  );

  const onRevoke = useCallback(
    (row: SecretRow, version: number) => {
      const live = row.versions.filter((v) => v.state !== 'revoked' && v.version !== version);
      const fallback = live.length ? Math.max(...live.map((v) => v.version)) : undefined;
      if (!window.confirm(revocationConfirm({ name: row.name, version, fallback }))) return;
      setBusy(row.name);
      void revokeSecretVersion(row.name, version)
        .then(() => {
          setNotice(`version ${version} of ${row.name} is revoked — its stored value is gone`);
          setError(null);
          reload();
        })
        .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
        .finally(() => setBusy(null));
    },
    [reload]
  );

  const onDestroy = useCallback(
    (row: SecretRow) => {
      if (!window.confirm(destroyConfirm({ name: row.name, versions: row.versions.length, owner: row.owner })))
        return;
      setBusy(row.name);
      void destroySecret(row.name)
        .then(() => {
          setNotice(`${row.name} is gone`);
          setError(null);
          reload();
        })
        .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
        .finally(() => setBusy(null));
    },
    [reload]
  );

  return { backend, location, rows: toRows(secrets), error, notice, busy, onWrite, onRevoke, onDestroy };
}

export default function SecretsSection(): JSX.Element {
  return <SecretsSurface {...useSecrets()} />;
}
