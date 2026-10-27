package sealing

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
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

	reqMethod string
	reqID     json.RawMessage
	call      *sealedCall // the exchange's sealed tools/call
	taskOf    string      // the upstream task id a tasks/* request names
	commit    []func() error
}

// sealedCall is a sealed tools/call of the exchange.
type sealedCall struct {
	tool, messageID string
}

// MCP returns the sealing state of one MCP exchange of the service the preamble names.
func (h *Hosting) MCP(pre tunnel.Preamble) *MCPExchange {
	x := &MCPExchange{h: h, pre: pre, service: pre.Service}
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
	h.mu.Lock()
	defer h.mu.Unlock()
	x.reqMethod, x.reqID = m.Method, m.ID
	svc, err := h.service(x.service)
	if err != nil {
		return nil, err
	}
	job, err := h.jobFor(x.service, x.jobID)
	if err != nil {
		return nil, err
	}
	switch {
	case m.Method == mcpwire.MethodToolsCall:
		return x.toolsCall(svc, job, body, m)
	case m.Method == "":
		return x.answer(job, body, m)
	case strings.HasPrefix(m.Method, "tasks/"):
		id, _ := memberString(m.Params, "taskId")
		if owner, ok := svc.Tasks[id]; ok && id != "" && owner != x.jobID {
			return nil, reject(sealed.RejectWrongTask, "a tasks/* request names a sealed task of another session")
		}
		x.taskOf = id
	}
	return body, nil
}

