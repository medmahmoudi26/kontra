package warden

// warden_identity.go — how a **Machine** becomes somebody, and stays that person.
//
// ADR 0036 puts one sentence of load on this file: "A **Warden**'s enrolment mints credentials
// scoped to one namespace, so a compromised **Machine** cannot address another tenant's queues at
// all." The scoping is slice 08. What lands here is the thing 08, 04 and 05 all need first — a
// credential a **Machine** holds, that it obtained itself, that nobody had to copy onto it.
//
// ═══ WHY A KEY AND NOT A TOKEN ═══
//
// Everything authenticated in this repo today is a static bearer token read from the environment
// (`control/orchestrator/src/auth.ts`, and `cli/internal/config/config.go` writes four of them into a compose stack). That model
// has one property that does not survive a **Fleet** of **Machines** running code kontra did not
// write: THE CREDENTIAL IS THE SAME BYTES ON EVERY HOLDER. To give a **Machine** a bearer token is
// to give it a copy of a secret that already exists somewhere else, and a compromised Machine hands
// back a credential that authenticates as every other Machine.
//
// So the persistent credential is a PRIVATE KEY THAT IS GENERATED ON THE MACHINE AND NEVER LEAVES
// IT. What crosses the wire at enrolment is a certificate signing request — a public key plus a
// proof that the sender holds the matching private one — and what comes back is a certificate. The
// Controller never sees the secret, so there is no moment at which it could leak one, and revoking
// one **Machine** does not touch any other.
//
// ═══ THE ONE-TIME TOKEN CARRIES THE CA'S FINGERPRINT, AND THAT IS THE WHOLE POINT ═══
//
// Enrolment has a chicken-and-egg at its centre: the **Machine** has no way to know which Controller
// is the real one, because the thing that would tell it — the CA — is what it is about to be given.
// Handing an operator two strings to paste (a token and a CA fingerprint) is how that gets solved
// badly: the second one is the one nobody copies.
//
// So there is ONE string and it carries both halves:
//
//	kw1.<43 chars: sha256 of the CA's SubjectPublicKeyInfo>.<43 chars: the one-time secret>
//
// `join` parses the pin FIRST, dials with a TLS config that trusts nothing but that pin, and only
// then sends the secret. The property that buys is worth stating plainly: A LEAKED ENROLMENT TOKEN
// CANNOT BE REPLAYED AGAINST AN IMPOSTOR CONTROLLER. Whoever holds the token can enrol a Machine
// into the real Fleet — which is what a bootstrap token is for, and why it is single-use and
// expires — but cannot use it to make a Machine trust a CA of their choosing, which is the
// compromise that would survive the token being spent.
//
// This is not an invention; k3s ships `K10<ca-hash>::<user>:<password>` for the same reason. It is
// written out here because the alternative — `InsecureSkipVerify` for the first request, "it is only
// the bootstrap" — is the one every implementation reaches for and is exactly the request that must
// not be insecure.
//
// ═══ WHAT PERSISTS, AND WHAT DELIBERATELY DOES NOT ═══
//
//	<state>/identity/key.pem    0600   the private key. Never sent, never logged, never copied.
//	<state>/identity/cert.pem   0644   what the CA issued for it.
//	<state>/identity/ca.pem     0644   the Fleet CA, so every later call verifies the Controller.
//	<state>/warden.json         0644   who this Warden is and where it calls home.
//
// AND NOTHING ELSE. There is no record here of which **Workers** are running, and that absence is a
// contract rather than an omission — `driver.go` states the rule this slice inherits ("`list()` reads
// the runtime, never a local database") and a Warden that wrote a `workers.json` beside its identity
// would have re-introduced it one directory up from where it was refused. warden_test.go asserts the
// state directory's contents after a reconcile, so the absence is checked rather than intended.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
)

// --- the enrolment token -------------------------------------------------------------------------

// enrolTokenPrefix versions the token format. A **Warden** and the Controller that enrols it are
// separated by an organisational boundary (ADR 0037: "The Warden is a protocol across an
// organisational boundary, and version skew with customers who upgrade slowly is now a permanent
// cost"), so the first field of the first string they exchange says which grammar it is in.
const enrolTokenPrefix = "kw1"

// enrolTokenSecretLen is the entropy of the single-use half, in bytes. 32 because the token is
// printed, pasted and occasionally logged by whoever is provisioning; it is spent once and expires,
// and neither of those is a reason to be able to guess it.
const enrolTokenSecretLen = 32

