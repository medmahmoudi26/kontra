// warden_identity_test.go — enrolment, against a real TLS handshake.
//
// NOTHING HERE ASSERTS AN ARGV OR A STRUCT FIELD WHERE A HANDSHAKE WOULD DO. The claims this slice
// makes about identity are claims about what a TLS stack does with a certificate — that a Controller
// which does not hold the pinned CA never receives the token, that the assignment route cannot be
// reached without a client certificate, that one Warden cannot read another's assignment — and every
// one of those passes vacuously against a test that checks a field. So each runs against
// `httptest.NewUnstartedServer` carrying THE REAL `tls.Config` the Controller serves, driven by the
// same `enrol` and the same `client()` a Machine uses.
//
// ═══ THE CONTROLS ═══
//
// The pin test is the one that most needs them, and it carries three:
//
//	the SAME token enrols successfully against the RIGHT Controller   → the token is good
//	the impostor's enrol handler is NEVER REACHED                     → the secret was not sent
//	the token is STILL SPENDABLE afterwards                           → and it really was not sent
//
// Without the first, "the impostor was refused" is indistinguishable from a broken token. Without
// the second and third, it is indistinguishable from a Warden that sent its token, got a
// certificate, and then rejected it — which is a different and much weaker property.
package warden

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- the fixture ---------------------------------------------------------------------------------

// controller stands up a real Fleet CA behind a real TLS listener, and hands back the two things a
// test needs: the URL a Machine would type, and the CA itself for writing assignments.
type controller struct {
	ca  *wardenCA
	srv *httptest.Server
	// enrolments counts requests that REACHED the enrol handler, which is how the pin test proves
	// the secret was never sent rather than merely never accepted.
	enrolments *atomic.Int64
}

func newController(t *testing.T) *controller {
	t.Helper()
	ca, err := openWardenCA(t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("creating a Fleet CA: %v", err)
	}
	seen := &atomic.Int64{}
	inner := ca.handler(io.Discard)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wardenEnrolPath {
			seen.Add(1)
		}
		inner.ServeHTTP(w, r)
	}))
	// `127.0.0.1` because that is what httptest listens on and therefore what the Machine will
	// verify the certificate's SANs against.
	cfg, err := ca.tlsConfig([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatalf("building the Controller's TLS config: %v", err)
	}
	srv.TLS = cfg
	// THE HANDSHAKE FAILURES BELOW ARE THE POINT, so their log lines are not. `net/http` writes
	// "TLS handshake error … remote error: tls: bad certificate" to the default logger every time a
	// Machine correctly refuses an impostor, and a suite that prints that in CI reads as broken.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// CONTROL ON THE FIXTURE ITSELF: httptest must have kept OUR certificate rather than minting its
	// own, or every pin assertion in this file would be about a certificate the CA never issued.
	if len(srv.TLS.Certificates) == 0 || srv.TLS.ClientAuth != tlsVerifyIfGiven {
		t.Fatalf("httptest replaced the Controller's TLS config, so nothing here tests the real one")
	}
	return &controller{ca: ca, srv: srv, enrolments: seen}
}

// tlsVerifyIfGiven is spelled out so the fixture's control above reads as the assertion it is.
const tlsVerifyIfGiven = 3 // tls.VerifyClientCertIfGiven

// fixtureTenant is the tenant every Machine in this file enrols into unless a test names another.
// NOT `default`: a fixture that used the fallback name would pass just as well against a Controller
// that ignored the tenant entirely, which is the whole thing slice 08 has to prove it does not.
const fixtureTenant = "acme"

func (c *controller) token(t *testing.T) string { return c.tokenFor(t, fixtureTenant) }

func (c *controller) tokenFor(t *testing.T, tenant string) string {
	t.Helper()
	tok, err := c.ca.mintToken(time.Hour, tenant)
	if err != nil {
		t.Fatalf("minting an enrolment token for %s: %v", tenant, err)
	}
	return tok.String()
}

