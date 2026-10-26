// Package card produces the served A2A card from the upstream card, as raw JSON (spec/relay.md §3, D11,
// D12, D52).
//
// The upstream card is kept verbatim (services.upstream_card) and served at
// {svc}/.well-known/agent-card.upstream.json. The served card is the upstream card normalized to the 1.0
// shape (0.2.x and 0.3 fields renamed, unknown fields kept) with exactly these fields set by the gate:
// supportedInterfaces, securitySchemes, securityRequirements, capabilities.pushNotifications,
// capabilities.streaming, capabilities.extendedAgentCard, capabilities.extensions (operator entries replace
// same-URI seller entries) and signatures. A card is never round-tripped through a typed struct: every
// other member keeps its bytes and its place.
//
// M1 rewrites without signing (PRD §14): the served card carries no signatures field and the seller's
// signatures are not copied (D13). Signer is the M3 seam.
package card

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Refresh is how often a serving replica re-fetches a card (spec §3).
const Refresh = 10 * time.Minute

// ErrBadCard is an upstream card the gate cannot serve: not a JSON object, or a malformed member the
// rewrite has to read.
var ErrBadCard = errors.New("card: unusable agent card")

// Input is everything the rewrite needs besides the upstream card.
type Input struct {
	Upstream      []byte // the upstream card, verbatim
	PublicURL     string // {public_url}
	ServicePath   string // {svc}: /agents/{name} or /agents/{tenant}/{name}
	OnlyAnonymous bool   // allow: is exactly [anonymous]: no security fields
	OIDC          bool   // add an openIdConnect scheme (operator IdP)
	// OIDCIssuer is the IdP's issuer URL; the scheme's openIdConnectUrl is its discovery document. OIDC
	// principals are not built in M1, so this stays empty there.
	OIDCIssuer     string
	SealedURI      string            // https://kontra.sh/ext/sealed/v1 for a sealed service (M3), else ""
	CardExtensions []json.RawMessage // the operator's card_extensions annotation (spec §13.4)
}

// member is one name/value pair of a JSON object, the value's bytes untouched.
type member struct {
	key string
	val json.RawMessage
}

// object is a JSON object in document order, duplicates included.
type object []member

func parseObject(b []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("%w: not a JSON object", ErrBadCard)
	}
	var o object
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: object key %v", ErrBadCard, tok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%w: member %q: %v", ErrBadCard, key, err)
		}
		o = append(o, member{key: key, val: raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: data after the card object", ErrBadCard)
	}
	return o, nil
}

func (o object) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

func (o object) has(key string) bool { _, ok := o.get(key); return ok }

// set replaces every member named key with one, at the first one's place, or appends it.
func (o object) set(key string, val json.RawMessage) object {
	out := make(object, 0, len(o)+1)
	done := false
	for _, m := range o {
		if m.key != key {
			out = append(out, m)
			continue
		}
		if !done {
			out = append(out, member{key: key, val: val})
			done = true
		}
	}
	if !done {
		out = append(out, member{key: key, val: val})
	}
	return out
}

// rename moves the value of from to the key to at from's place (to's own members are dropped).
func (o object) rename(from, to string) object {
	val, ok := o.get(from)
	if !ok {
		return o
	}
	out := make(object, 0, len(o))
	done := false
	for _, m := range o {
		switch {
		case m.key == to:
			continue
		case m.key == from:
			if !done {
				out = append(out, member{key: to, val: val})
				done = true
			}
		default:
			out = append(out, m)
		}
	}
	return out
}

func (o object) del(keys ...string) object {
	out := make(object, 0, len(o))
	for _, m := range o {
		drop := false
		for _, k := range keys {
			if m.key == k {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, m)
		}
	}
	return out
}

