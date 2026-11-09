# sellers/ap2-merchant

A small A2A 1.0 agent that plays the **Merchant** role of [AP2 v0.2.0](https://github.com/google-agentic-commerce/ap2)
(the Agent Payments Protocol, released 28 April 2026) for one example item: it answers a merchant-signed checkout,
checks a Checkout Mandate bound to it, and answers a Checkout Receipt. **It takes no payment.** It is written with
a2a-go v2.7.0 and the Go standard library's crypto, has no LLM, and imports nothing from Kontra; it lives in this
module only so that `go build ./...` builds it and the e2e tests can run it.

Kontra has no payment concept (D351): getting paid is the seller's own business, and AP2 is how an A2A seller
does it. The gate relays AP2 like any extension it does not own (spec/relay.md §2.6): the card entry served as
written, the activation forwarded, the data parts relayed equal under RFC 8785 JCS, sealed like any part on a
sealed service. It reads, verifies, stores apart from the task log and indexes nothing AP2.

| File | What |
|---|---|
| `main.go` | The agent: card, JSON-RPC and HTTP+JSON, the executor, the checkout, mandate check and receipt |
| `main_test.go` | Its behaviour over both bindings and the mandate check's refusals (`go test ./examples/sellers/ap2-merchant`) |
| `testdata/closed-checkout-mandate.sdjwt` | AP2 v0.2.0's own "Closed Checkout Mandate" example token (`docs/ap2/checkout_mandate.md`, Apache 2.0) |
| `kontra.yaml`, `kontra-sealed.yaml` | A team gate on one machine with the tunnel service `ap2-merchant`, plain and sealed |

## AP2's roles, and which one this is

AP2 v0.2.0 has five roles. A **Shopping Agent** acts for the user; a **Trusted Surface** shows the user what they
approve; a **Credential Provider** holds the user's payment credentials and issues the Payment Mandate's credential;
the **Merchant** sells and signs the checkout; the **Merchant Payment Processor** (MPP) takes the payment. Two
mandates, SD-JWT credentials, carry the user's authority: the **Checkout Mandate** (to the Merchant: "complete this
checkout") and the **Payment Mandate** (to the payment side: "pay this much for this checkout").

This example is the Merchant alone. A real merchant wires, of its own choosing:

- a **Merchant Payment Processor**, to which it forwards the Payment Mandate (`ap2.mandates.PaymentMandateSdJwt`)
  and which answers a Payment Receipt (`ap2.PaymentReceipt`). This example reads no Payment Mandate: a message that
  carries one is treated like any other;
- the **keys it trusts** for the shopper's mandates: the Credential Provider's or the agent provider's, against which
  it verifies the mandate's issuer signature, key binding and delegation chain (see what is not checked, below).

Kontra plays none of these roles, and the gate is never a party to the money.

## What it does

The card declares one extension, **required**:

```json
{ "uri": "https://github.com/google-agentic-commerce/ap2/v1", "required": true, "params": { "roles": ["merchant"] }, "description": "…" }
```

- A `SendMessage` or `SendStreamingMessage` that does not activate AP2 (`A2A-Extensions`, or its 0.3 name
  `X-A2A-Extensions`) is refused `ExtensionSupportRequiredError` (JSON-RPC `-32008`). `GetTask`, `CancelTask` and the
  rest need no activation. Behind a gate the gate refuses it first: the served card marks AP2 required.
- The first message of a task, whatever it says, stops at `input-required` with a text and an `ap2.checkout` data
  part, as AP2's Python merchant writes it:

  ```json
  { "ap2.checkout": { "checkout_id": "…", "checkout_jwt": "eyJ…", "checkout_hash": "…", "item_label": "Example item", "amount_cents": 1000, "currency": "USD" } }
  ```

  `checkout_jwt` is a JWS (ES256, `kid` the key's RFC 7638 thumbprint) over a UCP-shaped Checkout (`id`, `merchant`,
  `line_items`, `totals`, `currency`, `status: incomplete`, `iat`, `exp` one hour later), signed with the merchant's
  ECDSA P-256 key; `checkout_hash` is the base64url SHA-256 of the `checkout_jwt` string. The public key is served at
  `/.well-known/jwks.json` (directly, not through a gate, which relays the card and A2A only).
- A later message to the same task with a data part `{"ap2.mandates.CheckoutMandateSdJwt": "<SD-JWT>"}` is checked
  (below) and answered with a data part `{"ap2.CheckoutReceipt": "<JWT>"}`: a Checkout Receipt
  (`code/sdk/schemas/ap2/checkout_receipt.json`) signed with the same key, whose `reference` is the base64url SHA-256
  of the closed mandate's issuer-signed JWT (as AP2's SDK computes it, `get_closed_mandate_jwt`). `status: Success`
  with an `order_id` **completes** the task; `status: Error` with `error` and `error_description` leaves it at
  `input-required`, so a better mandate can follow (AP2: "the Merchant MUST return a Checkout Receipt JWT containing
  the appropriate error message").
- A later message without a mandate asks for one again. `CancelTask` abandons the checkout (`canceled`).
- Tasks live in memory; without `--key` the key is new at each start, so a restarted merchant honours none of the
  checkouts it issued.

The data-part keys are those of AP2 v0.2.0's Python samples (`code/samples/python/src/common/constants.py`,
`roles/merchant_agent/tools.py`), except `ap2.CheckoutReceipt`: the samples return the checkout receipt only over MCP,
so this example names it after their `ap2.PaymentReceipt`. AP2 leaves transport to the commerce protocol, and its Go
samples still use v0.1 keys: a shopper and a merchant agree on keys between themselves. The gate keys on none of them.

