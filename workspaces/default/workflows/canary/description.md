# canary

**Provisions a Fleet, sweeps on it, and lets you watch every part of it happen.** The first run
anybody does on a fresh install, and the one that shows what kontra actually is.

In one run you see a Fleet come into existence, a Worker pick up work on it, typed records arrive
in a pane every two seconds *while it is still running*, rows land in a Dataset you can query in
SQL, and the Machines destroyed when the scope exits — with a line of code responsible for each.

Nothing here is a mock. Real Fleet, real Worker, real Dataset, real stream. The only pretend part
is that the actor sleeps instead of reaching somebody else's estate, which is the one thing a first
run should not do.

## Run it

```sh
kontra deploy --actor workspaces/default/actors/canary
kontra workflow serve workspaces/default/workflows/canary --tmux
kontra workflow start Canary --wait
```

No `--input` needed. Every field has a default, and the console's launch form renders them with
help text because they are declared as `Annotated[..., Field(default=…, description=…)]` rather
than as comments — comments do not exist at runtime.

## What the knobs do

| field | default | what it changes |
|---|---|---|
| `targets` | `["alpha", "beta"]` | one target is one **Unit** — the unit of failure and of resumption |
| `steps` | `5` | phases per target; `targets × steps` is both the records you watch and the rows you get |
| `every` | `2.0` | seconds between records — the Workflow Stream's own flush interval |
| `machines` | `1` | Fleet scale |
| `sessions` | `1` | density per Machine |
| `provider` | `docker` | `docker` = Warden containers, no credential. `cloud` = DigitalOcean |
| `fail_on` | `""` | name a target and that one Unit raises, so per-unit isolation is visible |

## Where the time goes

The sweep is `targets × steps × every` and nothing else — 20 seconds at the defaults, all of it
deliberate and all of it on screen. Everything before it is the Fleet:

- **`docker`** — seconds. Warden containers on the Compose network.
- **`cloud`** — about 50 seconds to converge, plus cloud-init, plus the Worker's first poll.

That asymmetry is why `provider` defaults to `docker`. A first run should not depend on a cloud
credential, and `cloud` additionally needs `KONTRA_CONTROLLER` to be an address a Droplet can
actually reach — a Compose service name is not one, and a Machine that cannot call home starts,
registers nothing, and looks idle.

## What to look at while it runs

- **the run page** — three publishers on three topics: this workflow's `progress`, the actor's
  typed `canary/sweep` records, and the run's own history. They are separate because they answer
  different questions; folding them into one flat state is what makes a bar jump.
- **Datasets** — `canary_signals` fills while the run is still going. Click a cell to read it whole.
- **Logs** — every line carries the run id and the Worker identity, so "which process said this"
  is answerable without leaving the page.
