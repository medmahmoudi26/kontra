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
	"sync/atomic"
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

// Hosting is the hosting node's sealing, keyed per service and per job (spec §5 "Hosting node"): per job (the gate task
// id; for plain MCP calls, the session hash) the replyTo and rk of its first sealed request, the upstream task and
// context ids, the (messageId, part) pairs forwarded, and per gate task the MCP input requests it sealed and has not
// seen answered (InputRequest); per service every replyTo that opened a job, the upstream context id recorded per gate
// context, and for MCP every (session, messageId) forwarded. A job is opened, and its pairs recorded, only once the
// service has accepted the request; until then what a request will record is held in flight (hostedService.flight, and
// Hosting.nflight for the node-wide keys), so that a second copy sent while the first is in flight is replayed too, and
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
// SPEC-CONFLICT (D217; ext-sealed.md §5, rules 1–5): the rules bind the sealed parts of a request, and say nothing
// of an exchange without one (a GetTask, a CancelTask, a SubscribeToTask, an unsealed message) that names a sealed
// job's upstream task: its replies would be sealed to the replyTo of whatever job the preamble names, or not at all,
// so a gate could read a sealed task's content by asking for it on an exchange of a job of its own, or of none. The
// closest faithful behaviour: the node never lets an exchange reach a sealed job's upstream task or context unless
// the exchange is that job's (its preamble names the job's gate task, or for MCP its session): a request naming
// another job's upstream task id is wrong_task, one naming another gate context's upstream context wrong_context,
// and a service reply naming a sealed job's upstream task on an exchange of another job, or of none, is refused
// before anything of it is forwarded (§6's reasons). For the same reason a service with sealed jobs (or started with
// --sealed-required) gets nothing the node cannot read: a push config, inline or by method or route, is refused
// bad_header (it would have the service post a sealed task's plaintext to a URL the gate chose; the gate never sends
// one upstream, relay.md §10.1), as are a JSON-RPC request that is none of A2A 1.0's methods and a body that is not
// one JSON object; and a sealed job's MCP tasks keep their statusMessage sealed on every session's exchanges.
//
// Locking (M3 review robustness-4, D225): each hosted service's state is guarded by a lock of its own, under which its
// exchanges check and record and its commits append, sync and write its files, so that one service's disk never holds
// up another's exchanges; the node-wide replay checks (D221) are guarded by a lock of their own, over an index of the
// replyTos and MCP (session, messageId)s each service recorded, which is filled from every service's state on disk
// when the node starts. A service's lock is always taken before the node-wide one.
type Hosting struct {
	cfg HostingConfig
	log *slog.Logger
	st  *fileStore

	// Each hosted service's state has a lock of its own (hostedService.mu), under which its exchanges check and
	// record, and its commits append, sync and write its job files: one service's writes never wait for another's
	// (M3 review robustness-4, D225). smu guards the map only.
	smu  sync.Mutex
	svcs map[string]*hostedService

	// The node-wide replay checks (rule 2's replyTo and rule 5's MCP (session, messageId), across every service of the
	// node, D221) have a lock of their own: nmu guards their in-flight keys (flightKey's kinds "r" and "m", service "")
	// and the index of what each service recorded, which a service's exchange reads without another service's lock.
	// Lock order: a service's lock, then nmu; never the other way.
	nmu      sync.Mutex
	nflight  map[string]flightRef
	nReplyTo map[string]string // replyTo -> the service that recorded it
	nCalls   map[string]string // "<session>/<messageId>" -> the service that forwarded it
	// beforeClaim, when a test sets it, runs as an exchange is about to make a node-wide claim (claimNode), after its
	// own checks: a test holds two services' exchanges there to race their claims.
	beforeClaim func(kind string)

	// paths are the hosted services as the gate's URL path names them (SetServicePaths): what a sealed header's
	// service must be (D221).
	paths atomic.Pointer[map[string]string]
}

// SetServicePaths records how the gate's URL paths name the hosted services (spec/relay.md §2.1: "<name>" on a
// single-tenant gate, "<tenant>/<name>" on a multi-tenant one), read by the node from its welcome: a sealed header's
// service must be the path of the service the preamble names (D221). A service it does not name is "<name>".
func (h *Hosting) SetServicePaths(paths map[string]string) {
	m := make(map[string]string, len(paths))
	if cur := h.paths.Load(); cur != nil {
		for k, v := range *cur {
			m[k] = v
		}
	}
	for k, v := range paths {
		m[k] = v
	}
	h.paths.Store(&m)
}

// servicePath is the hosted service name as the gate's URL path names it (D221).
func (h *Hosting) servicePath(name string) string {
	if m := h.paths.Load(); m != nil {
		if p, ok := (*m)[name]; ok {
			return p
		}
	}
	return name
}

// checkService is D221's check of a request header: its service is the one the preamble names, as the gate's URL
// path names it. A sealed request the buyer sealed for another service of this node is that service's job's, never
// this one's (wrong_job).
func (h *Hosting) checkService(hd sealed.Header, service string) error {
	if hd.Service == "" || hd.Service != h.servicePath(service) {
		return reject(sealed.RejectWrongJob, "the header names another service than the preamble's")
	}
	return nil
}

// flightRef is one in-flight key: who holds it (for "r" the service and gate task, for "j" the replyTo and rk the
// job opens with) and how many exchanges do.
type flightRef struct {
	owner string
	n     int
}

