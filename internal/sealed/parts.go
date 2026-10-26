package sealed

import (
	"encoding/json"

	"kontra.sh/kontra/internal/unimpl"
)

// Part is a sealed A2A 1.0 part (spec/ext-sealed.md §3): raw ciphertext, the envelope media type, the original
// filename if any, and metadata["kontra.sealed"] = {"original": <the original mediaType, or the kind>}. Any
// other form of a part with the envelope type is refused: InvalidRequest from a caller, upstream_error from a
// service. A part over 256 KiB of ciphertext travels as a url part `kontra-blob:<ref>` between the gate and a
// node (spec/relay.md §10.4); BlobRef is then set and Ciphertext empty until the node has fetched it.
type Part struct {
	Ciphertext []byte
	Filename   string
	Original   string
	BlobRef    string
}

// MarshalPart returns the part's A2A 1.0 JSON.
func MarshalPart(p Part) (json.RawMessage, error) {
	return nil, unimpl.Error("sealed.MarshalPart", "M3 WP-01")
}

// ParsePart reports whether raw is a part with the envelope media type and, if so, decodes it. ok is true with
// an error for a part that claims the envelope type in another form (the gate's refusal, spec §3).
func ParsePart(raw json.RawMessage) (p Part, ok bool, err error) {
	return Part{}, false, unimpl.Error("sealed.ParsePart", "M3 WP-01")
}

// MCPEnvelope is `_meta["sh.kontra/sealed"]` of a sealed tools/call (whose arguments are then {}), of the
// params of a sealed upstream elicitation/create or sampling/createMessage, and of the hiring node's sealed
// answer to one (spec/ext-sealed.md §3, D62). The gate's scrub of Kontra's keys keeps it exactly there, on a
// sealed service, when it has these two members and no other (spec/relay.md §2.5, D174).
type MCPEnvelope struct {
	V        int    `json:"v"`
	Envelope string `json:"envelope"` // base64 of the age file
}

// MCPResult is the CallToolResult a hosting node returns for a sealed call: content replaced by one embedded
// resource {uri kontra:sealed, mimeType MediaType, blob}, structuredContent dropped, isError kept outside the
// envelope because the gate meters on it (spec/relay.md §11.4).
func MCPResult(ciphertext []byte, isError bool) (json.RawMessage, error) {
	return nil, unimpl.Error("sealed.MCPResult", "M3 WP-01")
}

// ParseMCPResult returns the ciphertext of a sealed CallToolResult, ok false for a result that is not one.
func ParseMCPResult(raw json.RawMessage) (ciphertext []byte, ok bool, err error) {
	return nil, false, unimpl.Error("sealed.ParseMCPResult", "M3 WP-01")
}
