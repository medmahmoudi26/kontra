package node

import (
	"context"
	"net/url"

	"github.com/coder/websocket"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/unimpl"
)

// Dialed is an upgraded connection before the handshake.
type Dialed struct {
	WS      *websocket.Conn
	Origin  string // tunnel.CanonicalOrigin of what was dialed: hello.origin
	CBValue []byte // the TLS exporter of this connection; nil over plain http (localhost)
}

// Dial opens GET {gate}/v1/tunnel with Upgrade: websocket, Sec-WebSocket-Protocol: kontra-tunnel.v1 and
// User-Agent: kontra/<version> (<os>/<arch>), no credential in any header (§3). It closes a connection on
// which the gate did not select the subprotocol, and sets the 16 KiB handshake read limit.
func Dial(ctx context.Context, gate *url.URL, userAgent string) (*Dialed, error) {
	return nil, unimpl.Error("node.Dial", "WP-03")
}

// Handshake is the node's half (§4): send hello (its exact bytes kept for the transcript), read the
// challenge, refuse cb none when requireCB, prove (and prove the old key for rotate_from), read welcome or
// error. A refusal is a *tunnel.HandshakeError.
func Handshake(ctx context.Context, d *Dialed, id *nodekey.Identity, hello *tunnel.Hello, requireCB bool) (*tunnel.Welcome, error) {
	return nil, unimpl.Error("node.Handshake", "WP-03")
}
