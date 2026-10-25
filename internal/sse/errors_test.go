package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/errordetails"

	"kontra.sh/kontra/internal/lifecycle"
)

// erring is an a2a-go RequestHandler whose every method fails with err.
type erring struct{ err error }

func (h erring) GetTask(context.Context, *a2a.GetTaskRequest) (*a2a.Task, error) { return nil, h.err }
func (h erring) ListTasks(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return nil, h.err
}
func (h erring) CancelTask(context.Context, *a2a.CancelTaskRequest) (*a2a.Task, error) {
	return nil, h.err
}
func (h erring) SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return nil, h.err
}
func (h erring) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, h.err) }
}
func (h erring) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, h.err) }
}
func (h erring) GetTaskPushConfig(context.Context, *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	return nil, h.err
}
func (h erring) ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	return nil, h.err
}
func (h erring) CreateTaskPushConfig(context.Context, *a2a.PushConfig) (*a2a.PushConfig, error) {
	return nil, h.err
}
func (h erring) DeleteTaskPushConfig(context.Context, *a2a.DeleteTaskPushConfigRequest) error {
	return h.err
}
func (h erring) GetExtendedAgentCard(context.Context, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return nil, h.err
}

// sentinels are every error a2a-go maps.
var sentinels = []error{
	a2a.ErrParseError, a2a.ErrInvalidRequest, a2a.ErrMethodNotFound, a2a.ErrInvalidParams, a2a.ErrInternalError,
	a2a.ErrServerError, a2a.ErrTaskNotFound, a2a.ErrTaskNotCancelable, a2a.ErrPushNotificationNotSupported,
	a2a.ErrUnsupportedOperation, a2a.ErrUnsupportedContentType, a2a.ErrInvalidAgentResponse,
	a2a.ErrExtendedCardNotConfigured, a2a.ErrExtensionSupportRequired, a2a.ErrVersionNotSupported,
	a2a.ErrUnauthenticated, a2a.ErrUnauthorized,
}

// driftCases are the sentinels bare, with a message, wrapped, with details and typed details, and errors
// that are none of them.
func driftCases() []error {
	var out []error
	for _, s := range sentinels {
		out = append(out, s,
			a2a.NewError(s, "the gate's own words"),
			fmt.Errorf("context: %w", s),
			a2a.NewError(s, "with details").WithDetails(map[string]any{"field": "x", "n": 2.0}),
			a2a.NewError(s, "with info").WithErrorInfoMeta(map[string]string{"taskId": "t1", "why": "because"}),
			a2a.NewError(s, "typed").WithTypedDetails(errordetails.NewTyped("type.googleapis.com/google.rpc.BadRequest", map[string]any{"a": "b"})),
		)
	}
	return append(out, errors.New("plain"), context.Canceled, context.DeadlineExceeded,
		fmt.Errorf("deadline: %w", context.DeadlineExceeded))
}

