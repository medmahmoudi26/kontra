// Package sealing is the nodes' half of the sealed extension (spec/ext-sealed.md §4–§7, D31, D61, D62, D162,
// D165): the hosting node unseals what the gate forwards to a sealed service and seals what the service
// answers, the hiring node seals what a local client sends to a sealed service and opens what comes back, and
// kontra seal is the escape hatch for scripting (§5.1).
//
// Both nodes read and write the A2A 1.0 shapes only, JSON-RPC and HTTP+JSON, unary and SSE (D165), and for MCP
// the 2025-11-25 tools/call, its result, and for a task the sealed call created its tasks/result, input requests,
// answers and statusMessage (D62); a sealed MCP service is never served to a 2026-07-28 request (D162). Neither
// node ever hands a key to a service or a client, and neither logs a plaintext part or a decrypted body, at any
// level: a log line names a reason, a service, a job and sizes, never content (sealed_no_plaintext scans the
// nodes' logs too).
//
// Work package: M3 WP-08.
package sealing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// Hosting-node bounds (spec/ext-sealed.md §5).
const (
	// DefaultMaxAge is --sealed-max-age: how old a request's iat may be; it must cover the operator's longest
	// quote expiry plus task.timeout.
	DefaultMaxAge = 25 * time.Hour
	// JobRetention is how long a job's state is kept after its last exchange (node-local clock).
	JobRetention = 7 * 24 * time.Hour
	// HostedDirName is the job state's directory under ~/.kontra: hosted/<gate host>/<service>/, 0600 files.
	HostedDirName = "hosted"
	// maxBody bounds a request or a reply body the node reads whole to open or seal it (a2awire.MaxBody).
	maxBody = a2awire.MaxBody
)

// HostingConfig configures the hosting side.
type HostingConfig struct {
	Dir        string                // ~/.kontra/hosted/<gate host>
	Identities func() []age.Identity // the identity file's keys, active first (a rotation's old key still opens)
	KeyIDs     func() []string       // their key ids, for the kids check (not_a_recipient)
	MaxAge     time.Duration         // --sealed-max-age
	Required   []string              // --sealed-required <service>: refuse an unsealed request (bad_header)
	// Now is the node's clock (tests); nil is time.Now.
	Now func() time.Time
}

// Hosting is the hosting node's sealing, keyed per service and per job (spec §5 "Hosting node"): per job (the
// gate task id; for plain MCP calls, the session hash) the replyTo and rk of its first sealed request, the
// upstream task and context ids, the (messageId, part) pairs forwarded, and per gate task the MCP input requests
// it sealed and has not seen answered (InputRequest); per service every replyTo that opened a job, the upstream
// context id recorded per gate context, and for MCP every (session, messageId) forwarded. A job is opened, and
// its pairs recorded, only once the service has accepted the request; until then what a request will record is
// held in flight (Hosting.flight), so that a second copy sent while the first is in flight is replayed too, and
// released with nothing recorded when the node answers the request itself or the service refuses it.
//
// Bodies are read with exact keys (internal/mcpwire.Parse, internal/strictjson; D172): one with a repeated or
// case-variant key at a level the node reads, a lone case variant of a member it reads there included (which
// encoding/json, the gate's and the service's decoder, takes for the member), a service's reply included, is
// refused bad_header, since the gate is not trusted to have checked a request; bodies are sealed and opened by
// re-encoding what was parsed, never patched in place. A request is read by its route first (an HTTP+JSON send is
// one whatever members its body has), and a reply in its request's binding; on a sealed job, or a service with
// sealed jobs, every 2xx reply is read as the gate's client reads it whatever its Content-Type, so none passes
// unread (Response).
//
// MCP input requests (D175): an upstream elicitation/create or sampling/createMessage of a sealed task is sealed
// with messageId = the upstream server's own JSON-RPC id for it (as a string), and session and tool of the call
// that created the task. The gate rewrites the JSON-RPC id to k.<taskId>.<seq> and back, and never touches the
// envelope. The answer, a JSON-RPC response the gate forwards on an exchange of that task, passes rule 4b: its
// header's session, tool and messageId equal the preamble's mcp_session, the task's tool and the response's
// id, which names an InputRequest of the preamble's task sealed less than MaxAge ago (wrong_message otherwise)
// and not answered yet (replayed); replyTo and rk equal the job's (wrong_job). Rule 5's (session, messageId)
// set is for requests only: a server's ids are another space than the client's. In a job whose request was
// sealed, a response whose id is that of an InputRequest MUST itself be sealed and pass rule 4b: one without an
// exact {v, envelope} in result._meta["sh.kontra/sealed"] is refused bad_header and nothing of it is forwarded
// to the service, so a gate cannot answer the service's question in plaintext for the buyer (spec §5, "Answers
// must be sealed").
//
// SPEC-CONFLICT (ext-sealed.md §5, rules 1–5): the rules bind the sealed parts of a request, and say nothing of an
// exchange without one (a GetTask, a CancelTask, a SubscribeToTask, an unsealed message) that names a sealed job's
// upstream task: its replies would be sealed to the replyTo of whatever job the preamble names, or not at all, so a
// gate could read a sealed task's content by asking for it on an exchange of a job of its own, or of none. The
// closest faithful behaviour: the node never lets an exchange reach a sealed job's upstream task or context unless
// the exchange is that job's (its preamble names the job's gate task, or for MCP its session): a request naming
// another job's upstream task id is wrong_task, one naming another gate context's upstream context wrong_context,
// and a service reply naming a sealed job's upstream task on an exchange of another job, or of none, is refused
// before anything of it is forwarded (§6's reasons). For the same reason a service with sealed jobs (or started with
// --sealed-required) gets nothing the node cannot read: a push config, inline or by method or route, is refused
// bad_header (it would have the service post a sealed task's plaintext to a URL the gate chose; the gate never sends
// one upstream, relay.md §10.1), as are a JSON-RPC request that is none of A2A 1.0's methods and a body that is not
// one JSON object; and a sealed job's MCP tasks keep their statusMessage sealed on every session's exchanges.
type Hosting struct {
	cfg HostingConfig
	log *slog.Logger
	st  *fileStore

	mu       sync.Mutex
	services map[string]*serviceState // loaded service.json, written through
	jobs     map[string]*jobState     // loaded job files by "<service>/<job>", written through
	// flight holds what a request the service has not accepted yet will record once it does (rule 2's replyTo, rule
	// 5's pairs and MCP (session, messageId), rule 4b's answered input request), by flightKey, with its gate task for a
	// replyTo: a second copy in flight is refused as a replay of the first, as it would be once the first is recorded.
	// An exchange releases its keys when the service accepts it (they are then recorded) or the node answers it
	// itself (nothing is recorded: the gate's retry is not a replay).
	flight map[string]flightRef
}

