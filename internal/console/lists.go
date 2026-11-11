package console

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- conversations

// conversationView is one conversation: the tasks of one caller in one context of one service (a context is scoped by
// caller, spec §4), newest activity first.
type conversationView struct {
	Service   string    `json:"service"`
	Caller    string    `json:"caller"`    // the buyer id: the conversation's address
	Principal string    `json:"principal"` // its latest task's principal, for display
	Via       string    `json:"via"`
	ContextID string    `json:"context_id"`
	Tasks     int       `json:"tasks"`
	State     string    `json:"state"` // its latest task's
	LastAt    time.Time `json:"last_at"`
	FirstAt   time.Time `json:"first_at"`
}

// convCursor is a conversations page's keyset: (last_at, service, caller, context) of the page's last row.
type convCursor struct {
	At      time.Time `json:"a"`
	Service string    `json:"s"`
	Caller  uuid.UUID `json:"b"`
	Context string    `json:"c"`
}

func encodeCursor(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, v) != nil {
		return fail(http.StatusUnprocessableEntity, "invalid_request", "before: not a cursor of this list")
	}
	return nil
}

func (c *Console) conversations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	limit, err := limitOf(r)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var cur *convCursor
	if s := r.URL.Query().Get("before"); s != "" {
		cur = &convCursor{}
		if err := decodeCursor(s, cur); err != nil {
			c.answer(w, r, nil, err)
			return
		}
	}
	var (
		at              *time.Time
		svc, ctxID      string
		buyer           *uuid.UUID
		serviceFilterOn = r.URL.Query().Get("service")
	)
	if cur != nil {
		at, svc, buyer, ctxID = &cur.At, cur.Service, &cur.Caller, cur.Context
	}
	rows, err := c.d.DB.Query(ctx, `
		select service, buyer_id, principal, via, context_id, n, state, last_at, first_at from (
			select t.service, t.buyer_id, t.context_id, count(*) as n, max(t.status_at) as last_at, min(t.created_at) as first_at,
			       (array_agg(t.state order by t.id desc))[1] as state, (array_agg(t.principal order by t.id desc))[1] as principal,
			       (array_agg(t.via order by t.id desc))[1] as via
			  from kontra.tasks t
			 where ($1::text = '' or t.service = $1)
			 group by t.tenant_id, t.service, t.buyer_id, t.context_id) g
		 where $2::timestamptz is null or (last_at, service, buyer_id, context_id) < ($2, $3::text, $4::uuid, $5::text)
		 order by last_at desc, service desc, buyer_id desc, context_id desc
		 limit $6`, serviceFilterOn, at, svc, buyer, ctxID, limit+1)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	defer rows.Close()
	items := []*conversationView{}
	var lastBuyer uuid.UUID
	for rows.Next() {
		var (
			v     conversationView
			buyer uuid.UUID
		)
		if err := rows.Scan(&v.Service, &buyer, &v.Principal, &v.Via, &v.ContextID, &v.Tasks, &v.State, &v.LastAt, &v.FirstAt); err != nil {
			c.answer(w, r, nil, err)
			return
		}
		v.Caller = buyer.String()
		if len(items) < limit {
			lastBuyer = buyer
		}
		items = append(items, &v)
	}
	if err := rows.Err(); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		l := items[limit-1]
		n := encodeCursor(convCursor{At: l.LastAt, Service: l.Service, Caller: lastBuyer, Context: l.ContextID})
		next = &n
	}
	c.answer(w, r, map[string]any{"items": items, "next": next}, nil)
}

// conversation is one conversation's tasks, oldest first (read through tasks_by_context).
func (c *Console) conversation(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	buyer, err := uuid.Parse(r.PathValue("caller"))
	if err != nil {
		c.answer(w, r, nil, fail(http.StatusNotFound, "conversation_not_found", "no such conversation"))
		return
	}
	rows, err := c.d.DB.Query(ctx, `select `+taskColumns+taskFrom+`
		where t.tenant_id = (select id from kontra.tenants where slug = 'default') -- tasks_by_context
		  and t.service = $1 and t.buyer_id = $2 and t.context_id = $3
		order by t.status_at desc, t.id desc limit 1000`, r.PathValue("service"), buyer, r.PathValue("context"))
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	tasks, err := collectTasks(rows)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	if len(tasks) == 0 {
		c.answer(w, r, nil, fail(http.StatusNotFound, "conversation_not_found", "no such conversation"))
		return
	}
	sortByCreated(tasks) // oldest first by creation
	c.answer(w, r, map[string]any{"service": r.PathValue("service"), "caller": buyer.String(), "context_id": r.PathValue("context"),
		"principal": tasks[len(tasks)-1].Caller, "tasks": tasks}, nil)
}

