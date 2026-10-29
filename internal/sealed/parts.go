package sealed

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"

	"kontra.sh/kontra/internal/strictjson"
)

// blobScheme is internal/blob.URIScheme (spec/relay.md §10.4, D181), repeated here so that this leaf does not
// import the bucket's client; a test keeps the two equal.
const blobScheme = "kontra-blob:"

// envelopeType is the envelope's type/subtype without its parameter, lowercased (ParsePart, claimsType).
const envelopeType = "application/vnd.kontra.sealed+age"

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

// sealedMeta is metadata["kontra.sealed"] of a sealed part.
type sealedMeta struct {
	Original string `json:"original"`
}

// MarshalPart returns the part's A2A 1.0 JSON.
//
// With Ciphertext: {"raw": <base64>, "mediaType": MediaType, "filename"?, "metadata": {"kontra.sealed":
// {"original": …}}}. With BlobRef instead (the gate's form for a node, spec/relay.md §10.4): the same with
// "url": "kontra-blob:<ref>" in place of raw. Exactly one of the two, and Original, must be set.
func MarshalPart(p Part) (json.RawMessage, error) {
	if p.Original == "" {
		return nil, errors.New("sealed: part: no original media type or kind")
	}
	meta := map[string]sealedMeta{MetadataKey: {Original: p.Original}}
	switch {
	case p.BlobRef != "" && len(p.Ciphertext) > 0:
		return nil, errors.New("sealed: part: both ciphertext and a blob ref")
	case p.BlobRef != "":
		if !validRef(p.BlobRef) {
			return nil, fmt.Errorf("sealed: part: blob ref %q is not a ref", p.BlobRef)
		}
		return json.Marshal(struct {
			URL       string                `json:"url"`
			MediaType string                `json:"mediaType"`
			Filename  string                `json:"filename,omitempty"`
			Metadata  map[string]sealedMeta `json:"metadata"`
		}{blobScheme + p.BlobRef, MediaType, p.Filename, meta})
	case len(p.Ciphertext) > 0:
		return json.Marshal(struct {
			Raw       string                `json:"raw"`
			MediaType string                `json:"mediaType"`
			Filename  string                `json:"filename,omitempty"`
			Metadata  map[string]sealedMeta `json:"metadata"`
		}{base64.StdEncoding.EncodeToString(p.Ciphertext), MediaType, p.Filename, meta})
	default:
		return nil, errors.New("sealed: part: no ciphertext")
	}
}

// ParsePart reports whether raw is a part with the envelope media type and, if so, decodes it. ok is true with
// an error for a part that claims the envelope type in another form (the gate's refusal, spec §3).
//
// A part claims the envelope type when any member whose name is "mediaType" under case folding (a2a-go decodes
// a Part case-insensitively) is a string naming the envelope type in any spelling media types allow: type,
// subtype and parameter names in any case, white space around ";", a quoted "1", other parameters beside v=1
// (RFC 9110 §8.3.1). A value mime.ParseMediaType cannot read that starts, case-insensitively, with
// "application/vnd.kontra.sealed+age" claims it too, since another reader may take it for the envelope. Another
// version (v=2) is not this envelope type: such a part is an ordinary one, unsealed for envelope v1 (which a
// required service refuses, spec/ext-sealed.md §5). A claiming part is accepted only in exactly the form
// MarshalPart writes with Ciphertext: the members raw (non-empty standard base64), mediaType (exactly MediaType),
// filename (a string) and metadata (an object), with no repeated or case-variant member at the part's level or in
// metadata; metadata["kontra.sealed"], when present, must be exactly {"original": <string>}. Every other form,
// text, url (a kontra-blob: one included) and data among them, is ok true with an error. A part that does not
// claim the type is ok false with no error; raw that is not one JSON object is ok false with an error.
func ParsePart(raw json.RawMessage) (p Part, ok bool, err error) {
	return parsePart(raw, false)
}

// ParseDeliveredPart is ParsePart for a node reading what the gate delivers (spec/relay.md §10.4): it also
// accepts the url form "kontra-blob:<ref>" with exactly MarshalPart's other members, returning BlobRef. The gate
// never uses it: a caller's or a service's kontra-blob: part is refused (D181), and ParsePart refuses it too.
func ParseDeliveredPart(raw json.RawMessage) (p Part, ok bool, err error) {
	return parsePart(raw, true)
}