// enrolToken is a one-time enrolment token as an operator holds it: the CA to pin, and the secret to
// spend. It is a struct rather than three return values because the two halves must never be
// separated — a caller that has the secret and not the pin is the insecure-bootstrap case this
// format exists to make unspellable.
type enrolToken struct {
	// CAPin is sha256 over the CA certificate's SubjectPublicKeyInfo. The SPKI and not the whole
	// certificate: a CA that re-issues its own certificate (a longer validity, a corrected name)
	// keeps its key, and pinning the certificate would invalidate every unspent token for a change
	// that altered nothing about who the Controller is.
	CAPin [sha256.Size]byte
	// Secret is spent exactly once. It is a string rather than bytes because it is only ever
	// compared by its hash and never interpreted.
	Secret string
}

// newEnrolToken mints one. The secret is generated here and the CALLER stores only its hash — see
// wardenCA.mintToken, which never writes the value it printed.
func newEnrolToken(caPin [sha256.Size]byte) (enrolToken, error) {
	b := make([]byte, enrolTokenSecretLen)
	if _, err := rand.Read(b); err != nil {
		return enrolToken{}, fmt.Errorf("no entropy to mint an enrolment token: %w", err)
	}
	return enrolToken{CAPin: caPin, Secret: base64.RawURLEncoding.EncodeToString(b)}, nil
}

// String is the one string an operator pastes onto a **Machine**.
func (t enrolToken) String() string {
	return enrolTokenPrefix + "." +
		base64.RawURLEncoding.EncodeToString(t.CAPin[:]) + "." + t.Secret
}

// parseEnrolToken reads one back, STRICTLY.
//
// Every refusal here is a refusal to dial, which is the point: a token whose pin is the wrong length
// would otherwise become a pin that matches nothing, and "matches nothing" and "was never checked"
// are one typo apart in every implementation that is lenient here.
func parseEnrolToken(s string) (enrolToken, error) {
	bad := func(why string) (enrolToken, error) {
		return enrolToken{}, fmt.Errorf("that is not an enrolment token (%s) — `kontra warden ca token` "+
			"prints one, and it is a single string of the form %s.<ca-fingerprint>.<secret>", why, enrolTokenPrefix)
	}
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return bad(fmt.Sprintf("%d dot-separated fields, want 3", len(parts)))
	}
	if parts[0] != enrolTokenPrefix {
		return bad(fmt.Sprintf("prefix %q, want %q", parts[0], enrolTokenPrefix))
	}
	pin, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(pin) != sha256.Size {
		return bad("the CA fingerprint is not 32 base64url bytes")
	}
	if parts[2] == "" {
		return bad("the secret half is empty")
	}
	var t enrolToken
	copy(t.CAPin[:], pin)
	t.Secret = parts[2]
	return t, nil
}

// hashEnrolSecret is how the Controller stores and looks up a token: by the hash of its secret, so
// the file on the Controller cannot be read back into a usable token. Hex rather than raw bytes
// because it is a JSON map key.
func hashEnrolSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// spkiPin is sha256 over a certificate's SubjectPublicKeyInfo — the value a token pins and the value
// a **Warden**'s id is derived from. `RawSubjectPublicKeyInfo` is the DER the key was serialised as,
// so this is stable across re-issuance of the certificate around the same key.
func spkiPin(c *x509.Certificate) [sha256.Size]byte {
	return sha256.Sum256(c.RawSubjectPublicKeyInfo)
}

// --- who a Warden is -----------------------------------------------------------------------------

// wardenIDPrefix marks a **Warden**'s name in logs, in a certificate subject, and in the file the
// Controller keeps its assignment in. Prefixed so a bare hex string in a journal is attributable.
const wardenIDPrefix = "wdn-"

