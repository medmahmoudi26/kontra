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
	"sync"
	"time"

	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/mcpwire"
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
//     was read) and reaches nothing; then internal/mcproots: the client's notifications/roots/list_changed and its
//     answer to a roots/list the node answered are 202 and dropped, an initialize declares the node's roots
//     capability, and a 2026-07-28 request loses roots from its clientCapabilities and inputResponses;
//   - the target's JSON responses and event streams (POST responses and the GET stream) are read message by message:
//     a roots/list request is removed (the other events keep their id: and retry: lines as they came) and answered
//     by the node itself, POSTed to the target on the exchange's Mcp-Session-Id and MCP-Protocol-Version (the fixed
//     target; nothing else is dialed); a 2026-07-28 result loses every roots/list entry of its inputRequests; a
//     message that fails strict decoding is not relayed (a JSON response becomes 502 with -32603, an event stream
//     ends before that event), as at the gate (relay.md §11.1).
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

// session is the roots state of the upstream session sid; with no session id (an initialize, a 2026-07-28
// request), a fresh one that nothing remembers.
func (f *mcpURL) session(sid string) *mcproots.Session {
	if sid == "" {
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
}

// request reads and filters a POST's body (other methods carry none). It answers the gate itself and returns false
// for a body that fails strict decoding (400) or a message the node drops (202).
func (f *mcpURL) request(w http.ResponseWriter, r *http.Request, log *slog.Logger, answer func(sid, version string, reply []byte)) (*mcpExchange, bool) {
	x := &mcpExchange{f: f, log: log, era: mcpwire.EraLegacy, httpMethod: r.Method, answer: answer,
		sid: r.Header.Get(mcpwire.HeaderSessionID), version: r.Header.Get(mcpwire.HeaderProtocolVersion)}
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
	sid := x.sid
	if x.era == mcpwire.EraModern {
		sid = "" // a modern request's Mcp-Session-Id means nothing (relay.md §11.12 rule 2)
	}
	d, err := f.session(sid).FromClient(x.era, body, m)
	if err != nil {
		log.Info("an MCP message is refused", "err", err)
		mcpError(w, http.StatusBadRequest, m.ID, mcpwire.CodeInvalidRequest, "invalid request")
		return nil, false
	}
	if d.Action == mcproots.Drop {
		log.Debug("a client message about roots is dropped: roots stay on the node (D173)", "method", m.Method)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(d.Body))
	r.ContentLength = int64(len(d.Body))
	r.Header.Del("Content-Length")
	r.TransferEncoding = nil
	return x, true
}

// response filters the target's response (the ReverseProxy's ModifyResponse, after stripResponse).
func (x *mcpExchange) response(res *http.Response) error {
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
	}
	return nil
}

// refuse replaces a JSON response the node does not relay with 502 and -32603 "upstream error", as the gate answers
// an upstream message that fails strict decoding (relay.md §11.1).
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
	d, err := x.f.session(sid).FromServer(x.era, data, m)
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
type sseFilter struct {
	src    *bufio.Reader
	body   io.Closer
	max    int
	filter func(data []byte) (out []byte, keep, ok bool)

	out []byte
	err error
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
	var (
		raw     [][]byte // the event's lines, each with its end
		data    [][]byte
		hasData bool
		size    int
	)
	for {
		line, end, err := s.line()
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
		if len(line) == 0 {
			if len(raw) == 0 {
				s.out = append(s.out, end...) // a blank line between events
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

// line reads one line and its end: \n, \r\n or \r.
func (s *sseFilter) line() (line, end []byte, err error) {
	for {
		c, err := s.src.ReadByte()
		if err != nil {
			return nil, nil, err
		}
		switch c {
		case '\n':
			return line, []byte("\n"), nil
		case '\r':
			if b, err := s.src.Peek(1); err == nil && b[0] == '\n' {
				_, _ = s.src.ReadByte()
				return line, []byte("\r\n"), nil
			}
			return line, []byte("\r"), nil
		}
		line = append(line, c)
		if len(line) > s.max {
			return nil, nil, errEventTooLarge
		}
	}
}
