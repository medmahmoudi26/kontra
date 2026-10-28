package sealing

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// MCP sealing on the hosting node (spec/ext-sealed.md §2, §3, §5 rules 1, 4, 4b and 5; D62, D162, D175). A sealed
// tools/call carries params.arguments = {} and params._meta["sh.kontra/sealed"] = {"v": 1, "envelope"}; the node
// opens it, checks it and forwards the plaintext arguments. The job is the client's gate session (the preamble's
// mcp_session, the session hash): its plain calls, and the tasks they create, whose upstream taskId is bound to it.
// SPEC-CONFLICT (D218; ext-sealed.md §5 "Hosting node"): §5 keys a job "by the gate task id; for plain MCP calls, by
// the session hash", while rule 4 holds one replyTo and rk per session and the hiring node keeps one per-job key per
// MCP session, its plain calls and the tasks created in it (§4). The closest faithful behaviour: a durable task is
// kept inside its session's job, with its gate task id and its input requests, so both keys find it.
// The call's CallToolResult, and a bound task's tasks/result, are sealed into one kontra:sealed resource; a bound
// task's statusMessage becomes "sealed" wherever the node reads one (a CreateTaskResult's task, tasks/get,
// tasks/cancel, tasks/list, notifications/tasks/status); its upstream elicitation/create and
// sampling/createMessage are sealed with messageId the upstream's own JSON-RPC id (an InputRequest of the job),
// and the answer to one must come back sealed and pass rule 4b.
//
// Order on a hosted MCP service (plan corrections WP-08): from the client, strict decoding, then the roots filter
// (internal/mcproots), then FromClient; from the server, the roots filter, then FromServer.

// MCPExchange is one MCP exchange's sealing state on the hosting node: a POST (its message, and what the server
// answers on its response), or a GET stream.
type MCPExchange struct {
	h       *Hosting
	pre     tunnel.Preamble
	service string
	jobID   string // "mcp-" + the session hash; "" on an exchange with no session (2026-07-28)
	// upstreamSID is the upstream session the exchange is on (the request's Mcp-Session-Id): the server's answers to
	// a sealed request, and the answers to its input requests, are the job's that sealed them on any exchange of
	// that upstream session, whatever the exchange's preamble says (svc.Responses, svc.Inputs).
	upstreamSID string

	reqMethod string
	reqID     json.RawMessage
	call      *sealedCall // the exchange's sealed tools/call
	taskOf    string      // the upstream task id a tasks/* request names
	commit    []func() error
	reserved  []string // what the commits record, in flight until then (hostedService.flight, Hosting.nflight)
}

// sealedCall is a sealed tools/call of the exchange: its tool and messageId, and the replyTo and rk its request
// carried, which its result is sealed to.
type sealedCall struct {
	tool, messageID string
	replyTo, rk     string
}

// MCP returns the sealing state of one MCP exchange of the service the preamble names, on no upstream session (an
// initialize, a stateless server); MCPOn names the upstream session.
func (h *Hosting) MCP(pre tunnel.Preamble) *MCPExchange { return h.MCPOn(pre, "") }

// MCPOn returns the sealing state of one MCP exchange on the upstream session upstreamSID (the request's
// Mcp-Session-Id: the node's server's own session id, for a URL server or the stdio bridge).
func (h *Hosting) MCPOn(pre tunnel.Preamble, upstreamSID string) *MCPExchange {
	x := &MCPExchange{h: h, pre: pre, service: pre.Service, upstreamSID: upstreamSID}
	if s := pre.MCPSession; len(s) == 64 && isHex(s) {
		x.jobID = "mcp-" + s
	}
	return x
}

// MCPJobID is the job id of a gate session hash (hex), as the hosting node keeps its state.
func MCPJobID(session string) string { return "mcp-" + session }

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// FromClient opens the client's message when it is a sealed tools/call or a sealed answer (rules 1, 4, 4b, 5), and
// returns the message to forward; a refusal is a *sealed.RejectError (the node answers it itself, §6), and nothing
// of the message reaches the server.
func (x *MCPExchange) FromClient(body []byte, m *mcpwire.Message) ([]byte, error) {
	h := x.h
	hs := h.lock(x.service)
	defer hs.mu.Unlock()
	x.reqMethod, x.reqID = m.Method, m.ID
	svc, err := h.service(x.service)
	if err != nil {
		return nil, err
	}
	job, err := h.jobFor(x.service, x.jobID)
	if err != nil {
		return nil, err
	}
	if owner, ok := x.sessionOwner(svc); ok && owner != x.jobID {
		// The upstream session the node forwards on is the request's Mcp-Session-Id, which the gate chooses as it does
		// the preamble: a message on the upstream session a sealed job's calls set up (a login, an opened document) is
		// that job's only (D217, D226). Another session's, or one with no session, would have its answer reach the gate
		// in plaintext, or sealed to the gate's own key.
		if m.Method == mcpwire.MethodToolsCall && hasSealedMeta(m.Params) {
			return nil, reject(sealed.RejectWrongJob, "a sealed tools/call on the upstream session of another session's sealed job")
		}
		return nil, reject(sealed.RejectBadHeader, "a message on the upstream session of another session's sealed job")
	}
	if m.Method != "" && len(m.ID) > 0 && x.recorded(svc, job, m.ID) {
		// A request that reuses the id of a sealed request the node recorded on the session: MCP never reuses a request
		// id in a session, and the record stays the sealed request's, so that its answer, on whichever exchange of the
		// session it comes (a resumed GET included), is sealed or refused, never relayed as this request's (D226).
		return nil, reject(sealed.RejectReplayed, "a request that reuses the JSON-RPC id of a sealed request on the session")
	}
	switch {
	case m.Method == mcpwire.MethodToolsCall:
		return x.toolsCall(svc, job, body, m)
	case m.Method == "":
		return x.answer(svc, job, body, m)
	case strings.HasPrefix(m.Method, "tasks/"):
		id, _ := memberString(m.Params, "taskId")
		if owner, ok := svc.Tasks[id]; ok && id != "" && owner != x.jobID {
			return nil, reject(sealed.RejectWrongTask, "a tasks/* request names a sealed task of another session")
		}
		x.taskOf = id
		if m.Method == "tasks/result" && job != nil && job.Tasks[id] != nil && len(m.ID) > 0 {
			// The result of a bound task is the job's to seal wherever the server sends it (a resumed stream included).
			x.record(svc, respKey(x.upstreamSID, m.ID), func(j *jobState) *sealedRequest {
				t := j.Tasks[id]
				if t == nil {
					return nil
				}
				return &sealedRequest{Kind: reqResult, Tool: t.Tool, MessageID: t.MessageID, Task: id, Gate: t.Gate}
			})
		}
	}
	return body, nil
}

