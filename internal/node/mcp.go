package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealing"
)

// The URL MCP filter's bounds.
const (
	// maxMCPMessage bounds one message the filter reads: a POST body, a JSON response, one SSE event. The gate's own
	// bounds are narrower (upstream.MaxBody, upstream.MaxSSEEvent), so the node never refuses what the gate would
	// relay.
	maxMCPMessage = 8 << 20
	// maxRootsSessions bounds the upstream sessions whose node-answered roots/list ids the filter remembers; the
	// least recently seen is forgotten first.
	maxRootsSessions = 1024
	// rootsAnswerTimeout bounds the node's POST of its roots/list answer to the target.
	rootsAnswerTimeout = 30 * time.Second
)

// mcpURL is the fixed-target proxy's MCP filter for one mcp:<name>=<url> service (spec/tunnel.md §8.2, §10; D173,
// D172). The proxy forwards MCP exchanges unchanged except for roots:
//
//   - a POST body is read with exact keys (mcpwire.Parse): one that fails is 400 (-32700 or -32600, its id when it
//     was read) and reaches nothing; then internal/mcproots: the client's notifications/roots/list_changed is 202
//     and dropped, and so is every client response but the answer to a server request this filter relayed and has
//     not seen answered (never a roots answer: mcproots.Session.FromClient's SPEC-CONFLICT); an initialize declares
//     the node's roots capability, and a 2026-07-28 request loses roots from its clientCapabilities and
//     inputResponses;
//   - the target is asked for identity encoding (the gate's tunnel transport asks for gzip, and the node reads what
//     the target answers); a JSON or event-stream response that comes coded anyway is 502 with -32603, never
//     relayed unread;
//   - the target's JSON responses and event streams (POST responses and the GET stream) are read message by message:
//     a roots/list request is removed (the other events keep their id: and retry: lines as they came) and answered
//     by the node itself, POSTed to the target on the exchange's Mcp-Session-Id and MCP-Protocol-Version (the fixed
//     target; nothing else is dialed); every other request's id is recorded as relayed, so that the client's answer
//     reaches the target; a 2026-07-28 result loses every roots/list entry of its inputRequests; a message that
//     fails strict decoding is not relayed (a JSON response becomes 502 with -32603, an event stream ends before
//     that event), as at the gate (relay.md §11.1).
//
// It is shared by the sessions of a node (a client's MCP session is pinned to one tunnel session, but the node keeps
// one filter per service, keyed by the upstream's session id).
type mcpURL struct {
	name  string
	roots *mcproots.Roots
	log   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*urlRoots
}

type urlRoots struct {
	s    *mcproots.Session
	seen time.Time
}

func newMCPURL(name string, roots *mcproots.Roots, log *slog.Logger) *mcpURL {
	return &mcpURL{name: name, roots: roots, log: log, sessions: map[string]*urlRoots{}}
}

// session is the roots state of the upstream session sid. A 2026-07-28 exchange has none: a fresh state that nothing
// remembers (it relays no server request and answers no roots/list). Legacy exchanges without a session id (an
// initialize, a stateless server's) share the service's sessionless state, so that the answer to a request such a
// server sent on a POST's stream reaches it.
func (f *mcpURL) session(era mcpwire.Era, sid string) *mcproots.Session {
	if era == mcpwire.EraModern {
		return mcproots.NewSession(f.roots)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[sid]
	if s == nil {
		if len(f.sessions) >= maxRootsSessions {
			var oldest string
			var at time.Time
			for k, v := range f.sessions {
				if oldest == "" || v.seen.Before(at) {
					oldest, at = k, v.seen
				}
			}
			delete(f.sessions, oldest)
		}
		s = &urlRoots{s: mcproots.NewSession(f.roots)}
		f.sessions[sid] = s
	}
	s.seen = time.Now()
	return s.s
}

func (f *mcpURL) forget(sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, sid)
}

// mcpExchange is one exchange through the filter.
type mcpExchange struct {
	f          *mcpURL
	log        *slog.Logger
	era        mcpwire.Era
	httpMethod string
	sid        string // the request's Mcp-Session-Id (the upstream's)
	version    string // the request's MCP-Protocol-Version
	reqID      json.RawMessage
	// answer POSTs the node's roots/list answer to the target on session sid.
	answer func(sid, version string, reply []byte)
	// seal is the exchange's sealing (internal/sealing), after the roots filter; nil seals nothing. refused is the
	// sealing's refusal of a message of the server's, which a JSON response answers as the node's own (§6).
	seal    *sealing.MCPExchange
	refused error
}

