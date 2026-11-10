// Command ap2-merchant is a small A2A 1.0 agent, written with a2a-go v2.7.0 and no LLM, that plays the Merchant role
// of AP2 v0.2.0 (the Agent Payments Protocol, https://github.com/google-agentic-commerce/ap2, released 28 April 2026)
// for one item, and takes no payment.
//
// Its card declares one extension, required: AP2 (https://github.com/google-agentic-commerce/ap2/v1) with
// params.roles ["merchant"]. A message that does not activate it is refused ExtensionSupportRequiredError (-32008).
// A first message stops at input-required with an `ap2.checkout` data part: a checkout JWT the merchant signs with its
// own ECDSA P-256 key (ES256) and the checkout's hash. A later message to the same task carrying an
// `ap2.mandates.CheckoutMandateSdJwt` data part is checked against that checkout (checkoutMandate below) and answered
// with an `ap2.CheckoutReceipt` data part, a receipt JWT the merchant signs: Success completes the task, Error leaves it
// waiting for a better mandate. The data-part keys are the AP2 v0.2.0 Python samples' (code/samples/python), except
// the receipt's, which the samples do not carry over A2A: `ap2.CheckoutReceipt` follows their `ap2.PaymentReceipt`.
//
// It is an example seller (examples/README.md): a plain A2A agent that imports nothing from Kontra and has no
// Kontra-specific endpoint. It serves its card at /.well-known/agent-card.json, its public key at
// /.well-known/jwks.json, JSON-RPC at its root and HTTP+JSON under /rest, all under one root URL, so that
// `kontra connect --serve a2a:ap2-merchant=http://127.0.0.1:9993` can host it.
//
//	ap2-merchant [--host 127.0.0.1] [--port 9993] [--key merchant.pem]
//
// --key is a PEM file holding a P-256 private key (SEC 1 "EC PRIVATE KEY" or PKCS #8 "PRIVATE KEY"); without it the
// merchant generates a key at start, so a restart cannot honour the checkouts it issued before. The first line on
// standard output is the agent's root URL (with the port chosen when --port is 0). SIGINT or SIGTERM stops it. Tasks
// live in memory: a restart forgets them.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
)

// AP2 v0.2.0 over A2A.
const (
	// ExtAP2 is AP2's A2A extension URI (code/samples/python/src/common/a2a_extension_utils.py).
	ExtAP2 = "https://github.com/google-agentic-commerce/ap2/v1"
	// KeyCheckout holds the merchant's checkout: {checkout_jwt, checkout_hash, …} (merchant_agent/tools.py).
	KeyCheckout = "ap2.checkout"
	// KeyCheckoutMandate holds a Checkout Mandate SD-JWT, as a string (common/constants.py).
	KeyCheckoutMandate = "ap2.mandates.CheckoutMandateSdJwt"
	// KeyPaymentMandate holds a Payment Mandate SD-JWT (common/constants.py). This merchant reads none: it is the
	// Merchant Payment Processor's (README.md).
	KeyPaymentMandate = "ap2.mandates.PaymentMandateSdJwt"
	// KeyCheckoutReceipt holds the merchant's Checkout Receipt JWT, as a string. Not a sample key: named after
	// common/constants.py's ap2.PaymentReceipt.
	KeyCheckoutReceipt = "ap2.CheckoutReceipt"
	// vctCheckout is a closed Checkout Mandate's vct (docs/ap2/checkout_mandate.md, "Type").
	vctCheckout = "mandate.checkout.1"
)

// The one item this merchant sells, and the merchant (the checkout's merchant object and the receipt's iss).
const (
	itemID      = "example-item"
	itemTitle   = "Example item"
	itemCents   = 1000
	currency    = "USD"
	merchantID  = "example-merchant"
	merchantNm  = "Example Merchant"
	website     = "https://merchant.example"
	checkoutTTL = time.Hour
)

// restPrefix is where the HTTP+JSON binding is served; JSON-RPC is served at the root.
const restPrefix = "/rest"

// jwksPath serves the merchant's public key, against which a shopper checks the checkout's and the receipt's
// signatures (not a Kontra endpoint: a gate relays only the card and A2A).
const jwksPath = "/.well-known/jwks.json"