// sessionOwner is the sealed job that holds the exchange's upstream session: the session's index entry, or the job a
// sealed call in flight on it is opening it for; ok false on an exchange with no upstream session, or on one no
// sealed job holds. the service's lock is held.
func (x *MCPExchange) sessionOwner(svc *serviceState) (string, bool) {
	if x.upstreamSID == "" {
		return "", false
	}
	if j, ok := svc.Sessions[x.upstreamSID]; ok {
		return j, true
	}
	return x.h.inFlight(flightKey("s", x.service, x.upstreamSID))
}

// recorded reports a request id the node recorded a sealed request under on the exchange's upstream session (the
// job's record, or, on an upstream session, the service's index of it). the service's lock is held.
func (x *MCPExchange) recorded(svc *serviceState, job *jobState, id json.RawMessage) bool {
	key := respKey(x.upstreamSID, id)
	if job != nil && job.Calls[key] != nil {
		return true
	}
	if x.upstreamSID == "" {
		return false
	}
	_, ok := svc.Responses[key]
	return ok
}

// hasSealedMeta reports params with a _meta["sh.kontra/sealed"] (exact keys).
func hasSealedMeta(params json.RawMessage) bool {
	meta, ok := mcpwire.Member(params, "_meta")
	if !ok {
		return false
	}
	_, ok = mcpwire.Member(meta, mcpwire.MetaSealed)
	return ok
}

// record arranges for the exchange's commit to record a sealed request of the session's job under key (the upstream
// session and the request's id), and to index it for the service when the exchange is on an upstream session.
func (x *MCPExchange) record(svc *serviceState, key string, req func(j *jobState) *sealedRequest) {
	service, jobID, sid := x.service, x.jobID, x.upstreamSID
	x.commit = append(x.commit, func() error {
		j, err := x.h.jobFor(service, jobID)
		if err != nil || j == nil {
			return err
		}
		r := req(j)
		if r == nil {
			return nil
		}
		r.At = x.h.cfg.Now()
		j.Calls[key] = r
		if sid != "" {
			svc.put(setResponses, key, indexEntry{Job: j.ID, Key: key})
		}
		if err := x.h.saveJob(service, j); err != nil {
			return err
		}
		return x.h.st.saveService(service, svc)
	})
}

