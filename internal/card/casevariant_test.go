package card

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"kontra.sh/kontra/internal/jws"
)

// caseCard is a 1.0 seller card with caps as its capabilities and top inserted before them.
func caseCard(top, caps string) string {
	return `{"name":"s","description":"d","version":"1",` +
		`"supportedInterfaces":[{"url":"https://seller.example/a2a","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],` +
		top + `"capabilities":` + caps + `,"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`
}

// spec §3, §2.4 (review round 2, D311): the gate writes capabilities' members and the operator's extension entries by
// their exact keys, and removes the seller's signatures, 0.x members and, for an only-anonymous service, its security
// fields by theirs. A seller's member equal to one of them under case folding only ("Extensions" after the gate's
// "extensions", a lone "Signatures" or "SecuritySchemes", an entry's "Required") is what a2a-go's case-insensitive
// card parser reads in its place, on a card the gate signs; such a card is refused, through the real install path
// (Keeper.Seed with the gate's signer), and nothing is served. The control cards install, and a2a-go then reads exactly
// the operator's entry.
func TestCaseVariantsOfGateMembersAreRefused(t *testing.T) {
	keys, err := jws.EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	const op = "https://operator.example/ext/escrow/v1"
	opEntry := []json.RawMessage{json.RawMessage(`{"uri":"` + op + `","required":true,"params":{"quote":"15.00"}}`)}
	install := func(up string, onlyAnonymous bool) (*Served, error) {
		k := NewKeeper(nil, Input{PublicURL: "https://gate.example", ServicePath: "/agents/t/s", OnlyAnonymous: onlyAnonymous,
			CardExtensions: opEntry}, WithSigner(&JWSSigner{Keys: keys, JWKSURL: "https://gate.example/.well-known/jwks.json"}))
		if err := k.Seed([]byte(up), time.Now()); err != nil {
			return nil, err
		}
		return k.Current()
	}

	for _, c := range []struct {
		name          string
		up            string
		onlyAnonymous bool
	}{
		// capabilities: a case variant of each member the gate writes, after the exact one or alone.
		{"Extensions after extensions (hides the operator's entry)", caseCard("", `{"extensions":[],"Extensions":[]}`), false},
		{"Extensions after extensions (forges the operator's entry)", caseCard("", `{"extensions":[],"Extensions":[{"uri":"`+op+`","required":false,"params":{"quote":"0.01"}}]}`), false},
		{"a lone Extensions", caseCard("", `{"Extensions":[{"uri":"`+op+`","required":false}]}`), false},
		{"Extensions before extensions", caseCard("", `{"Extensions":[],"extensions":[]}`), false},
		{"PushNotifications", caseCard("", `{"pushNotifications":true,"PushNotifications":false}`), false},
		{"a lone Streaming", caseCard("", `{"Streaming":false}`), false},
		{"a lone ExtendedAgentCard", caseCard("", `{"ExtendedAgentCard":true}`), false},
		{"two other capabilities keys equal under case folding", caseCard("", `{"foo":1,"FOO":2}`), false},
		// An extension entry: the members the gate reads of it.
		{"an entry's Required", caseCard("", `{"extensions":[{"uri":"https://seller.example/ext","Required":true}]}`), false},
		{"an entry's URI beside uri", caseCard("", `{"extensions":[{"uri":"https://seller.example/ext","URI":"`+op+`"}]}`), false},
		// The top level: members the gate removes (signing refuses a pair there, never a lone variant).
		{"a lone Signatures", caseCard(`"Signatures":[{"protected":"e30","signature":"AA"}],`, `{}`), false},
		{"a lone URL", caseCard(`"URL":"https://seller.example/internal",`, `{}`), false},
		{"a lone PreferredTransport", caseCard(`"PreferredTransport":"JSONRPC",`, `{}`), false},
		{"a lone AdditionalInterfaces", caseCard(`"AdditionalInterfaces":[],`, `{}`), false},
		{"a lone ProtocolVersion", caseCard(`"ProtocolVersion":"0.3",`, `{}`), false},
		{"a lone Security", caseCard(`"Security":[{"oauth":[]}],`, `{}`), false},
		{"a lone SecuritySchemes on an only-anonymous service", caseCard(`"SecuritySchemes":{"oauth":{"oauth2SecurityScheme":{"flows":{}}}},`, `{}`), true},
		{"a lone SecurityRequirements on an only-anonymous service", caseCard(`"SecurityRequirements":[{"schemes":{"oauth":{"list":[]}}}],`, `{}`), true},
		// The top level: members the gate writes.
		{"a lone SupportedInterfaces", caseCard(`"SupportedInterfaces":[{"url":"https://seller.example/harvest","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],`, `{}`), false},
		{"a lone Capabilities", caseCard(`"Capabilities":{"extensions":[]},`, `{}`), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := install(c.up, c.onlyAnonymous)
			if !errors.Is(err, ErrBadCard) {
				t.Fatalf("installed (err %v): %s", err, servedBody(s))
			}
			if _, err := Rewrite(Input{Upstream: []byte(c.up), PublicURL: "https://gate.example", ServicePath: "/agents/t/s",
				OnlyAnonymous: c.onlyAnonymous, CardExtensions: opEntry}); !errors.Is(err, ErrBadCard) {
				t.Fatalf("Rewrite: %v", err)
			}
		})
	}

	// Controls: the same cards with exact keys install, and a2a-go reads the operator's entry, exactly once and as the
	// operator wrote it, in place of the seller's of the same URI.
	for _, c := range []struct {
		name          string
		up            string
		onlyAnonymous bool
	}{
		{"exact extensions", caseCard("", `{"extensions":[{"uri":"`+op+`","required":false,"params":{"quote":"0.01"}}],"pushNotifications":false}`), false},
		{"no capabilities members", caseCard("", `{}`), false},
		{"exact security on an only-anonymous service", caseCard(`"securitySchemes":{"oauth":{"oauth2SecurityScheme":{"flows":{}}}},`, `{}`), true},
	} {
		t.Run("control: "+c.name, func(t *testing.T) {
			s, err := install(c.up, c.onlyAnonymous)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := agentcard.DefaultCardParser(s.Body)
			if err != nil {
				t.Fatal(err)
			}
			var uris []string
			for _, e := range parsed.Capabilities.Extensions {
				uris = append(uris, e.URI)
				if e.URI == op && (!e.Required || e.Params["quote"] != "15.00") {
					t.Errorf("a2a-go reads the operator's entry as %+v", e)
				}
			}
			if !slices.Equal(uris, []string{op}) {
				t.Errorf("a2a-go reads extensions %v, want the operator's only", uris)
			}
			if c.onlyAnonymous && (len(parsed.SecuritySchemes) != 0 || len(parsed.SecurityRequirements) != 0) {
				t.Errorf("an only-anonymous service's card: a2a-go reads schemes %v, requirements %v", parsed.SecuritySchemes, parsed.SecurityRequirements)
			}
		})
	}
}

func servedBody(s *Served) string {
	if s == nil {
		return ""
	}
	return string(s.Body)
}