// wardenIDFor derives a **Warden**'s identity FROM ITS OWN PUBLIC KEY.
//
// DERIVED, NOT ALLOCATED, and that removes a whole component. If the id were a number the Controller
// handed out, then authenticating a request would mean looking the caller's certificate up in a
// table — a table that has to be kept, backed up, and kept in step with the certificates it
// describes. Deriving it means the id is a function of the credential itself: whoever presents a
// certificate issued by this CA IS the Warden its key names, provably, with nothing to consult.
//
// Sixteen hex characters is 64 bits of a sha256. Two Machines colliding would need two P-256 keys
// whose SPKI hashes share 64 bits, which is not an accident anyone has; a Controller that wanted to
// defend against a chosen collision would compare the whole certificate, and it does — the id is a
// name, and the authentication is the chain.
func wardenIDFor(pub *x509.Certificate) string {
	pin := spkiPin(pub)
	return wardenIDPrefix + hex.EncodeToString(pin[:])[:16]
}

// wardenRecord is `warden.json`: who this **Machine** is and where it calls home.
//
// IT NAMES NO WORKER, and warden_test.go asserts that after a reconcile has run. See this file's
// header for why the absence is the contract.
type wardenRecord struct {
	WardenID string `json:"wardenId"`
	// Controller is the URL `join` was given, kept so `kontra warden serve` needs no argument
	// beyond its state directory — a systemd unit that had to carry the address as well as the
	// state would be a second place for it to be wrong.
	Controller string `json:"controller"`
	// CAFingerprint is the pin the token carried, recorded so `kontra warden status` can print
	// which Fleet this Machine belongs to without parsing a certificate.
	CAFingerprint string    `json:"caFingerprint"`
	EnrolledAt    time.Time `json:"enrolledAt"`
	// NotAfter is when the certificate stops working. There is NO RENEWAL in this slice and this
	// field is how that gap is visible rather than silent: `kontra warden status` prints the days
	// remaining, so a Fleet that is going to stop enrolling in a month says so a month early.
	NotAfter time.Time `json:"notAfter"`

	// Temporal, Namespace and TemporalTLS are where this **Machine**'s lifecycle is RECORDED — the
	// control plane its blocked watcher runs in (ADR 0037, slice 04; warden_workflow.go).
	//
	// NAMESPACE HERE IS A COPY, AND THE CERTIFICATE IS THE ORIGINAL. Slice 08 moved the authority
	// into the credential: the namespace is a URI SAN the Fleet CA signed, and this field is the same
	// value written out for `kontra warden status` to print without parsing a certificate. It is
	// CHECKED against the certificate on every load (`loadIdentity`) rather than trusted, because
	// this file is 0644 on the Machine's own disk and anything on that Machine can rewrite it — and
	// a namespace read out of a file a compromised Machine can edit is not a boundary at all.
	//
	// THEY ARRIVE AT ENROLMENT AND ARE NOT READ FROM THE ENVIRONMENT, unlike every other Temporal
	// address in this CLI (`config.TemporalAddress`). `kontra serve` and `kontra dispatch` run where an
	// operator exported KONTRA_ADDRESS; a **Warden** runs under a systemd unit on a Machine nobody
	// logs into, and warden.go states the rule this follows: "a unit and an EnvironmentFile that
	// disagree is a Warden that enrolled into one directory and serves from another". The Controller
	// is the one party that knows the address a Machine can reach it on, so it says so once, in the
	// answer to the only question the Machine ever asks it about itself.
	//
	// EMPTY IS A REAL ANSWER. A Machine that enrolled against a Controller with no control plane
	// configured reconciles exactly as it did in slice 03 and says, on every `serve`, that nothing is
	// recording it. That is the airgapped case, and it is a sentence rather than a silence.
	Temporal  string `json:"temporal,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// TemporalTLS says this Machine must offer its own certificate when it dials the control plane —
	// the thing that makes the credential this enrolment minted an actual TEMPORAL credential rather
	// than a note about which namespace to ask for. Off unless the Controller said otherwise, because
	// the single box and the install run a Temporal with no TLS at all.
	TemporalTLS bool `json:"temporalTls,omitempty"`
}

// wardenIdentity is a **Warden**'s credential, loaded and ready to dial with.
type wardenIdentity struct {
	Dir    string
	Record wardenRecord

	cert tls.Certificate
	ca   *x509.CertPool
	// scope is the namespace THIS CREDENTIAL AUTHORISES, read out of the certificate and not out of
	// the record. Every Temporal dial on a Machine takes its namespace from here — see
	// `temporalOptions`, which is the only place that is allowed to.
	scope wardenScope
}

// identity file names. One place, because `join` writes them and `serve` reads them and a drift
// between the two is a Machine that enrols and then cannot start.
const (
	wardenKeyFile    = "key.pem"
	wardenCertFile   = "cert.pem"
	wardenCAFile     = "ca.pem"
	wardenRecordFile = "warden.json"
	wardenIdentityIn = "identity"
)

func wardenIdentityDir(state string) string { return filepath.Join(state, wardenIdentityIn) }

// loadIdentity reads a **Warden**'s credential off disk.
//
// A MISSING IDENTITY IS ITS OWN ERROR, matched with errors.Is by `kontra warden serve`, because "this
// Machine has not enrolled" and "this Machine's key is corrupt" are two different mornings.
//
// ═══ THE NAMESPACE COMES OUT OF THE CERTIFICATE, AND A RECORD THAT DISAGREES IS REFUSED ═══
//
// This is where slice 08's boundary is enforced ON THE MACHINE, and it is worth being precise about
// what it is and is not.
//
// `warden.json` is mode 0644 on the Machine's own disk. Anything running as root there — which
// includes whatever went wrong to make this a question — can set `"namespace": "globex"` in it with
// one `sed`. If the dial took its namespace from that file, a compromised Machine would address
// another tenant's namespace using a certificate the Fleet CA issued, and every check upstream of it
// would pass, because the certificate is genuine and the namespace was never in it.
//
// So the namespace is read from the CERTIFICATE, which is signed and cannot be edited without
// breaking the chain, and the record's copy is compared against it. A mismatch is refused rather than
// silently corrected, because a Machine whose record says another tenant is a Machine somebody has
// been editing, and quietly using the right value would erase the only sign of it.
//
// What this does NOT claim: it is not what stops a truly compromised Machine, because code on that
// Machine can hold the key and dial Temporal itself, ignoring this function entirely. What stops THAT
// is the server: the certificate names one namespace, so a Temporal that maps the credential to its
// claims has one namespace to give it. This check is the honest half kontra can enforce on its own —
// it makes the namespace un-editable in every path kontra takes, so that "the Machine asked for
// another tenant" is never something kontra itself did.
func loadIdentity(state string) (*wardenIdentity, error) {
	dir := wardenIdentityDir(state)
	body, err := os.ReadFile(filepath.Join(state, wardenRecordFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s holds no identity: %w", state, errNotEnrolled)
	}
	if err != nil {
		return nil, err
	}
	var rec wardenRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("%s is not readable as a Warden record: %w", filepath.Join(state, wardenRecordFile), err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, wardenCertFile), filepath.Join(dir, wardenKeyFile))
	if err != nil {
		return nil, fmt.Errorf("%s has a record but no usable key pair (re-run `kontra warden join`): %w", state, err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, wardenCAFile))
	if err != nil {
		return nil, fmt.Errorf("%s has no CA to verify the Controller with (re-run `kontra warden join`): %w", state, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s holds no certificate this can parse", filepath.Join(dir, wardenCAFile))
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("%s holds a key pair whose certificate will not parse: %w", dir, err)
	}
	scope, err := wardenScopeOf(leaf)
	if err != nil {
		return nil, fmt.Errorf("this Machine's credential is not scoped to a tenant: %w", err)
	}
	if scope.WardenID != rec.WardenID {
		return nil, fmt.Errorf("%s says this Machine is %q and its certificate is for %q — the id is "+
			"derived from the key, so those cannot disagree unless one of the two was replaced",
			filepath.Join(state, wardenRecordFile), rec.WardenID, scope.WardenID)
	}
	if rec.Namespace != scope.Namespace {
		return nil, fmt.Errorf("%s says this Machine's namespace is %q and its CERTIFICATE says %q. "+
			"The certificate is the credential and the record is a copy of it, so this Machine will not "+
			"dial anything: a namespace read out of a file on the Machine is not a boundary, and one "+
			"that has been edited to name another tenant is the reason this check exists. Re-run "+
			"`kontra warden join` to get a record that matches, or put %q back",
			filepath.Join(state, wardenRecordFile), rec.Namespace, scope.Namespace, scope.Namespace)
	}
	return &wardenIdentity{Dir: state, Record: rec, cert: cert, ca: pool, scope: scope}, nil
}

// errNotEnrolled is "this Machine has never joined", as opposed to any other reason a credential
// cannot be loaded.
var errNotEnrolled = errors.New("not enrolled")

// client is the http.Client every later outbound call uses: mutual TLS, the Fleet CA as the ONLY
// root, and this Warden's certificate offered on every request.
//
// THE ROOT POOL IS THE FLEET CA ALONE, not the system pool plus it. A Warden talks to exactly one
// Controller and any public CA being able to vouch for it is a downgrade nobody would notice: the
// whole reason the CA fingerprint rode in on the enrolment token was to make the set of acceptable
// Controllers exactly one.
func (i *wardenIdentity) client() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      i.ca,
				Certificates: []tls.Certificate{i.cert},
			},
		},
	}
}

// temporalOptions is the ONLY way a **Machine** dials its control plane, and the only place its
// namespace comes from.
//
// ═══ ONE FUNCTION, SO THERE IS ONE PLACE TO GET IT WRONG ═══
//
// A `client.Options{HostPort: …, Namespace: …}` literal anywhere else on the Machine's side of this
// protocol is a second place the namespace is chosen, and the second place is the one that ends up
// reading a field instead of a certificate. So the namespace is not a parameter: it is
// `i.scope.Namespace`, taken from the signed credential, and this signature has nowhere to put
// another one.
//
// ═══ THE CERTIFICATE IS OFFERED TO TEMPORAL WHEN THE CONTROLLER SAYS IT IS WANTED ═══
//
// That is what makes what enrolment minted a CREDENTIAL rather than a note. ADR 0036: "A Warden's
// enrolment mints credentials scoped to one namespace, so a compromised Machine cannot address
// another tenant's queues at all." The second half of that sentence is the SERVER's to keep — a
// Temporal that maps this client certificate to claims for the namespace in its URI SAN gives a
// Machine exactly one namespace, whatever it asks for. kontra's half is that the Machine has one
// certificate, it names one namespace, and it is offered on every dial.
//
// IT IS OFF BY DEFAULT AND THAT IS SAID RATHER THAN HIDDEN. The single box, the install and every
// test in this repo run a Temporal with no TLS; offering a certificate to a plaintext listener does
// not fail closed, it fails to connect at all. So the Controller declares it once, at enrolment
// (`--temporal-tls`), and a Fleet whose Temporal does not check certificates is a Fleet whose
// tenancy is kontra's arrangement rather than the server's — true, and worth knowing, which is why
// `kontra warden status` prints which of the two this is.
//
// THE ROOT POOL IS THE SYSTEM'S PLUS THE FLEET CA, unlike `client()`. A Controller is exactly one
// server and pinning it to one key is the point; a Temporal frontend is somebody else's server, and
// its certificate is as likely to come from a public CA or the customer's own as from this Fleet's.
func (i *wardenIdentity) temporalOptions() (client.Options, error) {
	opts := client.Options{HostPort: i.Record.Temporal, Namespace: i.scope.Namespace}
	if err := validWardenNamespace(opts.Namespace); err != nil {
		return client.Options{}, fmt.Errorf("this Machine's credential names no namespace to dial: %w", err)
	}
	if !i.Record.TemporalTLS {
		return opts, nil
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if caPEM, err := os.ReadFile(filepath.Join(wardenIdentityDir(i.Dir), wardenCAFile)); err == nil {
		roots.AppendCertsFromPEM(caPEM)
	}
	opts.ConnectionOptions = client.ConnectionOptions{TLS: &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		Certificates: []tls.Certificate{i.cert},
	}}
	return opts, nil
}

// --- enrolling -----------------------------------------------------------------------------------

// enrolRequest and enrolResponse are the two JSON bodies of `POST /warden/enrol`. Small on purpose:
// a **Machine** asserts nothing about itself that matters, because everything it could assert is
// something an attacker with the token could assert too. The hostname is carried for the operator's
// benefit alone and the Controller records it as a claim, never as a fact.
type enrolRequest struct {
	CSR      string `json:"csr"`
	Hostname string `json:"hostname"`
}

type enrolResponse struct {
	WardenID    string `json:"wardenId"`
	Certificate string `json:"certificate"`
	CA          string `json:"ca"`
	// Temporal and Namespace are the control plane this Machine's watcher will run in. See
	// wardenRecord for why they travel here rather than being configured on the Machine.
	//
	// NEITHER OF THESE IS THE CREDENTIAL, AND SLICE 08 DID NOT MAKE THEM ONE. An address and a name
	// authorise nothing; the credential is `Certificate`, which now carries the namespace as a signed
	// URI SAN (warden_tenant.go). This `Namespace` is the same value in a field that is easy to read,
	// and `enrol` refuses the response outright if the two disagree — a Controller that names one
	// tenant and issues a certificate for another is the failure this check exists for, and it would
	// otherwise show up hours later as a Machine dialling a namespace its certificate has no claim on.
	Temporal    string `json:"temporal,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	TemporalTLS bool   `json:"temporalTls,omitempty"`
}