// marshal writes the object: keys JSON-encoded without HTML escaping, values verbatim.
func (o object) marshal() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(m.key))
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func mustJSON(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

// IsV1 reports whether a card has the 1.0 shape (supportedInterfaces).
func IsV1(upstream []byte) bool {
	o, err := parseObject(upstream)
	return err == nil && o.has("supportedInterfaces")
}

// v0Members are the 0.x card members that 1.0 replaced: the interface members and their protocolVersion,
// supportsAuthenticatedExtendedCard (capabilities.extendedAgentCard) and security (securityRequirements).
var v0Members = []string{"url", "preferredTransport", "additionalInterfaces", "protocolVersion", "supportsAuthenticatedExtendedCard", "security"}

// Normalize converts a pre-1.0 card to the 1.0 shape as raw JSON; a 1.0 card is returned as is (spec §3,
// D12). The 0.x members a2a-go's a2av0 parser maps are renamed the same way, and every other member is
// kept byte for byte, in its place:
//
//   - url, preferredTransport (default JSONRPC, which is what a 0.2.x card without it means) and
//     additionalInterfaces become supportedInterfaces, with the card's protocolVersion (default 0.3);
//     the top-level protocolVersion goes with them;
//   - supportsAuthenticatedExtendedCard becomes capabilities.extendedAgentCard;
//   - security becomes securityRequirements, on the card and on each skill, in the 1.0 shape;
//   - securitySchemes' OpenAPI-style entries become the 1.0 one-of wrappers.
//
// A card with both shapes (supportedInterfaces and 0.x members, as a2a-go v2's a2av0 card producers write
// for a seller that also serves 0.3 clients) keeps its supportedInterfaces and loses its 0.x members: the
// 0.x interface members go, supportsAuthenticatedExtendedCard and security are converted as above where the
// card has no 1.0 member of their own and dropped otherwise, and so is each skill's security.
//
// SPEC-CONFLICT (D88): spec §3 says "a 1.0 card is used as is" and keeps every seller-authored member but the
// gate's; read literally, a combined card's url, additionalInterfaces, preferredTransport and
// protocolVersion "0.3" would be served beside the gate's supportedInterfaces, so a 0.3 client reading the
// served card would dial the seller's own (often internal) address, past the gate's allow: list and
// authentication, against §3's "supportedInterfaces ... Never an upstream URL" and §2.4's 1.0-only
// downstream; and its security and supportsAuthenticatedExtendedCard would contradict the gate's own
// securityRequirements and extendedAgentCard: false (D52). The closest faithful behaviour: those 0.x
// members are the 0.x spelling of the very fields §3 gives the gate, so a combined card is normalized like
// a 0.x card for them, and a pure 1.0 card is still returned as is.
func Normalize(upstream []byte) ([]byte, error) {
	o, err := parseObject(upstream)
	if err != nil {
		return nil, err
	}
	if o.has("supportedInterfaces") {
		combined := slices.ContainsFunc(v0Members, o.has)
		if !combined {
			combined, err = skillsHaveV0Security(o)
			if err != nil {
				return nil, err
			}
		}
		if !combined {
			return upstream, nil
		}
		o = o.del("url", "preferredTransport", "additionalInterfaces", "protocolVersion")
		if o.has("securityRequirements") {
			o = o.del("security")
		}
		caps, err := capsObject(o)
		if err != nil {
			return nil, err
		}
		if caps.has("extendedAgentCard") {
			o = o.del("supportsAuthenticatedExtendedCard")
		}
		return normalizeV0Rest(o)
	}
	version := "0.3"
	if raw, ok := o.get("protocolVersion"); ok {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			version = v
		}
	}
	ifaces, err := v0Interfaces(o, version)
	if err != nil {
		return nil, err
	}
	// supportedInterfaces takes url's place (or the end); the other 0.x interface members go.
	if o.has("url") {
		o = o.rename("url", "supportedInterfaces").set("supportedInterfaces", mustJSON(ifaces))
	} else {
		o = o.set("supportedInterfaces", mustJSON(ifaces))
	}
	o = o.del("preferredTransport", "additionalInterfaces", "protocolVersion")
	return normalizeV0Rest(o)
}

// normalizeV0Rest converts supportsAuthenticatedExtendedCard, security, securitySchemes and each skill's
// security, once the interface members are in the 1.0 shape.
func normalizeV0Rest(o object) ([]byte, error) {
	if raw, ok := o.get("supportsAuthenticatedExtendedCard"); ok {
		var ext bool
		if err := json.Unmarshal(raw, &ext); err != nil {
			return nil, fmt.Errorf("%w: supportsAuthenticatedExtendedCard: %v", ErrBadCard, err)
		}
		caps, err := capsObject(o)
		if err != nil {
			return nil, err
		}
		if ext {
			caps = caps.set("extendedAgentCard", json.RawMessage("true"))
		}
		o = o.set("capabilities", caps.marshal()).del("supportsAuthenticatedExtendedCard")
	}
	if raw, ok := o.get("security"); ok {
		req, err := v0Security(raw)
		if err != nil {
			return nil, err
		}
		o = o.rename("security", "securityRequirements").set("securityRequirements", req)
	}
	if raw, ok := o.get("securitySchemes"); ok {
		schemes, err := v0Schemes(raw)
		if err != nil {
			return nil, err
		}
		o = o.set("securitySchemes", schemes)
	}
	if raw, ok := o.get("skills"); ok {
		skills, err := v0Skills(raw)
		if err != nil {
			return nil, err
		}
		o = o.set("skills", skills)
	}
	return o.marshal(), nil
}

