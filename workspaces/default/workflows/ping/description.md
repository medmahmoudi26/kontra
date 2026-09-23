# ping

The smallest caller workflow there is, for checking the wiring.

It dispatches nothing, provisions nothing and writes nothing — which is the point: when Serve or
Run misbehaves, this separates "the control plane is broken" from "my run is broken", and it
does it without spending a cent or touching a target.

## What it does

Returns what it was given, plus the workflow's own clock and run id. `workflow.now()` rather than a
real clock, because a workflow is replayed and a real clock would answer differently every time.

Serve it on a queue, press Run, and a Run appears in the list. If it does not, the problem is the
wiring — the tmux session, the interpreter, the queue name — and not your workflow.

## What it takes

    {"note": "hello"}

Optional, as is the argument itself: `run(self, req=None)`.

## What it leaves behind

Nothing but a closed Run in Temporal, and its result:

    {"ok": true, "note": "hello", "at": "…", "run": "ping-1755…"}

No fleet, no dataset, no Machines. Nothing to tear down and nothing to query afterwards.

## Run it

    kontra workflow serve ping --queue scratch --tmux
    kontra workflow start Ping --queue scratch --wait --input '{"note": "hello"}'
