package sealing

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// What a request records once the service accepts it is written down before the request is handed (D254). A
// request's records (a sealed call's job, binding, (session, messageId) and upstream session, an answer's input request
// answered, an A2A first message's job, replyTo and pairs) are data, commitOps: an exchange writes them to the
// service's pending/<id>.json, synced, before its body goes to the service; the service's acceptance (or the exchange's
// end once the body was handed, D227, D233) applies them and removes the file, and the node's answer of its own (a
// refusal, a non-2xx) removes the file without applying them. A file left behind is a request whose exchange the node
// never ended (a crash, a kill, a reboot, an exit racing the answer): the service may have had the body, so NewHosting
// applies it as an accepted one, and the job's binding, its upstream session and its replay sets hold after the restart
// as they would have once the answer came.

// commitOp is one record of an exchange's acceptance.
type commitOp struct {
	Kind    string `json:"kind"`
	Job     string `json:"job"`
	ReplyTo string `json:"reply_to,omitempty"`
	RK      string `json:"rk,omitempty"`
	// Session is a sealed tools/call's header session (the job's); Call its "<session>/<messageId>" (rule 5).
	Session string `json:"session,omitempty"`
	Call    string `json:"call,omitempty"`
	// SID is the upstream session the request went on ("" none).
	SID string `json:"sid,omitempty"`
	// Key is a record's respKey (opMCPRecord), or an input request's key in the job (opMCPAnswered).
	Key string `json:"key,omitempty"`
	// Req is a sealed call's record (opMCPRecord); without it the record is the tasks/result of the job's task Task.
	Req  *sealedRequest `json:"req,omitempty"`
	Task string         `json:"task,omitempty"`
	// Context and Pairs are an A2A message's gate context and (messageId, part) pairs (opA2ASend).
	Context string   `json:"context,omitempty"`
	Pairs   []string `json:"pairs,omitempty"`
}

// The kinds of commitOp.
const (
	opMCPCall     = "mcp_call"     // a sealed tools/call: the job (opened with its binding), its (session, messageId), its upstream session
	opMCPRecord   = "mcp_record"   // a sealed request whose answer is the job's to seal (a sealed call, a bound task's tasks/result)
	opMCPAnswered = "mcp_answered" // an input request of the job answered
	opA2ASend     = "a2a_send"     // a sealed A2A message: the job (opened with its binding), its replyTo, its pairs
)

// pendingFile is what pending/<id>.json holds.
type pendingFile struct {
	V   int        `json:"v"`
	Ops []commitOp `json:"ops"`
}

// applyOps applies an exchange's records, in order, and returns the job they recorded (the last op's). The service's
// lock is held.
func (h *Hosting) applyOps(service string, svc *serviceState, ops []commitOp) (*jobState, error) {
	var (
		job  *jobState
		errs []error
	)
	for _, op := range ops {
		j, err := h.applyOp(service, svc, op)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if j != nil {
			job = j
		}
	}
	return job, errors.Join(errs...)
}