func parsePart(raw json.RawMessage, blobs bool) (Part, bool, error) {
	pairs, err := topPairs(raw)
	if err != nil {
		return Part{}, false, fmt.Errorf("sealed: part: %w", err)
	}
	claims := false
	for _, kv := range pairs {
		if strings.EqualFold(kv.key, "mediaType") && claimsType(kv.val) {
			claims = true
			break
		}
	}
	if !claims {
		return Part{}, false, nil
	}
	refuse := func(format string, a ...any) (Part, bool, error) {
		return Part{}, true, fmt.Errorf("sealed: a part with the envelope media type must be exactly the raw form: %s",
			fmt.Sprintf(format, a...))
	}
	m, err := strictjson.Members(raw)
	if err != nil {
		return refuse("%v", err)
	}
	var mt string
	if v, ok := m["mediaType"]; !ok || !isString(v) || json.Unmarshal(v, &mt) != nil || mt != MediaType {
		return refuse("mediaType is not exactly %q", MediaType)
	}
	var p Part
	_, hasRaw := m["raw"]
	_, hasURL := m["url"]
	for k := range m {
		switch k {
		case "raw", "mediaType", "filename", "metadata":
		case "url":
			if !blobs || hasRaw {
				return refuse("it is in url form")
			}
		case "text", "data":
			return refuse("it is in %s form", k)
		default:
			return refuse("member %q", k)
		}
	}
	switch {
	case hasRaw:
		var s string
		if v := m["raw"]; !isString(v) || json.Unmarshal(v, &s) != nil {
			return refuse("raw is not a string")
		}
		ct, ok := decodeStd(s)
		if !ok {
			return refuse("raw is not standard base64 of a ciphertext")
		}
		p.Ciphertext = ct
	case hasURL:
		var s string
		if v := m["url"]; !isString(v) || json.Unmarshal(v, &s) != nil {
			return refuse("url is not a string")
		}
		ref, ok := strings.CutPrefix(s, blobScheme)
		if !ok || !validRef(ref) {
			return refuse("url is not %s<ref>", blobScheme)
		}
		p.BlobRef = ref
	default:
		return refuse("no raw member")
	}
	if v, ok := m["filename"]; ok {
		if !isString(v) || json.Unmarshal(v, &p.Filename) != nil {
			return refuse("filename is not a string")
		}
	}
	if v, ok := m["metadata"]; ok {
		meta, err := strictjson.Members(v)
		if err != nil {
			return refuse("metadata: %v", err)
		}
		if sv, ok := meta[MetadataKey]; ok {
			sm, err := strictjson.Members(sv)
			if err != nil || len(sm) != 1 {
				return refuse("metadata[%q] is not {\"original\": …}", MetadataKey)
			}
			o, ok := sm["original"]
			if !ok || !isString(o) || json.Unmarshal(o, &p.Original) != nil {
				return refuse("metadata[%q].original is not a string", MetadataKey)
			}
		}
	}
	return p, true, nil
}

// claimsType reports whether v is a JSON string naming the envelope type in any spelling (ParsePart).
func claimsType(v json.RawMessage) bool {
	var s string
	if !isString(v) || json.Unmarshal(v, &s) != nil {
		return false
	}
	mt, params, err := mime.ParseMediaType(s)
	if err != nil {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), envelopeType)
	}
	return mt == envelopeType && params["v"] == "1"
}

type pair struct {
	key string
	val json.RawMessage
}

// topPairs returns every member of one JSON object in order, repeated names included.
func topPairs(raw []byte) ([]pair, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var out []pair
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := tok.(string)
		if !ok {
			return nil, errors.New("a member name is not a string")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, pair{k, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("data after the part")
	}
	return out, nil
}

// MCPEnvelope is `_meta["sh.kontra/sealed"]` of a sealed tools/call (whose arguments are then {}), of the
// params of a sealed upstream elicitation/create or sampling/createMessage, and of the hiring node's sealed
// answer to one (spec/ext-sealed.md §3, D62). The gate's scrub of Kontra's keys keeps it exactly there, on a
// sealed service, when it has these two members and no other (spec/relay.md §2.5, D174).
type MCPEnvelope struct {
	V        int    `json:"v"`
	Envelope string `json:"envelope"` // base64 of the age file
}

// NewMCPEnvelope is the envelope of a ciphertext: {"v": 1, "envelope": <standard base64>}.
func NewMCPEnvelope(ciphertext []byte) MCPEnvelope {
	return MCPEnvelope{V: Version, Envelope: base64.StdEncoding.EncodeToString(ciphertext)}
}

// ParseMCPEnvelope returns the ciphertext of `_meta["sh.kontra/sealed"]`, refusing anything but exactly {"v": 1,
// "envelope": <standard base64>} read with exact keys (the rule of internal/mcpwire.SealedEnvelope): a third
// member, a repeated or case-variant one, another v (1.0 included) or an envelope that is not a non-empty base64
// string.
func ParseMCPEnvelope(v json.RawMessage) ([]byte, error) {
	m, err := strictjson.Members(v)
	if err != nil {
		return nil, fmt.Errorf("sealed: MCP envelope: %w", err)
	}
	if len(m) != 2 {
		return nil, errors.New("sealed: MCP envelope: want exactly v and envelope")
	}
	if ver, ok := m["v"]; !ok || !bytes.Equal(bytes.TrimSpace(ver), []byte("1")) {
		return nil, errors.New("sealed: MCP envelope: v is not 1")
	}
	var s string
	if env, ok := m["envelope"]; !ok || !isString(env) || json.Unmarshal(env, &s) != nil {
		return nil, errors.New("sealed: MCP envelope: envelope is not a string")
	}
	ct, ok := decodeStd(s)
	if !ok {
		return nil, errors.New("sealed: MCP envelope: envelope is not standard base64 of a ciphertext")
	}
	return ct, nil
}

// MCPResult is the CallToolResult a hosting node returns for a sealed call: content replaced by one embedded
// resource {uri kontra:sealed, mimeType MediaType, blob}, structuredContent dropped, isError kept outside the
// envelope because the gate meters on it (spec/relay.md §11.4).
func MCPResult(ciphertext []byte, isError bool) (json.RawMessage, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("sealed: MCP result: no ciphertext")
	}
	type resource struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType"`
		Blob     string `json:"blob"`
	}
	type content struct {
		Type     string   `json:"type"`
		Resource resource `json:"resource"`
	}
	return json.Marshal(struct {
		Content []content `json:"content"`
		IsError bool      `json:"isError"`
	}{[]content{{"resource", resource{ResourceURI, MediaType, base64.StdEncoding.EncodeToString(ciphertext)}}}, isError})
}

