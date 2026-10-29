package sealing

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kontra.sh/kontra/internal/mcpwire"
)

// The hosting node's durable job state (spec/ext-sealed.md §5 "Hosting node"): under
// ~/.kontra/hosted/<gate host>/<service>/, 0600 files in 0700 directories, node-local clocks only.
//
//   - service.json and service.log: the per-service sets, kept for at least --sealed-max-age (and the 300 s of clock
//     skew rule 1 allows): every replyTo that opened a job, with that job's gate task id; the upstream context id
//     recorded for each gate context; for MCP every (session, messageId) forwarded. They also index the upstream task
//     and context ids of the service's sealed jobs, so that an exchange of another job (or of none) never reaches
//     them, and the MCP requests the node sealed (a sealed call awaiting its result, an input request awaiting its
//     answer) by the upstream session and JSON-RPC id they travel on, so that their answers are sealed, or refused,
//     on whichever exchange of that upstream session they come. service.json is a snapshot; every change after it is
//     one line appended to service.log (synced), and Sweep (or a log grown past compactAfter) writes a new snapshot
//     and starts the log again, so that a commit costs one append whatever the sets hold.
//   - jobs/<job>.json: one job, kept until 7 days after its last exchange: the replyTo and rk of its first sealed
//     request, the upstream task and context ids, the (messageId, part) pairs forwarded, per artifactId what it
//     sealed (chunks), and for MCP the tasks the session's sealed calls created, the sealed requests awaiting an
//     answer and the input requests sealed.
//   - jobs/<job>.ct/<sha256>: a ciphertext the node sealed for the job, by the hash of its header (without auth) and
//     body, reused for the same header and body (so polling sees identical bytes).
//   - pending/<id>.json: what a request in flight records once the service accepts it, written before the request is
//     handed and applied as accepted when the node starts with it still there (pending.go, D254).
//
// A file is written whole to a temporary file in its directory, synced, renamed into place, and the directory synced
// (a rename the crash lost would forget a replyTo or a (session, messageId), and a replay of it would pass).

const stateVersion = 1

// serviceState is service.json, with service.log's changes applied.
type serviceState struct {
	V int `json:"v"`
	// ReplyTo maps every replyTo that opened a job to that job (rule 2's replay set).
	ReplyTo map[string]replyEntry `json:"reply_to"`
	// Contexts maps a gate context to the upstream context id the service gave a sealed job in it (rule 2).
	Contexts map[string]contextEntry `json:"contexts"`
	// MCPCalls are the (session, messageId) pairs of sealed tools/calls forwarded (rule 5), "<session>/<id>".
	MCPCalls map[string]time.Time `json:"mcp_calls"`
	// Tasks and UpstreamContexts index the upstream task and context ids of sealed jobs: an exchange of another
	// job, or of none, never reaches them (wrong_task, wrong_context), and a reply naming one is refused.
	Tasks            map[string]string `json:"tasks"`             // upstream task id -> job
	UpstreamContexts map[string]string `json:"upstream_contexts"` // upstream context id -> gate context
	// Responses indexes the MCP requests a job sealed (a sealed tools/call, a bound task's tasks/result) by the
	// upstream session and JSON-RPC id they went on (respKey): their answer is the job's to seal on any exchange of
	// that upstream session, and refused on an exchange of another job (spec §5, "seals every part of every reply").
	Responses map[string]indexEntry `json:"responses,omitempty"`
	// Inputs indexes the MCP input requests a job sealed by the upstream session and the server's JSON-RPC id
	// (respKey): an answer to one must be sealed and pass rule 4b against that job and task, whatever the preamble of
	// the exchange that carries it says (D175, "answers must be sealed").
	Inputs map[string]indexEntry `json:"inputs,omitempty"`
	// Sessions indexes the upstream sessions (the MCP server's Mcp-Session-Id) the sealed calls of a job went on, to
	// that job: only an exchange of that job (its preamble's mcp_session) reaches one (D217, D226), since the
	// upstream session the node forwards on is the request's header, which the gate chooses as it does the preamble.
	Sessions map[string]string `json:"sessions,omitempty"`
	// Held are the upstream sessions of sealed jobs that were swept, to the job each was: the server keeps what the
	// job's sealed calls set up there until the session ends, so they stay as guarded as Sessions until a DELETE of
	// one is answered (D255).
	Held map[string]string `json:"held,omitempty"`

	// pending are the changes not yet appended to service.log (saveService appends them).
	pending []svcOp
	// logged counts the lines service.log holds.
	logged int
}

