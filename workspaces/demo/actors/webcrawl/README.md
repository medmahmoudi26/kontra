# webcrawl

A Playwright crawler that emits one object per HTTP request and one per HTTP response.

```
@actor.load   one Chromium for the whole session
@actor.method one BrowserContext per unit, `concurrent_aruns` of them at once
@actor.close  close the browser
```

A **context** per unit, not a browser. A browser costs ~120 MB and several hundred ms to
launch; a context costs a few MB and is instant, and it is still a real isolation boundary —
its own cookie jar, storage and cache. So N seeds crawl concurrently in one process without
leaking session state between the programs we are pointed at, which for scope-separated
bug-bounty targets is a correctness property rather than hygiene.

## Output

One flat record type, `kind` discriminating request from response, `tx` joining the pair. They
are the same type on purpose: an arun yields records of one declared type, and a request and
its response are two observations of one transaction.

```sql
-- what answered, and how
SELECT status, count(*) FROM run WHERE kind='response' AND status > 0 GROUP BY status;

-- pair them
SELECT req.url, res.status FROM run req JOIN run res USING (tx)
 WHERE req.kind='request' AND res.kind='response';
```

The arun is an **async generator**, so each record is committed as its own durable sub-unit as
it is emitted — events land while the page is still loading. That is why capture feeds a queue
the generator drains rather than the obvious collect-then-yield: Playwright's `page.on(...)`
callbacks cannot yield, and buffering a whole page would give up exactly that property.

## Hardware and dependencies

`actor.json` declares what the actor needs, in both places it can run:

```json
"targets": {
  "container": {"memory": "3g", "cpus": 2, "shmSize": "1g"},
  "machine":   {"size": "s-2vcpu-4gb", "region": "sfo3", "image": "ubuntu-22-04-x64"}
}
```

`kontra fleet up --actor .` sizes Machines from `machine`; `kontra fleet deploy --actor .`
passes `container` through as the Worker's `--memory/--cpus/--shm-size`. Chromium with the
default 64 MB `/dev/shm` crashes under concurrent contexts, which is what `shmSize` is for.

`deploy.sh` installs the dependencies. One script, both Targets: `kontra deploy` runs it inside
the image while building a container Target, and a machine Target runs the same script over
SSH. That is why it is a script and not a Dockerfile — a Dockerfile can only express one of the
two, and deps that exist in only one place produce an actor that cannot be placed in the other.

## Running it

Locally — the actor (a Temporal activity worker) and the Temporal handler, both from one command:

```sh
kontra serve --actor examples/python/webcrawl --redis 10.124.0.2:6379
kontra dataset create examples/python/webcrawl/seeds.jsonl seeds
```

Then drive it from a caller workflow you serve — there is no dispatch verb (ADR 0023 §12):

```python
async with catalog.actor("webcrawl", "0.1.0") as crawl, \
           catalog.dataset("findings").writer() as out:
    async for batch in catalog.dataset("seeds").batches(100, order_by="seed"):
        found, dropped = await crawl.crawl(batch, out)
```

On a fleet:

```sh
kontra deploy --actor examples/python/webcrawl        # build the Artifact, push, register
kontra fleet up     --count 2 --tag crawl --actor examples/python/webcrawl
kontra fleet deploy --tag crawl --actor examples/python/webcrawl
```

…and the same caller, pointed at the scope. The query moves from a CLI flag into `batches()`:

```python
async for batch in catalog.dataset("scope_paid").batches(
    100,
    order_by="seed",
    where="kind='domain' AND bounty",
):
    found, dropped = await crawl.crawl(batch, out)
```

## Params

| param | default | why you would change it |
|---|---|---|
| `concurrent_aruns` | 6 | contexts in flight; the real throughput knob |
| `nav_timeout_ms` | 20000 | a timeout is recorded as an event, not a failed unit |
| `settle_ms` | 2000 | an SPA issues most of its XHRs *after* `load` |
| `wait_until` | `load` | `networkidle` for slow SPAs, `commit` for pure inventory |
| `max_events` | 400 | per-seed cap; the stream stops, the unit still commits |
| `capture_bodies` | false | sha256 + true length per response — slower, much larger |
| `block_media` | true | drops image/media/font: bytes without inventory value |

## Tests

```sh
.venv/bin/python -m pytest examples/python/webcrawl/test_webcrawl.py -q
```

Everything runs without a network except one test marked `live`, because the failures that
actually cost a run are the quiet ones: a header set that blows up a column store, a scope
row that is not a URL (`com.stubhubandroid` is a real one), an event that cannot be joined back
to its request.
