# sellers/tck

The seller the pinned A2A TCK runs against through the gate (`make tck`, [`test/tck`](../../../test/tck/README.md);
spec/relay.md §16 `tck_*`, D18, D279): a plain a2a-go v2.7.0 agent that does what the TCK's Gherkin scenarios ask
of a system under test (a2a-tck `d4d6fae`, `scenarios/core_operations.feature` and `scenarios/streaming.feature`).
It imports nothing from Kontra and has no Kontra-specific endpoint (`examples_test.go` checks the import).

The TCK says which behaviour it wants with the prefix of the message id, `tck-<scenario>-<8 hex digits>`, and the
gate passes message ids upstream unchanged (spec/relay.md §4):

| Message id prefix | The agent |
|---|---|
| `tck-complete-task` | completes the task, status message "Hello from TCK" |
| `tck-artifact-text` | adds an artifact with the text "Generated text content", completes |
| `tck-artifact-file` | adds an artifact with a file part `output.txt`, `text/plain`, content `tck`, completes |
| `tck-artifact-file-url` | adds an artifact with the URL `https://example.com/output.txt`, `output.txt`, `text/plain`, completes |
| `tck-artifact-data` | adds an artifact with the data `{"key": "value", "count": 42}`, completes |
| `tck-message-response` | answers with a message, "Direct message response", and no task |
| `tck-input-required` | stops at `input-required` (a follow-up `tck-complete-task` completes it) |
| `tck-reject-task` | rejects the task, status message "rejected" |
| `tck-stream-001`, `-003`, `tck-stream-ordering-001`, `tck-stream-artifact-text`, `-file` | working, the artifact, completed |
| `tck-stream-002` | completed |
| `tck-stream-artifact-chunked` | working, the chunk "chunk-1 ", the last chunk "chunk-2" (append), completed |
| `test-resubscribe-message-id` | working, 4 s, completed |
| anything else | completes, status message "Unhandled messageId prefix: <id>" |

A cancel ends a task that is not over `canceled`. The card lists `text/plain` and `application/json` as input
modes, and a2a-go's card enforcer answers any other media type `ContentTypeNotSupportedError` (spec §3.1.1; the
gate does not enforce modes, the upstream does, D14). Push notifications are the gate's (spec/relay.md §10): the
card declares none, and no required extension (the TCK never sends `A2A-Extensions`, a2a-tck#193).

## Run it

```sh
go run ./examples/sellers/tck --port 9996     # prints http://127.0.0.1:9996; JSON-RPC at /, HTTP+JSON under /rest
```

`--port 0` takes any free port; the first line on standard output is the root URL. Behind a gate it is a direct
backend (`backend: { a2a: { url: "http://127.0.0.1:9996", allow_private: true } }`) or hosted through the tunnel
(`kontra connect --serve a2a:tck=http://127.0.0.1:9996`); `test/tck/gate-*.yaml` are the configurations `make tck`
uses. The TCK can also run against it alone, as M0 measured a2a-go:

```sh
cd <a2a-tck at d4d6fae> && uv run --frozen ./run_tck.py --sut-host http://127.0.0.1:9996 --transport jsonrpc,http_json
```

Through the gate, the TCK's message ids are one per session, not one per call (a2a-tck
`tck/requirements/base.py`, `tck_id`): every prerequisite task after the first is the same request with the same
id, which the gate answers as a resend with the first one's task (spec/relay.md §15; A2A §3.3.1 lets an agent use
the message id to detect duplicates). See [`test/tck/README.md`](../../../test/tck/README.md).
