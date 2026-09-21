# canary

Calls one Actor twice — first on a worker here, then on a fleet it provisions — and reports whether the two halves agree.

## What it is for

Two different questions get asked when something breaks, and until now they needed two different runs.

*Does this installation work at all?* — the interpreter the worker booted with, the task queue, the Nexus endpoint the dispatch is addressed to, the codec that carries a Batch over S3. All of that is provable in a second, locally, for nothing.

*Does it work the same way on a Machine?* — a stale Artifact, a version pinned differently in two places, a codec that passes through locally and truncates over the object store. That costs four minutes and real money to find out, and it is usually asked only after a run has already failed.

`canary` is both, in that order. The cheap half runs first, so a wiring fault fails before any Droplet is created; the expensive half only ever runs against a control plane that has just been proven.

## The comparison is the output

Both halves are handed the **same units** and call the **same Methods** on the **same Actor** — the only thing that changes is which worker is polling the queue. That is what makes a divergence meaningful: `agree: false` says the placement changed the answer, which is the failure this exists to catch. Verdicts are compared by `(domain, nameserver)` rather than by list order, because two Machines answer in whatever order they finish.

`agree` is `null`, never `true`, when no fleet ran. An absent comparison and a passing one must not render the same.

## Machines cost money

`machines` defaults to **0**, which runs the local half only. Provisioning outlives the tab and bills by the hour, so it is asked for explicitly:

```
kontra workflow start Canary --queue canary --wait \
    --input '{"domains": ["example.com"], "machines": 2}'
```

The fleet scope destroys its Machines on exit, and it is a replayable step in a durable program rather than a line in a script that might not be reached.

## Input

| field | default | what it does |
|---|---|---|
| `domains` | `example.com`, `iana.org` | what to look up. No Dataset needed — a canary that required one would fail on a fresh installation for a reason unrelated to what it tests |
| `machines` | `0` | Droplets for the second half. `0` skips it |
| `sessions` | `2` | live Sessions per Machine — density, not scale |
| `tag` | `canary` | the DigitalOcean tag and `kf-<tag>-NN` name prefix |