// toolsCall checks and opens a sealed tools/call (rules 1, 4, 5); an unsealed one passes unless the service was
// started with --sealed-required, or the session has a sealed job.
func (x *MCPExchange) toolsCall(svc *serviceState, job *jobState, body []byte, m *mcpwire.Message) ([]byte, error) {
	params, err := strictjson.Members(m.Params)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, "a tools/call without params")
	}
	meta, env, hasEnv, err := sealedMeta(params)
	if err != nil {
		return nil, err
	}
	jobKey := flightKey("j", x.service, x.jobID)
	if !hasEnv {
		if x.h.Required(x.service) {
			return nil, reject(sealed.RejectBadHeader, "an unsealed tools/call to a service started with --sealed-required")
		}
		if _, opening := x.h.inFlight(jobKey); job != nil || opening && x.jobID != "" {
			// A sealed session is the hiring node's, which seals every call on it: an unsealed call is the gate's own,
			// made in the upstream session the buyer's sealed calls set up (a login, an opened document), and its result
			// would reach the gate in plaintext (§7, "cannot continue another buyer's job"; security review).
			return nil, reject(sealed.RejectBadHeader, "an unsealed tools/call on a session with a sealed job")
		}
		return body, nil
	}
	ct, err := sealed.ParseMCPEnvelope(env)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, err.Error())
	}
	if args, ok := params["arguments"]; ok && !emptyObject(args) {
		return nil, reject(sealed.RejectBadHeader, "a sealed tools/call whose arguments are not {}")
	}
	hd, plain, err := sealed.Open(x.h.cfg.Identities(), ct)
	if err != nil {
		return nil, err
	}
	if err := x.h.mcpRule1(hd, false); err != nil {
		return nil, err
	}
	if err := x.h.checkService(hd, x.service); err != nil {
		return nil, err
	}
	name, _ := a2awire.String(params, "name")
	idStr := mcpID(m.ID)
	switch {
	case hd.Tool != name || name == "":
		return nil, reject(sealed.RejectWrongMessage, "the header's tool is not params.name")
	case hd.Session != x.pre.MCPSession || x.jobID == "":
		return nil, reject(sealed.RejectWrongMessage, "the header's session is not the preamble's mcp_session")
	case hd.MessageID != x.pre.MCPRequestID || hd.MessageID != idStr:
		return nil, reject(sealed.RejectWrongMessage, "the header's messageId is not the preamble's mcp_request_id")
	}
	bind := bindOf(hd.ReplyTo, hd.RK)
	if job != nil && bindOf(job.ReplyTo, job.RK) != bind {
		return nil, reject(sealed.RejectWrongJob, "the session's job was opened with another replyTo")
	}
	// Rule 4's binding holds from the node's checks on: a call of another replyTo while the session's first sealed
	// call is in flight is refused as it would be once that call is recorded (otherwise whichever the server accepted
	// first would bind the job, and the other's result would be sealed to it).
	if b, ok := x.h.inFlight(jobKey); ok && b != bind {
		return nil, reject(sealed.RejectWrongJob, "the session's job is being opened with another replyTo")
	}
	callKey := hd.Session + "/" + hd.MessageID
	if _, seen := svc.MCPCalls[callKey]; seen {
		return nil, reject(sealed.RejectReplayed, "a (session, messageId) the service forwarded before")
	}
	if x.h.mcpCallElsewhere(x.service, callKey) {
		return nil, reject(sealed.RejectReplayed, "a (session, messageId) another service of this node forwarded")
	}
	flight := flightKey("m", "", callKey)
	if _, ok := x.h.inFlight(flight); ok {
		return nil, reject(sealed.RejectReplayed, "a (session, messageId) the service is being forwarded")
	}
	if firstByte(plain) != '{' || !json.Valid(plain) {
		return nil, reject(sealed.RejectBadHeader, "the sealed arguments are not a JSON object")
	}
	delete(meta, mcpwire.MetaSealed)
	params["arguments"] = plain
	if len(meta) == 0 {
		delete(params, "_meta")
	} else {
		params["_meta"] = a2awire.Encode(meta)
	}
	out, err := mcpwire.SetMember(body, "params", a2awire.Encode(params))
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, err.Error())
	}
	x.call = &sealedCall{tool: name, messageID: hd.MessageID, replyTo: hd.ReplyTo, rk: hd.RK}
	// Rule 5's node-wide check, checked again and reserved in one step under the node-wide lock (D221).
	if !x.h.claimNode(flight, x.pre.Task, x.h.nCalls, callKey, x.service) {
		x.call = nil
		return nil, reject(sealed.RejectReplayed, "a (session, messageId) another service of this node forwarded, or is forwarding")
	}
	x.reserved = append(x.reserved, flight)
	if job == nil {
		x.h.reserve(bind, jobKey)
		x.reserved = append(x.reserved, jobKey)
	}
	sid := x.upstreamSID
	if _, indexed := svc.Sessions[sid]; sid != "" && !indexed {
		// The upstream session is the job's from the node's checks on (D226), as its binding is.
		sk := flightKey("s", x.service, sid)
		x.h.reserve(x.jobID, sk)
		x.reserved = append(x.reserved, sk)
	}
	service, jobID, gate := x.service, x.jobID, x.pre.Task
	x.commit = append(x.commit, func() error {
		now := x.h.cfg.Now()
		j, err := x.h.jobFor(service, jobID)
		if err != nil {
			return err
		}
		switch {
		case j == nil:
			j = &jobState{ID: jobID, ReplyTo: hd.ReplyTo, RK: hd.RK, Created: now, Session: hd.Session}
			j.init()
		case bindOf(j.ReplyTo, j.RK) != bind:
			return fmt.Errorf("sealing: job %s is bound to another replyTo: this call records nothing", jobID)
		}
		j.LastSeen = now
		svc.put(setMCPCalls, callKey, now)
		x.h.indexNode(setMCPCalls, callKey, service, true)
		if sid != "" && svc.Sessions[sid] != j.ID {
			svc.put(setSessions, sid, j.ID)
		}
		if err := x.h.saveJob(service, j); err != nil {
			return err
		}
		return x.h.st.saveService(service, svc)
	})
	x.record(svc, respKey(x.upstreamSID, m.ID), func(*jobState) *sealedRequest {
		return &sealedRequest{Kind: reqCall, Tool: name, MessageID: hd.MessageID, Gate: gate, ReplyTo: hd.ReplyTo, RK: hd.RK}
	})
	return out, nil
}

// answer applies rule 4b and "answers must be sealed" to a client's JSON-RPC response. Which input request a response
// answers is the node's own record, not the preamble's word: on an upstream session, the input request it sealed with
// that id on that session (svc.Inputs), whatever job or task the exchange's preamble names; without one, the input
// request of the preamble's job and task. A response to one is refused unless it is sealed (bad_header) and passes
// rule 4b against that request's job and task (wrong_message when it comes on an exchange of another session or
// task, replayed when the request was answered).
func (x *MCPExchange) answer(svc *serviceState, job *jobState, body []byte, m *mcpwire.Message) ([]byte, error) {
	idStr := mcpID(m.ID)
	var (
		result map[string]json.RawMessage
		meta   map[string]json.RawMessage
		env    json.RawMessage
		hasEnv bool
	)
	if m.Result != nil && firstByte(m.Result) == '{' {
		var err error
		if result, err = strictjson.Members(m.Result); err != nil {
			return nil, reject(sealed.RejectBadHeader, "an answer that is not one with exact keys")
		}
		if meta, env, hasEnv, err = sealedMeta(result); err != nil {
			return nil, err
		}
	}
	var (
		ij    *jobState
		in    *InputRequest
		ikey  string
		known bool // the id names an input request this node sealed
	)
	// Every id the server would take the response for is looked up ("8" answers the server's request 8 for the
	// TypeScript SDK, mcpwire.AnswerKeys): a response the server reads as the answer to a sealed input request is
	// that request's answer here too.
	for _, rkey := range answerKeys(x.upstreamSID, m.ID) {
		switch {
		case x.upstreamSID != "":
			if e, ok := svc.Inputs[rkey]; ok && !known {
				known = true
				j, err := x.h.jobFor(x.service, e.Job)
				if err != nil {
					return nil, err
				}
				if j != nil {
					ij, in, ikey = j, j.Inputs[e.Key], e.Key
				}
			}
		case job != nil:
			if in == nil {
				ikey = rkey + "\x00" + x.pre.Task
				if in = job.Inputs[ikey]; in != nil {
					ij, known = job, true
				}
			}
			for k := range job.Inputs {
				if strings.HasPrefix(k, rkey+"\x00") {
					known = true // an input request of another task with this id: an unsealed answer is refused all the same
				}
			}
		}
	}
	if !known {
		if hasEnv {
			return nil, reject(sealed.RejectWrongMessage, "a sealed answer to no input request this node sealed")
		}
		return body, nil
	}
	if !hasEnv {
		return nil, reject(sealed.RejectBadHeader, "an unsealed answer to a sealed input request")
	}
	switch {
	case in == nil:
		return nil, reject(sealed.RejectWrongMessage, "an answer to an input request this node no longer holds")
	case ij.ID != x.jobID || in.Gate != x.pre.Task:
		return nil, reject(sealed.RejectWrongMessage, "an answer on an exchange of another session or task than its input request's")
	case in.Answered:
		return nil, reject(sealed.RejectReplayed, "an answer to an input request the service has had answered")
	}
	flight := flightKey("a", x.service, ij.ID, ikey)
	if _, ok := x.h.inFlight(flight); ok {
		return nil, reject(sealed.RejectReplayed, "an answer to an input request the service is being given")
	}
	ct, err := sealed.ParseMCPEnvelope(env)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, err.Error())
	}
	hd, plain, err := sealed.Open(x.h.cfg.Identities(), ct)
	if err != nil {
		return nil, err
	}
	if err := x.h.mcpRule1(hd, true); err != nil {
		return nil, err
	}
	if err := x.h.checkService(hd, x.service); err != nil {
		return nil, err
	}
	now := x.h.cfg.Now()
	switch {
	case hd.Session != x.pre.MCPSession, hd.Session != ij.Session, hd.Tool != in.Tool, hd.MessageID != idStr:
		return nil, reject(sealed.RejectWrongMessage, "the answer's session, tool or messageId is not the input request's")
	case hd.TaskID != in.Gate:
		// The answer names the gate task whose input request it answers (security review: upstream ids restart in each
		// durable task's own upstream session, so session, tool and messageId alone match another task's request).
		return nil, reject(sealed.RejectWrongMessage, "the answer's taskId is not its input request's task")
	case now.Sub(in.SealedAt) > x.h.cfg.MaxAge:
		return nil, reject(sealed.RejectWrongMessage, "the input request was sealed more than --sealed-max-age ago")
	case hd.ReplyTo != ij.ReplyTo || hd.RK != ij.RK:
		return nil, reject(sealed.RejectWrongJob, "the answer's replyTo or rk is not the job's")
	}
	if firstByte(plain) != '{' || !json.Valid(plain) {
		return nil, reject(sealed.RejectBadHeader, "the sealed answer is not a JSON object")
	}
	res, err := strictjson.Members(plain)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, "the sealed answer is not one object with exact keys")
	}
	delete(meta, mcpwire.MetaSealed)
	if len(meta) > 0 {
		res["_meta"] = a2awire.Encode(meta)
	}
	out, err := mcpwire.SetMember(body, "result", a2awire.Encode(res))
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, err.Error())
	}
	x.h.reserve(x.pre.Task, flight)
	x.reserved = append(x.reserved, flight)
	service, jobID, key := x.service, ij.ID, ikey
	x.commit = append(x.commit, func() error {
		j, err := x.h.jobFor(service, jobID)
		if err != nil || j == nil {
			return err
		}
		if r := j.Inputs[key]; r != nil {
			r.Answered = true
		}
		j.LastSeen = x.h.cfg.Now()
		return x.h.saveJob(service, j)
	})
	return out, nil
}

