/**
 * kontra for VS Code — a thin shell around the orchestrator's `/dev` route.
 *
 * IT RENDERS NOTHING. No form, no schema derivation, no runner. Windmill's extension takes exactly
 * this approach and it is the reason theirs is a few hundred lines: everything hard lives in the
 * web app, so the pane cannot drift from the console and cannot become a second-class copy of it.
 *
 * What this file does, in full:
 *   1. find the actor directory for the open file (the nearest `actor.json`);
 *   2. open a webview holding an <iframe> at `<orchestrator>/dev?actor=…&method=…&theme=…&token=…`;
 *   3. keep a stored credential out of settings and in VS Code's secret storage.
 *
 * NOTHING IS UPLOADED AND NOTHING IS BUILT. The orchestrator already reaches the checkout, and a
 * locally served actor runs `python <dir>/actor.py` straight from the directory. What makes a saved
 * line testable in seconds is `kontra serve --watch`, not this extension — which is why the pane
 * says so when nothing is serving, rather than letting a run fail with "no worker is serving this
 * folder's code" from inside an editor.
 */

import * as path from 'path';
import * as vscode from 'vscode';

import { actorDirFor, isInside, readActorKey } from './locate';
import { credentialPrefill } from './credential';

const SECRET_KEY = 'kontra.sessionToken';

function orchestratorUrl(): string {
  const raw = vscode.workspace.getConfiguration('kontra').get<string>('orchestratorUrl') ?? '';
  return raw.replace(/\/+$/, '') || 'http://127.0.0.1:8088';
}

/** dark|light, as the pane's `theme` parameter understands it. */
function editorTheme(): 'dark' | 'light' {
  const kind = vscode.window.activeColorTheme.kind;
  return kind === vscode.ColorThemeKind.Light || kind === vscode.ColorThemeKind.HighContrastLight
    ? 'light'
    : 'dark';
}

/**
 * Is a STORED token still a session this orchestrator will accept?
 *
 * WHY ASK AT ALL. The token lives in the OS keychain and outlives the control plane that minted it
 * — sessions are in-process, so `docker compose down` (or any restart) invalidates every one of
 * them while the keychain copy looks perfectly good. Handing that to the pane does not fail
 * loudly: the console adopts it, the first `/api/…` call 401s, the session clears and the gate
 * draws a PASSWORD FORM inside the iframe. That form is the worst possible place to land, because
 * a cross-origin frame in an editor webview gets neither the paste keystroke nor a working
 * right-click Paste — so the credential that would get past it cannot be entered.
 *
 * 401 IS THE ONLY VERDICT THAT COUNTS. `/api/state/actors` is bearer-gated and cheap; a valid
 * session answers 400 there (it wants parameters) and that is a PASS — the question is whether the
 * credential was accepted, not whether the call succeeded. An orchestrator that cannot be reached
 * is not a rejection either: re-prompting for a password because the network blinked would be a
 * worse answer than letting the pane report the real failure.
 */
async function tokenAccepted(base: string, token: string): Promise<boolean> {
  try {
    const res = await fetch(`${base}/api/state/actors`, {
      headers: { authorization: `Bearer ${token}` },
    });
    return res.status !== 401;
  } catch {
    return true;
  }
}

/**
 * THE LOG, because until now this extension was silent and silence is indistinguishable from
 * working.
 *
 * Every interesting thing here happens somewhere the operator cannot see: a token read from the
 * keychain, a schema derived in a container, a message posted into a cross-origin frame. When one
 * of those did nothing, the pane simply did not change — no error, no clue, nothing to attach a
 * question to. "I edited the code and nothing happened" is not a bug report anybody can act on,
 * and it was the only one this extension made possible.
 *
 * An OutputChannel rather than console.log: `console.log` in an extension host goes to a developer
 * window the operator does not have open, while this is `kontra: Show logs` and a panel they can
 * read and paste back. Timestamps because the question is almost always ORDERING — did the save
 * fire before the pane was ready?
 */
const out = vscode.window.createOutputChannel('kontra');

function log(line: string): void {
  // Local time, seconds resolution: this is read next to a file save, not correlated across hosts.
  const t = new Date().toTimeString().slice(0, 8);
  out.appendLine(`${t}  ${line}`);
}

/** The clipboard, or '' — a clipboard that refuses must not take the sign-in down with it. */
async function readClipboard(): Promise<string> {
  try {
    return await vscode.env.clipboard.readText();
  } catch {
    return '';
  }
}