// ParseMCPResult returns the ciphertext of a sealed CallToolResult, ok false for a result that is not one.
//
// A result is one when any content item is a resource whose uri is "kontra:sealed" or whose mimeType claims the
// envelope type. It must then be exactly MCPResult's shape, read with exact keys: content one item {"type":
// "resource", "resource": {"uri", "mimeType", "blob"}} with those values, no structuredContent, and isError, when
// present, a boolean (other members, _meta among them, are the caller's to read); otherwise ok is true with an
// error.
func ParseMCPResult(raw json.RawMessage) (ciphertext []byte, ok bool, err error) {
	m, err := strictjson.Members(raw)
	if err != nil {
		return nil, false, fmt.Errorf("sealed: MCP result: %w", err)
	}
	cv, has := m["content"]
	if !has {
		return nil, false, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(cv, &items); err != nil {
		return nil, false, nil
	}
	claims := false
	for _, it := range items {
		if claimsResource(it) {
			claims = true
			break
		}
	}
	if !claims {
		return nil, false, nil
	}
	refuse := func(why string) ([]byte, bool, error) {
		return nil, true, fmt.Errorf("sealed: a sealed MCP result must be exactly one %s resource: %s", ResourceURI, why)
	}
	if len(items) != 1 {
		return refuse("more than one content item")
	}
	if _, has := m["structuredContent"]; has {
		return refuse("structuredContent is present")
	}
	if v, has := m["isError"]; has {
		if s := string(bytes.TrimSpace(v)); s != "true" && s != "false" {
			return refuse("isError is not a boolean")
		}
	}
	item, err := strictjson.Members(items[0])
	if err != nil || len(item) != 2 {
		return refuse("the content item is not {type, resource}")
	}
	var typ string
	if v, has := item["type"]; !has || !isString(v) || json.Unmarshal(v, &typ) != nil || typ != "resource" {
		return refuse("the content item's type is not resource")
	}
	res, err := strictjson.Members(item["resource"])
	if err != nil || len(res) != 3 {
		return refuse("the resource is not {uri, mimeType, blob}")
	}
	for name, want := range map[string]string{"uri": ResourceURI, "mimeType": MediaType} {
		var s string
		if v, has := res[name]; !has || !isString(v) || json.Unmarshal(v, &s) != nil || s != want {
			return refuse(name + " is not " + want)
		}
	}
	var blob string
	if v, has := res["blob"]; !has || !isString(v) || json.Unmarshal(v, &blob) != nil {
		return refuse("blob is not a string")
	}
	ct, ok := decodeStd(blob)
	if !ok {
		return refuse("blob is not standard base64 of a ciphertext")
	}
	return ct, true, nil
}

// claimsResource reports whether a content item is an embedded resource naming kontra:sealed or the envelope
// type, under any member-name case.
func claimsResource(item json.RawMessage) bool {
	pairs, err := topPairs(item)
	if err != nil {
		return false
	}
	for _, kv := range pairs {
		if !strings.EqualFold(kv.key, "resource") {
			continue
		}
		inner, err := topPairs(kv.val)
		if err != nil {
			continue
		}
		for _, in := range inner {
			var s string
			if !isString(in.val) || json.Unmarshal(in.val, &s) != nil {
				continue
			}
			if strings.EqualFold(in.key, "uri") && strings.EqualFold(strings.TrimSpace(s), ResourceURI) {
				return true
			}
			if strings.EqualFold(in.key, "mimeType") && claimsType(in.val) {
				return true
			}
		}
	}
	return false
}

// decodeStd decodes non-empty standard base64 with padding, in the alphabet only (Go's decoder would skip CR
// and LF).
func decodeStd(s string) ([]byte, bool) {
	if s == "" || len(s)%4 != 0 {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
			return nil, false
		}
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// validRef is a blob ref as internal/blob mints them: lowercase letters and digits.
func validRef(ref string) bool {
	if ref == "" || len(ref) > 128 {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isString(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	return len(v) > 0 && v[0] == '"'
}
