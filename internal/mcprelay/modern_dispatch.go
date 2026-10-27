package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
)

// The 2026-07-28 dispatch of a gate-owned MCP task (spec/relay.md §11.12, D159, D178, D180; M3 WP-07): the dispatch
// speaks to the upstream in the era of the request that created the task. It sends stateless requests on the
// attempt's session (sticky after the first upstream id, §6.4), each with the stored client _meta, MCP-Protocol-Version
// 2026-07-28, Mcp-Method and Mcp-Name, and no mcp_session or mcp_request_id in its preambles (D180):
//
//   - the client's tools/call, as stored (params.task removed, the tasks extension kept, its Mcp-Param-* headers);
//   - a CallToolResult answering it is the task's result;
//   - a CreateTaskResult's taskId is the first upstream task id (§4): the dispatch polls upstream tasks/get
//     (Mcp-Name = that id) at the upstream's pollIntervalMs, bounded to 0.5–30 s, logging each status change; each new
//     key of its inputRequests is one input_request event under the gate id k.<taskId>.<seq>, the upstream key kept in
//     the event; the client's answers go upstream as tasks/update under the upstream keys; a terminal status ends the
//     task (completed logs its result, failed its error, cancelled is canceled);
//   - an InputRequiredResult answering the call (a multi round-trip request) is logged alike, one input_request per
//     inputRequests key with the round's requestState kept in the event, and once every request it listed is answered
//     the call is sent again with a new JSON-RPC id, the answers as inputResponses and the upstream's requestState.
//     The first InputRequiredResult is the first upstream id (D178: its first key is recorded as the attempt's
//     upstream_task_id), so the call is never sent afresh after it: a tunnel lost later fails the task tunnel_lost.
//
// The attempt's upstream_session column, which names a 2025-11-25 dispatch's own session, holds a modern dispatch's
// upstream task id (there is no session): a re-attach (D42) polls it again, and forward_cancel cancels it.

// maxRounds bounds the multi round-trip rounds of one call: an upstream that asks for ever fails the task
// upstream_error.
const maxRounds = 64

// modernUpstream is an input_request event's upstream object for a 2026-07-28 request (store.InputRequest.Upstream):
// its inputRequests key, and the upstream task it belongs to, or the multi round-trip round it was asked in (with the
// round's requestState on the first event of the round).
type modernUpstream struct {
	Key          string          `json:"key"`
	TaskID       string          `json:"taskId,omitempty"`
	Round        int             `json:"round,omitempty"`
	RequestState json.RawMessage `json:"requestState,omitempty"`
}

// modernRun is one 2026-07-28 dispatch. It shares the 2025-11-25 run's state and its writes (the fence, the task's
// bytes, the hold after a forwarded cancel, the failures and the terminal rows).
type modernRun struct {
	*legacyRun
	firstID   bool             // the attempt has its first upstream id
	taskKeys  map[string]int64 // the upstream task's inputRequests keys → their input_request seqs
	keyOfSeq  map[int64]string // an input_request seq → its upstream key (the task's requests)
	round     int              // the latest multi round-trip round (0: none in progress)
	roundSeqs map[int64]string // that round's input_request seqs → their upstream keys
	state     json.RawMessage  // that round's requestState, raw; nil when it gave none
	rounds    int              // rounds of this dispatch
}

// run is the dispatch: the call (or, re-attaching, the recorded state), then the follow until the task is terminal.
func (r *modernRun) run(ctx context.Context) lifecycle.DispatchResult {
	r.logged, r.upstreamID, r.sent = map[string]int64{}, map[int64]json.RawMessage{}, map[int64]bool{}
	r.taskKeys, r.keyOfSeq, r.roundSeqs = map[string]int64{}, map[int64]string{}, map[int64]string{}
	for _, seq := range r.answered {
		r.sent[seq] = true
	}
	r.interval = time.Second
	b, err := r.d.Log.PayloadBytes(ctx, r.taskID)
	if err != nil {
		return lifecycle.DispatchResult{}
	}
	r.bytes = b
	if err := r.loadModern(ctx); err != nil {
		return lifecycle.DispatchResult{}
	}
	r.firstID = r.attempt.UpstreamTaskID != ""
	switch {
	case r.attempt.UpstreamSession != "":
		r.upTask = r.attempt.UpstreamSession
		r.log.Info("re-attaching to the upstream task (2026-07-28)")
	case r.firstID:
		r.log.Info("re-attaching to a multi round-trip call (2026-07-28)", "round", r.round)
	default:
		if res, done := r.call(ctx, nil); done {
			return res
		}
	}
	return r.follow(ctx)
}

