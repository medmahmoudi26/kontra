package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/hooks"
	a2asse "kontra.sh/kontra/internal/sse"
)

// An Admission deny's code (spec §13.1, D325): A2A's own codes are their errors; the operator's (-32099 to -32050,
// Cloud's -32060 to -32063) are PERMISSION_DENIED on the A2A bindings with the code in the error's data, and the code
// itself to /mcp (CodeOf); any other code, and a deny without a refusal, is PERMISSION_DENIED with the message alone.
func TestDeniedCodes(t *testing.T) {
	for _, c := range []struct {
		code     int
		sentinel error
		operator bool
	}{
		{-32060, a2a.ErrUnauthorized, true},  // Cloud's seller_not_payable
		{-32061, a2a.ErrUnauthorized, true},  // payment_method_required
		{-32062, a2a.ErrUnauthorized, true},  // monthly_cap_reached
		{-32063, a2a.ErrUnauthorized, true},  // listing_unavailable
		{-32050, a2a.ErrUnauthorized, true},  // the range's ends
		{-32099, a2a.ErrUnauthorized, true},  //
		{-32049, a2a.ErrUnauthorized, false}, // just outside it
		{-32100, a2a.ErrUnauthorized, false}, //
		{-32042, a2a.ErrUnauthorized, false}, // MCP's URL elicitation: never an operator's
		{403, a2a.ErrUnauthorized, false},
		{-32602, a2a.ErrInvalidParams, false}, // an A2A code is its error
		{-32004, a2a.ErrUnsupportedOperation, false},
	} {
		err := denied(&hooks.Refusal{Code: c.code, Message: "this service's seller cannot be paid yet"})
		var oe *OperatorRefusalError
		if !errors.Is(err, c.sentinel) || errors.As(err, &oe) != c.operator || err.Error() != "this service's seller cannot be paid yet" {
			t.Errorf("%d: %v (operator %v)", c.code, err, oe)
			continue
		}
		code, msg := CodeOf(err)
		wantCode := codeForError(err)
		if c.operator {
			wantCode = c.code
		}
		if code != wantCode || msg != "this service's seller cannot be paid yet" {
			t.Errorf("%d: CodeOf %d %q, want %d", c.code, code, msg, wantCode)
		}
		// The A2A bindings: JSON-RPC and HTTP+JSON, as the gate writes them (sse, held to a2a-go by TestErrorMappingDrift).
		rpc := a2asse.ToJSONRPC(err)
		data, _ := json.Marshal(rpc.Data)
		var details []map[string]any
		_ = json.Unmarshal(data, &details)
		gotCode, has := any(nil), false
		for _, d := range details {
			if d["@type"] == "type.googleapis.com/google.protobuf.Struct" {
				gotCode, has = d["code"], true
			}
		}
		if c.operator != has || (has && gotCode != float64(c.code)) {
			t.Errorf("%d: JSON-RPC data %s, want the code in it: %v", c.code, data, c.operator)
		}
		if c.operator {
			status, body := a2asse.ToHTTP(err)
			var rest struct {
				Error struct {
					Status  string           `json:"status"`
					Message string           `json:"message"`
					Details []map[string]any `json:"details"`
				} `json:"error"`
			}
			if json.Unmarshal(body, &rest) != nil || status != http.StatusForbidden || rest.Error.Status != "PERMISSION_DENIED" ||
				rpc.Code != -31403 || len(rest.Error.Details) == 0 || rest.Error.Details[0]["code"] != float64(c.code) {
				t.Errorf("%d: JSON-RPC %d; HTTP+JSON %d %s", c.code, rpc.Code, status, body)
			}
		}
	}
	if err := denied(nil); !errors.Is(err, a2a.ErrUnauthorized) || err.Error() != "admission denied" {
		t.Errorf("a deny without a refusal: %v", err)
	}
	if err := denied(&hooks.Refusal{Code: -32060}); err.Error() != "admission denied" {
		t.Errorf("an operator's code without a message: %v", err)
	}
	if code, _ := CodeOf(denied(&hooks.Refusal{Code: -32060})); code != -32060 {
		t.Errorf("CodeOf an operator's code without a message: %d", code)
	}
}