// mcpRule1 is rule 1 for an MCP request or answer: dir req, one data part (part 0 of 1, no mediaType), the node's
// key id in kids, iat within the window, and replyTo and rk present; no A2A member (an answer's taskId aside).
//
// SPEC-CONFLICT (D218; ext-sealed.md §3's table, MCP request column: taskId "—"; rule 4b): an answer to an input
// request binds session, tool and messageId, and every durable task of a session has its own upstream session
// (relay.md §11.5, D22), whose request ids start again, so one answer would match the request of another task of
// the same tool: a gate could deliver the buyer's answer to one task's question as the answer to another's. The
// closest faithful behaviour, as the input request's own header already does (sealInput): the answer carries the
// gate task id of the input request it answers (its header's taskId, "" when that had none), and the node requires
// it; a tools/call still carries none.
func (h *Hosting) mcpRule1(hd sealed.Header, answer bool) error {
	switch {
	case hd.Dir != sealed.DirRequest:
		return reject(sealed.RejectBadHeader, "an MCP request header that is not dir req")
	case hd.Part != 0 || hd.Parts != 1 || hd.Kind != sealed.KindData || hd.MediaType != "":
		return reject(sealed.RejectBadHeader, "an MCP header that is not one data part")
	case hd.ReplyTo == "" || hd.RK == "":
		return reject(sealed.RejectBadHeader, "an MCP request header without replyTo or rk")
	case hd.TaskID != "" && !answer || hd.ContextID != "" || hd.ArtifactID != "" || hd.Chunk != nil:
		return reject(sealed.RejectBadHeader, "an MCP request header with A2A members")
	}
	if _, err := age.ParseX25519Recipient(hd.ReplyTo); err != nil {
		return reject(sealed.RejectBadHeader, "replyTo is not an X25519 recipient")
	}
	mine := h.cfg.KeyIDs()
	found := false
	for _, k := range hd.Kids {
		for _, m := range mine {
			found = found || k == m
		}
	}
	if !found {
		return reject(sealed.RejectNotRecipient, "the node's key id is not in kids")
	}
	now := h.cfg.Now().Unix()
	if hd.IAT > now+sealed.MaxClockSkew || now-hd.IAT > int64(h.cfg.MaxAge/time.Second) {
		return reject(sealed.RejectReplayed, "iat outside the node's window")
	}
	return nil
}

// Sealed reports whether a client message that failed strict decoding is the sealing's to refuse (bad_header,
// plan corrections WP-08) rather than the node's ordinary -32600: the service was started with --sealed-required,
// the exchange's session has a sealed job, or the bytes name the sealed envelope's key (in any case).
func (x *MCPExchange) Sealed(body []byte) bool {
	if x == nil {
		return false
	}
	if x.h.Required(x.service) || bytes.Contains(bytes.ToLower(body), []byte(mcpwire.MetaSealed)) {
		return true
	}
	hs := x.h.lock(x.service)
	defer hs.mu.Unlock()
	j, err := x.h.jobFor(x.service, x.jobID)
	return err == nil && j != nil
}

// RefuseMalformed answers a message that failed strict decoding on a sealed exchange: bad_header (§6).
func (x *MCPExchange) RefuseMalformed(w http.ResponseWriter, id json.RawMessage) {
	x.Refuse(w, id, reject(sealed.RejectBadHeader, "a message that is not one JSON-RPC message with exact keys (D172)"))
}

