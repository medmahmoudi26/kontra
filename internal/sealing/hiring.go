package sealing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// HiringConfig configures the hiring side.
type HiringConfig struct {
	Jobs *nodekey.JobKeys // ~/.kontra/jobs/<gate host>/
	Pins *pins.Store
	Opts pins.Options // --pin-recipients, --accept-change
	// Gate is the gate's canonical origin: the pins' key (ext-sealed.md §7).
	Gate string
	// Card returns an A2A service's served card, its gate signature verified with the dialed origin's jku (through
	// the tunnel; tunnel.md §9), for the service's path {svc}.
	Card func(ctx context.Context, svc string) (*Card, error)
	// Out receives the pins' lines (D187: recipient key ids and seller key thumbprints on a first sight and on every
	// change); kontra connect's stderr. Nil writes them to the log only.
	Out io.Writer
	// Now is the node's clock (tests); nil is time.Now.
	Now func() time.Time
}

// Card is a served card whose gate signature the hiring node verified (spec/relay.md §3).
type Card struct {
	Body []byte
	// SellerKeys are the key ids of the seller signatures on the upstream card the gate says it verified
	// (kontra.upstream.verified; the upstream card's hash checked against kontra.upstream.sha256), sorted; Signed is
	// that verified flag.
	SellerKeys []string
	Signed     bool
}

// Hiring is the hiring node's sealing (spec/ext-sealed.md §5 "Hiring node"), for a service whose verified card,
// or for MCP whose initialize result, carries the extension:
//
//  1. checks the service's pin (§7) before sealing anything;
//  2. mints a per-job key and rk, and a contextId when the client gave none;
//  3. seals every part of the outgoing message (MCP: arguments, with no Mcp-Param-* header) to all recipients;
//  4. opens every sealed part of the job's answers, checking dir, ids, its own key id, auth, parts 0..parts-1
//     once, and per artifactId the chunks in order; a part that fails is dropped and the client gets a failed
//     status with kontra.failure sealed_rejected. An MCP input request (D175) must carry the job's session, the
//     task's tool and a messageId not opened before in the task (replayed otherwise); Job keeps which gate
//     request id (k.<taskId>.<seq>) carried it, and the local client's answer to that id is sealed as a request
//     with the same session, tool and messageId and the job's replyTo and rk; 4a. an unsealed part in a sealed
//     job is refused except the gate's own terminal status text, handed over with metadata["kontra.sealed"] =
//     false;
//  5. hands the local client plaintext parts with their original mediaType.
//
// A job's key is deleted when the job is terminal and the task's kontra.keyRetainUntil, if any, has passed; an MCP
// session's when the session closes and every task created in it is done (spec/tunnel.md §2.3).
type Hiring struct {
	cfg HiringConfig
	log *slog.Logger

	mu       sync.Mutex
	services map[string]*serviceInfo // by {svc} path (A2A) or the gate's Mcp-Session-Id (MCP)
	reading  map[string]*cardRead    // a card read in flight, by {svc}
	jobs     map[string]*hireJob     // loaded jobs by id
}

// cardRead is one shared read of a service's card: done closes when si and err are set.
type cardRead struct {
	done chan struct{}
	si   *serviceInfo
	err  error
}

// infoTTL is how long a service's card (its recipients) is reused before it is read again.
const infoTTL = time.Minute

// NewHiring returns the hiring side.
func NewHiring(cfg HiringConfig, log *slog.Logger) (*Hiring, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.Jobs == nil || cfg.Pins == nil {
		return nil, errors.New("sealing: the hiring side needs the jobs directory and the pins")
	}
	if cfg.Gate == "" {
		return nil, errors.New("sealing: no gate")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Hiring{cfg: cfg, log: log.With("component", "sealing-hiring"), services: map[string]*serviceInfo{},
		reading: map[string]*cardRead{}, jobs: map[string]*hireJob{}}
	h.sweepJobs()
	return h, nil
}

// serviceInfo is what a service's card or initialize result says of sealing.
type serviceInfo struct {
	name       string // the pins' service name
	sealed     bool
	required   bool
	recipients []age.Recipient
	kids       []string
	at         time.Time
	unknown    bool   // the card could not be read: not known to be sealed (noCardTTL)
	session    string // an MCP session's (its hash): the entry goes with the session's job
	// idle is the session_idle_s the gate stated in an MCP session's initialize result (D224), 0 when it stated none.
	idle time.Duration
}

// ErrNoKey is a request for a sealed task whose per-job key this node does not hold (another node hired it, or the
// key was deleted at the task's end): the node cannot open its answers.
var ErrNoKey = errors.New("sealing: this node holds no per-job key for the task")

// LocalError is a refusal the hiring node answers its local client itself, with no stream opened. Body, when set,
// is the JSON body to answer with exactly (an MCP answer the client must read as it is, such as relay.md §11.12
// rule 3's legacy 400); otherwise the hiring server writes its own refusal with Err's text.
type LocalError struct {
	Status int
	Err    error
	Body   []byte
}

func (e *LocalError) Error() string { return e.Err.Error() }
func (e *LocalError) Unwrap() error { return e.Err }

func local(status int, format string, a ...any) error {
	return &LocalError{Status: status, Err: fmt.Errorf(format, a...)}
}

// Job is one hired job's sealing state, from Request to Response: its per-job key and rk, and for an MCP task
// the messageId of each input request it opened, by the gate request id that carried it (D175).
type Job struct {
	h       *Hiring
	service string
	info    *serviceInfo
	mcp     bool

	// A2A
	jsonrpc bool
	rpcID   json.RawMessage
	opKind  opKind
	stream  bool // a request whose answer a client reads as an event stream
	list    bool
	job     *hireJob // the job of a send or a task request; nil for a list
	newTask bool

	// MCP
	get       bool   // a GET stream
	session   string // the gate's Mcp-Session-Id hash
	sid       string // the gate's Mcp-Session-Id
	reqMethod string
	reqID     json.RawMessage
	taskOf    string // the gate task a tasks/* request names
	call      *sealedCall
	init      bool
	delete    bool
}

// hireJob is a job's persisted state: ~/.kontra/jobs/<gate host>/<job>.json beside its key.
type hireJob struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	RK        string    `json:"rk"`
	ContextID string    `json:"context_id,omitempty"`
	Session   string    `json:"session,omitempty"`
	Created   time.Time `json:"created"`
	DeleteAt  time.Time `json:"delete_at,omitempty"` // the key goes then (terminal + kontra.keyRetainUntil)
	Closed    bool      `json:"closed,omitempty"`    // an MCP session the client closed
	// IdleS is an MCP session's idle bound, the session_idle_s its gate stated at initialize (D224): its key goes once
	// the session has had no exchange that long and its tasks are done. 0: sessionJobIdle.
	IdleS int64 `json:"idle_s,omitempty"`
	// Tasks are an MCP session's tasks (by gate task id): tool and messageId of the call that created each.
	Tasks map[string]*hireTask `json:"tasks,omitempty"`
	// Inputs are the MCP input requests opened, by the gate request id that carried each: what the local client's
	// answer to that id is sealed with (D175).
	Inputs map[string]*hireInput `json:"inputs,omitempty"`
	// Artifacts are per artifactId the chunks opened (A2A rule 4).
	Artifacts map[string]*hireArtifact `json:"artifacts,omitempty"`

	key  *age.X25519Identity
	sent map[string]a2awire.Part // the plaintext of the parts this node sealed for the job, by ciphertext hash
	// calls are an MCP session's requests whose answer must be sealed (a sealed tools/call, a sealed task's
	// tasks/result), by the request's id (mcpwire.IDKey): their answer is opened, or refused, on any stream of the
	// session (rule 4, 4a), not on the exchange that sent them only (a GET resumed with Last-Event-ID).
	calls map[string]*hireCall
	// seen is when the job was last used (in memory): the hiring side's caches drop a job idle for jobCacheIdle.
	seen time.Time
}

type hireTask struct {
	Tool      string `json:"tool"`
	MessageID string `json:"message_id"`
	Done      bool   `json:"done,omitempty"`
}

// hireInput is an input request the node opened: the task it is of ("" for a plain call's; the key the replay check
// counts in), the header's tool, messageId and taskId, which the answer carries back.
type hireInput struct {
	Task      string `json:"task,omitempty"`
	Tool      string `json:"tool"`
	MessageID string `json:"message_id"`
	TaskID    string `json:"task_id,omitempty"`
}

// hireCall is an MCP request of the session whose answer is sealed: a sealed tools/call (Task "") or a sealed task's
// tasks/result.
type hireCall struct {
	Tool, MessageID string
	Task            string // a tasks/result's gate task
	done            bool
}

// maxSessionCalls bounds the requests a session keeps (hireJob.calls); the answered ones go first.
const maxSessionCalls = 4096

type hireArtifact struct {
	Next int               `json:"next"`
	Seen map[string]string `json:"seen,omitempty"` // "<chunk>/<part>" -> sha256 of the ciphertext
}

// --- the service's sealing ---

// info returns what the service's card says of sealing (read again after infoTTL), after the pin check (§7, rule
// 1). A refused change is a *LocalError wrapping the *pins.ChangeError. Concurrent requests for one service share one
// read of its card.
//
// SPEC-CONFLICT (D214; ext-sealed.md §5 "Hiring node", rule 1, and §7): the node seals for a service "whose verified
// card carries the extension" and checks the pin first, but nothing says what a request does when the card cannot be
// read at all (the gate unreachable for it, a 503 card route, a card that does not verify). The closest faithful
// behaviour: when the service's pin says it is sealed, the request is refused (502: nothing is sent in plaintext to a
// service known to be sealed); with no pin, or an unsealed one, it goes as it came, which is what a first sight of
// the service would record anyway (trust on first use, §7); that unknown is kept for noCardTTL only.
//
// The shared read runs on its own context, bounded by cardReadTimeout, never on the context of the request that
// started it: a first request that leaves would otherwise end the read for every request waiting on it, and its
// cancellation would be taken for an unreadable card, so that a sealed service with no pin yet looked unsealed and
// every request in the next noCardTTL went to the gate in plaintext. A request that leaves stops waiting; the read
// goes on for the others.
func (h *Hiring) info(ctx context.Context, svc string) (*serviceInfo, error) {
	h.mu.Lock()
	si := h.services[svc]
	if si != nil && h.cfg.Now().Sub(si.at) < si.ttl() {
		h.mu.Unlock()
		return si, nil
	}
	rd := h.reading[svc]
	if rd == nil {
		rd = &cardRead{done: make(chan struct{})}
		h.reading[svc] = rd
		go func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cardReadTimeout)
			defer cancel()
			si, err := h.readCard(rctx, svc)
			h.mu.Lock()
			rd.si, rd.err = si, err
			delete(h.reading, svc)
			close(rd.done)
			h.mu.Unlock()
		}()
	}
	h.mu.Unlock()
	select {
	case <-rd.done:
		return rd.si, rd.err
	case <-ctx.Done():
		return nil, local(http.StatusServiceUnavailable, "kontra connect: the request ended while the service's card was read")
	}
}

