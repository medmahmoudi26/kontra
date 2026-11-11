package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/a2avalid"
	"kontra.sh/kontra/internal/store"
)

// The A2A wire on the console (OpenSpec change console-a2a-protocol-view; D355-D360): each event's origin and the A2A
// object the caller received for it, a task's and a context's timeline (the events, and the calls on both legs that
// carried none), one exchange with its bodies and frames, and a service's two cards side by side, each with its
// validation against the pinned specification (a2avalid). Everything here is read from what the gate recorded: the
// console never infers an origin.

// ---------------------------------------------------------------- events

// callerType is the A2A object a logged event is for the caller: the StreamResponse member it holds (D360).
func callerType(payload []byte) string {
	var sr map[string]json.RawMessage
	if json.Unmarshal(payload, &sr) != nil {
		return ""
	}
	for member, t := range map[string]string{"task": store.TypeTask, "message": store.TypeMessage,
		"statusUpdate": store.TypeStatusUpdate, "artifactUpdate": store.TypeArtifactUpdate} {
		if _, ok := sr[member]; ok {
			return t
		}
	}
	return ""
}

// eventItem is one logged event as a timeline (and the events stream) shows it.
type eventItem struct {
	Type        string           `json:"type"`    // "event"
	TaskID      *string          `json:"task_id"` // null: a provisional record's (D362), which its caller never saw as a task
	Seq         int64            `json:"seq"`
	Kind        string           `json:"kind"`
	At          time.Time        `json:"at"`
	StoredApart bool             `json:"stored_apart"`
	Origin      json.RawMessage  `json:"origin"`      // null: written before the origin was recorded
	CallerType  string           `json:"caller_type"` // the A2A object the caller received for it
	Validation  *a2avalid.Result `json:"validation,omitempty"`
	Payload     json.RawMessage  `json:"payload,omitempty"`
}

func newEventItem(ev store.Event) *eventItem {
	id := ev.TaskID.String()
	it := &eventItem{Type: "event", TaskID: &id, Seq: ev.Seq, Kind: string(ev.Kind), At: ev.CreatedAt,
		StoredApart: ev.PayloadRef != "", Origin: ev.Origin, Payload: ev.Payload}
	if len(it.Origin) == 0 {
		it.Origin = json.RawMessage("null")
	}
	if len(ev.Payload) > 0 && ev.Kind != store.KindMetadata {
		it.CallerType = callerType(ev.Payload)
		if !it.StoredApart && it.CallerType != "" {
			v := a2avalid.Event(ev.Payload)
			it.Validation = &v
		}
	}
	return it
}

// provisional makes it an event of a provisional record (D362): the caller was sent no task, so it names none, and the
// caller received only the callee's Message that ended a message-only interaction (D5 logs it as a completed status),
// nothing for any other row.
func (it *eventItem) provisional() {
	it.TaskID = nil
	var o store.Origin
	if json.Unmarshal(it.Origin, &o) == nil && o.Actor == store.ActorCallee && o.Type == store.TypeMessage {
		it.CallerType = store.TypeMessage
		return
	}
	it.CallerType = ""
}

// ---------------------------------------------------------------- exchanges

// exchangeItem is one exchange in a timeline: what it was, never its bodies (GET /v1/exchanges/{id} has them).
type exchangeItem struct {
	Type         string    `json:"type"` // "exchange"
	ID           string    `json:"id"`
	At           time.Time `json:"at"`
	Leg          string    `json:"leg"`
	Method       string    `json:"method"`
	Binding      string    `json:"binding"`
	A2AVersion   *string   `json:"a2a_version"`
	TaskID       *string   `json:"task_id"`        // the gate task it names or works for
	CalleeTaskID *string   `json:"callee_task_id"` // the callee leg's agent task id
	ContextID    *string   `json:"context_id"`
	HTTPStatus   int       `json:"http_status"`
	RPCCode      *int      `json:"rpc_code"`
	Reason       *string   `json:"reason"`
	DurationMS   int       `json:"duration_ms"`
	Bodies       bool      `json:"bodies"` // its bodies or frames were kept
	Failed       bool      `json:"failed"`
}

const exchangeItemColumns = `x.id, x.at, x.leg, x.method, x.binding, x.a2a_version, coalesce(x.task_id, x.gate_task_id), x.callee_task_id,
	x.context_id, x.http_status, x.rpc_code, x.reason, x.duration_ms,
	x.request_body is not null or x.response_body is not null or exists (select 1 from kontra.a2a_exchange_frames f where f.exchange_id = x.id)`

