// Package a2ahttp serves every A2A route of the gate (spec/relay.md §2.1, §2.4, §3, §9).
//
// For each service path {svc} (/agents/{name}, or /agents/{tenant}/{name} on a multi-tenant gate):
//
//	GET  {svc} and {svc}/.well-known/agent-card.json   the served card (D10)
//	GET  {svc}/.well-known/agent-card.upstream.json    the upstream card, verbatim (D13)
//	GET  {svc}/.well-known/seller-jwks.json            the seller key set the gate verified it with (D223)
//	POST {svc} and {svc}/                              JSON-RPC 2.0 (the TCK posts the trailing slash)
//	     {svc}/message:send, {svc}/tasks…, …           HTTP+JSON, every route of spec §5.3
//
// JSON-RPC requests are read (the edge has bounded the body) to route SendStreamingMessage and
// SubscribeToTask to the gate's SSE writer and everything else to a2a-go's JSON-RPC handler; on HTTP+JSON,
// message:stream and tasks/{id}:subscribe (GET and POST) go to the SSE writer and the rest to a2a-go's
// REST handler under http.StripPrefix. A JSON-RPC request with a non-empty tenant is an unknown service.
//
// An unknown service, a service of the other kind, a disabled one, or one whose allow: does not admit the
// principal all get the same 404, with a body that fits the route (D50).
//
// Order of a request: the route (a path that is no route is the closed router's empty 404); the
// credential (a bad one is 401 on every path, so the answer never depends on whether a service exists);
// the service and its allow: list (the unknown-service 404; cards are public when allow: has anonymous or
// the service is listed, and are never version-checked); then, on the A2A routes, the JSON-RPC tenant
// check, A2A-Version (Versions), A2A-Extensions (Extensions), and the handler.
package a2ahttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/sse"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tenant"
)

// Service is what a route needs to know about one service.
type Service struct {
	Ref      store.ServiceRef
	Allow    auth.AllowList
	Listed   bool
	Disabled bool
	// Cards returns the service's current cards (card.Keeper.Current). Its served card also declares the
	// extensions A2A-Extensions is checked against (spec §2.4). An error, or a card marked Unavailable,
	// makes the card routes answer 503 (spec §3).
	Cards func() (*card.Served, error)
}

// Services resolves a path's tenant and name to a service (nil when there is none). On a single-tenant
// gate the tenant is tenant.TeamSlug ("default").
type Services interface {
	Lookup(ctx context.Context, tenant, name string) (*Service, error)
}

// HandlerSource builds a service's a2a-go handler; *relay.Handlers implements it.
type HandlerSource interface {
	For(ctx context.Context, svc store.ServiceRef) (*relay.ServiceHandler, error)
}

// Authenticator turns a request's credential into a principal; *auth.Authenticator implements it.
type Authenticator interface {
	Authenticate(r *http.Request, anonymousBuyer uuid.UUID) (auth.Principal, bool, error)
}

// Streams is the gate's SSE writer; *sse.Writer implements it.
type Streams interface {
	SendStreaming(rw http.ResponseWriter, r *http.Request, s sse.Stream, start sse.Starter)
	Subscribe(rw http.ResponseWriter, r *http.Request, s sse.Stream)
}

// Deps are the server's collaborators.
type Deps struct {
	Services Services
	Handlers HandlerSource // *relay.Handlers
	SSE      Streams       // *sse.Writer
	Auth     Authenticator // *auth.Authenticator
	Multi    bool          // multi-tenant paths
	Logger   *slog.Logger
	// There is no body limit here: the server is mounted on the edge's router (Register), and the edge
	// has read every body in full within limits.body before the route runs (spec §2.2), so every route
	// answers an oversized body the same way, the edge's 413.
}

// Server is the A2A route set, mounted on the edge's closed router under /agents/ (Register).
type Server struct {
	deps Deps

	mu         sync.Mutex
	transports map[string]*transports
	exts       map[string]cachedExtensions
}

// transports are a2a-go's two transports over one service handler.
type transports struct {
	sh      *relay.ServiceHandler
	jsonrpc http.Handler
	rest    http.Handler
}

// cachedExtensions is what a served card declares in capabilities.extensions, by the card's ETag.
type cachedExtensions struct {
	etag string
	set  *ExtensionSet
}

// ExtensionSet is what a served card declares in capabilities.extensions (spec §2.4): every URI, as a set,
// and those marked required: true, in the card's order. The card is the seller's (up to 256 KiB, §6.6), so
// a lookup never walks the list.
type ExtensionSet struct {
	declared map[string]struct{}
	required []string
}

// NewExtensionSet is the set of all, with required the URIs marked required (each also in all).
func NewExtensionSet(all, required []string) *ExtensionSet {
	s := &ExtensionSet{declared: make(map[string]struct{}, len(all)), required: required}
	for _, u := range all {
		s.declared[u] = struct{}{}
	}
	return s
}