// hostedService is one hosted service's state on the node and the lock that guards it.
type hostedService struct {
	mu    sync.Mutex
	state *serviceState        // loaded service.json and service.log, written through; nil until loaded
	jobs  map[string]*jobState // loaded job files by job id, written through
	// flight holds what a request the service has not accepted yet will record once it does (rule 5's pairs, rule 4b's
	// answered input request, the binding a new job opens with, the upstream session a sealed call opens a job's on),
	// by flightKey, with its owner: a second copy in flight is refused as a replay of the first, and a second binding
	// for a job being opened as wrong_job, as each would be once the first is recorded. An exchange releases its keys
	// when the service accepts it (they are then recorded) or the node answers it itself (nothing is recorded: the
	// gate's retry is not a replay). The node-wide keys are Hosting.nflight.
	flight map[string]flightRef
	// opening counts the exchanges opening a sealed job whose upstream task the node has not read yet (a first message
	// in flight, or accepted and not answered): while one is, a reply on an exchange of no sealed job that names an
	// upstream task or context the node has not indexed may be that job's, and waits (holdReply, D227). openingDone is
	// closed, and replaced, whenever one ends.
	opening     int
	openingDone chan struct{}
}

// svc is the service's hostedService, made on first use.
func (h *Hosting) svc(name string) *hostedService {
	h.smu.Lock()
	defer h.smu.Unlock()
	hs := h.svcs[name]
	if hs == nil {
		hs = &hostedService{jobs: map[string]*jobState{}, flight: map[string]flightRef{}, openingDone: make(chan struct{})}
		h.svcs[name] = hs
	}
	return hs
}

// lock locks the service's state and returns it; the caller unlocks hs.mu.
func (h *Hosting) lock(name string) *hostedService {
	hs := h.svc(name)
	hs.mu.Lock()
	return hs
}

// loadedJobs is the number of jobs held in memory, every service's (tests).
func (h *Hosting) loadedJobs() int {
	h.smu.Lock()
	all := make([]*hostedService, 0, len(h.svcs))
	for _, hs := range h.svcs {
		all = append(all, hs)
	}
	h.smu.Unlock()
	n := 0
	for _, hs := range all {
		hs.mu.Lock()
		n += len(hs.jobs)
		hs.mu.Unlock()
	}
	return n
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
	h := &Hosting{cfg: cfg, log: log.With("component", "sealing-hosting"), st: &fileStore{dir: cfg.Dir},
		svcs: map[string]*hostedService{}, nflight: map[string]flightRef{}, nReplyTo: map[string]string{},
		nCalls: map[string]string{}}
	// Every service's state is read now, so that the node-wide replay checks know what each service recorded before
	// any exchange (a service whose state cannot be read is read again, and refused, when an exchange names it).
	names, err := h.st.services()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		hs := h.lock(name)
		if _, err := h.service(name); err != nil {
			h.log.Warn("sealing: a hosted service's state could not be read", "service", name, "err", err)
		}
		hs.mu.Unlock()
	}
	return h, nil
}

// flightKey is the in-flight key of what a request records: kind "r" a replyTo (node-wide: service ""), "p" a job's
// (messageId, part) pair, "m" an MCP (session, messageId) (node-wide), "a" an MCP input request's answer, "j" the
// replyTo and rk a job is being opened with (sealing-spec: one binding per job, in flight as once recorded).
func flightKey(kind, service string, parts ...string) string {
	return kind + "\x00" + service + "\x00" + strings.Join(parts, "\x00")
}

// binding is a job's replyTo and rk as one value (the "j" key's owner).
func bindOf(replyTo, rk string) string { return replyTo + "\x00" + rk }

// flightOf is the in-flight map key belongs to, and the lock that guards it beyond the caller's: a node-wide key's
// (service "") is nflight under nmu, a service's that service's flight, whose lock the caller holds.
func (h *Hosting) flightOf(key string) (map[string]flightRef, *sync.Mutex) {
	_, rest, _ := strings.Cut(key, "\x00")
	service, _, _ := strings.Cut(rest, "\x00")
	if service == "" {
		return h.nflight, &h.nmu
	}
	return h.svc(service).flight, nil
}

// reserve records keys in flight for owner; the lock of each key's service is held.
func (h *Hosting) reserve(owner string, keys ...string) {
	for _, k := range keys {
		m, mu := h.flightOf(k)
		if mu != nil {
			mu.Lock()
		}
		r := m[k]
		r.owner = owner
		r.n++
		m[k] = r
		if mu != nil {
			mu.Unlock()
		}
	}
}

// unreserve drops one hold of each key; the lock of each key's service is held.
func (h *Hosting) unreserve(keys []string) {
	for _, k := range keys {
		m, mu := h.flightOf(k)
		if mu != nil {
			mu.Lock()
		}
		if r, ok := m[k]; ok {
			if r.n--; r.n <= 0 {
				delete(m, k)
			} else {
				m[k] = r
			}
		}
		if mu != nil {
			mu.Unlock()
		}
	}
}

// inFlight returns the gate task holding key, ok false when no exchange does; the lock of key's service is held.
func (h *Hosting) inFlight(key string) (string, bool) {
	m, mu := h.flightOf(key)
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	r, ok := m[key]
	return r.owner, ok
}