func sortByCreated(ts []*taskView) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j].TaskID < ts[j-1].TaskID; j-- { // UUIDv7: creation order
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

// ---------------------------------------------------------------- exchanges

type exchangeView struct {
	ID           string    `json:"id"`
	At           time.Time `json:"at"`
	Leg          string    `json:"leg"` // caller | callee (D355)
	Service      string    `json:"service"`
	Method       string    `json:"method"`
	Binding      string    `json:"binding"`
	A2AVersion   *string   `json:"a2a_version"`
	CalleeTaskID *string   `json:"callee_task_id"`
	Bodies       bool      `json:"bodies"`
	Principal    *string   `json:"principal"`
	Via          *string   `json:"via"`
	TaskID       *string   `json:"task_id"`
	ContextID    *string   `json:"context_id"`
	BytesIn      int64     `json:"bytes_in"`
	BytesOut     int64     `json:"bytes_out"`
	DurationMS   int       `json:"duration_ms"`
	HTTPStatus   int       `json:"http_status"`
	RPCCode      *int      `json:"rpc_code"`
	Reason       *string   `json:"reason"`
	Failed       bool      `json:"failed"`
}

type timeCursor struct {
	At time.Time `json:"a"`
	ID uuid.UUID `json:"i"`
}

func (c *Console) exchanges(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	q := r.URL.Query()
	limit, err := limitOf(r)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var cur timeCursor
	var at *time.Time
	var curID *uuid.UUID
	if s := q.Get("before"); s != "" {
		if err := decodeCursor(s, &cur); err != nil {
			c.answer(w, r, nil, err)
			return
		}
		at, curID = &cur.At, &cur.ID
	}
	var task *uuid.UUID
	if s := q.Get("task"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			c.answer(w, r, nil, fail(http.StatusUnprocessableEntity, "invalid_request", "task: a task id"))
			return
		}
		task = &id
	}
	failed := q.Get("failed") == "1" || q.Get("failed") == "true"
	leg := q.Get("leg")
	if leg != "" && leg != "caller" && leg != "callee" {
		c.answer(w, r, nil, fail(http.StatusUnprocessableEntity, "invalid_request", "leg: caller or callee"))
		return
	}
	// A task filter names the gate task on either leg (the callee leg's gate_task_id).
	rows, err := c.d.DB.Query(ctx, `select x.id, x.at, x.leg, x.service, x.method, x.binding, x.a2a_version, x.principal, x.via,
			coalesce(x.task_id, x.gate_task_id), x.callee_task_id, x.context_id, x.bytes_in, x.bytes_out, x.duration_ms,
			x.http_status, x.rpc_code, x.reason,
			x.request_body is not null or x.response_body is not null or exists (select 1 from kontra.a2a_exchange_frames f where f.exchange_id = x.id)
		  from kontra.a2a_exchanges x
		 where ($1::text = '' or x.service = $1) and ($2::text = '' or x.method = $2)
		   and ($3::uuid is null or x.task_id = $3 or x.gate_task_id = $3)
		   and (not $4 or x.http_status >= 400 or x.rpc_code is not null or x.reason is not null)
		   and ($5::timestamptz is null or (x.at, x.id) < ($5, $6::uuid))
		   and ($8::text = '' or x.leg = $8) and ($9::text = '' or x.context_id = $9)
		 order by x.at desc, x.id desc limit $7`, q.Get("service"), q.Get("method"), task, failed, at, curID, limit+1, leg, q.Get("context"))
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	defer rows.Close()
	items := []*exchangeView{}
	for rows.Next() {
		var (
			v      exchangeView
			id     uuid.UUID
			taskID *uuid.UUID
			status int16
		)
		if err := rows.Scan(&id, &v.At, &v.Leg, &v.Service, &v.Method, &v.Binding, &v.A2AVersion, &v.Principal, &v.Via, &taskID,
			&v.CalleeTaskID, &v.ContextID, &v.BytesIn, &v.BytesOut, &v.DurationMS, &status, &v.RPCCode, &v.Reason, &v.Bodies); err != nil {
			c.answer(w, r, nil, err)
			return
		}
		v.ID, v.HTTPStatus = id.String(), int(status)
		if taskID != nil {
			s := taskID.String()
			v.TaskID = &s
		}
		v.Failed = v.HTTPStatus >= 400 || v.RPCCode != nil || v.Reason != nil
		items = append(items, &v)
	}
	if err := rows.Err(); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		l := items[limit-1]
		n := encodeCursor(timeCursor{At: l.At, ID: uuid.MustParse(l.ID)})
		next = &n
	}
	c.answer(w, r, map[string]any{"items": items, "next": next}, nil)
}