// assign writes a tenant-wide or per-Warden assignment file. Slice 04 was forecast to replace this
// with Temporal and did not: pushing desired state through workflow history costs 11 events per
// change against 6 for the Machine reading it here and reporting that it changed, so the RECORD moved
// into the workflow and the assignment did not (warden_workflow.go's header has both counts).
//
// THE TENANT IS A DIRECTORY, and slice 08 made it one: `assignments/<tenant>/…`. It used to be
// `assignments/default.json` for the whole Fleet, which is a cross-tenant read the moment a second
// party enrols.
func (c *controller) assign(t *testing.T, tenant, name string, a wardenAssignment) {
	t.Helper()
	dir := filepath.Join(c.ca.Dir, caAssignDir, tenant)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- the token's grammar --------------------------------------------------------------------------

// THE TOKEN IS ONE STRING CARRYING TWO THINGS, and the parse refuses everything that is not exactly
// that. A lenient parse here does not fail safe: a token whose pin does not decode would become a
// pin that matches nothing, and "matches nothing" is one branch away from "was never checked".
func TestEnrolTokenRoundTripsAndRefusesEverythingElse(t *testing.T) {
	var pin [sha256.Size]byte
	for i := range pin {
		pin[i] = byte(i)
	}
	tok, err := newEnrolToken(pin)
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseEnrolToken(tok.String())
	if err != nil {
		t.Fatalf("a token this file just minted did not parse: %v", err)
	}
	if back.CAPin != pin || back.Secret != tok.Secret {
		t.Errorf("round trip lost something: pin %x secret %q", back.CAPin, back.Secret)
	}
	// TWO MINTS MUST DIFFER, or the "single use" property is about a constant.
	other, _ := newEnrolToken(pin)
	if other.Secret == tok.Secret {
		t.Fatal("two freshly minted tokens share a secret, so nothing here is random")
	}

	short := base64.RawURLEncoding.EncodeToString(pin[:16])
	for _, bad := range []string{
		"",
		"kw1",
		"kw1." + base64.RawURLEncoding.EncodeToString(pin[:]),          // no secret half
		"kw2." + base64.RawURLEncoding.EncodeToString(pin[:]) + ".abc", // a grammar this does not speak
		"kw1." + short + ".abc",                                        // a pin that is not 32 bytes
		"kw1.!!!not-base64!!!.abc",
		"kw1." + base64.RawURLEncoding.EncodeToString(pin[:]) + ".", // an empty secret
		base64.RawURLEncoding.EncodeToString(pin[:]) + ".abc",       // no prefix
	} {
		if _, err := parseEnrolToken(bad); err == nil {
			t.Errorf("parseEnrolToken(%q) accepted a string it must refuse", bad)
		}
	}
}

// --- the pin --------------------------------------------------------------------------------------

// THE SECRET IS NOT SENT TO A CONTROLLER THAT CANNOT PROVE IT HOLDS THE PINNED CA. This is the whole
// reason the CA's fingerprint rides inside the token, and it is what makes a leaked token useless for
// standing up an impostor Fleet.
//
// See this file's header for the three controls and why each is load-bearing.
func TestJoinRefusesAControllerThatDoesNotHoldThePinnedCA(t *testing.T) {
	real, impostor := newController(t), newController(t)
	token := real.token(t)

	// CONTROL 1: the impostor is a working Controller in every other respect — its own tokens enrol
	// against it — so what is being refused is the PIN and not a broken server.
	if _, err := enrol(context.Background(), t.TempDir(), impostor.srv.URL, impostor.token(t)); err != nil {
		t.Fatalf("the impostor cannot enrol anyone at all, so nothing here tests the pin: %v", err)
	}

	// THE CLAIM.
	before := impostor.enrolments.Load()
	_, err := enrol(context.Background(), t.TempDir(), impostor.srv.URL, token)
	if err == nil {
		t.Fatal("a Machine enrolled with a Controller holding a different CA")
	}
	if !strings.Contains(err.Error(), "does not hold the CA") {
		t.Errorf("the refusal does not say what happened, so an operator will read it as an expired "+
			"certificate: %v", err)
	}

	// CONTROL 2: the impostor's enrol handler was never reached — the connection was refused during
	// the handshake, before any body was written.
	if got := impostor.enrolments.Load(); got != before {
		t.Errorf("the impostor's enrol handler was reached %d time(s); the token WAS sent", got-before)
	}

	// CONTROL 3: …and the token is still spendable, which is the same fact from the other side.
	if _, err := enrol(context.Background(), t.TempDir(), real.srv.URL, token); err != nil {
		t.Errorf("the token was spent by the refused attempt after all: %v", err)
	}
}

// ENROLMENT HAS NO MEANING OVER PLAIN HTTP, and refusing loudly is better than a pin that silently
// has nothing to check. This is the one shortcut every bootstrap protocol is tempted by.
func TestEnrolRefusesPlainHTTP(t *testing.T) {
	c := newController(t)
	plain := strings.Replace(c.srv.URL, "https://", "http://", 1)
	_, err := enrol(context.Background(), t.TempDir(), plain, c.token(t))
	if err == nil {
		t.Fatal("enrolment over http was allowed")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("the refusal does not name the problem: %v", err)
	}
	// CONTROL: the very same Controller and the very same token DO work over https, so the refusal is
	// about the scheme rather than about anything else being wrong.
	if _, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.token(t)); err != nil {
		t.Fatalf("the control enrolment failed, so the refusal above proves nothing: %v", err)
	}
}

