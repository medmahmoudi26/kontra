package sse

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/errordetails"

	"kontra.sh/kontra/internal/lifecycle"
)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// jsonrpcCodes is a2a-go's JSON-RPC code table (internal/jsonrpc codeToError), in a fixed order. a2a-go
// ranges over a map, so for an error that wraps two sentinels its choice is random; this copy takes the
// first in this order. For every error that wraps one sentinel the two agree (TestErrorMappingDrift).
var jsonrpcCodes = []struct {
	code int
	err  error
}{
	{-32700, a2a.ErrParseError},
	{-32600, a2a.ErrInvalidRequest},
	{-32601, a2a.ErrMethodNotFound},
	{-32602, a2a.ErrInvalidParams},
	{-32603, a2a.ErrInternalError},
	{-32000, a2a.ErrServerError},
	{-32001, a2a.ErrTaskNotFound},
	{-32002, a2a.ErrTaskNotCancelable},
	{-32003, a2a.ErrPushNotificationNotSupported},
	{-32004, a2a.ErrUnsupportedOperation},
	{-32005, a2a.ErrUnsupportedContentType},
	{-32006, a2a.ErrInvalidAgentResponse},
	{-32007, a2a.ErrExtendedCardNotConfigured},
	{-32008, a2a.ErrExtensionSupportRequired},
	{-32009, a2a.ErrVersionNotSupported},
	{-31401, a2a.ErrUnauthenticated},
	{-31403, a2a.ErrUnauthorized},
}