// cardReadTimeout bounds one read of a service's card (through the tunnel, and its key set's).
const cardReadTimeout = 30 * time.Second

// noCardTTL is how long a service whose card could not be read is taken as not sealed before it is read again.
const noCardTTL = 5 * time.Second

func (si *serviceInfo) ttl() time.Duration {
	if si.unknown {
		return noCardTTL
	}
	return infoTTL
}

// readCard reads and records the service's card (info).
func (h *Hiring) readCard(ctx context.Context, svc string) (*serviceInfo, error) {
	if h.cfg.Card == nil {
		return nil, local(http.StatusBadGateway, "kontra connect cannot read the service's card")
	}
	c, err := h.cfg.Card(ctx, svc)
	if err == nil {
		return h.CardSeen(svc, c)
	}
	name := pinName(svc)
	if errors.Is(err, context.Canceled) {
		// Not the card's doing: nothing is recorded, and the next request reads it again.
		return nil, local(http.StatusServiceUnavailable, "kontra connect: the service's card read was canceled")
	}
	if pin, known, perr := h.cfg.Pins.Get(h.cfg.Gate, name); perr != nil || known && pin.Sealed {
		return nil, local(http.StatusBadGateway, "kontra connect: the sealed service's card could not be read or verified: %v", err)
	}
	h.log.Warn("sealing: the service's card could not be read or verified; it is not known to be sealed, so the request goes as it came",
		"service", name, "err", err)
	si := &serviceInfo{name: name, unknown: true, at: h.cfg.Now()}
	h.mu.Lock()
	h.services[svc] = si
	h.mu.Unlock()
	return si, nil
}

// CardSeen records what a verified card says of sealing and checks the service's pin; the hiring server calls it
// for every card it serves locally too. A refused change is a *LocalError wrapping the *pins.ChangeError.
func (h *Hiring) CardSeen(svc string, c *Card) (*serviceInfo, error) {
	ext, err := cardExtension(c.Body)
	if err != nil {
		return nil, local(http.StatusBadGateway, "kontra connect: the service's card: %v", err)
	}
	si, err := h.recordInfo(svc, pinName(svc), ext, c.SellerKeys, c.Signed)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.services[svc] = si
	h.mu.Unlock()
	return si, nil
}

// recordInfo builds a service's sealing info from its extension object (nil: not sealed) and checks the pin.
func (h *Hiring) recordInfo(key, name string, ext json.RawMessage, sellerKeys []string, signed bool) (*serviceInfo, error) {
	si := &serviceInfo{name: name, at: h.cfg.Now()}
	if ext != nil {
		var err error
		if si.recipients, si.kids, si.required, err = parseExtension(ext); err != nil {
			return nil, local(http.StatusBadGateway, "kontra connect: the service's sealed extension: %v", err)
		}
		si.sealed = true
	}
	pin := pins.Pin{Sealed: si.sealed, Recipients: si.kids, SellerKeys: sellerKeys, Signed: signed}
	out, err := h.cfg.Pins.Check(h.cfg.Gate, name, pin, h.cfg.Opts)
	var ce *pins.ChangeError
	switch {
	case errors.As(err, &ce):
		h.say(ce.Detail(h.cfg.Opts)+fmt.Sprintf(" (now: recipients %s; seller keys %s)", list(pin.Recipients), list(pin.SellerKeys)),
			"service", name)
		return nil, &LocalError{Status: http.StatusBadGateway, Err: err}
	case err != nil:
		return nil, local(http.StatusBadGateway, "kontra connect: the pins file: %v", err)
	case out.First:
		h.say(fmt.Sprintf("pins: %s at %s first seen: sealed %v, recipients %s, seller keys %s", name, h.cfg.Gate,
			out.Pin.Sealed, list(out.Pin.Recipients), list(out.Pin.SellerKeys)), "service", name)
	case len(out.Changes) > 0:
		var cs []string
		for _, c := range out.Changes {
			cs = append(cs, string(c))
		}
		h.say(fmt.Sprintf("pins: %s at %s changed (%s), accepted: sealed %v, recipients %s, seller keys %s", name, h.cfg.Gate,
			strings.Join(cs, ", "), out.Pin.Sealed, list(out.Pin.Recipients), list(out.Pin.SellerKeys)), "service", name)
	}
	return si, nil
}

// say writes a pins line on stderr and in the log (D187).
func (h *Hiring) say(line string, args ...any) {
	if h.cfg.Out != nil {
		fmt.Fprintln(h.cfg.Out, "kontra connect: "+tunnel.Printable(line))
	}
	h.log.Warn(tunnel.Printable(line), args...)
}

func list(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// pinName is a service's name in the pins file: its path without /agents/ or /mcp/ ("name", or "tenant/name").
func pinName(svc string) string {
	for _, p := range []string{"/agents/", "/mcp/"} {
		if rest, ok := strings.CutPrefix(svc, p); ok {
			return rest
		}
	}
	return svc
}

// cardExtension returns the sealed extension entry of a served card, nil when it has none (read with exact keys).
func cardExtension(card []byte) (json.RawMessage, error) {
	top, err := strictjson.Members(card)
	if err != nil {
		return nil, err
	}
	caps, ok := top["capabilities"]
	if !ok {
		return nil, nil
	}
	cm, err := strictjson.Members(caps)
	if err != nil {
		return nil, err
	}
	raw, ok := cm["extensions"]
	if !ok || firstByte(raw) != '[' {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, err
	}
	var found json.RawMessage
	for _, e := range elems {
		m, err := strictjson.Members(e)
		if err != nil {
			return nil, err
		}
		if uri, _ := a2awire.String(m, "uri"); uri == sealed.URI {
			if found != nil {
				return nil, errors.New("the sealed extension is listed twice")
			}
			found = e
		}
	}
	return found, nil
}

// parseExtension reads the sealed extension's params (ext-sealed.md §1): recipients (age1… X25519 recipients),
// envelope (exactly envelope v1's media type) and required.
func parseExtension(ext json.RawMessage) ([]age.Recipient, []string, bool, error) {
	m, err := strictjson.Members(ext)
	if err != nil {
		return nil, nil, false, err
	}
	pm, err := strictjson.Members(m["params"])
	if err != nil {
		return nil, nil, false, errors.New("no params")
	}
	var env string
	if json.Unmarshal(pm["envelope"], &env) != nil || env != sealed.MediaType {
		return nil, nil, false, fmt.Errorf("an envelope other than %s", sealed.MediaType)
	}
	var strs []string
	if err := json.Unmarshal(pm["recipients"], &strs); err != nil {
		return nil, nil, false, errors.New("recipients is not a list of strings")
	}
	required := false
	if raw, ok := pm["required"]; ok {
		if json.Unmarshal(raw, &required) != nil {
			return nil, nil, false, errors.New("required is not a boolean")
		}
	}
	var (
		rs   []age.Recipient
		kids []string
	)
	for _, s := range strs {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, nil, false, fmt.Errorf("a recipient is not an X25519 recipient: %v", err)
		}
		kid := tunnel.KeyID(r.String())
		if slices.Contains(kids, kid) {
			continue
		}
		rs = append(rs, r)
		kids = append(kids, kid)
	}
	slices.Sort(kids)
	return rs, kids, required, nil
}

// --- jobs ---

// newJob mints a per-job key and rk under a provisional id (a new task's first message, whose gate task id the
// first answer names) or the given id (an MCP session).
func (h *Hiring) newJob(id, service string) (*hireJob, error) {
	if id == "" {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		id = "p" + hex.EncodeToString(b[:])
	}
	key, err := h.cfg.Jobs.Mint(id, h.cfg.Now())
	if err != nil {
		return nil, err
	}
	rk, err := sealed.NewRK()
	if err != nil {
		return nil, err
	}
	j := &hireJob{ID: id, Service: service, RK: sealed.EncodeRK(rk), Created: h.cfg.Now(), key: key}
	j.init(h.cfg.Now())
	if err := h.saveJob(j); err != nil {
		_ = h.cfg.Jobs.Delete(id)
		return nil, err
	}
	h.jobs[id] = j
	return j, nil
}

// loadJob returns a job of this node, nil when it holds no key for it; h.mu is held.
func (h *Hiring) loadJob(id string) (*hireJob, error) {
	if j := h.jobs[id]; j != nil {
		j.seen = h.cfg.Now()
		return j, nil
	}
	if element("job", id) != nil {
		return nil, nil
	}
	key, err := h.cfg.Jobs.Load(id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	j := &hireJob{}
	p := filepath.Join(h.cfg.Jobs.Dir(), id+".json")
	if err := readJSON(p, j); err != nil {
		return nil, err
	}
	j.key = key
	j.init(h.cfg.Now())
	h.jobs[id] = j
	return j, nil
}

// init makes a job's maps, and marks it used at now.
func (j *hireJob) init(now time.Time) {
	if j.Tasks == nil {
		j.Tasks = map[string]*hireTask{}
	}
	if j.Inputs == nil {
		j.Inputs = map[string]*hireInput{}
	}
	if j.Artifacts == nil {
		j.Artifacts = map[string]*hireArtifact{}
	}
	if j.sent == nil {
		j.sent = map[string]a2awire.Part{}
	}
	if j.calls == nil {
		j.calls = map[string]*hireCall{}
	}
	j.seen = now
}

// track records a request of the session whose answer must be sealed, by its id; the oldest answered ones go when
// the session holds maxSessionCalls.
func (j *hireJob) track(id json.RawMessage, c *hireCall) {
	k, ok := mcpwire.IDKey(id)
	if !ok {
		return
	}
	if len(j.calls) >= maxSessionCalls {
		for ck, cc := range j.calls {
			if cc.done {
				delete(j.calls, ck)
			}
		}
	}
	j.calls[k] = c
}

// call is the session's request whose answer m is, nil when m answers none the node tracks. A response answers a
// tracked request when the local client would take it for that request's answer (mcpwire.AnswerKeys: "8" answers
// request 8 for the TypeScript SDK), so that a gate cannot pass its own plaintext result as a sealed call's by
// spelling the id otherwise (rule 4a; round 2 other-fixes-2).
func (j *hireJob) call(m *mcpwire.Message) *hireCall {
	if j == nil || m.Method != "" || len(m.ID) == 0 {
		return nil
	}
	for _, k := range mcpwire.AnswerKeys(m.ID) {
		if c := j.calls[k]; c != nil {
			return c
		}
	}
	return nil
}

func (h *Hiring) saveJob(j *hireJob) error {
	return writeJSON(h.cfg.Jobs.Dir(), j.ID+".json", j)
}

// renameJob moves a provisional job to the gate's task id once the first answer names it (nodekey.JobKeys.Rename);
// h.mu is held.
func (h *Hiring) renameJob(j *hireJob, to string) error {
	if j.ID == to {
		return nil
	}
	if element("job", to) != nil {
		return reject(sealed.RejectWrongTask, "the answer's task id is not a job id")
	}
	from := j.ID
	if err := h.cfg.Jobs.Rename(from, to); err != nil {
		return err
	}
	delete(h.jobs, from)
	j.ID = to
	h.jobs[to] = j
	if err := h.saveJob(j); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(h.cfg.Jobs.Dir(), from+".json"))
	return nil
}