// Refuse answers a message FromClient refused (§6): 400, Kontra-Node: sealed_rejected, its reason, a JSON-RPC
// error with the message's id.
func (x *MCPExchange) Refuse(w http.ResponseWriter, id json.RawMessage, err error) {
	x.h.log.Warn("sealing: an MCP message is refused", "service", x.service, "reason", reasonText(err))
	WriteReject(w, id, err)
}

// Accepted records what the service's acceptance of the exchange's message commits: a sealed call's job and its
// (session, messageId), an answer's input request answered. Called once the service accepted the message (a 2xx
// from a URL server, the message written to a stdio server); a message the node answered itself commits nothing.
func (x *MCPExchange) Accepted() {
	if x == nil || len(x.commit) == 0 && len(x.reserved) == 0 {
		return
	}
	hs := x.h.lock(x.service)
	defer hs.mu.Unlock()
	for _, c := range x.commit {
		if err := c(); err != nil {
			x.h.log.Error("sealing: the job's state was not written", "service", x.service, "err", err)
		}
	}
	x.commit = nil
	x.h.unreserve(x.reserved)
	x.reserved = nil
}

// Release drops what the exchange's message holds in flight without recording it: the node answered the message
// itself, or the server refused it (a non-2xx), so the gate's retry of it is not a replay (spec §5). After Accepted
// it does nothing. Callers release on every path where the message did not reach the server, and once more when
// the exchange ends.
func (x *MCPExchange) Release() {
	if x == nil {
		return
	}
	hs := x.h.lock(x.service)
	defer hs.mu.Unlock()
	x.commit = nil
	x.h.unreserve(x.reserved)
	x.reserved = nil
}

// FromServer seals one message of the server's for the client (after the roots filter): out is what to relay, keep
// false removes it, and an error refuses it (nothing of it is relayed; a JSON response becomes the node's
// refusal, an event stream ends before it).
//
// The answer to a sealed request of a job (a sealed tools/call, a bound task's tasks/result) is sealed by the job's
// record of that request, on whichever exchange of its upstream session it comes (a GET stream resumed with
// Last-Event-ID included), and refused on an exchange of another session; an error answer to one keeps its code only.
func (x *MCPExchange) FromServer(data []byte) (out []byte, keep bool, err error) {
	m, err := mcpwire.Parse(data, mcpwire.FromUpstream)
	if err != nil {
		return nil, false, reject(sealed.RejectBadHeader, "a server message that is not one with exact keys")
	}
	h := x.h
	hs := h.lock(x.service)
	defer hs.mu.Unlock()
	svc, err := h.service(x.service)
	if err != nil {
		return nil, false, err
	}
	job, err := h.jobFor(x.service, x.jobID)
	if err != nil {
		return nil, false, err
	}
	// A message naming a sealed task of another session never reaches this one.
	if id, ok := relatedTask(m); ok {
		if owner, bound := svc.Tasks[id]; bound && owner != x.jobID {
			return nil, false, reject(sealed.RejectWrongTask, "a server message names a sealed task of another session")
		}
	}
	if owner, ok := x.sessionOwner(svc); ok && owner != x.jobID {
		// A message of a sealed job's upstream session on an exchange of another session, or of none (a GET the gate
		// opened with the job's Mcp-Session-Id under another preamble): none of it is relayed (D217, D226).
		return nil, false, reject(sealed.RejectWrongJob, "a message of another session's sealed job's upstream session")
	}
	// Whether a response answers the exchange's request, and which recorded request it answers, as a client matches
	// ids (mcpwire.SameID, AnswerKeys): a response the client would take for a sealed request's answer is sealed or
	// refused as that answer, never relayed as another's.
	isResp := m.Method == "" && len(m.ID) > 0 && mcpwire.SameID(m.ID, x.reqID)
	if m.Method == "" && len(m.ID) > 0 {
		keys := answerKeys(x.upstreamSID, m.ID)
		key, owner := keys[0], ""
		var req *sealedRequest
		for _, k := range keys {
			o := ""
			if x.upstreamSID != "" {
				o = svc.Responses[k].Job
			}
			var r *sealedRequest
			if job != nil {
				r = job.Calls[k]
			}
			if o != "" || r != nil {
				key, owner, req = k, o, r
				break
			}
		}
		if owner != "" && owner != x.jobID {
			return nil, false, reject(sealed.RejectWrongMessage, "the answer to another session's sealed request")
		}
		switch {
		case req != nil:
		case isResp && x.call != nil:
			// The exchange's own sealed call, not recorded (the server refused it, or its record failed).
			req = &sealedRequest{Kind: reqCall, Tool: x.call.tool, MessageID: x.call.messageID, Gate: x.pre.Task,
				ReplyTo: x.call.replyTo, RK: x.call.rk}
		case isResp && x.reqMethod == "tasks/result" && job != nil && job.Tasks[x.taskOf] != nil:
			t := job.Tasks[x.taskOf]
			req = &sealedRequest{Kind: reqResult, Tool: t.Tool, MessageID: t.MessageID, Task: x.taskOf, Gate: t.Gate}
		case owner != "":
			// The job's sealed request, which the job no longer holds (older than --sealed-max-age): not relayed.
			return nil, false, nil
		case !isResp && job != nil:
			// On a sealed job's exchange, a response to no request of the exchange and to none the node holds a record
			// of: a sealed request's answer replayed after its record was pruned (Sweep, D226) looks the same as a
			// resumed answer to an unsealed one, and is not relayed.
			return nil, false, nil
		}
		if req != nil {
			return x.sealAnswer(svc, job, req, key, data, m)
		}
	}
	// A sealed task's statusMessage is "sealed" wherever the node reads one, on every exchange, a session with no
	// job and a modern exchange included (§2): an upstream that does not scope tasks/list or its status
	// notifications per session would otherwise hand one session another's.
	switch {
	case m.Method == "notifications/tasks/status":
		if id, _ := memberString(m.Params, "taskId"); id != "" {
			if owner, bound := svc.Tasks[id]; bound && owner != x.jobID {
				return nil, false, nil // another session's sealed task: not this session's to see
			}
		}
	case job == nil && isResp && m.Result != nil && (x.reqMethod == "tasks/get" || x.reqMethod == "tasks/cancel"):
		if id, _ := memberString(m.Result, "taskId"); id != "" && svc.Tasks[id] != "" {
			res, err := sealStatus(m.Result, "")
			if err != nil {
				return nil, false, err
			}
			if data, err = mcpwire.SetMember(data, "result", res); err != nil {
				return nil, false, reject(sealed.RejectBadHeader, err.Error())
			}
			return compactLine(data)
		}
	case job == nil && isResp && m.Result != nil && x.reqMethod == "tasks/list":
		res, err := sealList(svc, m.Result)
		if err != nil {
			return nil, false, err
		}
		if !bytes.Equal(res, m.Result) {
			if data, err = mcpwire.SetMember(data, "result", res); err != nil {
				return nil, false, reject(sealed.RejectBadHeader, err.Error())
			}
			return compactLine(data)
		}
	}
	if job == nil {
		return data, true, nil
	}
	changed := false
	switch {
	case isResp && m.Result != nil && (x.reqMethod == "tasks/get" || x.reqMethod == "tasks/cancel"):
		if id, _ := memberString(m.Result, "taskId"); job.Tasks[id] != nil || id != "" && svc.Tasks[id] != "" {
			res, err := sealStatus(m.Result, "")
			if err != nil {
				return nil, false, err
			}
			if data, err = mcpwire.SetMember(data, "result", res); err != nil {
				return nil, false, reject(sealed.RejectBadHeader, err.Error())
			}
			changed = true
		}
	case isResp && m.Result != nil && x.reqMethod == "tasks/list":
		res, err := sealList(svc, m.Result)
		if err != nil {
			return nil, false, err
		}
		if !bytes.Equal(res, m.Result) {
			if data, err = mcpwire.SetMember(data, "result", res); err != nil {
				return nil, false, reject(sealed.RejectBadHeader, err.Error())
			}
			changed = true
		}
	case m.Method == mcpwire.MethodElicit || m.Method == mcpwire.MethodSample:
		in := x.inputOf(job, m)
		if in == nil {
			break // not a request of a sealed call or of a task one created: only those are sealed (§2)
		}
		params, err := x.sealInput(svc, job, in, m)
		if err != nil {
			return nil, false, err
		}
		if data, err = mcpwire.SetMember(data, "params", params); err != nil {
			return nil, false, reject(sealed.RejectBadHeader, err.Error())
		}
		changed = true
	case m.Method == "notifications/tasks/status":
		if id, _ := memberString(m.Params, "taskId"); job.Tasks[id] != nil {
			p, err := sealStatus(m.Params, "")
			if err != nil {
				return nil, false, err
			}
			if data, err = mcpwire.SetMember(data, "params", p); err != nil {
				return nil, false, reject(sealed.RejectBadHeader, err.Error())
			}
			changed = true
		}
	}
	if changed {
		job.LastSeen = h.cfg.Now()
		if err := h.saveJob(x.service, job); err != nil {
			return nil, false, err
		}
		return compactLine(data)
	}
	return data, true, nil
}

