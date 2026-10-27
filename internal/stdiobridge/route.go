package stdiobridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// MethodTasksResult is the 2025-11-25 request whose POST stream receives its task's related messages.
const MethodTasksResult = "tasks/result"

// event is one message on an SSE stream: its number (its id is <child start>-<n>) and its JSON, one line.
type event struct {
	n      uint64
	data   []byte
	method string // for a log line
}

// stream is one open SSE response (a POST's or the GET's): what was routed to it and not yet written.
type stream struct {
	mu    sync.Mutex
	evs   []event
	size  int
	final bool // the last event is the request's response: the stream ends once it is written
	ended bool // the stream ends once what it holds is written (the session ended, or the GET was replaced)
	wake  chan struct{}
}

func newStream() *stream { return &stream{wake: make(chan struct{}, 1)} }

func (s *stream) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// push adds an event; final marks the response that ends a POST stream. It is false (and the stream ends) when
// the stream holds more than maxStreamQueue unwritten.
func (s *stream) push(ev event, final bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final || s.ended {
		return false
	}
	if s.size+len(ev.data) > maxStreamQueue {
		s.ended = true
		s.signal()
		return false
	}
	s.evs = append(s.evs, ev)
	s.size += len(ev.data)
	s.final = final
	s.signal()
	return true
}

// end ends the stream after what it holds.
func (s *stream) end() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	s.signal()
}

// take returns the events not yet written; done when nothing more will come.
func (s *stream) take() ([]event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs := s.evs
	s.evs, s.size = nil, 0
	return evs, s.final || s.ended
}