// endJob deletes a job's key and state now, or records when to (kontra.keyRetainUntil); h.mu is held.
func (h *Hiring) endJob(j *hireJob, retainUntil time.Time) {
	now := h.cfg.Now()
	if retainUntil.After(now) {
		if !j.DeleteAt.Equal(retainUntil) {
			j.DeleteAt = retainUntil
			if err := h.saveJob(j); err != nil {
				h.log.Warn("sealing: a job's retention was not recorded", "job", j.ID, "err", err)
			}
		}
		return
	}
	h.dropJob(j)
}

func (h *Hiring) dropJob(j *hireJob) {
	if err := h.cfg.Jobs.Delete(j.ID); err != nil {
		h.log.Warn("sealing: a per-job key was not deleted", "job", j.ID, "err", err)
		return
	}
	_ = os.Remove(filepath.Join(h.cfg.Jobs.Dir(), j.ID+".json"))
	delete(h.jobs, j.ID)
	h.log.Info("sealing: the job ended: its per-job key is deleted", "job", j.ID)
}

// Sweep deletes the keys whose retention (kontra.keyRetainUntil) has passed, the provisional keys of first messages
// that never got a gate task (provisionalMaxAge), and the keys of MCP sessions whose tasks are done and that have
// been idle for the session_idle_s their gate stated, or sessionJobIdle (the gate closes an idle session on its own,
// relay.md §11.2, and a client may leave without DELETE; D224).
func (h *Hiring) Sweep() { h.sweepJobs() }

// Bounds of the hiring side's job keys and caches.
const (
	// provisionalMaxAge is how long a new task's provisional key is kept without a gate task id: a first message
	// whose exchange the hiring server saw end releases it at once (Job.Release); this is the backstop.
	provisionalMaxAge = DefaultMaxAge
	// sessionJobIdle is how long an MCP session's key is kept with no exchange once its tasks are done when its gate
	// stated no session_idle_s in the initialize result (D224): past the gate's default session_idle_s (an hour), so
	// that only sessions the gate has closed lose theirs; a session the gate keeps longer is initialized again (the
	// node answers 404 for a session it no longer knows).
	sessionJobIdle = 24 * time.Hour
	// maxSessionIdle bounds the session_idle_s a gate states (D224): a key is the node's to delete, and a gate cannot
	// have the node keep an abandoned session's key for longer than this.
	maxSessionIdle = 30 * 24 * time.Hour
)

// idleBound is how long a session job is kept with no exchange once its tasks are done: the session_idle_s its gate
// stated (D224), or sessionJobIdle.
func (j *hireJob) idleBound() time.Duration {
	if j.IdleS > 0 {
		return time.Duration(j.IdleS) * time.Second
	}
	return sessionJobIdle
}

// sessionIdleOf reads _meta["sh.kontra/session_idle_s"] of an initialize result (D224): a positive whole number of
// seconds, at most maxSessionIdle; 0 when the gate stated none, or stated one the node does not take.
func sessionIdleOf(result json.RawMessage) time.Duration {
	meta, ok := mcpwire.Member(result, "_meta")
	if !ok {
		return 0
	}
	raw, ok := mcpwire.Member(meta, mcpwire.MetaSessionIdle)
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || n <= 0 || n > int64(maxSessionIdle/time.Second) {
		return 0
	}
	return time.Duration(n) * time.Second
}

func (h *Hiring) sweepJobs() {
	ents, err := os.ReadDir(h.cfg.Jobs.Dir())
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.cfg.Now()
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || element("job", id) != nil {
			continue
		}
		if lj := h.jobs[id]; lj != nil {
			switch {
			case strings.HasPrefix(id, "mcp-"):
				if now.Sub(lj.seen) >= lj.idleBound() && lj.tasksDone() {
					h.endSession(lj)
				}
			case !lj.DeleteAt.IsZero() && !now.Before(lj.DeleteAt), provisional(id) && now.Sub(lj.Created) >= provisionalMaxAge:
				h.dropJob(lj)
			case now.Sub(lj.seen) >= jobCacheIdle: // as the hosting side's
				delete(h.jobs, id) // read again from its file when an answer names it
			}
			continue
		}
		var j hireJob
		if readJSON(filepath.Join(h.cfg.Jobs.Dir(), e.Name()), &j) != nil {
			continue
		}
		j.ID = id
		switch {
		case !j.DeleteAt.IsZero() && !now.Before(j.DeleteAt):
			h.dropJob(&j)
		case provisional(id) && now.Sub(j.Created) >= provisionalMaxAge:
			h.dropJob(&j)
		case strings.HasPrefix(id, "mcp-") && j.tasksDone():
			// A session job not loaded: no exchange of it since this node started (its sessions are 404 here).
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) >= j.idleBound() {
				h.dropJob(&j)
			}
		}
	}
}

// tasksDone reports a session job whose tasks are all done.
func (j *hireJob) tasksDone() bool {
	for _, t := range j.Tasks {
		if !t.Done {
			return false
		}
	}
	return true
}

// endSession drops an MCP session's key and the session's entries; h.mu is held.
func (h *Hiring) endSession(hj *hireJob) {
	for sid, si := range h.services {
		if si.session != "" && "mcp-"+si.session == hj.ID {
			delete(h.services, sid)
		}
	}
	h.dropJob(hj)
}

// Release drops a new task's provisional per-job key when the exchange of its first message ended without an answer
// naming its gate task: the gate refused it (a non-2xx), the exchange failed before an answer, or the answer named no
// task. Nothing will ever be sealed to that key, and nothing would end it. A job that has its gate task id is kept.
// The hiring server calls it on every path where the exchange ends; Response does on a refusal and once a whole
// answer was read.
func (j *Job) Release() {
	if j == nil || j.mcp || !j.newTask || j.job == nil {
		return
	}
	h := j.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if hj := h.jobs[j.job.ID]; hj == j.job && provisional(hj.ID) {
		h.dropJob(hj)
	}
}

// --- requests ---

// Request seals a local client's request to a sealed service (the card's or initialize's recipients), or
// returns it unchanged for a service that is not sealed. A pin refusal is a *pins.ChangeError, answered locally.
// service is the request's {svc} path (/agents/… or /mcp/…).
func (h *Hiring) Request(ctx context.Context, service string, req *http.Request) (*http.Request, *Job, error) {
	if strings.HasPrefix(service, "/mcp/") {
		return h.mcpRequest(ctx, service, req)
	}
	if strings.HasSuffix(req.URL.Path, "/.well-known/agent-card.json") || strings.Contains(req.URL.Path, "/.well-known/") {
		return req, nil, nil // cards: verified and recorded by the hiring server (CardSeen)
	}
	si, err := h.info(ctx, service)
	if err != nil {
		return nil, nil, err
	}
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
		_ = req.Body.Close()
		if err != nil || len(b) > maxBody {
			return nil, nil, local(http.StatusRequestEntityTooLarge, "kontra connect: the request body could not be read")
		}
		body = b
	}
	j := &Job{h: h, service: service, info: si}
	if !si.sealed {
		restore(req, body) // nothing of it is the node's to read
		return req, j, nil
	}
	x := &Exchange{}
	o, err := x.classify(req, body)
	if err != nil {
		return nil, nil, local(http.StatusBadRequest, "kontra connect: %s", reasonText(err))
	}
	j.jsonrpc, j.rpcID, j.opKind, j.stream = x.jsonrpc, x.rpcID, o.kind, o.stream
	if x.jsonrpc && !o.known || o.unreadable {
		// A request the node cannot read as one of A2A 1.0's (an A2A 0.3 message/send, a body that is not one JSON
		// object) would reach the gate unsealed: nothing is sent in plaintext to a service known to be sealed.
		return nil, nil, local(http.StatusBadRequest, "kontra connect: a request to a sealed service that is not one of A2A 1.0's (it cannot be sealed)")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case o.kind == opSend:
		out, err := h.sealSend(j, body)
		if err != nil {
			return nil, nil, err
		}
		body = out
	case len(o.taskIDs) > 0:
		// The task the route names (the last "/tasks/" segment's, as a router without a tenant reads it).
		id := o.taskIDs[0]
		if !x.jsonrpc {
			id = lastRouteTask(o.taskIDs)
		}
		hj, err := h.loadJob(id)
		if err != nil {
			return nil, nil, local(http.StatusInternalServerError, "kontra connect: the job's key: %v", err)
		}
		j.job = hj // nil: answers whose sealed parts this node cannot open are refused (rule 4)
	default:
		j.list = true
	}
	restore(req, body)
	return req, j, nil
}

// lastRouteTask is the task id of a route's last "/tasks/{id}[:action]" segment (tasksRoute lists each segment's
// candidates in order, the one without its action last).
func lastRouteTask(ids []string) string {
	return ids[len(ids)-1]
}

