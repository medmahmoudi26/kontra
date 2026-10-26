package tunnel

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/unimpl"
)

// yamux configuration, identical on both sides (§7).
const (
	AcceptBacklog          = 64               // bounds streams awaiting Accept and what a hostile peer pins before it
	KeepAliveInterval      = 20 * time.Second // must be non-zero although yamux's keepalive is off (D33)
	ConnectionWriteTimeout = 15 * time.Second // also Ping()'s pong deadline
	MaxStreamWindowSize    = 256 << 10        // the default: a slow SSE reader stalls only its own stream
	StreamOpenTimeout      = 30 * time.Second // an unacknowledged open closes the whole session
	StreamCloseTimeout     = 5 * time.Minute
)

// YamuxConfig is §7's table, logging through logger at Warn (yamux needs exactly one of LogOutput and
// Logger): EnableKeepAlive false, because yamux closes on the first missed pong; liveness is the ping loop
// of liveness.go.
func YamuxConfig(logger *slog.Logger) *yamux.Config {
	panic(unimpl.Error("tunnel.YamuxConfig", "WP-03"))
}

// NetConn is websocket.NetConn over binary messages, after the handshake; compression stays off (§3, §7).
func NetConn(ctx context.Context, c *websocket.Conn) net.Conn {
	panic(unimpl.Error("tunnel.NetConn", "WP-03"))
}

// ServerSession starts the gate's yamux.Server on c (§1, §7).
func ServerSession(ctx context.Context, c *websocket.Conn, logger *slog.Logger) (*yamux.Session, error) {
	return nil, unimpl.Error("tunnel.ServerSession", "WP-03")
}

// ClientSession starts the node's yamux.Client on c (§1, §7).
func ClientSession(ctx context.Context, c *websocket.Conn, logger *slog.Logger) (*yamux.Session, error) {
	return nil, unimpl.Error("tunnel.ClientSession", "WP-03")
}

// CloseStatusOf reads the handshake code a close error carries (status 4000 + i), for the node's retry
// decision (§4).
func CloseStatusOf(err error) (Code, bool) {
	panic(unimpl.Error("tunnel.CloseStatusOf", "WP-03"))
}