/**
 * Sign in against the orchestrator and keep the session in SECRET STORAGE.
 *
 * Never in settings and never in the workspace: a workspace file is committed by somebody
 * eventually, and `settings.json` is synced. VS Code's secret storage is the OS keychain.
 */
async function connect(ctx: vscode.ExtensionContext): Promise<string | undefined> {
  const base = orchestratorUrl();
  const user = await vscode.window.showInputBox({ prompt: `kontra user at ${base}`, value: 'admin' });
  if (!user) return undefined;

  // PRE-FILLED FROM THE CLIPBOARD, because the paste keystroke is not ours to fix — see
  // `credential.ts`. `vscode.env.clipboard` is the editor's own API and needs no permission, so
  // this works wherever the extension does, Cursor included.
  const prefill = credentialPrefill(await readClipboard());
  const password = await vscode.window.showInputBox({
    prompt: prefill ? 'password — prefilled from the clipboard' : 'password',
    password: true,
    value: prefill,
  });
  if (!password) return undefined;

  try {
    const res = await fetch(`${base}/api/login`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ user, password }),
    });
    const body = (await res.json().catch(() => ({}))) as { token?: string; error?: string };
    if (!res.ok || !body.token) {
      // THE SERVER'S OWN SENTENCE. A 503 says the install has no console user and names the command
      // that creates one; replacing it with "login failed" throws away the only useful part.
      vscode.window.showErrorMessage(`kontra: ${body.error ?? `${res.status} ${res.statusText}`}`);
      return undefined;
    }
    await ctx.secrets.store(SECRET_KEY, body.token);
    return body.token;
  } catch (err) {
    vscode.window.showErrorMessage(
      `kontra: could not reach ${base} — ${err instanceof Error ? err.message : String(err)}`
    );
    return undefined;
  }
}

let panel: vscode.WebviewPanel | undefined;
/** When set, the pane stays on this file however the editor moves. */
let pinned: string | undefined;
/** The actor FOLDER the open pane is showing — a save outside it is not this pane's business. */
let paneDir: string | undefined;

/**
 * The webview's HTML: an iframe and nothing else.
 *
 * The token rides in the query string because that is the only channel an iframe has before its
 * first paint — and `DevPane` consumes it and removes it from the address on the first render, so
 * it does not survive in history or referrers.
 */
function html(src: string): string {
  // THE RELAY IS THE POINT OF THE SCRIPT, and it is three lines because it must be.
  //
  // ⌘V never reaches the inner document: the editor takes the keystroke, and a webview's context
  // menu is the editor's rather than the browser's, so its Paste acts on the editor. The keybinding
  // in package.json sends `kontra.paste` here instead; this forwards the text to the frame, and
  // `hostPaste.ts` on the other side writes it into whatever is focused.
  //
  // `${new URL(src).origin}` rather than '*': the text is a clipboard, so it goes to the one origin
  // we framed and to nothing that may have navigated it since.
  const origin = new URL(src).origin;
  return `<!doctype html>
<html><head><meta charset="utf-8">
<style>html,body,iframe{margin:0;padding:0;border:0;width:100%;height:100vh;display:block}</style>
</head><body><iframe id="f" src="${src}" allow="clipboard-read; clipboard-write"></iframe>
<script>
  // QUEUED UNTIL THE FRAME HAS LOADED. postMessage to a contentWindow that has not finished
  // navigating is DROPPED — silently, with no error anywhere — so a save in the first second after
  // the pane opens would do nothing and look exactly like the bug this whole path exists to fix.
  // The 'load' event is the frame's own readiness, which is the only signal available across an
  // origin boundary without the page agreeing to a handshake.
  var vscodeApi = acquireVsCodeApi();
  var frame = document.getElementById('f');
  var ready = false, queued = [];
  var send = function (m) { frame.contentWindow.postMessage(m, ${JSON.stringify(origin)}); };
  // QUEUED UNTIL THE PAGE SAYS IT IS LISTENING — not until the frame fires 'load'.
  //
  // 'load' reports that a DOCUMENT arrived. It says nothing about whether the app attached its
  // message listener, which happens in a React effect after commit. And the pane sits behind a
  // login gate: while that gate draws a password form the pane never mounts, so no listener exists
  // and anything sent is dropped silently, forever. A queue keyed on 'load' cannot see either case.
  // 'kontra.ready' is posted by the page itself, after the listener is attached.
  window.addEventListener('message', function (e) {
    if (!e.data) return;
    if (e.data.type === 'kontra.ready') {
      ready = true;
      queued.splice(0).forEach(send);
      return;
    }
    if (e.data.type === 'kontra.copied') { vscodeApi.postMessage(e.data); return; }
    if (e.data.type !== 'kontra.paste' && e.data.type !== 'kontra.reload' && e.data.type !== 'kontra.copy') return;
    if (ready) send(e.data); else queued.push(e.data);
  });
</script>
</body></html>`;
}