// maxBody bounds a request body: the gate's default limits.body (spec/relay.md §2.2), as the booking example does.
const maxBody = 4 << 20

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process: it serves until ctx ends and returns the exit code (2 for a usage error).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ap2-merchant", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "127.0.0.1", "address to listen on (0.0.0.0 in a container)")
	port := fs.Int("port", 9993, "port to listen on (0: any free port)")
	keyFile := fs.String("key", "", "PEM file with the merchant's P-256 signing key (default: a new key per start)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "ap2-merchant: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(stderr, "ap2-merchant: --port %d: want 0 to 65535\n", *port)
		return 2
	}
	key, err := loadKey(*keyFile)
	if err != nil {
		fmt.Fprintf(stderr, "ap2-merchant: %v\n", err)
		return 2
	}
	m, err := newMerchant(key)
	if err != nil {
		fmt.Fprintf(stderr, "ap2-merchant: %v\n", err)
		return 1
	}
	l, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(stderr, "ap2-merchant: %v\n", err)
		return 1
	}
	base := rootURL(l.Addr().(*net.TCPAddr))
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv := &http.Server{Handler: newHandler(base, m, log), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintln(stdout, base)
	fmt.Fprintf(stderr, "ap2-merchant: serving the card at %s%s, key %s at %s, JSON-RPC at / and HTTP+JSON under %s\n",
		base, a2asrv.WellKnownAgentCardPath, m.kid, jwksPath, restPrefix)

	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	select {
	case err := <-done:
		fmt.Fprintf(stderr, "ap2-merchant: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	return 0
}

// rootURL is the URL the card advertises. A wildcard listener is advertised on loopback: whoever reaches the agent
// from elsewhere (a gate, a sidecar) dials its own configured address, not the card's (D71).
func rootURL(a *net.TCPAddr) string {
	ip := a.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(a.Port))
}

// loadKey reads the P-256 key in file, or generates one when file is "".
func loadKey(file string) (*ecdsa.PrivateKey, error) {
	if file == "" {
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("--key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("--key %s: no PEM block", file)
	}
	var key any
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("--key %s: a %q block, want EC PRIVATE KEY or PRIVATE KEY", file, block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("--key %s: %w", file, err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, fmt.Errorf("--key %s: not a P-256 key (ES256)", file)
	}
	return ec, nil
}

// newHandler serves the agent whose root URL is base, with request bodies bounded by maxBody; a2a-go logs to log.
func newHandler(base string, m *merchant, log *slog.Logger) http.Handler {
	handler := a2asrv.NewHandler(agent{m}, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)), a2asrv.WithLogger(log),
		a2asrv.WithCallInterceptors(requireAP2{}))
	raw, err := json.Marshal(card(base))
	if err != nil {
		panic(err) // the card is this file's own constant data
	}
	jwks, err := json.Marshal(map[string]any{"keys": []any{m.jwk()}})
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, a2asrv.NewAgentCardHandler(rawCard(raw)))
	mux.HandleFunc("GET "+jwksPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})
	mux.Handle("POST /{$}", a2asrv.NewJSONRPCHandler(handler))
	mux.Handle(restPrefix+"/", http.StripPrefix(restPrefix, a2asrv.NewRESTHandler(handler)))
	return http.MaxBytesHandler(mux, maxBody)
}

// rawCard serves a card's JSON as it is (a2asrv.AgentCardJSONProducer).
type rawCard []byte

func (c rawCard) Card(context.Context) (*a2a.AgentCard, error) {
	var card a2a.AgentCard
	err := json.Unmarshal(c, &card)
	return &card, err
}

func (c rawCard) CardJSON(context.Context) ([]byte, error) { return c, nil }

// card is the agent's A2A 1.0 card.
func card(base string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        "Example Merchant (AP2)",
		Description: "Sells one example item under AP2 v0.2.0: a signed checkout, then a checkout receipt for a Checkout Mandate bound to it. Takes no payment.",
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(base+restPrefix, a2a.TransportProtocolHTTPJSON),
		},
		DefaultInputModes:  []string{"text/plain", "application/json"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
		Capabilities:       a2a.AgentCapabilities{Streaming: true, Extensions: extensions()},
		Skills: []a2a.AgentSkill{{
			ID:          "checkout",
			Name:        "Check out the example item",
			Description: "Answers an ap2.checkout (a merchant-signed checkout JWT), then a Checkout Receipt for an ap2.mandates.CheckoutMandateSdJwt bound to it.",
			Tags:        []string{"ap2", "merchant", "checkout"},
			Examples:    []string{"I'd like to buy the example item"},
		}},
	}
}

