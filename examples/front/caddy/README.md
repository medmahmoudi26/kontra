# front/caddy

Caddy in front of a Kontra gate: Caddy terminates TLS (a certificate from an ACME CA for a public name, from its own
local CA for a loopback or private address) and proxies every request to the gate's plaintext listener (spec/relay.md
§2.2). [`Caddyfile`](Caddyfile) is the whole front; `make tck VARIANT=caddy` runs the pinned A2A TCK through it
unchanged (`tck_behind_caddy`, D279; [`test/tck`](../../../test/tck/README.md)). Pinned: Caddy 2.11.7 (the release
tarball's sha256 is in `.github/workflows/tck.yml`).

## The gate's side

```yaml
listen: 127.0.0.1:8080                  # plaintext, reachable from Caddy only
public_url: https://gateway.example.com # what callers and nodes use: Caddy's site
tls: { terminated_by_proxy: true }      # plaintext behind an https public_url (D69); the gate logs a warning
edge:
  client_ip: { from: x_forwarded_for, trusted: ["127.0.0.1/32"] }
```

- `tls.terminated_by_proxy` lets the gate listen in plaintext with an `https` `public_url`. HSTS is then Caddy's to
  send (add `header Strict-Transport-Security "max-age=63072000"` to the site if you want it), and the tunnel's
  channel binding is `none`: the gate cannot bind a node's proof to a TLS session it does not terminate (spec/tunnel.md
  §4). A node's `--gate` is still the `public_url`.
- `edge.client_ip` makes `rps_per_ip` and `conns_per_ip` count callers, not Caddy: on Caddy's connections only, the
  client address is the rightmost `X-Forwarded-For` hop that is not trusted. Caddy's default for the header is the
  right one: it sets it to the client's address and drops what an untrusted client sent. `trusted` names Caddy's
  address as the gate sees it; on another machine, that machine's.

## Run it

```sh
KONTRA_FRONT_SITE=gateway.example.com KONTRA_GATE=127.0.0.1:8080 caddy run --config examples/front/caddy/Caddyfile
```

| Variable | What | Default |
|---|---|---|
| `KONTRA_FRONT_SITE` | the site: the `public_url` host, or a full `https://host:port` | (required) |
| `KONTRA_GATE` | the gate's `listen` address | (required) |
| `KONTRA_CADDY_ADMIN` | Caddy's admin API address, or `off` | `localhost:2019` |
| `KONTRA_CADDY_BIND` | the addresses Caddy listens on | every interface |
| `KONTRA_CADDY_HTTP_PORT`, `KONTRA_CADDY_HTTPS_PORT` | Caddy's ports (80 also answers ACME HTTP challenges and redirects to HTTPS) | 80, 443 |

What the file sets on the proxy, and why:

- `flush_interval -1`: server-sent events (`message:stream`, `SubscribeToTask`, the MCP GET streams) and long polls
  pass every byte on at once. The gate sends a `: keep-alive` comment every 15 s, so idle streams are not cut.
- `stream_close_delay 5m`: the tunnel is a WebSocket (`GET /v1/tunnel`) a node keeps open for hours. On a config
  reload Caddy closes upgraded connections at once by default; with the delay, nodes reconnect over five minutes
  instead of all at the same moment.
- `skip_install_trust`: Caddy's local CA, used for a loopback or private address, is never installed in the
  machine's trust store. The TCK run trusts it through `SSL_CERT_FILE` instead.

The TCK run (`scripts/tck.sh`, VARIANT=caddy) sets `KONTRA_FRONT_SITE=https://127.0.0.1:<port>`,
`KONTRA_CADDY_ADMIN=off`, `KONTRA_CADDY_BIND=127.0.0.1`, free ports, and `XDG_DATA_HOME`/`XDG_CONFIG_HOME` under its
run directory, so the local CA and its certificates are removed with the run.