type replyEntry struct {
	Job string    `json:"job"`
	At  time.Time `json:"at"`
}

type contextEntry struct {
	Upstream string    `json:"upstream"`
	At       time.Time `json:"at"`
}

// indexEntry names a job's entry: the job and its key in the job's map.
type indexEntry struct {
	Job string `json:"job"`
	Key string `json:"key"`
	// At is when an input request the node did not relay was recorded (Inputs with no Key, D232): Sweep drops it once
	// it is older than --sealed-max-age.
	At time.Time `json:"at,omitzero"`
}

// The sets of serviceState, as service.log names them.
const (
	setReplyTo          = "reply_to"
	setContexts         = "contexts"
	setMCPCalls         = "mcp_calls"
	setTasks            = "tasks"
	setUpstreamContexts = "upstream_contexts"
	setResponses        = "responses"
	setInputs           = "inputs"
	setSessions         = "sessions"
	setHeld             = "held"
)

// svcOp is one line of service.log: a member of a set put (V) or deleted (Del).
type svcOp struct {
	Set string          `json:"s"`
	Key string          `json:"k"`
	V   json.RawMessage `json:"v,omitempty"`
	Del bool            `json:"d,omitempty"`
}

// put sets a member of a set and records the change for service.log.
func (st *serviceState) put(set, key string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return // the sets hold strings, times and small structs only
	}
	op := svcOp{Set: set, Key: key, V: b}
	_ = st.apply(op)
	st.pending = append(st.pending, op)
}

// del deletes a member of a set and records the change for service.log.
func (st *serviceState) del(set, key string) {
	op := svcOp{Set: set, Key: key, Del: true}
	_ = st.apply(op)
	st.pending = append(st.pending, op)
}

// apply applies one change to the sets.
func (st *serviceState) apply(op svcOp) error {
	switch op.Set {
	case setReplyTo:
		return applyOp(st.ReplyTo, op)
	case setContexts:
		return applyOp(st.Contexts, op)
	case setMCPCalls:
		return applyOp(st.MCPCalls, op)
	case setTasks:
		return applyOp(st.Tasks, op)
	case setUpstreamContexts:
		return applyOp(st.UpstreamContexts, op)
	case setResponses:
		return applyOp(st.Responses, op)
	case setInputs:
		return applyOp(st.Inputs, op)
	case setSessions:
		return applyOp(st.Sessions, op)
	case setHeld:
		return applyOp(st.Held, op)
	}
	return fmt.Errorf("sealing: service.log names an unknown set %q", op.Set)
}

func applyOp[V any](m map[string]V, op svcOp) error {
	if op.Del {
		delete(m, op.Key)
		return nil
	}
	var v V
	if err := json.Unmarshal(op.V, &v); err != nil {
		return err
	}
	m[op.Key] = v
	return nil
}

// respKey is the key of an MCP request or answer on an upstream session: the upstream's Mcp-Session-Id and the
// JSON-RPC id (mcpwire.IDKey, so that 1 and 1.0 are one id).
func respKey(upstreamSID string, id json.RawMessage) string {
	k, ok := mcpwire.IDKey(id)
	if !ok {
		k = "r:" + string(bytes.TrimSpace(id))
	}
	return upstreamSID + "\x00" + k
}

// answerKeys are the respKeys of every request a message whose id is id answers as a client matches it
// (mcpwire.AnswerKeys: "8" answers request 8 for the TypeScript SDK), its own first: a response is sealed, opened or
// refused as the answer to any of them, never relayed as another's.
func answerKeys(upstreamSID string, id json.RawMessage) []string {
	ks := mcpwire.AnswerKeys(id)
	if len(ks) == 0 {
		return []string{respKey(upstreamSID, id)}
	}
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = upstreamSID + "\x00" + k
	}
	return out
}