// Declares reports whether the card declares uri.
func (s *ExtensionSet) Declares(uri string) bool {
	if s == nil {
		return false
	}
	_, ok := s.declared[uri]
	return ok
}

// Required are the URIs the card marks required: true.
func (s *ExtensionSet) Required() []string {
	if s == nil {
		return nil
	}
	return s.required
}

// Bounds on what a request's A2A-Extensions may make the gate do (security: a header of up to 1 MiB of
// distinct URIs must cost the gate linear time, never a scan per URI). A real client activates a few
// extensions; URIs past MaxRequestedExtensions distinct ones, and any longer than MaxExtensionURI bytes,
// are ignored like URIs the card does not declare (§2.4).
const (
	MaxRequestedExtensions = 64
	MaxExtensionURI        = 2048
	maxMissingNamed        = 16 // the required URIs an ExtensionSupportRequiredError names
)

// New returns the A2A routes.
func New(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Server{deps: deps, transports: map[string]*transports{}, exts: map[string]cachedExtensions{}}
}

// Register mounts every A2A route of spec §2.1 on the edge's closed router: the JSON-RPC routes with
// HandleJSONRPC (the edge's 413 and 429 then carry a JSON-RPC error), the others with Handle. Any other
// path under /agents/ stays the router's empty 404.
func (s *Server) Register(rt *edge.Router) {
	p := "/agents/{name}"
	if s.deps.Multi {
		p = "/agents/{tenant}/{name}"
	}
	rt.HandleJSONRPC("POST "+p, s)
	rt.HandleJSONRPC("POST "+p+"/{$}", s)
	for _, pat := range []string{
		"GET " + p,
		"GET " + p + "/.well-known/agent-card.json",
		"GET " + p + "/.well-known/agent-card.upstream.json",
		"GET " + p + "/.well-known/seller-jwks.json",
		"POST " + p + "/message:send",
		"POST " + p + "/message:stream",
		"GET " + p + "/tasks",
		"GET " + p + "/tasks/{task}",
		"POST " + p + "/tasks/{task}",
		"GET " + p + "/tasks/{task}/pushNotificationConfigs",
		"POST " + p + "/tasks/{task}/pushNotificationConfigs",
		"GET " + p + "/tasks/{task}/pushNotificationConfigs/{config}",
		"DELETE " + p + "/tasks/{task}/pushNotificationConfigs/{config}",
		"GET " + p + "/extendedAgentCard",
	} {
		rt.Handle(pat, s)
	}
}

// kind is what a route serves.
type kind int

const (
	routeCard          kind = iota // GET {svc}, {svc}/.well-known/agent-card.json
	routeUpstreamCard              // GET {svc}/.well-known/agent-card.upstream.json
	routeSellerJWKS                // GET {svc}/.well-known/seller-jwks.json (D223)
	routeJSONRPC                   // POST {svc}, POST {svc}/
	routeREST                      // HTTP+JSON, served by a2a-go
	routeRESTStream                // POST {svc}/message:stream
	routeRESTSubscribe             // GET|POST {svc}/tasks/{id}:subscribe
)

// route is a parsed A2A path.
type route struct {
	kind         kind
	tenant, name string
	prefix       string // the service path, {svc}
	taskID       string // routeRESTSubscribe
}

func (rt route) binding() sse.Binding {
	if rt.kind == routeJSONRPC {
		return sse.JSONRPC
	}
	return sse.HTTPJSON
}

func (rt route) card() bool {
	return rt.kind == routeCard || rt.kind == routeUpstreamCard || rt.kind == routeSellerJWKS
}

// parseRoute classifies a request by method and path; ok is false for anything that is not an A2A route.
// The tenant and name are not validated here: a malformed one is an unknown service, answered in the
// route's shape.
func parseRoute(r *http.Request, multi bool) (route, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/agents/")
	if !ok {
		return route{}, false
	}
	var rt route
	if multi {
		var found bool
		if rt.tenant, rest, found = strings.Cut(rest, "/"); !found {
			return route{}, false
		}
	}
	rt.name, rest, _ = strings.Cut(rest, "/")
	if rt.name == "" || (multi && rt.tenant == "") {
		return route{}, false
	}
	rt.prefix = "/agents/" + rt.name
	if multi {
		rt.prefix = "/agents/" + rt.tenant + "/" + rt.name
	}
	whole := r.URL.Path == rt.prefix // "{svc}" without a trailing slash
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	post := r.Method == http.MethodPost
	switch {
	case whole && get:
		rt.kind = routeCard
	case whole && post, rest == "" && post: // POST {svc} and POST {svc}/
		rt.kind = routeJSONRPC
	case rest == ".well-known/agent-card.json" && get:
		rt.kind = routeCard
	case rest == ".well-known/agent-card.upstream.json" && get:
		rt.kind = routeUpstreamCard
	case rest == ".well-known/seller-jwks.json" && get:
		rt.kind = routeSellerJWKS
	case rest == "message:send" && post:
		rt.kind = routeREST
	case rest == "message:stream" && post:
		rt.kind = routeRESTStream
	case (rest == "tasks" || rest == "extendedAgentCard") && r.Method == http.MethodGet:
		rt.kind = routeREST
	case strings.HasPrefix(rest, "tasks/"):
		return taskRoute(rt, r.Method, strings.TrimPrefix(rest, "tasks/"))
	default:
		return route{}, false
	}
	return rt, true
}