/** The actor under the cursor and the URL that runs it — shared so the pane and the browser
 *  command can never disagree about which folder or which token. */
async function runnerTarget(
  ctx: vscode.ExtensionContext
): Promise<{ key: string; dir: string; url: URL } | undefined> {
  const file = pinned ?? vscode.window.activeTextEditor?.document.uri.fsPath;
  if (!file) {
    vscode.window.showWarningMessage('kontra: open an actor file first.');
    return;
  }
  const dir = actorDirFor(file);
  if (!dir) {
    // A CLEAR MESSAGE, not an empty frame. The common case is a file that simply is not part of an
    // actor, and saying which directory was searched is what makes that obvious.
    vscode.window.showWarningMessage(
      `kontra: no actor.json above ${path.basename(file)} — this file is not part of an actor.`
    );
    return;
  }
  const key = readActorKey(dir);
  if (!key) {
    vscode.window.showWarningMessage(`kontra: ${path.join(dir, 'actor.json')} has no name.`);
    return;
  }

  let token = await ctx.secrets.get(SECRET_KEY);
  // A KEYCHAIN ENTRY IS NOT A LIVE SESSION — see `tokenAccepted`. Checked before the pane is
  // handed it, because the pane's way of reporting a dead token is a login form nobody can paste
  // into.
  if (token && !(await tokenAccepted(orchestratorUrl(), token))) {
    log('the stored token was refused — signing in again');
    await ctx.secrets.delete(SECRET_KEY);
    token = undefined;
  }
  if (!token) token = await connect(ctx);
  if (!token) return;

  const url = new URL(`${orchestratorUrl()}/dev`);
  url.searchParams.set('actor', key);
  // THE FOLDER, because the pane would otherwise guess. It joins actor→folder by name+version, and
  // `workspaces.kontra` normally holds the same actor in several workspaces — so the guess picks
  // whichever registered first, which is how an edit in one workspace showed no change in the pane.
  url.searchParams.set('dir', dir);
  url.searchParams.set('theme', editorTheme());
  url.searchParams.set('token', token);
  return { key, dir, url };
}

async function openRunner(ctx: vscode.ExtensionContext): Promise<void> {
  const target = await runnerTarget(ctx);
  if (!target) return;
  const { key, dir, url } = target;

  if (!panel) {
    panel = vscode.window.createWebviewPanel('kontra.runner', 'kontra', vscode.ViewColumn.Beside, {
      enableScripts: true,
      retainContextWhenHidden: true,
    });
    panel.onDidDispose(() => (panel = undefined));
    panel.webview.onDidReceiveMessage((m: { type?: string; text?: string }) => {
      if (m?.type !== 'kontra.copied' || typeof m.text !== 'string') return;
      log(`copy: ${m.text.length} character(s) taken from the pane`);
      void vscode.env.clipboard.writeText(m.text);
    });
  }
  // The pane is showing THIS folder now, so a save inside it is the one that matters (`onSaved`).
  paneDir = dir;
  log(`pane → ${key} from ${dir}`);
  panel.title = `kontra — ${key}`;
  panel.webview.html = html(url.toString());
  panel.reveal(vscode.ViewColumn.Beside, true);
}

/**
 * Deliver the clipboard into the pane, because the editor will not.
 *
 * Bound to ⌘V/Ctrl+V in package.json, scoped by `when: activeWebviewPanelId == 'kontra.runner'` so
 * it only fires while THIS pane has focus — every other ⌘V in the editor is untouched. The
 * clipboard is read here, with the editor's own API and no browser permission, and the webview
 * relays it to the frame (see `html`).
 */
/**
 * Copy the pane's selection — the mirror of `pasteIntoPane`, and needed for the same reason.
 *
 * The extension cannot read a cross-origin frame's selection, so it ASKS and the page answers on
 * the message channel (`hostBridge.ts:listenForHostCopy`). The answer is written with the editor's
 * own clipboard API, which needs no browser permission.
 */
async function copyFromPane(): Promise<void> {
  if (!panel) return log('copy: no pane open');
  log('copy: asking the pane for its selection');
  await panel.webview.postMessage({ type: 'kontra.copy' });
}