// extensions are the card's extension entries: AP2's alone, required, in the merchant role.
func extensions() []a2a.AgentExtension {
	return []a2a.AgentExtension{{
		URI:         ExtAP2,
		Description: "Plays the AP2 v0.2.0 Merchant role: a signed checkout, Checkout Mandates and Checkout Receipts as data parts.",
		Required:    true,
		Params:      map[string]any{"roles": []any{"merchant"}},
	}}
}

// requireAP2 refuses a message that does not activate AP2 (A2A-Extensions, or its 0.3 name X-A2A-Extensions):
// ExtensionSupportRequiredError, as the card's required entry says. Other methods (GetTask, CancelTask, …) need no
// activation.
type requireAP2 struct {
	a2asrv.PassthroughCallInterceptor
}

func (requireAP2) Before(ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	switch cc.Method() {
	case "SendMessage", "SendStreamingMessage":
	default:
		return ctx, nil, nil
	}
	for _, header := range []string{a2a.SvcParamExtensions, "X-A2A-Extensions"} {
		values, _ := cc.ServiceParams().Get(header)
		for _, v := range values {
			for _, uri := range strings.Split(v, ",") {
				if strings.TrimSpace(uri) == ExtAP2 {
					return ctx, nil, nil
				}
			}
		}
	}
	return ctx, nil, a2a.ErrExtensionSupportRequired
}

// agent is the agent's a2asrv.AgentExecutor.
type agent struct{ m *merchant }

// What the agent says.
const (
	askMandate = "Here is your checkout for 1 × " + itemTitle + " (10.00 USD), signed by " + merchantNm + ". Send a Checkout " +
		"Mandate bound to it in a data part " + KeyCheckoutMandate + " to complete the checkout. This merchant takes no " +
		"payment: pay through your Credential Provider and the merchant's payment processor."
	askAgain = "Send a Checkout Mandate bound to this task's checkout in a data part " + KeyCheckoutMandate + "."
)

// Execute runs one turn: the first message of a task is answered with a checkout; a later one carrying a Checkout
// Mandate is checked against it and answered with a Checkout Receipt.
func (a agent) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		issued, ok := issuedCheckout(ec)
		if !ok {
			c, err := a.m.checkout()
			if err != nil {
				yield(nil, err)
				return
			}
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(askMandate), a2a.NewDataPart(map[string]any{KeyCheckout: c}))
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, msg), nil)
			return
		}
		token, found := dataString(ec.Message, KeyCheckoutMandate)
		if !found {
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(askAgain))
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, msg), nil)
			return
		}
		r := a.m.complete(token, issued)
		receipt, err := a.m.sign(r)
		if err != nil {
			yield(nil, err)
			return
		}
		parts := []*a2a.Part{a2a.NewTextPart(r.text(issued.id)), a2a.NewDataPart(map[string]any{KeyCheckoutReceipt: receipt})}
		state := a2a.TaskStateCompleted
		if r.Status != "Success" {
			state = a2a.TaskStateInputRequired
		}
		yield(a2a.NewStatusUpdateEvent(ec, state, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, parts...)), nil)
	}
}

// Cancel ends a task that is not over yet canceled: the checkout is abandoned.
func (agent) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask != nil && ec.StoredTask.Status.State.Terminal() {
			yield(nil, a2a.ErrTaskNotCancelable)
			return
		}
		msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("The checkout was abandoned."))
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, msg), nil)
	}
}

// issued is a checkout the merchant answered: its JWT and the id in its payload.
type issued struct {
	jwt, id string
}