// await waits for a request's response (the final event) and returns its JSON; ok false when the stream ended
// without one or ctx ended first. Events before the response are dropped (an initialize is answered with JSON).
func (s *stream) await(ctx context.Context) ([]byte, bool) {
	for {
		s.mu.Lock()
		if s.final && len(s.evs) > 0 {
			resp := s.evs[len(s.evs)-1].data
			s.mu.Unlock()
			return resp, true
		}
		ended := s.ended
		s.evs, s.size = nil, 0
		s.mu.Unlock()
		if ended {
			return nil, false
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// --- client to child ---

// Errors of register.
var errIDInUse = errors.New("stdiobridge: a request with this id is in flight")

// register opens a POST stream for request m: the response to its id, the messages carrying its progressToken
// (params._meta.progressToken) and, for tasks/result, those related to its task, are routed to it.
func (c *child) register(m *mcpwire.Message) (*stream, error) {
	key, ok := mcpwire.IDKey(m.ID)
	if !ok {
		return nil, fmt.Errorf("stdiobridge: request id %s", clip(string(m.ID)))
	}
	st := newStream()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.gone:
		return nil, errGone
	case c.pending[key] != nil:
		return nil, errIDInUse
	}
	c.pending[key] = st
	if tok, ok := progressToken(m.Params, "_meta"); ok && c.progress[tok] == nil {
		c.progress[tok] = st
	}
	if m.Method == MethodTasksResult {
		if id, ok := stringMember(m.Params, "taskId"); ok && c.taskRes[id] == nil {
			c.taskRes[id] = st
		}
	}
	return st, nil
}

// unregister removes st's routes, those that still lead to it.
func (c *child) unregister(m *mcpwire.Message, st *stream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, routes := range []map[string]*stream{c.pending, c.progress, c.taskRes} {
		for k, s := range routes {
			if s == st {
				delete(routes, k)
			}
		}
	}
}

// post serves a POST on the session: roots first (D173), then a request gets an event stream with what is routed
// to it, a notification or a response 202 once it is queued for the child.
func (c *child) post(w http.ResponseWriter, r *http.Request, body []byte, m *mcpwire.Message) {
	if !c.begin() {
		sessionNotFound(w, m.ID)
		return
	}
	defer c.end()
	d, err := c.roots.FromClient(mcpwire.EraLegacy, body, m)
	if err != nil {
		c.log.Info("a client message is refused", "err", err)
		writeRPCError(w, http.StatusBadRequest, m.ID, mcpwire.CodeInvalidRequest, "invalid request")
		return
	}
	if d.Action == mcproots.Drop {
		c.log.Debug("a client message about roots is dropped: roots stay on the node (D173)", "method", m.Method)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if m.Method == "" || len(m.ID) == 0 {
		if err := c.send(d.Body); err != nil {
			c.refuseSend(w, m, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	st, err := c.register(m)
	switch {
	case errors.Is(err, errGone):
		sessionNotFound(w, m.ID)
		return
	case err != nil:
		writeRPCError(w, http.StatusBadRequest, m.ID, mcpwire.CodeInvalidRequest, "Invalid Request: a request with this id is in flight")
		return
	}
	defer c.unregister(m, st)
	if err := c.send(d.Body); err != nil {
		c.refuseSend(w, m, err)
		return
	}
	c.serveStream(w, r, st)
}

func (c *child) refuseSend(w http.ResponseWriter, m *mcpwire.Message, err error) {
	if errors.Is(err, errBusy) {
		c.log.Warn("a client message is refused: the stdio server does not read its stdin")
		nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull, m.ID, "kontra connect: the stdio server does not read its input")
		return
	}
	sessionNotFound(w, m.ID)
}

// serveGET opens the session's GET stream: the queue first, from Last-Event-ID when it names an event of this child
// (never past the first message no GET has had), else from the first message no GET has had; then what is routed to
// it, until the client goes, the session ends or another GET takes its place.
func (c *child) serveGET(w http.ResponseWriter, r *http.Request) {
	if !c.begin() {
		sessionNotFound(w, nil)
		return
	}
	defer c.end()
	st := newStream()
	c.mu.Lock()
	if c.gone {
		c.mu.Unlock()
		sessionNotFound(w, nil)
		return
	}
	if c.get != nil {
		c.get.end()
	}
	after := c.delivered
	if k, ok := c.parseEventID(r.Header.Get(mcpwire.HeaderLastEventID)); ok && k < after {
		after = k
	}
	for _, ev := range c.queue {
		if ev.n > after && st.push(ev, false) && ev.n > c.delivered {
			c.delivered = ev.n
		}
	}
	c.get = st
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.get == st {
			c.get = nil
		}
		c.mu.Unlock()
	}()
	c.serveStream(w, r, st)
}

// serveStream writes st as an event stream until it is done or the client goes, flushing per event.
func (c *child) serveStream(w http.ResponseWriter, r *http.Request, st *stream) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	var buf bytes.Buffer
	for {
		evs, done := st.take()
		if len(evs) > 0 {
			buf.Reset()
			for _, ev := range evs {
				fmt.Fprintf(&buf, "id: %s\nevent: message\ndata: %s\n\n", c.eventID(ev.n), ev.data)
			}
			if _, err := w.Write(buf.Bytes()); err != nil {
				return
			}
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return
			}
		}
		if done {
			return
		}
		select {
		case <-st.wake:
		case <-r.Context().Done():
			return
		}
	}
}

// --- child to client ---

// route sends one line of the child's stdout where it goes (§10): read with exact keys (a message that fails is not
// relayed, and a request whose response it was ends without one), roots/list answered by the node, a response to
// its POST, a progress notification to its request's POST, a related-task message to its task's open tasks/result,
// everything else to the GET stream.
func (c *child) route(line []byte) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, line); err != nil {
		c.log.Debug("a line of the stdio server's stdout is not JSON: dropped", "bytes", len(line))
		return
	}
	msg := buf.Bytes()
	m, err := mcpwire.Parse(msg, mcpwire.FromUpstream)
	if err != nil {
		c.log.Warn("a message from the stdio server is not relayed: it is not one JSON-RPC message with exact keys (D172)", "err", err)
		if m != nil && len(m.ID) > 0 {
			if key, ok := mcpwire.IDKey(m.ID); ok {
				c.mu.Lock()
				st := c.pending[key]
				delete(c.pending, key)
				c.mu.Unlock()
				if st != nil {
					st.end()
				}
			}
		}
		return
	}
	d, err := c.roots.FromServer(mcpwire.EraLegacy, msg, m)
	if err != nil {
		c.log.Warn("a message from the stdio server is not relayed", "err", err)
		return
	}
	switch d.Action {
	case mcproots.Answer:
		if err := c.send(d.Reply); err != nil {
			c.log.Warn("the node's answer to roots/list could not be written", "err", err)
		}
		c.log.Debug("roots/list answered by the node (D173)", "roots", len(c.b.cfg.Roots.Dirs()))
		return
	case mcproots.Drop:
		return
	}
	msg = d.Body
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return
	}
	c.seq++
	ev := event{n: c.seq, data: msg, method: m.Method}
	if m.Method == "" {
		key, _ := mcpwire.IDKey(m.ID)
		st := c.pending[key]
		if st == nil {
			c.log.Debug("a response of the stdio server answers no open request: dropped")
			return
		}
		delete(c.pending, key)
		if !st.push(ev, true) {
			c.log.Warn("a POST stream the gate does not read was ended")
		}
		return
	}
	if tok, ok := progressToken(m.Params, ""); ok {
		if st := c.progress[tok]; st != nil {
			c.pushTo(st, ev)
			return
		}
	}
	if id, ok := relatedTask(m.Params); ok {
		if st := c.taskRes[id]; st != nil {
			c.pushTo(st, ev)
			return
		}
	}
	c.enqueueGET(ev)
}