// taskRoute classifies {svc}/tasks/{id}[:action][/pushNotificationConfigs[/{configId}]].
func taskRoute(rt route, method, rest string) (route, bool) {
	segs := strings.Split(rest, "/")
	if segs[0] == "" {
		return route{}, false
	}
	switch len(segs) {
	case 1:
		if id, ok := strings.CutSuffix(segs[0], ":subscribe"); ok && (method == http.MethodGet || method == http.MethodPost) {
			rt.kind, rt.taskID = routeRESTSubscribe, id
			return rt, true
		}
		if method == http.MethodGet || (method == http.MethodPost && strings.HasSuffix(segs[0], ":cancel")) {
			rt.kind = routeREST
			return rt, true
		}
	case 2:
		if segs[1] == "pushNotificationConfigs" && (method == http.MethodGet || method == http.MethodPost) {
			rt.kind = routeREST
			return rt, true
		}
	case 3:
		if segs[1] == "pushNotificationConfigs" && segs[2] != "" && (method == http.MethodGet || method == http.MethodDelete) {
			rt.kind = routeREST
			return rt, true
		}
	}
	return route{}, false
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, ok := parseRoute(r, s.deps.Multi)
	if !ok {
		emptyNotFound(w)
		return
	}
	ctx := r.Context()
	var (
		id   json.RawMessage
		body []byte
	)
	if rt.kind == routeJSONRPC {
		var err error
		if body, err = readBody(r); err != nil {
			// The edge buffered the body; only a server used without it can get here.
			sse.WriteError(w, sse.JSONRPC, nil, http.StatusBadRequest, a2a.NewError(a2a.ErrInvalidRequest, "the request body could not be read"))
			return
		}
		id = rpcID(body)
	}
	ctx = withRoute(ctx, routeInfo{binding: rt.binding(), id: id})
	r = r.WithContext(ctx)

	// The credential first: a bad one is 401 whether or not the path names a service (D50).
	p, authenticated, err := s.deps.Auth.Authenticate(r, uuid.Nil)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		unauthenticated(w, rt, id)
		return
	case err != nil:
		s.internal(w, rt, id, "authenticate", err)
		return
	}
	svc, err := s.lookup(ctx, rt)
	if err != nil {
		s.internal(w, rt, id, "look up the service", err)
		return
	}
	if svc == nil || svc.Disabled || svc.Ref.Kind != "a2a" {
		unknownService(w, rt, id)
		return
	}
	if !authenticated {
		buyerTenant := uuid.Nil // a team gate: one fixed buyer for every anonymous caller (spec §2.3)
		if s.deps.Multi {
			buyerTenant = svc.Ref.TenantID
		}
		buyer, err := ids.AnonymousBuyer(buyerTenant)
		if err != nil {
			s.internal(w, rt, id, "derive the anonymous buyer", err)
			return
		}
		p = auth.Anonymous(buyer)
	}
	admitted := svc.Allow.Admits(p)
	if rt.card() {
		if !admitted && !svc.Allow.AdmitsAnonymous() && !svc.Listed {
			unknownService(w, rt, id)
			return
		}
		cards := svc.Cards
		get := func(*http.Request) (*card.Served, error) {
			if cards == nil {
				return nil, card.ErrUnavailable
			}
			return cards()
		}
		if rt.kind == routeSellerJWKS {
			card.SellerJWKSHandler(get).ServeHTTP(w, r)
			return
		}
		card.Handler(get, rt.kind == routeUpstreamCard).ServeHTTP(w, r)
		return
	}
	if !admitted {
		unknownService(w, rt, id)
		return
	}
	r = r.WithContext(auth.WithPrincipal(ctx, p))
	if rt.kind == routeJSONRPC {
		// Strict decoding (§2.4, D172): after the credential and service checks, before params.tenant, the method
		// that routes a stream, the version and extension checks, Admission and any task row.
		if refused := strictRefusal(body); refused != nil {
			s.deps.Logger.Info("a2ahttp: a JSON-RPC request was refused by strict decoding", "path", rt.prefix, "error", refused)
			sid := id
			var ke *strictjson.KeyError
			if errors.As(refused, &ke) && len(ke.Path) == 0 {
				sid = nil // the request object itself is ambiguous: no id was read unambiguously
			}
			sse.WriteError(w, sse.JSONRPC, sid, http.StatusBadRequest, a2a.NewError(a2a.ErrInvalidRequest, "invalid request"))
			return
		}
		s.serveJSONRPC(w, r, rt, svc, p, body)
		return
	}
	s.serveREST(w, r, rt, svc, p)
}