async function pasteIntoPane(): Promise<void> {
  if (!panel) return log('paste: no pane open');
  const text = await readClipboard();
  if (!text) return log('paste: the clipboard is empty');
  log(`paste: ${text.length} character(s) posted to the pane`);
  await panel.webview.postMessage({ type: 'kontra.paste', text });
}

/**
 * Tell the pane the file changed, the moment it is saved.
 *
 * THIS IS THE WHOLE "INSTANT" — the pane's form is derived from the code on disk, so it is correct
 * as soon as something re-reads it, and the editor is the one party that knows exactly when to. No
 * worker restart is involved: a schema comes from `internals.schemadump` reading the files, not
 * from a booted worker's catalog entry. (Actually RUNNING the new code still needs a serve — the
 * worker holds what it imported at boot. The form tracking the file and the worker tracking the
 * file are two different promises, and only the first one is made here.)
 *
 * SCOPED TO THE PANE'S OWN FOLDER, because an editor saves constantly and every unrelated save
 * would otherwise spend a schema derivation — which shells out to Python.
 */
function onSaved(doc: vscode.TextDocument): void {
  const file = doc.uri.fsPath;
  if (!panel) return log(`saved ${path.basename(file)} — no pane open, nothing to refresh`);
  if (!paneDir) return log(`saved ${path.basename(file)} — the pane has no folder yet`);
  if (!isInside(file, paneDir)) {
    return log(`saved ${path.basename(file)} — outside ${paneDir}, not this pane's actor`);
  }
  log(`saved ${path.basename(file)} → reload posted to the pane`);
  void panel.webview.postMessage({ type: 'kontra.reload' });
}

/**
 * Open the runner in the operator's real browser.
 *
 * THE ESCAPE HATCH, and it is not a consolation prize. The pane is a cross-origin iframe nested in
 * an editor webview, and that nesting is what breaks the clipboard — the keystroke may never reach
 * the editor's keybinding layer, and `clipboard-read` is not delegated to a nested frame. In an
 * ordinary browser tab none of that is true: ⌘C, ⌘V, right-click and devtools all just work,
 * because it is just a page.
 *
 * It is the SAME URL the pane frames, built by the same code, so the two cannot drift — and the
 * token still rides in the query string and is stripped on first render.
 */
async function openInBrowser(ctx: vscode.ExtensionContext): Promise<void> {
  const target = await runnerTarget(ctx);
  if (!target) return;
  log(`opening ${target.key} in the browser`);
  await vscode.env.openExternal(vscode.Uri.parse(target.url.toString()));
}

export function activate(ctx: vscode.ExtensionContext): void {
  log('kontra activated');
  ctx.subscriptions.push(
    out,
    vscode.commands.registerCommand('kontra.showLogs', () => out.show(true)),
    vscode.commands.registerCommand('kontra.openRunner', () => void openRunner(ctx)),
    vscode.commands.registerCommand('kontra.paste', () => void pasteIntoPane()),
    vscode.commands.registerCommand('kontra.copy', () => void copyFromPane()),
    vscode.commands.registerCommand('kontra.openInBrowser', () => void openInBrowser(ctx)),
    vscode.workspace.onDidSaveTextDocument(onSaved),
    vscode.commands.registerCommand('kontra.connect', async () => {
      await ctx.secrets.delete(SECRET_KEY);
      if (await connect(ctx)) vscode.window.showInformationMessage('kontra: connected.');
    }),
    vscode.commands.registerCommand('kontra.signOut', async () => {
      await ctx.secrets.delete(SECRET_KEY);
      vscode.window.showInformationMessage('kontra: credential forgotten.');
    }),
    vscode.commands.registerCommand('kontra.pin', () => {
      pinned = vscode.window.activeTextEditor?.document.uri.fsPath;
      vscode.window.showInformationMessage(
        pinned ? `kontra: pinned to ${path.basename(pinned)}` : 'kontra: nothing to pin.'
      );
    }),
    vscode.commands.registerCommand('kontra.unpin', () => {
      pinned = undefined;
      vscode.window.showInformationMessage('kontra: unpinned — the pane follows the editor again.');
    }),
    // FOLLOW THE EDITOR, unless pinned. Pinning is the difference between a demo and something used
    // all day: you navigate away to read a helper, and the pane you were running from survives.
    vscode.window.onDidChangeActiveTextEditor((editor) => {
      if (!panel || pinned || !editor) return;
      if (actorDirFor(editor.document.uri.fsPath)) void openRunner(ctx);
    })
  );
}

export function deactivate(): void {
  panel?.dispose();
}
