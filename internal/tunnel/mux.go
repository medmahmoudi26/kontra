package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
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

// YamuxConfig is §7's table, logging through YamuxLogger(logger) (yamux needs exactly one of LogOutput and
// Logger): EnableKeepAlive false, because yamux closes on the first missed pong; liveness is the ping loop
// of liveness.go. A nil logger discards yamux's lines.
//
// The configuration is per session when its lines must say which session they are about: logger then
// carries the session's attributes (the gate's Handler does so).
func YamuxConfig(logger *slog.Logger) *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = AcceptBacklog
	c.EnableKeepAlive = false
	c.KeepAliveInterval = KeepAliveInterval
	c.ConnectionWriteTimeout = ConnectionWriteTimeout
	c.MaxStreamWindowSize = MaxStreamWindowSize
	c.StreamOpenTimeout = StreamOpenTimeout
	c.StreamCloseTimeout = StreamCloseTimeout
	c.LogOutput = nil
	c.Logger = YamuxLogger(logger)
	return c
}

// YamuxLogger is yamux's Logger writing through logger: yamux writes every line with one level prefix
// ("[ERR] yamux: …", "[WARN] yamux: …"), and they all reach logger's handler at Warn, with
// component=yamux, so logger should carry no component of its own. A nil logger discards yamux's lines.
func YamuxLogger(logger *slog.Logger) *log.Logger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return slog.NewLogLogger(logger.With("component", "yamux").Handler(), slog.LevelWarn)
}

// NetConn is websocket.NetConn over binary messages, after the handshake; compression stays off (§3, §7):
// it is negotiated at the upgrade, and neither side offers it. ctx bounds the connection's life: when it
// ends, every read and write fails.
//
// Closing it closes the WebSocket with StatusNormalClosure, and a peer's StatusNormalClosure or
// StatusGoingAway reads as io.EOF, so yamux ends without logging "[ERR] yamux: Failed to read header"
// (research tunnel-crypto §3.5.8). Each yamux frame is two WebSocket messages (header, then body).
func NetConn(ctx context.Context, c *websocket.Conn) net.Conn {
	return &quietConn{Conn: websocket.NetConn(ctx, c, websocket.MessageBinary), ctx: ctx}
}

// quietConn keeps the ends of a session out of yamux's error log, which takes every read error but io.EOF
// and those that say "closed" or "reset by peer" for "[ERR] yamux: Failed to read header":
//   - a read that fails because this side closed the connection, or ended its context, is net.ErrClosed
//     (websocket.NetConn's Close cancels its own pending read first: "context canceled");
//   - a connection that ended without a close frame (a killed peer: "failed to read frame header: EOF")
//     is io.EOF, as one closed with StatusNormalClosure is. The session's owner logs its end.
type quietConn struct {
	net.Conn
	ctx    context.Context
	closed atomic.Bool
}

func (c *quietConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	switch {
	case err == nil || err == io.EOF:
	case c.closed.Load() || c.ctx.Err() != nil:
		err = net.ErrClosed
	case errors.Is(err, io.EOF):
		err = io.EOF
	}
	return n, err
}

func (c *quietConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// ServerSession starts the gate's yamux.Server on c (§1, §7) with YamuxConfig(logger).
func ServerSession(ctx context.Context, c *websocket.Conn, logger *slog.Logger) (*yamux.Session, error) {
	return ServerSessionWith(ctx, c, YamuxConfig(logger))
}

// ClientSession starts the node's yamux.Client on c (§1, §7) with YamuxConfig(logger).
func ClientSession(ctx context.Context, c *websocket.Conn, logger *slog.Logger) (*yamux.Session, error) {
	return ClientSessionWith(ctx, c, YamuxConfig(logger))
}

// ServerSessionWith is ServerSession with a configuration of the caller's (tests shorten its durations).
// The configuration must keep yamux's keepalive off (D33), and c must have negotiated Subprotocol.
func ServerSessionWith(ctx context.Context, c *websocket.Conn, cfg *yamux.Config) (*yamux.Session, error) {
	if err := checkMux(c, cfg); err != nil {
		return nil, err
	}
	s, err := yamux.Server(NetConn(ctx, c), cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnel: yamux server: %w", err)
	}
	return s, nil
}

// ClientSessionWith is ClientSession with a configuration of the caller's (tests shorten its durations).
// The configuration must keep yamux's keepalive off (D33), and c must have negotiated Subprotocol.
func ClientSessionWith(ctx context.Context, c *websocket.Conn, cfg *yamux.Config) (*yamux.Session, error) {
	if err := checkMux(c, cfg); err != nil {
		return nil, err
	}
	s, err := yamux.Client(NetConn(ctx, c), cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnel: yamux client: %w", err)
	}
	return s, nil
}

// checkMux refuses what §7 and D33 rule out before yamux takes the connection over.
func checkMux(c *websocket.Conn, cfg *yamux.Config) error {
	switch {
	case c == nil:
		return errors.New("tunnel: no WebSocket connection")
	case cfg == nil:
		return errors.New("tunnel: no yamux configuration")
	case cfg.EnableKeepAlive:
		return errors.New("tunnel: yamux keepalive must be off: liveness is the ping loop (D33)")
	case c.Subprotocol() != Subprotocol:
		return fmt.Errorf("tunnel: the WebSocket did not negotiate %s", Subprotocol)
	}
	return nil
}

// CloseStatusOf reads the handshake code a close error carries (status 4000 + i), for the node's retry
// decision (§4). It is false for an error that carries no close frame, or a close status outside the
// codes' range.
func CloseStatusOf(err error) (Code, bool) {
	s := websocket.CloseStatus(err)
	if s < 0 {
		return "", false
	}
	return CodeOfStatus(int(s))
}