// applyOp applies one record and writes the job and the service's sets. A record applied twice (a pending file left
// by a node that stopped between applying it and removing the file) records nothing more. The service's lock is held.
func (h *Hosting) applyOp(service string, svc *serviceState, op commitOp) (*jobState, error) {
	now := h.cfg.Now()
	j, err := h.jobFor(service, op.Job)
	if err != nil {
		return nil, err
	}
	switch op.Kind {
	case opMCPCall:
		switch {
		case j == nil:
			j = &jobState{ID: op.Job, ReplyTo: op.ReplyTo, RK: op.RK, Created: now, Session: op.Session}
			j.init()
		case bindOf(j.ReplyTo, j.RK) != bindOf(op.ReplyTo, op.RK):
			return nil, fmt.Errorf("sealing: job %s is bound to another replyTo: this call records nothing", op.Job)
		}
		j.LastSeen = now
		svc.put(setMCPCalls, op.Call, now)
		h.indexNode(setMCPCalls, op.Call, service, true)
		if op.SID != "" && svc.Sessions[op.SID] != j.ID {
			svc.put(setSessions, op.SID, j.ID)
		}
	case opMCPRecord:
		if j == nil {
			return nil, nil
		}
		if j.Calls[op.Key] != nil {
			return j, nil // recorded already
		}
		var r *sealedRequest
		if op.Req != nil {
			c := *op.Req
			r = &c
		} else if t := j.Tasks[op.Task]; t != nil {
			r = &sealedRequest{Kind: reqResult, Tool: t.Tool, MessageID: t.MessageID, Task: op.Task, Gate: t.Gate}
		} else {
			return j, nil
		}
		r.At = now
		j.Calls[op.Key] = r
		if op.SID != "" {
			svc.put(setResponses, op.Key, indexEntry{Job: j.ID, Key: op.Key})
		}
	case opMCPAnswered:
		if j == nil {
			return nil, nil
		}
		if r := j.Inputs[op.Key]; r != nil {
			r.Answered = true
		}
		j.LastSeen = now
		return j, h.saveJob(service, j)
	case opA2ASend:
		switch {
		case j == nil:
			j = &jobState{ID: op.Job, ReplyTo: op.ReplyTo, RK: op.RK, Created: now, GateContext: op.Context}
			j.init()
		case bindOf(j.ReplyTo, j.RK) != bindOf(op.ReplyTo, op.RK):
			// The job recorded meanwhile with another binding is never overwritten.
			return nil, fmt.Errorf("sealing: job %s is bound to another replyTo: this request opens nothing", op.Job)
		}
		for _, p := range op.Pairs {
			if !slices.Contains(j.Pairs, p) {
				j.Pairs = append(j.Pairs, p)
			}
		}
		j.LastSeen = now
		svc.put(setReplyTo, op.ReplyTo, replyEntry{Job: j.ID, At: now})
		h.indexNode(setReplyTo, op.ReplyTo, service, true)
	default:
		return nil, fmt.Errorf("sealing: a pending record of no known kind %q", op.Kind)
	}
	if err := h.saveJob(service, j); err != nil {
		return nil, err
	}
	return j, h.st.saveService(service, svc)
}

// writePending writes ops to a new pending file of the service, synced, and returns its id. The service's lock is
// held.
func (h *Hosting) writePending(service string, ops []commitOp) (string, error) {
	dir, err := h.st.pendingDir(service)
	if err != nil {
		return "", err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if err := writeJSON(dir, id+".json", pendingFile{V: stateVersion, Ops: ops}); err != nil {
		return "", err
	}
	return id, nil
}

// removePending removes a pending file ("" none). The removal is not synced: one a crash lost is applied at the next
// start, which records a request the service may not have had (the gate's retry of it is then a replay): it never
// forgets one it had.
func (h *Hosting) removePending(service, id string) {
	if id == "" {
		return
	}
	dir, err := h.st.pendingDir(service)
	if err != nil {
		return
	}
	if err := os.Remove(filepath.Join(dir, id+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.log.Warn("sealing: a pending record was not removed", "service", service, "err", err)
	}
}

// recoverPending applies, as accepted, every pending file the service's exchanges left (the node stopped while they
// were open) and removes it. The service's lock is held.
func (h *Hosting) recoverPending(service string) error {
	dir, err := h.st.pendingDir(service)
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	svc, err := h.service(service)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !hexKey(name) {
			continue
		}
		var pf pendingFile
		if err := readJSON(filepath.Join(dir, e.Name()), &pf); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := h.applyOps(service, svc, pf.Ops); err != nil {
			errs = append(errs, err)
		}
		h.log.Info("sealing: a request the node had in flight when it stopped is recorded as accepted (D254)", "service", service)
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pendingDir is the directory of a service's pending files.
func (s *fileStore) pendingDir(service string) (string, error) {
	dir, err := s.serviceDir(service)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pending"), nil
}
