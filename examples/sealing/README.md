# sealing

End-to-end sealing (M3, [`spec/ext-sealed.md`](../../spec/ext-sealed.md)): the hiring node seals every part a local
client sends to a sealed service, to the keys enrolled for that service; the hosting node opens them before the
service sees anything and seals what the service answers to a key only the hiring node holds. The gate stores and
relays ciphertext it cannot open: Postgres, its logs and its bucket hold envelopes, never a plaintext part
(`sealed_no_plaintext`). Neither the service nor the client handles a key: both are unmodified.

What is sealed (§2): the content of every A2A part (`text`, `raw`, `url` or `data`), both directions; for MCP the
`tools/call` arguments, the result's `content` and `structuredContent`, and, for a task a sealed call created, its
upstream elicitation and sampling requests, the client's answers, and its `statusMessage` (which becomes
`"sealed"`). What is not: task states and ids, message and artifact ids, metadata, a part's `filename` (do not put
secrets in it), tool names, `isError`.

## Run it on one machine

The gate is `examples/sellers/hello-exec/kontra-sealed.yaml` (`sealed: true` on a tunnel service; a sealed service
needs a tunnel backend, D91, and an upstream card with an A2A 1.0 interface, D165). The seller is hello-exec's. Two
`kontra connect` processes play the two nodes; each has its own identity key, so each gets its own `HOME`.

```sh
docker compose -f compose.dev.yaml up -d --wait
go build -o kontra-gate ./cmd/kontra-gate && go build -o kontra ./cmd/kontra
CFG=examples/sellers/hello-exec/kontra-sealed.yaml
./kontra-gate migrate -f $CFG
export KONTRA_API_KEY="$(./kontra-gate enroll -f $CFG --api-key demo)"
./kontra-gate serve -f $CFG &

examples/sellers/hello-exec/run.sh &                        # the seller on 127.0.0.1:9999

# The hosting node: enrolled for hello, so its key is one of the service's recipients.
HOME=/tmp/seller ./kontra connect --gate http://localhost:8080 --listen 127.0.0.1:7781 \
  --enroll "$(./kontra-gate enroll -f $CFG hello)" --serve a2a:hello=http://127.0.0.1:9999 &

# The hiring node: it hires with the API key, and seals for its local clients on 127.0.0.1:7777.
HOME=/tmp/buyer ./kontra connect --gate http://localhost:8080 --api-key "$KONTRA_API_KEY" &
#  -> kontra connect: pins: hello at http://localhost:8080 first seen: sealed true, recipients k1_…, seller keys none

a2a send -a http://localhost:7777/agents/hello "a secret"
#  -> completed, "Hello World": the client never sees an envelope
psql postgres://kontra:kontra@localhost:5432/kontra -c "select payload from kontra.task_events"
#  -> every part is {"raw": "<age ciphertext>", "mediaType": "application/vnd.kontra.sealed+age;v=1", ...}
```

An unsealed message straight to the gate is refused before any task exists (`sealed_unsealed_part`):

```sh
a2a send -a http://localhost:8080/agents/hello --auth "Bearer $KONTRA_API_KEY" "plain"
#  -> InvalidRequestError (-32600): this service is sealed
```

The hiring node keeps a **pin** per (gate, service) in `~/.kontra/pins` (§7): whether the service is sealed, its
recipients' key ids, and the seller keys that verified its upstream card. A downgrade (sealed to unsealed, signed
to unsigned) is always refused. A new recipient (a second hosting node enrolled for `hello`) is refused until the
operator accepts it: `kontra connect … --accept-change hello`; `--pin-recipients` refuses every new one. Both print
the key ids and thumbprints on stderr and in the log, for comparison out of band (D164, D187).

The hosting node keeps its job state under `~/.kontra/hosted/<gate host>/<service>/` (0600): which reply key opened
which job, the pairs it forwarded, the input requests it sealed. It refuses a replayed or reordered part itself
(`400`, `Kontra-Node: sealed_rejected`, `Kontra-Sealed-Reject: replayed`, §6); the gate fails the task
`sealed_rejected` and writes an audit row. `--sealed-max-age` (default 25 h) bounds how old a sealed request may be:
it must cover the longest quote expiry plus `task.timeout`. `--sealed-required hello` makes the hosting node refuse
an unsealed request even if a gate let one through.

MCP works the same way between two `kontra connect`: a service with `sealed:` gets the extension in its
`initialize` result under `capabilities.experimental["sh.kontra/sealed"]`, and the hiring node seals `tools/call`
arguments (`arguments: {}`, the envelope in `_meta["sh.kontra/sealed"]`). A stock MCP client talking straight to
the gate sees no envelope, and a required sealed service refuses its calls. Sealed MCP services are served to
2025-11-25 and 2025-06-18 clients only (D162).

## Without a node: `kontra seal`

The escape hatch for scripting (§5.1, PRD §8), not the path:

```sh
./kontra seal --service http://localhost:8080/agents/hello note.txt > message.json
#  -> one A2A Message (role user, a fresh messageId, a contextId) whose one part is note.txt sealed to the
#     service's recipients, with metadata["kontra.sealed.replyTo"] = a per-job key kept in ~/.kontra/jobs/<gate>/
curl -s http://localhost:8080/agents/hello -H "Authorization: Bearer $KONTRA_API_KEY" -H 'A2A-Version: 1.0' \
  -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"SendMessage\",\"params\":{\"message\":$(cat message.json)}}" > answer.json
./kontra seal --open answer.json        # the plaintext of every reply part a key of ~/.kontra/jobs opens
```

`kontra seal` verifies the gate's signature on the card (with the key set at `<gate>/.well-known/jwks.json`) and
checks the pin, as `kontra connect` does; a card that does not verify is refused.

## Sealing in-process

There is no SDK: reproduce §3 exactly. A request part is an [age](https://age-encryption.org/v1) file (binary, X25519
recipients) of `JCS(header) || 0x00 || body`. The header for one text part of a new task's first message:

```json
{"contextId":"<the message's contextId>","dir":"req","iat":<unix seconds>,"kids":["k1_…","k1_…"],"kind":"text",
 "mediaType":"","messageId":"<the message's messageId>","part":0,"parts":1,"replyTo":"age1<your per-job key>",
 "rk":"<32 random bytes, base64url without padding>","v":1}
```

`kids` are the recipients' key ids, sorted: `"k1_" + lowercase(base32_nopad(SHA-256(utf8(recipient))))[0:26]`
([`spec/tunnel.md`](../../spec/tunnel.md) §2.2). A follow-up adds `taskId`. The header must be RFC 8785 JCS (its
members sorted, no white space, ASCII only): receivers re-serialize it and compare bytes. The part is
`{"raw": "<base64 of the age file>", "mediaType": "application/vnd.kontra.sealed+age;v=1", "metadata":
{"kontra.sealed": {"original": "<the original mediaType, or the kind>"}}}`, and the message's metadata carries
`"kontra.sealed.replyTo": "<the same replyTo>"`.

Go, with [`filippo.io/age`](https://pkg.go.dev/filippo.io/age) v1.3.2:

```go
var recipients []age.Recipient
for _, r := range card.Recipients { // the card's capabilities.extensions[uri=https://kontra.sh/ext/sealed/v1].params.recipients
	x, err := age.ParseX25519Recipient(r)
	if err != nil { return err }
	recipients = append(recipients, x)
}
job, _ := age.GenerateX25519Identity() // keep it: replies are sealed to job.Recipient()
var out bytes.Buffer
w, err := age.Encrypt(&out, recipients...)
if err != nil { return err }
w.Write(headerJCS) // the header above, already in JCS form
w.Write([]byte{0})
w.Write(body)
if err := w.Close(); err != nil { return err }
raw := base64.StdEncoding.EncodeToString(out.Bytes())
```

Python, with [`pyrage`](https://pypi.org/project/pyrage/):

```python
import base64, json, os, time
from pyrage import encrypt, decrypt, x25519

recipients = [x25519.Recipient.from_str(r) for r in card_recipients]
job = x25519.Identity.generate()            # keep it; job.to_public() is the replyTo
rk = os.urandom(32)
header = {"v": 1, "dir": "req", "iat": int(time.time()), "contextId": ctx, "messageId": msg_id,
          "part": 0, "parts": 1, "kind": "text", "mediaType": "", "kids": sorted(kids),
          "replyTo": str(job.to_public()), "rk": base64.urlsafe_b64encode(rk).rstrip(b"=").decode()}
hdr = json.dumps(header, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()  # JCS for these values
part = {"raw": base64.b64encode(encrypt(hdr + b"\x00" + body, recipients)).decode(),
        "mediaType": "application/vnd.kontra.sealed+age;v=1", "metadata": {"kontra.sealed": {"original": "text"}}}
```

(`json.dumps` with sorted keys and no white space is JCS for strings, integers, lists and objects; it is not for
floats or non-ASCII strings, which a header never holds.)

A reply part opens with the per-job key: decrypt the whole file, split at the first `0x00`, check that the header is
canonical, `dir` is `"res"`, its `kids` hold your key's id and its `taskId`, `contextId` and `messageId` (or
`artifactId`) are the reply's; then check `auth`, `base64url(HMAC-SHA256(rk, JCS(header without auth) || 0x00 ||
body))`, which only the holder of `rk` (you and the hosting node) can compute:

```python
plain = decrypt(base64.b64decode(reply_part["raw"]), [job])
hdr, body = plain.split(b"\x00", 1)
header = json.loads(hdr)
auth = header.pop("auth")
mac = hmac.new(rk, json.dumps(header, sort_keys=True, separators=(",", ":")).encode() + b"\x00" + body, "sha256").digest()
assert hmac.compare_digest(base64.urlsafe_b64decode(auth + "=" * (-len(auth) % 4)), mac)
```

A gate can still write its own failure text on a sealed task (a lost tunnel, a timeout): that is the one unsealed
part a hiring node delivers, marked `metadata["kontra.sealed"] = false` (§5 rule 4a).