// flightRef is one in-flight key: the gate task that holds it and how many exchanges do.
type flightRef struct {
	owner string
	n     int
}

// NewHosting opens the job state directory.
func NewHosting(cfg HostingConfig, log *slog.Logger) (*Hosting, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.Dir == "" {
		return nil, errors.New("sealing: no job state directory")
	}
	if cfg.Identities == nil || cfg.KeyIDs == nil {
		return nil, errors.New("sealing: no identity")
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := privateDir(cfg.Dir); err != nil {
		return nil, err
	}
	return &Hosting{cfg: cfg, log: log.With("component", "sealing-hosting"), st: &fileStore{dir: cfg.Dir},
		services: map[string]*serviceState{}, jobs: map[string]*jobState{}, flight: map[string]flightRef{}}, nil
}

// flightKey is the in-flight key of what a request records: kind "r" a replyTo, "p" a job's (messageId, part) pair,
// "m" an MCP (session, messageId), "a" an MCP input request's answer.
func flightKey(kind, service string, parts ...string) string {
	return kind + "\x00" + service + "\x00" + strings.Join(parts, "\x00")
}

// reserve records keys in flight for owner; h.mu is held.
func (h *Hosting) reserve(owner string, keys ...string) {
	for _, k := range keys {
		r := h.flight[k]
		r.owner = owner
		r.n++
		h.flight[k] = r
	}
}

// unreserve drops one hold of each key; h.mu is held.
func (h *Hosting) unreserve(keys []string) {
	for _, k := range keys {
		r, ok := h.flight[k]
		if !ok {
			continue
		}
		if r.n--; r.n <= 0 {
			delete(h.flight, k)
			continue
		}
		h.flight[k] = r
	}
}

// inFlight returns the gate task holding key, ok false when no exchange does; h.mu is held.
func (h *Hosting) inFlight(key string) (string, bool) {
	r, ok := h.flight[key]
	return r.owner, ok
}

// InputRequest is an MCP input request this node sealed for a gate task and has not seen answered.
type InputRequest struct {
	Task      string    `json:"task"`       // the preamble's task (the gate task; for a passthrough task its upstream id)
	MessageID string    `json:"message_id"` // the upstream server's JSON-RPC id, as a string
	SealedAt  time.Time `json:"sealed_at"`  // node clock; the answer must come within MaxAge
	Answered  bool      `json:"answered,omitempty"`
}

// Exchange is one forwarded exchange's sealing state, from Request to Response.
type Exchange struct {
	h       *Hosting
	pre     tunnel.Preamble
	service string
	jsonrpc bool
	rpcID   json.RawMessage

	// stream is a request whose answer a client reads as an event stream (SendStreamingMessage, SubscribeToTask;
	// message:stream, :subscribe).
	stream bool

	// job is the sealed job whose replies are sealed (nil: an unsealed exchange, whose replies pass unless they
	// name a sealed job's upstream task).
	job *jobState
	// commit is what the service's acceptance records (rule 2's new job, rule 5's pairs); reserved are its in-flight
	// keys (Hosting.flight) until then.
	commit   func() error
	reserved []string
	done     bool
}

// Release drops what the exchange holds in flight without recording it: the node answered the request itself (the
// service did not answer, or answered it with an error), so the gate's retry of it is not a replay (spec §5). After
// the service accepted the request (Response with a 2xx) it does nothing. The proxy calls it when it answers an
// exchange itself; Request also arranges it for when the request's context ends.
func (x *Exchange) Release() {
	if x == nil {
		return
	}
	x.h.mu.Lock()
	defer x.h.mu.Unlock()
	x.h.unreserve(x.reserved)
	x.reserved = nil
	if !x.done {
		x.commit = nil
	}
}

// Request checks and unseals one request the gate forwards to a sealed service (§5 rules 1–5) with the
// preamble's ids, replacing each sealed part with its plaintext part. A refusal is a *sealed.RejectError, which
// the proxy answers itself: 400, Kontra-Node: sealed_rejected, Kontra-Sealed-Reject: <reason> (§6).
//
// On a refusal the returned Exchange is not nil: its ID is the request's JSON-RPC id, for the answer.
func (h *Hosting) Request(ctx context.Context, pre tunnel.Preamble, req *http.Request) (*http.Request, *Exchange, error) {
	x := &Exchange{h: h, pre: pre, service: pre.Service}
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
		_ = req.Body.Close()
		if err != nil {
			return nil, x, reject(sealed.RejectBadHeader, "the request body could not be read")
		}
		if len(b) > maxBody {
			return nil, x, reject(sealed.RejectBadHeader, "a request body over the node's bound")
		}
		body = b
	}
	op, err := x.classify(req, body)
	if err != nil {
		return nil, x, err
	}
	x.stream = op.stream
	h.mu.Lock()
	defer h.mu.Unlock()
	svc, err := h.service(x.service)
	if err != nil {
		return nil, x, err
	}
	// A service with sealed state (or --sealed-required) gets only what the node can read: a body it cannot read as
	// one JSON object, a JSON-RPC request that is none of A2A 1.0's methods, and a push config are refused. The
	// gate never sends the upstream a push config (spec/relay.md §10.1: the gate is the agent's only client), and
	// one would have the service post a sealed task's plaintext to a URL the gate chose, outside the node.
	guarded := h.guarded(x.service, svc)
	switch {
	case guarded && op.unreadable:
		return nil, x, reject(sealed.RejectBadHeader, "a body that is not one JSON object, to a service with sealed jobs")
	case guarded && x.jsonrpc && !op.known:
		return nil, x, reject(sealed.RejectBadHeader, "a JSON-RPC request that is none of A2A 1.0's methods, to a service with sealed jobs")
	case guarded && op.kind == opPush:
		return nil, x, reject(sealed.RejectBadHeader, "a push config, to a service with sealed jobs (the upstream's own push configs are never used)")
	}
	switch op.kind {
	case opSend:
		out, err := x.send(svc, body, guarded)
		if err != nil {
			return nil, x, err
		}
		body = out
	default:
		if err := x.other(svc, op.taskIDs); err != nil {
			return nil, x, err
		}
	}
	if len(x.reserved) > 0 {
		context.AfterFunc(ctx, x.Release) // a request whose exchange ends unanswered records nothing
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Length")
	req.TransferEncoding = nil
	if len(body) == 0 {
		req.Body = http.NoBody
	}
	return req, x, nil
}

// opKind is what an A2A request asks.
type opKind int

const (
	opOther opKind = iota // GetTask, CancelTask, SubscribeToTask, ListTasks, a card
	opSend                // SendMessage, SendStreamingMessage
	opPush                // Create/Get/List/DeleteTaskPushNotificationConfig, /tasks/{id}/pushNotificationConfigs
)

type op struct {
	kind opKind
	// stream is a request whose answer a client reads as an event stream.
	stream bool
	// known is a JSON-RPC request with one of A2A 1.0's methods (every HTTP+JSON request is read by its route).
	known bool
	// unreadable is a request with a body that is not one JSON object (an HTTP+JSON request other than a send).
	unreadable bool
	// taskIDs are the upstream task ids the request names (a route may name more than one candidate).
	taskIDs []string
}

// The members of a JSON-RPC request the node reads (D172): a lone case variant of one is refused, since a2a-go's
// handler decodes the request with encoding/json, which takes it for the member.
var rpcRequestReads = []strictjson.Path{{"jsonrpc"}, {"method"}, {"params"}, {"id"},
	{"params", "id"}, {"params", "taskId"}, {"params", "message"}, {"params", "metadata"}, {"params", "configuration"}}

// a2aMethods are A2A 1.0's JSON-RPC methods (a2a-go v2.7.0 internal/jsonrpc), each with the params member that
// names its task ("" none) and whether its answer is an event stream.
var a2aMethods = map[string]struct {
	kind   opKind
	task   string
	stream bool
}{
	"SendMessage":                      {kind: opSend},
	"SendStreamingMessage":             {kind: opSend, stream: true},
	"GetTask":                          {task: "id"},
	"CancelTask":                       {task: "id"},
	"SubscribeToTask":                  {task: "id", stream: true},
	"ListTasks":                        {},
	"GetExtendedAgentCard":             {},
	"CreateTaskPushNotificationConfig": {kind: opPush, task: "taskId"},
	"GetTaskPushNotificationConfig":    {kind: opPush, task: "taskId"},
	"ListTaskPushNotificationConfigs":  {kind: opPush, task: "taskId"},
	"DeleteTaskPushNotificationConfig": {kind: opPush, task: "taskId"},
}

// classify reads which A2A operation the request is. The route comes first: a body on .../message:send or
// .../message:stream is an HTTP+JSON send, read at HTTP+JSON's levels, whatever members it has (a "jsonrpc" member
// there is refused: a2a-go's HTTP+JSON handler ignores it, so the node and the service would read the body
// differently). Any other body that is one JSON object with an exact "jsonrpc" member is a JSON-RPC request; every
// level is read with exact keys, and a body that repeats a key, or holds a lone case variant of a member the node
// reads, is refused bad_header (D172), whatever the route.
func (x *Exchange) classify(req *http.Request, body []byte) (op, error) {
	p := req.URL.Path
	var top map[string]json.RawMessage
	unreadable := false
	if len(bytes.TrimSpace(body)) > 0 {
		m, err := strictjson.Members(body)
		var ke *strictjson.KeyError
		switch {
		case errors.As(err, &ke):
			return op{}, reject(sealed.RejectBadHeader, "a body with a repeated or case-variant key: "+err.Error())
		case err != nil:
			unreadable = true
		default:
			top = m
		}
	}
	if send, stream := sendRoute(p); send {
		if top == nil {
			return op{}, reject(sealed.RejectBadHeader, "a send request that is not one JSON object")
		}
		for k := range top {
			if strings.EqualFold(k, "jsonrpc") {
				return op{}, reject(sealed.RejectBadHeader, "an HTTP+JSON send request with a jsonrpc member")
			}
		}
		if err := strictjson.CheckMembers(top, a2awire.HTTPJSONRequestLevels...); err != nil {
			return op{}, reject(sealed.RejectBadHeader, "a send request that is not one with exact keys: "+err.Error())
		}
		return op{kind: opSend, stream: stream, known: true}, nil
	}
	if top != nil {
		if _, ok := top["jsonrpc"]; ok {
			x.jsonrpc = true
			x.rpcID = top["id"]
			if err := strictjson.CheckReads(top, a2awire.JSONRPCRequestLevels, rpcRequestReads); err != nil {
				return op{}, reject(sealed.RejectBadHeader, "a JSON-RPC request that is not one with exact keys: "+err.Error())
			}
			method, _ := a2awire.String(top, "method")
			m, ok := a2aMethods[method]
			if !ok {
				return op{}, nil
			}
			o := op{kind: m.kind, stream: m.stream, known: true}
			if m.task != "" {
				if params, err := strictjson.Members(top["params"]); err == nil {
					if id, _ := a2awire.String(params, m.task); id != "" {
						o.taskIDs = []string{id}
					}
				}
			}
			return o, nil
		}
		if err := strictjson.CheckReads(top, nil, rpcRequestReads[:4]); err != nil {
			return op{}, reject(sealed.RejectBadHeader, "a body with a case variant of a JSON-RPC member: "+err.Error())
		}
	}
	o := op{unreadable: unreadable}
	o.taskIDs, o.stream, o.kind = tasksRoute(p)
	return o, nil
}

// sendRoute reports an HTTP+JSON send route (.../message:send, .../message:stream), and whether it streams. Routes
// are matched without regard to case: a service's router may.
func sendRoute(p string) (send, stream bool) {
	lp := strings.ToLower(p)
	switch {
	case strings.HasSuffix(lp, "/message:send"):
		return true, false
	case strings.HasSuffix(lp, "/message:stream"):
		return true, true
	}
	return false, false
}

// tasksRoute reads an HTTP+JSON route under .../tasks/{id}: every task id a "/tasks/" segment of the path may name (a
// tenant prefix can hold one too; with and without a trailing ":action"), whether it subscribes, and whether it is a
// push config route (.../tasks/{id}/pushNotificationConfigs...).
func tasksRoute(p string) (ids []string, stream bool, kind opKind) {
	lp := strings.ToLower(p)
	for i := 0; ; {
		j := strings.Index(lp[i:], "/tasks/")
		if j < 0 {
			break
		}
		start := i + j + len("/tasks/")
		seg, tail, _ := strings.Cut(p[start:], "/")
		if seg != "" {
			ids = append(ids, seg)
			if k := strings.LastIndex(seg, ":"); k > 0 {
				ids = append(ids, seg[:k])
				if strings.EqualFold(seg[k+1:], "subscribe") {
					stream = true
				}
			}
		}
		if next, _, _ := strings.Cut(tail, "/"); strings.EqualFold(next, "pushNotificationConfigs") {
			kind = opPush
		}
		i = start - 1 // the segment's own slash may begin the next "/tasks/"
	}
	return ids, stream, kind
}

// guarded reports a service the node holds sealed state for (or one started with --sealed-required); h.mu is held.
func (h *Hosting) guarded(service string, svc *serviceState) bool {
	if h.Required(service) || len(svc.ReplyTo) > 0 || len(svc.Tasks) > 0 || len(svc.UpstreamContexts) > 0 {
		return true
	}
	prefix := flightKey("r", service)
	for k := range h.flight {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// other checks a request that carries no message: on a sealed job's exchange it may name only the job's upstream
// task (wrong_task); on any other exchange it may not name a sealed job's (wrong_task).
func (x *Exchange) other(svc *serviceState, taskIDs []string) error {
	j, err := x.h.jobFor(x.service, x.pre.Task)
	if err != nil {
		return err
	}
	if j != nil {
		x.job = j
		if len(taskIDs) > 0 && j.UpstreamTask != "" {
			if !slices.Contains(taskIDs, j.UpstreamTask) {
				return reject(sealed.RejectWrongTask, "the request names another upstream task than the job's")
			}
			for _, id := range taskIDs {
				if owner, ok := svc.Tasks[id]; ok && owner != j.ID {
					return reject(sealed.RejectWrongTask, "the request names another sealed job's upstream task")
				}
			}
		}
		return nil
	}
	for _, id := range taskIDs {
		if owner, ok := svc.Tasks[id]; ok && owner != x.pre.Task {
			return reject(sealed.RejectWrongTask, "the request names a sealed job's upstream task on another task's exchange")
		}
	}
	return nil
}

// opened is one sealed part of a request, opened.
type opened struct {
	index int // the part's index in the message
	hdr   sealed.Header
	body  []byte
	part  a2awire.Part
}

// send checks and unseals a send request's message (rules 1–5) and returns the body to forward. guarded is a
// service with sealed state: an inline push config is refused there, as on any sealed request.
func (x *Exchange) send(svc *serviceState, body []byte, guarded bool) ([]byte, error) {
	var (
		msg      map[string]json.RawMessage
		sealedP  []opened
		unsealed int
		visitErr error
		push     bool // configuration.taskPushNotificationConfig
	)
	ids := x.h.cfg.Identities()
	binding := a2awire.BindingHTTP
	if x.jsonrpc {
		binding = a2awire.BindingJSONRPC
	}
	w := a2awire.Walker{Binding: binding, Object: func(kind a2awire.Kind, m map[string]json.RawMessage) error {
		if kind != a2awire.KindRequest {
			return nil
		}
		if raw, ok := m["configuration"]; ok {
			if c, err := strictjson.Members(raw); err == nil {
				if v, ok := c["taskPushNotificationConfig"]; ok && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
					push = true
				}
			}
		}
		return nil
	}, Container: func(c *a2awire.Container) error {
		if c.Where != a2awire.WhereRequest {
			return nil
		}
		msg = c.Members
		for i, p := range c.Parts {
			sp, claims, err := sealed.ParseDeliveredPart(a2awire.Encode(p))
			switch {
			case claims && err != nil:
				return reject(sealed.RejectBadHeader, err.Error())
			case !claims:
				unsealed++
				continue
			case sp.BlobRef != "":
				// The proxy inlines kontra-blob: parts before sealing reads the body (internal/node): one still here was
				// not fetched, and a part is opened only once its whole file is authenticated.
				return reject(sealed.RejectBadHeader, "a sealed part whose blob was not fetched")
			}
			hdr, plain, err := sealed.Open(ids, sp.Ciphertext)
			if err != nil {
				return err
			}
			sealedP = append(sealedP, opened{index: i, hdr: hdr, body: plain, part: p})
		}
		if len(sealedP) == 0 {
			return nil
		}
		out := make([]a2awire.Part, len(c.Parts))
		copy(out, c.Parts)
		for _, o := range sealedP {
			pp, err := plainPart(o.hdr, o.body, o.part)
			if err != nil {
				return err
			}
			out[o.index] = pp
		}
		c.SetParts(out)
		return nil
	}}
	out, _, err := w.WalkRequest(body)
	if err == nil {
		err = visitErr
	}
	if err != nil {
		if _, ok := sealed.ReasonOf(err); ok {
			return nil, err
		}
		return nil, reject(sealed.RejectBadHeader, "a send request that is not an A2A 1.0 request with exact keys (D172)")
	}
	if msg == nil {
		return nil, reject(sealed.RejectBadHeader, "a send request without a message")
	}
	msgTask, _ := a2awire.String(msg, "taskId")
	msgCtx, _ := a2awire.String(msg, "contextId")
	msgID, _ := a2awire.String(msg, "messageId")
	if push && (guarded || len(sealedP) > 0) {
		// The service would post the task, its plaintext parts included, to a URL the gate chose (spec/relay.md
		// §10.1: the gate never sends the upstream a push config).
		return nil, reject(sealed.RejectBadHeader, "an inline push config, to a service with sealed jobs")
	}
	if len(sealedP) == 0 {
		return out, x.unsealedSend(svc, msgTask, msgCtx, unsealed)
	}
	if unsealed > 0 && slices.Contains(x.h.cfg.Required, x.service) {
		return nil, reject(sealed.RejectBadHeader, "an unsealed part beside sealed ones (--sealed-required)")
	}
	h, err := x.h.rule1(sealedP)
	if err != nil {
		return nil, err
	}
	if rt, ok := replyToMeta(msg); !ok || rt != h.ReplyTo {
		return nil, reject(sealed.RejectBadHeader, "metadata[\"kontra.sealed.replyTo\"] is not the header's replyTo")
	}
	if h.MessageID != msgID || msgID == "" {
		return nil, reject(sealed.RejectWrongMessage, "the header's messageId is not the message's")
	}
	if x.pre.Task == "" {
		return nil, reject(sealed.RejectWrongTask, "the preamble names no gate task")
	}
	if h.ContextID != x.pre.Context {
		return nil, reject(sealed.RejectWrongContext, "the header's contextId is not the preamble's gate context")
	}
	pairs := make([]string, len(sealedP))
	for i, o := range sealedP {
		pairs[i] = pairKey(o.hdr.MessageID, o.hdr.Part)
	}
	now := x.h.cfg.Now()
	j, err := x.h.jobFor(x.service, x.pre.Task)
	if err != nil {
		return nil, err
	}
	if h.TaskID == "" {
		// Rule 2: a new task's first message.
		if msgTask != "" {
			return nil, reject(sealed.RejectWrongTask, "a first message that names an upstream task")
		}
		if msgCtx != "" {
			if c, ok := svc.Contexts[x.pre.Context]; !ok || c.Upstream != msgCtx {
				return nil, reject(sealed.RejectWrongContext, "the message's contextId is not the upstream context recorded for the gate context")
			}
		}
		if e, ok := svc.ReplyTo[h.ReplyTo]; ok && e.Job != x.pre.Task {
			return nil, reject(sealed.RejectReplayed, "the replyTo opened a job for another gate task")
		}
		if owner, ok := x.h.inFlight(flightKey("r", x.service, h.ReplyTo)); ok && owner != x.pre.Task {
			return nil, reject(sealed.RejectReplayed, "the replyTo is opening a job for another gate task")
		}
		if j != nil && (j.ReplyTo != h.ReplyTo || j.RK != h.RK) {
			return nil, reject(sealed.RejectWrongJob, "the gate task's job was opened with another replyTo")
		}
	} else {
		// Rule 3: a follow-up of job T.
		if h.TaskID != x.pre.Task {
			return nil, reject(sealed.RejectWrongTask, "the header's taskId is not the preamble's task")
		}
		if j == nil || j.ReplyTo != h.ReplyTo || j.RK != h.RK {
			return nil, reject(sealed.RejectWrongJob, "no job of this replyTo and rk for the task")
		}
		if j.UpstreamTask == "" || msgTask != j.UpstreamTask {
			return nil, reject(sealed.RejectWrongTask, "the message's taskId is not the job's upstream task")
		}
		if j.UpstreamContext != "" && msgCtx != "" && msgCtx != j.UpstreamContext {
			return nil, reject(sealed.RejectWrongContext, "the message's contextId is not the job's upstream context")
		}
		if j.GateContext != "" && h.ContextID != j.GateContext {
			return nil, reject(sealed.RejectWrongContext, "the header's contextId is not the job's gate context")
		}
	}
	// Rule 5, for the pairs the job forwarded and for those a request of the job's has in flight.
	flight := make([]string, 0, len(pairs)+1)
	for _, pk := range pairs {
		if j != nil && slices.Contains(j.Pairs, pk) {
			return nil, reject(sealed.RejectReplayed, "a (messageId, part) pair the job forwarded before")
		}
		k := flightKey("p", x.service, x.pre.Task, pk)
		if _, ok := x.h.inFlight(k); ok {
			return nil, reject(sealed.RejectReplayed, "a (messageId, part) pair the job is forwarding")
		}
		flight = append(flight, k)
	}
	if j == nil {
		j = &jobState{ID: x.pre.Task, ReplyTo: h.ReplyTo, RK: h.RK, Created: now, GateContext: x.pre.Context,
			Artifacts: map[string]*artifactState{}, Tasks: map[string]*mcpTask{}}
	}
	x.job = j
	if h.TaskID == "" {
		flight = append(flight, flightKey("r", x.service, h.ReplyTo))
	}
	x.h.reserve(x.pre.Task, flight...)
	x.reserved = flight
	service, job := x.service, j
	x.commit = func() error {
		at := x.h.cfg.Now()
		job.Pairs = append(job.Pairs, pairs...)
		job.LastSeen = at
		svc.ReplyTo[h.ReplyTo] = replyEntry{Job: job.ID, At: at}
		if err := x.h.saveJob(service, job); err != nil {
			return err
		}
		return x.h.st.saveService(service, svc)
	}
	return out, nil
}

// unsealedSend is a send request with no sealed part: refused bad_header under --sealed-required; on a sealed job's
// exchange its replies are sealed all the same; it may not name another job's upstream task or another gate context's
// upstream context.
func (x *Exchange) unsealedSend(svc *serviceState, msgTask, msgCtx string, _ int) error {
	if slices.Contains(x.h.cfg.Required, x.service) {
		return reject(sealed.RejectBadHeader, "an unsealed request to a service started with --sealed-required")
	}
	j, err := x.h.jobFor(x.service, x.pre.Task)
	if err != nil {
		return err
	}
	if j != nil {
		x.job = j
		if msgTask != "" && j.UpstreamTask != "" && msgTask != j.UpstreamTask {
			return reject(sealed.RejectWrongTask, "the message names another upstream task than the job's")
		}
		return nil
	}
	if owner, ok := svc.Tasks[msgTask]; ok && msgTask != "" && owner != x.pre.Task {
		return reject(sealed.RejectWrongTask, "the message names a sealed job's upstream task")
	}
	if gc, ok := svc.UpstreamContexts[msgCtx]; ok && msgCtx != "" && gc != x.pre.Context {
		return reject(sealed.RejectWrongContext, "the message names a sealed job's upstream context")
	}
	return nil
}

// rule1 checks every sealed part of one message (rule 1, and that its parts agree): dir req; parts exactly
// 0..parts-1 once, in the order of the message (bad_header); the node's key id in kids (not_a_recipient); iat not
// more than 300 s ahead nor older than MaxAge (replayed). It returns the message's header (part 0's).
func (h *Hosting) rule1(ps []opened) (sealed.Header, error) {
	n := len(ps)
	first := ps[0].hdr
	mine := h.cfg.KeyIDs()
	now := h.cfg.Now().Unix()
	for i, o := range ps {
		hd := o.hdr
		switch {
		case hd.Dir != sealed.DirRequest:
			return sealed.Header{}, reject(sealed.RejectBadHeader, "a sealed part of a request whose header is not dir req")
		case hd.Parts != n || hd.Part != i:
			return sealed.Header{}, reject(sealed.RejectBadHeader, "the message's sealed parts are not exactly 0..parts-1 in order")
		case hd.ReplyTo == "" || hd.RK == "":
			return sealed.Header{}, reject(sealed.RejectBadHeader, "a request header without replyTo or rk")
		case hd.ReplyTo != first.ReplyTo || hd.RK != first.RK || hd.IAT != first.IAT || hd.TaskID != first.TaskID ||
			hd.ContextID != first.ContextID || hd.MessageID != first.MessageID || !slices.Equal(hd.Kids, first.Kids):
			return sealed.Header{}, reject(sealed.RejectBadHeader, "the sealed parts of one message disagree")
		case hd.Session != "" || hd.Tool != "" || hd.ArtifactID != "" || hd.Chunk != nil:
			return sealed.Header{}, reject(sealed.RejectBadHeader, "an A2A request header with members of another kind")
		}
	}
	if _, err := age.ParseX25519Recipient(first.ReplyTo); err != nil {
		return sealed.Header{}, reject(sealed.RejectBadHeader, "replyTo is not an X25519 recipient")
	}
	if !slices.ContainsFunc(first.Kids, func(k string) bool { return slices.Contains(mine, k) }) {
		return sealed.Header{}, reject(sealed.RejectNotRecipient, "the node's key id is not in kids")
	}
	if first.IAT > now+sealed.MaxClockSkew || now-first.IAT > int64(h.cfg.MaxAge/time.Second) {
		return sealed.Header{}, reject(sealed.RejectReplayed, "iat outside the node's window")
	}
	if first.ContextID == "" || first.MessageID == "" {
		return sealed.Header{}, reject(sealed.RejectBadHeader, "an A2A request header without contextId or messageId")
	}
	return first, nil
}

// ID is the request's JSON-RPC id (nil for HTTP+JSON), for the node's answer to a refused request.
func (x *Exchange) ID() json.RawMessage {
	if x == nil || !x.jsonrpc {
		return nil
	}
	return x.rpcID
}

// Response seals every part of the service's answer to the job's replyTo, with the reply header and auth,
// reusing the ciphertext of a part already sealed with the same header and body (so polling sees identical
// bytes); for an exchange whose request was not sealed it passes the answer unchanged. It also commits the job
// once the service has accepted the request (a 2xx), and releases what the request held in flight otherwise.
//
// On a sealed job's exchange, or one of a service with sealed jobs, every 2xx answer is read as the gate's client
// reads it, whatever it is labelled (a2a-go v2.7.0 a2aclient: a unary request's answer is decoded as JSON whatever
// its Content-Type; a stream's is read as SSE): a stream's text/event-stream answer event by event, any other as
// one JSON body, sealed, or refused bad_header when it is not one A2A 1.0 answer with exact keys (a body that is not
// JSON included). Only a stream's answer labelled text/event-stream is read as SSE, so no 2xx body reaches the gate
// unread.
func (h *Hosting) Response(ctx context.Context, x *Exchange, resp *http.Response) (*http.Response, error) {
	if x == nil {
		return resp, nil
	}
	if resp.StatusCode/100 != 2 {
		x.Release() // the service did not accept the request: nothing is recorded
	} else {
		h.mu.Lock()
		var err error
		if x.commit != nil && !x.done {
			x.done = true
			err = x.commit()
		}
		h.unreserve(x.reserved)
		x.reserved = nil
		h.mu.Unlock()
		if err != nil {
			// The service has the request; its replies are sealed all the same (x.job), and the job's file is
			// written again with them.
			h.log.Error("sealing: the job's state was not written", "service", x.service, "err", err)
		}
	}
	h.mu.Lock()
	svc, err := h.service(x.service)
	guard := err == nil && (len(svc.Tasks) > 0 || len(svc.UpstreamContexts) > 0)
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if x.job == nil && !guard {
		return resp, nil // nothing sealed on this service: the answer passes as it came
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case x.stream && a2awire.IsEventStream(ct):
		body := resp.Body
		pr, pw := io.Pipe()
		go func() {
			err := sseTransform(body, pw, func(data []byte) ([]byte, error) { return x.reply(data) })
			_ = body.Close()
			if err != nil {
				h.log.Warn("sealing: a service event stream ends before an event the node refused", "service", x.service,
					"reason", reasonText(err))
				err = nil // the stream ends cleanly before the event: nothing of it is forwarded
			}
			_ = pw.CloseWithError(err)
		}()
		resp.Body = pr
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return resp, nil
	case resp.StatusCode/100 != 2:
		return resp, nil // an error: no client reads it as an answer
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return refuseResponse(resp, x, reject(sealed.RejectBadHeader, "a reply over the node's bound")), nil
	}
	if len(bytes.TrimSpace(b)) == 0 {
		setBody(resp, b)
		return resp, nil
	}
	out, err := x.reply(b)
	if err != nil {
		h.log.Warn("sealing: a service reply is refused", "service", x.service, "reason", reasonText(err))
		return refuseResponse(resp, x, err), nil
	}
	if !a2awire.IsJSON(ct) {
		resp.Header.Set("Content-Type", "application/json") // what it was read as
	}
	setBody(resp, out)
	return resp, nil
}

// reply checks and seals one reply body (a JSON response, or one SSE event's data).
func (x *Exchange) reply(b []byte) ([]byte, error) {
	h := x.h
	h.mu.Lock()
	defer h.mu.Unlock()
	svc, err := h.service(x.service)
	if err != nil {
		return nil, err
	}
	j := x.job
	if j != nil {
		// Reload what the job holds now (another exchange of the task may have sealed meanwhile).
		if cur, err := h.jobFor(x.service, j.ID); err == nil && cur != nil {
			j, x.job = cur, cur
		}
	}
	changedIndex := false
	binding := a2awire.BindingHTTP
	if x.jsonrpc {
		binding = a2awire.BindingJSONRPC
	}
	w := a2awire.Walker{
		Binding: binding,
		Object: func(kind a2awire.Kind, m map[string]json.RawMessage) error {
			taskKey, ctxKey := "taskId", "contextId"
			if kind == a2awire.KindTask {
				taskKey = "id"
			}
			task, _ := a2awire.String(m, taskKey)
			cctx, _ := a2awire.String(m, ctxKey)
			if j == nil {
				// An unsealed exchange may not carry a sealed job's task or context.
				if owner, ok := svc.Tasks[task]; ok && task != "" && owner != x.pre.Task {
					return reject(sealed.RejectWrongTask, "a reply names a sealed job's upstream task on another exchange")
				}
				if gc, ok := svc.UpstreamContexts[cctx]; ok && cctx != "" && gc != x.pre.Context {
					return reject(sealed.RejectWrongContext, "a reply names a sealed job's upstream context on another exchange")
				}
				return nil
			}
			if task != "" {
				if owner, ok := svc.Tasks[task]; ok && owner != j.ID {
					return reject(sealed.RejectWrongTask, "a reply names another sealed job's upstream task")
				}
				switch {
				case j.UpstreamTask == "":
					j.UpstreamTask = task
					svc.Tasks[task] = j.ID
					changedIndex = true
				case task != j.UpstreamTask:
					return reject(sealed.RejectWrongTask, "a reply names another upstream task than the job's")
				}
			}
			if cctx != "" {
				if gc, ok := svc.UpstreamContexts[cctx]; ok && gc != j.GateContext {
					return reject(sealed.RejectWrongContext, "a reply names another gate context's upstream context")
				}
				switch {
				case j.UpstreamContext == "":
					j.UpstreamContext = cctx
					svc.UpstreamContexts[cctx] = j.GateContext
					if j.GateContext != "" {
						svc.Contexts[j.GateContext] = contextEntry{Upstream: cctx, At: h.cfg.Now()}
					}
					changedIndex = true
				case cctx != j.UpstreamContext:
					return reject(sealed.RejectWrongContext, "a reply names another upstream context than the job's")
				}
			}
			return nil
		},
	}
	if j != nil {
		w.Container = func(c *a2awire.Container) error { return x.sealContainer(j, c) }
	}
	out, changed, err := w.WalkReply(b)
	if err != nil {
		if _, ok := sealed.ReasonOf(err); ok {
			return nil, err
		}
		// The error is not quoted: a service's reply is plaintext, and a JSON error may hold a byte of it (the node
		// never logs a plaintext part, sealed_no_plaintext).
		return nil, reject(sealed.RejectBadHeader, "a reply that is not an A2A 1.0 answer with exact keys (D172)")
	}
	if j != nil {
		j.LastSeen = h.cfg.Now()
		if err := h.saveJob(x.service, j); err != nil {
			return nil, err
		}
	}
	if changedIndex {
		if err := h.st.saveService(x.service, svc); err != nil {
			return nil, err
		}
	}
	if !changed {
		return b, nil
	}
	return out, nil
}

// sealContainer seals every part of one message or artifact of a reply to the job's replyTo (§5): a message's parts
// with its messageId; an artifact update's with the artifactId and the next chunk; a whole artifact's (a Task's)
// reusing the ciphertext of the parts the gate already holds unchanged and sealing the rest as the next chunk.
func (x *Exchange) sealContainer(j *jobState, c *a2awire.Container) error {
	if len(c.Parts) == 0 {
		return nil
	}
	base := sealed.Header{V: sealed.Version, Dir: sealed.DirReply, TaskID: x.pre.Task, ContextID: j.GateContext}
	if base.TaskID == "" {
		base.TaskID = j.ID
	}
	plains := make([]plainOf, len(c.Parts))
	for i, p := range c.Parts {
		if _, claims, _ := sealed.ParseDeliveredPart(a2awire.Encode(p)); claims {
			return reject(sealed.RejectBadHeader, "the service sent a part with the sealed envelope type")
		}
		pl, err := plainOfPart(p)
		if err != nil {
			return err
		}
		plains[i] = pl
	}
	out := make([]a2awire.Part, len(c.Parts))
	switch c.Where {
	case a2awire.WhereArtifact, a2awire.WhereArtifactUpdate:
		aid, ok := a2awire.String(c.Members, "artifactId")
		if !ok || aid == "" {
			return reject(sealed.RejectBadHeader, "an artifact without an artifactId")
		}
		st := j.Artifacts[aid]
		if st == nil {
			st = &artifactState{}
			j.Artifacts[aid] = st
		}
		base.ArtifactID = aid
		var seal []int // the indexes to seal as a new chunk
		if c.Where == a2awire.WhereArtifactUpdate {
			appendUpdate := false
			if raw, ok := c.Owner["append"]; ok {
				appendUpdate = bytes.Equal(bytes.TrimSpace(raw), []byte("true"))
			}
			if !appendUpdate {
				st.Held = nil
			}
			for i := range plains {
				seal = append(seal, i)
			}
		} else {
			reuse := len(st.Held) <= len(plains)
			for i := 0; reuse && i < len(st.Held); i++ {
				reuse = st.Held[i].Plain == plains[i].hash
			}
			if !reuse {
				st.Held = nil
			}
			for i := range plains {
				if i < len(st.Held) {
					ct, ok := x.h.st.ciphertext(x.service, j.ID, st.Held[i].CT)
					if !ok {
						// The cached ciphertext is gone: the artifact is sealed again as a new chunk.
						st.Held, seal = nil, nil
						for k := range plains {
							seal = append(seal, k)
						}
						break
					}
					out[i] = sealedPartOf(ct, c.Parts[i], plains[i])
					continue
				}
				seal = append(seal, i)
			}
		}
		if len(seal) > 0 {
			chunk := st.Next
			st.Next++
			for k, i := range seal {
				hd := base
				hd.Chunk = &chunk
				hd.Part, hd.Parts = k, len(seal)
				ct, key, err := x.sealOne(j, hd, plains[i])
				if err != nil {
					return err
				}
				out[i] = sealedPartOf(ct, c.Parts[i], plains[i])
				st.Held = append(st.Held, heldPart{Chunk: chunk, Part: k, Parts: len(seal), Plain: plains[i].hash, CT: key})
			}
		}
	default:
		mid, ok := a2awire.String(c.Members, "messageId")
		if !ok || mid == "" {
			return reject(sealed.RejectBadHeader, "a message without a messageId")
		}
		base.MessageID = mid
		for i := range plains {
			hd := base
			hd.Part, hd.Parts = i, len(plains)
			ct, _, err := x.sealOne(j, hd, plains[i])
			if err != nil {
				return err
			}
			out[i] = sealedPartOf(ct, c.Parts[i], plains[i])
		}
	}
	c.SetParts(out)
	return nil
}

// sealOne seals one plaintext part to the job's replyTo with auth, reusing the ciphertext of the same header and
// body when the job sealed it before; it returns the ciphertext and its cache key.
func (x *Exchange) sealOne(j *jobState, hd sealed.Header, pl plainOf) ([]byte, string, error) {
	rcpt, err := age.ParseX25519Recipient(j.ReplyTo)
	if err != nil {
		return nil, "", fmt.Errorf("sealing: the job's replyTo: %w", err)
	}
	rk, err := sealed.DecodeRK(j.RK)
	if err != nil {
		return nil, "", fmt.Errorf("sealing: the job's rk: %w", err)
	}
	hd.Kind, hd.MediaType = pl.kind, pl.mediaType
	hd.Kids = []string{tunnel.KeyID(rcpt.String())}
	hd.Auth = ""
	canon, err := hd.Canonical()
	if err != nil {
		return nil, "", err
	}
	key := cacheKey(canon, pl.body)
	if ct, ok := x.h.st.ciphertext(x.service, j.ID, key); ok {
		return ct, key, nil
	}
	if hd.Auth, err = sealed.Auth(rk, hd, pl.body); err != nil {
		return nil, "", err
	}
	ct, err := sealed.Seal([]age.Recipient{rcpt}, hd, pl.body)
	if err != nil {
		return nil, "", err
	}
	if err := x.h.st.keepCiphertext(x.service, j.ID, key, ct); err != nil {
		return nil, "", err
	}
	return ct, key, nil
}

// service returns the service's state, loading it once; h.mu is held.
func (h *Hosting) service(name string) (*serviceState, error) {
	if s := h.services[name]; s != nil {
		return s, nil
	}
	s, err := h.st.loadService(name)
	if err != nil {
		return nil, err
	}
	h.services[name] = s
	return s, nil
}

// jobFor returns the job id of the service, nil when it has none; h.mu is held.
func (h *Hosting) jobFor(service, id string) (*jobState, error) {
	if id == "" || element("job", id) != nil {
		return nil, nil
	}
	k := service + "/" + id
	if j := h.jobs[k]; j != nil {
		return j, nil
	}
	j, err := h.st.loadJob(service, id)
	if errors.Is(err, errNoJob) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.jobs[k] = j
	return j, nil
}

// saveJob writes a job through; h.mu is held.
func (h *Hosting) saveJob(service string, j *jobState) error {
	h.jobs[service+"/"+j.ID] = j
	return h.st.saveJob(service, j)
}

// Sweep drops jobs JobRetention after their last exchange, and the per-service entries older than MaxAge (and the
// clock skew rule 1 allows) that no kept job needs.
func (h *Hosting) Sweep(now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	services, err := h.st.services()
	if err != nil {
		return err
	}
	var errs []error
	keepFor := h.cfg.MaxAge + sealed.MaxClockSkew*time.Second
	for _, name := range services {
		svc, err := h.service(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		jobs, err := h.st.jobs(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		gone := map[string]bool{}
		for _, id := range jobs {
			j, err := h.jobFor(name, id)
			if err != nil || j == nil {
				continue
			}
			last := j.LastSeen
			if last.IsZero() {
				last = j.Created
			}
			if now.Sub(last) < JobRetention {
				continue
			}
			if err := h.st.removeJob(name, id); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(h.jobs, name+"/"+id)
			gone[id] = true
		}
		for k, v := range svc.ReplyTo {
			if now.Sub(v.At) > keepFor || gone[v.Job] {
				delete(svc.ReplyTo, k)
			}
		}
		for k, v := range svc.MCPCalls {
			if now.Sub(v) > keepFor {
				delete(svc.MCPCalls, k)
			}
		}
		for k, v := range svc.Contexts {
			if now.Sub(v.At) > JobRetention {
				delete(svc.Contexts, k)
			}
		}
		for k, v := range svc.Tasks {
			if gone[v] {
				delete(svc.Tasks, k)
			}
		}
		if err := h.st.saveService(name, svc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Required reports whether the node refuses an unsealed request to service (--sealed-required).
func (h *Hosting) Required(service string) bool { return slices.Contains(h.cfg.Required, service) }

func pairKey(messageID string, part int) string { return fmt.Sprintf("%s\x00%d", messageID, part) }

// replyToMeta is the message's metadata["kontra.sealed.replyTo"].
func replyToMeta(msg map[string]json.RawMessage) (string, bool) {
	raw, ok := msg["metadata"]
	if !ok {
		return "", false
	}
	m, err := strictjson.Members(raw)
	if err != nil {
		return "", false
	}
	return a2awire.String(m, sealed.MetadataReplyTo)
}

func reject(r sealed.Reject, detail string) *sealed.RejectError {
	return &sealed.RejectError{Reason: r, Detail: detail}
}

// reasonText is a refusal's reason (and detail, which never quotes a plaintext) for the node's log.
func reasonText(err error) string {
	var re *sealed.RejectError
	if errors.As(err, &re) {
		if re.Detail != "" {
			return string(re.Reason) + ": " + re.Detail
		}
		return string(re.Reason)
	}
	return err.Error()
}

func setBody(res *http.Response, b []byte) {
	res.Body = io.NopCloser(bytes.NewReader(b))
	res.ContentLength = int64(len(b))
	res.Header.Set("Content-Length", fmt.Sprint(len(b)))
	res.TransferEncoding = nil
}

// refuseResponse replaces a service's reply the node refused with the node's own answer (§6): nothing of the
// reply is forwarded.
func refuseResponse(resp *http.Response, x *Exchange, err error) *http.Response {
	var id json.RawMessage
	if x != nil && x.jsonrpc {
		id = x.rpcID
	}
	status, hdr, body := RejectAnswer(id, err)
	resp.StatusCode, resp.Status = status, fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Header = hdr
	resp.Trailer = nil
	setBody(resp, body)
	return resp
}

// RejectAnswer is a hosting node's refusal of a sealed request (ext-sealed.md §6): 400, Kontra-Node:
// sealed_rejected, Kontra-Sealed-Reject: <reason>, and a JSON-RPC error body (-32602, "sealed part rejected:
// <reason>") with the request's id when it had one. An error that is not a *sealed.RejectError is bad_header.
func RejectAnswer(id json.RawMessage, err error) (int, http.Header, []byte) {
	reason, ok := sealed.ReasonOf(err)
	if !ok {
		reason = sealed.RejectBadHeader
	}
	if len(bytes.TrimSpace(id)) == 0 {
		id = json.RawMessage("null")
	}
	body, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{-32602, "sealed part rejected: " + string(reason)}})
	h := http.Header{}
	h.Set(tunnel.HeaderNode, string(tunnel.NodeSealedRejected))
	h.Set(tunnel.HeaderSealedReject, string(reason))
	h.Set("Content-Type", "application/json")
	h.Set("Connection", "close")
	return http.StatusBadRequest, h, body
}

// WriteReject writes RejectAnswer to w.
func WriteReject(w http.ResponseWriter, id json.RawMessage, err error) {
	status, hdr, body := RejectAnswer(id, err)
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	for k, v := range hdr {
		h[k] = v
	}
	h.Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
