# crawl4ai — streaming deep-crawl actor (two tiers)

crawl4ai **as a library**: one `AsyncWebCrawler` (one browser) per session opened in
`@actor.load`, one seed deep-crawled per `@actor.method` unit with crawl4ai's **native**
`BFSDeepCrawlStrategy`, browser shut down in `@actor.close`.

**Isolation**: one browser, but **one `BrowserContext` per in-flight arun**. crawl4ai caches
contexts by a config signature, so identical per-seed configs would put every concurrent seed in
ONE context — one cookie jar / storage / cache shared by unrelated targets. The actor routes
crawl4ai's page allocation to the arun's own context (built with crawl4ai's own
`create_browser_context`/`setup_context`, so the fingerprint is unchanged) and closes it when the
seed finishes, so live contexts are `concurrent_crawls` wide, never batch wide.

**Streaming**: every page is `yield`ed, so the framework commits it as its own durable
sub-unit blob (`units/{run}/{node}/u{i}/{page-sha}.json`) the moment it is crawled. Page
content **never** rides the resume state. Records are content-deterministic (stable response
fields only — no `Date`/`Set-Cookie` headers) so the sub-unit sha is stable and a re-emit is an
idempotent overwrite. See the wiki [[Data-Plane]] / [[Durability-and-Failures]].

**Two durability tiers** (the `resumable` param):

- **DEFAULT (`resumable=false`) — atomic per seed.** No `arun_state`; bounded by `max_pages`. A
  browser/host death re-crawls the whole seed from scratch on the retry — safe because each page
  is keyed by its content sha, so re-emission overwrites idempotently and nothing duplicates.
- **RESUMABLE (`resumable=true` **and** `self.emit_durable`) — frontier resume.** crawl4ai's native
  BFS carries a **frontier-only** state (`strategy_type, visited, pending, depths, pages_crawled` —
  no page content); the actor wires it straight to `self.arun_state` via `resume_state` /
  `on_state_change`, so a death resumes the frontier instead of restarting. The right use of
  `arun_state`: a small, replay-tolerant frontier, never a result store. **Gated on
  `self.emit_durable`**: the frontier may skip pages on resume only if those pages were durably
  emitted, so with no object store (inline mode) this tier falls back to the DEFAULT atomic re-crawl.

The seam (`actor.py`):

```python
resumable = bool(param.get("resumable", False)) and self.emit_durable  # gate on durable emit
strategy = BFSDeepCrawlStrategy(
    max_depth=..., max_pages=...,
    resume_state=(await self.arun_state.get("frontier")) if resumable else None,
    on_state_change=(lambda s: self.arun_state.set("frontier", s)) if resumable else None,
)
async for result in await self.crawler.arun(url, config=cfg):   # stream=True
    yield _page_out(result, url, discovery)                      # one durable sub-unit per page
```

> Note: the native frontier advances just **before** a page is yielded, so a death in that tiny
> window skips one already-fetched page on resume — an accepted at-most-once-miss tradeoff of
> honoring crawl4ai's native ordering, not the whole-seed loss the old checkpoint design risked.

## Knobs (`params`)

| knob | default | meaning |
|---|---|---|
| `concurrent_crawls` | 4 | seeds in flight at once over the one browser (the arun window) |
| `depth` | 2 | BFS max depth |
| `discovery_mode` | false | true = link graph only (drop markdown) |
| `headless` | true | forced true when no `$DISPLAY` |
| `max_pages` | 50 | per-seed cap (also settable per unit) |
| `resumable` | false | false = atomic per seed (re-crawl on death); true = frontier resume via `arun_state` |

## Run

```sh
# local dev — ONE command starts both halves: the actor (a Temporal activity worker on
# crawl4ai-1.0.0-sessions) and the handler (the workflow, on crawl4ai-1.0.0). The one venv
# needs this actor's own deps, the same ones its Dockerfile installs:
#   .venv/bin/pip install 'crawl4ai>=0.8,<1' 'simhash>=2.1,<3'
#   .venv/bin/python -m playwright install chromium
# (>=0.8 for the native resume_state / on_state_change hooks the RESUMABLE tier drives.)
# KONTRA_S3_ENDPOINT is inherited from the environment — unset means inline mode, which
# also means `resumable` falls back to the DEFAULT atomic tier (it gates on emit_durable).
KONTRA_S3_ENDPOINT=http://localhost:8333 \
  kontra serve --actor examples/python/crawl4ai --redis 127.0.0.1:6379

# dispatch
kontra actor crawl4ai@1.0.0 dispatch --input examples/python/crawl4ai/seeds.jsonl \
  --params 'depth=1,discovery_mode=true'
```

Tests: `.venv/bin/python -m pytest examples/python/crawl4ai/test_crawl4ai.py`
(tier + isolation tests against the fake seam need only `simhash`, which `actor.py` imports at
module level, and no browser; the `e2e` tests drive real Chromium —
the two per-arun isolation ones need only crawl4ai + Chromium, and the engine-loop one drives the
real engine with an in-memory state store).