func restore(req *http.Request, body []byte) {
	if len(body) == 0 {
		req.Body = http.NoBody
		req.ContentLength = 0
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Length")
	req.TransferEncoding = nil
}

// sealSend seals every part of a send request's message (rules 1–3): a new task gets a provisional job, a key, an
// rk and a contextId when it has none; a follow-up uses its task's job. h.mu is held.
func (h *Hiring) sealSend(j *Job, body []byte) ([]byte, error) {
	if len(j.info.recipients) == 0 {
		return nil, local(http.StatusBadGateway, "kontra connect: the sealed service has no recipient (no node is enrolled for it)")
	}
	var lerr error
	binding := a2awire.BindingHTTP
	if j.jsonrpc {
		binding = a2awire.BindingJSONRPC
	}
	w := a2awire.Walker{Binding: binding, Container: func(c *a2awire.Container) error {
		if c.Where != a2awire.WhereRequest {
			return nil
		}
		msgID, _ := a2awire.String(c.Members, "messageId")
		if msgID == "" {
			lerr = local(http.StatusBadRequest, "kontra connect: a message to a sealed service needs a messageId")
			return lerr
		}
		taskID, _ := a2awire.String(c.Members, "taskId")
		ctxID, _ := a2awire.String(c.Members, "contextId")
		var job *hireJob
		if taskID != "" {
			var err error
			if job, err = h.loadJob(taskID); err != nil {
				lerr = local(http.StatusInternalServerError, "kontra connect: the job's key: %v", err)
				return lerr
			}
			if job == nil {
				lerr = &LocalError{Status: http.StatusBadRequest, Err: fmt.Errorf("kontra connect: %w (task %s)", ErrNoKey, taskID)}
				return lerr
			}
			if ctxID == "" {
				ctxID = job.ContextID
			}
		} else {
			var err error
			if job, err = h.newJob("", j.service); err != nil {
				lerr = local(http.StatusInternalServerError, "kontra connect: a per-job key could not be minted: %v", err)
				return lerr
			}
			j.newTask = true
			if ctxID == "" {
				ctxID = uuid.NewString()
			}
			job.ContextID = ctxID
			if err := h.saveJob(job); err != nil {
				lerr = local(http.StatusInternalServerError, "kontra connect: the job's state: %v", err)
				return lerr
			}
			c.Set("contextId", a2awire.Quote(ctxID))
		}
		j.job = job
		rk, _ := sealed.DecodeRK(job.RK)
		base := sealed.Header{V: sealed.Version, Dir: sealed.DirRequest, IAT: h.cfg.Now().Unix(), TaskID: taskID,
			ContextID: ctxID, MessageID: msgID, Kids: j.info.kids, ReplyTo: job.key.Recipient().String(), RK: sealed.EncodeRK(rk)}
		out := make([]a2awire.Part, len(c.Parts))
		for i, p := range c.Parts {
			if _, claims, _ := sealed.ParseDeliveredPart(a2awire.Encode(p)); claims {
				lerr = local(http.StatusBadRequest, "kontra connect: a part already has the sealed envelope type")
				return lerr
			}
			pl, err := plainOfPart(p)
			if err != nil {
				lerr = local(http.StatusBadRequest, "kontra connect: %s", reasonText(err))
				return lerr
			}
			hd := base
			hd.Part, hd.Parts, hd.Kind, hd.MediaType = i, len(c.Parts), pl.kind, pl.mediaType
			ct, err := sealed.Seal(j.info.recipients, hd, pl.body)
			if err != nil {
				lerr = local(http.StatusInternalServerError, "kontra connect: sealing failed: %v", err)
				return lerr
			}
			out[i] = sealedPartOf(ct, p, pl)
			job.sent[ctHash(ct)] = p
		}
		c.SetParts(out)
		md := map[string]json.RawMessage{}
		if raw, ok := c.Members["metadata"]; ok {
			if m, err := strictjson.Members(raw); err == nil {
				md = m
			}
		}
		md[sealed.MetadataReplyTo] = a2awire.Quote(base.ReplyTo)
		c.Set("metadata", a2awire.Encode(md))
		return nil
	}}
	out, _, err := w.WalkRequest(body)
	if lerr != nil {
		return nil, lerr
	}
	if err != nil {
		// The error is not quoted: it may hold a byte of the client's plaintext (a JSON syntax error).
		return nil, local(http.StatusBadRequest, "kontra connect: not an A2A 1.0 send request with exact keys (D172)")
	}
	if j.job == nil {
		return nil, local(http.StatusBadRequest, "kontra connect: a send request without a message")
	}
	return out, nil
}

// provisional reports a job id newJob minted for a new task's first message: "p" and 24 hex digits, which no gate
// task id (a UUID) is.
func provisional(id string) bool {
	return len(id) == 25 && id[0] == 'p' && isHex(id[1:])
}

func ctHash(ct []byte) string {
	s := sha256.Sum256(ct)
	return hex.EncodeToString(s[:])
}

// --- answers ---

// Response opens the gate's answer for job (unary or SSE), inlining kontra-blob: parts fetched on blob streams.
// (The hiring server inlines them first; a part is opened once its whole age file is read.)
//
// For a sealed service every 2xx answer is opened, whatever it is labelled, as the local client reads it (a2a-go
// v2.7.0 a2aclient: a unary request's answer is decoded as JSON whatever its Content-Type; a streaming request's is
// read as SSE unless it is labelled JSON, an absent or unparsable Content-Type included, and always over
// HTTP+JSON): a gate cannot slip unsealed content past rule 4a by labelling it otherwise. An answer the node cannot
// read at all (not JSON, over the node's bound, not an A2A 1.0 answer with exact keys) is replaced by the local
// client's failure (sealed_rejected); a non-2xx answer is an error no client reads as one, and passes.
func (h *Hiring) Response(ctx context.Context, job *Job, resp *http.Response) (*http.Response, error) {
	if job == nil {
		return resp, nil
	}
	if job.mcp {
		return h.mcpResponse(ctx, job, resp)
	}
	if !job.info.sealed || resp.StatusCode/100 != 2 {
		job.Release() // the gate refused the first message: no task, and nothing to open
		return resp, nil
	}
	ct := resp.Header.Get("Content-Type")
	if job.stream && !a2awire.IsJSON(ct) {
		body := resp.Body
		pr, pw := io.Pipe()
		go func() {
			err := sseTransformMulti(body, pw, func(data []byte) ([][]byte, error) {
				out, f, err := h.openA2A(job, data)
				if err != nil {
					return nil, err
				}
				if f != nil {
					// The event without its refused parts (none when the whole event was refused), then the failed
					// status, and the stream ends.
					if out == nil {
						return [][]byte{failedEvent(job, h.failureOf(job, f))}, errStopAfter
					}
					return [][]byte{out, failedEvent(job, f)}, errStopAfter
				}
				return [][]byte{out}, nil
			})
			_ = body.Close()
			if errors.Is(err, errEventTooLarge) {
				// An event over the node's bound: the failed status, and the stream ends.
				f := h.failureOf(job, &failure{reason: sealed.RejectBadHeader})
				_, err = pw.Write([]byte("data: " + string(failedEvent(job, f)) + "\n\n"))
			}
			job.Release() // a stream that named no task: its provisional key goes
			_ = pw.CloseWithError(err)
		}()
		resp.Body = pr
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		resp.Header.Set("Content-Type", "text/event-stream")
		return resp, nil
	}
	defer job.Release() // an answer that named no task: its provisional key goes
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		h.log.Warn("sealing: an answer over the node's bound is refused (rule 4)", "service", job.info.name)
		return h.refuseAnswer(job, resp, &failure{reason: sealed.RejectBadHeader}), nil
	}
	if len(bytes.TrimSpace(b)) == 0 {
		setBody(resp, b)
		return resp, nil
	}
	out, f, err := h.openA2A(job, b)
	if err != nil {
		return nil, err
	}
	if f != nil && out == nil {
		return h.refuseAnswer(job, resp, f), nil
	}
	if f != nil {
		if out, err = failTask(job, out, f); err != nil {
			return nil, err
		}
	}
	if !a2awire.IsJSON(ct) {
		resp.Header.Set("Content-Type", "application/json") // what it was read as
	}
	setBody(resp, out)
	return resp, nil
}

// failureOf completes a failure with the job's task when the answer named none.
func (h *Hiring) failureOf(job *Job, f *failure) *failure {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f.taskID == "" && job.job != nil && !provisional(job.job.ID) {
		f.taskID, f.context = job.job.ID, job.job.ContextID
	}
	return f
}

// refuseAnswer replaces a 2xx answer the node cannot open at all with the local client's failure (rule 4): the job's
// task with the failed status (kontra.failure sealed_rejected) in the shape the request answers, when the node
// knows the task; otherwise a JSON-RPC error (JSON-RPC) or a 502 (HTTP+JSON). Nothing of the gate's answer is kept.
func (h *Hiring) refuseAnswer(job *Job, resp *http.Response, f *failure) *http.Response {
	f = h.failureOf(job, f)
	status, ct := http.StatusOK, "application/json"
	var body []byte
	switch {
	case f.taskID != "" && (job.opKind == opSend || job.opKind == opOther && !job.list):
		task := map[string]json.RawMessage{"id": a2awire.Quote(f.taskID), "status": failedStatus(f)}
		if f.context != "" {
			task["contextId"] = a2awire.Quote(f.context)
		}
		res := a2awire.Encode(task)
		if job.opKind == opSend {
			res = a2awire.Encode(map[string]json.RawMessage{"task": res})
		}
		body = res
		if job.jsonrpc {
			body = a2awire.Encode(map[string]json.RawMessage{"jsonrpc": a2awire.Quote("2.0"), "id": rpcIDOf(job), "result": res})
		}
	case job.jsonrpc:
		body, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpcIDOf(job), "error": map[string]any{
			"code": -32603, "message": "sealed part rejected: " + string(f.reason),
			"data": map[string]any{"kontra.failure": "sealed_rejected"}}})
	default:
		// google.rpc.Status, as the HTTP+JSON binding answers an error.
		status = http.StatusBadGateway
		body, _ = json.Marshal(map[string]any{"error": map[string]any{"code": status, "status": "INTERNAL",
			"message": "sealed part rejected: " + string(f.reason) + " (kontra.failure sealed_rejected)"}})
	}
	resp.StatusCode, resp.Status = status, fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Header = http.Header{"Content-Type": {ct}}
	resp.Trailer = nil
	setBody(resp, body)
	return resp
}

func rpcIDOf(job *Job) json.RawMessage {
	if len(bytes.TrimSpace(job.rpcID)) == 0 {
		return json.RawMessage("null")
	}
	return job.rpcID
}

// failure is the first sealed part an answer's walk refused.
type failure struct {
	reason          sealed.Reject
	taskID, context string
}

