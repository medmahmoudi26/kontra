package sse

import (
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ToJSONRPC maps an error to A2A v1.0.1's JSON-RPC error (-32001 TaskNotFound … -32009
// VersionNotSupported, google.rpc.ErrorInfo in data). a2a-go's own mapping is internal to its module, so
// the gate keeps this copy and a test that compares it with what a2a-go's JSON-RPC handler writes for
// every error, so a bump that changes the mapping fails the build (D7).
func ToJSONRPC(err error) RPCError {
	panic(unimpl.Error("sse.ToJSONRPC", "WP-06"))
}

// ToHTTP maps an error to the HTTP+JSON binding's status and google.rpc.Status body.
func ToHTTP(err error) (status int, body []byte) {
	panic(unimpl.Error("sse.ToHTTP", "WP-06"))
}