func scanExchangeItem(row pgx.Row) (*exchangeItem, error) {
	var (
		it     exchangeItem
		id     uuid.UUID
		task   *uuid.UUID
		status int16
	)
	if err := row.Scan(&id, &it.At, &it.Leg, &it.Method, &it.Binding, &it.A2AVersion, &task, &it.CalleeTaskID, &it.ContextID,
		&status, &it.RPCCode, &it.Reason, &it.DurationMS, &it.Bodies); err != nil {
		return nil, err
	}
	it.Type, it.ID, it.HTTPStatus = "exchange", id.String(), int(status)
	if task != nil {
		s := task.String()
		it.TaskID = &s
	}
	it.Failed = it.HTTPStatus >= 400 || it.RPCCode != nil || it.Reason != nil
	return &it, nil
}

// ---------------------------------------------------------------- timelines

// opening is how a context was opened (D3, D357): by the caller sending its contextId, or by the gate assigning one in
// its answer; null when the context's first event has no origin (written before it was recorded).
type opening struct {
	ContextID string `json:"context_id"`
	Assigned  *bool  `json:"assigned"` // true: the gate minted it; false: the caller sent it; null: not recorded
	TaskID    string `json:"task_id"`  // the context's first task
}

// calleeContext is the agent's own context for a gate context, on one session or backend (spec §4).
type calleeContext struct {
	ContextID string   `json:"context_id"`
	Backend   string   `json:"backend"` // tunnel | direct
	Session   *string  `json:"session"` // the tunnel session, for a tunnel backend
	TaskIDs   []string `json:"task_ids"`
}

// timelineMax bounds a timeline's items.
const timelineMax = 2000

// timeline is the items of tasks (events, and the exchanges that carried none) in time order, then seq.
func (c *Console) timeline(ctx context.Context, tasks []uuid.UUID) ([]any, bool, error) {
	hidden, err := c.provisional(ctx, tasks)
	if err != nil {
		return nil, false, err
	}
	var events []*eventItem
	carried := map[string]bool{}
	for _, id := range tasks {
		for after := int64(0); ; {
			evs, err := c.d.Log.Events(ctx, id, after, 1000)
			if err != nil {
				return nil, false, err
			}
			for _, ev := range evs {
				it := newEventItem(ev)
				if hidden[id] {
					it.provisional()
				}
				var o store.Origin
				if json.Unmarshal(ev.Origin, &o) == nil && o.ExchangeID != "" {
					carried[o.ExchangeID] = true
				}
				events = append(events, it)
				after = ev.Seq
			}
			if len(evs) < 1000 || len(events) > timelineMax {
				break
			}
		}
	}
	rows, err := c.d.DB.Query(ctx, `select `+exchangeItemColumns+` from kontra.a2a_exchanges x
		where (x.task_id = any($1) and x.leg = 'caller') or (x.gate_task_id = any($1) and x.leg = 'callee')
		order by x.at, x.id limit $2`, tasks, timelineMax)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var items []any
	type timed struct {
		at  time.Time
		seq int64
		v   any
	}
	var all []timed
	for _, e := range events {
		all = append(all, timed{e.At, e.Seq, e})
	}
	for rows.Next() {
		x, err := scanExchangeItem(rows)
		if err != nil {
			return nil, false, err
		}
		if carried[x.ID] {
			continue // the event it carried names it
		}
		if x.TaskID != nil && hidden[uuid.MustParse(*x.TaskID)] {
			x.TaskID = nil // a provisional record's id, which the caller was never sent (D362)
		}
		all = append(all, timed{x.At, 0, x})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.Before(all[j].at)
		}
		return all[i].seq < all[j].seq
	})
	more := len(all) > timelineMax
	for i, t := range all {
		if i == timelineMax {
			break
		}
		items = append(items, t.v)
	}
	if items == nil {
		items = []any{}
	}
	return items, more, nil
}