type iface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

func v0Interfaces(o object, version string) ([]iface, error) {
	var url, preferred string
	if raw, ok := o.get("url"); ok {
		if err := json.Unmarshal(raw, &url); err != nil {
			return nil, fmt.Errorf("%w: url: %v", ErrBadCard, err)
		}
	}
	if raw, ok := o.get("preferredTransport"); ok {
		if err := json.Unmarshal(raw, &preferred); err != nil {
			return nil, fmt.Errorf("%w: preferredTransport: %v", ErrBadCard, err)
		}
	}
	if preferred == "" {
		preferred = "JSONRPC"
	}
	out := []iface{}
	if url != "" {
		out = append(out, iface{URL: url, ProtocolBinding: preferred, ProtocolVersion: version})
	}
	if raw, ok := o.get("additionalInterfaces"); ok {
		var extra []struct {
			URL       string `json:"url"`
			Transport string `json:"transport"`
		}
		if err := json.Unmarshal(raw, &extra); err != nil {
			return nil, fmt.Errorf("%w: additionalInterfaces: %v", ErrBadCard, err)
		}
		for _, e := range extra {
			if e.URL == "" || (e.URL == url && e.Transport == preferred) {
				continue
			}
			out = append(out, iface{URL: e.URL, ProtocolBinding: e.Transport, ProtocolVersion: version})
		}
	}
	return out, nil
}

func capsObject(o object) (object, error) {
	raw, ok := o.get("capabilities")
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return object{}, nil
	}
	caps, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: capabilities: %v", ErrBadCard, err)
	}
	return caps, nil
}

// v0Security converts OpenAPI-style requirements, [{name: [scopes]}], to 1.0's
// [{"schemes": {name: {"list": [scopes]}}}].
func v0Security(raw json.RawMessage) (json.RawMessage, error) {
	var reqs []map[string][]string
	if err := json.Unmarshal(raw, &reqs); err != nil {
		return nil, fmt.Errorf("%w: security: %v", ErrBadCard, err)
	}
	type list struct {
		List []string `json:"list"`
	}
	out := make([]map[string]map[string]list, 0, len(reqs))
	for _, r := range reqs {
		s := map[string]list{}
		for name, scopes := range r {
			if scopes == nil {
				scopes = []string{}
			}
			s[name] = list{List: scopes}
		}
		out = append(out, map[string]map[string]list{"schemes": s})
	}
	return mustJSON(out), nil
}

// v0Schemes converts each OpenAPI-style scheme ({"type": "http", "scheme": "bearer"}) to the 1.0 one-of
// wrapper ({"httpAuthSecurityScheme": {"scheme": "bearer"}}); an entry already in the 1.0 shape, or of an
// unknown type, is kept as it is.
func v0Schemes(raw json.RawMessage) (json.RawMessage, error) {
	schemes, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: securitySchemes: %v", ErrBadCard, err)
	}
	for i, m := range schemes {
		var s map[string]json.RawMessage
		if json.Unmarshal(m.val, &s) != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(s["type"], &typ)
		pick := func(wrapper string, rename map[string]string, keys ...string) {
			inner := map[string]json.RawMessage{}
			for _, k := range keys {
				if v, ok := s[k]; ok {
					if to, ok := rename[k]; ok {
						k = to
					}
					inner[k] = v
				}
			}
			schemes[i].val = mustJSON(map[string]any{wrapper: inner})
		}
		switch typ {
		case "apiKey":
			pick("apiKeySecurityScheme", map[string]string{"in": "location"}, "in", "name", "description")
		case "http":
			pick("httpAuthSecurityScheme", nil, "scheme", "bearerFormat", "description")
		case "openIdConnect":
			pick("openIdConnectSecurityScheme", nil, "openIdConnectUrl", "description")
		case "mutualTLS":
			pick("mtlsSecurityScheme", nil, "description")
		case "oauth2":
			pick("oauth2SecurityScheme", nil, "flows", "oauth2MetadataUrl", "description")
		}
	}
	return schemes.marshal(), nil
}

