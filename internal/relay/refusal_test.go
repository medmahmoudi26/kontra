package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/hooks"
	a2asse "kontra.sh/kontra/internal/sse"
)

// An Admission deny's code (spec §13.1, D325): A2A's own codes are their errors; the operator's (-32099 to -32050,
// Cloud's -32060 to -32063) are PERMISSION_DENIED on the A2A bindings with the code in the error's data, and the code
// itself to /mcp (CodeOf); any other code, and a deny without a refusal, is PERMISSION_DENIED with the message alone,
// and -32603 to /mcp.
func TestDeniedCodes(t *testing.T) {
	for _, c := range []struct {
		code     int
		sentinel error
		operator bool
		mcp      int // what /mcp answers (CodeOf)
	}{
		{-32060, a2a.ErrUnauthorized, true, -32060},  // Cloud's seller_not_payable
		{-32061, a2a.ErrUnauthorized, true, -32061},  // payment_method_required
		{-32062, a2a.ErrUnauthorized, true, -32062},  // monthly_cap_reached
		{-32063, a2a.ErrUnauthorized, true, -32063},  // listing_unavailable
		{-32050, a2a.ErrUnauthorized, true, -32050},  // the range's ends
		{-32099, a2a.ErrUnauthorized, true, -32099},  //
		{-32049, a2a.ErrUnauthorized, false, -32603}, // just outside it
		{-32100, a2a.ErrUnauthorized, false, -32603}, //
		{-32042, a2a.ErrUnauthorized, false, -32603}, // MCP's URL elicitation: never an operator's
		{-32000, a2a.ErrUnauthorized, false, -32603},
		{403, a2a.ErrUnauthorized, false, -32603},
		{-32602, a2a.ErrInvalidParams, false, -32602}, // an A2A code is its error
		{-32004, a2a.ErrUnsupportedOperation, false, -32004},
	} {
		err := denied(&hooks.Refusal{Code: c.code, Message: "this service's seller cannot be paid yet"})
		var oe *OperatorRefusalError
		if !errors.Is(err, c.sentinel) || errors.As(err, &oe) != c.operator || err.Error() != "this service's seller cannot be paid yet" {
			t.Errorf("%d: %v (operator %v)", c.code, err, oe)
			continue
		}
		if code, msg := CodeOf(err); code != c.mcp || msg != "this service's seller cannot be paid yet" {
			t.Errorf("%d: CodeOf %d %q, want %d", c.code, code, msg, c.mcp)
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
		if !c.operator && c.sentinel == a2a.ErrUnauthorized && rpc.Code != -31403 {
			t.Errorf("%d: JSON-RPC %d, want PERMISSION_DENIED (-31403)", c.code, rpc.Code)
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

// The texts an operator reads to choose a deny's code say what the gate does with it (G3's review): hooks.json's
// refusal.code repeats relay.md §13.1's rule for a code that is neither A2A's nor the operator's, which /mcp answers
// as CodeOf does (-32603) and the A2A routes as PERMISSION_DENIED with the message alone, and §13.1's word on
// /mcp/{svc}, which answers every code as it is (the MCP relay's own tests hold it to that).
func TestDeniedCodesAreWhatTheTextsSay(t *testing.T) {
	other, _ := CodeOf(denied(&hooks.Refusal{Code: -32000, Message: "no"}))
	rpc := a2asse.ToJSONRPC(denied(&hooks.Refusal{Code: -32000, Message: "no"}))
	if data, _ := json.Marshal(rpc.Data); rpc.Code != -31403 || rpc.Message != "no" || strings.Contains(string(data), "google.protobuf.Struct") {
		t.Fatalf("a code neither A2A's nor the operator's is JSON-RPC %d %q with data %s, want PERMISSION_DENIED with the message alone",
			rpc.Code, rpc.Message, data)
	}
	rules := []string{
		fmt.Sprintf("Any other code is PERMISSION_DENIED with the message alone on the A2A routes and %d on /mcp.", other),
		"/mcp/{svc} answers every code as it is.",
	}
	flat := func(s string) string { return strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "")), " ") }

	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "relay.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(b), "**A deny's code (D325).**")
	section, _, _ = strings.Cut(section, "\nRules:")
	b, err = os.ReadFile(filepath.Join("..", "..", "spec", "schema", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	desc := schema.Defs["decision"].Properties["refusal"].Properties["code"].Description
	if section == "" || desc == "" {
		t.Fatalf("relay.md §13.1's deny codes (%d bytes) or hooks.json's refusal.code description (%d bytes) not found", len(section), len(desc))
	}
	for _, rule := range rules {
		if !strings.Contains(flat(section), rule) {
			t.Errorf("relay.md §13.1 does not say %q", rule)
		}
		if !strings.Contains(flat(desc), rule) {
			t.Errorf("hooks.json's refusal.code description does not say %q:\n%s", rule, desc)
		}
	}
}