// sealAnswer seals the server's answer to a sealed request of the job (req, recorded under key): a sealed call's
// CallToolResult to the call's own replyTo and rk (which must be the job's, wrong_job otherwise), or its
// CreateTaskResult binding the task to the job; a bound task's tasks/result to the job's; an error with its code
// only (errorResponse's SPEC-CONFLICT). the service's lock is held.
func (x *MCPExchange) sealAnswer(svc *serviceState, job *jobState, req *sealedRequest, key string, data []byte,
	m *mcpwire.Message) ([]byte, bool, error) {
	h := x.h
	if m.Error != nil {
		e := map[string]json.RawMessage{"code": json.RawMessage(fmt.Sprint(m.Error.Code)), "message": a2awire.Quote(sealedErrorText)}
		out, err := mcpwire.SetMember(data, "error", a2awire.Encode(e))
		if err != nil {
			return nil, false, reject(sealed.RejectBadHeader, err.Error())
		}
		if r := job.callOf(key); r != nil && !r.Answered {
			r.Answered = true
			if err := h.saveJob(x.service, job); err != nil {
				return nil, false, err
			}
		}
		return compactLine(out)
	}
	if m.Result == nil || job == nil {
		return nil, false, reject(sealed.RejectBadHeader, "a result of a sealed request the session holds no job for")
	}
	var res json.RawMessage
	switch req.Kind {
	case reqCall:
		if bindOf(job.ReplyTo, job.RK) != bindOf(req.ReplyTo, req.RK) {
			return nil, false, reject(sealed.RejectWrongJob, "the session's job is bound to another replyTo than the call's")
		}
		if task, ok := mcpwire.Member(m.Result, "task"); ok && firstByte(task) == '{' {
			up, _ := memberString(task, "taskId")
			if up == "" {
				return nil, false, reject(sealed.RejectBadHeader, "a CreateTaskResult without a taskId")
			}
			if owner, bound := svc.Tasks[up]; bound && owner != job.ID {
				return nil, false, reject(sealed.RejectWrongTask, "a CreateTaskResult names another job's task")
			}
			if job.Tasks[up] == nil {
				job.Tasks[up] = &mcpTask{Gate: req.Gate, Tool: req.Tool, MessageID: req.MessageID}
			}
			if svc.Tasks[up] != job.ID {
				svc.put(setTasks, up, job.ID)
				if err := h.st.saveService(x.service, svc); err != nil {
					return nil, false, err
				}
			}
			r, err := sealStatus(m.Result, "task")
			if err != nil {
				return nil, false, err
			}
			res = r
			break
		}
		r, err := x.sealResult(job, m.Result, req.Tool, req.MessageID, "")
		if err != nil {
			return nil, false, err
		}
		res = r
	case reqResult:
		t := job.Tasks[req.Task]
		if t == nil {
			return nil, false, reject(sealed.RejectWrongTask, "a tasks/result of a task the job does not hold")
		}
		r, err := x.sealResult(job, m.Result, t.Tool, t.MessageID, t.Gate)
		if err != nil {
			return nil, false, err
		}
		res = r
	default:
		return nil, false, reject(sealed.RejectBadHeader, "a sealed request of no known kind")
	}
	out, err := mcpwire.SetMember(data, "result", res)
	if err != nil {
		return nil, false, reject(sealed.RejectBadHeader, err.Error())
	}
	if r := job.Calls[key]; r != nil {
		r.Answered = true
	}
	job.LastSeen = h.cfg.Now()
	if err := h.saveJob(x.service, job); err != nil {
		return nil, false, err
	}
	return compactLine(out)
}