// jobState is one job's file.
type jobState struct {
	V        int       `json:"v"`
	ID       string    `json:"id"`
	ReplyTo  string    `json:"reply_to"`
	RK       string    `json:"rk"` // base64url, the header's form
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	// A2A: the gate's ids (the job's id is the gate task), the upstream ids the service gave the task, the pairs
	// forwarded, and what was sealed per artifactId.
	GateContext     string                    `json:"gate_context,omitempty"`
	UpstreamTask    string                    `json:"upstream_task,omitempty"`
	UpstreamContext string                    `json:"upstream_context,omitempty"`
	Pairs           []string                  `json:"pairs,omitempty"` // "<messageId>\x00<part>"
	Artifacts       map[string]*artifactState `json:"artifacts,omitempty"`
	// MCP: the session (its hash, the preamble's mcp_session) and the tasks its sealed calls created, by upstream
	// task id; the sealed requests whose answer the node seals (Calls), and the input requests it sealed (Inputs),
	// by respKey (and for Inputs the preamble's task the answer must come on).
	Session string                    `json:"session,omitempty"`
	Tasks   map[string]*mcpTask       `json:"tasks,omitempty"`
	Calls   map[string]*sealedRequest `json:"calls,omitempty"`
	Inputs  map[string]*InputRequest  `json:"inputs,omitempty"`
}

// callOf is the job's record under key, nil for a nil job or none.
func (j *jobState) callOf(key string) *sealedRequest {
	if j == nil {
		return nil
	}
	return j.Calls[key]
}

// init makes the job's maps.
func (j *jobState) init() {
	if j.Artifacts == nil {
		j.Artifacts = map[string]*artifactState{}
	}
	if j.Tasks == nil {
		j.Tasks = map[string]*mcpTask{}
	}
	if j.Calls == nil {
		j.Calls = map[string]*sealedRequest{}
	}
	if j.Inputs == nil {
		j.Inputs = map[string]*InputRequest{}
	}
}

// artifactState is what the node sealed for one artifactId: Next is the next chunk number, and Held the parts the
// gate holds for it in order (a replacement, append false or a snapshot that changed, starts it again).
type artifactState struct {
	Next int        `json:"next"`
	Held []heldPart `json:"held"`
}

type heldPart struct {
	Chunk int    `json:"chunk"`
	Part  int    `json:"part"`
	Parts int    `json:"parts"`
	Plain string `json:"plain"` // sha256 of the plaintext part's kind, mediaType and body
	CT    string `json:"ct"`    // the ciphertext's cache key
}

// mcpTask is a task a sealed tools/call created (rule 4: its upstream taskId is bound to the job).
type mcpTask struct {
	Gate      string `json:"gate,omitempty"` // the gate task (durable); empty for passthrough
	Tool      string `json:"tool"`
	MessageID string `json:"message_id"` // the creating call's
}

// sealedRequest is an MCP request of a job whose answer the node seals: a sealed tools/call (its result, sealed to
// the call's own replyTo and rk), or a bound task's tasks/result (sealed to the job's).
type sealedRequest struct {
	Kind      string    `json:"kind"` // reqCall or reqResult
	Tool      string    `json:"tool"`
	MessageID string    `json:"message_id"`     // the header's messageId: the call's client id
	Task      string    `json:"task,omitempty"` // reqResult: the upstream task
	Gate      string    `json:"gate,omitempty"` // the gate task: of the call's exchange (a durable task's), or the bound task's
	ReplyTo   string    `json:"reply_to,omitempty"`
	RK        string    `json:"rk,omitempty"`
	At        time.Time `json:"at"`
	Answered  bool      `json:"answered,omitempty"`
}

const (
	reqCall   = "call"
	reqResult = "result"
)

// InputRequest is an MCP input request this node sealed (D175): of a task a sealed call created, or of a sealed plain
// call (on that call's own stream).
type InputRequest struct {
	// Gate is the gate task of a durable task (its header's taskId, and the preamble's task its answer must come on);
	// "" for a passthrough task's or a plain call's, whose answers come on the session's own exchanges.
	Gate      string    `json:"gate,omitempty"`
	UpTask    string    `json:"up_task,omitempty"` // the upstream task ("" for a plain call's)
	Tool      string    `json:"tool"`
	MessageID string    `json:"message_id"` // the upstream server's JSON-RPC id, as a string
	SealedAt  time.Time `json:"sealed_at"`  // node clock; the answer must come within MaxAge
	Answered  bool      `json:"answered,omitempty"`
}

// fileStore reads and writes the state of one gate host's hosted services.
type fileStore struct {
	dir string // ~/.kontra/hosted/<gate host>
}

// errNoJob is a job that has no file.
var errNoJob = errors.New("sealing: no such job")

func (s *fileStore) serviceDir(service string) (string, error) {
	if err := element("service", service); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, service), nil
}

// compactAfter is how many lines service.log may hold before a commit writes a new snapshot.
const compactAfter = 4096

