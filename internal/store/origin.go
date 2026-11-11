package store

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Origin is where a logged event came from (task_events.origin, migration 0007, D357): the actor that sent it, the
// A2A type it sent and the A2A method it travelled in, the traffic record's exchange that carried it when one was
// recorded, the type the gate logged instead when it converted it (D5's Message, a polled Task's difference), and
// on a caller's first message whether the gate minted its contextId (D3).
type Origin struct {
	Actor           string `json:"actor"`
	Type            string `json:"type"`
	Method          string `json:"method,omitempty"`
	ExchangeID      string `json:"exchange_id,omitempty"`
	ConvertedTo     string `json:"converted_to,omitempty"`
	ContextAssigned *bool  `json:"context_assigned,omitempty"`
}

// Actors.
const (
	ActorCaller = "caller"
	ActorCallee = "callee"
	ActorGate   = "gate"
)

// A2A types an origin names, beyond the methods.
const (
	TypeTask           = "Task"
	TypeMessage        = "Message"
	TypeStatusUpdate   = "TaskStatusUpdateEvent"
	TypeArtifactUpdate = "TaskArtifactUpdateEvent"
)

// TypeOf is the A2A type of an event: Task, Message, TaskStatusUpdateEvent or TaskArtifactUpdateEvent.
func TypeOf(ev a2a.Event) string {
	switch ev.(type) {
	case *a2a.Task:
		return TypeTask
	case *a2a.Message:
		return TypeMessage
	case *a2a.TaskStatusUpdateEvent:
		return TypeStatusUpdate
	case *a2a.TaskArtifactUpdateEvent:
		return TypeArtifactUpdate
	}
	return ""
}

// GateOrigin is the origin of an event the gate wrote itself: a status (a failure, a reconnect wait, a cancel
// without an upstream task).
func GateOrigin() *Origin { return &Origin{Actor: ActorGate, Type: TypeStatusUpdate} }

// encode is o as task_events.origin holds it: null for none.
func (o *Origin) encode() any {
	if o == nil || o.Actor == "" {
		return nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil
	}
	return b
}

// Origins are the origins a dispatch gives the events it yields, by event: a2a-go hands the yielded event, the same
// pointer, to Store.Update after it crosses its event pipe (asynchronously), so the origin is keyed by the event, not
// by the order of writes. A dispatch also names the origin of the caller's message a2a-go writes for its turn
// (SetCaller).
type Origins struct {
	mu     sync.Mutex
	by     map[a2a.Event]*Origin
	caller *Origin
}

type originsKey struct{}

// WithOrigins returns ctx carrying a fresh Origins, for one dispatch.
func WithOrigins(ctx context.Context) (context.Context, *Origins) {
	o := &Origins{by: map[a2a.Event]*Origin{}}
	return context.WithValue(ctx, originsKey{}, o), o
}

// OriginsFrom returns the Origins ctx carries.
func OriginsFrom(ctx context.Context) *Origins {
	o, _ := ctx.Value(originsKey{}).(*Origins)
	return o
}

// Set names ev's origin.
func (o *Origins) Set(ev a2a.Event, origin *Origin) {
	if o == nil || ev == nil || origin == nil {
		return
	}
	o.mu.Lock()
	o.by[ev] = origin
	o.mu.Unlock()
}

// SetCaller names the origin of the caller's message the turn writes.
func (o *Origins) SetCaller(origin *Origin) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.caller = origin
	o.mu.Unlock()
}

// take returns, and forgets, ev's origin; a caller's message without one gets the turn's caller origin.
func (o *Origins) take(ev a2a.Event) *Origin {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if origin, ok := o.by[ev]; ok {
		delete(o.by, ev)
		return origin
	}
	if m, ok := ev.(*a2a.Message); ok && m.Role == a2a.MessageRoleUser {
		return o.caller
	}
	return nil
}

type requestOriginKey struct{}

// WithRequestOrigin returns ctx carrying the origin of the caller's request it serves (the A2A route sets it: the
// method and the exchange that carries it), which the work queue writes on the caller's message.
func WithRequestOrigin(ctx context.Context, o *Origin) context.Context {
	return context.WithValue(ctx, requestOriginKey{}, o)
}

// RequestOrigin is a copy of the origin WithRequestOrigin set, or a caller origin of type SendMessage when none was
// (a request that did not come through an A2A route).
func RequestOrigin(ctx context.Context) *Origin {
	if o, ok := ctx.Value(requestOriginKey{}).(*Origin); ok && o != nil {
		cp := *o
		return &cp
	}
	return &Origin{Actor: ActorCaller, Type: "SendMessage", Method: "SendMessage"}
}