// inputOf is the input request an upstream elicitation/create or sampling/createMessage is, when the node seals it
// (D175): of a task a sealed call of the job created (its related-task, or the preamble's gate task), or, without a
// related task, of the exchange's own sealed call (a plain call's request on its stream). nil: not sealed.
//
// SPEC-CONFLICT (D219; ext-sealed.md §2: input requests and their answers are sealed "for a task the sealed call
// created"; §2 and D62 also say the gate holds no plaintext of a sealed MCP call, plain or durable): a plain call's
// server builds its input requests from the sealed arguments too (server-everything's sampleLLM sends the prompt).
// The closest faithful behaviour: a sealed plain call's input requests are sealed as a task's are, with the call's
// tool and no taskId, and their answers must come sealed.
func (x *MCPExchange) inputOf(job *jobState, m *mcpwire.Message) *InputRequest {
	up, ok := relatedTask(m)
	t := job.Tasks[up]
	if !ok || t == nil {
		for id, tt := range job.Tasks {
			if !ok && x.pre.Task != "" && tt.Gate == x.pre.Task {
				up, t = id, tt
			}
		}
	}
	switch {
	case t != nil:
		return &InputRequest{Gate: t.Gate, UpTask: up, Tool: t.Tool}
	case !ok && x.call != nil:
		return &InputRequest{Tool: x.call.tool}
	}
	return nil
}

// compactLine is a message the node rewrote, on one line (an SSE data line, a stdio line).
func compactLine(data []byte) ([]byte, bool, error) {
	var c bytes.Buffer
	if err := json.Compact(&c, data); err != nil {
		return nil, false, reject(sealed.RejectBadHeader, "a sealed message that is not JSON")
	}
	return c.Bytes(), true, nil
}

// sealResult seals a CallToolResult (§3): content and structuredContent into one kontra:sealed resource, isError
// kept outside (the gate meters on it), every other member kept.
func (x *MCPExchange) sealResult(job *jobState, result json.RawMessage, tool, messageID, gateTask string) (json.RawMessage, error) {
	res, err := strictjson.Members(result)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, "a result that is not one object with exact keys")
	}
	if err := strictjson.CheckReads(res, nil, resultReads); err != nil {
		return nil, reject(sealed.RejectBadHeader, "a result with a case variant of a member the node reads")
	}
	if _, claims, _ := sealed.ParseMCPResult(result); claims {
		return nil, reject(sealed.RejectBadHeader, "the service sent a result with the sealed resource")
	}
	inner := map[string]json.RawMessage{}
	if v, ok := res["content"]; ok {
		inner["content"] = v
	}
	if v, ok := res["structuredContent"]; ok {
		inner["structuredContent"] = v
	}
	body, err := jcs.Transform(a2awire.Encode(inner))
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, "a result with no canonical form")
	}
	isError := false
	if v, ok := res["isError"]; ok {
		switch string(bytes.TrimSpace(v)) {
		case "true":
			isError = true
		case "false":
		default:
			return nil, reject(sealed.RejectBadHeader, "isError is not a boolean")
		}
	}
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirReply, Service: x.h.servicePath(x.service), TaskID: gateTask,
		Session: job.Session, Tool: tool, MessageID: messageID, Part: 0, Parts: 1}
	ct, err := x.h.sealFor(x.service, job, hd, plainOf{kind: sealed.KindData, body: body})
	if err != nil {
		return nil, err
	}
	env, err := sealed.MCPResult(ct, isError)
	if err != nil {
		return nil, err
	}
	envm, _ := strictjson.Members(env)
	delete(res, "structuredContent")
	res["content"] = envm["content"]
	res["isError"] = envm["isError"]
	return a2awire.Encode(res), nil
}

// resultReads are the members of a CallToolResult the node reads: a lone case variant of one is refused (D172).
var resultReads = []strictjson.Path{{"content"}, {"structuredContent"}, {"isError"}}

// sealInput seals an upstream input request (D175): its params, without _meta, become the body; params are replaced
// by {"_meta": {<the related-task key>, "sh.kontra/sealed": …}}; the InputRequest is recorded in the job by the
// upstream session and the server's id (and the gate task its answer must come on), and indexed for the service when
// the exchange is on an upstream session, so that its answer is checked against it on whichever exchange it comes.
func (x *MCPExchange) sealInput(svc *serviceState, job *jobState, in *InputRequest, m *mcpwire.Message) (json.RawMessage, error) {
	if len(m.ID) == 0 {
		return nil, reject(sealed.RejectBadHeader, "an input request without an id")
	}
	params, err := strictjson.Members(m.Params)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, "an input request without params")
	}
	var related json.RawMessage
	if raw, ok := params["_meta"]; ok {
		meta, err := strictjson.Members(raw)
		if err != nil {
			return nil, reject(sealed.RejectBadHeader, "an input request's _meta is not one object with exact keys")
		}
		related = meta[mcpwire.MetaRelatedTask]
	}
	delete(params, "_meta")
	body, err := jcs.Transform(a2awire.Encode(params))
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, "an input request with no canonical form")
	}
	msgID := mcpID(m.ID)
	// SPEC-CONFLICT (D218; ext-sealed.md §3's table): the MCP reply column names taskId only "for a durable task's
	// result", and an input request is "sealed like a reply" with session, tool and messageId. The closest faithful
	// header: a durable task's input request carries its gate task id too (as its result does), so that the hiring
	// node can tell an input request replayed from another task of the same session and tool (rule 4, which it then
	// checks), and its answer names it back (mcpRule1); a passthrough task's, or a plain call's, carries none.
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirReply, Service: x.h.servicePath(x.service), TaskID: in.Gate,
		Session: job.Session, Tool: in.Tool, MessageID: msgID, Part: 0, Parts: 1}
	ct, err := x.h.sealFor(x.service, job, hd, plainOf{kind: sealed.KindData, body: body})
	if err != nil {
		return nil, err
	}
	envb, err := json.Marshal(sealed.NewMCPEnvelope(ct))
	if err != nil {
		return nil, err
	}
	meta := map[string]json.RawMessage{mcpwire.MetaSealed: envb}
	if related != nil {
		meta[mcpwire.MetaRelatedTask] = related
	}
	rkey := respKey(x.upstreamSID, m.ID)
	ikey := rkey + "\x00" + in.Gate
	if cur := job.Inputs[ikey]; cur == nil || cur.Answered && cur.MessageID == msgID && cur.Tool != in.Tool {
		in.MessageID, in.SealedAt = msgID, x.h.cfg.Now()
		job.Inputs[ikey] = in
	}
	if x.upstreamSID != "" {
		if e := svc.Inputs[rkey]; e.Job != job.ID || e.Key != ikey {
			svc.put(setInputs, rkey, indexEntry{Job: job.ID, Key: ikey})
			if err := x.h.st.saveService(x.service, svc); err != nil {
				return nil, err
			}
		}
	}
	return a2awire.Encode(map[string]json.RawMessage{"_meta": a2awire.Encode(meta)}), nil
}

