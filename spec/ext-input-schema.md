# Input schema extension `https://kontra.sh/ext/input-schema/v1`

Status: **draft** (M0). Open source, optional.

A2A 1.0 has no input schema. This extension lets an agent say what its first message's `data` part
should look like, so a client can render a typed form and validate before sending. Agents and clients
that ignore it still work with free-form text. MCP needs no equivalent: tools carry `inputSchema`
natively.

## 1. Card entry

Seller-authored, in the agent's own card (no Kontra code: it is a JSON entry in the card the agent
already serves, e.g. through `a2a server --card card.json`). The gate passes it through unchanged
([`relay.md`](relay.md) §3).

```json
{
  "uri": "https://kontra.sh/ext/input-schema/v1",
  "description": "JSON Schema for the first message's data part, for form rendering.",
  "required": false,
  "params": {
    "schema": {
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object",
      "properties": { "topic": { "type": "string" }, "length": { "type": "integer", "minimum": 1 } },
      "required": ["topic"]
    },
    "mediaType": "application/json"
  }
}
```

| Param | Rule |
|---|---|
| `schema` | A JSON Schema 2020-12 document, at most 64 KiB serialized. `$ref` only to `#/…` (no remote references). |
| `mediaType` | Optional, default `application/json`: the `mediaType` the data part should carry. |

`required` SHOULD be `false`: the schema describes the agent's preferred input, not a protocol
requirement.

## 2. Use

- **Clients** (`kontra connect` on :7777, the Cloud console) that see the entry render a form from
  `schema` and send the first message with one `data` part (`{"data": <object>, "mediaType":
  <mediaType>}`), optionally followed by text parts. They validate the object against the schema before
  sending and refuse to send an invalid one, with the validation error shown to the person.
- **The gate does not validate.** It relays parts it does not interpret, and with a sealed service it
  cannot read them. Validation is the agent's job and the client's courtesy.
- **Follow-up messages** (after `input-required`) are not covered by the schema.

## 3. Versioning

A new major version of the extension gets a new URI (`…/input-schema/v2`). Changes to an agent's schema
are the agent's own versioning, visible through the card hash the gate tracks.