// toolsCall checks and opens a sealed tools/call (rules 1, 4, 5); an unsealed one passes unless the service was
// started with --sealed-required.
func (x *MCPExchange) toolsCall(svc *serviceState, job *jobState, body []byte, m *mcpwire.Message) ([]byte, error) {
	params, err := strictjson.Members(m.Params)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, "a tools/call without params")
	}
	meta, env, hasEnv, err := sealedMeta(params)
	if err != nil {
		return nil, err
	}
	if !hasEnv {
		if x.h.Required(x.service) {
			return nil, reject(sealed.RejectBadHeader, "an unsealed tools/call to a service started with --sealed-required")
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
	if err := x.h.mcpRule1(hd); err != nil {
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
	if job != nil && (job.ReplyTo != hd.ReplyTo || job.RK != hd.RK) {
		return nil, reject(sealed.RejectWrongJob, "the session's job was opened with another replyTo")
	}
	callKey := hd.Session + "/" + hd.MessageID
	if _, seen := svc.MCPCalls[callKey]; seen {
		return nil, reject(sealed.RejectReplayed, "a (session, messageId) the service forwarded before")
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
	x.call = &sealedCall{tool: name, messageID: hd.MessageID}
	service, jobID := x.service, x.jobID
	x.commit = append(x.commit, func() error {
		now := x.h.cfg.Now()
		j, err := x.h.jobFor(service, jobID)
		if err != nil {
			return err
		}
		if j == nil {
			j = &jobState{ID: jobID, ReplyTo: hd.ReplyTo, RK: hd.RK, Created: now, Session: hd.Session,
				Artifacts: map[string]*artifactState{}, Tasks: map[string]*mcpTask{}}
		}
		j.LastSeen = now
		svc.MCPCalls[callKey] = now
		if err := x.h.saveJob(service, j); err != nil {
			return err
		}
		return x.h.st.saveService(service, svc)
	})
	return out, nil
}

// answer applies rule 4b and "answers must be sealed" to a client's JSON-RPC response on a job's exchange.
func (x *MCPExchange) answer(job *jobState, body []byte, m *mcpwire.Message) ([]byte, error) {
	if job == nil {
		return body, nil
	}
	idStr := mcpID(m.ID)
	var (
		task *mcpTask
		in   *InputRequest
	)
	answered := false
	for up, t := range job.Tasks {
		if x.pre.Task != "" && t.Gate != x.pre.Task {
			continue
		}
		if r := t.Inputs[idStr]; r != nil {
			if r.Answered {
				answered = true
				continue
			}
			task, in = t, r
			_ = up
			break
		}
	}
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
	if in == nil {
		switch {
		case hasEnv && answered:
			return nil, reject(sealed.RejectReplayed, "an answer to an input request the service has had answered")
		case hasEnv:
			return nil, reject(sealed.RejectWrongMessage, "a sealed answer to no input request this node sealed")
		}
		return body, nil
	}
	if !hasEnv {
		return nil, reject(sealed.RejectBadHeader, "an unsealed answer to a sealed input request")
	}
	ct, err := sealed.ParseMCPEnvelope(env)
	if err != nil {
		return nil, reject(sealed.RejectBadHeader, err.Error())
	}
	hd, plain, err := sealed.Open(x.h.cfg.Identities(), ct)
	if err != nil {
		return nil, err
	}
	if err := x.h.mcpRule1(hd); err != nil {
		return nil, err
	}
	now := x.h.cfg.Now()
	switch {
	case hd.Session != x.pre.MCPSession, hd.Tool != task.Tool, hd.MessageID != idStr:
		return nil, reject(sealed.RejectWrongMessage, "the answer's session, tool or messageId is not the input request's")
	case now.Sub(in.SealedAt) > x.h.cfg.MaxAge:
		return nil, reject(sealed.RejectWrongMessage, "the input request was sealed more than --sealed-max-age ago")
	case hd.ReplyTo != job.ReplyTo || hd.RK != job.RK:
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
	service, jobID, msgID := x.service, x.jobID, idStr
	x.commit = append(x.commit, func() error {
		j, err := x.h.jobFor(service, jobID)
		if err != nil || j == nil {
			return err
		}
		for _, t := range j.Tasks {
			if r := t.Inputs[msgID]; r != nil && (x.pre.Task == "" || t.Gate == x.pre.Task) {
				r.Answered = true
			}
		}
		j.LastSeen = x.h.cfg.Now()
		return x.h.saveJob(service, j)
	})
	return out, nil
}

// mcpRule1 is rule 1 for an MCP request or answer: dir req, one data part (part 0 of 1, no mediaType), the node's
// key id in kids, iat within the window, and replyTo and rk present; no A2A member.
func (h *Hosting) mcpRule1(hd sealed.Header) error {
	switch {
	case hd.Dir != sealed.DirRequest:
		return reject(sealed.RejectBadHeader, "an MCP request header that is not dir req")
	case hd.Part != 0 || hd.Parts != 1 || hd.Kind != sealed.KindData || hd.MediaType != "":
		return reject(sealed.RejectBadHeader, "an MCP header that is not one data part")
	case hd.ReplyTo == "" || hd.RK == "":
		return reject(sealed.RejectBadHeader, "an MCP request header without replyTo or rk")
	case hd.TaskID != "" || hd.ContextID != "" || hd.ArtifactID != "" || hd.Chunk != nil:
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
	x.h.mu.Lock()
	defer x.h.mu.Unlock()
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
	if x == nil || len(x.commit) == 0 {
		return
	}
	x.h.mu.Lock()
	defer x.h.mu.Unlock()
	for _, c := range x.commit {
		if err := c(); err != nil {
			x.h.log.Error("sealing: the job's state was not written", "service", x.service, "err", err)
		}
	}
	x.commit = nil
}

// FromServer seals one message of the server's for the client (after the roots filter): out is what to relay, keep
// false removes it, and an error refuses it (nothing of it is relayed; a JSON response becomes the node's
// refusal, an event stream ends before it).
func (x *MCPExchange) FromServer(data []byte) (out []byte, keep bool, err error) {
	m, err := mcpwire.Parse(data, mcpwire.FromUpstream)
	if err != nil {
		return nil, false, reject(sealed.RejectBadHeader, "a server message that is not one with exact keys")
	}
	h := x.h
	h.mu.Lock()
	defer h.mu.Unlock()
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
	if job == nil {
		return data, true, nil
	}
	isResp := m.Method == "" && len(m.ID) > 0 && bytes.Equal(bytes.TrimSpace(m.ID), bytes.TrimSpace(x.reqID))
	changed := false
	switch {
	case isResp && m.Result != nil && x.call != nil:
		if task, ok := mcpwire.Member(m.Result, "task"); ok && firstByte(task) == '{' {
			up, _ := memberString(task, "taskId")
			if up == "" {
				return nil, false, reject(sealed.RejectBadHeader, "a CreateTaskResult without a taskId")
			}
			if owner, bound := svc.Tasks[up]; bound && owner != job.ID {
				return nil, false, reject(sealed.RejectWrongTask, "a CreateTaskResult names another job's task")
			}
			t := job.Tasks[up]
			if t == nil {
				t = &mcpTask{Gate: x.pre.Task, Tool: x.call.tool, MessageID: x.call.messageID, Inputs: map[string]*InputRequest{}}
				job.Tasks[up] = t
			}
			svc.Tasks[up] = job.ID
			if err := h.st.saveService(x.service, svc); err != nil {
				return nil, false, err
			}
			res, err := sealStatus(m.Result, "task")
			if err != nil {
				return nil, false, err
			}
			if data, err = mcpwire.SetMember(data, "result", res); err != nil {
				return nil, false, reject(sealed.RejectBadHeader, err.Error())
			}
			changed = true
			break
		}
		res, err := x.sealResult(job, m.Result, x.call.tool, x.call.messageID, "")
		if err != nil {
			return nil, false, err
		}
		if data, err = mcpwire.SetMember(data, "result", res); err != nil {
			return nil, false, reject(sealed.RejectBadHeader, err.Error())
		}
		changed = true
	case isResp && m.Result != nil && x.reqMethod == "tasks/result" && job.Tasks[x.taskOf] != nil:
		t := job.Tasks[x.taskOf]
		res, err := x.sealResult(job, m.Result, t.Tool, t.MessageID, t.Gate)
		if err != nil {
			return nil, false, err
		}
		if data, err = mcpwire.SetMember(data, "result", res); err != nil {
			return nil, false, reject(sealed.RejectBadHeader, err.Error())
		}
		changed = true
	case isResp && m.Result != nil && (x.reqMethod == "tasks/get" || x.reqMethod == "tasks/cancel"):
		if id, _ := memberString(m.Result, "taskId"); job.Tasks[id] != nil {
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
		res, err := x.sealList(job, m.Result)
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
		up, ok := relatedTask(m)
		t := job.Tasks[up]
		if !ok || t == nil {
			// Not a request of a task a sealed call created: only those are sealed (§2).
			for id, tt := range job.Tasks {
				if !ok && x.pre.Task != "" && tt.Gate == x.pre.Task {
					up, t = id, tt
				}
			}
		}
		if t == nil {
			break
		}
		params, err := x.sealInput(job, up, t, m)
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
		var c bytes.Buffer
		if err := json.Compact(&c, data); err != nil {
			return nil, false, reject(sealed.RejectBadHeader, "a sealed message that is not JSON")
		}
		data = c.Bytes() // one line: an SSE data line, a stdio line
	}
	return data, true, nil
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
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirReply, TaskID: gateTask, Session: job.Session, Tool: tool,
		MessageID: messageID, Part: 0, Parts: 1}
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

// sealInput seals an upstream input request of a bound task (D175): its params, without _meta, become the body;
// params are replaced by {"_meta": {<the related-task key>, "sh.kontra/sealed": …}}; the InputRequest is recorded.
func (x *MCPExchange) sealInput(job *jobState, up string, t *mcpTask, m *mcpwire.Message) (json.RawMessage, error) {
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
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirReply, TaskID: t.Gate, Session: job.Session, Tool: t.Tool,
		MessageID: msgID, Part: 0, Parts: 1}
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
	if t.Inputs == nil {
		t.Inputs = map[string]*InputRequest{}
	}
	if t.Inputs[msgID] == nil {
		task := t.Gate
		if task == "" {
			task = up
		}
		t.Inputs[msgID] = &InputRequest{Task: task, MessageID: msgID, SealedAt: x.h.cfg.Now()}
	}
	return a2awire.Encode(map[string]json.RawMessage{"_meta": a2awire.Encode(meta)}), nil
}

// sealList replaces the statusMessage of every bound task of a tasks/list result.
func (x *MCPExchange) sealList(job *jobState, result json.RawMessage) (json.RawMessage, error) {
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
		if id, _ := memberString(e, "taskId"); job.Tasks[id] != nil {
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