// openA2A opens the sealed parts of one answer body (rule 4, 4a, 5): it returns the body with plaintext parts and
// the refused ones dropped, and the first refusal; or, for a body that is not one A2A 1.0 answer with exact keys,
// no body and the refusal (nothing of it may reach the local client).
func (h *Hiring) openA2A(job *Job, b []byte) ([]byte, *failure, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var (
		fail     *failure
		curTask  string
		curCtx   string
		curJob   = job.job
		terminal = map[string]time.Time{} // jobs ended by this answer -> keyRetainUntil
	)
	refuse := func(r sealed.Reject) {
		h.log.Debug("sealing: a part of an answer is refused (rule 4)", "reason", string(r))
		if fail == nil {
			fail = &failure{reason: r, taskID: curTask, context: curCtx}
		}
	}
	binding := a2awire.BindingHTTP
	if job.jsonrpc {
		binding = a2awire.BindingJSONRPC
	}
	w := a2awire.Walker{
		Binding: binding,
		Object: func(kind a2awire.Kind, m map[string]json.RawMessage) error {
			key := "taskId"
			if kind == a2awire.KindTask {
				key = "id"
			}
			id, _ := a2awire.String(m, key)
			cctx, _ := a2awire.String(m, "contextId")
			if id == "" {
				return nil
			}
			curTask, curCtx = id, cctx
			switch {
			case job.list || curJob == nil && job.job == nil:
				j, err := h.loadJob(id)
				if err != nil {
					return err
				}
				curJob = j
			case curJob != nil && provisional(curJob.ID) && curJob.ID != id:
				// The first answer names the gate task: the provisional job takes its id.
				if err := h.renameJob(curJob, id); err != nil {
					var re *sealed.RejectError
					if errors.As(err, &re) {
						refuse(re.Reason)
						curJob = nil
						return nil
					}
					return err
				}
			case curJob != nil && curJob.ID != id:
				refuse(sealed.RejectWrongTask)
				curJob = nil
			}
			if curJob != nil && terminalState(kind, m) {
				terminal[curJob.ID] = retainUntil(m)
			}
			return nil
		},
		Container: func(c *a2awire.Container) error {
			if len(c.Parts) == 0 {
				return nil
			}
			parts, r := h.openContainer(curJob, curTask, c)
			if r != "" {
				refuse(r)
			}
			c.SetParts(parts)
			return nil
		},
	}
	out, _, err := w.WalkReply(b)
	if err != nil {
		if _, ok := sealed.ReasonOf(err); !ok && !isWalkError(err) {
			return nil, nil, err
		}
		// Not an A2A 1.0 answer with exact keys (D172): a level with a repeated key, or a lone case variant of a member
		// the walk reads, which the local client's decoder would take for the member (a gate's "Artifacts" past rule
		// 4a), or not JSON at all. Nothing of it reaches the local client, and the job is not ended by it.
		h.log.Warn("sealing: an answer that is not an A2A 1.0 answer with exact keys is refused (rule 4)")
		return nil, &failure{reason: sealed.RejectBadHeader, taskID: curTask, context: curCtx}, nil
	}
	for id, until := range terminal {
		if j := h.jobs[id]; j != nil {
			h.endJob(j, until)
		}
	}
	if job.job != nil && h.jobs[job.job.ID] != nil {
		if err := h.saveJob(job.job); err != nil {
			h.log.Warn("sealing: the job's state was not written", "job", job.job.ID, "err", err)
		}
	}
	return out, fail, nil
}

// isWalkError reports an error of the walk itself (a body that is not one A2A 1.0 answer with exact keys), rather
// than of the job's state.
func isWalkError(err error) bool {
	var ke *strictjson.KeyError
	return errors.As(err, &ke) || errors.Is(err, strictjson.ErrNotObject) || errors.Is(err, a2awire.ErrShape)
}

// openContainer opens one message's or artifact's parts with the job's key (rule 4), keeping the gate's own terminal
// text unsealed (4a), and returns the parts for the local client and the first refusal's reason ("" none).
func (h *Hiring) openContainer(j *hireJob, task string, c *a2awire.Container) ([]a2awire.Part, sealed.Reject) {
	role, _ := a2awire.String(c.Members, "role")
	var (
		out    []a2awire.Part
		reason sealed.Reject
	)
	fail := func(r sealed.Reject) {
		h.log.Debug("sealing: a part is refused", "reason", string(r), "where", int(c.Where), "role", role)
		if reason == "" {
			reason = r
		}
	}
	type sealedIn struct {
		i   int
		hd  sealed.Header
		pl  []byte
		raw a2awire.Part
	}
	var ins []sealedIn
	for i, p := range c.Parts {
		sp, claims, err := sealed.ParseDeliveredPart(a2awire.Encode(p))
		switch {
		case claims && err != nil:
			fail(sealed.RejectBadHeader)
			continue
		case claims && sp.BlobRef != "":
			fail(sealed.RejectBadHeader) // the hiring server inlines blobs before the answer is opened
			continue
		case !claims:
			if gateText(c, p) {
				md := map[string]json.RawMessage{}
				if raw, ok := p["metadata"]; ok {
					if m, err := strictjson.Members(raw); err == nil {
						md = m
					}
				}
				md[sealed.MetadataKey] = json.RawMessage("false")
				np := a2awire.Part{}
				for k, v := range p {
					np[k] = v
				}
				np["metadata"] = a2awire.Encode(md)
				out = append(out, np)
				continue
			}
			fail(sealed.RejectBadHeader) // 4a: an unsealed part in a sealed job
			h.log.Debug("sealing: an unsealed part in a sealed job is refused (rule 4a)", "where", int(c.Where))
			continue
		}
		if j == nil {
			fail(sealed.RejectUndecryptable)
			continue
		}
		if role == "ROLE_USER" && c.Where == a2awire.WhereHistory {
			// SPEC-CONFLICT (D215; ext-sealed.md §5 rule 4: every sealed part the node receives for the job is opened
			// with dir res): a Task's history holds the client's own message as the gate stored it, sealed dir req to
			// the service's recipients, which the per-job key cannot open. The closest faithful behaviour: such a
			// part is restored from what this node sealed for the job (its plaintext, kept in memory), and otherwise
			// left out of what the local client gets (it is the client's own content), never refused.
			if orig, ok := j.sent[ctHash(sp.Ciphertext)]; ok {
				out = append(out, orig)
			}
			continue
		}
		hd, pl, err := sealed.Open([]age.Identity{j.key}, sp.Ciphertext)
		if err != nil {
			r, _ := sealed.ReasonOf(err)
			fail(r)
			continue
		}
		ins = append(ins, sealedIn{i: i, hd: hd, pl: pl, raw: p})
	}
	if len(ins) > 0 {
		rk, _ := sealed.DecodeRK(j.RK)
		own := tunnel.KeyID(j.key.Recipient().String())
		mid, _ := a2awire.String(c.Members, "messageId")
		aid, _ := a2awire.String(c.Members, "artifactId")
		artifact := c.Where == a2awire.WhereArtifact || c.Where == a2awire.WhereArtifactUpdate
		ok := true
		for k, in := range ins {
			hd := in.hd
			switch {
			case hd.Dir != sealed.DirReply:
				fail(sealed.RejectBadHeader)
				ok = false
			case !slices.Contains(hd.Kids, own):
				fail(sealed.RejectNotRecipient)
				ok = false
			case sealed.VerifyAuth(rk, hd, in.pl) != nil:
				fail(sealed.RejectBadHeader)
				ok = false
			case hd.TaskID != task:
				fail(sealed.RejectWrongTask)
				ok = false
			case hd.ContextID != j.ContextID && j.ContextID != "":
				fail(sealed.RejectWrongContext)
				ok = false
			case !artifact && (hd.MessageID != mid || hd.Part != k || hd.Parts != len(ins) || hd.Chunk != nil):
				fail(sealed.RejectWrongMessage)
				ok = false
			case artifact && hd.ArtifactID != aid:
				fail(sealed.RejectWrongMessage)
				ok = false
			}
		}
		if ok && artifact {
			if r := h.checkChunks(j, aid, c, ins2(ins, func(s sealedIn) (sealed.Header, []byte) { return s.hd, s.raw["raw"] })); r != "" {
				fail(r)
				ok = false
			}
		}
		if ok {
			byIndex := map[int]a2awire.Part{}
			for _, in := range ins {
				pp, err := plainPart(in.hd, in.pl, in.raw)
				if err != nil {
					fail(sealed.RejectBadHeader)
					ok = false
					break
				}
				byIndex[in.i] = pp
			}
			if ok {
				// Keep the parts in their order: unsealed ones the gate wrote (4a) were appended as they came.
				var ordered []a2awire.Part
				for i := range c.Parts {
					if pp, has := byIndex[i]; has {
						ordered = append(ordered, pp)
					}
				}
				out = append(ordered, out...)
			}
		}
	}
	if out == nil {
		out = []a2awire.Part{}
	}
	return out, reason
}

// ins2 maps the opened parts of a container to their headers and raw ciphertext members.
func ins2[T any](xs []T, f func(T) (sealed.Header, []byte)) []chunkPart {
	out := make([]chunkPart, len(xs))
	for i, x := range xs {
		out[i].hd, out[i].raw = f(x)
	}
	return out
}

type chunkPart struct {
	hd  sealed.Header
	raw []byte
}

// checkChunks is rule 4's artifact check: the parts of each chunk are exactly 0..parts-1 in order, a chunk opened
// before must come again byte for byte (the hosting node reuses ciphertext), a new one must be the next, and an
// update is one chunk. h.mu is held.
//
// SPEC-CONFLICT (D215; ext-sealed.md §5 rule 4: "chunks arrive in order"; §3: chunk counts the updates the hosting
// node sealed for the artifactId): a Task's artifact holds the chunks the gate merged, and after an append: false
// only the chunks since; a hiring node that sees the artifact first in a snapshot (a client that polls) cannot tell
// that from a gate that dropped the first chunks. The closest faithful behaviour: a first sight of an artifact in a
// Task may start at any chunk, its chunks then consecutive; from then on every chunk is in order or a byte-identical
// repeat.
func (h *Hiring) checkChunks(j *hireJob, aid string, c *a2awire.Container, ps []chunkPart) sealed.Reject {
	st := j.Artifacts[aid]
	first := st == nil
	if st == nil {
		st = &hireArtifact{Seen: map[string]string{}}
	}
	if st.Seen == nil {
		st.Seen = map[string]string{}
	}
	update := c.Where == a2awire.WhereArtifactUpdate
	i := 0
	var chunks []int
	for i < len(ps) {
		hd := ps[i].hd
		if hd.Chunk == nil {
			return sealed.RejectBadHeader
		}
		ch, n := *hd.Chunk, hd.Parts
		if i+n > len(ps) {
			return sealed.RejectBadHeader
		}
		for k := 0; k < n; k++ {
			h2 := ps[i+k].hd
			if h2.Chunk == nil || *h2.Chunk != ch || h2.Part != k || h2.Parts != n {
				return sealed.RejectBadHeader
			}
		}
		chunks = append(chunks, ch)
		i += n
	}
	if update && len(chunks) != 1 {
		return sealed.RejectBadHeader
	}
	for k := 1; k < len(chunks); k++ {
		if chunks[k] != chunks[k-1]+1 {
			return sealed.RejectBadHeader
		}
	}
	next := st.Next
	if first && !update && len(chunks) > 0 {
		next = chunks[0]
	}
	i = 0
	for _, ch := range chunks {
		n := ps[i].hd.Parts
		if ch < next {
			for k := 0; k < n; k++ {
				if st.Seen[fmt.Sprintf("%d/%d", ch, k)] != rawHash(ps[i+k].raw) {
					return sealed.RejectReplayed // a chunk seen before, with other bytes
				}
			}
		} else if ch == next {
			for k := 0; k < n; k++ {
				st.Seen[fmt.Sprintf("%d/%d", ch, k)] = rawHash(ps[i+k].raw)
			}
			next++
		} else {
			return sealed.RejectBadHeader // a chunk out of order
		}
		i += n
	}
	st.Next = next
	j.Artifacts[aid] = st
	return ""
}