// loadModern reads the input requests the task has logged (a re-attach finds those of an earlier dispatch): the
// upstream task's keys, and the latest round with its requestState.
func (r *modernRun) loadModern(ctx context.Context) error {
	reqs, err := r.d.Log.InputRequests(ctx, r.taskID, 0)
	if err != nil {
		return err
	}
	for _, q := range reqs {
		var up modernUpstream
		if json.Unmarshal(q.Upstream, &up) != nil || up.Key == "" {
			continue
		}
		switch {
		case up.TaskID != "":
			r.taskKeys[up.Key], r.keyOfSeq[q.Seq] = q.Seq, up.Key
		case up.Round > 0:
			if up.Round > r.round {
				r.round, r.roundSeqs, r.state = up.Round, map[int64]string{}, nil
			}
			if up.Round == r.round {
				r.roundSeqs[q.Seq] = up.Key
				if len(up.RequestState) > 0 {
					r.state = up.RequestState
				}
			}
		}
	}
	return nil
}

// modernParams are params with the stored call's _meta (every modern request carries the client's per-request
// fields, §11.12).
func modernParams(work *store.MCPWork, params map[string]any) (json.RawMessage, error) {
	p, _ := mcpwire.Member(work.Request, "params")
	if meta, ok := mcpwire.Member(p, "_meta"); ok {
		params["_meta"] = meta
	}
	return json.Marshal(params)
}

// callBody is the stored tools/call to send: the client's JSON-RPC id the first time, a new one for a call sent again
// with answers (MRTR: "the JSON-RPC id MUST be different between the initial request and the retry"), with
// params.inputResponses and params.requestState set for a retry.
func (r *modernRun) callBody(answers json.RawMessage, retry bool) ([]byte, json.RawMessage, error) {
	body := r.work.Request
	id := r.work.RequestID
	if retry {
		id = json.RawMessage(strconv.Quote(r.id()))
		var err error
		if body, err = mcpwire.SetMember(body, "id", id); err != nil {
			return nil, nil, err
		}
		params, ok := mcpwire.Member(body, "params")
		if !ok || firstByte(params) != '{' {
			return nil, nil, errors.New("mcprelay: the stored tools/call has no params")
		}
		if params, err = mcpwire.SetMember(params, "inputResponses", answers); err != nil {
			return nil, nil, err
		}
		if len(r.state) > 0 {
			params, err = mcpwire.SetMember(params, "requestState", r.state)
		} else {
			params, err = mcpwire.DeleteMember(params, "requestState")
		}
		if err != nil {
			return nil, nil, err
		}
		if body, err = mcpwire.SetMember(body, "params", params); err != nil {
			return nil, nil, err
		}
	}
	return body, id, nil
}

