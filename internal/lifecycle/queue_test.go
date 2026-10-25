package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/limiter"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/store"
)

const (
	secretToken  = "s3cr3t-bearer-token-a1b2"
	secretCookie = "c00kie-session-z9y8"
	secretKey    = "x-api-key-q7w6"
)

// callContextOf is a2asrv's encoded call context of a request that carried credentials: what
// a2a-go puts in workqueue.Payload.CallContext (ctxcodec.go), headers lowercased as NewServiceParams
// keeps them, plus a custom attribute.
func callContextOf(o store.Owner) map[string]any {
	return map[string]any{
		"callContext": map[string]any{
			"tenant": "",
			"svcParams": map[string]any{
				"a2a-version":    []any{"1.0"},
				"a2a-extensions": []any{"https://example.com/ext/one"},
				"authorization":  []any{"Bearer " + secretToken},
				"cookie":         []any{"session=" + secretCookie},
				"X-Api-Key":      []any{secretKey},
			},
			"user": map[string]any{"name": o.BuyerID.String(), "authenticated": true,
				"attributes": map[string]any{"principal": o.Principal, "via": o.Via}},
			"protocolVersion": "1.0",
		},
		"attrs": map[string]any{"leak": secretToken},
	}
}

func requestOf(msgID, text string) *a2a.SendMessageRequest {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.ID = msgID
	return &a2a.SendMessageRequest{Message: m}
}

func TestSanitizeKeepsOnlyVersionAndExtensions(t *testing.T) {
	req := requestOf("m-1", "hello")
	wp, err := Sanitize(&workqueue.Payload{Type: workqueue.PayloadTypeExecute, ExecuteRequest: req, CallContext: callContextOf(buyer)}, buyer)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(wp)
	for _, s := range []string{secretToken, secretCookie, secretKey, "authorization", "cookie", "attrs"} {
		if strings.Contains(strings.ToLower(string(b)), strings.ToLower(s)) {
			t.Errorf("%q survived sanitizing: %s", s, b)
		}
	}
	cc := wp.CallContext["callContext"].(map[string]any)
	params := cc["svcParams"].(map[string]any)
	if len(params) != 2 || params["a2a-version"] == nil || params["a2a-extensions"] == nil {
		t.Errorf("service params %v", params)
	}
	user := cc["user"].(map[string]any)
	if user["name"] != buyer.BuyerID.String() || cc["protocolVersion"] != "1.0" {
		t.Errorf("call context lost the identity: %v", cc)
	}
	if wp.Request != req || wp.Owner != buyer {
		t.Error("the request and the owner are kept as they are")
	}
	if _, err := Sanitize(&workqueue.Payload{}, buyer); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("a payload without a request: %v", err)
	}
	if wp, err := Sanitize(&workqueue.Payload{ExecuteRequest: req}, buyer); err != nil || wp.CallContext != nil {
		t.Errorf("no call context: %v %v", wp.CallContext, err)
	}
}

func TestRebuild(t *testing.T) {
	id := a2a.TaskID(uuid.Must(uuid.NewV7()).String())
	req := requestOf("m-1", "hello")
	wi := &store.WorkItem{ID: uuid.Must(uuid.NewV7()), Kind: store.WorkExecute,
		Payload: store.WorkPayload{Request: req, CallContext: callContextOf(buyer), Owner: buyer}}
	p, err := Rebuild(id, wi)
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != workqueue.PayloadTypeExecute || p.TaskID != id || p.ExecuteRequest != req {
		t.Errorf("payload %+v", p)
	}
	b, _ := json.Marshal(p.CallContext)
	if strings.Contains(string(b), secretToken) || strings.Contains(string(b), secretCookie) || !strings.Contains(string(b), "a2a-version") {
		t.Errorf("rebuilt call context %s: sanitized again", b)
	}
	follow := requestOf("m-2", "more")
	follow.Message.TaskID = a2a.TaskID(uuid.Must(uuid.NewV7()).String())
	if _, err := Rebuild(id, &store.WorkItem{Kind: store.WorkExecute, Payload: store.WorkPayload{Request: follow}}); !errors.Is(err, workqueue.ErrMalformedPayload) {
		t.Errorf("a work item of another task: %v", err)
	}
	if _, err := Rebuild(id, &store.WorkItem{Kind: store.WorkCancel, Payload: store.WorkPayload{Request: req}}); !errors.Is(err, workqueue.ErrMalformedPayload) {
		t.Errorf("a cancel item: %v", err)
	}
	if _, err := Rebuild(id, nil); err == nil {
		t.Error("no work item")
	}
}