// sealList replaces the statusMessage of every sealed task (bound to any job of the service) of a tasks/list result.
func sealList(svc *serviceState, result json.RawMessage) (json.RawMessage, error) {
	res, err := strictjson.Members(result)
	if err != nil {
		return result, nil
	}
	raw, ok := res["tasks"]
	if !ok || firstByte(raw) != '[' {
		return result, nil
	}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil {
		return result, nil
	}
	changed := false
	for i, e := range elems {
		if id, _ := memberString(e, "taskId"); id != "" && svc.Tasks[id] != "" {
			s, err := sealStatus(e, "")
			if err != nil {
				return nil, err
			}
			elems[i], changed = s, true
		}
	}
	if !changed {
		return result, nil
	}
	res["tasks"] = a2awire.EncodeArray(elems)
	return a2awire.Encode(res), nil
}

// sealStatus replaces the statusMessage of a task object (obj itself, or obj[member]) with "sealed" (§2).
func sealStatus(obj json.RawMessage, member string) (json.RawMessage, error) {
	m, err := strictjson.Members(obj)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, "a task that is not one object with exact keys")
	}
	target := m
	if member != "" {
		if target, err = strictjson.Members(m[member]); err != nil {
			return nil, reject(sealed.RejectBadHeader, "a task that is not one object with exact keys")
		}
	}
	if err := strictjson.CheckReads(target, nil, []strictjson.Path{{"statusMessage"}, {"taskId"}}); err != nil {
		return nil, reject(sealed.RejectBadHeader, "a task with a case variant of statusMessage")
	}
	if _, ok := target["statusMessage"]; ok {
		target["statusMessage"] = a2awire.Quote("sealed")
	}
	if member != "" {
		m[member] = a2awire.Encode(target)
		return a2awire.Encode(m), nil
	}
	return a2awire.Encode(target), nil
}

// sealFor seals one body to the job's replyTo with auth, reusing the ciphertext of the same header and body.
func (h *Hosting) sealFor(service string, job *jobState, hd sealed.Header, pl plainOf) ([]byte, error) {
	x := &Exchange{h: h, service: service}
	ct, _, err := x.sealOne(job, hd, pl)
	return ct, err
}

// sealedMeta reads obj._meta and its "sh.kontra/sealed" with exact keys: a case variant of the key is bad_header.
func sealedMeta(obj map[string]json.RawMessage) (meta map[string]json.RawMessage, env json.RawMessage, ok bool, err error) {
	raw, has := obj["_meta"]
	if !has || firstByte(raw) != '{' {
		for k := range obj {
			if k != "_meta" && strings.EqualFold(k, "_meta") {
				return nil, nil, false, reject(sealed.RejectBadHeader, "a case variant of _meta")
			}
		}
		return map[string]json.RawMessage{}, nil, false, nil
	}
	meta, err = strictjson.Members(raw)
	if err != nil {
		return nil, nil, false, reject(sealed.RejectBadHeader, "_meta is not one object with exact keys")
	}
	for k, v := range meta {
		if k == mcpwire.MetaSealed {
			env, ok = v, true
			continue
		}
		if strings.EqualFold(k, mcpwire.MetaSealed) {
			return nil, nil, false, reject(sealed.RejectBadHeader, "a case variant of the sealed envelope's key")
		}
	}
	return meta, env, ok, nil
}

// relatedTask is the related-task taskId of a message's params._meta or result._meta.
func relatedTask(m *mcpwire.Message) (string, bool) {
	for _, obj := range []json.RawMessage{m.Params, m.Result} {
		if meta, ok := mcpwire.Member(obj, "_meta"); ok {
			if rt, ok := mcpwire.Member(meta, mcpwire.MetaRelatedTask); ok {
				if id, ok := memberString(rt, "taskId"); ok {
					return id, true
				}
			}
		}
	}
	return "", false
}

// memberString is obj's member key when it is a string, read with exact keys.
func memberString(obj json.RawMessage, key string) (string, bool) {
	v, ok := mcpwire.Member(obj, key)
	if !ok || firstByte(v) != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

// mcpID is a JSON-RPC id as the header's messageId (§3: "as a string"): a string id as it is, any other id as the
// JSON text it came as, the gate's rule for the preamble's mcp_request_id (internal/mcprelay idString).
func mcpID(id json.RawMessage) string {
	id = bytes.TrimSpace(id)
	if len(id) > 0 && id[0] == '"' {
		var s string
		if json.Unmarshal(id, &s) == nil {
			return s
		}
	}
	return string(id)
}

func emptyObject(raw json.RawMessage) bool {
	m, err := strictjson.Members(raw)
	return err == nil && len(m) == 0
}