// restStatuses is a2a-go's HTTP+JSON table (internal/rest errorMappings), in its own order: the first
// sentinel the error wraps decides the HTTP status and the google.rpc status.
var restStatuses = []struct {
	err        error
	httpStatus int
	grpcStatus string
}{
	{a2a.ErrParseError, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrInvalidRequest, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrMethodNotFound, http.StatusNotImplemented, "UNIMPLEMENTED"},
	{a2a.ErrInvalidParams, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrInternalError, http.StatusInternalServerError, "INTERNAL"},
	{a2a.ErrServerError, http.StatusInternalServerError, "INTERNAL"},
	{a2a.ErrTaskNotFound, http.StatusNotFound, "NOT_FOUND"},
	{a2a.ErrTaskNotCancelable, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrPushNotificationNotSupported, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrUnsupportedOperation, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrUnsupportedContentType, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrInvalidAgentResponse, http.StatusInternalServerError, "INTERNAL"},
	{a2a.ErrExtendedCardNotConfigured, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrExtensionSupportRequired, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrVersionNotSupported, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrUnauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED"},
	{a2a.ErrUnauthorized, http.StatusForbidden, "PERMISSION_DENIED"},

	{context.Canceled, 499, "CANCELLED"},
	{context.DeadlineExceeded, http.StatusGatewayTimeout, "DEADLINE_EXCEEDED"},
}

// details builds what a2a-go puts after the error's message: the a2a.Error's Details as a Struct, its
// typed details except ErrorInfo, whose metadata is merged into the gate's ErrorInfo, and that ErrorInfo
// last (reason, domain a2a-protocol.org, metadata with the timestamp).
func details(err error, reason string, metadata map[string]string) []*errordetails.Typed {
	var out []*errordetails.Typed
	var ae *a2a.Error
	if errors.As(err, &ae) {
		if len(ae.Details) > 0 {
			out = append(out, errordetails.NewFromStruct(ae.Details))
		}
		for _, d := range ae.TypedDetails {
			if d.TypeURL == errordetails.ErrorInfoType {
				if m, ok := stringMap(d.Value["metadata"]); ok {
					maps.Copy(metadata, m)
				}
				continue
			}
			out = append(out, d)
		}
	}
	return append(out, errordetails.NewErrorInfo(reason, a2a.ProtocolDomain, metadata))
}

// stringMap is a2a-go's utils.ToStringMap: a map[string]string, or a map[string]any whose values are all
// strings.
func stringMap(v any) (map[string]string, bool) {
	switch m := v.(type) {
	case map[string]string:
		return m, true
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, val := range m {
			s, ok := val.(string)
			if !ok {
				return nil, false
			}
			out[k] = s
		}
		return out, true
	}
	return nil, false
}

func timestamp() map[string]string {
	return map[string]string{"timestamp": time.Now().UTC().Format(time.RFC3339)}
}

// ToJSONRPC maps an error to A2A v1.0.1's JSON-RPC error (-32001 TaskNotFound … -32009
// VersionNotSupported, google.rpc.ErrorInfo in data). a2a-go's own mapping is internal to its module, so
// the gate keeps this copy and a test that compares it with what a2a-go's JSON-RPC handler writes for
// every error, so a bump that changes the mapping fails the build (D7). Data is the list of typed details
// a2a-go writes: a Struct for the error's Details, its other typed details, and the ErrorInfo.
func ToJSONRPC(err error) RPCError {
	if err == nil {
		err = a2a.ErrInternalError
	}
	code, reason := -32603, "INTERNAL_ERROR"
	for _, m := range jsonrpcCodes {
		if errors.Is(err, m.err) {
			code, reason = m.code, a2a.ErrorReason(m.err)
			break
		}
	}
	return RPCError{Code: code, Message: err.Error(), Data: details(err, reason, timestamp())}
}

// restStatus is the inner object of a google.rpc.Status body.
type restStatus struct {
	Code    int                   `json:"code"`
	Status  string                `json:"status"`
	Message string                `json:"message"`
	Details []*errordetails.Typed `json:"details,omitempty"`
}

// ToHTTP maps an error to the HTTP+JSON binding's status and google.rpc.Status body, as a2a-go's REST
// handler writes it on its streaming routes (no taskId in the ErrorInfo metadata). TestErrorMappingDrift
// compares it with a2a-go for every error.
func ToHTTP(err error) (status int, body []byte) {
	if err == nil {
		err = a2a.ErrInternalError
	}
	status, grpc, reason := http.StatusInternalServerError, "INTERNAL", "INTERNAL_ERROR"
	for _, m := range restStatuses {
		if errors.Is(err, m.err) {
			status, grpc, reason = m.httpStatus, m.grpcStatus, a2a.ErrorReason(m.err)
			break
		}
	}
	return status, restBody(status, grpc, err.Error(), details(err, reason, timestamp()))
}

func restBody(code int, status, msg string, det []*errordetails.Typed) []byte {
	b, err := json.Marshal(struct {
		Error restStatus `json:"error"`
	}{restStatus{Code: code, Status: status, Message: msg, Details: det}})
	if err != nil {
		// A detail that does not encode (a caller-supplied Details value): the status alone.
		b, _ = json.Marshal(struct {
			Error restStatus `json:"error"`
		}{restStatus{Code: code, Status: status, Message: msg}})
	}
	return append(b, '\n')
}

// internalCodes are the sentinels whose JSON-RPC code is a server's own failure (-32603, -32000, -32006;
// HTTP 500 on HTTP+JSON). An error that wraps none of the sentinels is one too.
var internalCodes = []error{a2a.ErrInternalError, a2a.ErrServerError, a2a.ErrInvalidAgentResponse}

// internalError reports whether err maps to a server failure: the first sentinel it wraps, in the order
// ToJSONRPC reads them, is one of internalCodes, or it wraps none (an error a2a-go answers -32603).
func internalError(err error) bool {
	for _, m := range jsonrpcCodes {
		if errors.Is(err, m.err) {
			return slices.Contains(internalCodes, m.err)
		}
	}
	return true
}

// Public is the error a caller is answered with for err. A caller's own mistake (task not found, invalid
// params, a refused Update, …) keeps its text. An internal error (Internal, Server or InvalidAgentResponse,
// or none of the sentinels; HTTP 5xx, including an Update not accepted in time) keeps only the gate's own
// words: the message of the outermost *a2a.Error in its chain (such as "the task's lifecycle is
// unavailable"), else the text of its sentinel ("internal error"). The rest of its text is a cause the
// caller never reads: a database error, the Temporal dial target, a2a-go's wrapping of them. The error
// returned wraps err, so the code, the reason, the details and the 503 of an Update not accepted in time
// are unchanged; redacted reports whether the text changed, so the caller can log err.
func Public(err error) (pub error, redacted bool) {
	if err == nil || !internalError(err) {
		return err, false
	}
	msg := a2a.ErrInternalError.Error()
	for _, s := range []error{a2a.ErrServerError, a2a.ErrInvalidAgentResponse, context.DeadlineExceeded, context.Canceled} {
		if errors.Is(err, s) {
			msg = s.Error()
			break
		}
	}
	var ae *a2a.Error
	if errors.As(err, &ae) && ae.Message != "" {
		msg = ae.Message
	}
	if msg == err.Error() {
		return err, false
	}
	out := &a2a.Error{Err: err, Message: msg}
	if ae != nil {
		// a2a-go reads details from the outermost *a2a.Error; keep the gate's (ErrorInfo() appends to its
		// own copy).
		out.Details, out.TypedDetails = ae.Details, slices.Clone(ae.TypedDetails)
	}
	return out, true
}

func public(err error) error {
	pub, _ := Public(err)
	return pub
}

// Unavailable reports whether err is an Update the task's lifecycle did not accept, or did not complete,
// in time (lifecycle.ErrUnavailable): -32603 on JSON-RPC, HTTP 503 on HTTP+JSON (spec §7, §14.1).
func Unavailable(err error) bool { return errors.Is(err, lifecycle.ErrUnavailable) }

// HTTPError is the HTTP+JSON answer to err: ToHTTP of Public(err), except that an Update not accepted in
// time is HTTP 503 (google.rpc UNAVAILABLE, reason INTERNAL_ERROR, so an A2A client still reads -32603),
// where a2a-go would write 500 (spec §7, §14.1: "-32603 (HTTP 503)"; A2A §3.3.2 allows 500 or 503 for it).
func HTTPError(err error) (status int, body []byte) {
	err = public(err)
	if !Unavailable(err) {
		return ToHTTP(err)
	}
	return http.StatusServiceUnavailable,
		restBody(http.StatusServiceUnavailable, "UNAVAILABLE", err.Error(), details(err, "INTERNAL_ERROR", timestamp()))
}

// JSONRPCErrorBody is a JSON-RPC error response for request id (null when id is empty).
func JSONRPCErrorBody(id json.RawMessage, e RPCError) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   RPCError        `json:"error"`
	}{"2.0", id, e})
	if err != nil {
		e.Data = nil
		b, _ = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   RPCError        `json:"error"`
		}{"2.0", id, e})
	}
	return append(b, '\n')
}

