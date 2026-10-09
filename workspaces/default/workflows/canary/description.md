# canary

**Provisions a Fleet, sweeps on it, and lets you watch every part of it happen.** The first run
anybody does on a fresh install, and the one that shows what kontra actually is.

In one run you see a Fleet come into existence, a Worker pick up work on it, a line arrive every
two seconds saying what it is doing *while it is still doing it*, rows land in a Dataset you can
query in SQL, the Machines destroyed when the scope exits, and a report saying what came of it —
with a line of code responsible for each.

Nothing here is a mock. Real Fleet, real Worker, real Dataset. The only pretend part
is that the actor sleeps instead of reaching somebody else's estate, which is the one thing a first
run should not do.

## Run it

```sh
kontra deploy --actor workspaces/default/actors/canary
kontra workflow serve workspaces/default/workflows/canary
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
| `every` | `2.0` | seconds between records — one log line and one Dataset row each |
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

## What to read when it is done

**The report** — *Read the report* at the bottom of the run page. It is `report.md` beside
`workflow.py`, rendered from what the run returned: one sentence on how it went, the Fleet and the
sweep in a row, every target with its records and outcome (`swept`, `dropped` with the target's own
error, or `voided`), and the SQL that reads back exactly this run's rows. Set `fail_on: beta` to see
a dropped target in it.

Press **Run** in the console to get this report. A run started with `kontra workflow start` dials
Temporal directly, so nothing pins its template and it gets the default report, with a warning
saying so.

## What to look at while it runs

- **the run page** — the log rail and the Dataset tail, side by side. The workflow narrates the
  run, the actor narrates the sweep, and both land on the same rail carrying the run id and the
  Worker identity. There is no separate progress pane: a Workflow Stream lives in workflow memory
  and dies with the run, so a pane fed by one is empty for everybody who opens the page after it
  finishes — which, at 55 seconds, is everybody. Typed streaming returns when there is a durable
  store behind it.
- **Datasets** — `canary_signals` fills while the run is still going. Click a cell to read it whole.
- **Logs** — every line carries the run id and the Worker identity, so "which process said this"
  is answerable without leaving the page.