// claimNode is the node-wide replay check and its reservation in one step under nmu (D221: rule 2's replyTo, rule 5's
// MCP (session, messageId)): false when index records id for another service than service, or another owner holds key
// in flight; otherwise owner holds key. Checked and reserved at once, so that two services' exchanges of one request
// cannot both pass.
func (h *Hosting) claimNode(key, owner string, index map[string]string, id, service string) bool {
	if h.beforeClaim != nil {
		kind, _, _ := strings.Cut(key, "\x00")
		h.beforeClaim(kind)
	}
	h.nmu.Lock()
	defer h.nmu.Unlock()
	if s, ok := index[id]; ok && s != service {
		return false
	}
	if r, ok := h.nflight[key]; ok && r.owner != owner {
		return false
	}
	r := h.nflight[key]
	r.owner = owner
	r.n++
	h.nflight[key] = r
	return true
}

// indexNode records that service holds id in a node-wide set (setReplyTo or setMCPCalls), or no longer does (put
// false), for the checks of other services' exchanges. The service's lock is held.
func (h *Hosting) indexNode(set, id, service string, put bool) {
	h.nmu.Lock()
	defer h.nmu.Unlock()
	index := h.nReplyTo
	if set == setMCPCalls {
		index = h.nCalls
	}
	cur, ok := index[id]
	switch {
	case put && !ok:
		index[id] = service
	case !put && ok && cur == service:
		delete(index, id)
	}
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
	// name a sealed job's upstream task). bind is the replyTo and rk they are sealed to: those of the request the
	// exchange carried (or, on an exchange that carries none, the job's when the request came), never a job's that
	// another exchange recorded since (reply refuses a job bound otherwise, wrong_job).
	job  *jobState
	bind string
	// commit is what the service's acceptance records (rule 2's new job, rule 5's pairs); reserved are its in-flight
	// keys (hostedService.flight, Hosting.nflight) until then.
	commit   func() error
	reserved []string
	done     bool
	// opening is an exchange counted in Hosting.opening (it opens a job whose upstream task is not known yet).
	opening bool
	// handed is set once the forwarded body was read whole (by the transport, to the service): a request whose
	// exchange then ends without an answer was the service's to act on, and is recorded (End).
	handed atomic.Bool
}

// handedBody is a forwarded request body that marks its exchange handed once it was read whole.
type handedBody struct {
	r *bytes.Reader
	x *Exchange
}

func (b *handedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if b.r.Len() == 0 {
		b.x.handed.Store(true)
	}
	return n, err
}

func (b *handedBody) Close() error { return nil }

// endOpening drops the exchange from its service's opening count; the service's lock is held.
func (x *Exchange) endOpening() {
	if !x.opening {
		return
	}
	x.opening = false
	hs := x.h.svc(x.service)
	if hs.opening--; hs.opening < 0 {
		hs.opening = 0
	}
	close(hs.openingDone)
	hs.openingDone = make(chan struct{})
}

// Release drops what the exchange holds in flight without recording it: the node answered the request itself (the
// service did not answer, or answered it with an error), so the gate's retry of it is not a replay (spec §5). After
// the service accepted the request (Response with a 2xx) it does nothing. The proxy calls it when it answers an
// exchange itself; Request also arranges it for when the request's context ends.
func (x *Exchange) Release() {
	if x == nil {
		return
	}
	hs := x.h.lock(x.service)
	defer hs.mu.Unlock()
	x.h.unreserve(x.reserved)
	x.reserved = nil
	if !x.done {
		x.commit = nil
	}
	x.endOpening()
}