// WriteError writes err as an ordinary response of binding b, before any SSE header (spec §9): on
// JSON-RPC a JSON-RPC error response carrying the request's id, with HTTP status 200 as a2a-go answers
// every JSON-RPC error, or status when it is not zero (401, 404); on HTTP+JSON a google.rpc.Status with
// HTTPError's status, or status when it is not zero. The message is Public's: an internal error's cause
// is never written (the caller logs it).
//
// SPEC-CONFLICT (D86): spec §2.4 ("VersionNotSupportedError (-32009, HTTP 400)"), §7 and §14.1 ("-32603 (HTTP
// 503)") give a JSON-RPC code and an HTTP status without saying which binding the status belongs to. Sent
// on the JSON-RPC route, a 400 or 503 hides the error from A2A clients: a2a-go's JSON-RPC client reads a
// JSON-RPC error only from an HTTP 200 response and reports any other status as "unexpected HTTP status"
// (a2aclient/jsonrpc.go), and a2a-go's own JSON-RPC server answers every error with 200. Closest faithful
// behaviour: the code on JSON-RPC (HTTP 200), the status on HTTP+JSON, as A2A §3.3.2's error table maps
// them. The spec's explicit JSON-RPC statuses are kept: the unknown-service answer is HTTP 404 with a
// JSON-RPC body (§2.1), and a refused credential is 401.
func WriteError(w http.ResponseWriter, b Binding, id json.RawMessage, status int, err error) {
	var body []byte
	if b == JSONRPC {
		body = JSONRPCErrorBody(id, ToJSONRPC(public(err)))
		if status == 0 {
			status = http.StatusOK
		}
	} else {
		var s int
		s, body = HTTPError(err)
		if status == 0 {
			status = s
		}
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
