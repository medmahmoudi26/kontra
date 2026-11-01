package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"sync"
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
//
// The lines are rate-limited per logger, so per session (LogBurst, LogRefill): a peer can make yamux log a
// line for every 12-byte frame it sends (M2 review, security-2).
func YamuxLogger(logger *slog.Logger) *log.Logger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return slog.NewLogLogger(newLimitHandler(logger.With("component", "yamux").Handler()), slog.LevelWarn)
}

// NetConn is websocket.NetConn over binary messages, after the handshake; compression stays off (§3, §7):
// it is negotiated at the upgrade, and neither side offers it. ctx bounds the connection's life: when it
// ends, every read and write fails.
//
// Closing it closes the WebSocket with StatusNormalClosure, and a peer's StatusNormalClosure or
// StatusGoingAway reads as io.EOF, so yamux ends without logging "[ERR] yamux: Failed to read header"
// (research tunnel-crypto §3.5.8). Each yamux frame is two WebSocket messages (header, then body).
//
// Each write has ConnectionWriteTimeout: a write still blocked then fails and closes the connection.
func NetConn(ctx context.Context, c *websocket.Conn) net.Conn {
	return netConn(ctx, c, ConnectionWriteTimeout)
}

// netConn is NetConn whose writes each have writeTimeout (none when it is not positive). yamux's send loop
// is the connection's only writer, and it writes with no deadline of its own: a peer that stops reading
// would hold it, and every frame queued behind it, until the session ends some other way. With the deadline
// the loop fails within writeTimeout and the session closes, as yamux's own stream writes and pings give
// up after the same time (ConnectionWriteTimeout; M2 review, security-1).
func netConn(ctx context.Context, c *websocket.Conn, writeTimeout time.Duration) *quietConn {
	return &quietConn{Conn: websocket.NetConn(ctx, c, websocket.MessageBinary), ws: c, ctx: ctx, writeTimeout: writeTimeout}
}

// quietConn keeps the ends of a session out of yamux's error log, which takes every read error but io.EOF
// and those that say "closed" or "reset by peer" for "[ERR] yamux: Failed to read header":
//   - a read that fails because this side closed the connection, or ended its context, is net.ErrClosed
//     (websocket.NetConn's Close cancels its own pending read first: "context canceled");
//   - a connection that ended without a close frame (a killed peer: "failed to read frame header: EOF")
//     is io.EOF, as one closed with StatusNormalClosure is. The session's owner logs its end.
type quietConn struct {
	net.Conn
	ws           *websocket.Conn
	ctx          context.Context
	writeTimeout time.Duration
	closed       atomic.Bool

	handshake atomic.Bool                   // CloseGracefully: Close runs the WebSocket close handshake
	session   atomic.Pointer[yamux.Session] // the session over it: its key in conns
}

// Write writes p as one WebSocket message within writeTimeout. websocket.NetConn closes the connection
// when a deadline passes during a write; the deadline is cleared after each write, since one that passes
// between writes would fail the next write too.
func (c *quietConn) Write(p []byte) (int, error) {
	if c.writeTimeout <= 0 {
		return c.Conn.Write(p)
	}
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	n, err := c.Conn.Write(p)
	_ = c.Conn.SetWriteDeadline(time.Time{})
	return n, err
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

// Close closes the connection at once (websocket.NetConn's Close ends the read in progress, and closing the
// WebSocket then closes the TCP connection under it), or with the close handshake after CloseGracefully.
func (c *quietConn) Close() error {
	c.closed.Store(true)
	if s := c.session.Load(); s != nil {
		conns.Delete(s)
	}
	if !c.handshake.Load() {
		return c.Conn.Close()
	}
	// The close frame goes out behind every frame written before it. The read in progress (yamux's) goes on,
	// so what the peer still sends is read and dropped (yamux is shut down) and its writes meet an open
	// connection, until its answer to the close frame ends that read; then the TCP connection closes.
	err := c.ws.Close(websocket.StatusNormalClosure, "")
	_ = c.Conn.Close() // websocket.NetConn's timers and contexts: the WebSocket is closed already
	return err
}

// conns is the connection under each session ServerSessionWith or ClientSessionWith started, until it closes
// (CloseGracefully).
var conns sync.Map // *yamux.Session → *quietConn

func track(s *yamux.Session, c *quietConn) {
	c.session.Store(s)
	conns.Store(s, c)
	if c.closed.Load() { // closed before it was stored: Close found no key, or nothing under it
		conns.Delete(s)
	}
}

// CloseGracefully closes s, a session ServerSession or ClientSession started, as its Close does (no stream
// opens, every stream ends), except that the WebSocket under it closes with the close handshake: the close
// frame goes out after every frame written before it, and the connection stays open, what the peer still sends
// read and dropped, until the peer answers that frame or the WebSocket library's bound passes (5 s; 5 s more
// while the frame cannot be written). A peer's WebSocket answers it once its reader reaches it, so once its
// yamux has read every frame written before it.
//
// Close instead ends the connection at once, and a peer still writing on it (an answer on a stream this side
// aborted) meets a reset, which can end the peer's session before it reads the last frames this side wrote:
// the {"t": "revoked"} line of a revoked key's session was lost so (spec/tunnel.md §12; issue #12). Use it when
// those frames matter. It returns when the connection is closed. On a session already closing it waits for that
// close, as Close does, and changes nothing.
func CloseGracefully(s *yamux.Session) error {
	if c, ok := conns.Load(s); ok {
		c.(*quietConn).handshake.Store(true)
	}
	return s.Close()
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
//
// The gate's session reads the node's frames through a ping budget (PingBurst, PingRefill): a node that
// sends more ping SYNs ends its session (ErrPingFlood). Each write to the connection has the
// configuration's ConnectionWriteTimeout.
func ServerSessionWith(ctx context.Context, c *websocket.Conn, cfg *yamux.Config) (*yamux.Session, error) {
	if err := checkMux(c, cfg); err != nil {
		return nil, err
	}
	qc := netConn(ctx, c, cfg.ConnectionWriteTimeout)
	s, err := yamux.Server(newPingGuard(qc), cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnel: yamux server: %w", err)
	}
	track(s, qc)
	return s, nil
}

// ClientSessionWith is ClientSession with a configuration of the caller's (tests shorten its durations).
// The configuration must keep yamux's keepalive off (D33), and c must have negotiated Subprotocol. Each
// write to the connection has the configuration's ConnectionWriteTimeout.
func ClientSessionWith(ctx context.Context, c *websocket.Conn, cfg *yamux.Config) (*yamux.Session, error) {
	if err := checkMux(c, cfg); err != nil {
		return nil, err
	}
	qc := netConn(ctx, c, cfg.ConnectionWriteTimeout)
	s, err := yamux.Client(qc, cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnel: yamux client: %w", err)
	}
	track(s, qc)
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
