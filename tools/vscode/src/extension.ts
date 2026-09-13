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

import { actorDirFor, readActorKey } from './locate';

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
 * Sign in against the orchestrator and keep the session in SECRET STORAGE.
 *
 * Never in settings and never in the workspace: a workspace file is committed by somebody
 * eventually, and `settings.json` is synced. VS Code's secret storage is the OS keychain.
 */
async function connect(ctx: vscode.ExtensionContext): Promise<string | undefined> {
  const base = orchestratorUrl();
  const user = await vscode.window.showInputBox({ prompt: `kontra user at ${base}`, value: 'admin' });
  if (!user) return undefined;
  const password = await vscode.window.showInputBox({ prompt: 'password', password: true });
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

/**
 * The webview's HTML: an iframe and nothing else.
 *
 * The token rides in the query string because that is the only channel an iframe has before its
 * first paint — and `DevPane` consumes it and removes it from the address on the first render, so
 * it does not survive in history or referrers.
 */
function html(src: string): string {
  return `<!doctype html>
<html><head><meta charset="utf-8">
<style>html,body,iframe{margin:0;padding:0;border:0;width:100%;height:100vh;display:block}</style>
</head><body><iframe src="${src}" allow="clipboard-read; clipboard-write"></iframe></body></html>`;
}

async function openRunner(ctx: vscode.ExtensionContext): Promise<void> {
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
  if (!token) token = await connect(ctx);
  if (!token) return;

  const url = new URL(`${orchestratorUrl()}/dev`);
  url.searchParams.set('actor', key);
  url.searchParams.set('theme', editorTheme());
  url.searchParams.set('token', token);

  if (!panel) {
    panel = vscode.window.createWebviewPanel('kontra.runner', 'kontra', vscode.ViewColumn.Beside, {
      enableScripts: true,
      retainContextWhenHidden: true,
    });
    panel.onDidDispose(() => (panel = undefined));
  }
  panel.title = `kontra — ${key}`;
  panel.webview.html = html(url.toString());
  panel.reveal(vscode.ViewColumn.Beside, true);
}

export function activate(ctx: vscode.ExtensionContext): void {
  ctx.subscriptions.push(
    vscode.commands.registerCommand('kontra.openRunner', () => void openRunner(ctx)),
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