### The mandate check, and what it does not check

The check follows docs/ap2/specification.md "Verification / Merchant" as far as an example can:

1. The closed mandate is the last SD-JWT of the presentation (after the last `~~` of a delegation chain). It is read
   as an SD-JWT (RFC 9901): its payload with every presented disclosure put back whose digest the payload, or a
   disclosure it includes, lists; `_sd_alg` must be `sha-256` or absent; a disclosure presented or referenced twice
   is refused.
2. A disclosed object with `vct: mandate.checkout.1` must hold `checkout_jwt` and `checkout_hash`, and
   `checkout_hash` must be the base64url SHA-256 of `checkout_jwt` (docs/ap2/checkout_mandate.md "Mandate Schema").
3. `checkout_jwt` must be byte for byte the checkout this task issued, signed by this merchant's key and not expired;
   the mandate's own `exp`, when present, must not have passed.

Error codes: `invalid_mandate`, `checkout_hash_mismatch`, `unknown_checkout`, `checkout_expired`, `mandate_expired`.

It does **not** verify the mandate's issuer signature, its key-binding JWT (`sd_hash`, `aud`, `nonce`) or its
delegation chain (`cnf`), nor evaluate an open mandate's constraints: those need the keys of the Credential Provider
and the shopping agent, which a real merchant gets from the parties it works with, and AP2's SDK
(`code/sdk/python/ap2/sdk`) implements. A merchant that ships must add them.

```
ap2-merchant [--host 127.0.0.1] [--port 9993] [--key merchant.pem]
```

It serves its card at `/.well-known/agent-card.json`, JSON-RPC at its root (`http://127.0.0.1:9993`) and HTTP+JSON
under `/rest`, all under one root URL, which is what `kontra connect --serve` needs. `--key` is a PEM P-256 private
key, SEC 1 or PKCS #8 (`openssl ecparam -name prime256v1 -genkey -noout -out merchant.pem`). `--port 0` takes any
free port; the first line on standard output is the root URL; `SIGINT` or `SIGTERM` stops it.

## Talk to it directly

```sh
go run ./examples/sellers/ap2-merchant &          # http://127.0.0.1:9993

ap2() { curl -s http://127.0.0.1:9993 -H 'Content-Type: application/json' -H 'A2A-Version: 1.0' \
  -H 'A2A-Extensions: https://github.com/google-agentic-commerce/ap2/v1' "$@"; }

ap2 -d '{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER",
  "parts":[{"text":"I would like the example item"}]}}}'
#  … "state":"TASK_STATE_INPUT_REQUIRED" … {"data":{"ap2.checkout":{"checkout_jwt":"eyJ…", …}}}

# The shopper's side builds the Checkout Mandate over that checkout_jwt (AP2's SDK, or main_test.go's shopper), then:
ap2 -d '{"jsonrpc":"2.0","id":2,"method":"SendMessage","params":{"message":{"messageId":"m2","role":"ROLE_USER",
  "taskId":"<task id>","parts":[{"data":{"ap2.mandates.CheckoutMandateSdJwt":"<SD-JWT>"}}]}}}'
#  … "state":"TASK_STATE_COMPLETED" … "Checkout … is confirmed as order order_…" {"data":{"ap2.CheckoutReceipt":"eyJ…"}}
```

Without the `A2A-Extensions` header the first call answers `{"error":{"code":-32008,…}}`.

## Through the tunnel

`kontra.yaml` here is a team gate on one machine (the gate on `127.0.0.1:8080`, Postgres and Temporal from the
repository's `compose.dev.yaml`) with one tunnel service, `ap2-merchant`. From the repository root:

```sh
docker compose -f compose.dev.yaml up -d --wait
go build -o kontra-gate ./cmd/kontra-gate && go build -o kontra ./cmd/kontra && go build -o ap2-merchant ./examples/sellers/ap2-merchant

./kontra-gate migrate -f examples/sellers/ap2-merchant/kontra.yaml
export KONTRA_API_KEY="$(./kontra-gate enroll -f examples/sellers/ap2-merchant/kontra.yaml --api-key demo)"
./kontra-gate serve -f examples/sellers/ap2-merchant/kontra.yaml &

./ap2-merchant --key merchant.pem &                         # http://127.0.0.1:9993
./kontra connect --gate http://localhost:8080 --serve a2a:ap2-merchant=http://127.0.0.1:9993 \
  --enroll "$(./kontra-gate enroll -f examples/sellers/ap2-merchant/kontra.yaml ap2-merchant)" &
#    a2a ap2-merchant  http://localhost:8080/agents/ap2-merchant
```

Then the same two calls as above against `http://localhost:8080/agents/ap2-merchant`, with
`-H "Authorization: Bearer $KONTRA_API_KEY"`. The gate's card at `/agents/ap2-merchant/.well-known/agent-card.json`
carries the AP2 entry as the merchant wrote it, `required` and `params.roles` included; the response echoes the
activation in `A2A-Extensions`; the checkout and the mandate cross the gate unchanged. With `kontra-sealed.yaml`
(and `kontra connect` on both sides, examples/sealing/README.md) every part, the mandate included, is sealed end to
end: the gate's task log holds only the sealed envelope.

`/mcp`'s `hire` cannot carry AP2: it activates no extension of the seller's (D328), and this merchant's card
requires one, so `hire` of it is a tool error naming the A2A URL and the AP2 URI (D294). Hire it over A2A.

`test/e2e` `TestAP2Passthrough` (`ap2_passthrough`) runs this merchant through the tunnel, plain and sealed.