// ---------------------------------------------------------------- MCP calls

type callView struct {
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	Service    string    `json:"service"`
	Tool       string    `json:"tool"`
	Caller     string    `json:"caller"`
	TaskID     *string   `json:"task_id"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
	DurationMS int       `json:"duration_ms"`
	Outcome    string    `json:"outcome"` // ok | error | unknown (a passthrough task-augmented call, spec §11.5)
}

func (c *Console) calls(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	limit, err := limitOf(r)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var cur timeCursor
	var at *time.Time
	var curID *uuid.UUID
	if s := r.URL.Query().Get("before"); s != "" {
		if err := decodeCursor(s, &cur); err != nil {
			c.answer(w, r, nil, err)
			return
		}
		at, curID = &cur.At, &cur.ID
	}
	rows, err := c.d.DB.Query(ctx, `select id, at, service, tool, principal, task_id, bytes_in, bytes_out, duration_ms, is_error
		  from kontra.calls
		 where ($1::text = '' or service = $1) and ($2::timestamptz is null or (at, id) < ($2, $3::uuid))
		 order by at desc, id desc limit $4`, r.URL.Query().Get("service"), at, curID, limit+1)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	defer rows.Close()
	items := []*callView{}
	for rows.Next() {
		var (
			v      callView
			id     uuid.UUID
			taskID *uuid.UUID
			isErr  *bool
		)
		if err := rows.Scan(&id, &v.At, &v.Service, &v.Tool, &v.Caller, &taskID, &v.BytesIn, &v.BytesOut, &v.DurationMS, &isErr); err != nil {
			c.answer(w, r, nil, err)
			return
		}
		v.ID = id.String()
		if taskID != nil {
			s := taskID.String()
			v.TaskID = &s
		}
		switch {
		case isErr == nil:
			v.Outcome = "unknown"
		case *isErr:
			v.Outcome = "error"
		default:
			v.Outcome = "ok"
		}
		items = append(items, &v)
	}
	if err := rows.Err(); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		l := items[limit-1]
		n := encodeCursor(timeCursor{At: l.At, ID: uuid.MustParse(l.ID)})
		next = &n
	}
	c.answer(w, r, map[string]any{"items": items, "next": next}, nil)
}

// ---------------------------------------------------------------- services and tunnels

type serviceView struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Backend     string          `json:"backend"` // tunnel | direct
	Source      string          `json:"source"`  // config | db (a node declared it)
	Sealed      bool            `json:"sealed"`
	Disabled    bool            `json:"disabled"`
	Retention   int64           `json:"retention"` // seconds; 0: kept forever
	Reachable   bool            `json:"reachable"`
	Sessions    int             `json:"sessions"`
	Description *string         `json:"description"`
	URL         string          `json:"url"`
	Card        json.RawMessage `json:"card,omitempty"` // the served card (GET /v1/services/{name})
	Agent       *agentSummary   `json:"agent"`          // the served card in brief (GET /v1/services); null: none
}

// agentSummary is what the Cards page shows of a served A2A card, each field clipped (the card is the agent's).
type agentSummary struct {
	Name        *string        `json:"name"`
	Version     *string        `json:"version"`
	Description *string        `json:"description"`
	Provider    *string        `json:"provider"`
	Skills      []summarySkill `json:"skills"`
}

type summarySkill struct {
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples"`
	InputModes  []string `json:"input_modes"`
	OutputModes []string `json:"output_modes"`
}

// summaryCard is the part of an A2A card the summary reads; a card that does not parse has no summary.
type summaryCard struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Provider    struct {
		Organization string `json:"organization"`
	} `json:"provider"`
	Skills []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Tags        []any  `json:"tags"`
		Examples    []any  `json:"examples"`
		InputModes  []any  `json:"inputModes"`
		OutputModes []any  `json:"outputModes"`
	} `json:"skills"`
}

// strs is the non-empty strings of xs, at most n, each clipped to m bytes.
func strs(xs []any, n, m int) []string {
	out := []string{}
	for _, x := range xs {
		if t, ok := x.(string); ok && len(out) < n {
			if c := clip(t, m); c != nil {
				out = append(out, *c)
			}
		}
	}
	return out
}

