package gate

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
)

// D223 through a real gate and two in-process `kontra connect`: a sealed tunnel service whose seller signs its card
// naming its key by kid only, and whose seller_keys the gate holds. The gate's signature names the key set it verified
// with (kontra.upstream.jwks) and serves it on its origin; the hiring node fetches it through its tunnel, verifies the
// kid-only signature with it, and pins the key's RFC 7638 thumbprint, never the unverified kid:<kid>.
func TestSellerKeySetThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	seller, _ := jws.EphemeralKeySet()
	signed := sellerSign(t, seller, agentCard)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, signed)
	})
	mux.Handle("/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(&helloExec{})))
	agent := httptest.NewServer(mux)
	t.Cleanup(agent.Close)
	services := `services:
  signed:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
    sealed: true
    seller_keys: ` + jwksOf(t, seller) + `
`
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	team, err := tenant.EnsureTeam(t.Context(), e.pool)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(t.Context(), team, "signed", "test")
	if err != nil {
		t.Fatal(err)
	}
	host := startNode(t, t.TempDir(), "--gate", a.url, "--name", "host", "--serve", "a2a:signed="+agent.URL, "--enroll", tok)
	host.local(t)
	key := e.mintKey("buyer")
	eventually(t, 30*time.Second, "the signed service's card", func() bool {
		st, _, b := get(t, a.url, "/agents/signed/.well-known/agent-card.json", key)
		return st == http.StatusOK && strings.Contains(string(b), "age1")
	})
	// The gate serves the key set its signature names: the seller's public keys.
	st, hdr, set := get(t, a.url, "/agents/signed/.well-known/seller-jwks.json", key)
	if st != http.StatusOK || hdr.Get("Content-Type") != "application/jwk-set+json" {
		t.Fatalf("the seller key set: %d %v %s", st, hdr, set)
	}
	served, err := jws.ParseJWKS(set)
	if err != nil {
		t.Fatal(err)
	}
	sk, _ := seller.KeyID()
	want, ok := served.Thumbprint(sk)
	if !ok {
		t.Fatalf("the served set has no key %s: %s", sk, set)
	}
	if strings.Contains(string(set), `"d"`) {
		t.Fatalf("the served set holds a private member: %s", set)
	}

	hirerHome := t.TempDir()
	hirer := startNode(t, hirerHome, "--gate", a.url, "--name", "hirer", "--api-key", key)
	local := hirer.local(t)
	eventually(t, 30*time.Second, "the local card, verified", func() bool {
		resp, err := http.Get(local + "/agents/signed/.well-known/agent-card.json")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	ps, err := pins.Open(filepath.Join(hirerHome, nodekey.DirName, pins.FileName))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(a.url)
	origin, _ := tunnel.CanonicalOrigin(u)
	var pin pins.Pin
	eventually(t, 10*time.Second, "the pin", func() bool {
		p, known, err := ps.Get(origin, "signed")
		pin = p
		return err == nil && known
	})
	if !pin.Sealed || !pin.Signed || len(pin.SellerKeys) != 1 || pin.SellerKeys[0] != want {
		t.Fatalf("the pin %+v, want the seller key's thumbprint %s (verified with the set the gate named)", pin, want)
	}
	if !strings.Contains(hirer.out.String()+hirer.logs.String(), want) {
		t.Error("the hiring node does not print the seller key's thumbprint")
	}
}