func (s *fileStore) loadService(service string) (*serviceState, error) {
	dir, err := s.serviceDir(service)
	if err != nil {
		return nil, err
	}
	st := &serviceState{}
	if err := readJSON(filepath.Join(dir, "service.json"), st); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	st.init()
	// The changes after the snapshot, in order. A last line a crash or a failed write cut short (no newline, or not one
	// change) is no change, and is cut off the file here, synced: the next change appended after it would otherwise
	// share its line, and be lost with every change after it at the next start (round 2 sealing-fixes-7).
	p := filepath.Join(dir, "service.log")
	if err := checkPrivate(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return st, nil
		}
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("sealing: %w", err)
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 64<<10)
	var good int64 // the offset after the last whole change
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("sealing: %s: %w", p, err)
		}
		if len(line) == 0 {
			break
		}
		var op svcOp
		if line[len(line)-1] != '\n' || json.Unmarshal(line, &op) != nil {
			if err := f.Truncate(good); err != nil {
				return nil, fmt.Errorf("sealing: %s: cutting a torn change: %w", p, err)
			}
			if err := f.Sync(); err != nil {
				return nil, fmt.Errorf("sealing: %s: %w", p, err)
			}
			break
		}
		if err := st.apply(op); err != nil {
			return nil, fmt.Errorf("sealing: %s: %w", p, err)
		}
		st.logged++
		good += int64(len(line))
	}
	return st, nil
}

func (st *serviceState) init() {
	st.V = stateVersion
	if st.ReplyTo == nil {
		st.ReplyTo = map[string]replyEntry{}
	}
	if st.Contexts == nil {
		st.Contexts = map[string]contextEntry{}
	}
	if st.MCPCalls == nil {
		st.MCPCalls = map[string]time.Time{}
	}
	if st.Tasks == nil {
		st.Tasks = map[string]string{}
	}
	if st.UpstreamContexts == nil {
		st.UpstreamContexts = map[string]string{}
	}
	if st.Responses == nil {
		st.Responses = map[string]indexEntry{}
	}
	if st.Inputs == nil {
		st.Inputs = map[string]indexEntry{}
	}
	if st.Sessions == nil {
		st.Sessions = map[string]string{}
	}
	if st.Held == nil {
		st.Held = map[string]string{}
	}
}

// saveService appends the service's pending changes to service.log, synced (one append, whatever the sets hold);
// a log past compactAfter lines is compacted into a new snapshot.
func (s *fileStore) saveService(service string, st *serviceState) error {
	if len(st.pending) == 0 {
		return nil
	}
	if st.logged+len(st.pending) > compactAfter {
		return s.compactService(service, st)
	}
	dir, err := s.serviceDir(service)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, op := range st.pending {
		b, err := json.Marshal(op)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := appendFile(dir, "service.log", buf.Bytes()); err != nil {
		return err
	}
	st.logged += len(st.pending)
	st.pending = nil
	return nil
}

// compactService writes the service's sets as a new snapshot and starts service.log again.
func (s *fileStore) compactService(service string, st *serviceState) error {
	dir, err := s.serviceDir(service)
	if err != nil {
		return err
	}
	if err := writeJSON(dir, "service.json", st); err != nil {
		return err
	}
	// A crash here replays the old log over the new snapshot: every change in it is already there, and applying one
	// again changes nothing.
	if err := os.Remove(filepath.Join(dir, "service.log")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sealing: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	st.pending, st.logged = nil, 0
	return nil
}

func (s *fileStore) jobPath(service, job string) (string, error) {
	dir, err := s.serviceDir(service)
	if err != nil {
		return "", err
	}
	if err := element("job", job); err != nil {
		return "", err
	}
	return filepath.Join(dir, "jobs", job+".json"), nil
}

func (s *fileStore) loadJob(service, job string) (*jobState, error) {
	p, err := s.jobPath(service, job)
	if err != nil {
		return nil, err
	}
	j := &jobState{}
	if err := readJSON(p, j); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNoJob
		}
		return nil, err
	}
	j.init()
	return j, nil
}

func (s *fileStore) saveJob(service string, j *jobState) error {
	p, err := s.jobPath(service, j.ID)
	if err != nil {
		return err
	}
	j.V = stateVersion
	return writeJSON(filepath.Dir(p), filepath.Base(p), j)
}