func TestHeartbeater(t *testing.T) {
	var hb Heartbeater
	if hb.HeartbeatInterval().Seconds() != 5 {
		t.Errorf("interval %v", hb.HeartbeatInterval())
	}
	if err := hb.Heartbeat(context.Background()); err != nil {
		t.Errorf("outside an activity: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hb.Heartbeat(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("an aborted dispatch's heartbeat: %v, want its context's error", err)
	}
}

func TestQueueRegisterHandlerKeepsTheFunction(t *testing.T) {
	q := NewQueue(store.ServiceRef{}, nil, nil, QueueOptions{})
	calls := 0
	fn := func(context.Context, *workqueue.Payload) (a2a.SendMessageResult, error) {
		calls++
		return nil, nil
	}
	// a2a-go passes its limiter; the queue keeps the function itself, so the limiter stays off.
	q.RegisterHandler(workqueue.HandlerConfig{Limiter: limiter.ConcurrencyConfig{MaxExecutions: 1}}, fn)
	for range 3 {
		if _, err := q.Handler()(context.Background(), &workqueue.Payload{}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Errorf("%d calls", calls)
	}
}

func TestQueueWriteRefusals(t *testing.T) {
	q := NewQueue(store.ServiceRef{}, nil, nil, QueueOptions{Owner: func(context.Context) (store.Owner, error) { return buyer, nil }})
	if _, err := q.Write(context.Background(), &workqueue.Payload{Type: workqueue.PayloadTypeCancel, CancelRequest: &a2a.CancelTaskRequest{}}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("a cancel payload: %v (CancelTask is the interceptor's, D4)", err)
	}
	if _, err := q.Write(context.Background(), &workqueue.Payload{Type: workqueue.PayloadTypeExecute}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("an execute payload without a request: %v", err)
	}
	noOwner := NewQueue(store.ServiceRef{}, nil, nil, QueueOptions{})
	if _, err := noOwner.Write(context.Background(), &workqueue.Payload{Type: workqueue.PayloadTypeExecute, ExecuteRequest: requestOf("m", "x")}); err == nil {
		t.Error("a queue without an owner function wrote")
	}
}

func TestAdmittedDecision(t *testing.T) {
	st, exp, meta, err := admitted(hooks.Decision{Outcome: hooks.Allow, Metadata: map[string]any{"kontra.escrow.status": "none"}})
	if err != nil || st != store.StateSubmitted || exp != nil || meta["kontra.escrow.status"] != "none" {
		t.Errorf("allow: %s %v %v %v", st, exp, meta, err)
	}
	st, exp, meta, err = admitted(hooks.Decision{Outcome: hooks.Quote, Quote: &hooks.QuoteTerms{ApproveURL: "https://pay.example/t/1", ExpiresAt: "2026-10-25T13:00:00Z"}})
	if err != nil || st != store.StateQuoted || exp == nil || exp.Hour() != 13 || meta[MetaApproveURL] != "https://pay.example/t/1" {
		t.Errorf("quote: %s %v %v %v", st, exp, meta, err)
	}
	if _, _, _, err := admitted(hooks.Decision{Outcome: hooks.Quote, Quote: &hooks.QuoteTerms{ExpiresAt: "soon"}}); !errors.Is(err, a2a.ErrInternalError) {
		t.Errorf("a quote without a valid expiry: %v", err)
	}
}

func TestBuiltinHooksRegisterUnderTheirNames(t *testing.T) {
	r := &recordingRegistry{}
	RegisterBuiltinHooks(r, hooks.BuiltinLifecycle{})
	want := []string{HookOnDispatched, HookOnTerminal, HookReviewWindow, HookOnReviewMessage, HookOnReviewClosed}
	if strings.Join(r.names, ",") != strings.Join(want, ",") {
		t.Errorf("registered %v", r.names)
	}
	b := hooks.BuiltinLifecycle{}
	ctx := context.Background()
	if err := b.OnDispatched(ctx, hooks.TaskRef{}, hooks.DispatchedArgs{Attempt: 1}); err != nil {
		t.Error(err)
	}
	if p, err := b.OnTerminal(ctx, hooks.TaskRef{}); err != nil || p.Metadata != nil || p.HoldOpenSeconds != 0 {
		t.Errorf("OnTerminal %+v %v", p, err)
	}
	if w, err := b.ReviewWindow(ctx, hooks.TaskRef{}); err != nil || w.Open {
		t.Errorf("ReviewWindow %+v %v: open source opens no window", w, err)
	}
	if r, err := b.OnReviewMessage(ctx, hooks.TaskRef{}, hooks.ReviewMessageArgs{}); err != nil || r.Accept {
		t.Errorf("OnReviewMessage %+v %v", r, err)
	}
	if p, err := b.OnReviewClosed(ctx, hooks.TaskRef{}, hooks.ReviewClosedArgs{}); err != nil || p.Metadata != nil {
		t.Errorf("OnReviewClosed %+v %v", p, err)
	}
}

type recordingRegistry struct{ names []string }

func (r *recordingRegistry) RegisterActivityWithOptions(_ any, o activity.RegisterOptions) {
	r.names = append(r.names, o.Name)
}

func TestClassifyUpdateErrors(t *testing.T) {
	refusedErr := classify(temporal.NewApplicationError("a turn is running", ErrTypeRefused), "no")
	if !errors.Is(refusedErr, ErrRefused) || !errors.Is(refusedErr, a2a.ErrUnsupportedOperation) || errors.Is(refusedErr, ErrClosed) {
		t.Errorf("a validator's refusal: %v", refusedErr)
	}
	for _, err := range []error{serviceerror.NewNotFound("workflow execution already completed"),
		fmt.Errorf("failed workflow start: %w", serviceerror.NewWorkflowExecutionAlreadyStarted("closed", "", ""))} {
		c := classify(err, "no")
		if !errors.Is(c, ErrClosed) || !errors.Is(c, ErrRefused) || !errors.Is(c, a2a.ErrUnsupportedOperation) {
			t.Errorf("a closed workflow (%v): %v", err, c)
		}
	}
	u := classify(context.DeadlineExceeded, "no")
	if !errors.Is(u, ErrUnavailable) || !errors.Is(u, a2a.ErrInternalError) || errors.Is(u, ErrRefused) {
		t.Errorf("no answer in time: %v", u)
	}
	if c := classifyCancel(temporal.NewApplicationError("the task is completed", ErrTypeNotCancelable)); !errors.Is(c, a2a.ErrTaskNotCancelable) {
		t.Errorf("a cancel of an ended task: %v", c)
	}
	if c := classifyCancel(serviceerror.NewNotFound("gone")); !errors.Is(c, a2a.ErrTaskNotCancelable) {
		t.Errorf("a cancel of a closed workflow: %v", c)
	}
	if c := classifyCancel(context.DeadlineExceeded); !errors.Is(c, ErrUnavailable) {
		t.Errorf("a cancel not accepted in time: %v", c)
	}
}

// Admission is called before any task exists and fails closed (spec §13.1): a new task whose request
// carries no admission decision, or one that is neither allow nor quote, is refused before anything is
// written, never created with an id of Write's own.
func TestQueueWriteNewTaskNeedsAnAdmission(t *testing.T) {
	q := NewQueue(store.ServiceRef{}, nil, nil, QueueOptions{Owner: func(context.Context) (store.Owner, error) { return buyer, nil }})
	p := &workqueue.Payload{Type: workqueue.PayloadTypeExecute, ExecuteRequest: requestOf("m-1", "hello")}
	for name, ctx := range map[string]context.Context{
		"no admission": context.Background(),
		"denied":       hooks.WithAdmitted(context.Background(), hooks.Admitted{TaskID: "01920000-0000-7000-8000-00000000000f", Decision: hooks.Decision{Outcome: hooks.Deny}}),
		"no decision":  hooks.WithAdmitted(context.Background(), hooks.Admitted{TaskID: "01920000-0000-7000-8000-00000000000f"}),
		"no task id":   hooks.WithAdmitted(context.Background(), hooks.Admitted{Decision: hooks.Decision{Outcome: hooks.Allow}}),
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("Write went on to write the task: %v", r)
					}
				}()
				_, err = q.Write(ctx, p)
			}()
			if !errors.Is(err, a2a.ErrInternalError) || !strings.Contains(err.Error(), "admission unavailable") {
				t.Fatalf("a new task %s: %v, want -32603 admission unavailable", name, err)
			}
		})
	}
}
