# kontra for VS Code

Run an actor's Method from your editor, against the code on disk.

    kontra: Run this actor          opens the runner beside the file
    kontra: Pin / Unpin             hold the pane on one file while you navigate
    kontra: Connect                 sign in; the session goes to the OS keychain
    kontra: Forget the credential

## What it is, and what it deliberately is not

It renders **nothing**. The pane is an `<iframe>` at `<orchestrator>/dev`, which is the console's own
`MethodCall` with the chrome removed. There is no form, no schema derivation and no runner in this
extension — all three live in the web app, so the pane cannot drift from the console.

**Nothing is uploaded and nothing is built.** The orchestrator already reaches your checkout, and a
locally served actor runs `python <dir>/actor.py` straight from the directory.

## For a saved line to be testable in seconds

    kontra serve --actor ./myactor --watch

`--watch` re-execs the Worker on save. Without it this pane is a viewer attached to whatever was
last served — the queue does not move across an edit, so a run just executes the older code.

To stop **inside** a Method, see `docs/debugging.md`: a `debugpy` shim behind `KONTRA_PYTHON`, an
attach configuration, and the two-minute heartbeat you will need to raise.

## Install

    npm install && npm run compile

then load the folder as an extension development host (F5), or package it with `vsce`.

The orchestrator address is `kontra.orchestratorUrl` (default `http://127.0.0.1:8088`). The session
token is kept in VS Code's secret storage — never in settings, which sync, and never in the
workspace, which gets committed.