// enrol exchanges a one-time token for an identity that persists.
//
// The order of operations is the security argument and it does not commute:
//
//  1. parse the token, so the pin exists before anything is dialled;
//  2. generate a key ON THIS MACHINE and build a CSR — the private half never enters this function's
//     return value, let alone the network;
//  3. dial with a TLS config that verifies the Controller AGAINST THE PIN and nothing else;
//  4. only now send the secret;
//  5. verify that what came back chains to the pinned CA, because a Controller that authenticated
//     is still not permitted to hand back a certificate from somewhere else;
//  6. write the key with 0600 before writing anything else, so a crash between steps leaves a
//     directory that `loadIdentity` refuses rather than one it half-accepts.
func enrol(ctx context.Context, state, controller, token string) (*wardenIdentity, error) {
	tok, err := parseEnrolToken(token)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimRight(controller, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("--controller %q is not a URL (want https://<controller>:%d)", controller, wardenCADefaultPort)
	}
	if base.Scheme != "https" {
		// PLAIN HTTP IS NOT A DEGRADED ENROLMENT, IT IS A DIFFERENT ONE. Over http the pin has
		// nothing to check, so the single-use secret would be readable by anything on the path and
		// the certificate that came back would be whatever that thing chose to send.
		return nil, fmt.Errorf("--controller %q is not https, and enrolment has no meaning over http: "+
			"the CA fingerprint in the token is what proves the Controller is yours, and there is no "+
			"certificate to check it against on a plaintext connection", controller)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating this Machine's key: %w", err)
	}
	host, _ := os.Hostname()
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		// THE SUBJECT IS A REQUEST, NOT A FACT, and the CA replaces it — see wardenCA.sign. It is
		// filled in with the hostname anyway so that a CSR captured in a log says which Machine
		// made it.
		Subject: pkix.Name{CommonName: host},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("building this Machine's certificate request: %w", err)
	}

	body, _ := json.Marshal(enrolRequest{
		CSR:      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		Hostname: host,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+wardenEnrolPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.Secret)

	host2, _, err := net.SplitHostPort(base.Host)
	if err != nil {
		host2 = base.Host
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: pinnedTLS(tok.CAPin, host2)}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enrolling with %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var msg struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		if msg.Error == "" {
			msg.Error = resp.Status
		}
		return nil, fmt.Errorf("the Controller refused this enrolment (%d): %s", resp.StatusCode, msg.Error)
	}
	var out enrolResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("the Controller's answer is not an enrolment: %w", err)
	}

	// STEP 5. The connection proved the Controller holds the pinned CA's key; it did not prove that
	// what it just sent back was issued by it. A Controller that handed out a certificate from some
	// other CA would produce a Machine that enrolled successfully and could never authenticate
	// again — a failure that shows up hours later, on the reconcile loop, as a TLS error with no
	// enrolment anywhere near it.
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM([]byte(out.CA)) {
		return nil, errors.New("the Controller sent no CA certificate this can parse")
	}
	leaf, err := parseOnePEMCertificate(out.Certificate)
	if err != nil {
		return nil, fmt.Errorf("the Controller sent no certificate this can parse: %w", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: caPool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("the certificate the Controller issued does not chain to the CA it sent: %w", err)
	}
	if got := wardenIDFor(leaf); got != out.WardenID {
		return nil, fmt.Errorf("the Controller named this Warden %q but issued a certificate for %q — "+
			"the id is derived from the key, so those cannot disagree", out.WardenID, got)
	}
	// THE SAME CROSS-CHECK, FOR THE FIELD THAT AUTHORISES. The certificate is the credential and
	// `out.Namespace` is a copy of what is in it; a Controller whose answer disagrees with what it
	// just signed is one whose Machines would dial a namespace their certificate has no claim on, and
	// that fails hours later on the reconcile loop with no enrolment anywhere near it.
	scope, err := wardenScopeOf(leaf)
	if err != nil {
		return nil, fmt.Errorf("the certificate the Controller issued is not scoped to a tenant, so this "+
			"Machine would have no namespace of its own: %w", err)
	}
	if out.Namespace != scope.Namespace {
		return nil, fmt.Errorf("the Controller says this Machine's namespace is %q and the certificate it "+
			"issued says %q — a tenant is a Temporal namespace (ADR 0036) and those cannot disagree",
			out.Namespace, scope.Namespace)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	dir := wardenIdentityDir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create %s to hold this Machine's identity: %w", dir, err)
	}
	// 0600 AND FIRST. The key is the credential; the certificate and the CA are public. Writing the
	// key first means an interrupted enrolment leaves a directory with no record in it, which
	// loadIdentity reports as "not enrolled" — the state `join` can simply be run again from.
	if err := os.WriteFile(filepath.Join(dir, wardenKeyFile),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, wardenCertFile), []byte(out.Certificate), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, wardenCAFile), []byte(out.CA), 0o644); err != nil {
		return nil, err
	}
	rec := wardenRecord{
		WardenID:      out.WardenID,
		Controller:    base.String(),
		CAFingerprint: base64.RawURLEncoding.EncodeToString(tok.CAPin[:]),
		EnrolledAt:    time.Now().UTC(),
		NotAfter:      leaf.NotAfter.UTC(),
		Temporal:      out.Temporal,
		// FROM THE CERTIFICATE, NOT FROM THE RESPONSE. They have just been proved equal, so this
		// chooses which one is the source — and the source is the signed one, so that the record can
		// only ever be a faithful copy of the credential.
		Namespace:   scope.Namespace,
		TemporalTLS: out.TemporalTLS,
	}
	recBody, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(state, wardenRecordFile), append(recBody, '\n'), 0o644); err != nil {
		return nil, err
	}
	return loadIdentity(state)
}

