# Debugging a kontra actor or workflow

kontra ships no debugger, deliberately. An ordinary one already attaches — what was missing was
somewhere that says how, and a way to survive the activity heartbeat while you think.

There are two situations and they need different tools.

| you want to stop inside… | use | time limit |
|---|---|---|
| an **actor Method** (an activity) | `debugpy` / `dlv`, attached to a locally served worker | the heartbeat — see below |
| a **caller workflow** | `kontra workflow replay` | **none** |

## A Method, live

`kontra serve --actor <dir> --mode local` builds nothing: it runs `python <dir>/actor.py` straight
from the directory. The interpreter is fully overridable and **not validated**, so a shim is all it
takes.

```sh
#!/bin/sh
# ~/bin/kontra-debug-python   (chmod +x)
exec /path/to/.venv/bin/python -m debugpy --listen 127.0.0.1:5678 "$@"
```

```sh
KONTRA_PYTHON=~/bin/kontra-debug-python kontra serve --actor ./myactor --watch
```

Then **attach** — `"request": "attach"`, never `"launch"`:

```jsonc
// .vscode/launch.json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Attach to a served kontra actor",
      "type": "debugpy",
      "request": "attach",
      "connect": { "host": "127.0.0.1", "port": 5678 },
      // No pathMappings: --mode local runs the code from this directory, which is the point.
      "justMyCode": false
    }
  ]
}
```

A `launch` configuration starts a **second, unrelated** interpreter. kontra spawns the one you care
about, so its breakpoints never bind and the pane sits there looking broken.

Add `--wait-for-client` to the shim only if you need to stop during the actor's import; without it
the Worker registers immediately and you attach whenever you like.

### The two-minute ceiling, and how to lift it

`runtime/handler/workflow.go` sets, on the activity that runs a Method:

```go
StartToCloseTimeout: time.Hour,          // generous — not what catches a wedge
HeartbeatTimeout:    2 * time.Minute,    // "silence for two minutes means stuck, not busy"
RetryPolicy: { MaximumAttempts: 10, InitialInterval: time.Second }
```

The actor heartbeats **once per committed Unit**. A breakpoint inside a Unit emits nothing, so:

- at two minutes Temporal declares the attempt dead and **retries it**;
- the retry lands on the same sessions queue, polled by the same Worker, so **a second invocation of
  your Method starts while the first is still frozen at your breakpoint** — up to ten times;
- with a single-Unit Batch (what a one-off probe dispatches) there is no commit to beat on at all,
  so the full two minutes elapse with zero heartbeats.

That default is right for production. For a debugging session, a dispatch can ask for more:
`EntryInput.debug_heartbeat_seconds`.

It rides the **dispatch input**, never an environment variable, and that is not a preference.
`HeartbeatTimeout` is set in *workflow* code, which must replay identically — the same file already
says values are "derived from this workflow's own task queue rather than from env, because workflow
code must stay deterministic". A number read from the environment would replay differently on a
worker whose environment differs, corrupting the history of every run that used it. In the input it
is recorded, so a replay sees exactly what the original attempt saw.

Bounded in both directions (`heartbeatFor`): a request **below** two minutes is ignored, because
nothing from outside should make an activity more fragile than production; a request **above**
`StartToClose` is capped, because a heartbeat that can never fire reads as "liveness checking is off"
while claiming a number.

> **Not for production.** A long heartbeat reinstates the wedge the default exists to catch — a
> genuinely stuck Unit then holds its lease for the relaxed duration instead of two minutes.

### A Go actor

Same shape, with Delve. `dlv` speaks DAP natively, so any DAP-capable editor attaches:

```sh
dlv attach $(pgrep -f 'myactor') --headless --listen=127.0.0.1:2345 --api-version=2
```

The heartbeat ceiling is identical — it belongs to the activity, not to the language.

## A caller workflow: replay, with no clock

A workflow is not debugged by attaching. Record its history and replay it:

```sh
kontra workflow history <workflow-id> -o run.json
kontra workflow replay ./workflows/hunt/workflow.py --history run.json
```

**Nothing times out.** No heartbeat, no `StartToClose`, no server waiting on a poller — sit on a
breakpoint for an hour. And it is post-mortem: a run that failed on a fleet three days ago is
steppable on a laptop, which is the only cheap way at the bugs that appear only at fleet scale.

Debug it by running that command under your editor's debugger — it is an ordinary local Python
process, so a plain `launch` configuration works here (unlike the actor case above).

**Activity code is not run.** A Method's results come from the history as recorded values, so a
breakpoint inside a Method will never be hit by replay. Replay covers the caller's decisions:
splitting, chaining, branching, early exit.

Exit codes: `0` clean · `1` the code is non-deterministic against that history · `2` the replay could
not be performed (a missing file, an import error). The third is a fact about your setup, not about
your workflow, and is kept distinct for that reason.

## Why there is no `kontra debug`

Windmill ships a DAP server over WebSocket into a Monaco editor. It is good work and kontra does not
need it: your code runs on your machine under `--mode local`, where the debugger you already use
attaches to it. What kontra owes you is the two things above — the interpreter hook, and enough time
to think.

Their own threat model is worth noting here: `REQUIRE_SIGNED_DEBUG_REQUESTS` "now defaults to
`true`". The word *now* is doing the work — the debug endpoint shipped unsigned. A debugger is
arbitrary code execution with variable inspection on a host running untrusted actors. If kontra ever
grows a remote debug surface, requests are signed on day one.