// request reads and filters a POST's body (other methods carry none). It answers the gate itself and returns false
// for a body that fails strict decoding (400) or a message the node drops (202).
func (f *mcpURL) request(w http.ResponseWriter, r *http.Request, log *slog.Logger, answer func(sid, version string, reply []byte),
	seal *sealing.MCPExchange) (*mcpExchange, bool) {
	x := &mcpExchange{f: f, log: log, era: mcpwire.EraLegacy, httpMethod: r.Method, answer: answer,
		sid: r.Header.Get(mcpwire.HeaderSessionID), version: r.Header.Get(mcpwire.HeaderProtocolVersion), seal: seal}
	if r.Method != http.MethodPost {
		return x, true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPMessage+1))
	if err != nil {
		mcpError(w, http.StatusBadRequest, nil, mcpwire.CodeInvalidRequest, "the request body could not be read")
		return nil, false
	}
	if len(body) > maxMCPMessage {
		mcpError(w, http.StatusRequestEntityTooLarge, nil, mcpwire.CodeInvalidRequest, "request too large")
		return nil, false
	}
	m, err := mcpwire.Parse(body, mcpwire.FromClient)
	if err != nil {
		log.Info("an MCP message is refused: it is not one JSON-RPC message with exact keys (D172)", "err", err)
		var id json.RawMessage
		if m != nil {
			id = m.ID
		}
		if x.seal.Sealed(body) {
			// On a sealed exchange the refusal is the sealing's: bad_header (plan corrections WP-08), and nothing of it
			// reaches the server.
			x.seal.RefuseMalformed(w, id)
			return nil, false
		}
		switch {
		case errors.Is(err, mcpwire.ErrParse):
			mcpError(w, http.StatusBadRequest, nil, mcpwire.CodeParse, "Parse error")
		case errors.Is(err, mcpwire.ErrBatch):
			mcpError(w, http.StatusBadRequest, nil, mcpwire.CodeInvalidRequest, "batch not supported")
		default:
			mcpError(w, http.StatusBadRequest, id, mcpwire.CodeInvalidRequest, "invalid request")
		}
		return nil, false
	}
	x.reqID = m.ID
	if x.version == mcpwire.V20260728 && m.Method != mcpwire.MethodInitialize {
		x.era = mcpwire.EraModern
	}
	// A modern request's Mcp-Session-Id means nothing (relay.md §11.12 rule 2): session ignores it.
	d, err := f.session(x.era, x.sid).FromClient(x.era, body, m)
	if err != nil {
		log.Info("an MCP message is refused", "err", err)
		mcpError(w, http.StatusBadRequest, m.ID, mcpwire.CodeInvalidRequest, "invalid request")
		return nil, false
	}
	if d.Action == mcproots.Drop {
		log.Debug("a client message is dropped: roots stay on the node, and a response reaches the server only as the answer to a request the node relayed (D173)", "method", m.Method)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)
		return nil, false
	}
	if x.seal != nil {
		// Sealing after the roots filter (plan corrections WP-08): a sealed tools/call or answer is opened; a refusal is
		// the node's own answer (ext-sealed.md §6), and nothing of the message reaches the server.
		dm, err := mcpwire.Parse(d.Body, mcpwire.FromClient)
		if err != nil {
			mcpError(w, http.StatusBadRequest, m.ID, mcpwire.CodeInvalidRequest, "invalid request")
			return nil, false
		}
		out, err := x.seal.FromClient(d.Body, dm)
		if err != nil {
			x.seal.Refuse(w, m.ID, err)
			return nil, false
		}
		d.Body = out
	}
	r.Body = io.NopCloser(bytes.NewReader(d.Body))
	r.ContentLength = int64(len(d.Body))
	r.Header.Del("Content-Length")
	r.TransferEncoding = nil
	return x, true
}

