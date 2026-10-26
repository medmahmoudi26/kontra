package a2ahttp

import (
	"errors"

	"kontra.sh/kontra/internal/strictjson"
)

// jsonrpcLevels are the levels of an A2A JSON-RPC request the gate reads itself or through a2a-go's second
// decoding, below the request object (spec/relay.md §2.4, D172): params (tenant, the task id), params.metadata,
// params.configuration, params.message, params.message.metadata, each of params.message.parts and each part's
// metadata (the kontra. keys of spec §2.5).
var jsonrpcLevels = []strictjson.Path{
	{"params"},
	{"params", "metadata"},
	{"params", "configuration"},
	{"params", "message"},
	{"params", "message", "metadata"},
	{"params", "message", "parts", "*"},
	{"params", "message", "parts", "*", "metadata"},
}

// strictJSONRPC refuses a JSON-RPC body whose keys repeat, or differ only in case, at a level the gate reads
// (InvalidRequestError, -32600, HTTP 400), so that ServeHTTP's own reads (rpcID, the method that routes a
// stream, tenantOf) and a2a-go's decoding see the same members. ServeHTTP calls it (through strictRefusal) after
// the credential and service checks and before serveJSONRPC reads the body; a2a_case_smuggling covers it.
func strictJSONRPC(body []byte) error {
	return strictjson.Check(body, jsonrpcLevels...)
}

// strictRefusal is strictJSONRPC's refusal of a body, nil when it passes. Only an ambiguous level (a
// *strictjson.KeyError) is refused here: a body that is not JSON or not an object is left to a2a-go's own
// answer (-32700 or -32600), as before M3, since there both readers see the same nothing.
//
// A lone case variant ({"METHOD": …} alone, "Tenant") is not refused: every reader of this route decodes with
// encoding/json (rpcID, the stream routing and tenantOf here, a2a-go's handler after), which takes it for the
// member alike, and the request a2a-go decodes is the one the gate acts on and re-encodes upstream.
func strictRefusal(body []byte) error {
	err := strictJSONRPC(body)
	var ke *strictjson.KeyError
	if errors.As(err, &ke) {
		return err
	}
	return nil
}
