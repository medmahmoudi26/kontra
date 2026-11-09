# sellers/a2ui

A small A2A agent that takes a table booking through an [A2UI](https://a2ui.org) v0.9.1 form: a name, the number of
guests and a date and time. It is the examples' A2UI seller ([`spec/relay.md`](../../../spec/relay.md) §2.6, D326 to
D329). It is written with a2a-go v2.7.0, speaks A2A 1.0, uses no LLM and imports nothing from Kontra; it lives in this
module only so that `go build ./...` builds it and the e2e tests can run it.

| File | What |
|---|---|
| `main.go` | The agent: card, JSON-RPC and HTTP+JSON, the executor, the form |
| `main_test.go` | Its behaviour over both bindings, held to the fixtures (`go test ./examples/sellers/a2ui`; `-update` rewrites the fixtures it writes) |
| `kontra.yaml` | A team gate on one machine with the tunnel service `a2ui` |
| `testdata/a2ui-v0.9.1/` | The A2UI v0.9.1 schemas and basic catalog, vendored unmodified with A2UI's licence ([its README](testdata/a2ui-v0.9.1/README.md)) |
| `testdata/messages/` | The form the agent sends (`surface.json`), the message that deletes it (`delete.json`), a renderer's action (`action.json`), a client's capabilities (`capabilities.json`); a task's surface id is written `booking-{taskId}` |
| `testdata/card/` | The card's A2UI params |
| `testdata/booking.json` | A booking a first message's data part may hold |
| `testdata/invalid/` | Fixtures the schemas must refuse |

`make check-specs` validates every fixture against the vendored A2UI schemas, formats included (an RFC 3339
`timestamp`), and checks the vendored files against `SHA256SUMS`; the
Go tests hold the agent's output to the fixtures.

## What it does

The card declares one extension, not required (its entry writes `"required": false`): **A2UI v0.9.1**,
`https://a2ui.org/a2a-extension/a2ui/v0.9.1`, with `params.supportedCatalogIds` the basic catalog,
`https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json`. Its skill's `examples` ("a table, please", "book a
table for two") suggest a first message, which a client shows as it likes; `/mcp` `describe` returns them. A first
message is plain A2A: there is no typed first message (D350).

A first message stops the task at `input-required`. What the agent asks depends on whether the client renders A2UI:

- **It does** when its `a2uiClientCapabilities` (message metadata, else the request's) list the basic catalog under
  `v0.9.1` or `v0.9`, or, when it sent none, when it activates the extension (`A2A-Extensions`, or the message's
  `extensions`). Its question is "Fill in the booking form: a name, the number of guests and a date and time." and a
  data part holding the form: A2UI's list of messages, `createSurface`, `updateComponents` (a `Column` of a title, a
  `TextField`, a `ChoicePicker`, a `DateTimeInput` and a `Button` whose action `book` carries the three fields) and
  `updateDataModel` (two guests chosen). The part is marked `metadata.mimeType: "application/a2ui+json"`, as A2UI
  specifies, and `mediaType: "application/a2ui+json"`, A2A 1.0's own field for the same thing.
- **It does not**: the question asks, in text, for a data part holding the booking, `{"name": string, "guests": 1 to
  6, "date": RFC 3339 date-time}`.