func summarize(raw []byte) *agentSummary {
	var k summaryCard
	if len(raw) == 0 || json.Unmarshal(raw, &k) != nil {
		return nil
	}
	a := &agentSummary{Name: clip(k.Name, 200), Version: clip(k.Version, 64), Description: clip(k.Description, 4000),
		Provider: clip(k.Provider.Organization, 200), Skills: []summarySkill{}}
	for _, sk := range k.Skills {
		if len(a.Skills) == skillsMax {
			break
		}
		name := clip(sk.Name, 200)
		if name == nil {
			name = clip(sk.ID, 200)
		}
		if name == nil {
			continue
		}
		a.Skills = append(a.Skills, summarySkill{Name: *name, Description: clip(sk.Description, 2000),
			Tags: strs(sk.Tags, 10, 64), Examples: strs(sk.Examples, examples, 500),
			InputModes: strs(sk.InputModes, 10, 100), OutputModes: strs(sk.OutputModes, 10, 100)})
	}
	return a
}

const serviceColumns = `s.name, s.kind, case when s.backend ? 'tunnel' then 'tunnel' else 'direct' end, s.source,
	s.sealed <> 'off', s.disabled_at is not null, s.retention_s,
	(select count(*) from kontra.tunnels u where u.tenant_id = s.tenant_id and s.name = any(u.services)),
	s.card_fetched_at is not null, left(s.card ->> 'description', 500)`

func (c *Console) scanService(row interface{ Scan(...any) error }, extra ...any) (*serviceView, error) {
	var (
		v       serviceView
		fetched bool
	)
	dst := append([]any{&v.Name, &v.Kind, &v.Backend, &v.Source, &v.Sealed, &v.Disabled, &v.Retention, &v.Sessions, &fetched,
		&v.Description}, extra...)
	if err := row.Scan(dst...); err != nil {
		return nil, err
	}
	v.Reachable = !v.Disabled && (v.Sessions > 0 || (v.Backend == "direct" && fetched))
	v.URL = strings.TrimRight(c.d.Config.PublicURL, "/") + "/" + map[string]string{"a2a": "agents", "mcp": "mcp"}[v.Kind] + "/" + v.Name
	return &v, nil
}

func (c *Console) services(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	rows, err := c.d.DB.Query(ctx, `select `+serviceColumns+`, case when s.kind = 'a2a' then s.card::text end from kontra.services s
		order by s.disabled_at is not null, s.name`)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	defer rows.Close()
	items := []*serviceView{}
	for rows.Next() {
		var card []byte
		v, err := c.scanService(rows, &card)
		if err != nil {
			c.answer(w, r, nil, err)
			return
		}
		v.Agent = summarize(card)
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	c.answer(w, r, map[string]any{"items": items}, nil)
}

var errNoService = fail(http.StatusNotFound, "service_not_found", "no such service")

func (c *Console) service(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	var card []byte
	v, err := c.scanService(c.d.DB.QueryRow(ctx, `select `+serviceColumns+`, s.card::text from kontra.services s where s.name = $1
		order by s.disabled_at is not null limit 1`, r.PathValue("name")), &card)
	if err != nil {
		if isNoRows(err) {
			err = errNoService
		}
		c.answer(w, r, nil, err)
		return
	}
	if len(card) > 0 {
		v.Card = json.RawMessage(card)
	}
	c.answer(w, r, v, nil)
}

type tunnelView struct {
	SessionID   string    `json:"session_id"`
	KeyID       string    `json:"key_id"`
	NodeName    *string   `json:"node_name"`
	Services    []string  `json:"services"`
	CLIVersion  string    `json:"cli_version"`
	ReplicaID   string    `json:"replica_id"`
	ConnectedAt time.Time `json:"connected_at"`
	LastPingAt  time.Time `json:"last_ping_at"`
}

func (c *Console) tunnels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	rows, err := c.d.DB.Query(ctx, `select u.session_id, u.key_id, k.node_name, u.services, u.cli_version, u.replica_id,
			u.connected_at, u.last_ping_at
		  from kontra.tunnels u join kontra.tunnel_keys k on k.key_id = u.key_id
		 order by u.connected_at desc`)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	defer rows.Close()
	items := []*tunnelView{}
	for rows.Next() {
		var (
			v  tunnelView
			id uuid.UUID
		)
		if err := rows.Scan(&id, &v.KeyID, &v.NodeName, &v.Services, &v.CLIVersion, &v.ReplicaID, &v.ConnectedAt, &v.LastPingAt); err != nil {
			c.answer(w, r, nil, err)
			return
		}
		v.SessionID = id.String()
		items = append(items, &v)
	}
	if err := rows.Err(); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	c.answer(w, r, map[string]any{"items": items}, nil)
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