func rawHash(raw []byte) string {
	s := sha256.Sum256(raw)
	return hex.EncodeToString(s[:])
}

// gateText reports rule 4a's exception: the single text part of a terminal status message the gate wrote, which
// carries metadata["kontra.failure"] (or "kontra.canceled"), or names an upstream terminal state that came without a
// message (spec/relay.md §6.3–§6.4).
func gateText(c *a2awire.Container, p a2awire.Part) bool {
	if c.Where != a2awire.WhereStatus || len(c.Parts) != 1 {
		return false
	}
	text, ok := a2awire.String(p, "text")
	if !ok {
		return false
	}
	st, err := strictjson.Members(c.Owner["status"])
	if err != nil {
		return false
	}
	state, _ := a2awire.String(st, "state")
	switch state {
	case "TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED":
	default:
		return false
	}
	if raw, ok := c.Members["metadata"]; ok {
		if md, err := strictjson.Members(raw); err == nil {
			if _, ok := md["kontra.failure"]; ok {
				return true
			}
			if _, ok := md["kontra.canceled"]; ok {
				return true
			}
		}
	}
	switch text {
	case "the task completed", "the task failed", "the task was canceled", "the task was rejected":
		return true // store.TerminalStatus's own texts
	}
	// relay.Translate's message for an upstream terminal status that came without one (spec/relay.md §6.3).
	if st, ok := strings.CutPrefix(text, "the service ended the task: "); ok {
		switch st {
		case "completed", "failed", "canceled", "rejected":
			return true
		}
	}
	return false
}

// terminalState reports a Task or a status update whose state is terminal.
func terminalState(kind a2awire.Kind, m map[string]json.RawMessage) bool {
	if kind != a2awire.KindTask && kind != a2awire.KindStatusUpdate {
		return false
	}
	st, err := strictjson.Members(m["status"])
	if err != nil {
		return false
	}
	state, _ := a2awire.String(st, "state")
	switch state {
	case "TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED":
		return true
	}
	return false
}

// retainUntil is the task's metadata["kontra.keyRetainUntil"] (RFC 3339), zero when absent.
func retainUntil(m map[string]json.RawMessage) time.Time {
	md, err := strictjson.Members(m["metadata"])
	if err != nil {
		return time.Time{}
	}
	s, _ := a2awire.String(md, "kontra.keyRetainUntil")
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// failedStatus is the status the local client receives for a refused sealed part (rule 4).
func failedStatus(f *failure) json.RawMessage {
	msg := map[string]any{
		"messageId": uuid.NewString(),
		"role":      "ROLE_AGENT",
		"parts":     []map[string]any{{"text": "sealed part rejected: " + string(f.reason)}},
		"metadata":  map[string]any{"kontra.failure": "sealed_rejected", sealed.MetadataKey: false},
	}
	if f.taskID != "" {
		msg["taskId"] = f.taskID
	}
	if f.context != "" {
		msg["contextId"] = f.context
	}
	b, _ := json.Marshal(map[string]any{"state": "TASK_STATE_FAILED", "message": msg,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano)})
	return b
}

// failTask sets the failed status on the Task an answer carries (a Task, {task}, a JSON-RPC result of either).
func failTask(job *Job, body []byte, f *failure) ([]byte, error) {
	top, err := strictjson.Members(body)
	if err != nil {
		return nil, err
	}
	set := func(t map[string]json.RawMessage) {
		t["status"] = failedStatus(f)
	}
	edit := func(m map[string]json.RawMessage) (bool, error) {
		if raw, ok := m["task"]; ok {
			t, err := strictjson.Members(raw)
			if err != nil {
				return false, err
			}
			set(t)
			m["task"] = a2awire.Encode(t)
			return true, nil
		}
		if _, ok := m["status"]; ok {
			set(m)
			return true, nil
		}
		if _, ok := m["message"]; ok {
			// A Message answer with no task: the local client gets the failed task instead.
			delete(m, "message")
			m["task"] = a2awire.Encode(map[string]json.RawMessage{"id": a2awire.Quote(f.taskID),
				"contextId": a2awire.Quote(f.context), "status": failedStatus(f)})
			return true, nil
		}
		return false, nil
	}
	if _, ok := top["jsonrpc"]; ok {
		res, err := strictjson.Members(top["result"])
		if err != nil {
			return body, nil
		}
		if changed, err := edit(res); err != nil || !changed {
			return body, err
		}
		top["result"] = a2awire.Encode(res)
		return a2awire.Encode(top), nil
	}
	if changed, err := edit(top); err != nil || !changed {
		return body, err
	}
	return a2awire.Encode(top), nil
}

// failedEvent is the SSE event's data that ends a stream with the failed status: a final statusUpdate.
func failedEvent(job *Job, f *failure) []byte {
	upd := map[string]json.RawMessage{"taskId": a2awire.Quote(f.taskID), "contextId": a2awire.Quote(f.context),
		"status": failedStatus(f), "final": json.RawMessage("true")}
	res := a2awire.Encode(map[string]json.RawMessage{"statusUpdate": a2awire.Encode(upd)})
	if job.jsonrpc {
		id := job.rpcID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		res = a2awire.Encode(map[string]json.RawMessage{"jsonrpc": a2awire.Quote("2.0"), "id": id, "result": res})
	}
	return res
}

// --- MCP ---

// mcpRequest is a local client's MCP message: an initialize learns the service's sealing from its result; on a
// sealed session a tools/call's arguments and an answer to a sealed input request are sealed (§3), and no
// Mcp-Param-* header is sent.
func (h *Hiring) mcpRequest(ctx context.Context, service string, req *http.Request) (*http.Request, *Job, error) {
	j := &Job{h: h, service: service, mcp: true, sid: req.Header.Get(mcpwire.HeaderSessionID)}
	if j.sid != "" {
		s := sha256.Sum256([]byte(j.sid))
		j.session = hex.EncodeToString(s[:])
	}
	if req.Method == http.MethodDelete {
		j.delete = true
		return req, j, nil
	}
	if req.Method != http.MethodPost {
		// A GET stream: the input requests of a sealed session's tasks may come on it (a passthrough task's), opened
		// like those of a tasks/result stream.
		h.mu.Lock()
		j.info = h.services[j.sid]
		h.mu.Unlock()
		if j.info == nil && j.sid != "" && h.sealedTarget(service, "") {
			// A session this node does not know (it restarted, or dropped the session's key, D224) to a service whose
			// pin says sealed (or cannot be read): what comes on its stream would reach the client unread, a result
			// for a sealed call it still waits for included, so it is answered here as a POST is: 404, and the client
			// initializes again (round 2 sealing-fixes-4).
			return nil, nil, &LocalError{Status: http.StatusNotFound,
				Err: errors.New("kontra connect: an MCP session this node does not know: initialize again")}
		}
		j.get = req.Method == http.MethodGet
		return req, j, nil
	}
	b, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
	_ = req.Body.Close()
	if err != nil || len(b) > maxBody {
		return nil, nil, local(http.StatusRequestEntityTooLarge, "kontra connect: the request body could not be read")
	}
	m, err := mcpwire.Parse(b, mcpwire.FromClient)
	if err != nil {
		if h.sealedTarget(service, j.sid) {
			// To a service known to be sealed nothing goes that the node cannot read: it may hold arguments in
			// plaintext under a key the gate reads otherwise (D172).
			var id json.RawMessage
			if m != nil {
				id = m.ID
			}
			const msg = "kontra connect: not one JSON-RPC message with exact keys, to a sealed service (D172)"
			body, berr := mcpwire.ErrorBody(id, mcpwire.CodeInvalidRequest, msg, nil)
			if berr != nil {
				body, _ = mcpwire.ErrorBody(nil, mcpwire.CodeInvalidRequest, msg, nil)
			}
			return nil, nil, &LocalError{Status: http.StatusBadRequest, Body: body, Err: errors.New(msg)}
		}
		// The gate answers it (-32600 or -32700): nothing of it is sealed.
		restore(req, b)
		return req, nil, nil
	}
	j.reqMethod, j.reqID = m.Method, m.ID
	if m.Method == mcpwire.MethodInitialize {
		j.init = true
		restore(req, b)
		return req, j, nil
	}
	if j.sid == "" && h.sealedTarget(service, "") {
		// A request without a session (a 2026-07-28 client's first request, D162) to a service pinned sealed is
		// answered here as a legacy server answers it (relay.md §11.12 rule 3), so that the client falls back to
		// initialize: nothing of it, Mcp-Param-* headers included, reaches the gate in plaintext (ext-sealed.md §2).
		return nil, nil, &LocalError{Status: http.StatusBadRequest, Body: mcpwire.LegacyNoSessionBody,
			Err: errors.New("kontra connect: a request without a session to a sealed MCP service: initialize first")}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	si := h.services[j.sid]
	if si == nil {
		if j.sid != "" {
			// A session this node did not see initialized (it restarted): its sealing is unknown, so nothing is sent;
			// the client re-initializes on 404 (MCP's rule for an unknown session).
			return nil, nil, &LocalError{Status: http.StatusNotFound, Err: errors.New("kontra connect: an MCP session this node does not know: initialize again")}
		}
		restore(req, b)
		return req, nil, nil
	}
	j.info = si
	if !si.sealed {
		restore(req, b)
		return req, j, nil
	}
	for k := range req.Header {
		if strings.HasPrefix(strings.ToLower(k), strings.ToLower(mcpwire.HeaderParamPrefix)) {
			req.Header.Del(k) // §2: never for a sealed call
		}
	}
	if m.Method != "" && len(m.ID) > 0 && j.session != "" {
		// A request that reuses the id of one whose answer was sealed: that one's record goes, so that this one's
		// answer is not taken for it (a sealed call or task result is tracked again below).
		if hj, err := h.loadJob("mcp-" + j.session); err == nil && hj != nil {
			if k, ok := mcpwire.IDKey(m.ID); ok {
				delete(hj.calls, k)
			}
		}
	}
	switch {
	case m.Method == mcpwire.MethodToolsCall:
		out, err := h.sealCall(j, b, m)
		if err != nil {
			return nil, nil, err
		}
		b = out
	case m.Method == "" && len(m.ID) > 0:
		out, err := h.sealAnswer(j, b, m)
		if err != nil {
			return nil, nil, err
		}
		b = out
	case strings.HasPrefix(m.Method, "tasks/"):
		j.taskOf, _ = memberString(m.Params, "taskId")
		if m.Method == "tasks/result" && j.session != "" && len(m.ID) > 0 {
			// A sealed task's result is opened (or refused) on whichever stream of the session it comes.
			if hj, err := h.loadJob("mcp-" + j.session); err == nil && hj != nil && hj.Tasks[j.taskOf] != nil {
				t := hj.Tasks[j.taskOf]
				hj.track(m.ID, &hireCall{Tool: t.Tool, MessageID: t.MessageID, Task: j.taskOf})
			}
		}
	}
	restore(req, b)
	return req, j, nil
}

// sealedTarget reports an MCP request the node must read before anything of it is sent: on a session the node saw
// initialized sealed, or, without a session (or on one it does not know), to a service whose pin says sealed (an
// unreadable pin counts as sealed: nothing is sent in plaintext to a service that may be).
func (h *Hiring) sealedTarget(service, sid string) bool {
	if sid != "" {
		h.mu.Lock()
		si := h.services[sid]
		h.mu.Unlock()
		if si != nil {
			return si.sealed
		}
	}
	pin, known, err := h.cfg.Pins.Get(h.cfg.Gate, pinName(service))
	return err != nil || known && pin.Sealed
}

// sessionJob is the per-job key of an MCP session (its plain calls and the tasks created in it); h.mu is held.
func (h *Hiring) sessionJob(j *Job) (*hireJob, error) {
	id := "mcp-" + j.session
	hj, err := h.loadJob(id)
	if err != nil || hj != nil {
		return hj, err
	}
	hj, err = h.newJob(id, j.service)
	if err != nil {
		return nil, err
	}
	hj.Session = j.session
	if j.info != nil && j.info.idle > 0 {
		hj.IdleS = int64(j.info.idle / time.Second) // the gate's session_idle_s (D224)
	}
	return hj, h.saveJob(hj)
}

// sealCall seals a tools/call's arguments (§3, rule 3): params.arguments = {} and the envelope in
// params._meta["sh.kontra/sealed"].
func (h *Hiring) sealCall(j *Job, b []byte, m *mcpwire.Message) ([]byte, error) {
	if j.session == "" {
		return nil, local(http.StatusBadRequest, "kontra connect: a sealed tools/call needs an MCP session")
	}
	if len(j.info.recipients) == 0 {
		return nil, local(http.StatusBadGateway, "kontra connect: the sealed service has no recipient (no node is enrolled for it)")
	}
	params, err := strictjson.Members(m.Params)
	if err != nil {
		return nil, local(http.StatusBadRequest, "kontra connect: a tools/call without params")
	}
	name, _ := a2awire.String(params, "name")
	args := json.RawMessage("{}")
	if v, ok := params["arguments"]; ok {
		args = v
	}
	body, err := jcs.Transform(args)
	if err != nil {
		return nil, local(http.StatusBadRequest, "kontra connect: arguments with no canonical form")
	}
	hj, err := h.sessionJob(j)
	if err != nil {
		return nil, local(http.StatusInternalServerError, "kontra connect: a per-job key could not be minted: %v", err)
	}
	msgID := mcpID(m.ID)
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirRequest, IAT: h.cfg.Now().Unix(), Session: j.session, Tool: name,
		MessageID: msgID, Part: 0, Parts: 1, Kind: sealed.KindData, Kids: j.info.kids,
		ReplyTo: hj.key.Recipient().String(), RK: hj.RK}
	ct, err := sealed.Seal(j.info.recipients, hd, body)
	if err != nil {
		return nil, local(http.StatusInternalServerError, "kontra connect: sealing failed: %v", err)
	}
	if err := setEnvelope(params, ct); err != nil {
		return nil, local(http.StatusBadRequest, "kontra connect: %v", err)
	}
	params["arguments"] = json.RawMessage("{}")
	j.call = &sealedCall{tool: name, messageID: msgID}
	hj.track(m.ID, &hireCall{Tool: name, MessageID: msgID})
	out, err := mcpwire.SetMember(b, "params", a2awire.Encode(params))
	if err != nil {
		return nil, local(http.StatusBadRequest, "kontra connect: %v", err)
	}
	return out, nil
}