// issuedCheckout is the checkout this task was answered, if it was: the first agent message's ap2.checkout part,
// in the task's history or the question it waits on. The task store is the agent's own memory.
func issuedCheckout(ec *a2asrv.ExecutorContext) (issued, bool) {
	if ec.StoredTask == nil {
		return issued{}, false
	}
	for _, m := range append(slices.Clone(ec.StoredTask.History), ec.StoredTask.Status.Message) {
		if m == nil || m.Role != a2a.MessageRoleAgent {
			continue
		}
		for _, p := range m.Parts {
			obj, _ := p.Data().(map[string]any)
			c, _ := obj[KeyCheckout].(map[string]any)
			if jwt, _ := c["checkout_jwt"].(string); jwt != "" {
				id, _ := c["checkout_id"].(string)
				return issued{jwt: jwt, id: id}, true
			}
		}
	}
	return issued{}, false
}

// dataString is the string under key in the first data part of m that has key (the AP2 samples' find_data_part).
func dataString(m *a2a.Message, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	for _, p := range m.Parts {
		obj, _ := p.Data().(map[string]any)
		if v, ok := obj[key]; ok {
			s, _ := v.(string)
			return s, true
		}
	}
	return "", false
}

// --- the merchant: its key, checkouts and receipts ---

// merchant signs checkouts and receipts with its key, whose RFC 7638 thumbprint is kid.
type merchant struct {
	key *ecdsa.PrivateKey
	kid string
	now func() time.Time
}

func newMerchant(key *ecdsa.PrivateKey) (*merchant, error) {
	m := &merchant{key: key, now: time.Now}
	jwk := m.jwk()
	// RFC 7638: the required members, in lexicographic order, no whitespace.
	thumb, err := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{jwk["crv"], jwk["kty"], jwk["x"], jwk["y"]})
	if err != nil {
		return nil, err
	}
	m.kid = b64(sha(thumb))
	return m, nil
}

// jwk is the merchant's public key as a JWK (RFC 7517), with its kid once newMerchant set it.
func (m *merchant) jwk() map[string]string {
	pt, err := m.key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		panic(err) // a P-256 key loadKey checked
	}
	j := map[string]string{"kty": "EC", "crv": "P-256", "x": b64(pt[1:33]), "y": b64(pt[33:65]), "alg": "ES256", "use": "sig"}
	if m.kid != "" {
		j["kid"] = m.kid
	}
	return j
}

// checkout is a new checkout for the one item, as the ap2.checkout part's value: the merchant-signed checkout JWT
// (a UCP-shaped Checkout, as AP2's Python merchant writes it), its hash and what a shopper shows of it.
func (m *merchant) checkout() (map[string]any, error) {
	now := m.now()
	id := uuid.NewString()
	totals := []any{map[string]any{"type": "subtotal", "amount": itemCents}, map[string]any{"type": "total", "amount": itemCents}}
	payload := map[string]any{
		"id":       id,
		"merchant": map[string]any{"id": merchantID, "name": merchantNm, "website": website},
		"line_items": []any{map[string]any{
			"id":       "li_1",
			"item":     map[string]any{"id": itemID, "title": itemTitle, "price": itemCents},
			"quantity": 1,
			"totals":   totals,
		}},
		"status":   "incomplete",
		"currency": currency,
		"totals":   totals,
		"iat":      now.Unix(),
		"exp":      now.Add(checkoutTTL).Unix(),
	}
	jwt, err := m.sign(payload)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"checkout_id":   id,
		"checkout_jwt":  jwt,
		"checkout_hash": b64(sha([]byte(jwt))),
		"item_label":    itemTitle,
		"amount_cents":  itemCents,
		"currency":      currency,
	}, nil
}