// response filters the target's response (the ReverseProxy's ModifyResponse, after stripResponse).
func (x *mcpExchange) response(res *http.Response) error {
	switch {
	case x.seal != nil && res.StatusCode/100 == 2:
		x.seal.Accepted() // the server accepted the message: a sealed call's job and pair, an answer's request
	case x.seal != nil:
		x.seal.Release() // the server refused it: nothing is recorded, and the gate's retry is no replay
	}
	if x.httpMethod == http.MethodDelete {
		if x.sid != "" && (res.StatusCode/100 == 2 || res.StatusCode == http.StatusNotFound) {
			x.f.forget(x.sid)
		}
		return nil
	}
	sid := x.sid
	if sid == "" {
		sid = res.Header.Get(mcpwire.HeaderSessionID) // an initialize's
	}
	if x.era == mcpwire.EraModern {
		sid = ""
	}
	mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if (mt == "application/json" || mt == "text/event-stream") && !identityCoded(res.Header) {
		// The node asked for identity (exchange's Rewrite): a coded body is one it cannot read for roots, so it is
		// not relayed unread.
		x.log.Warn("an MCP response is not relayed: the server sent it with a content coding the node did not ask for",
			"content_encoding", clipHeader(strings.Join(res.Header.Values("Content-Encoding"), ", ")))
		_ = res.Body.Close()
		x.refuse(res)
		return nil
	}
	switch mt {
	case "application/json":
		body, err := io.ReadAll(io.LimitReader(res.Body, maxMCPMessage+1))
		_ = res.Body.Close()
		if err != nil {
			return fmt.Errorf("the MCP server's response could not be read: %w", err)
		}
		if len(bytes.TrimSpace(body)) == 0 {
			res.Body = io.NopCloser(bytes.NewReader(body))
			return nil
		}
		out, keep, ok := x.filter(sid, body)
		if x.refused != nil {
			status, hdr, b := sealing.RejectAnswer(x.reqID, x.refused)
			res.StatusCode, res.Status = status, fmt.Sprintf("%d %s", status, http.StatusText(status))
			res.Header, res.Trailer = hdr, nil
			res.Body = io.NopCloser(bytes.NewReader(b))
			res.ContentLength = int64(len(b))
			res.Header.Set("Content-Length", strconv.Itoa(len(b)))
			res.TransferEncoding = nil
			return nil
		}
		if len(body) > maxMCPMessage || !ok || !keep {
			x.refuse(res)
			return nil
		}
		res.Body = io.NopCloser(bytes.NewReader(out))
		res.ContentLength = int64(len(out))
		res.Header.Set("Content-Length", strconv.Itoa(len(out)))
		res.TransferEncoding = nil
	case "text/event-stream":
		res.Body = &sseFilter{src: bufio.NewReaderSize(res.Body, 32<<10), body: res.Body, max: maxMCPMessage,
			filter: func(data []byte) ([]byte, bool, bool) { return x.filter(sid, data) }}
		res.ContentLength = -1
		res.Header.Del("Content-Length")
	default:
		if x.seal != nil && res.StatusCode/100 == 2 {
			// A 2xx body labelled otherwise (text/plain, none) is read as one JSON message all the same, as the A2A
			// side reads every 2xx body (sealing.Hosting.Response): a sealed call's result labelled text/plain would
			// otherwise reach the gate unsealed (ext-sealed.md §5, "seals every part of every reply").
			return x.unlabelled(sid, res)
		}
	}
	return nil
}

// unlabelled reads a 2xx response that is neither JSON nor an event stream, on an exchange with sealing, as one JSON
// message through filter: the sealed message is relayed as application/json, an empty body as it came, and one
// the filter refuses (or that is no JSON-RPC message with exact keys) is the node's refusal; nothing of it is
// relayed unread.
func (x *mcpExchange) unlabelled(sid string, res *http.Response) error {
	if !identityCoded(res.Header) {
		_ = res.Body.Close()
		x.refuse(res)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxMCPMessage+1))
	_ = res.Body.Close()
	if err != nil {
		return fmt.Errorf("the MCP server's response could not be read: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		res.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	if len(body) > maxMCPMessage {
		x.refuse(res)
		return nil
	}
	out, keep, ok := x.filter(sid, body)
	if x.refused != nil {
		status, hdr, b := sealing.RejectAnswer(x.reqID, x.refused)
		res.StatusCode, res.Status = status, fmt.Sprintf("%d %s", status, http.StatusText(status))
		res.Header, res.Trailer = hdr, nil
		res.Body = io.NopCloser(bytes.NewReader(b))
		res.ContentLength = int64(len(b))
		res.Header.Set("Content-Length", strconv.Itoa(len(b)))
		res.TransferEncoding = nil
		return nil
	}
	if !ok || !keep {
		x.refuse(res)
		return nil
	}
	res.Header.Set("Content-Type", "application/json") // what it was read as
	res.Body = io.NopCloser(bytes.NewReader(out))
	res.ContentLength = int64(len(out))
	res.Header.Set("Content-Length", strconv.Itoa(len(out)))
	res.TransferEncoding = nil
	return nil
}

// identityCoded reports whether a response's Content-Encoding is absent or identity only.
func identityCoded(h http.Header) bool {
	for _, v := range h.Values("Content-Encoding") {
		for _, c := range strings.Split(v, ",") {
			if c = strings.TrimSpace(c); c != "" && !strings.EqualFold(c, "identity") {
				return false
			}
		}
	}
	return true
}