// setEnvelope puts {"v": 1, "envelope"} at obj._meta["sh.kontra/sealed"], keeping _meta's other keys.
func setEnvelope(obj map[string]json.RawMessage, ct []byte) error {
	meta := map[string]json.RawMessage{}
	if raw, ok := obj["_meta"]; ok {
		m, err := strictjson.Members(raw)
		if err != nil {
			return errors.New("_meta is not one object with exact keys")
		}
		meta = m
	}
	env, err := json.Marshal(sealed.NewMCPEnvelope(ct))
	if err != nil {
		return err
	}
	meta[mcpwire.MetaSealed] = env
	obj["_meta"] = a2awire.Encode(meta)
	return nil
}

// sealAnswer seals the local client's answer to an input request this node opened (§3, D175): a request with the
// input request's session, tool and messageId, the job's replyTo and rk; result = {"_meta": {<related-task>,
// "sh.kontra/sealed"}}.
func (h *Hiring) sealAnswer(j *Job, b []byte, m *mcpwire.Message) ([]byte, error) {
	if j.session == "" {
		return b, nil
	}
	hj, err := h.loadJob("mcp-" + j.session)
	if err != nil || hj == nil {
		return b, nil
	}
	in := hj.Inputs[mcpID(m.ID)]
	if in == nil {
		return b, nil // not an answer to a sealed input request
	}
	if m.Error != nil || m.Result == nil {
		return nil, local(http.StatusBadRequest, "kontra connect: the answer to a sealed input request must be a result (it is sealed)")
	}
	res, err := strictjson.Members(m.Result)
	if err != nil {
		return nil, local(http.StatusBadRequest, "kontra connect: an answer that is not one object with exact keys")
	}
	var related json.RawMessage
	if raw, ok := res["_meta"]; ok {
		if meta, err := strictjson.Members(raw); err == nil {
			related = meta[mcpwire.MetaRelatedTask]
		}
	}
	delete(res, "_meta")
	body, err := jcs.Transform(a2awire.Encode(res))
	if err != nil {
		return nil, local(http.StatusBadRequest, "kontra connect: an answer with no canonical form")
	}
	// The answer names the input request's gate task as its header did (sealing's mcpRule1 SPEC-CONFLICT), so that
	// it cannot be delivered as another task's answer.
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirRequest, IAT: h.cfg.Now().Unix(), Session: j.session,
		Tool: in.Tool, MessageID: in.MessageID, TaskID: in.TaskID, Part: 0, Parts: 1, Kind: sealed.KindData,
		Kids: j.info.kids, ReplyTo: hj.key.Recipient().String(), RK: hj.RK}
	ct, err := sealed.Seal(j.info.recipients, hd, body)
	if err != nil {
		return nil, local(http.StatusInternalServerError, "kontra connect: sealing failed: %v", err)
	}
	out := map[string]json.RawMessage{}
	if related != nil {
		out["_meta"] = a2awire.Encode(map[string]json.RawMessage{mcpwire.MetaRelatedTask: related})
	}
	if err := setEnvelope(out, ct); err != nil {
		return nil, err
	}
	nb, err := mcpwire.SetMember(b, "result", a2awire.Encode(out))
	if err != nil {
		return nil, local(http.StatusBadRequest, "kontra connect: %v", err)
	}
	return nb, nil
}

// mcpResponse opens the gate's MCP answer (JSON or SSE), message by message.
//
// On a sealed session nothing reaches the local client unread: a 2xx answer that is neither JSON nor an event stream
// (a GET stream's is read as one whatever it is labelled, as clients read it), a body over the node's bound and a
// message that is not one JSON-RPC message with exact keys (D172) are refused like an unsealed result (rule 4a): a
// JSON answer becomes the node's sealed_rejected error, and an event stream ends before the event.
func (h *Hiring) mcpResponse(_ context.Context, j *Job, resp *http.Response) (*http.Response, error) {
	if j.delete {
		if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotFound {
			h.closeSession(j)
		}
		return resp, nil
	}
	if !j.init && resp.StatusCode == http.StatusNotFound && j.sid != "" {
		// The gate closed the session (idle, tunnel_lost, revoked, upstream_closed: relay.md §11.2 answers 404): its
		// key goes once its tasks are done, as on the client's DELETE, and the client initializes again.
		h.closeSession(j)
		return resp, nil
	}
	if j.init {
		if sid := resp.Header.Get(mcpwire.HeaderSessionID); sid != "" {
			j.sid = sid
			s := sha256.Sum256([]byte(sid))
			j.session = hex.EncodeToString(s[:])
		}
	}
	if !j.init && (j.info == nil || !j.info.sealed) {
		return resp, nil
	}
	sealedSession := !j.init
	ok := resp.StatusCode/100 == 2
	ct := resp.Header.Get("Content-Type")
	switch {
	case a2awire.IsEventStream(ct), sealedSession && ok && j.get && !a2awire.IsJSON(ct):
		if !a2awire.IsEventStream(ct) {
			resp.Header.Set("Content-Type", "text/event-stream") // what it was read as
		}
		body := resp.Body
		pr, pw := io.Pipe()
		go func() {
			err := sseTransform(body, pw, func(data []byte) ([]byte, error) { return h.mcpMessage(j, data) })
			_ = body.Close()
			if errors.Is(err, errUnreadable) {
				h.log.Warn("sealing: an event of a sealed session that is not one JSON-RPC message with exact keys: the stream ends before it (rule 4)")
				err = nil // the stream ends cleanly before the event: nothing of it is relayed
			}
			_ = pw.CloseWithError(err)
		}()
		resp.Body = pr
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return resp, nil
	case !a2awire.IsJSON(ct):
		if !sealedSession || !ok {
			return resp, nil
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			setBody(resp, nil) // a 202 without a body
			return resp, nil
		}
		h.log.Warn("sealing: an answer of a sealed session that is neither JSON nor an event stream is refused (rule 4)")
		return h.mcpFailResponse(j, resp), nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		if sealedSession && ok {
			h.log.Warn("sealing: an answer of a sealed session over the node's bound is refused (rule 4)")
			return h.mcpFailResponse(j, resp), nil
		}
		return nil, errors.New("sealing: an MCP answer over the node's bound")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		setBody(resp, b)
		return resp, nil
	}
	out, err := h.mcpMessage(j, b)
	if errors.Is(err, errUnreadable) {
		if !ok {
			setBody(resp, b) // an error status: no client reads it as a message
			return resp, nil
		}
		h.log.Warn("sealing: an answer of a sealed session that is not one JSON-RPC message with exact keys is refused (rule 4)")
		return h.mcpFailResponse(j, resp), nil
	}
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []byte{}
	}
	setBody(resp, out)
	return resp, nil
}