func (s *Server) lookup(ctx context.Context, rt route) (*Service, error) {
	tn := tenant.TeamSlug
	if s.deps.Multi {
		tn = rt.tenant
	}
	if !config.NameRE.MatchString(rt.name) || !config.NameRE.MatchString(tn) {
		return nil, nil
	}
	return s.deps.Services.Lookup(ctx, tn, rt.name)
}

// readBody reads the request body in full and puts it back for the handler. The edge has already read it
// within limits.body (Deps): a second bound here would answer the same body differently by route.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	return b, nil
}

// rpcRequest is a JSON-RPC 2.0 request, its params and id kept raw.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      json.RawMessage `json:"id"`
}

// rpcID returns the request's id when the body is a JSON-RPC request with a valid id (a string, a
// number or null), else nil (written as null).
func rpcID(body []byte) json.RawMessage {
	var req rpcRequest
	if len(body) == 0 || json.Unmarshal(body, &req) != nil || !validID(req.ID) {
		return nil
	}
	return req.ID
}

// validID accepts a JSON-RPC id a2a-go accepts: a string, a number or null (or none).
func validID(id json.RawMessage) bool {
	id = bytes.TrimSpace(id)
	if len(id) == 0 {
		return true
	}
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case nil, string, float64:
		return true
	}
	return false
}

// tenantOf returns params.tenant when params is an object with a string tenant.
func tenantOf(params json.RawMessage) string {
	var p struct {
		Tenant string `json:"tenant"`
	}
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return ""
	}
	return p.Tenant
}

func (s *Server) serveJSONRPC(w http.ResponseWriter, r *http.Request, rt route, svc *Service, p auth.Principal, body []byte) {
	var req rpcRequest
	parsed := json.Unmarshal(body, &req) == nil
	id := rpcID(body)
	if parsed && tenantOf(req.Params) != "" {
		// The gate's path names the service; its interfaces carry no A2A tenant (spec §2.1).
		unknownService(w, rt, id)
		return
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if parsed && (req.Method == "SendStreamingMessage" || req.Method == "SubscribeToTask") {
			s.jsonrpcStream(w, r, rt, svc, p, req)
			return
		}
		t, err := s.transportsFor(r.Context(), svc)
		if err != nil {
			s.internal(w, rt, id, "build the service handler", err)
			return
		}
		boundWrites(t.jsonrpc, ResponseWriteTimeout).ServeHTTP(w, r)
	})
	s.a2a(svc, next).ServeHTTP(w, r)
}

// a2a is the middleware every A2A request (not a card) passes: A2A-Version, then A2A-Extensions against
// the service's served card.
func (s *Server) a2a(svc *Service, next http.Handler) http.Handler {
	return Versions(Extensions(s.declared(svc), next))
}

func (s *Server) jsonrpcStream(w http.ResponseWriter, r *http.Request, rt route, svc *Service, p auth.Principal, req rpcRequest) {
	if !validID(req.ID) {
		sse.WriteError(w, sse.JSONRPC, nil, 0, a2a.ErrInvalidRequest)
		return
	}
	id := req.ID
	if req.JSONRPC != "2.0" {
		sse.WriteError(w, sse.JSONRPC, id, 0, a2a.ErrInvalidRequest)
		return
	}
	st := sse.Stream{Binding: sse.JSONRPC, RequestID: id, Service: svc.Ref, Owner: ownerOf(p)}
	if req.Method == "SubscribeToTask" {
		var sub a2a.SubscribeToTaskRequest
		if err := json.Unmarshal(req.Params, &sub); err != nil {
			sse.WriteError(w, sse.JSONRPC, id, 0, unmarshalError(err))
			return
		}
		s.subscribe(w, r, st, sub.ID)
		return
	}
	var msg a2a.SendMessageRequest
	if err := json.Unmarshal(req.Params, &msg); err != nil {
		sse.WriteError(w, sse.JSONRPC, id, 0, unmarshalError(err))
		return
	}
	s.sendStreaming(w, r, rt, st, svc, &msg)
}

// unmarshalError is a2a-go's JSON-RPC handleUnmarshalError: a type mismatch is InvalidParams, anything
// else a ParseError.
func unmarshalError(err error) error {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return fmt.Errorf("%w: %w", a2a.ErrInvalidParams, err)
	}
	return fmt.Errorf("%w: %w", a2a.ErrParseError, err)
}

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request, st sse.Stream, taskID a2a.TaskID) {
	k, ok, err := sse.ParseLastEventID(r.Header)
	if err != nil {
		sse.WriteError(w, st.Binding, st.RequestID, 0, err)
		return
	}
	if ok {
		st.LastEventID = &k
	}
	st.TaskID = taskID
	s.deps.SSE.Subscribe(w, r, st)
}