// End is the exchange's end without an answer of the node's: the request's context ended (the gate ended the tunnel
// stream, or the tunnel went) before the service answered. A request the node had handed to the service whole is
// the service's to act on, so it is recorded as an accepted one is (its job, replyTo and pairs: the gate's retry is
// then a replay, and the gate cannot open the gate task with a binding of its own); one never handed records nothing,
// as Release (D227). After Response or Release it does nothing more.
func (x *Exchange) End() {
	if x == nil {
		return
	}
	if !x.handed.Load() {
		x.Release()
		return
	}
	hs := x.h.lock(x.service)
	defer hs.mu.Unlock()
	if x.commit != nil && !x.done {
		x.done = true
		if err := x.commit(); err != nil {
			x.h.log.Error("sealing: the job's state was not written", "service", x.service, "err", err)
		}
	}
	x.h.unreserve(x.reserved)
	x.reserved = nil
	x.endOpening()
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
	hs := h.lock(x.service)
	defer hs.mu.Unlock()
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
	case guarded && op.list:
		// The gate never lists the upstream's tasks (it holds them, relay.md §6): a list would hand it every task of the
		// service, a sealed job's that the node has not indexed yet included (D227).
		return nil, x, reject(sealed.RejectBadHeader, "a task list, to a service with sealed jobs")
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
	if len(x.reserved) > 0 || x.opening {
		context.AfterFunc(ctx, x.End) // a request whose exchange ends unanswered: recorded once it was handed (End)
	}
	req.Body = &handedBody{r: bytes.NewReader(body), x: x}
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
	// list is a ListTasks (JSON-RPC, or HTTP+JSON's GET .../tasks).
	list bool
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
			o := op{kind: m.kind, stream: m.stream, known: true, list: method == "ListTasks"}
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
	if lp := strings.ToLower(strings.TrimRight(p, "/")); lp == "/tasks" || strings.HasSuffix(lp, "/tasks") {
		o.list = true
	}
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

// guarded reports a service the node holds sealed state for (or one started with --sealed-required); the service's
// lock is held.
func (h *Hosting) guarded(service string, svc *serviceState) bool {
	if h.Required(service) || len(svc.ReplyTo) > 0 || len(svc.Tasks) > 0 || len(svc.UpstreamContexts) > 0 {
		return true
	}
	prefix := flightKey("j", service)
	for k := range h.svc(service).flight {
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
	if j == nil {
		if _, opening := x.h.inFlight(flightKey("j", x.service, x.pre.Task)); opening {
			// The gate task's first sealed message is with the service, unanswered: its job's upstream task is not known,
			// so a request of the task could reach it unsealed. The gate asks for a task only once it has its first
			// upstream id (relay.md §6.3), which comes with the answer (D227).
			return reject(sealed.RejectWrongTask, "a request of a gate task whose first sealed message the service has not answered")
		}
	}
	if j != nil {
		x.job, x.bind = j, bindOf(j.ReplyTo, j.RK)
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
	if unsealed > 0 {
		// A message with a sealed part is the hiring node's, which seals every part of it (§5, hiring rule 3): an
		// unsealed part beside sealed ones is the gate's, written into the buyer's message (an instruction to the
		// service about the buyer's sealed content), whatever --sealed-required says (security review: the A2A half of
		// D175's "answers must be sealed").
		return nil, reject(sealed.RejectBadHeader, "an unsealed part beside sealed ones")
	}
	h, err := x.h.rule1(sealedP)
	if err != nil {
		return nil, err
	}
	if err := x.h.checkService(h, x.service); err != nil {
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
		if x.h.replyToElsewhere(x.service, h.ReplyTo) {
			return nil, reject(sealed.RejectReplayed, "the replyTo opened a job of another service of this node")
		}
		if owner, ok := x.h.inFlight(flightKey("r", "", h.ReplyTo)); ok && owner != x.service+"\x00"+x.pre.Task {
			return nil, reject(sealed.RejectReplayed, "the replyTo is opening a job for another gate task")
		}
		if j != nil && (j.ReplyTo != h.ReplyTo || j.RK != h.RK) {
			return nil, reject(sealed.RejectWrongJob, "the gate task's job was opened with another replyTo")
		}
		// The job's binding is the first request's from the node's checks on, not from its record only: a first message
		// of another replyTo for the same gate task while one is in flight would otherwise pass too, and whichever the
		// service accepted last would bind the job, so that the buyer's replies were sealed to the other's key (rules 2–4,
		// §7 "cannot continue another buyer's job").
		if b, ok := x.h.inFlight(flightKey("j", x.service, x.pre.Task)); ok && b != bindOf(h.ReplyTo, h.RK) {
			return nil, reject(sealed.RejectWrongJob, "the gate task's job is being opened with another replyTo")
		}
	} else {
		// Rule 3: a follow-up of job T. A header naming another task than the preamble's is a follow-up sent into
		// another job: wrong_job, as rule 3 and sealed_replay say.
		if h.TaskID != x.pre.Task {
			return nil, reject(sealed.RejectWrongJob, "the header's taskId is not the preamble's task")
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
	if h.TaskID == "" {
		// Rule 2's node-wide check, checked again and reserved in one step under the node-wide lock: another service's
		// exchange of the same first message may be between its checks and its reservation (D221).
		rk := flightKey("r", "", h.ReplyTo)
		if !x.h.claimNode(rk, x.service+"\x00"+x.pre.Task, x.h.nReplyTo, h.ReplyTo, x.service) {
			return nil, reject(sealed.RejectReplayed, "the replyTo opened, or is opening, a job of another service of this node")
		}
		x.reserved = append(x.reserved, rk)
	}
	x.h.reserve(x.pre.Task, flight...)
	x.reserved = append(x.reserved, flight...)
	if j == nil {
		j = &jobState{ID: x.pre.Task, ReplyTo: h.ReplyTo, RK: h.RK, Created: now, GateContext: x.pre.Context}
		j.init()
		jk := flightKey("j", x.service, x.pre.Task)
		x.h.reserve(bindOf(h.ReplyTo, h.RK), jk)
		x.reserved = append(x.reserved, jk)
	}
	if j.UpstreamTask == "" && !x.opening {
		x.opening = true
		x.h.svc(x.service).opening++
	}
	x.job, x.bind = j, bindOf(h.ReplyTo, h.RK)
	service, job := x.service, j
	x.commit = func() error {
		at := x.h.cfg.Now()
		// The job recorded meanwhile, if any: the same binding (the gate's retry of the first message) is that job, and
		// another is never overwritten.
		cur, err := x.h.jobFor(service, job.ID)
		if err != nil {
			return err
		}
		if cur != nil && cur != job {
			if bindOf(cur.ReplyTo, cur.RK) != bindOf(job.ReplyTo, job.RK) {
				return fmt.Errorf("sealing: job %s is bound to another replyTo: this request opens nothing", job.ID)
			}
			job = cur
			x.job = cur
		}
		job.Pairs = append(job.Pairs, pairs...)
		job.LastSeen = at
		svc.put(setReplyTo, h.ReplyTo, replyEntry{Job: job.ID, At: at})
		x.h.indexNode(setReplyTo, h.ReplyTo, service, true)
		if err := x.h.saveJob(service, job); err != nil {
			return err
		}
		return x.h.st.saveService(service, svc)
	}
	return out, nil
}

// replyToElsewhere reports a replyTo that opened a job of another service this node hosts: one identity key opens
// every service's requests, so rule 2's replyTo set and rule 5's (session, messageId) set are checked across every
// service of the node (D221, ext-sealed.md §5), beside the header's service member, which binds a sealed request to
// the service the buyer hired (checkService). It reads the node-wide index (nmu), never another service's state.
func (h *Hosting) replyToElsewhere(service, replyTo string) bool {
	h.nmu.Lock()
	defer h.nmu.Unlock()
	s, ok := h.nReplyTo[replyTo]
	return ok && s != service
}

// mcpCallElsewhere reports an MCP (session, messageId) a sealed call of another service of this node forwarded
// (replyToElsewhere's node-wide check).
func (h *Hosting) mcpCallElsewhere(service, callKey string) bool {
	h.nmu.Lock()
	defer h.nmu.Unlock()
	s, ok := h.nCalls[callKey]
	return ok && s != service
}

// unsealedSend is a send request with no sealed part: refused bad_header under --sealed-required, and on the
// exchange of a sealed job (recorded, or being opened); it may not name another job's upstream task or another gate
// context's upstream context.
func (x *Exchange) unsealedSend(svc *serviceState, msgTask, msgCtx string, _ int) error {
	if slices.Contains(x.h.cfg.Required, x.service) {
		return reject(sealed.RejectBadHeader, "an unsealed request to a service started with --sealed-required")
	}
	j, err := x.h.jobFor(x.service, x.pre.Task)
	if err != nil {
		return err
	}
	_, opening := x.h.inFlight(flightKey("j", x.service, x.pre.Task))
	if j != nil || opening {
		// A job opened by a sealed request is the hiring node's, which seals every message of it: an unsealed follow-up
		// is the gate's own (answering the service's question for the buyer, say), and its replies would reach the buyer
		// sealed and authenticated as the service's answer to the buyer. The A2A half of D175's "answers must be
		// sealed", whatever --sealed-required says (security review).
		return reject(sealed.RejectBadHeader, "an unsealed message on a sealed job's exchange")
	}
	if owner, ok := svc.Tasks[msgTask]; ok && msgTask != "" && owner != x.pre.Task {
		return reject(sealed.RejectWrongTask, "the message names a sealed job's upstream task")
	}
	if _, ok := svc.UpstreamContexts[msgCtx]; ok && msgCtx != "" {
		// An unsealed message into a sealed job's upstream context, under any gate task, the job's own gate context
		// included: the hiring node seals every message to a sealed service, and an agent that keeps a conversation per
		// context would answer it from the buyer's sealed conversation, unsealed (round 2 sealing-fixes-6).
		return reject(sealed.RejectBadHeader, "an unsealed message into a sealed job's upstream context")
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
			hd.ContextID != first.ContextID || hd.MessageID != first.MessageID || hd.Service != first.Service ||
			!slices.Equal(hd.Kids, first.Kids):
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
		hs := h.lock(x.service)
		var err error
		if x.commit != nil && !x.done {
			x.done = true
			err = x.commit()
		}
		h.unreserve(x.reserved)
		x.reserved = nil
		hs.mu.Unlock()
		if err != nil {
			// The service has the request; its replies are sealed all the same (x.job), and the job's file is
			// written again with them.
			h.log.Error("sealing: the job's state was not written", "service", x.service, "err", err)
		}
	}
	hs := h.lock(x.service)
	svc, err := h.service(x.service)
	// A job being opened counts (D227): until its first reply is read, its upstream task is not indexed, and a reply on
	// another exchange that names it must be read to be held.
	guard := err == nil && h.guarded(x.service, svc)
	hs.mu.Unlock()
	if err != nil {
		return nil, err
	}
	streaming := false
	defer func() {
		if !streaming {
			hs.mu.Lock()
			x.endOpening() // the job's first reply is read (or there was none to read)
			hs.mu.Unlock()
		}
	}()
	if x.job == nil && !guard {
		return resp, nil // nothing sealed on this service: the answer passes as it came
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case x.stream && a2awire.IsEventStream(ct):
		streaming = true
		body := resp.Body
		pr, pw := io.Pipe()
		go func() {
			err := sseTransform(body, pw, func(data []byte) ([]byte, error) { return x.replyHeld(ctx, data) })
			hs.mu.Lock()
			x.endOpening()
			hs.mu.Unlock()
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
		if x.job == nil {
			return resp, nil // an error: no client reads it as an answer
		}
		return h.errorResponse(x, resp), nil
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
	out, err := x.replyHeld(ctx, b)
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

// sealedErrorText replaces a service's error message on a sealed job's exchange (errorResponse, scrubError): the code
// stays, the text and data do not, since a service's error routinely quotes the input it refused.
const sealedErrorText = "the service returned an error (its text stays on the node: the job is sealed)"

// errorResponse is a sealed job's non-2xx answer as the gate gets it: its error's code (and HTTP+JSON status) with
// sealedErrorText, never the service's text, which may quote the sealed input (a validation error echoing an
// argument). A body that is not one error object becomes the node's own error of that status.
//
// SPEC-CONFLICT (D195; ext-sealed.md §2, §7): §2 lists what is sealed (parts, MCP arguments and results) and says
// nothing of errors, while §7 promises that the gate's database and logs hold ciphertext only, and the gate stores
// and logs a service's error text. The closest faithful behaviour: on a sealed job the hosting node keeps an error's
// code and replaces its message and data with fixed text; sealing them for the hiring node to open would need §3 to
// define an error's envelope.
func (h *Hosting) errorResponse(x *Exchange, resp *http.Response) *http.Response {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	_ = resp.Body.Close()
	out, ok := []byte(nil), false
	if err == nil && len(b) <= maxBody {
		out, ok = scrubError(b)
	}
	if !ok {
		if x.jsonrpc {
			id := x.rpcID
			if len(bytes.TrimSpace(id)) == 0 {
				id = json.RawMessage("null")
			}
			out = a2awire.Encode(map[string]json.RawMessage{"jsonrpc": a2awire.Quote("2.0"), "id": id,
				"error": a2awire.Encode(map[string]json.RawMessage{"code": json.RawMessage("-32603"), "message": a2awire.Quote(sealedErrorText)})})
		} else {
			out = a2awire.Encode(map[string]json.RawMessage{"error": a2awire.Encode(map[string]json.RawMessage{
				"code": json.RawMessage(fmt.Sprint(resp.StatusCode)), "status": a2awire.Quote("UNKNOWN"), "message": a2awire.Quote(sealedErrorText)})})
		}
	}
	h.log.Info("sealing: a sealed job's error is relayed with its code only", "service", x.service, "status", resp.StatusCode)
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Content-Encoding")
	resp.Trailer = nil
	setBody(resp, out)
	return resp
}

// scrubError replaces the message and data of a body's error object (a JSON-RPC error, or HTTP+JSON's
// google.rpc.Status) with sealedErrorText, keeping its code (and status); ok false when the body is not one JSON
// object whose error member is one (any case variant of "error" counts, as a lenient decoder reads it).
func scrubError(body []byte) ([]byte, bool) {
	top, err := strictjson.Members(body)
	if err != nil {
		return nil, false
	}
	key := ""
	for k := range top {
		if strings.EqualFold(k, "error") {
			if key != "" {
				return nil, false
			}
			key = k
		}
	}
	if key == "" {
		return nil, false
	}
	e, err := strictjson.Members(top[key])
	if err != nil {
		return nil, false
	}
	ne := map[string]json.RawMessage{"message": a2awire.Quote(sealedErrorText)}
	for k, v := range e {
		switch {
		case strings.EqualFold(k, "code") && firstByte(v) != '"' && json.Valid(v):
			ne["code"] = v
		case strings.EqualFold(k, "status") && firstByte(v) == '"':
			var st string
			if json.Unmarshal(v, &st) == nil && isStatusName(st) {
				ne["status"] = v
			}
		}
	}
	if _, ok := ne["code"]; !ok {
		ne["code"] = json.RawMessage("-32603")
	}
	out := map[string]json.RawMessage{"error": a2awire.Encode(ne)}
	for k, v := range top {
		if k == "jsonrpc" || k == "id" {
			out[k] = v
		}
	}
	return a2awire.Encode(out), true
}

// isStatusName reports a google.rpc.Code name (upper case letters and underscores), which quotes nothing.
func isStatusName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c == '_') {
			return false
		}
	}
	return true
}

// lastSeenEvery is how often a job's LastSeen is written when nothing else of the job changed: retention is counted
// in days, and a poll of an unchanged task should not cost a write.
const lastSeenEvery = 10 * time.Minute

// jobDigest is what a reply may change of a job (its artifacts' chunks, its upstream ids): reply writes the job when
// it changed, and LastSeen alone only every lastSeenEvery.
func jobDigest(j *jobState) string {
	b, _ := json.Marshal(struct {
		A    map[string]*artifactState
		T, C string
	}{j.Artifacts, j.UpstreamTask, j.UpstreamContext})
	return string(b)
}

// replyHold bounds how long a reply waits for the sealed jobs of its service being opened (replyHeld).
var replyHold = 30 * time.Second

// errHold is reply's answer for a reply that must wait for a sealed job of its service being opened (replyHeld).
var errHold = errors.New("sealing: a reply waits for a sealed job being opened")

// replyHeld is reply, waiting while it is held: a reply on an exchange of no sealed job that names an upstream task or
// context the node has not indexed, while a sealed job of the service is being opened, may name that job's, which
// the node learns from the job's own first reply. It waits for every such opening to end (the job's first reply
// read, its exchange answered by the node or ended), for at most replyHold, and is then read again: a reply that
// names the job's task is refused then, any other passes. A reply still held after replyHold, or whose request's
// context ends, is refused wrong_task (D227).
func (x *Exchange) replyHeld(ctx context.Context, b []byte) ([]byte, error) {
	deadline := time.NewTimer(replyHold)
	defer deadline.Stop()
	for {
		out, err := x.reply(b)
		if !errors.Is(err, errHold) {
			return out, err
		}
		hs := x.h.lock(x.service)
		n, ch := hs.opening, hs.openingDone
		hs.mu.Unlock()
		if n == 0 {
			continue
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, reject(sealed.RejectWrongTask, "a reply held for a sealed job being opened: the exchange ended")
		case <-deadline.C:
			return nil, reject(sealed.RejectWrongTask, "a reply naming a task the node has not indexed, while a sealed job of the service is being opened")
		}
	}
}

// reply checks and seals one reply body (a JSON response, or one SSE event's data).
func (x *Exchange) reply(b []byte) ([]byte, error) {
	h := x.h
	hs := h.lock(x.service)
	defer hs.mu.Unlock()
	svc, err := h.service(x.service)
	if err != nil {
		return nil, err
	}
	j := x.job
	if j != nil {
		// Reload what the job holds now (another exchange of the task may have sealed meanwhile); a job bound to
		// another replyTo than the exchange's request is never sealed to.
		if cur, err := h.jobFor(x.service, j.ID); err == nil && cur != nil {
			if bindOf(cur.ReplyTo, cur.RK) != x.bind {
				return nil, reject(sealed.RejectWrongJob, "the job is bound to another replyTo than the exchange's request")
			}
			j, x.job = cur, cur
		}
		if out, ok := scrubError(b); ok {
			// A JSON-RPC error answer of a sealed job: its code only (errorResponse).
			return out, nil
		}
	}
	before := ""
	if j != nil {
		before = jobDigest(j)
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
				// An unsealed exchange may not carry a sealed job's task or context (a sealed job's own gate context
				// included: round 2 sealing-fixes-6).
				owner, taskIndexed := svc.Tasks[task]
				if taskIndexed && task != "" && owner != x.pre.Task {
					return reject(sealed.RejectWrongTask, "a reply names a sealed job's upstream task on another exchange")
				}
				_, ctxIndexed := svc.UpstreamContexts[cctx]
				if ctxIndexed && cctx != "" {
					return reject(sealed.RejectWrongContext, "a reply names a sealed job's upstream context on another exchange")
				}
				if hs.opening > 0 && (task != "" && !taskIndexed || cctx != "" && !ctxIndexed) {
					return errHold // it may name the task of a sealed job being opened (replyHeld, D227)
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
					svc.put(setTasks, task, j.ID)
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
					svc.put(setUpstreamContexts, cctx, j.GateContext)
					if j.GateContext != "" {
						svc.put(setContexts, j.GateContext, contextEntry{Upstream: cctx, At: h.cfg.Now()})
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
	if errors.Is(err, errHold) {
		return nil, err
	}
	if err != nil {
		if _, ok := sealed.ReasonOf(err); ok {
			return nil, err
		}
		// The error is not quoted: a service's reply is plaintext, and a JSON error may hold a byte of it (the node
		// never logs a plaintext part, sealed_no_plaintext).
		return nil, reject(sealed.RejectBadHeader, "a reply that is not an A2A 1.0 answer with exact keys (D172)")
	}
	if j != nil {
		if now := h.cfg.Now(); jobDigest(j) != before || now.Sub(j.LastSeen) >= lastSeenEvery {
			j.LastSeen = now
			if err := h.saveJob(x.service, j); err != nil {
				return nil, err
			}
		}
	}
	if changedIndex {
		if err := h.st.saveService(x.service, svc); err != nil {
			return nil, err
		}
	}
	if j != nil && j.UpstreamTask != "" {
		x.endOpening() // the job's upstream task is indexed: a reply held for it is read again
	}
	if !changed {
		return b, nil
	}
	return out, nil
}

// sealContainer seals every part of one message or artifact of a reply to the job's replyTo (§5): a message's parts
// with its messageId; an artifact update's with the artifactId and the next chunk; a whole artifact's (a Task's)
// reusing the ciphertext of the parts the gate already holds unchanged and sealing the rest as the next chunk.
//
// SPEC-CONFLICT (D216; ext-sealed.md §3's table, chunk: "for artifact updates, the 0-based count of updates for that
// artifactId this node sealed; 0 for a whole artifact"): a whole artifact reuses the ciphertext of the parts the gate
// already holds (§5, so that polling sees identical bytes), and those carry the chunk they were sealed as; its new
// parts cannot be chunk 0 again once a chunk 0 was sealed, since the hiring node refuses a (chunk, part) seen before
// with other bytes (rule 4). The closest faithful behaviour: chunk counts every chunk this node sealed for the
// artifactId, an update or the new parts of a whole artifact, so that a whole artifact sealed first is chunk 0 and
// the updates that follow count on from it; §3 now says so.
func (x *Exchange) sealContainer(j *jobState, c *a2awire.Container) error {
	if c.Where == a2awire.WhereArtifact || c.Where == a2awire.WhereArtifactUpdate {
		// An artifact's name and description are the service's words about its content, and nothing seals them (§2
		// lists a part's filename as the only member of content left outside): on a sealed job they are not relayed.
		// SPEC-CONFLICT (D220; ext-sealed.md §2): §2 names neither; the closest faithful behaviour to "the gate
		// holds ciphertext only" (§7) drops them, any case variant included (a lenient decoder takes one for the
		// member).
		for k := range c.Members {
			if strings.EqualFold(k, "name") || strings.EqualFold(k, "description") {
				c.Set(k, nil)
			}
		}
	}
	if len(c.Parts) == 0 {
		return nil
	}
	base := sealed.Header{V: sealed.Version, Dir: sealed.DirReply, Service: x.h.servicePath(x.service), TaskID: x.pre.Task,
		ContextID: j.GateContext}
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

// service returns the service's state, loading it once and indexing its node-wide sets (nReplyTo, nCalls); the
// service's lock is held.
func (h *Hosting) service(name string) (*serviceState, error) {
	hs := h.svc(name)
	if hs.state != nil {
		return hs.state, nil
	}
	s, err := h.st.loadService(name)
	if err != nil {
		return nil, err
	}
	hs.state = s
	for k := range s.ReplyTo {
		h.indexNode(setReplyTo, k, name, true)
	}
	for k := range s.MCPCalls {
		h.indexNode(setMCPCalls, k, name, true)
	}
	return s, nil
}

// jobFor returns the job id of the service, nil when it has none; the service's lock is held.
func (h *Hosting) jobFor(service, id string) (*jobState, error) {
	if id == "" || element("job", id) != nil {
		return nil, nil
	}
	hs := h.svc(service)
	if j := hs.jobs[id]; j != nil {
		return j, nil
	}
	j, err := h.st.loadJob(service, id)
	if errors.Is(err, errNoJob) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hs.jobs[id] = j
	return j, nil
}

// saveJob writes a job through; the service's lock is held.
func (h *Hosting) saveJob(service string, j *jobState) error {
	h.svc(service).jobs[j.ID] = j
	return h.st.saveJob(service, j)
}

// jobCacheIdle is how long a job stays loaded in memory after its last exchange (Sweep evicts it then; it is read
// again from its file when an exchange names it).
const jobCacheIdle = time.Hour

// Sweep drops jobs JobRetention after their last exchange, and the per-service entries they leave: a replyTo or an
// MCP (session, messageId) once it is older than MaxAge (and the clock skew rule 1 allows), whatever became of its
// job, since rule 2 and rule 5 refuse a replay for that long (§5); a gate context's upstream context, and the
// reverse index, once no kept job is in it; the indexes of the jobs dropped. A job file is read to decide, not kept
// loaded, and a loaded job idle for jobCacheIdle is evicted, so that the jobs in memory are the busy ones. Each
// service's sets are written as a new snapshot.
func (h *Hosting) Sweep(now time.Time) error {
	services, err := h.st.services()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range services {
		// One service at a time, under its own lock: the other services' exchanges go on meanwhile (robustness-4).
		hs := h.lock(name)
		errs = append(errs, h.sweepService(name, hs, now)...)
		hs.mu.Unlock()
	}
	return errors.Join(errs...)
}

// sweepService is Sweep for one service; its lock is held.
func (h *Hosting) sweepService(name string, hs *hostedService, now time.Time) []error {
	var errs []error
	keepFor := h.cfg.MaxAge + sealed.MaxClockSkew*time.Second
	// A job outlives every request of it that rule 1 still lets through: its pairs (rule 5) and binding must be there
	// as long as its first message could be resent under its own gate task (the replyTo stays MaxAge; round 2
	// sealing-fixes-8).
	retain := max(JobRetention, keepFor)
	svc, err := h.service(name)
	if err != nil {
		return append(errs, err)
	}
	jobs, err := h.st.jobs(name)
	if err != nil {
		return append(errs, err)
	}
	gone := map[string]bool{}
	liveCtx, liveUp := map[string]bool{}, map[string]bool{}
	for _, id := range jobs {
		j := hs.jobs[id]
		if j == nil {
			if j, err = h.st.loadJob(name, id); err != nil {
				continue
			}
		}
		last := j.LastSeen
		if last.IsZero() {
			last = j.Created
		}
		if now.Sub(last) < retain {
			liveCtx[j.GateContext], liveUp[j.UpstreamContext] = true, true
			// A sealed MCP request or input request older than MaxAge is no longer answerable (rule 4b's age; a
			// result that late is a replay of one already sealed): it leaves the job. An answered one's index entry
			// goes with it, so that the indexes do not grow with a long-lived session (round 2 sealing-fixes-9): a
			// late answer to it on the job's session is dropped there all the same, since an exchange of a sealed
			// job relays no response that answers neither its own request nor one the node holds (FromServer), and
			// no other exchange reaches the session (D226). An unanswered one's entry stays with the job, so that an
			// answer to it is still the job's, and refused.
			pruned := false
			for ck, c := range j.Calls {
				if now.Sub(c.At) > keepFor {
					delete(j.Calls, ck)
					pruned = true
					if e, ok := svc.Responses[ck]; ok && e.Job == j.ID && c.Answered {
						svc.del(setResponses, ck)
					}
				}
			}
			for ik, in := range j.Inputs {
				if now.Sub(in.SealedAt) > keepFor {
					delete(j.Inputs, ik)
					pruned = true
					if in.Answered {
						for k, e := range svc.Inputs {
							if e.Job == j.ID && e.Key == ik {
								svc.del(setInputs, k)
							}
						}
					}
				}
			}
			if pruned {
				if err := h.st.saveJob(name, j); err != nil {
					errs = append(errs, err)
				}
			}
			if hs.jobs[id] != nil && now.Sub(last) >= jobCacheIdle {
				delete(hs.jobs, id)
			}
			continue
		}
		if err := h.st.removeJob(name, id); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(hs.jobs, id)
		gone[id] = true
	}
	for k, v := range svc.ReplyTo {
		if now.Sub(v.At) > keepFor {
			svc.del(setReplyTo, k)
			h.indexNode(setReplyTo, k, name, false)
		}
	}
	for k, v := range svc.MCPCalls {
		if now.Sub(v) > keepFor {
			svc.del(setMCPCalls, k)
			h.indexNode(setMCPCalls, k, name, false)
		}
	}
	for k, v := range svc.Contexts {
		if !liveCtx[k] && now.Sub(v.At) > JobRetention {
			svc.del(setContexts, k)
		}
	}
	for k, v := range svc.UpstreamContexts {
		if c, ok := svc.Contexts[v]; !liveUp[k] && (!ok || c.Upstream != k) {
			svc.del(setUpstreamContexts, k)
		}
	}
	for k, v := range svc.Tasks {
		if gone[v] {
			svc.del(setTasks, k)
		}
	}
	for k, v := range svc.Responses {
		if gone[v.Job] {
			svc.del(setResponses, k)
		}
	}
	for k, v := range svc.Inputs {
		if gone[v.Job] {
			svc.del(setInputs, k)
		}
	}
	for k, v := range svc.Sessions {
		if gone[v] {
			svc.del(setSessions, k)
		}
	}
	if err := h.st.compactService(name, svc); err != nil {
		errs = append(errs, err)
	}
	return errs
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