// sign is payload as a compact JWS, ES256, with the merchant's kid.
func (m *merchant) sign(payload any) (string, error) {
	h, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": m.kid})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	input := b64(h) + "." + b64(p)
	digest := sha([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, m.key, digest)
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64(sig), nil
}

// verifyOwn checks that jwt is ES256-signed by this merchant's key and returns its payload.
func (m *merchant) verifyOwn(jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a compact JWS")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := decodeJSON(parts[0], &header); err != nil || header.Alg != "ES256" {
		return nil, errors.New("not an ES256 JWS")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("not an ES256 signature")
	}
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&m.key.PublicKey, sha([]byte(parts[0]+"."+parts[1])), r, s) {
		return nil, errors.New("not signed by this merchant")
	}
	var payload map[string]any
	if err := decodeJSON(parts[1], &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// receipt is a Checkout Receipt's payload (AP2 v0.2.0 code/sdk/schemas/ap2/checkout_receipt.json): Success names
// the order, Error the error.
type receipt struct {
	Status           string `json:"status"` // Success or Error
	Iss              string `json:"iss"`
	Iat              int64  `json:"iat"`
	Reference        string `json:"reference"`
	OrderID          string `json:"order_id,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// text is what the agent says beside the receipt for the checkout id.
func (r receipt) text(id string) string {
	if r.Status == "Success" {
		return fmt.Sprintf("Checkout %s is confirmed as order %s. No payment was taken here.", id, r.OrderID)
	}
	return fmt.Sprintf("The Checkout Mandate for checkout %s was refused (%s): %s. %s", id, r.Error, r.ErrorDescription, askAgain)
}

// complete checks a Checkout Mandate against the checkout the task issued and answers the receipt: Success with a
// new order id, or Error with why. Its reference is the hash of the closed mandate JWT, computed as AP2's Python SDK
// does (mandate.py get_closed_mandate_jwt; docs/ap2/specification.md "Dispute").
func (m *merchant) complete(token string, c issued) receipt {
	r := receipt{Iss: website, Iat: m.now().Unix(), Reference: b64(sha([]byte(closedJWT(token))))}
	if code, err := m.checkoutMandate(token, c); err != nil {
		r.Status, r.Error, r.ErrorDescription = "Error", code, err.Error()
		return r
	}
	r.Status, r.OrderID = "Success", "order_"+uuid.NewString()
	return r
}

// checkoutMandate is the Merchant's check of a Checkout Mandate (docs/ap2/specification.md "Verification", "Merchant";
// docs/ap2/checkout_mandate.md "Mandate Schema"), as far as this example goes:
//
//   - the closed mandate (the last SD-JWT of a "~~" chain) is read as an SD-JWT (RFC 9901): its payload with every
//     disclosure presented whose digest it, or a disclosure it includes, lists; _sd_alg is sha-256 or absent;
//   - a disclosed object with vct mandate.checkout.1 holds checkout_jwt and checkout_hash, and checkout_hash is the
//     base64url SHA-256 of the checkout_jwt string;
//   - checkout_jwt is byte for byte the checkout this task issued, signed by this merchant's key, and not expired; the
//     mandate itself, if it has exp, is not expired.
//
// It does NOT verify the mandate's issuer signature, its key-binding JWT or delegation chain (sd_hash, cnf), nor
// evaluate an open mandate's constraints: those need the Credential Provider's and the shopping agent's keys, which
// a real merchant obtains from the parties it works with (README.md). It returns an error code and why on failure.
func (m *merchant) checkoutMandate(token string, c issued) (string, error) {
	claims, err := disclose(closedSegment(token))
	if err != nil {
		return "invalid_mandate", err
	}
	mandate, ok := findVCT(claims, vctCheckout)
	if !ok {
		return "invalid_mandate", fmt.Errorf("no disclosed claims with vct %s", vctCheckout)
	}
	jwt, _ := mandate["checkout_jwt"].(string)
	hash, _ := mandate["checkout_hash"].(string)
	if jwt == "" || hash == "" {
		return "invalid_mandate", errors.New("the mandate does not disclose checkout_jwt and checkout_hash")
	}
	if b64(sha([]byte(jwt))) != hash {
		return "checkout_hash_mismatch", errors.New("checkout_hash is not the SHA-256 of checkout_jwt")
	}
	if jwt != c.jwt {
		return "unknown_checkout", errors.New("checkout_jwt is not the checkout this task issued")
	}
	payload, err := m.verifyOwn(jwt)
	if err != nil {
		return "unknown_checkout", fmt.Errorf("checkout_jwt: %v", err)
	}
	now := m.now().Unix()
	if exp, ok := payload["exp"].(float64); !ok || int64(exp) < now {
		return "checkout_expired", errors.New("the checkout has expired")
	}
	if exp, ok := mandate["exp"].(float64); ok && int64(exp) < now {
		return "mandate_expired", errors.New("the mandate has expired")
	}
	return "", nil
}

// closedSegment is the closed mandate of a presentation: after the last "~~" of a delegation chain, or all of it.
func closedSegment(token string) string {
	if i := strings.LastIndex(token, "~~"); i >= 0 {
		return token[i+2:]
	}
	return token
}

// closedJWT is the closed mandate's issuer-signed JWT, the receipt's reference input (AP2 SDK
// get_closed_mandate_jwt).
func closedJWT(token string) string {
	s, _, _ := strings.Cut(closedSegment(token), "~")
	return s
}

// disclose reads an SD-JWT "<jwt>~<disclosure>~…~[<kb-jwt>]" (RFC 9901 §4) and returns its payload with the presented
// disclosures put back (§7.1 steps 3 to 5). Nothing is signature-checked.
func disclose(sdjwt string) (map[string]any, error) {
	pieces := strings.Split(sdjwt, "~")
	parts := strings.Split(pieces[0], ".")
	if len(parts) != 3 {
		return nil, errors.New("the mandate is not an SD-JWT")
	}
	var payload map[string]any
	if err := decodeJSON(parts[1], &payload); err != nil {
		return nil, fmt.Errorf("the mandate's payload: %v", err)
	}
	if alg, ok := payload["_sd_alg"]; ok && alg != "sha-256" {
		return nil, fmt.Errorf("_sd_alg %v: only sha-256 is supported", alg)
	}
	disclosures := map[string][]any{}
	for _, d := range pieces[1:] {
		if d == "" || strings.Contains(d, ".") { // the empty end, or a key-binding JWT
			continue
		}
		var arr []any
		if err := decodeJSON(d, &arr); err != nil || (len(arr) != 2 && len(arr) != 3) {
			return nil, errors.New("a disclosure is not a JSON array of 2 or 3 elements")
		}
		digest := b64(sha([]byte(d)))
		if _, dup := disclosures[digest]; dup {
			return nil, errors.New("a disclosure is presented twice")
		}
		disclosures[digest] = arr
	}
	used := map[string]bool{}
	out, err := resolve(payload, disclosures, used)
	if err != nil {
		return nil, err
	}
	obj, _ := out.(map[string]any)
	return obj, nil
}

// resolve replaces v's digests by the disclosures they name: an object's _sd entries become members, an array's
// {"...": digest} elements become values; digests with no disclosure are left out. A disclosure used twice is
// refused.
func resolve(v any, disclosures map[string][]any, used map[string]bool) (any, error) {
	take := func(digest any, n int) ([]any, error) {
		d, _ := digest.(string)
		arr, ok := disclosures[d]
		if !ok || len(arr) != n {
			return nil, nil
		}
		if used[d] {
			return nil, errors.New("a disclosure is referenced twice")
		}
		used[d] = true
		return arr, nil
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if k == "_sd" || k == "_sd_alg" {
				continue
			}
			r, err := resolve(val, disclosures, used)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		digests, _ := x["_sd"].([]any)
		for _, digest := range digests {
			arr, err := take(digest, 3)
			if err != nil {
				return nil, err
			}
			if arr == nil {
				continue
			}
			name, ok := arr[1].(string)
			if _, clash := out[name]; !ok || clash || name == "_sd" || name == "..." {
				return nil, errors.New("a disclosure names a claim it may not")
			}
			r, err := resolve(arr[2], disclosures, used)
			if err != nil {
				return nil, err
			}
			out[name] = r
		}
		return out, nil
	case []any:
		out := []any{}
		for _, el := range x {
			if obj, ok := el.(map[string]any); ok && len(obj) == 1 && obj["..."] != nil {
				arr, err := take(obj["..."], 2)
				if err != nil {
					return nil, err
				}
				if arr == nil {
					continue
				}
				el = arr[1]
			}
			r, err := resolve(el, disclosures, used)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	}
	return v, nil
}

// findVCT is the first object in v, depth first, whose vct is vct.
func findVCT(v any, vct string) (map[string]any, bool) {
	switch x := v.(type) {
	case map[string]any:
		if x["vct"] == vct {
			return x, true
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if m, ok := findVCT(x[k], vct); ok {
				return m, true
			}
		}
	case []any:
		for _, el := range x {
			if m, ok := findVCT(el, vct); ok {
				return m, true
			}
		}
	}
	return nil, false
}

func sha(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// decodeJSON reads a base64url (unpadded) JSON value into v.
func decodeJSON(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}