// pinnedTLS is the client config `join` dials with: it trusts NOTHING except a chain whose root is
// the key the token pinned.
//
// `InsecureSkipVerify` WITH `VerifyPeerCertificate` IS THE STRICTER CONFIGURATION HERE, and the flag
// name is why this comment exists. Go's own documentation says the callback is how a caller supplies
// its own verification; leaving the default verification on would require the CA to be in the
// system's trust store, which is the opposite of what a private Fleet CA is. What replaces it is a
// full chain build against a pool containing exactly one key, plus a hostname check — so nothing is
// skipped, one root is substituted for two hundred.
func pinnedTLS(pin [sha256.Size]byte, serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // replaced by VerifyPeerCertificate, below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, 0, len(rawCerts))
			for _, der := range rawCerts {
				c, err := x509.ParseCertificate(der)
				if err != nil {
					return fmt.Errorf("the Controller presented a certificate this cannot parse: %w", err)
				}
				certs = append(certs, c)
			}
			if len(certs) == 0 {
				return errors.New("the Controller presented no certificate at all")
			}
			roots, inter := x509.NewCertPool(), x509.NewCertPool()
			pinned := false
			for _, c := range certs {
				if spkiPin(c) == pin {
					roots.AddCert(c)
					pinned = true
					continue
				}
				inter.AddCert(c)
			}
			if !pinned {
				// THE MESSAGE MUST NOT BE "TLS ERROR". This is the one refusal in enrolment that
				// means somebody is between the Machine and its Controller, and an operator reading
				// a generic handshake failure will assume a certificate expired and try again.
				return fmt.Errorf("the Controller at %s does not hold the CA this enrolment token pins — "+
					"the token names a Fleet CA whose fingerprint is %s and this server's chain contains "+
					"no such key, so the token was NOT sent", serverName,
					base64.RawURLEncoding.EncodeToString(pin[:]))
			}
			_, err := certs[0].Verify(x509.VerifyOptions{
				Roots:         roots,
				Intermediates: inter,
				DNSName:       serverName,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		},
	}
}

// parseOnePEMCertificate reads exactly one CERTIFICATE block. "Exactly one" because a caller that
// accepted the first of several would silently ignore whatever came after it.
func parseOnePEMCertificate(body string) (*x509.Certificate, error) {
	block, rest := pem.Decode([]byte(body))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM CERTIFICATE block")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("more than one PEM block, and only one certificate was expected")
	}
	return x509.ParseCertificate(block.Bytes)
}
