package hiring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// JWKSCacheTTL is how long the gate's key set, fetched through the tunnel, is reused (the gate serves it with
// max-age=300, D166).
const JWKSCacheTTL = 5 * time.Minute

// maxCard bounds a served card the node reads (the gate's own limit on an upstream card is 256 KiB, and the
// served card adds the gate's members); maxJWKS a key set.
const (
	maxCard = 1 << 20
	maxJWKS = 256 << 10
)

// ErrCardNotVerified is a served card whose gate signature does not verify with a jku equal to the dialed
// origin's key set: it is not served locally (spec/tunnel.md §9).
var ErrCardNotVerified = errors.New("hiring: the gate's card signature does not verify")

// jwksEntry is one cached key set.
type jwksEntry struct {
	keys    *jws.KeySetPublic
	fetched time.Time
}

type jwksKey struct {
	client *http.Client
	url    string
}

// jwksCache holds the key sets fetched per client (one per hiring server: its tunnel) and jku.
var (
	jwksMu    sync.Mutex
	jwksCache = map[jwksKey]jwksEntry{}
	now       = time.Now // tests
)

// JWKSURL is the only jku a node accepts on the gate's card signature: {the origin it dialed}/.well-known/jwks.json.
func JWKSURL(gate *url.URL) (string, error) {
	origin, err := tunnel.CanonicalOrigin(gate)
	if err != nil {
		return "", err
	}
	return origin + jws.JWKSPath, nil
}

// RewriteCard verifies the gate's signature on a served card (jku = gate origin + /.well-known/jwks.json, the
// key set fetched from that origin only, through jwks, and cached JWKSCacheTTL) and returns it with every
// supportedInterfaces[].url moved to local, the node's own base for the service (spec/tunnel.md §9): its path
// and query kept, its scheme and host local's. A card none of whose signatures verifies with that jku is
// ErrCardNotVerified and is never served (relay.md §3: the gate's signature is how a node knows the card is the
// gate's). The rewritten card leaves out signatures, which no longer cover it; every other member is kept as it
// came. The card is read with exact keys.
func RewriteCard(ctx context.Context, card []byte, gate *url.URL, local *url.URL, jwks *http.Client) ([]byte, error) {
	if len(card) > maxCard {
		return nil, fmt.Errorf("%w: a card over %d bytes", ErrCardNotVerified, maxCard)
	}
	jku, err := JWKSURL(gate)
	if err != nil {
		return nil, err
	}
	m, err := strictjson.Members(card)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCardNotVerified, err)
	}
	sigs, err := signatures(m["signatures"])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCardNotVerified, err)
	}
	if len(sigs) == 0 {
		return nil, fmt.Errorf("%w: the card is not signed", ErrCardNotVerified)
	}
	verified := false
	for attempt := 0; attempt < 2 && !verified; attempt++ {
		keys, err := keySet(ctx, jwks, jku, attempt > 0)
		if err != nil {
			return nil, fmt.Errorf("%w: the key set at %s: %w", ErrCardNotVerified, jku, err)
		}
		for _, sig := range sigs {
			h, err := jws.Verify(card, sig, keys)
			if err == nil && h.JKU == jku {
				verified = true
				break
			}
		}
	}
	if !verified {
		return nil, fmt.Errorf("%w with the key set at %s", ErrCardNotVerified, jku)
	}
	delete(m, "signatures")
	if raw, ok := m["supportedInterfaces"]; ok {
		out, err := rewriteInterfaces(raw, local)
		if err != nil {
			return nil, err
		}
		m["supportedInterfaces"] = out
	}
	return encodeObject(m), nil
}

// signatures decodes the card's signatures, each with exact keys.
func signatures(raw json.RawMessage) ([]jws.Signature, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, errors.New("signatures is not an array")
	}
	out := make([]jws.Signature, 0, len(elems))
	for _, e := range elems {
		m, err := strictjson.Members(e)
		if err != nil {
			return nil, err
		}
		var s jws.Signature
		if json.Unmarshal(m["protected"], &s.Protected) != nil || json.Unmarshal(m["signature"], &s.Signature) != nil {
			return nil, errors.New("a signature without protected and signature strings")
		}
		s.Header = m["header"]
		out = append(out, s)
	}
	return out, nil
}

