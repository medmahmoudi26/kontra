package sealing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The hosting node's durable job state (spec/ext-sealed.md §5 "Hosting node"): under
// ~/.kontra/hosted/<gate host>/<service>/, 0600 files in 0700 directories, node-local clocks only.
//
//   - service.json: the per-service sets, kept for at least --sealed-max-age (and the 300 s of clock skew rule 1
//     allows): every replyTo that opened a job, with that job's gate task id; the upstream context id recorded for
//     each gate context; for MCP every (session, messageId) forwarded. It also indexes the upstream task and context
//     ids of the service's sealed jobs, so that an exchange of another job (or of none) never reaches them.
//   - jobs/<job>.json: one job, kept until 7 days after its last exchange: the replyTo and rk of its first sealed
//     request, the upstream task and context ids, the (messageId, part) pairs forwarded, per artifactId what it
//     sealed (chunks), and for MCP the tasks the session's sealed calls created with their input requests.
//   - jobs/<job>.ct/<sha256>: a ciphertext the node sealed for the job, by the hash of its header (without auth) and
//     body, reused for the same header and body (so polling sees identical bytes).
//
// A file is written whole to a temporary file in its directory, synced and renamed into place.

const stateVersion = 1

// serviceState is service.json.
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
}

type replyEntry struct {
	Job string    `json:"job"`
	At  time.Time `json:"at"`
}

type contextEntry struct {
	Upstream string    `json:"upstream"`
	At       time.Time `json:"at"`
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
	// task id.
	Session string              `json:"session,omitempty"`
	Tasks   map[string]*mcpTask `json:"tasks,omitempty"`
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
	Gate      string                   `json:"gate,omitempty"` // the gate task (durable); empty for passthrough
	Tool      string                   `json:"tool"`
	MessageID string                   `json:"message_id"` // the creating call's
	Inputs    map[string]*InputRequest `json:"inputs,omitempty"`
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
}

func (s *fileStore) saveService(service string, st *serviceState) error {
	dir, err := s.serviceDir(service)
	if err != nil {
		return err
	}
	return writeJSON(dir, "service.json", st)
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
	if j.Artifacts == nil {
		j.Artifacts = map[string]*artifactState{}
	}
	if j.Tasks == nil {
		j.Tasks = map[string]*mcpTask{}
	}
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
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("sealing: %s: %w", path, err)
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
// then renamed.
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
