package a2ahttp

import "kontra.sh/kontra/internal/strictjson"

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
// stream, tenantOf) and a2a-go's decoding see the same members. M3 WP-04 calls it in ServeHTTP after the
// credential and service checks and before serveJSONRPC reads the body; a2a_case_smuggling covers it.
func strictJSONRPC(body []byte) error {
	return strictjson.Check(body, jsonrpcLevels...)
}
