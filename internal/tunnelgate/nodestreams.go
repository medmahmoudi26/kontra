package tunnelgate

import (
	"context"
	"io"
	"net"
	"net/http"

	"kontra.sh/kontra/internal/unimpl"
)

// NodeStreams serves the streams a node opens (spec/tunnel.md §9, §8.3), replacing M2's refuseNodeStream:
//
//   - op http (hiring): one HTTP/1.1 request read from the stream is served by Hire, the gate's own public
//     router, with the session in the request's context (HiringSession) so that authentication takes the
//     session's principal: the API key from the handshake's auth when present, else node:<key_id> with the key's
//     tenant and buyer ids.NodeBuyer (spec/relay.md §2.3). Its edge limits are the session's. A request the
//     session's revoked API key made is 401 (spec/tunnel.md §12, auth_revoked); paths outside /agents/…, /mcp/…,
//     /mcp and /.well-known/… never reach Hire (the node refuses them first, and the gate again).
//   - op blob: Blobs resolves ref through the blobs table, checks that its task is one this session serves or
//     one whose buyer is this session's hiring principal's, and writes one JSON line, {"size": n} then exactly n
//     bytes, or {"error": "not_found" | "forbidden" | "too_large"} (an object over 64 MiB), then closes the stream
//     (spec/tunnel.md §8.3, D181).
//
// In flight (§7, §14, D137, D182): a stream counts from Accept until both halves have ended; over Limit (32, 8
// for an anonymous session) the gate answers 429 on it, and still closes the session of a node that keeps more
// than Limit refused streams open, since a refusal frees nothing while the node holds its half.
//
// Work package: M3 WP-03.
type NodeStreams struct {
	Hire  http.Handler
	Blobs BlobServer
	Limit int
}

// BlobServer writes one object for a blob stream.
type BlobServer interface {
	// ServeBlob checks ref against sess and writes the size line and the object's bytes, or the error line, to w.
	ServeBlob(ctx context.Context, sess *Session, ref string, w io.Writer) error
}

// Serve handles one accepted node-opened stream of sess until it ends.
func (n *NodeStreams) Serve(ctx context.Context, sess *Session, st net.Conn) error {
	return unimpl.Error("tunnelgate.NodeStreams.Serve", "M3 WP-03")
}

type hiringKey struct{}

// WithHiringSession marks a request as one a node opened on sess (spec/tunnel.md §9).
func WithHiringSession(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, hiringKey{}, sess)
}

// HiringSession returns the session a request arrived on, ok false for a request from the network.
func HiringSession(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(hiringKey{}).(*Session)
	return s, ok && s != nil
}