func (s *Server) sendStreaming(w http.ResponseWriter, r *http.Request, rt route, st sse.Stream, svc *Service, msg *a2a.SendMessageRequest) {
	t, err := s.transportsFor(r.Context(), svc)
	if err != nil {
		s.internal(w, rt, st.RequestID, "build the service handler", err)
		return
	}
	params := a2asrv.NewServiceParams(r.Header)
	s.deps.SSE.SendStreaming(w, r, st, sse.HandlerStarter(t.sh.Handler, msg, params))
}

func (s *Server) serveREST(w http.ResponseWriter, r *http.Request, rt route, svc *Service, p auth.Principal) {
	var next http.Handler
	switch rt.kind {
	case routeRESTSubscribe:
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.subscribe(w, r, sse.Stream{Binding: sse.HTTPJSON, Service: svc.Ref, Owner: ownerOf(p)}, a2a.TaskID(rt.taskID))
		})
	case routeRESTStream:
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var msg a2a.SendMessageRequest
			body, err := readBody(r)
			if err == nil {
				err = decodeStrict(body, &msg)
			}
			if err != nil {
				sse.WriteError(w, sse.HTTPJSON, nil, 0, a2a.ErrParseError) // as a2a-go's REST handler answers it
				return
			}
			s.sendStreaming(w, r, rt, sse.Stream{Binding: sse.HTTPJSON, Service: svc.Ref, Owner: ownerOf(p)}, svc, &msg)
		})
	default:
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, err := s.transportsFor(r.Context(), svc)
			if err != nil {
				s.internal(w, rt, nil, "build the service handler", err)
				return
			}
			n := &note{}
			r = r.WithContext(context.WithValue(r.Context(), noteKey{}, n))
			boundWrites(http.StripPrefix(rt.prefix, t.rest), ResponseWriteTimeout).ServeHTTP(&restWriter{ResponseWriter: w, note: n}, r)
		})
	}
	s.a2a(svc, next).ServeHTTP(w, r)
}

// decodeStrict is a2a-go's utils.DecodeJSON: one JSON value and nothing after it.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the request")
	}
	return nil
}

func ownerOf(p auth.Principal) store.Owner {
	return store.Owner{BuyerID: p.BuyerID, Principal: p.ID, Via: string(p.Via), TenantID: p.TenantID}
}

// transportsFor returns a2a-go's transports over the service's handler, built again when the relay's
// handler cache built a new one (a config or NOTIFY kontra_services change).
func (s *Server) transportsFor(ctx context.Context, svc *Service) (*transports, error) {
	sh, err := s.deps.Handlers.For(ctx, svc.Ref)
	if err != nil {
		return nil, err
	}
	key := svc.Ref.TenantID.String() + "/" + svc.Ref.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.transports[key]; t != nil && t.sh == sh {
		return t, nil
	}
	h := publicHandler{RequestHandler: sh.Handler, log: s.deps.Logger}
	t := &transports{
		sh:      sh,
		jsonrpc: a2asrv.NewJSONRPCHandler(h),
		rest:    a2asrv.NewRESTHandler(h),
	}
	s.transports[key] = t
	return t, nil
}

// declared returns the extensions the service's served card declares (capabilities.extensions: every
// URI, and those marked required: true), parsed once per card ETag. A service without a servable card
// declares none.
func (s *Server) declared(svc *Service) func(*http.Request) *ExtensionSet {
	return func(*http.Request) *ExtensionSet {
		if svc.Cards == nil {
			return nil
		}
		c, _ := svc.Cards() // a card past its 30 minutes is still what the service declared
		if c == nil || len(c.Body) == 0 {
			return nil
		}
		key := svc.Ref.TenantID.String() + "/" + svc.Ref.Name
		s.mu.Lock()
		e, ok := s.exts[key]
		s.mu.Unlock()
		if ok && e.etag == c.ETag && c.ETag != "" {
			return e.set
		}
		e = cachedExtensions{etag: c.ETag, set: NewExtensionSet(cardExtensions(c.Body))}
		s.mu.Lock()
		s.exts[key] = e
		s.mu.Unlock()
		return e.set
	}
}