// --- single use ------------------------------------------------------------------------------------

// A ONE-TIME TOKEN IS SPENT ONCE. The Controller records it as spent BEFORE it signs anything
// (warden_ca.go), so a second attempt is refused whether or not the first one finished.
func TestAnEnrolmentTokenIsSpentExactlyOnce(t *testing.T) {
	c := newController(t)
	token := c.token(t)

	first, err := enrol(context.Background(), t.TempDir(), c.srv.URL, token)
	if err != nil {
		t.Fatalf("the first enrolment failed, so nothing below is about single use: %v", err)
	}
	if first.Record.WardenID == "" {
		t.Fatal("the first enrolment produced no Warden id")
	}
	if _, err := enrol(context.Background(), t.TempDir(), c.srv.URL, token); err == nil {
		t.Fatal("the same token enrolled a second Machine")
	}
	// CONTROL: a FRESH token still works, so what was refused is the reuse and not the Controller
	// having stopped enrolling.
	if _, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.token(t)); err != nil {
		t.Errorf("a fresh token was refused too, so the Controller is simply broken: %v", err)
	}
}

// AN EXPIRED TOKEN IS REFUSED. Separate from single use because the two are different failures and a
// Controller that only implemented one of them would pass the test above.
func TestAnExpiredEnrolmentTokenIsRefused(t *testing.T) {
	c := newController(t)
	tok, err := c.ca.mintToken(-time.Second, fixtureTenant) // already past its expiry
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enrol(context.Background(), t.TempDir(), c.srv.URL, tok.String()); err == nil {
		t.Fatal("an expired token enrolled a Machine")
	}
	// CONTROL: the same CA, a token with a future expiry, works.
	if _, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.token(t)); err != nil {
		t.Errorf("a live token was refused, so the expiry check is not what refused the other: %v", err)
	}
}

// --- what the CA will and will not take from a Machine -------------------------------------------------

// THE CSR'S SUBJECT IS DISCARDED. A CSR is an assertion by whoever built it; the only fact in it is
// the key and the proof of possession. A CA that copied the subject through would let a Machine name
// itself — the classic that turns "the client sent its own CN" into an authorisation bug the moment
// anything downstream reads a name.
func TestTheCAIgnoresTheNameAMachineAsksFor(t *testing.T) {
	ca, err := openWardenCA(t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "wdn-0000000000000000", Organization: []string{"kontra Controller"}},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))

	leaf, err := ca.sign(csr, fixtureTenant)
	if err != nil {
		t.Fatalf("signing a well-formed request failed: %v", err)
	}
	if leaf.Subject.CommonName == "wdn-0000000000000000" {
		t.Error("the CA issued the name the Machine asked for")
	}
	if want := wardenIDFor(leaf); leaf.Subject.CommonName != want {
		t.Errorf("the subject is %q; it must be the id derived from the key, %q", leaf.Subject.CommonName, want)
	}
	if len(leaf.Subject.Organization) != 1 || leaf.Subject.Organization[0] != "kontra Warden" {
		t.Errorf("the organisation the Machine asked for survived: %v", leaf.Subject.Organization)
	}

	// A WARDEN'S CERTIFICATE IS CLIENT AUTH AND NOTHING ELSE, because a Machine dials out and is never
	// dialled. ServerAuth here would be a certificate that could stand up a TLS server the whole Fleet
	// trusts.
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("the certificate's extended key usage is %v, want client auth alone", leaf.ExtKeyUsage)
	}
	if leaf.IsCA {
		t.Error("the CA issued a Warden a certificate that can sign others")
	}
}