// skillsHaveV0Security reports whether a skill of the card carries a 0.x security member.
func skillsHaveV0Security(o object) (bool, error) {
	raw, ok := o.get("skills")
	if !ok {
		return false, nil
	}
	var skills []json.RawMessage
	if err := json.Unmarshal(raw, &skills); err != nil {
		return false, nil // not an array: kept as the seller wrote it
	}
	for _, s := range skills {
		if so, err := parseObject(s); err == nil && so.has("security") {
			return true, nil
		}
	}
	return false, nil
}

// v0Skills renames each skill's security to securityRequirements, in the 1.0 shape; a skill that already
// has securityRequirements (a card in both shapes) only loses its security.
func v0Skills(raw json.RawMessage) (json.RawMessage, error) {
	var skills []json.RawMessage
	if err := json.Unmarshal(raw, &skills); err != nil {
		return nil, fmt.Errorf("%w: skills: %v", ErrBadCard, err)
	}
	changed := false
	for i, s := range skills {
		o, err := parseObject(s)
		if err != nil {
			continue // not an object: kept as the seller wrote it
		}
		sec, ok := o.get("security")
		if !ok {
			continue
		}
		if o.has("securityRequirements") {
			skills[i] = o.del("security").marshal()
			changed = true
			continue
		}
		req, err := v0Security(sec)
		if err != nil {
			return nil, err
		}
		skills[i] = o.rename("security", "securityRequirements").set("securityRequirements", req).marshal()
		changed = true
	}
	if !changed {
		return raw, nil
	}
	var b bytes.Buffer
	b.WriteByte('[')
	for i, s := range skills {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(s)
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// Rewrite returns the served card bytes: the normalized card with the gate's fields of §3 set and its
// signatures removed (M1 does not sign). Members the gate does not own keep their bytes and order.
func Rewrite(in Input) ([]byte, error) {
	norm, err := Normalize(in.Upstream)
	if err != nil {
		return nil, err
	}
	o, err := parseObject(norm)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(in.PublicURL, "/") + in.ServicePath
	o = o.set("supportedInterfaces", mustJSON([]iface{
		{URL: endpoint, ProtocolBinding: "JSONRPC", ProtocolVersion: "1.0"},
		{URL: endpoint, ProtocolBinding: "HTTP+JSON", ProtocolVersion: "1.0"},
	}))
	if in.OnlyAnonymous {
		o = o.del("securitySchemes", "securityRequirements", "security")
	} else {
		schemes := object{{key: "bearer", val: mustJSON(map[string]any{"httpAuthSecurityScheme": map[string]string{"scheme": "Bearer"}})}}
		reqs := []map[string]map[string]map[string][]string{{"schemes": {"bearer": {"list": {}}}}}
		if in.OIDC && in.OIDCIssuer != "" {
			schemes = append(schemes, member{key: "oidc", val: mustJSON(map[string]any{"openIdConnectSecurityScheme": map[string]string{
				"openIdConnectUrl": strings.TrimRight(in.OIDCIssuer, "/") + "/.well-known/openid-configuration",
			}})})
			reqs = append(reqs, map[string]map[string]map[string][]string{"schemes": {"oidc": {"list": {}}}})
		}
		o = o.set("securitySchemes", schemes.marshal()).set("securityRequirements", mustJSON(reqs)).del("security")
	}
	caps, err := capsObject(o)
	if err != nil {
		return nil, err
	}
	caps = caps.set("pushNotifications", json.RawMessage("true")).
		set("streaming", json.RawMessage("true")).
		set("extendedAgentCard", json.RawMessage("false"))
	exts, err := extensions(caps, in)
	if err != nil {
		return nil, err
	}
	if exts != nil {
		caps = caps.set("extensions", exts)
	}
	o = o.set("capabilities", caps.marshal()).del("signatures")
	return o.marshal(), nil
}

// extensions is the seller's capabilities.extensions with every entry whose URI the gate or the operator
// asserts replaced by theirs (spec §3, §13.4); nil when there is nothing to write.
func extensions(caps object, in Input) (json.RawMessage, error) {
	var gate []json.RawMessage
	owned := map[string]bool{}
	if in.SealedURI != "" {
		gate = append(gate, mustJSON(map[string]string{"uri": in.SealedURI}))
		owned[in.SealedURI] = true
	}
	for _, e := range in.CardExtensions {
		var ext struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(e, &ext); err != nil || ext.URI == "" {
			return nil, fmt.Errorf("card: a card_extensions entry is not an extension object with a uri")
		}
		if owned[ext.URI] {
			continue
		}
		owned[ext.URI] = true
		gate = append(gate, e)
	}
	raw, ok := caps.get("extensions")
	if !ok && len(gate) == 0 {
		return nil, nil
	}
	var seller []json.RawMessage
	if ok && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &seller); err != nil {
			return nil, fmt.Errorf("%w: capabilities.extensions: %v", ErrBadCard, err)
		}
	}
	if len(gate) == 0 {
		return raw, nil
	}
	out := make([]json.RawMessage, 0, len(seller)+len(gate))
	for _, e := range seller {
		var ext struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(e, &ext) == nil && owned[ext.URI] {
			continue
		}
		out = append(out, e)
	}
	out = append(out, gate...)
	var b bytes.Buffer
	b.WriteByte('[')
	for i, e := range out {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(bytes.TrimSpace(e))
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// Signer signs a served card (ES256 over RFC 8785 JCS, protected header with kontra.upstream, D13). M3.
type Signer interface {
	Sign(ctx context.Context, served []byte, upstreamSHA256 [32]byte, upstreamURL string, verified bool) (json.RawMessage, error)
}

// Served is one service's current cards.
type Served struct {
	Body         []byte // the served card
	Upstream     []byte // the upstream card, verbatim
	ETag         string // hash of Body
	LastModified time.Time
	Unavailable  bool // configured seller keys and no verifying signature (M3), or no fetch for 30 min: card routes answer 503
}

// ETag is the strong entity tag of a served body: a quoted hex SHA-256 prefix. Replicas that fetched the
// same upstream bytes serve the same body and therefore the same tag.
func ETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// CacheControl is the card responses' Cache-Control (spec §3).
const CacheControl = "max-age=60"

// Handler serves a card with Cache-Control: max-age=60, ETag and Last-Modified (the TCK hard-fails
// without them). upstream selects the verbatim upstream card.
//
//   - GET and HEAD only; any other method is an empty 404, as the edge's closed router answers it.
//   - A service with no servable card (none fetched yet, or none for 30 minutes, or configured seller keys
//     and no signature that verifies: Served.Unavailable) answers 503 with an empty body (spec §3).
//   - The served card's ETag is Served.ETag, the upstream card's the same hash over its own bytes; both
//     carry the served card's Last-Modified (the time its body last changed).
//   - Conditional requests (A2A §8.6.2: clients SHOULD revalidate): If-None-Match matching the ETag, or *,
//     is 304; without If-None-Match, If-Modified-Since at or after Last-Modified is 304 on the served card.
//     The upstream card answers If-Modified-Since in full: its bytes can change while the served body (and
//     so Last-Modified) does not.
//
// get is called once per request; the route has already decided that the caller may see the cards (an
// unknown or forbidden service is the route's 404, spec §2.1, D50). Card GETs are not version-checked.
func Handler(get func(r *http.Request) (*Served, error), upstream bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s, err := get(r)
		if err != nil || s == nil || s.Unavailable {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, etag := s.Body, s.ETag
		if upstream {
			body, etag = s.Upstream, ETag(s.Upstream)
		}
		if etag == "" {
			etag = ETag(body)
		}
		lastMod := s.LastModified.UTC().Truncate(time.Second)
		if lastMod.IsZero() || lastMod.Unix() <= 0 {
			lastMod = time.Unix(0, 0).UTC() // never absent (the TCK fails without it); a card without a time is old
		}
		h := w.Header()
		h.Set("Cache-Control", CacheControl)
		h.Set("ETag", etag)
		h.Set("Last-Modified", lastMod.Format(http.TimeFormat))
		if notModified(r, etag, lastMod, !upstream) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})
}

// notModified evaluates If-None-Match (weak comparison, RFC 9110 §13.1.2), and If-Modified-Since only
// when there is no If-None-Match and byDate is set.
func notModified(r *http.Request, etag string, lastMod time.Time, byDate bool) bool {
	if inm := r.Header.Values("If-None-Match"); len(inm) > 0 {
		for _, v := range inm {
			for tag := range strings.SplitSeq(v, ",") {
				tag = strings.TrimSpace(tag)
				if tag == "*" || strings.TrimPrefix(tag, "W/") == strings.TrimPrefix(etag, "W/") {
					return true
				}
			}
		}
		return false
	}
	if !byDate {
		return false
	}
	since, err := http.ParseTime(r.Header.Get("If-Modified-Since"))
	return err == nil && !lastMod.After(since)
}