func clipHeader(s string) string {
	if len(s) > 64 {
		return strings.ToValidUTF8(s[:64], "") + "…"
	}
	return s
}

// refuse replaces a response the node does not relay with 502 and -32603 "upstream error", as the gate answers an
// upstream message that fails strict decoding (relay.md §11.1).
func (x *mcpExchange) refuse(res *http.Response) {
	b, err := mcpwire.ErrorBody(x.reqID, mcpwire.CodeInternal, "upstream error", nil)
	if err != nil {
		b, _ = mcpwire.ErrorBody(nil, mcpwire.CodeInternal, "upstream error", nil)
	}
	res.StatusCode, res.Status = http.StatusBadGateway, "502 Bad Gateway"
	res.Header = http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(b))}}
	res.Body = io.NopCloser(bytes.NewReader(b))
	res.ContentLength = int64(len(b))
	res.TransferEncoding = nil
	res.Trailer = nil
}

// filter is one message of the target's: out the bytes to relay, keep false to remove it, ok false when it is not
// relayed and what follows it neither (strict decoding).
func (x *mcpExchange) filter(sid string, data []byte) (out []byte, keep, ok bool) {
	m, err := mcpwire.Parse(data, mcpwire.FromUpstream)
	if err != nil {
		x.log.Warn("a message from the MCP server is not relayed: it is not one JSON-RPC message with exact keys (D172)", "err", err)
		return nil, false, false
	}
	d, err := x.f.session(x.era, sid).FromServer(x.era, data, m)
	if err != nil {
		x.log.Warn("a message from the MCP server is not relayed", "err", err)
		return nil, false, false
	}
	switch d.Action {
	case mcproots.Answer:
		x.log.Debug("roots/list answered by the node (D173)")
		x.answer(sid, x.version, d.Reply)
		return nil, false, true
	case mcproots.Drop:
		x.log.Debug("a roots/list on a 2026-07-28 exchange is dropped (D173)")
		return nil, false, true
	}
	if x.seal != nil {
		// Sealing after the roots filter: a message it refuses is not relayed, nor anything after it.
		out, keep, err := x.seal.FromServer(d.Body)
		if err != nil {
			x.refused = err
			return nil, false, false
		}
		return out, keep, true
	}
	return d.Body, true, true
}

// answerRoots POSTs the node's answer to the target's roots/list, on the upstream session sid, to the endpoint the
// exchange went to (the fixed target; nothing else is dialed). It does not wait.
func (p *Proxy) answerRoots(sv Serve, endpoint string, log *slog.Logger) func(sid, version string, reply []byte) {
	return func(sid, version string, reply []byte) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), rootsAnswerTimeout)
			defer cancel()
			u := url.URL{Scheme: sv.Target.Scheme, Host: sv.Target.Host, Path: endpoint}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(reply))
			if err != nil {
				log.Warn("the node's roots/list answer could not be sent", "err", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if sid != "" {
				req.Header.Set(mcpwire.HeaderSessionID, sid)
			}
			if version != "" {
				req.Header.Set(mcpwire.HeaderProtocolVersion, version)
			}
			resp, err := p.transports[sv.Name].RoundTrip(req)
			if err != nil {
				log.Warn("the node's roots/list answer did not reach the MCP server", "err", err)
				return
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				log.Warn("the MCP server refused the node's roots/list answer", "status", resp.StatusCode)
			}
		}()
	}
}