// normalize decodes a JSON error and drops the ErrorInfo timestamp (the only value that differs between
// two runs), checking first that it is there and is RFC 3339.
func normalize(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			if x["@type"] == errordetails.ErrorInfoType {
				md, _ := x["metadata"].(map[string]any)
				ts, _ := md["timestamp"].(string)
				if _, err := time.Parse(time.RFC3339, ts); err != nil {
					t.Errorf("ErrorInfo timestamp %q: %v", ts, err)
				}
				delete(md, "timestamp")
			}
			for _, v := range x {
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(v)
	return v
}

// TestErrorMappingDrift: ToJSONRPC and ToHTTP equal what a2a-go's own JSON-RPC and REST handlers write
// for every a2a error sentinel (D7: a bump that changes the mapping fails here).
func TestErrorMappingDrift(t *testing.T) {
	for _, e := range driftCases() {
		t.Run(e.Error(), func(t *testing.T) {
			h := erring{err: e}
			// JSON-RPC: a2a-go writes the error object of a failed GetTask.
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"GetTask","params":{"id":"t"}}`))
			a2asrv.NewJSONRPCHandler(h).ServeHTTP(rec, req)
			var resp struct {
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Error) == 0 {
				t.Fatalf("a2a-go wrote %s", rec.Body)
			}
			mine, err := json.Marshal(ToJSONRPC(e))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := normalize(t, mine), normalize(t, resp.Error); !reflect.DeepEqual(got, want) {
				t.Errorf("JSON-RPC:\n mine   %s\n a2a-go %s", mine, resp.Error)
			}
			// HTTP+JSON: a2a-go's message:send path writes the error without a task id, as its streaming
			// routes do.
			rec = httptest.NewRecorder()
			req = httptest.NewRequest(http.MethodPost, "/message:send", strings.NewReader(`{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]}}`))
			a2asrv.NewRESTHandler(h).ServeHTTP(rec, req)
			status, body := ToHTTP(e)
			if status != rec.Code {
				t.Errorf("HTTP status %d, a2a-go %d", status, rec.Code)
			}
			if got, want := normalize(t, body), normalize(t, rec.Body.Bytes()); !reflect.DeepEqual(got, want) {
				t.Errorf("HTTP+JSON:\n mine   %s\n a2a-go %s", body, rec.Body)
			}
		})
	}
}

// The codes and statuses the specs name (relay.md §2.4, §4, §7, §8, §9).
func TestErrorCodes(t *testing.T) {
	for _, c := range []struct {
		err    error
		code   int
		status int
	}{
		{a2a.ErrTaskNotFound, -32001, 404},
		{a2a.ErrTaskNotCancelable, -32002, 400},
		{a2a.ErrUnsupportedOperation, -32004, 400},
		{a2a.ErrExtensionSupportRequired, -32008, 400},
		{a2a.ErrVersionNotSupported, -32009, 400},
		{a2a.ErrInvalidParams, -32602, 400},
		{a2a.ErrInternalError, -32603, 500},
	} {
		if got := ToJSONRPC(c.err).Code; got != c.code {
			t.Errorf("%v: code %d, want %d", c.err, got, c.code)
		}
		if got, _ := ToHTTP(c.err); got != c.status {
			t.Errorf("%v: HTTP %d, want %d", c.err, got, c.status)
		}
	}
}

// An Update the lifecycle did not accept in time is -32603 on JSON-RPC and HTTP 503 on HTTP+JSON, still
// read as an internal error by an A2A client (spec §7, §14.1).
func TestUnavailableIs503(t *testing.T) {
	err := fmt.Errorf("%w: %w", a2a.NewError(a2a.ErrInternalError, "the task's lifecycle is unavailable"), lifecycle.ErrUnavailable)
	if c := ToJSONRPC(err).Code; c != -32603 {
		t.Errorf("JSON-RPC code %d", c)
	}
	status, body := HTTPError(err)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d", status)
	}
	var b struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Details []struct {
				Type   string `json:"@type"`
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	last := b.Error.Details[len(b.Error.Details)-1]
	if b.Error.Code != 503 || b.Error.Status != "UNAVAILABLE" || last.Reason != "INTERNAL_ERROR" {
		t.Errorf("body %s", body)
	}
	// Any other internal error keeps a2a-go's 500.
	if s, _ := HTTPError(a2a.ErrInternalError); s != 500 {
		t.Errorf("internal error: %d", s)
	}
}

// WriteError: JSON-RPC errors carry the request id at HTTP 200 (or the status asked for); HTTP+JSON
// errors their mapped status.
func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, JSONRPC, json.RawMessage(`"req-1"`), 0, a2a.ErrTaskNotFound)
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   RPCError        `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || resp.JSONRPC != "2.0" || string(resp.ID) != `"req-1"` || resp.Error.Code != -32001 {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, JSONRPC, nil, http.StatusNotFound, a2a.ErrMethodNotFound)
	if rec.Code != 404 || !bytes.Contains(rec.Body.Bytes(), []byte(`"id":null`)) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, HTTPJSON, nil, 0, a2a.ErrTaskNotFound)
	if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/json" || !bytes.Contains(rec.Body.Bytes(), []byte(`"NOT_FOUND"`)) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func lifecycleUnavailable() error { return lifecycle.ErrUnavailable }