// cardExtensions reads capabilities.extensions of a served card, entry by entry: an entry that is not an
// object with a string uri is skipped, so one malformed seller entry cannot hide the operator's.
func cardExtensions(body []byte) (all, required []string) {
	var c struct {
		Capabilities struct {
			Extensions []json.RawMessage `json:"extensions"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(body, &c) != nil {
		var loose struct {
			Capabilities json.RawMessage `json:"capabilities"`
		}
		if json.Unmarshal(body, &loose) != nil || json.Unmarshal(loose.Capabilities, &c.Capabilities) != nil {
			return nil, nil
		}
	}
	seen := map[string]bool{}
	for _, raw := range c.Capabilities.Extensions {
		var e struct {
			URI      string          `json:"uri"`
			Required json.RawMessage `json:"required"`
		}
		if json.Unmarshal(raw, &e) != nil || e.URI == "" || seen[e.URI] {
			continue
		}
		seen[e.URI] = true
		all = append(all, e.URI)
		if string(bytes.TrimSpace(e.Required)) == "true" {
			required = append(required, e.URI)
		}
	}
	return all, required
}

// routeInfo is how the middleware writes its errors: the route's binding and, on JSON-RPC, the id.
type routeInfo struct {
	binding sse.Binding
	id      json.RawMessage
}

type routeKey struct{}

func withRoute(ctx context.Context, ri routeInfo) context.Context {
	return context.WithValue(ctx, routeKey{}, ri)
}

// routeOf returns the request's route info; a request the server did not route (the middleware used on
// its own) is HTTP+JSON, its id read from a JSON-RPC body when it has one.
func routeOf(r *http.Request) routeInfo {
	if ri, ok := r.Context().Value(routeKey{}).(routeInfo); ok {
		return ri
	}
	return routeInfo{binding: sse.HTTPJSON}
}

// Version is the only A2A version the gate serves downstream (PRD §18).
const Version = string(a2a.Version)

// Versions normalizes A2A-Version (header, else query parameter): 1.0 and 1.0.<patch> become 1.0; absent
// or empty means 0.3 and is refused with VersionNotSupportedError (-32009, HTTP 400) naming the header to
// send (spec §2.4, D9).
//
// Every non-empty A2A-Version header line must be 1.0 or 1.0.<patch>; the query parameter is read only
// when no header line has a value. The request goes on with exactly one header, "A2A-Version: 1.0", so
// a2a-go's own check (one value, equal to the version its transport serves) passes. The error is written
// in the route's shape: a JSON-RPC error with HTTP status 200 on JSON-RPC, as a2a-go answers every
// JSON-RPC error; a google.rpc.Status FAILED_PRECONDITION with HTTP 400 on HTTP+JSON.
func Versions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := checkVersion(r); err != nil {
			ri := routeOf(r)
			sse.WriteError(w, ri.binding, ri.id, 0, err)
			return
		}
		r2 := withHeader(r)
		r2.Header.Del(a2a.SvcParamVersion)
		r2.Header.Set(a2a.SvcParamVersion, Version)
		next.ServeHTTP(w, r2)
	})
}

const sendVersion = `send the header "A2A-Version: 1.0"`

func checkVersion(r *http.Request) error {
	var vals []string
	for _, v := range r.Header.Values(a2a.SvcParamVersion) {
		if v = strings.TrimSpace(v); v != "" {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		if q := strings.TrimSpace(r.URL.Query().Get(a2a.SvcParamVersion)); q != "" {
			vals = append(vals, q)
		}
	}
	if len(vals) == 0 {
		return a2a.NewError(a2a.ErrVersionNotSupported,
			"no A2A-Version: the request is A2A 0.3, and this gate serves A2A 1.0 only; "+sendVersion)
	}
	for _, v := range vals {
		if !isV10(v) {
			return a2a.NewError(a2a.ErrVersionNotSupported,
				fmt.Sprintf("A2A-Version %q is not served: this gate serves A2A 1.0 only; %s", v, sendVersion))
		}
	}
	return nil
}

// isV10 accepts 1.0 and 1.0.<patch>: patch numbers must not affect negotiation (A2A §3.6).
func isV10(v string) bool {
	if v == Version {
		return true
	}
	patch, ok := strings.CutPrefix(v, Version+".")
	if !ok || patch == "" {
		return false
	}
	for i := 0; i < len(patch); i++ {
		if patch[i] < '0' || patch[i] > '9' {
			return false
		}
	}
	return true
}

// Extensions splits, trims and deduplicates A2A-Extensions (one comma-separated header, several headers,
// or both; X-A2A-Extensions as an alias), drops URIs the served card does not declare, refuses a request
// missing a required URI with ExtensionSupportRequiredError (-32008) before any task row exists, and
// echoes the activated URIs in one A2A-Extensions response header (spec §2.4, D9).
//
// The request goes on with one A2A-Extensions header line per activated URI, so a2a-go's call context
// lists each URI as one requested extension, and without X-A2A-Extensions. The activated URIs are the
// requested ones the card declares, in the order the request named them. The work is linear in the header
// and the card's required list: at most MaxRequestedExtensions distinct URIs of at most MaxExtensionURI
// bytes are considered, each looked up in the card's set.
func Extensions(declared func(r *http.Request) *ExtensionSet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested := splitExtensions(r.Header.Values(a2a.SvcParamExtensions), r.Header.Values("X-"+a2a.SvcParamExtensions))
		var set *ExtensionSet
		if declared != nil {
			set = declared(r)
		}
		var activated []string
		active := map[string]bool{}
		for _, u := range requested {
			if set.Declares(u) {
				activated = append(activated, u)
				active[u] = true
			}
		}
		var missing []string
		n := 0
		for _, u := range set.Required() {
			if !active[u] {
				n++
				if len(missing) < maxMissingNamed {
					missing = append(missing, u)
				}
			}
		}
		if n > 0 {
			named := strings.Join(missing, ", ")
			if n > len(missing) {
				named += fmt.Sprintf(" and %d more", n-len(missing))
			}
			ri := routeOf(r)
			sse.WriteError(w, ri.binding, ri.id, 0, a2a.NewError(a2a.ErrExtensionSupportRequired,
				"the service requires the extension"+plural(n)+" "+named+": send it in A2A-Extensions"))
			return
		}
		r2 := withHeader(r)
		r2.Header.Del(a2a.SvcParamExtensions)
		r2.Header.Del("X-" + a2a.SvcParamExtensions)
		for _, u := range activated {
			r2.Header.Add(a2a.SvcParamExtensions, u)
		}
		if len(activated) > 0 {
			w.Header().Set(a2a.SvcParamExtensions, strings.Join(activated, ", "))
		}
		next.ServeHTTP(w, r2)
	})
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// splitExtensions returns the URIs of every header line given, comma-split, trimmed, without empty ones,
// duplicates and URIs over MaxExtensionURI bytes, in order of first appearance, at most
// MaxRequestedExtensions of them: the rest are ignored (spec §2.4 ignores URIs the card does not declare;
// no card declares this many).
func splitExtensions(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, vals := range lists {
		for _, v := range vals {
			for u := range strings.SplitSeq(v, ",") {
				if len(out) == MaxRequestedExtensions {
					return out
				}
				if u = strings.TrimSpace(u); u != "" && len(u) <= MaxExtensionURI && !seen[u] {
					seen[u] = true
					out = append(out, u)
				}
			}
		}
	}
	return out
}

// withHeader returns a shallow copy of r with a header of its own, which the middleware may change.
func withHeader(r *http.Request) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	r2.Header = r.Header.Clone()
	if r2.Header == nil {
		r2.Header = http.Header{}
	}
	return r2
}

// UnknownService is the message of the unknown-service answer (spec §2.1, D50).
const UnknownService = "unknown service"

// unknownService is the one answer for a path naming no service, a service of the other kind, a disabled
// one, or one whose allow: does not admit the principal: 404 with a body that fits the route (D50).
func unknownService(w http.ResponseWriter, rt route, id json.RawMessage) {
	switch {
	case rt.card():
		emptyNotFound(w)
	case rt.kind == routeJSONRPC:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(sse.JSONRPCErrorBody(id, sse.RPCError{Code: -32601, Message: UnknownService}))
	default:
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": http.StatusNotFound, "status": "NOT_FOUND", "message": UnknownService}})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(append(b, '\n'))
	}
}

// unauthenticated is a bad credential (not exactly one Bearer token, an unknown, revoked or expired key):
// 401 on every route, a JSON-RPC or google.rpc.Status body on the A2A routes, empty on cards.
func unauthenticated(w http.ResponseWriter, rt route, id json.RawMessage) {
	w.Header().Set("WWW-Authenticate", `Bearer`)
	if rt.card() {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	sse.WriteError(w, rt.binding(), id, http.StatusUnauthorized, a2a.NewError(a2a.ErrUnauthenticated, "unauthenticated"))
}

// internal is a failure of the gate's own (Postgres, the handler cache): 503 on cards, -32603 on the A2A
// routes. The cause is logged, never sent.
func (s *Server) internal(w http.ResponseWriter, rt route, id json.RawMessage, what string, err error) {
	s.deps.Logger.Warn("a2ahttp: could not "+what, "path", rt.prefix, "error", err)
	if rt.card() {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	sse.WriteError(w, rt.binding(), id, 0, a2a.NewError(a2a.ErrInternalError, "the gate could not serve the request"))
}

func emptyNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNotFound)
}

// note records, for one HTTP+JSON request, an error that a2a-go's REST handler would write as 500 but the
// gate answers 503: an Update the task's lifecycle did not accept in time (spec §14.1).
type note struct {
	mu  sync.Mutex
	err error
}

type noteKey struct{}

func noteError(ctx context.Context, err error) {
	if err == nil || !sse.Unavailable(err) {
		return
	}
	if n, ok := ctx.Value(noteKey{}).(*note); ok {
		n.mu.Lock()
		n.err = err
		n.mu.Unlock()
	}
}

// publicHandler is the service's handler as a2a-go's JSON-RPC and REST transports call it. a2a-go writes
// an error's whole text as its message, so every error is answered as sse.Public makes it: an internal
// error keeps only the gate's own words, and its cause (a database error, the Temporal dial target) is
// logged here instead. On HTTP+JSON an Update not accepted in time is noted first, for restWriter's 503.
type publicHandler struct {
	a2asrv.RequestHandler
	log *slog.Logger
}

func (h publicHandler) public(ctx context.Context, method string, err error) error {
	if err == nil {
		return nil
	}
	noteError(ctx, err)
	pub, redacted := sse.Public(err)
	if redacted {
		h.log.Warn("a2ahttp: "+method+" failed", "error", err)
	}
	return pub
}

func (h publicHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	v, err := h.RequestHandler.GetTask(ctx, req)
	return v, h.public(ctx, "GetTask", err)
}

func (h publicHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	v, err := h.RequestHandler.ListTasks(ctx, req)
	return v, h.public(ctx, "ListTasks", err)
}

func (h publicHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	v, err := h.RequestHandler.CancelTask(ctx, req)
	return v, h.public(ctx, "CancelTask", err)
}

func (h publicHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	v, err := h.RequestHandler.SendMessage(ctx, req)
	return v, h.public(ctx, "SendMessage", err)
}

// SendStreamingMessage and SubscribeToTask are served by the gate's SSE writer and never routed to
// a2a-go's transports; their errors are made public all the same.
func (h publicHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return h.events(ctx, "SendStreamingMessage", h.RequestHandler.SendStreamingMessage(ctx, req))
}

func (h publicHandler) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return h.events(ctx, "SubscribeToTask", h.RequestHandler.SubscribeToTask(ctx, req))
}

func (h publicHandler) events(ctx context.Context, method string, seq iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range seq {
			if !yield(ev, h.public(ctx, method, err)) {
				return
			}
		}
	}
}

func (h publicHandler) GetTaskPushConfig(ctx context.Context, req *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	v, err := h.RequestHandler.GetTaskPushConfig(ctx, req)
	return v, h.public(ctx, "GetTaskPushConfig", err)
}

func (h publicHandler) ListTaskPushConfigs(ctx context.Context, req *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	v, err := h.RequestHandler.ListTaskPushConfigs(ctx, req)
	return v, h.public(ctx, "ListTaskPushConfigs", err)
}

func (h publicHandler) CreateTaskPushConfig(ctx context.Context, req *a2a.PushConfig) (*a2a.PushConfig, error) {
	v, err := h.RequestHandler.CreateTaskPushConfig(ctx, req)
	return v, h.public(ctx, "CreateTaskPushConfig", err)
}

func (h publicHandler) DeleteTaskPushConfig(ctx context.Context, req *a2a.DeleteTaskPushConfigRequest) error {
	return h.public(ctx, "DeleteTaskPushConfig", h.RequestHandler.DeleteTaskPushConfig(ctx, req))
}

func (h publicHandler) GetExtendedAgentCard(ctx context.Context, req *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	v, err := h.RequestHandler.GetExtendedAgentCard(ctx, req)
	return v, h.public(ctx, "GetExtendedAgentCard", err)
}

// ResponseWriteTimeout bounds how long a2a-go's answer to a request that is not a stream (GetTask,
// ListTasks, a blocking SendMessage) may take to write once it starts: a caller that stops reading (a zero
// TCP window) would otherwise hold the handler, its connection's slot, its key's in-flight slot and the
// whole answer (a snapshot, a ListTasks page of up to 8 MiB) for as long as it likes. The public listener
// has no WriteTimeout (edge.go: streams), and the gate's own streams bound each write (sse.WriteTimeout).
const ResponseWriteTimeout = 60 * time.Second

// boundWrites runs h with a write deadline armed when the answer starts (its first WriteHeader or Write),
// never before: a blocking SendMessage may wait for its task long before it writes, and an HTTP/2 stream is
// reset when its deadline passes, written to or not. The deadline is lifted when h returns, so a later
// request on the same connection starts without it.
func boundWrites(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bw := &boundedWriter{ResponseWriter: w, rc: http.NewResponseController(w), timeout: d}
		defer func() { _ = bw.rc.SetWriteDeadline(time.Time{}) }()
		h.ServeHTTP(bw, r)
	})
}

// boundedWriter arms its write deadline at the answer's first byte.
type boundedWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	armed   bool
}

func (b *boundedWriter) arm() {
	if !b.armed {
		b.armed = true
		_ = b.rc.SetWriteDeadline(time.Now().Add(b.timeout))
	}
}

func (b *boundedWriter) WriteHeader(code int) {
	b.arm()
	b.ResponseWriter.WriteHeader(code)
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	b.arm()
	return b.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection beneath.
func (b *boundedWriter) Unwrap() http.ResponseWriter { return b.ResponseWriter }

// restWriter turns a2a-go's 500 for a noted error into the gate's 503 answer (sse.HTTPError, whose
// message is sse.Public's) and drops what a2a-go writes after it.
type restWriter struct {
	http.ResponseWriter
	note    *note
	swallow bool
}

func (w *restWriter) WriteHeader(code int) {
	if code == http.StatusInternalServerError {
		w.note.mu.Lock()
		err := w.note.err
		w.note.mu.Unlock()
		if err != nil {
			status, body := sse.HTTPError(err)
			w.ResponseWriter.Header().Set("Content-Type", "application/json")
			w.ResponseWriter.WriteHeader(status)
			_, _ = w.ResponseWriter.Write(body)
			w.swallow = true
			return
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *restWriter) Write(b []byte) (int, error) {
	if w.swallow {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *restWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