// call sends the tools/call (answers nil: the first one; otherwise the round's answers) and handles its answer.
func (r *modernRun) call(ctx context.Context, answers json.RawMessage) (lifecycle.DispatchResult, bool) {
	body, id, err := r.callBody(answers, answers != nil)
	if err != nil {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	m, _, _, err := r.link.call(ctx, body, id, nil)
	if r.stop != nil {
		return *r.stop, true
	}
	if err != nil {
		xe := asExchange(err)
		switch {
		case xe.class == classRPC:
			return r.finishModernError(ctx, xe.raw) // §11.8 case 1: the upstream's error is the task's outcome
		case !r.firstID:
			return r.beforeFirst(ctx, err)
		case ctx.Err() != nil:
			return lifecycle.DispatchResult{}, true
		case xe.class == classTunnelLost:
			r.log.Info("the attempt's tunnel was lost sending a call again after its first upstream id", "error", xe.Error())
			return r.fail(ctx, store.ReasonTunnelLost, "")
		case xe.class == classSealed:
			return r.fail(ctx, store.ReasonSealedRejected, "")
		case xe.class == classFull, xe.class == classThrottled:
			// Refused before the upstream ran it: sent again at the next round of the follow.
			r.log.Info("the call sent again with its answers was refused for now", "error", xe.Error())
			return lifecycle.DispatchResult{}, false
		case xe.class == classUnreachable:
			// SPEC-CONFLICT (relay §6.4, §11.12): after the first upstream id a failed exchange of a running task is
			// made again for 30 s (the GetTask rule), but the call sent again with its answers may have run upstream
			// before its answer was lost, and §11.12 does not say whether it may be repeated. The closest faithful
			// behaviour: it is not repeated after an exchange that may have reached the upstream; the task fails
			// upstream_unreachable.
			return r.fail(ctx, store.ReasonUpstreamUnreachable, "")
		}
		r.log.Warn("the upstream's answer to the call failed", "error", xe.Error())
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	if err := mcpwire.CheckModernTask(m.Result); err != nil {
		r.log.Warn("strict decoding refused the upstream's answer to the call", "level", "result")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	switch rt, _ := stringMember(m.Result, "resultType"); rt {
	case "task":
		return r.created(ctx, m.Result)
	case "input_required":
		return r.inputRound(ctx, m.Result)
	}
	return r.finishModernResult(ctx, m.Result)
}

// recordFirst records the attempt's first upstream id (a CreateTaskResult's taskId, or the first InputRequiredResult's
// first key) under the fence and signals kontra.dispatched (§4, §14.1, D178).
func (r *modernRun) recordFirst(ctx context.Context, id string) bool {
	if r.firstID {
		return true
	}
	if err := r.d.Log.SetFirstUpstreamID(ctx, r.fence, id, ""); err != nil {
		r.log.Warn("the first upstream id was not recorded", "error", err)
		return false
	}
	r.firstID = true
	if r.d.Signals != nil {
		if err := r.d.Signals.SignalDispatched(context.WithoutCancel(ctx), a2a.TaskID(r.taskID.String()), r.n); err != nil {
			r.log.Warn("kontra.dispatched was not signalled", "error", err)
		}
	}
	return true
}

// created follows a CreateTaskResult (resultType "task"): its taskId the first upstream task id (or, after a multi
// round-trip call, the upstream task the attempt now polls), its status logged.
func (r *modernRun) created(ctx context.Context, result json.RawMessage) (lifecycle.DispatchResult, bool) {
	up, _ := stringMember(result, "taskId")
	status, _ := stringMember(result, "status")
	st, ok := store.StateFromMCP(status)
	if up == "" || !ok {
		r.log.Warn("the upstream's CreateTaskResult names no task or no status it defines")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	if !r.recordFirst(ctx, up) {
		return lifecycle.DispatchResult{}, true
	}
	if err := r.d.Log.SetUpstreamSession(ctx, r.fence, up); err != nil {
		return lifecycle.DispatchResult{}, true
	}
	r.upTask, r.round, r.roundSeqs, r.state = up, 0, map[int64]string{}, nil
	r.setIntervalMs(result)
	if st.Terminal() {
		// A task that ended already: its tasks/get carries the outcome.
		return lifecycle.DispatchResult{}, false
	}
	text, _ := stringMember(result, "statusMessage")
	if st == store.StateInputRequired {
		st = store.StateWorking // its requests come with tasks/get, which logs input_required with them
	}
	return r.logStatus(ctx, st, text)
}

// setIntervalMs takes the upstream's pollIntervalMs, bounded to 0.5–30 s (§11.12).
func (r *modernRun) setIntervalMs(obj json.RawMessage) {
	v, ok := mcpwire.Member(obj, "pollIntervalMs")
	if !ok {
		return
	}
	var ms float64
	if json.Unmarshal(v, &ms) != nil || ms <= 0 {
		return
	}
	lo, hi := r.d.pollBounds()
	r.interval = min(max(time.Duration(ms*float64(time.Millisecond)), lo), hi)
}

// inputRound logs an InputRequiredResult answering the call (§11.12, D178): it is the first upstream id when the
// attempt has none (its first key, or "requestState" when it lists no request); each of its inputRequests is an
// input_request event of a new round, the round's requestState kept in the round's first event; the task is
// input_required until every one is answered. One that lists no request is answered at once (the call sent again
// with its requestState).
func (r *modernRun) inputRound(ctx context.Context, result json.RawMessage) (lifecycle.DispatchResult, bool) {
	if r.rounds++; r.rounds > maxRounds {
		r.log.Warn("the upstream asked for input more times than the dispatch answers", "rounds", r.rounds)
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	reqs, _ := mcpwire.Member(result, "inputRequests")
	state, hasState := mcpwire.Member(result, "requestState")
	if hasState && firstByte(state) != '"' {
		r.log.Warn("the upstream's requestState is not a string")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	keys := mcpwire.Keys(reqs)
	if len(reqs) > 0 && firstByte(reqs) != '{' || len(keys) == 0 && !hasState {
		r.log.Warn("the upstream's InputRequiredResult lists no request and no requestState")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	first := "requestState"
	if len(keys) > 0 {
		first = keys[0]
	}
	if !r.recordFirst(ctx, first) {
		return lifecycle.DispatchResult{}, true
	}
	round := r.round + 1
	r.round, r.roundSeqs, r.state = round, map[int64]string{}, nil
	if hasState {
		r.state = bytes.Clone(state)
	}
	for i, k := range keys {
		entry, _ := mcpwire.Member(reqs, k)
		up := modernUpstream{Key: k, Round: round}
		if i == 0 && hasState {
			up.RequestState = r.state
		}
		seq, ok := r.logInput(ctx, entry, up)
		if !ok {
			if r.stop != nil {
				return *r.stop, true
			}
			return r.fail(ctx, store.ReasonUpstreamError, "")
		}
		r.roundSeqs[seq] = k
	}
	if len(keys) == 0 {
		return r.call(ctx, json.RawMessage(`{}`))
	}
	return r.logStatus(ctx, store.StateInputRequired, "")
}

// logInput logs one inputRequests entry as an input_request event (§11.12, D159): the request as clients see it, its
// JSON-RPC id the gate id k.<taskId>.<seq>, its params after §2.5; the upstream key (and its task or round) kept in
// the event, never shown. A malformed entry is false (the caller fails the task upstream_error).
func (r *modernRun) logInput(ctx context.Context, entry json.RawMessage, up modernUpstream) (int64, bool) {
	method, ok := stringMember(entry, "method")
	if !ok || method == "" {
		r.log.Warn("an upstream inputRequests entry names no method")
		return 0, false
	}
	params, hasParams := mcpwire.Member(entry, "params")
	if hasParams && firstByte(params) != '{' {
		r.log.Warn("an upstream inputRequests entry's params is not an object")
		return 0, false
	}
	params, err := mcpwire.ScrubParams(mcpwire.FromUpstream, method, params, false)
	if err != nil {
		return 0, false
	}
	upb, err := json.Marshal(up)
	if err != nil {
		return 0, false
	}
	var n int
	seq, err := r.d.Log.AppendInputRequestUpstream(context.WithoutCancel(ctx), r.fence, r.taskID, upb, func(gateID string) (json.RawMessage, error) {
		b := rpcBody(gateID, method, params)
		n = len(b) + len(upb) + 32
		if r.bytes+int64(n) > r.d.taskBytes() {
			return nil, errOverTaskBytes
		}
		return b, nil
	})
	switch {
	case err == nil:
		r.bytes += int64(n)
		r.log.Info("an input request of the upstream was logged", "seq", seq, "method", method)
		return seq, true
	case errors.Is(err, errOverTaskBytes):
		res, _ := r.fail(ctx, store.ReasonUpstreamError, "")
		r.stop = &res
	case errors.Is(err, store.ErrFenced), errors.Is(err, a2a.ErrTaskNotFound):
		r.stop = &lifecycle.DispatchResult{}
	default:
		if row, lerr := r.d.Log.Load(context.WithoutCancel(ctx), r.taskID); lerr == nil && row.State.Terminal() {
			r.stop = &lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal}
		} else {
			r.log.Warn("an upstream input request was not logged", "error", err)
		}
	}
	return 0, false
}

// follow waits for the task to end: polling the upstream task when there is one, the client's answers (from the
// log, on a wake or every 2 s) going upstream, and, while a multi round-trip call waits for its answers, the tunnel
// session checked every second (a tunnel lost after the first upstream id fails the task tunnel_lost, D178).
func (r *modernRun) follow(ctx context.Context) lifecycle.DispatchResult {
	var wake <-chan struct{}
	if r.d.Waker != nil {
		w, stop := r.d.Waker.WaitTask(ctx, r.taskID)
		defer stop()
		wake = w
	}
	poll := time.NewTimer(0)
	defer poll.Stop()
	answers := time.NewTicker(2 * time.Second)
	defer answers.Stop()
	held := time.NewTicker(time.Second)
	defer held.Stop()
	step := func() (lifecycle.DispatchResult, bool) { return r.answers(ctx) }
	if res, done := step(); done {
		return res
	}
	for {
		var res lifecycle.DispatchResult
		done := false
		select {
		case <-ctx.Done():
			return lifecycle.DispatchResult{}
		case <-poll.C:
			if r.upTask != "" {
				res, done = r.poll(ctx)
			}
			poll.Reset(r.interval)
		case <-wake:
			res, done = step()
		case <-answers.C:
			res, done = step()
		case <-held.C:
			if r.upTask == "" && r.round > 0 && !r.link.held() {
				r.log.Info("the attempt's tunnel session is gone while a multi round-trip call waits for its answers")
				res, done = r.fail(ctx, store.ReasonTunnelLost, "")
			}
		}
		if r.stop != nil {
			return *r.stop
		}
		if done {
			return res
		}
	}
}

// poll is one upstream tasks/get (§11.12): a changed status is logged; new inputRequests keys are logged (input
// _required); a terminal status ends the task. -32602 or a 404 (the upstream no longer knows its task) fails it
// upstream_error, "upstream task expired"; a lost tunnel tunnel_lost; an upstream that cannot be reached for 30 s
// upstream_unreachable (pollFailed).
func (r *modernRun) poll(ctx context.Context) (lifecycle.DispatchResult, bool) {
	params, err := modernParams(r.work, map[string]any{"taskId": r.upTask})
	if err != nil {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	m, err := r.link.request(pctx, r.id(), "tasks/get", params, nil)
	cancel()
	if err != nil {
		return r.pollFailed(ctx, err)
	}
	r.failing = time.Time{}
	if err := mcpwire.CheckModernTask(m.Result); err != nil {
		r.log.Warn("strict decoding refused the upstream's tasks/get answer", "level", "result")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	status, _ := stringMember(m.Result, "status")
	st, ok := store.StateFromMCP(status)
	if !ok {
		r.log.Warn("the upstream's tasks/get answered a status the extension does not define")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	r.setIntervalMs(m.Result)
	text, _ := stringMember(m.Result, "statusMessage")
	switch st {
	case store.StateCompleted:
		res, ok := mcpwire.Member(m.Result, "result")
		if !ok || firstByte(res) != '{' {
			r.log.Warn("the upstream's completed task carries no result")
			return r.fail(ctx, store.ReasonUpstreamError, "")
		}
		return r.finishModernResult(ctx, res)
	case store.StateFailed:
		e, ok := mcpwire.Member(m.Result, "error")
		if !ok {
			return r.fail(ctx, store.ReasonUpstreamError, "")
		}
		return r.finishModernError(ctx, e)
	case store.StateCanceled:
		return r.cancelledUpstream(ctx)
	case store.StateInputRequired:
		reqs, _ := mcpwire.Member(m.Result, "inputRequests")
		if len(reqs) > 0 && firstByte(reqs) != '{' {
			return r.fail(ctx, store.ReasonUpstreamError, "")
		}
		for _, k := range mcpwire.Keys(reqs) {
			if _, done := r.taskKeys[k]; done {
				continue // a key names one request for the task's whole life (ext-tasks, "Task Update Requests")
			}
			entry, _ := mcpwire.Member(reqs, k)
			seq, ok := r.logInput(ctx, entry, modernUpstream{Key: k, TaskID: r.upTask})
			if !ok {
				if r.stop != nil {
					return *r.stop, true
				}
				return r.fail(ctx, store.ReasonUpstreamError, "")
			}
			r.taskKeys[k], r.keyOfSeq[seq] = seq, k
		}
	}
	return r.logStatus(ctx, st, text)
}

// answers takes the client's answers from the log: an upstream task's go upstream as tasks/update (sendTaskAnswers);
// a multi round-trip round's, once every request of the round is answered, are the call sent again (§11.12). A task
// that ended meanwhile (the workflow's cancel or timeout) ends the run.
func (r *modernRun) answers(ctx context.Context) (lifecycle.DispatchResult, bool) {
	if r.upTask != "" {
		r.sendTaskAnswers(ctx)
		if r.stop != nil {
			return *r.stop, true
		}
		return lifecycle.DispatchResult{}, false
	}
	if r.round == 0 {
		return lifecycle.DispatchResult{}, false
	}
	row, err := r.d.Log.Load(ctx, r.taskID)
	if err != nil {
		return lifecycle.DispatchResult{}, false
	}
	if row.State.Terminal() {
		return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal}, true
	}
	resps, err := r.d.Log.InputResponses(ctx, r.taskID, 0)
	if err != nil {
		return lifecycle.DispatchResult{}, false
	}
	got := map[int64]json.RawMessage{}
	for _, a := range resps {
		if _, ok := r.roundSeqs[a.RequestSeq]; ok {
			got[a.RequestSeq] = a.Response
		}
	}
	if len(got) < len(r.roundSeqs) {
		return lifecycle.DispatchResult{}, false
	}
	// Every request of the round is answered: the call is sent again with the answers, in the round's key order. An
	// answer that is a JSON-RPC error (a 2025-11-25 reader's) has no inputResponses form; its key is left out, and the
	// upstream asks again for what it still needs (MRTR, "Error Handling").
	var out bytes.Buffer
	out.WriteByte('{')
	n := 0
	for seq, key := range orderedSeqs(r.roundSeqs) {
		res, ok := mcpwire.Member(got[seq], "result")
		if !ok {
			continue
		}
		if n > 0 {
			out.WriteByte(',')
		}
		kb, _ := json.Marshal(key)
		out.Write(kb)
		out.WriteByte(':')
		out.Write(res)
		n++
	}
	out.WriteByte('}')
	if res, done := r.logStatus(ctx, store.StateWorking, ""); done {
		return res, true
	}
	// The call's answer starts the next round, makes an upstream task or ends the task; a call refused before the
	// upstream ran it keeps this round, and is sent again at the next wake.
	return r.call(ctx, out.Bytes())
}

// orderedSeqs iterates a round's seqs in order (the order the upstream listed its requests in).
func orderedSeqs(m map[int64]string) func(func(int64, string) bool) {
	return func(yield func(int64, string) bool) {
		seqs := make([]int64, 0, len(m))
		for s := range m {
			seqs = append(seqs, s)
		}
		for i := 1; i < len(seqs); i++ {
			for j := i; j > 0 && seqs[j] < seqs[j-1]; j-- {
				seqs[j], seqs[j-1] = seqs[j-1], seqs[j]
			}
		}
		for _, s := range seqs {
			if !yield(s, m[s]) {
				return
			}
		}
	}
}

// sendTaskAnswers writes upstream, as one tasks/update under the upstream keys (§11.12), every answer to the upstream
// task's requests the task has logged and no dispatch of the attempt has written; each one written (or refused by
// the upstream) is recorded on the work item under the fence (store.Log.MarkAnswerDelivered), so a re-attach (D42)
// does not write it again. An answer that is a JSON-RPC error has no inputResponses form and is recorded without
// being sent (the upstream may ask again). A lost tunnel fails the task tunnel_lost.
func (r *modernRun) sendTaskAnswers(ctx context.Context) {
	resps, err := r.d.Log.InputResponses(ctx, r.taskID, 0)
	if err != nil {
		return
	}
	var (
		out  bytes.Buffer
		seqs []int64
		skip []int64
	)
	out.WriteByte('{')
	for _, a := range resps {
		if r.sent[a.Seq] {
			continue
		}
		key, ok := r.keyOfSeq[a.RequestSeq]
		if !ok {
			r.sent[a.Seq] = true // an answer to a request of no upstream task (a round's: its call carries it)
			continue
		}
		res, ok := mcpwire.Member(a.Response, "result")
		if !ok {
			skip = append(skip, a.Seq)
			continue
		}
		if len(seqs) > 0 {
			out.WriteByte(',')
		}
		kb, _ := json.Marshal(key)
		out.Write(kb)
		out.WriteByte(':')
		out.Write(res)
		seqs = append(seqs, a.Seq)
	}
	out.WriteByte('}')
	if len(seqs) > 0 {
		params, err := modernParams(r.work, map[string]any{"taskId": r.upTask, "inputResponses": json.RawMessage(out.Bytes())})
		if err != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = r.link.request(pctx, r.id(), "tasks/update", params, nil)
		cancel()
		if err != nil {
			switch xe := asExchange(err); xe.class {
			case classUnreachable, classFull, classThrottled:
				return // written at the next wake
			case classTunnelLost:
				r.log.Info("the attempt's tunnel exchange was lost writing answers upstream", "error", xe.Error())
				res, _ := r.fail(ctx, store.ReasonTunnelLost, "")
				r.stop = &res
				return
			}
			r.log.Warn("the upstream refused the answers to its task's input requests", "error", err)
		} else {
			r.log.Info("answers to input requests were written upstream", "answers", len(seqs))
		}
	}
	for _, seq := range append(seqs, skip...) {
		r.sent[seq] = true
		if err := r.d.Log.MarkAnswerDelivered(context.WithoutCancel(ctx), r.fence, r.workItem, seq); err != nil {
			if errors.Is(err, store.ErrFenced) {
				r.stop = &lifecycle.DispatchResult{}
				return
			}
			r.log.Warn("an answer's delivery was not recorded; a re-attach may write it again", "seq", seq, "error", err)
		}
	}
}

// finishModernResult logs the upstream's CallToolResult as the task's result (§11.8 case 1, §11.12): after §2.5, the
// task completed, or failed for a top-level isError true as for a 2025-11-25 upstream (§11.8; a modern reader is
// shown it completed, D159), its calls row in the same statement.
func (r *modernRun) finishModernResult(ctx context.Context, result json.RawMessage) (lifecycle.DispatchResult, bool) {
	res, err := mcpwire.ScrubResult(mcpwire.FromUpstream, mcpwire.MethodToolsCall, result, false)
	if err != nil {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	isErr := false
	if v, ok := mcpwire.Member(res, "isError"); ok && bytes.Equal(bytes.TrimSpace(v), []byte("true")) {
		isErr = true
	}
	state := store.StateCompleted
	if isErr {
		state = store.StateFailed
	}
	err = r.append(ctx, upstreamWrite, func(row *store.TaskRow) (store.Write, error) {
		w, err := store.MCPResultWrite(row, state, "result", res, nil)
		if err != nil {
			return store.Write{}, err
		}
		w.Call, err = r.d.Log.MCPCall(ctx, r.taskID, isErr, int64(len(res)))
		return w, err
	})
	if errors.Is(err, errOverTaskBytes) {
		r.log.Warn("the upstream's result would carry the task past limits.task_bytes; it is not logged")
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	return r.ended(err, "")
}

// finishModernError logs the upstream's JSON-RPC error (answering the call, or a failed task's error) as the task's
// result: failed upstream_error, the error verbatim (§11.8 case 1). An error that is not a JSON-RPC error object is a
// failure the gate writes (upstream_error).
func (r *modernRun) finishModernError(ctx context.Context, raw json.RawMessage) (lifecycle.DispatchResult, bool) {
	if c, ok := mcpwire.Member(raw, "code"); !ok || firstByte(c) == '"' {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	if m, ok := mcpwire.Member(raw, "message"); !ok || firstByte(m) != '"' {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	err := r.append(ctx, upstreamWrite, func(row *store.TaskRow) (store.Write, error) {
		w, err := store.MCPResultWrite(row, store.StateFailed, "error", raw, map[string]any{store.MetaFailure: string(store.ReasonUpstreamError)})
		if err != nil {
			return store.Write{}, err
		}
		w.Call, err = r.d.Log.MCPCall(ctx, r.taskID, true, int64(len(raw)))
		return w, err
	})
	if errors.Is(err, errOverTaskBytes) {
		return r.fail(ctx, store.ReasonUpstreamError, "")
	}
	return r.ended(err, string(store.ReasonUpstreamError))
}

// modernHeaders sets a 2026-07-28 request's headers (2026-07-28 Streamable HTTP, "Request Metadata"):
// MCP-Protocol-Version, Mcp-Method and Mcp-Name from the body (the Base64 sentinel when the value is not plain ASCII),
// and for a tools/call the client's Mcp-Param-* headers.
func (k *mcpLink) modernHeaders(h http.Header, body []byte) {
	h.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20260728)
	if len(body) == 0 {
		return
	}
	m, err := mcpwire.Parse(body, mcpwire.FromClient)
	if err != nil || m.Method == "" {
		return
	}
	h.Set(mcpwire.HeaderMethod, m.Method)
	if src, ok := mcpwire.NameSource(m.Method); ok {
		if v, ok := stringMember(m.Params, src); ok {
			h.Set(mcpwire.HeaderName, mcpwire.EncodeHeaderValue(v))
		}
	}
	if m.Method == mcpwire.MethodToolsCall {
		for name, vals := range k.params {
			h[http.CanonicalHeaderKey(name)] = append([]string(nil), vals...)
		}
	}
}
