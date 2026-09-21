# redditscrape

Crawls Reddit for CMS/platform migration talk on a fleet it provisions and destroys itself, and
lands one row per post and per comment in a Dataset.

Built from the client's prototype (kept verbatim at `examples/private/reddit-prototype/`). The script's own bookkeeping — dated file
stems, a jsonl replayed on startup to work out which subreddits were done, "discard the partial
subreddit so the next run redoes it" — is gone, because a Run already commits per Unit. What was
kept is every measured anti-block finding: the in-page `fetch()` instead of navigating to a
`.json` URL, a warm-up that probes a real endpoint before spending requests, host-scoped
`cf_clearance`, 5s pacing, and Reddit's own `sort=top` order left un-re-sorted.

## Two transports, one signature

| `actor` | How it reads Reddit | Needs | Machine |
| --- | --- | --- | --- |
| `redditapi` **(default)** | OAuth bearer token, `oauth.reddit.com/...` | Reddit script-app credentials | 1 vCPU / 1 GB, no browser |
| `reddit` | Camoufox browser, `old.reddit.com/...json` | an address Reddit serves anonymously | 2 vCPU / 4 GB, ~150 MB browser |

Both expose the same `discover` / `harvest` signature and produce the same Dataset, so switching
is one word in the request: `{"actor": "reddit"}`. Everything that is not transport — the
taxonomy, the search paging, the comment tree, the row builders — is a single shared
`redditcore.py`, symlinked into both actor directories so a Bundle stays self-contained while the
checkout keeps one editable original.

`redditapi` is the default because it is the only one with a credential to present, and a
credential is what the block is keyed on (see **Egress**). It is also lighter by every measure: no
browser, no 150 MB fetch, no warm-up, a Machine ready in seconds. Its free tier is 100
queries/minute and scoped to non-commercial use, so client work needs a commercial agreement with
Reddit — a contractual question, which is why both transports still exist.

## Two Methods

| Method | Takes | Emits | Does |
| --- | --- | --- | --- |
| `discover` | `Target` (subreddit, tier) | `Post` | searches one subreddit, keeps the posts that match |
| `harvest` | `Post` | `Document` | fetches one thread, emits the post row and every comment row |

`Post` is deliberately both `discover`'s output and `harvest`'s input, so the posts are the work
list and the fan-out is the platform's rather than a nested loop's.

`tier` is the ambiguity switch, not a label. In a platform subreddit the whole feed is on topic;
in a general one each keyword is searched and then re-checked locally, because Reddit's search
matches loosely and "payload" in r/webdev is an HTTP payload far more often than the CMS.

## Output

One flat `Document` row per post and per comment, discriminated by `type`, using the prototype's
own column names so this Dataset and the client's spreadsheet share a vocabulary.

Three comment counts ride on every post row, and the redundancy is the point:

- `num_comments` — Reddit's own number, the independent one
- `comments_fetched` — what we actually parsed
- `fetch_failed` — whether the fetch errored

Reddit says 40 and we fetched 0 is a hole in the dataset. Reddit says 0 is an empty thread.
Without the independent number those two rows were identical forever.

## One Machine, one session

`sessions` is density, and for this workflow it must stay at 1. Reddit's free OAuth tier is 100
queries/minute **per client**, not per connection, so a second session on the same credential does
not double throughput — it halves each session's share and races the ratelimit floor both are
reading. (For `reddit` it is worse: each session is its own Camoufox behind the same Machine IP.)

## A fresh Droplet per crawl

`machines` provisions inside the run's own scope and is destroyed at scope exit, so every crawl
leaves from an address that did not exist before it started and does not outlive it. That is the
default behaviour of `fleet.up`, not an option — the Machine lives inside the `async with`
precisely so it cannot be shared between runs.

It is worth having. It is **not** what gets you past Reddit — see below.

## Egress — read before running

Measured 2026-08-24 from three brand-new Droplets, one per region, minutes old:

| path | sfo3 | nyc3 | ams3 |
| --- | --- | --- | --- |
| `POST www.reddit.com/api/v1/access_token` | 401 | 401 | 401 |
| `oauth.reddit.com/<anything>` | 403 | 403 | 403 |
| `www.reddit.com/r/<sub>/new.json` | 403 | 403 | 403 |
| `api.reddit.com`, `old.reddit.com`, `/.rss` | 403 | 403 | 403 |

Every 403 is the same 1522-byte `<title>Blocked</title>` page carrying "blocked due to a network
policy". **Rotating the address does not help** — those were new addresses, and they were blocked
on arrival.

The exemption is the useful part. Reddit is not banning DigitalOcean; it is banning traffic to
data hosts that does not carry a token it accepts, while deliberately leaving open the one
endpoint you need in order to obtain such a token. Every probe above sent
`Authorization: bearer FAKE`, which an edge can reject without consulting the API.

**Whether a valid token clears the edge is the one variable that could not be tested here**, because
no Reddit credential exists on this control plane. It is exactly what `redditapi` supplies, which
is why it is the default. Get one in two minutes at <https://www.reddit.com/prefs/apps> — app type
**script** — and pass `client_id`, `client_secret`, `username`, `password` and a descriptive
`user_agent` in the request.

Without them the run does not hang and does not fabricate: `@actor.load` raises
`NonRetryableError` naming the missing field before any crawling happens, the Dataset is still
promoted, and the Run fails saying it harvested nothing.
