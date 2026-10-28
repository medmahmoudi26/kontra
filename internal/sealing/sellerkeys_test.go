package sealing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/pins"
)

// sellerSign signs card (A2A 1.0 §8.4: ES256 over the JCS of the card without signatures) with key under kid, its
// public key in the protected header's jwk when withJWK, and returns the card with that signature.
func sellerSign(t *testing.T, card []byte, key *ecdsa.PrivateKey, kid string, withJWK bool) []byte {
	t.Helper()
	prot := map[string]any{"alg": "ES256", "kid": kid}
	if withJWK {
		b, _ := key.PublicKey.Bytes() // 0x04 || X || Y
		prot["jwk"] = map[string]string{"kty": "EC", "crv": "P-256",
			"x": base64.RawURLEncoding.EncodeToString(b[1:33]), "y": base64.RawURLEncoding.EncodeToString(b[33:])}
	}
	pb, _ := json.Marshal(prot)
	p64 := base64.RawURLEncoding.EncodeToString(pb)
	canon, err := jws.CardPayload(card)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(p64 + "." + base64.RawURLEncoding.EncodeToString(canon)))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	var m map[string]json.RawMessage
	_ = json.Unmarshal(card, &m)
	sigs, _ := json.Marshal([]map[string]string{{"protected": p64, "signature": base64.RawURLEncoding.EncodeToString(sig)}})
	m["signatures"] = sigs
	out, _ := json.Marshal(m)
	return out
}

func hashOf(t *testing.T, card []byte) string {
	t.Helper()
	c, err := jcs.Transform(card)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(c)
	return hex.EncodeToString(s[:])
}