// mcpError writes a JSON-RPC error the node answers itself, without Kontra-Node: a 400 the node writes is final and
// taken as the service's own (D151).
func mcpError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	b, err := mcpwire.ErrorBody(id, code, msg, nil)
	if err != nil {
		b, _ = mcpwire.ErrorBody(nil, code, msg, nil)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// errEventTooLarge ends a stream whose event is over the filter's bound.
var errEventTooLarge = errors.New("node: an SSE event over the MCP filter's bound")

// sseFilter relays an event stream event by event through filter: an event without data (a priming event, a
// comment) passes as it came; a data event's message (its data lines joined) passes as it came, or with its data
// lines replaced by one when the filter rewrote it, or not at all when the filter removed it; the stream ends before
// an event the filter refuses, and before an event left unterminated at the end (as a client drops it). Lines end
// with \n, \r\n or \r, as the gate reads them (internal/mcprelay's sseReader), so no line can hide from the filter.
//
// A \r ends its line at once: the filter never waits for the byte after it (a server whose lines end with a lone \r
// would otherwise have each event held until it writes again, minutes on a GET stream). A \n that turns out to
// follow it is the rest of that line end, \r\n, and goes where the \r went: with its line, or with the event the
// filter removed. The bytes relayed are those that came, in order.
//
// One UTF-8 byte order mark at the start of the stream is dropped (WHATWG SSE; stock clients drop it): kept, it would
// make the first event's field "U+FEFF data", which the filter would pass unread (roots, sealing) and clients dispatch.
type sseFilter struct {
	src    *bufio.Reader
	body   io.Closer
	max    int
	filter func(data []byte) (out []byte, keep, ok bool)
	// started is set once the stream's first bytes were looked at for a byte order mark.
	started bool

	out []byte
	err error
	// cr is set when the last line read ended with a \r whose next byte was not read yet; crKept says whether that
	// \r ended an event (or a blank line) that was relayed, when the event is already done with.
	cr, crKept bool
}

func (s *sseFilter) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		s.next()
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

func (s *sseFilter) Close() error { return s.body.Close() }

// next reads one event and sets what is relayed of it.
func (s *sseFilter) next() {
	if !s.started {
		s.started = true
		if b, err := s.src.Peek(1); err == nil && b[0] == 0xEF {
			if b, err := s.src.Peek(3); err == nil && bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
				_, _ = s.src.Discard(3)
			}
		}
	}
	var (
		raw     [][]byte // the event's lines, each with its end
		data    [][]byte
		hasData bool
		size    int
	)
	for {
		line, end, lf, err := s.line()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.EOF // an unterminated last event is dropped
			}
			s.err = err
			return
		}
		if size += len(line) + len(end); size > s.max {
			s.err = errEventTooLarge
			return
		}
		if lf {
			// The \n of a \r\n whose \r ended the line before.
			switch {
			case len(raw) > 0:
				raw[len(raw)-1] = append(raw[len(raw)-1], '\n')
			case s.crKept:
				s.out = append(s.out, '\n')
				return
			}
			continue
		}
		if len(line) == 0 {
			if len(raw) == 0 {
				s.out = append(s.out, end...) // a blank line between events
				s.crKept = true
				return
			}
			raw = append(raw, end)
			break
		}
		raw = append(raw, append(append([]byte(nil), line...), end...))
		name, value, _ := bytes.Cut(line, []byte(":"))
		if string(name) == "data" {
			hasData = true
			data = append(data, bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	msg := bytes.Join(data, []byte("\n"))
	s.crKept = true
	if !hasData || len(bytes.TrimSpace(msg)) == 0 {
		s.out = bytes.Join(raw, nil)
		return
	}
	out, keep, ok := s.filter(msg)
	switch {
	case !ok:
		s.err = io.EOF // the stream ends before this event
		return
	case !keep:
		s.crKept = false
		return // removed whole, its id: and retry: with it
	case bytes.Equal(out, msg):
		s.out = bytes.Join(raw, nil)
		return
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, out); err != nil {
		s.err = io.EOF
		return
	}
	wrote := false
	for _, l := range raw {
		name, _, _ := bytes.Cut(bytes.TrimRight(l, "\r\n"), []byte(":"))
		if string(name) == "data" && len(bytes.TrimRight(l, "\r\n")) > 0 {
			if !wrote {
				s.out = append(s.out, "data: "...)
				s.out = append(s.out, compact.Bytes()...)
				s.out = append(s.out, '\n')
				wrote = true
			}
			continue
		}
		s.out = append(s.out, l...)
	}
}

// line reads one line and its end: \n, \r\n or \r. After a \r it looks at the next byte only when it is already
// buffered, and never waits for it: a \r with nothing behind it yet ends the line as \r, and a \n read first by
// the next call is returned alone with lf set (the rest of that \r\n).
func (s *sseFilter) line() (line, end []byte, lf bool, err error) {
	if s.cr {
		s.cr = false
		c, err := s.src.ReadByte()
		if err != nil {
			return nil, nil, false, err
		}
		if c == '\n' {
			return nil, []byte("\n"), true, nil
		}
		_ = s.src.UnreadByte()
	}
	for {
		c, err := s.src.ReadByte()
		if err != nil {
			return nil, nil, false, err
		}
		switch c {
		case '\n':
			return line, []byte("\n"), false, nil
		case '\r':
			if s.src.Buffered() == 0 {
				s.cr = true
				return line, []byte("\r"), false, nil
			}
			if b, err := s.src.Peek(1); err == nil && b[0] == '\n' {
				_, _ = s.src.ReadByte()
				return line, []byte("\r\n"), false, nil
			}
			return line, []byte("\r"), false, nil
		}
		line = append(line, c)
		if len(line) > s.max {
			return nil, nil, false, errEventTooLarge
		}
	}
}
