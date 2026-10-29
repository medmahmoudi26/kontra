package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// card_passthrough (spec/relay.md §3, §16; D13, D166): an A2A seller whose card carries the seller's ES256
// signature, behind a direct service configured with seller_keys holding that key, served by two replicas that
// share one signing key. From either replica: the served card has exactly one signature, the gate's, which
// a2a-go's verifier accepts through its jku (the public URL's JWKS); its protected header's kontra.upstream.sha256
// is the SHA-256 of the upstream card's JCS, its url the upstream route, and verified is true; the upstream route
// serves the seller's card byte for byte, its signature included; the seller's fields are byte-identical under JCS;
// both replicas sign with the same key and publish the same JWKS.
func TestCardPassthrough(t *testing.T) {
	seller, err := jws.EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	var (
		once   sync.Once
		signed []byte
	)
	sign := func(card []byte) []byte {
		once.Do(func() {
			sig, err := seller.Sign(card, "https://seller.example/.well-known/jwks.json", nil)
			if err != nil {
				panic(err)
			}
			m, err := strictjson.Members(card)
			if err != nil {
				panic(err)
			}
			sb, _ := json.Marshal([]jws.Signature{sig})
			m["signatures"] = sb
			signed, _ = json.Marshal(m)
		})
		return signed
	}
	sellerJWKS, err := seller.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, Options{
		Services:   map[string]slowagent.Config{"sold": {CardHook: sign}},
		SellerKeys: map[string]string{"sold": string(sellerJWKS)},
	})
	ctx := ctxFor(t, 3*time.Minute)
	jku := h.Front.URL + jws.JWKSPath
	verifier := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{
		KeyResolver: a2acrypto.NewJWKSKeyResolver(http.DefaultClient, []string{jku})})
	canon, err := jcs.Transform(signed)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	var kids, jwksBodies []string
	for _, g := range h.Gates {
		served := getCard(t, ctx, g.Svc("sold")+"/.well-known/agent-card.json", h.APIKey)
		m, err := strictjson.Members(served)
		if err != nil {
			t.Fatal(err)
		}
		var sigs []a2a.AgentCardSignature
		if err := json.Unmarshal(m["signatures"], &sigs); err != nil || len(sigs) != 1 {
			t.Fatalf("%s: signatures %s, want exactly the gate's", g.Name, m["signatures"])
		}
		if err := verifier.Verify(context.Background(), served, &sigs[0]); err != nil {
			t.Fatalf("%s: a2a-go's verifier refuses the gate's signature: %v", g.Name, err)
		}
		hb, err := base64.RawURLEncoding.DecodeString(sigs[0].Protected)
		if err != nil {
			t.Fatal(err)
		}
		var header jws.Header
		if err := json.Unmarshal(hb, &header); err != nil || header.Upstream == nil {
			t.Fatalf("%s: protected header %s", g.Name, hb)
		}
		if header.JKU != jku || header.Upstream.SHA256 != hex.EncodeToString(sum[:]) || !header.Upstream.Verified ||
			header.Upstream.URL != h.Front.URL+"/agents/sold/.well-known/agent-card.upstream.json" {
			t.Fatalf("%s: protected header %s", g.Name, hb)
		}
		kids = append(kids, header.Kid)
		if up := getCard(t, ctx, g.Svc("sold")+"/.well-known/agent-card.upstream.json", h.APIKey); string(up) != string(signed) {
			t.Fatalf("%s: the upstream card is not the seller's byte for byte:\n%s\n%s", g.Name, up, signed)
		}
		if strip(t, served) != strip(t, signed) {
			t.Fatalf("%s: the seller's fields differ under JCS:\n%s\n%s", g.Name, strip(t, served), strip(t, signed))
		}
		b, st := getBody(ctx, g.URL+jws.JWKSPath, "")
		if st != http.StatusOK {
			t.Fatalf("%s: the JWKS: %d", g.Name, st)
		}
		jwksBodies = append(jwksBodies, string(b))
	}
	if kids[0] != kids[1] || kids[0] == "" || jwksBodies[0] != jwksBodies[1] || !strings.Contains(jwksBodies[0], kids[0]) {
		t.Fatalf("the replicas sign with different keys: kids %v, JWKS\n%s\n%s", kids, jwksBodies[0], jwksBodies[1])
	}
	if got := scalar[[]byte](t, h, `select card_hash from kontra.services where name = 'sold'`); hex.EncodeToString(got) != hex.EncodeToString(sum[:]) {
		t.Fatalf("services.card_hash %x, want %x", got, sum)
	}
}

// strip is the JCS of a card without the members the gate writes (its interfaces, security, capabilities and
// signatures): the seller's fields.
func strip(t *testing.T, card []byte) string {
	t.Helper()
	m, err := strictjson.Members(card)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"supportedInterfaces", "securitySchemes", "securityRequirements", "capabilities", "signatures"} {
		delete(m, k)
	}
	raw, _ := json.Marshal(m)
	out, err := jcs.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