// keySet returns the gate's key set at jku, from the cache unless fresh is set or it is older than JWKSCacheTTL.
// The request goes to jku, built from the dialed origin and never from a card, through client (the tunnel).
func keySet(ctx context.Context, client *http.Client, jku string, fresh bool) (*jws.KeySetPublic, error) {
	k := jwksKey{client: client, url: jku}
	jwksMu.Lock()
	e, ok := jwksCache[k]
	jwksMu.Unlock()
	if ok && !fresh && now().Sub(e.fetched) < JWKSCacheTTL {
		return e.keys, nil
	}
	if client == nil {
		return nil, errors.New("no client for the key set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jku, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxJWKS+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxJWKS {
		return nil, fmt.Errorf("a key set over %d bytes", maxJWKS)
	}
	keys, err := jws.ParseJWKS(b)
	if err != nil {
		return nil, err
	}
	jwksMu.Lock()
	jwksCache[k] = jwksEntry{keys: keys, fetched: now()}
	jwksMu.Unlock()
	return keys, nil
}

// forgetKeySets drops the key sets cached for client (its server stops).
func forgetKeySets(client *http.Client) {
	jwksMu.Lock()
	defer jwksMu.Unlock()
	for k := range jwksCache {
		if k.client == client {
			delete(jwksCache, k)
		}
	}
}

// rewriteInterfaces moves every interface url to local: scheme and host local's, path and query the gate's.
func rewriteInterfaces(raw json.RawMessage, local *url.URL) (json.RawMessage, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, fmt.Errorf("%w: supportedInterfaces is not an array", ErrCardNotVerified)
	}
	for i, e := range elems {
		m, err := strictjson.Members(e)
		if err != nil {
			return nil, fmt.Errorf("%w: an interface: %v", ErrCardNotVerified, err)
		}
		var s string
		if v, ok := m["url"]; ok {
			if json.Unmarshal(v, &s) != nil {
				return nil, fmt.Errorf("%w: an interface url that is not a string", ErrCardNotVerified)
			}
			u, err := url.Parse(s)
			if err != nil {
				return nil, fmt.Errorf("%w: an interface url: %v", ErrCardNotVerified, err)
			}
			nu := url.URL{Scheme: local.Scheme, Host: local.Host, Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
			m["url"] = jsonString(nu.String())
		}
		elems[i] = encodeObject(m)
	}
	var b bytes.Buffer
	b.WriteByte('[')
	for i, e := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(e)
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// encodeObject writes members in key order, values as they came (compacted), without HTML escaping.
func encodeObject(m map[string]json.RawMessage) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(k))
		b.WriteByte(':')
		if err := json.Compact(&b, m[k]); err != nil {
			b.Write(m[k])
		}
	}
	b.WriteByte('}')
	return b.Bytes()
}

func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// cardMembers are members only a card has (A2A 1.0, and 0.x's url and additionalInterfaces): an answer with any of
// them on a card route, under case folding, is a card, verified before it is served. A Task, a Message and a list
// have none.
var cardMembers = []string{"supportedInterfaces", "signatures", "url", "additionalInterfaces", "preferredTransport", "skills", "capabilities"}

// isCardRoute reports a GET path that may answer a served card: {svc} (/agents/{name} or /agents/{tenant}/{name})
// and {svc}/.well-known/agent-card.json (spec/relay.md §2.1). The upstream card (agent-card.upstream.json) is
// the seller's, verbatim and never the gate's to sign.
func isCardRoute(path string) (must, may bool) {
	rest, ok := strings.CutPrefix(path, "/agents/")
	if !ok {
		return false, false
	}
	if strings.HasSuffix(rest, "/.well-known/agent-card.json") {
		return true, true
	}
	segs := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	return false, len(segs) == 1 || len(segs) == 2
}

// isCard reports whether a GET answer on a card route is a card: one with a member only a card has, compared under
// case folding (strings.EqualFold), since a2a-go decodes a card with encoding/json, which matches member names so:
// {"SupportedInterfaces": …} is a card to it. A body that is not one JSON object with exact keys is an error: the
// node does not serve what it cannot read exactly on a route that may carry a card.
func isCard(body []byte) (bool, error) {
	m, err := strictjson.Members(body)
	if err != nil {
		return false, err
	}
	for k := range m {
		for _, c := range cardMembers {
			if strings.EqualFold(k, c) {
				return true, nil
			}
		}
	}
	return false, nil
}