Each task's form is a surface of its own, `booking-` and the task's id: A2UI refuses a `createSurface` for an id the
client still shows, and one context (A2UI's session) holds as many forms as it has tasks. The messages' `version` is
`v0.9.1` for a client that said it reads v0.9.1 (a `v0.9.1` capabilities entry listing the basic catalog, or the
activation of the v0.9.1 URI) and `v0.9` otherwise: a client that lists the catalog under `v0.9` alone may be a v0.9
renderer, whose schemas accept only `"v0.9"`, and v0.9.1's accept both. The mark is `application/a2ui+json` in either
version, as A2UI's own Node sample agent sends its v0.9 messages (D329).

The form's Book button sends an A2UI `action` back in a data part marked the same way; the agent reads it as the list
A2UI v0.9.1 specifies or as one message, under either MIME name (`application/a2ui+json`, v0.9's
`application/json+a2ui`), by `metadata.mimeType` or `mediaType`, and v0.8's `userAction` too. A booking that checks
out completes the task with a summary, "Booked a table for 4 under "Robin" on Monday, 9 November 2026 at 19:30.", and,
when the task sent a form, a second A2UI part deleting it (`deleteSurface`, in the form's version): its Book button has
nothing left to book. The agent reads the booking, never the action's `surfaceId`. One that does not check out is asked
to be corrected, naming what is wrong, in text alone: the client keeps its form and what the person typed in it (A2UI
creates a surface once, and the agent sends a task's form once).

A first message whose data part already holds that booking is booked at once; one holding part of it gets the form with that part filled in. A data part without `name`,
`guests` or `date` (A2UI's samples send `{"useStreaming": false}` beside each message) is not a booking. `CancelTask`
ends a waiting task `canceled`, deleting its form as a booking does. Tasks live in memory, and a request body over 4
MiB is refused, as the booking example's.

```
a2ui [--host 127.0.0.1] [--port 9995]
```

It serves its card at `/.well-known/agent-card.json`, JSON-RPC at its root and HTTP+JSON under `/rest`, all under one
root URL, as `kontra connect --serve` needs. The first line on standard output is the root URL; `SIGINT` or `SIGTERM`
stops it.

## Talk to it directly

a2a-cli sends text only, so these are JSON-RPC requests with `curl`:

```sh
go run ./examples/sellers/a2ui &        # http://127.0.0.1:9995

curl -s http://127.0.0.1:9995/ -H 'A2A-Version: 1.0' -H 'Content-Type: application/json' \
  -H 'A2A-Extensions: https://a2ui.org/a2a-extension/a2ui/v0.9.1' \
  -d '{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER",
       "parts":[{"text":"a table, please"}]}}}'
#  {"result":{"task":{"id":"<task id>", … "status":{"state":"TASK_STATE_INPUT_REQUIRED","message":{"parts":[
#    {"text":"Fill in the booking form: …"},{"data":[{"createSurface":{… "surfaceId":"booking-<task id>"},…},
#    {"updateComponents":…},{"updateDataModel":…}],
#    "mediaType":"application/a2ui+json","metadata":{"mimeType":"application/a2ui+json"}}] …

curl -s http://127.0.0.1:9995/ -H 'A2A-Version: 1.0' -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"SendMessage","params":{"message":{"messageId":"m2","role":"ROLE_USER",
       "taskId":"<task id>","parts":[{"data":'"$(cat examples/sellers/a2ui/testdata/messages/action.json)"',
       "metadata":{"mimeType":"application/a2ui+json"}}]}}}'
#  … "state":"TASK_STATE_COMPLETED" … "Booked a table for 4 under \"Robin\" on Monday, 9 November 2026 at 19:30." …
#    {"data":[{"deleteSurface":{"surfaceId":"booking-<task id>"},"version":"v0.9.1"}], …

curl -s http://127.0.0.1:9995/rest/message:send -H 'A2A-Version: 1.0' -H 'Content-Type: application/json' \
  -d '{"message":{"messageId":"m3","role":"ROLE_USER","parts":[{"mediaType":"application/json",
       "data":{"name":"Robin","guests":4,"date":"2026-11-09T19:30:00+01:00"}}]}}'
#  … "state":"TASK_STATE_COMPLETED" … "Booked a table for 4 under \"Robin\" on Monday, 9 November 2026 at 19:30 +01:00." …
```

An A2UI renderer (`@a2ui/lit`, `@a2ui/react`, `@a2ui/angular` on `@a2ui/web_core`) draws the form from the first
answer's data part and sends the action itself; the renderers ship no A2A client, so the page wires one in.

## Through the gate

`kontra.yaml` here is a team gate on one machine with one tunnel service, `a2ui`; the commands are the booking example's
([`../booking/README.md`](../booking/README.md#through-the-tunnel-m2)) with `a2ui` for `booking` and port 9995. The gate
interprets no A2UI ([`spec/relay.md`](../../../spec/relay.md) §2.6):

- the card's A2UI entry is served as the agent wrote it;
- `A2A-Extensions: https://a2ui.org/a2a-extension/a2ui/v0.9.1` is echoed and reaches the agent, and so does message
  metadata (`a2uiClientCapabilities`); only `kontra.*` keys are removed;
- the form and the action pass as data parts with their marks, equal under JCS to what was sent (the gate re-encodes
  them: member order, number spelling and escapes may change);
- a part over 256 KiB is no longer a data part (it is put in the bucket and becomes a URL), so an agent keeps each A2UI
  part under that, splitting a large surface's messages across parts;
- the gate speaks A2A 1.0 only to callers, so a client on A2A 0.3 (A2UI's reference web clients use `@a2a-js/sdk` 0.3)
  cannot call it as it is; an agent on A2A 0.3 behind it must send one A2UI message per data part (D327);
- `/mcp`'s `hire` and `reply` cannot activate the extension nor send message metadata, and the hire result drops a
  part's marks (D328): this agent answers a `hire` in text, and `reply` with a data part holding the booking books. A
  part's own `metadata` and `mediaType` do reach it, so `reply` with the form's action in a part marked
  `application/a2ui+json` books too.

`test/e2e` `TestA2UIPassthrough` (`a2ui_passthrough`) runs this agent through two gate replicas, hosted by `kontra
connect`.
