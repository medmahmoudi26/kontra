import { useEffect, useRef } from 'react';
import { RegisterFolder } from '@kontra/frontend';

/**
 * `POST /api/sources/:kind` — the ONE route this form talks to, answered here so the refusal cell
 * shows the component's real error state instead of an unreachable orchestrator's.
 *
 * It refuses everything, and only one cell below ever submits. The body is the envelope every
 * refusing route on this API answers in (`{"error": …}`), because unwrapping it is `reason()`'s job
 * and the point of the cell is what an operator ends up reading.
 */
const realFetch = globalThis.fetch.bind(globalThis);
globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  const method = (init?.method ?? 'GET').toUpperCase();
  if (method === 'POST' && /\/sources\/(actor|workflow)$/.test(url)) {
    let path = '';
    try {
      path = (JSON.parse(String(init?.body ?? '{}')) as { path?: string }).path ?? '';
    } catch {
      /* the form always sends JSON */
    }
    const marker = url.endsWith('/workflow') ? 'workflow.py' : 'actor.json';
    return Promise.resolve(
      new Response(
        JSON.stringify({
          error: `${path} has no ${marker} — that is what makes a folder ${
            marker === 'actor.json' ? 'an Actor' : 'a Workflow'
          }`,
        }),
        { status: 400, statusText: 'Bad Request', headers: { 'content-type': 'application/json' } }
      )
    );
  }
  return realFetch(input, init);
}) as typeof fetch;

function Frame({ children, max }: { children: React.ReactNode; max?: number }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground" style={{ maxWidth: max }}>
        {children}
      </div>
    </div>
  );
}

/** The 340px box the Actors header reserves for this form — its real width on both pages. */
function InHeaderSlot({ children }: { children: React.ReactNode }) {
  return (
    <Frame max={372}>
      <div className="w-[340px]">{children}</div>
    </Frame>
  );
}

/**
 * The form's open/typed/submitted states live in its own `useState` and there is no prop that
 * reaches them — the operator opens it, types, and presses add. This drives the REAL affordances in
 * order (click `register`, type into the path field, submit the form) so a card can show those
 * states without a second implementation of the form standing in for them.
 *
 * Scoped to this cell's own subtree, never `document`: the product renders every cell of a card on
 * one page, and a document-wide query would drive somebody else's form.
 */
function Compose({
  kind,
  path,
  submit,
  children,
}: {
  kind: 'actor' | 'workflow';
  /** What to type into the path field once it exists. Absent leaves the prefilled default root. */
  path?: string;
  submit?: boolean;
  children: React.ReactNode;
}) {
  const host = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const root = host.current;
    if (!root) return;
    let step = 0;
    const timer = setInterval(() => {
      if (step === 0) {
        const open = root.querySelector<HTMLElement>(`[data-testid="register-${kind}-open"]`);
        if (open) {
          open.click();
          step = 1;
        }
        return;
      }
      if (step === 1) {
        const field = root.querySelector<HTMLInputElement>(`[data-testid="register-${kind}-path"]`);
        if (!field) return;
        if (path !== undefined) {
          // React owns `value`, so the native setter plus an `input` event is what a keystroke
          // looks like to it — assigning `.value` alone is a change React never hears about.
          Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set?.call(field, path);
          field.dispatchEvent(new Event('input', { bubbles: true }));
        }
        step = 2;
        return;
      }
      if (submit) {
        root.querySelector<HTMLFormElement>(`[data-testid="register-${kind}-form"]`)?.requestSubmit();
      }
      clearInterval(timer);
    }, 20);
    return () => clearInterval(timer);
  }, [kind, path, submit]);
  return <div ref={host}>{children}</div>;
}

const noop = () => {};

/**
 * Closed, which is how both pages rest — a header action beside the refresh, not a control buried in
 * the list below. On an installation with nothing registered that list is one line of prose, and a
 * button inside it would be the only thing on the page to press.
 */
export function Closed() {
  return (
    <Frame>
      <header className="flex flex-wrap items-baseline gap-3">
        <h1 className="m-0 text-lg font-semibold">Actors</h1>
        <span className="text-[11.5px] text-muted-foreground">
          Deployed workers, and the folders their code lives in.
        </span>
        <div className="ml-auto w-[340px] shrink-0 text-right">
          <RegisterFolder kind="actor" defaultRoot="/home/med/.kontra/actors" onRegistered={noop} />
        </div>
      </header>
    </Frame>
  );
}

/**
 * Open, with the default root PREFILLED and the trailing slash already there. A path field with no
 * default is a guess about a convention the operator has not read yet — and the root comes from the
 * SERVER, because `KONTRA_HOME` moves it and the browser cannot see that.
 *
 * One field, and it is a path: there is nothing to upload and nothing to name, because `actor.json`
 * already gives the Actor its name and version.
 */
export function OpenWithTheDefaultRoot() {
  return (
    <InHeaderSlot>
      <Compose kind="actor">
        <RegisterFolder kind="actor" defaultRoot="/home/med/.kontra/actors" onRegistered={noop} />
      </Compose>
    </InHeaderSlot>
  );
}

/**
 * A real checkout typed over the default — the common case, because code being registered usually
 * lives where it was cloned rather than under `~/.kontra`. There is deliberately NO directory
 * picker: a browser's picker reads the CLIENT's filesystem, and the folder has to exist on the
 * machine the ORCHESTRATOR runs on, which is not the same box when the console is open against a
 * controller over the VPC.
 */
export function APathTyped() {
  return (
    <InHeaderSlot>
      <Compose kind="actor" path="/srv/checkout/examples/python/probe">
        <RegisterFolder kind="actor" defaultRoot="/home/med/.kontra/actors" onRegistered={noop} />
      </Compose>
    </InHeaderSlot>
  );
}

/**
 * The server refused, and its sentence is the whole content of the answer: it names the path AND the
 * file it wanted. "Registration failed" would send the operator back to guess which of the two is
 * wrong — here the typo is visible in the message and the field still holds what they typed.
 */
export function TheServerRefused() {
  return (
    <InHeaderSlot>
      <Compose kind="actor" path="/srv/checkout/examples/python/prob" submit>
        <RegisterFolder kind="actor" defaultRoot="/home/med/.kontra/actors" onRegistered={noop} />
      </Compose>
    </InHeaderSlot>
  );
}

/**
 * One form, two pages. Only the marker file differs — `workflow.py` instead of `actor.json`, in both
 * the hint and the placeholder — which is exactly why there is one implementation: two would have
 * drifted on the one string that distinguishes them.
 */
export function ForAWorkflowFolder() {
  return (
    <InHeaderSlot>
      <Compose kind="workflow" path="/home/med/.kontra/workflows/nscheck">
        <RegisterFolder
          kind="workflow"
          defaultRoot="/home/med/.kontra/workflows"
          onRegistered={noop}
        />
      </Compose>
    </InHeaderSlot>
  );
}

/**
 * Opened before the server's answer about where the default root is had landed. An empty root
 * prefills NOTHING, not `/` — `${''}/` put a lone slash in the field, which registers as "/ has no
 * actor.json": a refusal about a path the operator never typed.
 */
export function BeforeTheRootIsKnown() {
  return (
    <InHeaderSlot>
      <Compose kind="actor">
        <RegisterFolder kind="actor" defaultRoot="" onRegistered={noop} />
      </Compose>
    </InHeaderSlot>
  );
}
