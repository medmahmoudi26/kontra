package a2ahttp

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/jws"
)

// spec §2.4 (review round 2, D311): what a service declares is read from its served card with exact keys, the
// capabilities object and each extension entry as the gate wrote them. A case variant that a decoder matching names
// case-insensitively takes for "extensions" or "required" (the last of the variants wins there) declares nothing here:
// a seller's "Extensions" after the gate's "extensions" can neither make the operator's optional entry required for
// the A2A route and hire's activation (D300) nor hide its required one. card.Rewrite refuses such a card, so none is
// served; this is the reader's half.
func TestDeclaredReadsExactKeys(t *testing.T) {
	const op = "https://operator.example/ext/escrow/v1"
	for _, c := range []struct {
		name     string
		body     string
		all, req []string
	}{
		{"exact", `{"capabilities":{"extensions":[{"uri":"` + op + `","required":true},{"uri":"https://s.example/x"}]}}`,
			[]string{op, "https://s.example/x"}, []string{op}},
		{"Extensions after extensions forcing required", `{"capabilities":{"extensions":[{"uri":"` + op + `","required":false}],` +
			`"Extensions":[{"uri":"` + op + `","required":true}]}}`, nil, nil},
		{"Extensions after extensions hiding required", `{"capabilities":{"extensions":[{"uri":"` + op + `","required":true}],"Extensions":[]}}`,
			nil, nil},
		{"a lone Extensions", `{"capabilities":{"Extensions":[{"uri":"` + op + `","required":true}]}}`, nil, nil},
		{"an entry's Required beside required", `{"capabilities":{"extensions":[{"uri":"` + op + `","required":false,"Required":true}]}}`,
			nil, nil},
		{"an entry's lone Required", `{"capabilities":{"extensions":[{"uri":"` + op + `","Required":true}]}}`, []string{op}, nil},
		{"a lone Capabilities", `{"Capabilities":{"extensions":[{"uri":"` + op + `","required":true}]}}`, nil, nil},
	} {
		all, req := cardExtensions([]byte(c.body))
		if !slices.Equal(all, c.all) || !slices.Equal(req, c.req) {
			t.Errorf("%s: declares %v, requires %v; want %v, %v", c.name, all, req, c.all, c.req)
		}
	}

	// Through the install path: a card the gate signs declares, to the route and to hire, exactly what a2a-go reads of
	// it, the operator's entry included.
	keys, err := jws.EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	k := card.NewKeeper(nil, card.Input{PublicURL: "https://gate.example", ServicePath: "/agents/t/s",
		CardExtensions: []json.RawMessage{json.RawMessage(`{"uri":"` + op + `","required":true}`)}},
		card.WithSigner(&card.JWSSigner{Keys: keys, JWKSURL: "https://gate.example/.well-known/jwks.json"}))
	up := `{"name":"s","description":"d","version":"1","supportedInterfaces":[{"url":"https://seller.example/a2a",` +
		`"protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{"extensions":[{"uri":"` + op + `","required":false},` +
		`{"uri":"https://seller.example/ext","required":true}]},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`
	if err := k.Seed([]byte(up), time.Now()); err != nil {
		t.Fatal(err)
	}
	s, err := k.Current()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := agentcard.DefaultCardParser(s.Body)
	if err != nil {
		t.Fatal(err)
	}
	var wantAll, wantReq []string
	for _, e := range parsed.Capabilities.Extensions {
		wantAll = append(wantAll, e.URI)
		if e.Required {
			wantReq = append(wantReq, e.URI)
		}
	}
	set := New(Deps{}).Declared(&Service{Cards: func() (*card.Served, error) { return s, nil }})
	got := slices.Clone(set.Required())
	slices.Sort(got)
	slices.Sort(wantReq)
	if !slices.Equal(got, wantReq) || len(wantReq) != 2 {
		t.Errorf("required: the gate reads %v, a2a-go %v", got, wantReq)
	}
	for _, u := range wantAll {
		if !set.Declares(u) {
			t.Errorf("a2a-go reads %s, the gate does not declare it", u)
		}
	}
}