// mcpFailResponse replaces a sealed session's answer with mcpFail's error for the request's id.
func (h *Hiring) mcpFailResponse(j *Job, resp *http.Response) *http.Response {
	b, err := h.mcpFail(&mcpwire.Message{ID: j.reqID})
	if err != nil {
		b, _ = h.mcpFail(&mcpwire.Message{})
	}
	resp.StatusCode, resp.Status = http.StatusOK, "200 OK"
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	resp.Trailer = nil
	setBody(resp, b)
	return resp
}

// errUnreadable is a message of the gate's on a sealed session that is not one JSON-RPC message with exact keys.
var errUnreadable = errors.New("sealing: a message of a sealed session that is not one JSON-RPC message with exact keys")

// mcpMessage opens one message of the gate's for the local client; nil drops it. On a sealed session a message that
// is not one JSON-RPC message with exact keys is errUnreadable: the client's parser would read members the node
// did not (D172).
func (h *Hiring) mcpMessage(j *Job, data []byte) ([]byte, error) {
	m, err := mcpwire.Parse(data, mcpwire.FromUpstream)
	if err != nil {
		if j.init {
			return data, nil // the session's sealing is not known yet: not the node's to read
		}
		return nil, errUnreadable
	}
	if j.init {
		return h.initResult(j, data, m)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	isResp := m.Method == "" && len(m.ID) > 0 && mcpwire.SameID(m.ID, j.reqID)
	hj, err := h.loadJob("mcp-" + j.session)
	if err != nil {
		return nil, err
	}
	if hj != nil {
		hj.seen = h.cfg.Now()
	}
	if c := hj.call(m); c != nil {
		return h.openAnswer(j, hj, c, data, m)
	}
	switch {
	case isResp && m.Result != nil && j.call != nil:
		return h.mcpFail(m) // a sealed call of a session with no job: nothing opens its result
	case m.Method == mcpwire.MethodElicit || m.Method == mcpwire.MethodSample:
		return h.openInput(j, hj, data, m)
	}
	return data, nil
}

// openAnswer opens the answer to a request of the session the node tracks (rule 4): a sealed call's result, or its
// CreateTaskResult (the task is the session's); a sealed task's tasks/result. An error is the gate's or the hosting
// node's own failure (relay.md §11.8): an error, never a part. h.mu is held.
func (h *Hiring) openAnswer(j *Job, hj *hireJob, c *hireCall, data []byte, m *mcpwire.Message) ([]byte, error) {
	if m.Error != nil || m.Result == nil {
		if c.Task != "" {
			if t := hj.Tasks[c.Task]; t != nil {
				t.Done = true
				h.maybeEndSession(hj)
			}
		}
		c.done = true
		return data, nil
	}
	if c.Task == "" {
		if task, ok := mcpwire.Member(m.Result, "task"); ok && firstByte(task) == '{' {
			id, _ := memberString(task, "taskId")
			if id != "" && hj.Tasks[id] == nil {
				hj.Tasks[id] = &hireTask{Tool: c.Tool, MessageID: c.MessageID}
				if err := h.saveJob(hj); err != nil {
					return nil, err
				}
			}
			c.done = true
			return data, nil
		}
		c.done = true
		return h.openResult(j, hj, data, m, c.Tool, c.MessageID, "")
	}
	out, err := h.openResult(j, hj, data, m, c.Tool, c.MessageID, c.Task)
	if t := hj.Tasks[c.Task]; t != nil {
		t.Done = true
	}
	c.done = true
	h.maybeEndSession(hj)
	return out, err
}

// initResult records the sealing an initialize result declares (capabilities.experimental["sh.kontra/sealed"]) for
// the session the gate opened, after the pin check.
func (h *Hiring) initResult(j *Job, data []byte, m *mcpwire.Message) ([]byte, error) {
	if m.Result == nil || j.sid == "" {
		return data, nil
	}
	var ext json.RawMessage
	if caps, ok := mcpwire.Member(m.Result, "capabilities"); ok {
		if exp, ok := mcpwire.Member(caps, "experimental"); ok {
			if v, ok := mcpwire.Member(exp, mcpwire.MetaSealed); ok {
				ext = v
			}
		}
	}
	si, err := h.recordInfo(j.sid, pinName(j.service), ext, nil, false)
	if err != nil {
		var le *LocalError
		msg := "kontra connect refused the service's sealing (pins)"
		if errors.As(err, &le) {
			msg = le.Error()
		}
		b, _ := mcpwire.ErrorBody(m.ID, mcpwire.CodeInternal, tunnel.Printable(msg), nil)
		return b, nil
	}
	si.session = j.session
	si.idle = sessionIdleOf(m.Result)
	h.mu.Lock()
	h.services[j.sid] = si
	h.mu.Unlock()
	return data, nil
}

// openResult opens a sealed CallToolResult (rule 4): dir res, the session, tool and messageId of the call, the
// gate task for a task's result, the node's key id, auth; an unsealed result is refused (4a).
func (h *Hiring) openResult(j *Job, hj *hireJob, data []byte, m *mcpwire.Message, tool, msgID, task string) ([]byte, error) {
	ct, claims, err := sealed.ParseMCPResult(m.Result)
	if !claims || err != nil || hj == nil {
		return h.mcpFail(m)
	}
	hd, plain, err := sealed.Open([]age.Identity{hj.key}, ct)
	if err != nil {
		return h.mcpFail(m)
	}
	rk, _ := sealed.DecodeRK(hj.RK)
	own := tunnel.KeyID(hj.key.Recipient().String())
	switch {
	case hd.Dir != sealed.DirReply, !slices.Contains(hd.Kids, own), sealed.VerifyAuth(rk, hd, plain) != nil,
		hd.Session != j.session, hd.Tool != tool, hd.MessageID != msgID, hd.TaskID != task, hd.Kind != sealed.KindData:
		return h.mcpFail(m)
	}
	inner, err := strictjson.Members(plain)
	if err != nil {
		return h.mcpFail(m)
	}
	res, err := strictjson.Members(m.Result)
	if err != nil {
		return h.mcpFail(m)
	}
	delete(res, "content")
	delete(res, "structuredContent")
	for k, v := range inner {
		if k == "content" || k == "structuredContent" {
			res[k] = v
		}
	}
	return mcpwire.SetMember(data, "result", a2awire.Encode(res))
}

// openInput opens a sealed input request (D175): of a task a sealed call of this session created (its related task),
// or, without a related task, of the exchange's own sealed call (a plain call's request on its stream); the job's
// session, the task's or call's tool, a messageId new in that task or call; the gate request id that carried it is
// kept for the answer. On a sealed session every other input request is dropped, as an unsealed part is (rule 4a): a
// gate could otherwise ask the local client a question of its own and read the answer in plaintext. A replayed one,
// or one that does not open, is dropped too.
func (h *Hiring) openInput(j *Job, hj *hireJob, data []byte, m *mcpwire.Message) ([]byte, error) {
	task, related := relatedTask(m)
	var tool, key string
	switch {
	case hj == nil:
	case related && hj.Tasks[task] != nil:
		tool, key = hj.Tasks[task].Tool, task
	case !related && j.call != nil:
		tool, key = j.call.tool, "call:"+j.call.messageID
	}
	if tool == "" {
		h.log.Warn("sealing: an input request of a sealed session that no sealed call of it made: dropped (rule 4a)")
		return nil, nil
	}
	params, err := strictjson.Members(m.Params)
	if err != nil {
		return nil, nil
	}
	meta, env, ok, err := sealedMeta(params)
	if err != nil || !ok {
		h.log.Warn("sealing: an input request of a sealed call came unsealed: dropped (rule 4a)", "task", task)
		return nil, nil
	}
	ct, err := sealed.ParseMCPEnvelope(env)
	if err != nil {
		return nil, nil
	}
	hd, plain, err := sealed.Open([]age.Identity{hj.key}, ct)
	if err != nil {
		h.log.Warn("sealing: an input request that does not open: dropped", "task", task)
		return nil, nil
	}
	rk, _ := sealed.DecodeRK(hj.RK)
	own := tunnel.KeyID(hj.key.Recipient().String())
	switch {
	case hd.Dir != sealed.DirReply, !slices.Contains(hd.Kids, own), sealed.VerifyAuth(rk, hd, plain) != nil,
		hd.Session != j.session, hd.Tool != tool, hd.Kind != sealed.KindData,
		related && hd.TaskID != "" && hd.TaskID != task, !related && hd.TaskID != "":
		h.log.Warn("sealing: an input request whose header does not match its task or call: dropped", "task", task)
		return nil, nil
	}
	for _, seen := range hj.Inputs {
		if seen.Task == key && seen.MessageID == hd.MessageID {
			h.log.Info("sealing: an input request opened before: dropped (replayed)", "task", task)
			return nil, nil
		}
	}
	np, err := strictjson.Members(plain)
	if err != nil {
		return nil, nil
	}
	delete(meta, mcpwire.MetaSealed)
	if len(meta) > 0 {
		np["_meta"] = a2awire.Encode(meta)
	}
	hj.Inputs[mcpID(m.ID)] = &hireInput{Task: key, Tool: tool, MessageID: hd.MessageID, TaskID: hd.TaskID}
	if err := h.saveJob(hj); err != nil {
		return nil, err
	}
	return mcpwire.SetMember(data, "params", a2awire.Encode(np))
}

// mcpFail is the local client's answer for a result rule 4 refuses: a JSON-RPC error with sh.kontra/failure
// sealed_rejected (relay.md §11.8's shape).
func (h *Hiring) mcpFail(m *mcpwire.Message) ([]byte, error) {
	h.log.Warn("sealing: a result of a sealed call is refused (rule 4)")
	return mcpwire.ErrorBody(m.ID, mcpwire.CodeInternal, "sealed part rejected", map[string]any{"sh.kontra/failure": "sealed_rejected"})
}

// closeSession records that the client closed its MCP session: its key goes when every task it created is done.
func (h *Hiring) closeSession(j *Job) {
	if j.session == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.services, j.sid)
	hj, err := h.loadJob("mcp-" + j.session)
	if err != nil || hj == nil {
		return
	}
	hj.Closed = true
	_ = h.saveJob(hj)
	h.maybeEndSession(hj)
}

// maybeEndSession deletes a closed session's key once every task created in it is done; h.mu is held.
func (h *Hiring) maybeEndSession(hj *hireJob) {
	if !hj.Closed {
		_ = h.saveJob(hj)
		return
	}
	for _, t := range hj.Tasks {
		if !t.Done {
			_ = h.saveJob(hj)
			return
		}
	}
	h.dropJob(hj)
}