// Finding sealing-spec-4: the pins recorded the kid labels of the upstream card's signatures, never verified, as the
// seller's "thumbprints": a gate that replaced the card and its signature with a forged one claiming the pinned kid
// left the pin unchanged. A signature that carries its key is verified now and its key's RFC 7638 thumbprint pinned;
// one that names its key only is pinned as the unverified "kid:<kid>", never as a thumbprint.
func TestReviewSellerKeyIDsVerified(t *testing.T) {
	seller, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	attacker, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	up := []byte(`{"name":"booking","description":"the seller's card"}`)

	// A forged signature whose header claims the seller's kid: no key of the seller's.
	prot := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"seller-key-1"}`))
	forged := []byte(`{"name":"booking (a gate's copy)","signatures":[{"protected":"` + prot + `","signature":"AAAA"}]}`)
	ids, err := SellerKeyIDs(forged, hashOf(t, forged))
	if err != nil || len(ids) != 1 || ids[0] != "kid:seller-key-1" {
		t.Fatalf("a kid-only signature: %v %v, want the unverified kid:seller-key-1", ids, err)
	}

	// The seller's signature carrying its key: its thumbprint.
	genuine := sellerSign(t, up, seller, "seller-key-1", true)
	ids, err = SellerKeyIDs(genuine, hashOf(t, genuine))
	if err != nil || len(ids) != 1 || strings.HasPrefix(ids[0], "kid:") || len(ids[0]) != 43 {
		t.Fatalf("a signature carrying its key: %v %v", ids, err)
	}
	want := ids[0]
	// The same card under a forged signature naming the same kid with the seller's public key: does not verify.
	bad := sellerSign(t, up, attacker, "seller-key-1", false)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(bad, &m)
	var sigs []map[string]string
	_ = json.Unmarshal(m["signatures"], &sigs)
	sk, _ := seller.PublicKey.Bytes()
	p, _ := json.Marshal(map[string]any{"alg": "ES256", "kid": "seller-key-1", "jwk": map[string]string{"kty": "EC", "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(sk[1:33]), "y": base64.RawURLEncoding.EncodeToString(sk[33:])}})
	sigs[0]["protected"] = base64.RawURLEncoding.EncodeToString(p)
	m["signatures"], _ = json.Marshal(sigs)
	bad, _ = json.Marshal(m)
	if ids, err := SellerKeyIDs(bad, hashOf(t, bad)); err != nil || len(ids) != 0 {
		t.Fatalf("a signature that does not verify with the key it carries: %v %v", ids, err)
	}
	// A gate's own key, carried: another thumbprint (the pin sees a new seller key).
	other := sellerSign(t, up, attacker, "seller-key-1", true)
	if ids, err := SellerKeyIDs(other, hashOf(t, other)); err != nil || len(ids) != 1 || ids[0] == want {
		t.Fatalf("another key under the pinned kid: %v %v", ids, err)
	}
}

// M3 review round 2, sealing-fixes-5: a gate that replaces the seller's upstream card and keeps a signature that does not verify (here
// the seller's jwk with another key's signature) made the card count as signed with no seller key, and the pin
// accepted the loss of the pinned key silently, even under --pin-recipients. Such a card is unsigned now (a downgrade
// from a signed pin, always refused); a kid-only signature still counts as signed (D223).
func TestReview2SellerSignatureThatVerifiesNothingIsUnsigned(t *testing.T) {
	node, _ := age.GenerateX25519Identity()
	seller, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	attacker, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	g := newSealGate(t, node)
	serve := func(up []byte) *Card {
		t.Helper()
		g.upstream = up
		g.card = g.signWith(t, cardWith(extensionFor(true, node.Recipient())), &jws.Upstream{SHA256: hashOf(t, up),
			URL: g.srv.URL + "/agents/echo/.well-known/agent-card.upstream.json", Verified: true})
		c, err := VerifyCard(context.Background(), g.card, g.srv.URL, func(ctx context.Context, u string) ([]byte, error) {
			return httpGet(ctx, http.DefaultClient, u, "")
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	genuine := serve(sellerSign(t, []byte(`{"name":"booking","description":"the seller's card"}`), seller, "seller-key-1", true))
	if !genuine.Signed || len(genuine.SellerKeys) != 1 {
		t.Fatalf("the seller's card: %+v", genuine)
	}
	forgedUp := sellerSign(t, []byte(`{"name":"booking","description":"THE GATE'S card"}`), attacker, "seller-key-1", false)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(forgedUp, &m)
	var sigs []map[string]string
	_ = json.Unmarshal(m["signatures"], &sigs)
	sk, _ := seller.PublicKey.Bytes()
	p, _ := json.Marshal(map[string]any{"alg": "ES256", "kid": "seller-key-1", "jwk": map[string]string{"kty": "EC", "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(sk[1:33]), "y": base64.RawURLEncoding.EncodeToString(sk[33:])}})
	sigs[0]["protected"] = base64.RawURLEncoding.EncodeToString(p)
	m["signatures"], _ = json.Marshal(sigs)
	forgedUp, _ = json.Marshal(m)
	forged := serve(forgedUp)
	if forged.Signed || len(forged.SellerKeys) != 0 {
		t.Fatalf("a card whose seller signature verifies with no key counts as signed: %+v", forged)
	}
	st, err := pins.Open(filepath.Join(t.TempDir(), "pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	pin := func(c *Card) pins.Pin {
		return pins.Pin{Sealed: true, Recipients: []string{"k1_a"}, SellerKeys: c.SellerKeys, Signed: c.Signed}
	}
	if _, err := st.Check("https://gate", "booking", pin(genuine), pins.Options{PinRecipients: true}); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []pins.Options{{PinRecipients: true}, {Accept: []string{"booking"}}} {
		if out, err := st.Check("https://gate", "booking", pin(forged), opts); err == nil {
			t.Fatalf("%+v: the forged card is accepted: %+v", opts, out)
		}
	}
	// A kid-only signature is unverifiable and pinned so: signed.
	prot := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"seller-1"}`))
	if c := serve([]byte(`{"name":"echo","signatures":[{"protected":"` + prot + `","signature":"c2ln"}]}`)); !c.Signed {
		t.Fatalf("a kid-only signature: %+v", c)
	}
}