func (c *child) pushTo(st *stream, ev event) {
	if !st.push(ev, false) {
		c.log.Warn("a POST stream the gate does not read was ended")
	}
}

// enqueueGET queues ev for the GET stream (QueueMessages or QueueBytes, the oldest dropped; an undelivered one is
// logged) and hands it to the open GET, if any. c.mu is held.
func (c *child) enqueueGET(ev event) {
	c.queue = append(c.queue, ev)
	c.queueSize += len(ev.data)
	for len(c.queue) > QueueMessages || (c.queueSize > QueueBytes && len(c.queue) > 1) {
		old := c.queue[0]
		c.queue = c.queue[1:]
		c.queueSize -= len(old.data)
		if old.n > c.delivered {
			c.log.Warn("the GET queue is full: its oldest message, never delivered, is dropped",
				"event_id", c.eventID(old.n), "method", old.method, "queue_messages", QueueMessages, "queue_bytes", QueueBytes)
		}
	}
	if c.get != nil && c.get.push(ev, false) {
		c.delivered = ev.n
	}
}

// --- reading members ---

// progressToken is the progress token of params (under _meta when in is "_meta": a client request's; at the top of
// params: a progress notification's), keyed as an id.
func progressToken(params json.RawMessage, in string) (string, bool) {
	obj := []byte(params)
	if in != "" {
		v, ok := mcpwire.Member(obj, in)
		if !ok {
			return "", false
		}
		obj = v
	}
	v, ok := mcpwire.Member(obj, "progressToken")
	if !ok {
		return "", false
	}
	return mcpwire.IDKey(v)
}

// relatedTask is the taskId of params._meta["io.modelcontextprotocol/related-task"], read with exact keys.
func relatedTask(params json.RawMessage) (string, bool) {
	meta, ok := mcpwire.Member(params, "_meta")
	if !ok {
		return "", false
	}
	rel, ok := mcpwire.Member(meta, mcpwire.MetaRelatedTask)
	if !ok {
		return "", false
	}
	if _, err := strictjson.Members(rel); err != nil {
		return "", false // an ambiguous key: routed to the GET stream, never to a task's
	}
	return stringMember(rel, "taskId")
}

// stringMember is obj's member key when it is a string.
func stringMember(obj json.RawMessage, key string) (string, bool) {
	v, ok := mcpwire.Member(obj, key)
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}