// ciphertext reads a cached ciphertext of a job.
func (s *fileStore) ciphertext(service, job, key string) ([]byte, bool) {
	p, err := s.jobPath(service, job)
	if err != nil || !hexKey(key) {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSuffix(p, ".json")+".ct", key))
	if err != nil {
		return nil, false
	}
	return b, true
}

// keepCiphertext caches a ciphertext of a job under key (written once; an existing one is kept).
func (s *fileStore) keepCiphertext(service, job, key string, ct []byte) error {
	p, err := s.jobPath(service, job)
	if err != nil {
		return err
	}
	if !hexKey(key) {
		return errors.New("sealing: a ciphertext key that is not a hash")
	}
	dir := strings.TrimSuffix(p, ".json") + ".ct"
	if _, err := os.Stat(filepath.Join(dir, key)); err == nil {
		return nil
	}
	return writeFile(dir, key, ct)
}

// removeJob deletes a job's file and its ciphertexts.
func (s *fileStore) removeJob(service, job string) error {
	p, err := s.jobPath(service, job)
	if err != nil {
		return err
	}
	errs := []error{os.RemoveAll(strings.TrimSuffix(p, ".json") + ".ct")}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// services lists the hosted services that have state.
func (s *fileStore) services() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && element("service", e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// jobs lists the jobs of a service that have a file.
func (s *fileStore) jobs(service string) ([]string, error) {
	dir, err := s.serviceDir(service)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(dir, "jobs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() && element("job", name) == nil {
			out = append(out, name)
		}
	}
	return out, nil
}

// element refuses a name that is not one path element of the characters a service name, a gate task id or a job
// id is made of.
func element(what, s string) error {
	if s == "" || len(s) > 200 || s == "." || s == ".." || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "-") {
		return fmt.Errorf("sealing: %s %q is not a name", what, s)
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return fmt.Errorf("sealing: %s %q is not a name", what, s)
		}
	}
	return nil
}

func hexKey(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// cacheKey is the ciphertext cache key of a header (without auth) and body.
func cacheKey(hdrJCS, body []byte) string {
	h := sha256.New()
	h.Write(hdrJCS)
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func readJSON(path string, v any) error {
	if err := checkPrivate(path); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("sealing: %s: %w", path, err)
	}
	return nil
}

// checkPrivate refuses a state file that is not a regular file, or is open to group or others.
func checkPrivate(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("sealing: %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("sealing: %s has mode %04o: the job state must not be open to group or others (chmod 600 %s)",
			path, fi.Mode().Perm(), path)
	}
	return nil
}

func writeJSON(dir, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFile(dir, name, b)
}

// writeFile puts data at dir/name atomically: a 0600 temporary file in dir (created 0700 with its parents), synced,
// renamed, and the directory synced, so that the rename survives a crash.
func writeFile(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sealing: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("sealing: %w", err)
	}
	tname := tmp.Name()
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tname, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tname)
		return fmt.Errorf("sealing: %w", err)
	}
	return syncDir(dir)
}

// appendFile appends data to dir/name (created 0600, and the directory synced when it is new), synced.
func appendFile(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sealing: %w", err)
	}
	p := filepath.Join(dir, name)
	_, statErr := os.Lstat(p)
	if statErr == nil {
		if err := checkPrivate(p); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("sealing: %w", err)
	}
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	_, err = appendWrite(f, data)
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		// A write cut short (ENOSPC, say) leaves no part of it behind: the next append starts on a line of its own.
		if terr := f.Truncate(size); terr == nil {
			_ = f.Sync()
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sealing: %w", err)
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		return syncDir(dir)
	}
	return nil
}

// appendWrite writes an append (tests replace it, to cut one short).
var appendWrite = func(f *os.File, data []byte) (int, error) { return f.Write(data) }

// syncDirHook, when set (tests), sees every directory syncDir syncs.
var syncDirHook func(dir string)

// syncDir syncs a directory, so that a rename or a new file in it survives a crash.
func syncDir(dir string) error {
	if syncDirHook != nil {
		syncDirHook(dir)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sealing: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sealing: sync %s: %w", dir, err)
	}
	return nil
}

// privateDir creates dir (and its parents) 0700 when missing and refuses one open to group or others.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sealing: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("sealing: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("sealing: %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("sealing: %s has mode %04o: the job state's directory must not be open to group or others "+
			"(chmod 700 %s)", dir, fi.Mode().Perm(), dir)
	}
	return nil
}