// provisional is the records among tasks that are not exposed (D362).
func (c *Console) provisional(ctx context.Context, tasks []uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := c.d.DB.Query(ctx, `select id from kontra.tasks where id = any($1) and exposed_at is null`, tasks)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// contextOf is a context's opening and its callee contexts, for its tasks oldest first.
func (c *Console) contextOf(ctx context.Context, tasks []uuid.UUID, contextID string) (*opening, []*calleeContext, error) {
	op := &opening{ContextID: contextID}
	if len(tasks) > 0 {
		op.TaskID = tasks[0].String()
		var raw []byte
		err := c.d.DB.QueryRow(ctx, `select origin::text from kontra.task_events where task_id = $1 and seq = 1`, tasks[0]).Scan(&raw)
		if err != nil && !isNoRows(err) {
			return nil, nil, err
		}
		var o store.Origin
		if len(raw) > 0 && json.Unmarshal(raw, &o) == nil {
			op.Assigned = o.ContextAssigned
		}
	}
	rows, err := c.d.DB.Query(ctx, `select upstream_context_id, backend, tunnel_session, task_id from kontra.attempts
		where task_id = any($1) and upstream_context_id is not null order by started_at`, tasks)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*calleeContext
	by := map[string]*calleeContext{}
	for rows.Next() {
		var (
			cc      calleeContext
			session *uuid.UUID
			task    uuid.UUID
		)
		if err := rows.Scan(&cc.ContextID, &cc.Backend, &session, &task); err != nil {
			return nil, nil, err
		}
		if session != nil {
			s := session.String()
			cc.Session = &s
		}
		key := cc.ContextID + "|" + cc.Backend + "|" + ptr(cc.Session)
		if have, ok := by[key]; ok {
			have.TaskIDs = appendOnce(have.TaskIDs, task.String())
			continue
		}
		cc.TaskIDs = []string{task.String()}
		by[key] = &cc
		out = append(out, &cc)
	}
	if out == nil {
		out = []*calleeContext{}
	}
	return op, out, rows.Err()
}

func ptr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func appendOnce(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// GET /v1/tasks/{task_id}/timeline: the task's items, its context's opening and its callee ids.
func (c *Console) taskTimeline(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	id, err := taskIDOf(r)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	t, err := c.loadTask(ctx, id, false)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	items, more, err := c.timeline(ctx, []uuid.UUID{id})
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	tasks, err := c.contextTasks(ctx, t.Service, uuid.MustParse(t.CallerID), t.ContextID)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	op, callee, err := c.contextOf(ctx, tasks, t.ContextID)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	c.answer(w, r, map[string]any{"task": t, "opening": op, "callee_contexts": callee, "items": items, "more": more}, nil)
}

// contextTasks are a context's task ids, oldest first.
func (c *Console) contextTasks(ctx context.Context, service string, buyer uuid.UUID, contextID string) ([]uuid.UUID, error) {
	rows, err := c.d.DB.Query(ctx, `select t.id from kontra.tasks t
		where t.tenant_id = (select id from kontra.tenants where slug = 'default') -- tasks_by_context
		  and t.service = $1 and t.buyer_id = $2 and t.context_id = $3 order by t.id limit 1000`, service, buyer, contextID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// GET /v1/contexts/{service}/{caller}/{context}/timeline: every interaction of the context, in time order.
func (c *Console) contextTimeline(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	buyer, err := uuid.Parse(r.PathValue("caller"))
	if err != nil {
		c.answer(w, r, nil, fail(http.StatusNotFound, "conversation_not_found", "no such context"))
		return
	}
	contextID := r.PathValue("context")
	tasks, err := c.contextTasks(ctx, r.PathValue("service"), buyer, contextID)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	if len(tasks) == 0 {
		c.answer(w, r, nil, fail(http.StatusNotFound, "conversation_not_found", "no such context"))
		return
	}
	items, more, err := c.timeline(ctx, tasks)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	op, callee, err := c.contextOf(ctx, tasks, contextID)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	hidden, err := c.provisional(ctx, tasks)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	ids := []string{} // the context's tasks: a message-only interaction is none (D362)
	for _, t := range tasks {
		if !hidden[t] {
			ids = append(ids, t.String())
		}
	}
	if hidden[uuid.MustParse(op.TaskID)] {
		op.TaskID = ""
	}
	for _, cc := range callee {
		kept := []string{}
		for _, t := range cc.TaskIDs {
			if !hidden[uuid.MustParse(t)] {
				kept = append(kept, t)
			}
		}
		cc.TaskIDs = kept
	}
	c.answer(w, r, map[string]any{"service": r.PathValue("service"), "caller": buyer.String(), "context_id": contextID,
		"task_ids": ids, "opening": op, "callee_contexts": callee, "items": items, "more": more}, nil)
}

// ---------------------------------------------------------------- GET /v1/exchanges/{id}

// body is a kept body as the console shows it: text when it is UTF-8, else base64; with its full size, whether it
// was cut, and its validation.
type body struct {
	Text       *string          `json:"text"`
	Base64     *string          `json:"base64"`
	Size       int64            `json:"size"`
	Truncated  bool             `json:"truncated"`
	Validation *a2avalid.Result `json:"validation,omitempty"`
}

func newBody(data []byte, size *int64, truncated bool) *body {
	if data == nil {
		return nil
	}
	b := &body{Truncated: truncated, Size: int64(len(data))}
	if size != nil {
		b.Size = *size
	}
	if utf8.Valid(data) {
		s := string(data)
		b.Text = &s
	} else {
		s := base64.StdEncoding.EncodeToString(data)
		b.Base64 = &s
	}
	return b
}

type frameView struct {
	N          int              `json:"n"`
	OffsetMS   int              `json:"offset_ms"`
	SSEID      *string          `json:"sse_id"`
	Data       *body            `json:"data"`
	Validation *a2avalid.Result `json:"validation"`
}

type exchangeDetail struct {
	exchangeItem
	Service       string       `json:"service"`
	Principal     *string      `json:"principal"`
	Via           *string      `json:"via"`
	BytesIn       int64        `json:"bytes_in"`
	BytesOut      int64        `json:"bytes_out"`
	Request       *body        `json:"request"`
	Response      *body        `json:"response"`
	Frames        []*frameView `json:"frames"`
	FramesDropped int          `json:"frames_dropped"`
	BodiesKept    bool         `json:"bodies_kept"` // the gate kept this exchange's bodies (traffic.bodies was on)
}

func (c *Console) exchange(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		c.answer(w, r, nil, errNoExchange)
		return
	}
	var (
		d                   exchangeDetail
		req, resp           []byte
		reqSize, respSize   *int64
		reqTrunc, respTrunc bool
	)
	row := c.d.DB.QueryRow(ctx, `select `+exchangeItemColumns+`, x.service, x.principal, x.via, x.bytes_in, x.bytes_out,
		x.request_body, x.request_size, x.request_truncated, x.response_body, x.response_size, x.response_truncated,
		x.frames_dropped from kontra.a2a_exchanges x where x.id = $1`, id)
	var (
		xid    uuid.UUID
		task   *uuid.UUID
		status int16
	)
	it := &d.exchangeItem
	err = row.Scan(&xid, &it.At, &it.Leg, &it.Method, &it.Binding, &it.A2AVersion, &task, &it.CalleeTaskID, &it.ContextID,
		&status, &it.RPCCode, &it.Reason, &it.DurationMS, &it.Bodies, &d.Service, &d.Principal, &d.Via, &d.BytesIn, &d.BytesOut,
		&req, &reqSize, &reqTrunc, &resp, &respSize, &respTrunc, &d.FramesDropped)
	if err != nil {
		if isNoRows(err) {
			err = errNoExchange
		}
		c.answer(w, r, nil, err)
		return
	}
	it.Type, it.ID, it.HTTPStatus = "exchange", xid.String(), int(status)
	if task != nil {
		s := task.String()
		it.TaskID = &s
	}
	it.Failed = it.HTTPStatus >= 400 || it.RPCCode != nil || it.Reason != nil
	version := ""
	if it.A2AVersion != nil {
		version = *it.A2AVersion
	}
	d.Request = newBody(req, reqSize, reqTrunc)
	if d.Request != nil && isA2AMethod(it.Method) {
		v := a2avalid.Request(version, it.Binding, it.Method, req, reqTrunc)
		d.Request.Validation = &v
	}
	d.Response = newBody(resp, respSize, respTrunc)
	if d.Response != nil && isA2AMethod(it.Method) {
		v := a2avalid.Response(version, it.Binding, it.Method, resp, respTrunc)
		d.Response.Validation = &v
	} else if d.Response != nil && it.Method == "card" || d.Response != nil && it.Method == "upstream_card" {
		v := a2avalid.Card(resp, cardVersion(resp))
		d.Response.Validation = &v
	}
	rows, err := c.d.DB.Query(ctx, `select n, offset_ms, sse_id, data, size, truncated from kontra.a2a_exchange_frames
		where exchange_id = $1 order by n limit 2000`, id)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	defer rows.Close()
	d.Frames = []*frameView{}
	for rows.Next() {
		var (
			f     frameView
			data  []byte
			size  int64
			trunc bool
		)
		if err := rows.Scan(&f.N, &f.OffsetMS, &f.SSEID, &data, &size, &trunc); err != nil {
			c.answer(w, r, nil, err)
			return
		}
		f.Data = newBody(data, &size, trunc)
		if isA2AMethod(it.Method) {
			v := a2avalid.Frame(version, it.Binding, it.Method, data, trunc)
			f.Validation = &v
		}
		d.Frames = append(d.Frames, &f)
	}
	if err := rows.Err(); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	d.BodiesKept = d.Request != nil || d.Response != nil || len(d.Frames) > 0
	c.answer(w, r, d, nil)
}

var errNoExchange = fail(http.StatusNotFound, "exchange_not_found", "no such exchange: it was not recorded, or retention deleted it")

func isA2AMethod(m string) bool {
	switch m {
	case "card", "upstream_card", "seller_jwks", "unknown", "":
		return false
	}
	return true
}

// cardVersion is the A2A version a card says it speaks: protocolVersion (0.3), else its interfaces' (1.0).
func cardVersion(raw []byte) string {
	var k struct {
		ProtocolVersion     string `json:"protocolVersion"`
		SupportedInterfaces []struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"supportedInterfaces"`
	}
	if json.Unmarshal(raw, &k) != nil {
		return ""
	}
	if len(k.SupportedInterfaces) > 0 && k.SupportedInterfaces[0].ProtocolVersion != "" {
		return k.SupportedInterfaces[0].ProtocolVersion
	}
	return k.ProtocolVersion
}

// ---------------------------------------------------------------- GET /v1/services/{name}/cards

// cardSide is one of a service's two cards: the callee's as fetched, or the gate's served card.
type cardSide struct {
	Card       json.RawMessage  `json:"card"`
	Version    string           `json:"version"`
	Validation *a2avalid.Result `json:"validation"`
}

// The members the gate sets or rewrites on a served card (spec §3): a path whose value differs from the callee's is
// listed in gate_set; these are the ones it may set even when equal.
func (c *Console) serviceCards(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	var (
		kind           string
		served, callee []byte
	)
	err := c.d.DB.QueryRow(ctx, `select s.kind, s.card::text, s.upstream_card from kontra.services s where s.name = $1
		order by s.disabled_at is not null limit 1`, r.PathValue("name")).Scan(&kind, &served, &callee)
	if err != nil {
		if isNoRows(err) {
			err = errNoService
		}
		c.answer(w, r, nil, err)
		return
	}
	if kind != "a2a" {
		c.answer(w, r, nil, fail(http.StatusNotFound, "service_not_found", "an MCP server has no agent card"))
		return
	}
	side := func(raw []byte, version string) *cardSide {
		if len(raw) == 0 {
			return nil
		}
		s := &cardSide{Version: a2avalid.Normalize(version)}
		if json.Valid(raw) {
			s.Card = json.RawMessage(raw)
		} else {
			q, _ := json.Marshal(string(raw))
			s.Card = q
		}
		v := a2avalid.Card(raw, version)
		s.Validation = &v
		return s
	}
	out := map[string]any{"name": r.PathValue("name"), "callee": side(callee, cardVersion(callee)), "served": side(served, a2avalid.V10),
		"gate_set": gateSet(callee, served)}
	c.answer(w, r, out, nil)
}

// gateSet is the JSON paths of the served card whose value is not the callee's (absent there, or different), down to
// the members of capabilities: what the gate set or rewrote.
func gateSet(callee, served []byte) []string {
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(callee, &a)
	if json.Unmarshal(served, &b) != nil {
		return []string{}
	}
	out := []string{}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "capabilities" {
			var ca, cb map[string]json.RawMessage
			_ = json.Unmarshal(a[k], &ca)
			_ = json.Unmarshal(b[k], &cb)
			sub := make([]string, 0, len(cb))
			for kk := range cb {
				sub = append(sub, kk)
			}
			sort.Strings(sub)
			for _, kk := range sub {
				if !sameJSON(ca[kk], cb[kk]) {
					out = append(out, "$.capabilities."+kk)
				}
			}
			continue
		}
		if !sameJSON(a[k], b[k]) {
			out = append(out, "$."+k)
		}
	}
	return out
}

func sameJSON(x, y json.RawMessage) bool {
	if len(x) == 0 || len(y) == 0 {
		return len(x) == len(y)
	}
	var a, b any
	if json.Unmarshal(x, &a) != nil || json.Unmarshal(y, &b) != nil {
		return string(x) == string(y)
	}
	ax, _ := json.Marshal(a)
	bx, _ := json.Marshal(b)
	return string(ax) == string(bx)
}