// A CSR THE SENDER CANNOT PROVE IT HOLDS THE KEY FOR IS REFUSED. Without the signature check a CSR is
// just a public key somebody typed, and this would issue certificates for keys held by whoever asked.
func TestTheCARefusesACertificateRequestThatIsNotSignedByItsOwnKey(t *testing.T) {
	ca, err := openWardenCA(t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL: intact, it signs.
	good := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	if _, err := ca.sign(good, fixtureTenant); err != nil {
		t.Fatalf("the intact request was refused, so the corruption below proves nothing: %v", err)
	}
	// One byte of the SIGNATURE flipped — the last byte of the DER, which is inside it.
	tampered := make([]byte, len(der))
	copy(tampered, der)
	tampered[len(tampered)-1] ^= 0xff
	bad := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: tampered}))
	if _, err := ca.sign(bad, fixtureTenant); err == nil {
		t.Error("the CA signed a certificate request whose own signature does not verify")
	}
}

// --- the assignment route --------------------------------------------------------------------------

// THE ASSIGNMENT ROUTE TAKES NO BEARER TOKEN, ONLY A CERTIFICATE. If it took either, the one-time
// enrolment token would have become a permanent credential — the exact thing the split between the
// two routes exists to prevent.
func TestTheAssignmentRouteRefusesEverythingButAClientCertificate(t *testing.T) {
	c := newController(t)
	token := c.token(t)
	id, err := enrol(context.Background(), t.TempDir(), c.srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	c.assign(t, fixtureTenant, "default", wardenAssignment{Generation: 1, Workers: []assignedWorker{{Name: "n", Version: "1"}}})

	// CONTROL: with the certificate, it answers.
	w := &warden{id: id, http: id.client()}
	got, err := w.assignment(context.Background())
	if err != nil {
		t.Fatalf("the enrolled Machine could not read its assignment, so the refusals below prove "+
			"nothing: %v", err)
	}
	if len(got) != 1 || got[0].Name != "n" {
		t.Fatalf("the assignment came back wrong: %v", got)
	}

	// No certificate at all: refused, even holding the token that enrolled this Machine.
	anon := c.srv.Client() // trusts the server, offers no client certificate
	req, _ := http.NewRequest(http.MethodGet, c.srv.URL+wardenAssignmentPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := anon.Do(req)
	if err != nil {
		t.Fatalf("the anonymous request could not be made at all: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the assignment route answered %s to a caller with no certificate", resp.Status)
	}
}

// A WARDEN CANNOT READ ANOTHER WARDEN'S ASSIGNMENT, and there is no request field it could put
// another's name in even if it tried: the id is derived from the key the handshake proved it holds.
func TestAWardenReadsOnlyItsOwnAssignment(t *testing.T) {
	c := newController(t)
	a, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.token(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.token(t))
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL: two enrolments really are two different Wardens. If they shared an id this test would
	// be comparing one Machine with itself.
	if a.Record.WardenID == b.Record.WardenID {
		t.Fatalf("two enrolments produced one id (%s), so nothing here is separated", a.Record.WardenID)
	}

	c.assign(t, fixtureTenant, a.Record.WardenID, wardenAssignment{Workers: []assignedWorker{{Name: "for-a", Version: "1"}}})
	c.assign(t, fixtureTenant, b.Record.WardenID, wardenAssignment{Workers: []assignedWorker{{Name: "for-b", Version: "1"}}})

	for _, tc := range []struct {
		id   *wardenIdentity
		want string
	}{{a, "for-a"}, {b, "for-b"}} {
		w := &warden{id: tc.id, http: tc.id.client()}
		got, err := w.assignment(context.Background())
		if err != nil {
			t.Fatalf("%s could not read its assignment: %v", tc.id.Record.WardenID, err)
		}
		if len(got) != 1 || got[0].Name != tc.want {
			t.Errorf("%s was given %v, want %s", tc.id.Record.WardenID, got, tc.want)
		}
	}
}

// A FLEET-WIDE DEFAULT IS THE COMMON CASE, and no file at all is an EMPTY assignment rather than an
// error — a Machine that has enrolled and been given nothing should hold nothing. A Warden that
// treated "no file" as a failure would keep whatever it was running last, which is the drift a
// desired-state loop exists to remove.
func TestAnAssignmentFallsBackToTheFleetDefaultAndOtherwiseIsEmpty(t *testing.T) {
	c := newController(t)
	id, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.token(t))
	if err != nil {
		t.Fatal(err)
	}
	w := &warden{id: id, http: id.client()}

	got, err := w.assignment(context.Background())
	if err != nil {
		t.Fatalf("a Machine with no assignment file got an error rather than an empty answer: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("no assignment file produced %d Worker(s): %v", len(got), got)
	}

	c.assign(t, fixtureTenant, "default", wardenAssignment{Workers: []assignedWorker{{Name: "fleetwide", Version: "1"}}})
	got, err = w.assignment(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "fleetwide" {
		t.Fatalf("the Fleet-wide default was not used: %v %v", got, err)
	}

	// …and a per-Warden file overrides it, which is what `spread=` and packing will need.
	c.assign(t, fixtureTenant, id.Record.WardenID, wardenAssignment{Workers: []assignedWorker{{Name: "justme", Version: "1"}}})
	got, err = w.assignment(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "justme" {
		t.Errorf("the per-Warden assignment did not override the default: %v %v", got, err)
	}
}

// --- what persists ------------------------------------------------------------------------------------

// THE IDENTITY OUTLIVES THE PROCESS, which is the whole word "persists" in this slice's title. The
// "restart" is a fresh `loadIdentity` over the same directory — everything a systemd restart has —
// and it must be able to make the same authenticated call.
func TestAnIdentityPersistsAndStillAuthenticatesAfterAProcessRestart(t *testing.T) {
	c := newController(t)
	dir := t.TempDir()
	first, err := enrol(context.Background(), dir, c.srv.URL, c.token(t))
	if err != nil {
		t.Fatal(err)
	}
	c.assign(t, fixtureTenant, "default", wardenAssignment{Workers: []assignedWorker{{Name: "still-here", Version: "1"}}})

	// The restart.
	second, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("the identity did not survive: %v", err)
	}
	if second.Record.WardenID != first.Record.WardenID {
		t.Errorf("the Machine came back as somebody else: %s then %s", first.Record.WardenID, second.Record.WardenID)
	}
	w := &warden{id: second, http: second.client()}
	got, err := w.assignment(context.Background())
	if err != nil {
		t.Fatalf("the reloaded identity could not authenticate: %v", err)
	}
	if len(got) != 1 || got[0].Name != "still-here" {
		t.Errorf("the reloaded Machine read %v", got)
	}

	// THE KEY IS 0600 AND THE REST IS NOT. A world-readable private key is the one file permission
	// that matters here, and it is checked rather than intended.
	fi, err := os.Stat(filepath.Join(wardenIdentityDir(dir), wardenKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the private key is mode %o, want 600", perm)
	}
	// …and it is a key, not the certificate written twice.
	body, err := os.ReadFile(filepath.Join(wardenIdentityDir(dir), wardenKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if block, _ := pem.Decode(body); block == nil || !strings.Contains(block.Type, "PRIVATE KEY") {
		t.Errorf("%s does not hold a private key", wardenKeyFile)
	}
}

// A DIRECTORY THAT WAS NEVER ENROLLED SAYS SO BY NAME, because `kontra warden serve` prints the
// `join` command in that case and prints an error in every other — and telling the two apart is the
// difference between an operator running one command and reading a stack trace.
func TestAnUnenrolledDirectoryIsItsOwnError(t *testing.T) {
	_, err := loadIdentity(t.TempDir())
	if err == nil {
		t.Fatal("an empty directory loaded as an identity")
	}
	if !isNotEnrolled(err) {
		t.Errorf("the error is not recognisable as `not enrolled`: %v", err)
	}

	// CONTROL: a directory with a RECORD but no key is a DIFFERENT failure — corrupt, not absent —
	// and must not be reported as "never joined", or `join` would be suggested for a Machine that
	// already spent its token.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, wardenRecordFile), []byte(`{"wardenId":"wdn-x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err = loadIdentity2(dir)
	if err == nil {
		t.Fatal("a record with no key loaded as an identity")
	}
	if isNotEnrolled(err) {
		t.Errorf("a half-written identity is reported as `not enrolled`: %v", err)
	}
}

func isNotEnrolled(err error) bool { return err != nil && errorsIs(err, errNotEnrolled) }

// Two tiny shims so the assertions above read as prose. `errorsIs` is `errors.Is` under another
// name, kept local so this file's imports stay about crypto and HTTP.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func loadIdentity2(dir string) error {
	_, err := loadIdentity(dir)
	return err
}
