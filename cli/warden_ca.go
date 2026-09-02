package main

// warden_ca.go — the Controller's half of the **Warden** protocol: the Fleet CA, and the two
// endpoints a **Machine** dials.
//
// ═══ WHY THIS IS IN GO AND NOT IN THE ORCHESTRATOR ═══
//
// Every other control-plane surface in this repo is a Fastify route in `control/orchestrator/src`, and the first
// instinct was to add two more there. Three facts moved it here, in this order:
//
//  1. NODE CANNOT ISSUE AN X.509 CERTIFICATE. `node:crypto` parses one (`X509Certificate`) and signs
//     arbitrary bytes, and that is all — there is no issuance API. The backend's `package.json`
//     carries no `node-forge`, no `@peculiar/x509` and no `selfsigned`. The two ways to do it there
//     were a new dependency on a security-critical path, or hand-rolled DER for a TBSCertificate.
//     `crypto/x509.CreateCertificate` is in the standard library on this side.
//
//  2. A CA PRIVATE KEY SHOULD NOT LIVE IN THE ORCHESTRATOR'S PROCESS. That process serves an API
//     which — measured and recorded in `cli/config.go` — is unauthenticated on most of its routes,
//     spawns this CLI as a child with caller-influenced arguments, and executes registered folders.
//     It is the correct home for almost everything and the wrong home for the one key that can mint
//     an identity for any Machine in the Fleet.
//
//  3. ONE LANGUAGE MEANS NO CROSS-LANGUAGE LITERAL TO DRIFT. This repo's recurring failure is a
//     derivation written twice and diverging — `infra/CONTEXT.md` records the Bundle key doing
//     exactly that ("built out of separate arguments in Go and rebuilt out of separate arguments in
//     TypeScript, so no sweep could find it"). The certificate profile, the id derivation and the
//     token grammar are all shared by the two sides of enrolment, and both sides are here.
//
// ═══ WHAT THE ASSIGNMENT ENDPOINT IS, AND WHAT REPLACES IT ═══
//
// `GET /warden/assignment` reads a JSON file the operator wrote. That is not the design; it is the
// smallest thing that makes the reconcile loop real in a slice that must not contain Temporal.
//
// SLICE 03 SAID SLICE 04 WOULD DELETE THIS, AND SLICE 04 DID NOT. The forecast was that ADR 0037's
// blocked workflow would carry the desired state and this route would go with it. What slice 04
// found, counting the events, is that pushing an assignment through workflow history costs 11 events
// per change — a signal, a cancelled watch and a fresh one — against 6 for the Machine reading it
// here on its own next turn and REPORTING that it changed. So what moved into the workflow is the
// record that the assignment changed, and not the assignment; the reasoning and both counts are in
// warden_workflow.go's header. This endpoint is still the smallest thing that works and is still not
// the design — slices 09 and 10 (Leases, `fleet.place()`) are what replace an operator writing a
// JSON file, and neither of them makes history the transport.
//
// It is a file rather than a database, and it is READ ON EVERY REQUEST rather than cached, so that
// the property the loop is being tested for — desired state, not commands; a reboot replays nothing
// — is true of the Controller too. There is no queue here, nothing is delivered, and a **Machine**
// that was off for a week asks the same question and gets the same answer as one that was not.
//
// ═══ THE ENDPOINT AN OPERATOR MUST NOT CONFUSE FOR THE OTHER ═══
//
// Two routes, two entirely different admissions, and they are written next to each other so the
// difference is impossible to miss:
//
//	POST /warden/enrol       Bearer <one-time secret>   — NO client certificate; that is the point
//	GET  /warden/assignment  a client certificate this CA issued — NO bearer token is accepted
//
// A server that accepted either credential on either route would have made the one-time token a
// permanent one. `tls.VerifyClientCertIfGiven` is what lets one listener do both: the handshake
// verifies a certificate if one is offered and does not require one, and each handler then insists
// on exactly the credential it wants.
//
// ═══ WHAT SLICE 08 ADDED: BOTH ROUTES NOW CARRY A TENANT, AND NEITHER TAKES IT FROM THE CALLER ═══
//
//	POST /warden/enrol       the tenant comes from the TOKEN the operator minted (`spendToken`),
//	                         and is written into the certificate as its one URI SAN (`sign`).
//	GET  /warden/assignment  the tenant comes from THAT CERTIFICATE (`wardenScopeOf`), and selects
//	                         one directory under `assignments/`.
//
// So there is no request field, on either route, that a Machine could put a tenant in. That is the
// same rule the subject already obeyed, applied to the thing that actually authorises — see
// warden_tenant.go for why the namespace is the only boundary Temporal has, and for the two places
// this code was relying on a derived NAME where it needed one.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The two paths a **Warden** knows. Constants because both sides of the protocol are in this repo
// and a literal typed twice is the thing this file's header says not to do.
const (
	wardenEnrolPath      = "/warden/enrol"
	wardenAssignmentPath = "/warden/assignment"
)

// wardenCADefaultPort is where the Controller listens for Wardens.
//
// A PORT OF ITS OWN, not 8088. The orchestrator's port carries an API that is unauthenticated on
// most routes; this one carries a listener whose entire purpose is that it authenticates, and
// sharing a socket between the two would make "which admission applies here" a routing question.
const wardenCADefaultPort = 8443

// caFileNames. The CA's own key is the only 0600 file the Controller keeps for this.
const (
	caKeyFile    = "ca-key.pem"
	caCertFile   = "ca.pem"
	caTokensFile = "tokens.json"
	caAssignDir  = "assignments"
)

// wardenCA is the Fleet's certificate authority, and the file-backed token list beside it.
type wardenCA struct {
	Dir  string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	// temporal is the address enrolment hands back as the control plane (slice 04). Set by
	// `ca serve` and empty everywhere else, because minting a token and listing them do not need to
	// know where Temporal is — and a field that were required to open a CA would make the airgapped
	// Controller impossible to start.
	//
	// THERE IS NO `namespace` FIELD ANY MORE, and that is slice 08. A Controller-wide namespace was
	// one namespace for every Machine that ever enrolled here, which is the collision ADR 0036 §6
	// describes: two customers shipping `nscheck@0.1.0` on one task queue. The namespace now comes
	// from the TENANT the enrolment token was minted for, so it is decided once, by an operator, per
	// Machine — see warden_tenant.go.
	temporal string
	// temporalTLS says the control plane requires client certificates, so a **Machine** should offer
	// the identity this CA issued it when it dials Temporal. Off by default: the single box and the
	// appliance run a Temporal with no TLS at all, and a Warden that offered a certificate to a
	// plaintext listener would simply fail to connect.
	temporalTLS bool

	// plane starts the worker that executes a tenant's Machines' watchers, lazily, in that tenant's
	// namespace. Nil in every test and in `ca token` / `ca list`; `ensure` is nil-safe. See
	// wardenPlaneSet for why it cannot be one worker.
	plane *wardenPlaneSet

	// mu guards tokens.json against two concurrent enrolments spending one token, and tenants.json
	// beside it. It is a process-local lock over a single directory, which is exactly as strong as
	// this slice's single listener needs and no stronger; a second Controller process sharing the
	// directory would need the file lock this deliberately does not have. Named here rather than
	// discovered later.
	mu sync.Mutex
}

// caToken is one unspent enrolment token as the Controller holds it: the HASH of the secret, never
// the secret. Keyed by that hash in tokens.json, so the file cannot be read back into a token that
// works.
type caToken struct {
	ExpiresAt time.Time `json:"expiresAt"`
	// Tenant is the namespace every certificate issued against this token is scoped to.
	//
	// IT IS DECIDED AT MINT AND NOWHERE ELSE. The alternative — a Machine naming its own tenant in
	// the enrolment request — is the same class of bug as a CSR naming its own subject (`sign`), and
	// worse: a subject is a name, and this is the authorisation boundary itself. The one moment a
	// human is present and knows which customer a Machine belongs to is when they mint the token to
	// paste onto it, so that is the only moment this is chosen.
	Tenant string `json:"tenant"`
	// SpentAt is set rather than the record being deleted, so `kontra warden ca token --list` can
	// say "that token was used at 14:02" instead of "unknown token" — which is the difference
	// between an operator re-running a paste and an operator hunting an attacker.
	SpentAt  *time.Time `json:"spentAt,omitempty"`
	SpentBy  string     `json:"spentBy,omitempty"`
	IssuedAt time.Time  `json:"issuedAt"`
}

// caCertValidity is how long the Fleet CA itself lives. Ten years because rotating it invalidates
// every Warden in the Fleet at once, and a CA that expires unexpectedly is the outage that takes the
// whole control plane's authentication with it.
const caCertValidity = 10 * 365 * 24 * time.Hour

// wardenCertValidity is how long an enrolled **Warden**'s certificate lives.
//
// ONE YEAR, AND THERE IS NO RENEWAL IN THIS SLICE. That is a gap, not a design: ADR 0037 names the
// Warden's own upgrade path as "a day-one design problem, not a later one" and certificate renewal
// is the same family of problem. What this slice does about it is refuse to hide it — `warden.json`
// records `notAfter` and `kontra warden status` prints the days remaining, so the Fleet says it is
// going to stop working a long time before it does.
const wardenCertValidity = 365 * 24 * time.Hour

// serverCertValidity is the listener's own certificate. Short, because it is re-issued from the CA
// on every `ca serve` and nothing depends on it outliving the process.
const serverCertValidity = 90 * 24 * time.Hour

// openWardenCA loads the Fleet CA, creating it on first use.
//
// CREATE-ON-FIRST-USE rather than a mandatory `ca init`, because the failure mode of the alternative
// is an operator running `ca serve` and getting an error about a command they have not heard of. The
// creation is announced on `out` so it is never silent — a CA appearing is the single most
// consequential thing this file does, and a Fleet whose CA was regenerated by accident is a Fleet
// whose Machines all stop authenticating at once.
func openWardenCA(dir string, out io.Writer) (*wardenCA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create %s to hold the Fleet CA: %w", dir, err)
	}
	keyPath, certPath := filepath.Join(dir, caKeyFile), filepath.Join(dir, caCertFile)
	if fileExists(keyPath) && fileExists(certPath) {
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("the Fleet CA in %s is not loadable: %w", dir, err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, err
		}
		key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("the Fleet CA key in %s is not an ECDSA key", dir)
		}
		return &wardenCA{Dir: dir, cert: leaf, key: key}, nil
	}
	if fileExists(keyPath) != fileExists(certPath) {
		// HALF A CA IS NOT A CA, and regenerating over it would silently orphan every Machine that
		// enrolled against the half that survives.
		return nil, fmt.Errorf("%s holds one of %s/%s and not the other — that is a half-written CA, "+
			"and creating a new one over it would orphan every Machine already enrolled", dir, caKeyFile, caCertFile)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kontra Fleet CA"},
		NotBefore:             time.Now().Add(-time.Hour), // clock skew between a Controller and a fresh Machine is real
		NotAfter:              time.Now().Add(caCertValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// A CA THAT CANNOT ISSUE AN INTERMEDIATE. Nothing in this design needs one, and a path
		// length of zero means a stolen Warden certificate cannot be used to sign anything even if
		// some future code forgot to check `IsCA`.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	ca := &wardenCA{Dir: dir, cert: leaf, key: key}
	if out != nil {
		fmt.Fprintf(out, "created a new Fleet CA in %s (fingerprint %s)\n"+
			"  every Machine enrolled against a PREVIOUS CA in this directory would stop authenticating;\n"+
			"  if that is a surprise, stop and find the CA you meant to use.\n", dir, ca.pinString())
	}
	return ca, nil
}

func (c *wardenCA) pin() [sha256.Size]byte { return spkiPin(c.cert) }

// pinString is the CA's fingerprint in THE SAME SPELLING AN ENROLMENT TOKEN CARRIES — base64url, no
// padding. One representation everywhere, because the whole point of the fingerprint is that an
// operator can compare the one their Controller prints with the one inside a token, and two encodings
// of one value make that comparison fail for a reason nobody would guess.
func (c *wardenCA) pinString() string {
	p := c.pin()
	return base64.RawURLEncoding.EncodeToString(p[:])
}

func (c *wardenCA) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

// randomSerial is a 128-bit serial. Random rather than a counter because a counter is state the CA
// would have to keep consistent, and the only requirement on a serial is that two certificates from
// one issuer do not share it.
func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// --- tokens ---------------------------------------------------------------------------------------

func (c *wardenCA) tokensPath() string { return filepath.Join(c.Dir, caTokensFile) }

func (c *wardenCA) readTokens() (map[string]caToken, error) {
	body, err := os.ReadFile(c.tokensPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]caToken{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]caToken{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", c.tokensPath(), err)
	}
	return m, nil
}

func (c *wardenCA) writeTokens(m map[string]caToken) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.tokensPath(), append(body, '\n'), 0o600)
}

// mintToken creates a one-time enrolment token FOR ONE TENANT, and records only its hash.
//
// The secret is returned and never written. There is therefore no way to recover a token that was
// printed and lost, which is the correct property and is stated in the command's output so nobody
// goes looking for the file that would have it.
//
// THE TENANT IS VALIDATED HERE, WHICH IS THE LAST MOMENT A HUMAN IS PRESENT. A namespace this Fleet
// cannot carry — one that will not fit a certificate's URI or one directory name — must be refused
// at the command an operator is typing, not on a Machine nobody logs into, hours later, as a Warden
// that enrolled and then could not load its own identity.
func (c *wardenCA) mintToken(ttl time.Duration, tenant string) (enrolToken, error) {
	if err := validWardenNamespace(tenant); err != nil {
		return enrolToken{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, err := newEnrolToken(c.pin())
	if err != nil {
		return enrolToken{}, err
	}
	m, err := c.readTokens()
	if err != nil {
		return enrolToken{}, err
	}
	// THE TENANT IS RECORDED BEFORE THE TOKEN IS. `tokens.json` is swept and `tenants.json` is not,
	// so a Controller that wrote the token first and then failed would hold a spendable token for a
	// tenant it does not know it serves — and would not execute that tenant's watchers on the next
	// restart. See caTenantsFile.
	if err := c.noteTenantLocked(tenant); err != nil {
		return enrolToken{}, err
	}
	now := time.Now().UTC()
	// Spent and expired records are swept here rather than by a timer: this is the only writer, it
	// runs rarely, and a sweep that needs its own schedule is a second thing that can fail.
	for h, t := range m {
		if t.SpentAt != nil && now.Sub(*t.SpentAt) > 30*24*time.Hour {
			delete(m, h)
		} else if t.SpentAt == nil && now.After(t.ExpiresAt.Add(30*24*time.Hour)) {
			delete(m, h)
		}
	}
	m[hashEnrolSecret(tok.Secret)] = caToken{IssuedAt: now, ExpiresAt: now.Add(ttl), Tenant: tenant}
	if err := c.writeTokens(m); err != nil {
		return enrolToken{}, err
	}
	return tok, nil
}

// spendToken is the admission for `POST /warden/enrol`: single use, and it is recorded as spent
// BEFORE a certificate is issued. It returns THE TENANT the token was minted for, which is the only
// thing that decides which namespace the certificate about to be signed will authorise.
//
// THE ORDER IS THE WHOLE GUARANTEE. Marking it spent after signing would leave a window in which two
// concurrent requests both pass the check and both get a certificate, and "single use" would be true
// only of requests that did not race. The cost is that a token is burnt if signing then fails, which
// is the right way round: minting another token is one command, and a token that can be spent twice
// is not a one-time token.
//
// A TOKEN WITH NO TENANT IS REFUSED RATHER THAN DEFAULTED. `tokens.json` is a file on disk and a
// token minted by the previous version of this binary has no `tenant` key; reading that as "the
// default namespace" would put a stranger's Machine into whichever tenant happens to be called
// `default`, which is the whole failure this slice removes. So it is an error, and the operator
// mints a new token — one command.
func (c *wardenCA) spendToken(secret string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.readTokens()
	if err != nil {
		return "", err
	}
	h := hashEnrolSecret(secret)
	t, ok := m[h]
	if !ok {
		return "", errors.New("unknown enrolment token")
	}
	if t.SpentAt != nil {
		return "", fmt.Errorf("that enrolment token was already spent at %s", t.SpentAt.Format(time.RFC3339))
	}
	if time.Now().After(t.ExpiresAt) {
		return "", fmt.Errorf("that enrolment token expired at %s", t.ExpiresAt.Format(time.RFC3339))
	}
	if err := validWardenNamespace(t.Tenant); err != nil {
		return "", fmt.Errorf("that enrolment token names no usable tenant, so there is no namespace to "+
			"scope a certificate to — mint a fresh one with `kontra warden ca token --tenant <name>`: %w", err)
	}
	now := time.Now().UTC()
	t.SpentAt = &now
	m[h] = t
	return t.Tenant, c.writeTokens(m)
}

// noteTokenSpender records WHICH Warden spent a token, after the fact. Best-effort: the enrolment
// has already succeeded by the time this is called and failing it here would tell the Machine its
// identity did not persist when it did.
func (c *wardenCA) noteTokenSpender(secret, wardenID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.readTokens()
	if err != nil {
		return
	}
	h := hashEnrolSecret(secret)
	if t, ok := m[h]; ok {
		t.SpentBy = wardenID
		m[h] = t
		_ = c.writeTokens(m)
	}
}

// --- signing ---------------------------------------------------------------------------------------

// sign turns a CSR into a **Warden**'s client certificate, SCOPED TO ONE NAMESPACE.
//
// THE CSR'S SUBJECT IS DISCARDED, AND SO IS EVERYTHING ELSE IN IT EXCEPT THE KEY. A CSR is an
// assertion by whoever built it, and the only part of it that is a fact is the public key plus the
// signature proving the sender holds the private half. So the subject this CA writes is derived from
// that key (`wardenIDFor`), which means a Machine cannot name itself, cannot claim another Machine's
// name, and cannot claim a name with meaning to some later authorisation check — the failure that
// makes "the client sent its own CN" a classic.
//
// THE SAME RULE NOW COVERS THE THING THAT ACTUALLY AUTHORISES. `tmpl.URIs` is built here from the
// `namespace` argument — which came from the TOKEN (`spendToken`), which came from the operator who
// minted it — and `csr.URIs` is never read. A Machine that put `kontra:///ns/globex/warden/…` in its
// own request gets a certificate for the tenant its token names, because the request's SANs are as
// much an assertion as its subject was. `TestTheCAIgnoresTheTenantAMachineAsksFor` is that claim.
//
// The signature on the CSR IS checked (`CheckSignature`), because without it the CSR is just a
// public key somebody typed and this would issue certificates for keys the requester does not hold.
func (c *wardenCA) sign(csrPEM, namespace string) (*x509.Certificate, error) {
	if err := validWardenNamespace(namespace); err != nil {
		return nil, fmt.Errorf("this CA will not issue an unscoped certificate: %w", err)
	}
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("no PEM CERTIFICATE REQUEST block in the enrolment")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unparseable certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the certificate request is not signed by the key it carries: %w", err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("a Warden's key must be ECDSA P-256")
	}
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("a Warden's key must be ECDSA P-256, got %s", pub.Curve.Params().Name)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	// The id is derived from the SPKI, and the SPKI is not available until the certificate exists —
	// so it is computed from a throwaway parse of the marshalled key rather than from the
	// certificate this is about to make. Same bytes either way: `MarshalPKIXPublicKey` produces the
	// DER that becomes `RawSubjectPublicKeyInfo`.
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(spki)
	id := wardenIDPrefix + fmt.Sprintf("%x", sum[:])[:16]

	scope, err := wardenScope{Namespace: namespace, WardenID: id}.uri()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: id, Organization: []string{"kontra Warden"}},
		// THE TENANT, AS THE ONE THING IN THIS CERTIFICATE THAT AUTHORISES ANYTHING. Exactly one URI,
		// never taken from the request — see this function's header and warden_tenant.go.
		URIs:      []*url.URL{scope},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(wardenCertValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		// CLIENT AUTH AND NOTHING ELSE. A Warden dials out and is never dialled (ADR 0037: "Every
		// arrow is dialled by the Machine"), so a certificate that also carried ServerAuth would be
		// a certificate that could stand up a TLS server the rest of the Fleet trusts.
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pub, c.key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// serverCertificate issues the listener's own certificate from the same CA, so that the fingerprint
// in an enrolment token verifies the very server the token is pasted at.
//
// The SANs are the addresses a **Machine** will actually type. They are an argument rather than a
// discovery because a Controller behind a name kontra does not know about — which is every real
// deployment — would otherwise serve a certificate for its private IP and refuse every enrolment
// that used the name.
func (c *wardenCA) serverCertificate(sans []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kontra Controller"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(serverCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, s)
	}
	if len(tmpl.DNSNames) == 0 && len(tmpl.IPAddresses) == 0 {
		return tls.Certificate{}, errors.New("a Controller certificate with no name or address in it " +
			"would be refused by every Warden that dialled it")
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}, nil
}

// localSANs is what `ca serve` puts in its certificate when the operator names none: this host's
// name, loopback, and every non-loopback address it has.
//
// A LIST, NOT A GUESS. A Controller is reached by its VPC address from a Machine and by `localhost`
// from whoever is administering it, and a certificate carrying only one of those makes the other
// path fail with a name-mismatch that reads like an attack.
func localSANs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			add(ipnet.IP.String())
		}
	}
	return out
}

// --- the assignment ---------------------------------------------------------------------------------

// wardenAssignment is the desired state one **Machine** is asked to hold.
//
// DESIRED STATE, NOT COMMANDS, and every field here is chosen to keep it that way. There is no
// "start" and no "stop": the list is what should be running, so a **Worker** disappearing from it is
// how it is stopped and a **Warden** that was off for a week converges on the current answer rather
// than replaying a week of instructions. ADR 0037 states the same shape from the caller's side:
// "`place()` is idempotent desired state, so it is also the scale verb."
type wardenAssignment struct {
	// Generation exists for the operator and for slice 04's Transcript, not for the loop: the
	// Warden reconciles what it was told regardless, because a generation the Machine compared
	// against would be a memory of a previous answer, and a memory is the thing this design does
	// without.
	Generation int              `json:"generation"`
	Workers    []assignedWorker `json:"workers"`

	// Egress is where this Machine's Workers may reach, and it is the one field here that is NOT about
	// a Worker. It belongs to the assignment rather than to `assignedWorker` because the thing it
	// governs is the Machine's network namespace: ADR 0037 says packed "Workers may share a Machine,
	// and they share its egress address", so a per-Worker egress field would be several answers to a
	// question with one. See warden_egress.go — the policy is installed in the HOST's nftables, and the
	// floor it carves holes in is not this file's to switch off.
	Egress egressPolicy `json:"egress,omitempty"`
}

// assignedWorker is one **Worker** as the control plane asks for it.
//
// IT NAMES AN ARTIFACT AND AN ENVIRONMENT. `machine.ts` states the principle this follows — "The
// route picks WHICH stack; it never picks WHAT the stack contains" — and the shape here is the same:
// the Controller names a digest-pinned image and the variables the halves need, and the image's own
// entrypoint is what runs.
//
// ARGV IS OPTIONAL AND GRANTS NOTHING THE IMAGE DOES NOT. Under `podman` an empty argv means the
// image's declared entrypoint runs (driver_podman.go), which is the normal case; a Controller that
// can name a digest can already choose what executes, so carrying an argv adds no authority there.
// Under `process` it is different in kind and worth knowing: there is no container, so an argv is a
// command on the host — which is why `process` is the single-box and airgap driver (ADR 0036), where
// the Controller and the operator are the same party.
type assignedWorker struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Image is `<repo>@sha256:<64 hex>`. Refused by `podman` if it is a tag; unread by `process`.
	Image string `json:"image,omitempty"`
	// Env is what BOTH halves get, plus each half's own additions. A map rather than a list because
	// a Controller writing this by hand should not be able to set one variable twice.
	Env     map[string]string `json:"env,omitempty"`
	Actor   assignedHalf      `json:"actor,omitempty"`
	Handler assignedHalf      `json:"handler,omitempty"`
}

type assignedHalf struct {
	Argv []string          `json:"argv,omitempty"`
	Dir  string            `json:"dir,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
}

// spec turns one assigned Worker into the driver's own shape, in the **Machine's** own namespace.
//
// The environment is materialised as `KEY=VALUE` in SORTED order. Sorted because Go's map iteration
// is deliberately random, and an unsorted environment would make two identical assignments produce
// two different `podman run` argvs — which is invisible until something compares them.
func (a assignedWorker) spec(namespace string) workerSpec {
	half := func(h assignedHalf) procSpec {
		return procSpec{Dir: h.Dir, Argv: h.Argv, Env: envList(a.Env, h.Env, a.derivedEnv(namespace))}
	}
	return workerSpec{
		Name:    a.Name,
		Version: a.Version,
		Image:   a.Image,
		Actor:   half(a.Actor),
		Handler: half(a.Handler),
	}
}

// derivedEnv is the part of a **Worker**'s environment that is NOT the assignment's to write.
//
// ═══ THIS WAS A COMMENT WITHOUT A FUNCTION, AND SLICE 08 FOUND IT ═══
//
// `envList`'s header said, in slice 03 and slice 04: "KONTRA_ACTOR_NAME AND KONTRA_ACTOR_VERSION ARE
// ADDED HERE and not left to the assignment … Derived from the Worker's own identity, they cannot do
// either." The function merged two maps and added nothing. So both failures it claimed to prevent
// were reachable from a JSON file the whole time — an assignment that OMITS them starts a pair that
// polls nothing (`runtime/handler/main.go:31-32` reads exactly those two to pick its queue), and one that
// CONTRADICTS them starts a Worker whose `KONTRA_WORKER` label says one thing and whose queue says
// another, which the reconcile loop then cannot see and restarts every five seconds.
//
// ═══ AND KONTRA_NAMESPACE IS THE ONE THIS SLICE HAD TO ADD ═══
//
// A **Warden** being scoped to one namespace is worth nothing if the **Workers** it starts are not:
// they are the processes that actually poll. `runtime/handler/main.go:65`, `runtime/go/temporalhost:230` and
// `runtime/python/internals/temporal/host.py:340` all read `KONTRA_NAMESPACE` and all default it to
// `"default"` — so without this line, every tenant's Workers poll `nscheck-0.1.0` in ONE namespace
// and ADR 0036 §6's collision survives the whole slice, with the Warden's own watcher correctly
// separated above it.
//
// IT COMES FROM THE MACHINE'S CERTIFICATE and OVERRIDES the assignment (it is applied last in
// `envList`), for the same reason the actor name does: it is a derived fact, not a choice. An
// assignment is a file an operator writes, and the one thing a file must not be able to say is which
// tenant the Workers on this Machine belong to.
func (a assignedWorker) derivedEnv(namespace string) map[string]string {
	out := map[string]string{
		"KONTRA_ACTOR_NAME":    a.Name,
		"KONTRA_ACTOR_VERSION": a.Version,
	}
	// EMPTY IS NOT WRITTEN, because `KONTRA_NAMESPACE=` is not the same as it being unset: the actor
	// hosts read it with a default, and an empty value would reach Temporal verbatim. The only caller
	// that can produce empty is a test building a `warden` with no credential; `warden.assignment`
	// takes it from a scope that `wardenScopeOf` has already refused to leave blank.
	if namespace != "" {
		out["KONTRA_NAMESPACE"] = namespace
	}
	return out
}

// envList merges the Worker's environment, one half's, and the DERIVED variables, and renders it.
//
// THE ORDER IS THE RULE: the assignment's shared map, then the half's own (so a per-half PYTHONPATH
// can override a shared one), then `derivedEnv` — which therefore wins over both and cannot be
// overridden by anything anybody writes in a JSON file. See `derivedEnv` for which three variables
// those are and why each one is not the assignment's to set.
func envList(shared, own, derived map[string]string) []string {
	merged := map[string]string{}
	for _, m := range []map[string]string{shared, own, derived} {
		for k, v := range m {
			merged[k] = v
		}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// readAssignment answers what one **Warden** should be running: its own file if it has one, else its
// TENANT's default.
//
// ═══ THE DEFAULT IS PER TENANT, AND THAT IS THE COLLISION THIS SLICE CLOSES ═══
//
// It used to be `assignments/default.json` — ONE file, read by every Machine that had ever enrolled
// against this Controller. The moment a second party enrols, that file is a cross-tenant read in the
// plainest possible form: tenant A's Machine is handed the image digest, the environment and the
// argv that tenant B wrote. So the layout is `assignments/<namespace>/`, the namespace comes from the
// CALLER'S CERTIFICATE, and there is no path from one tenant's directory to another's — the namespace
// grammar (warden_tenant.go) has no `/` and cannot begin with a dot, so it is exactly one component.
//
// A LEGACY TOP-LEVEL `default.json` IS AN ERROR, NOT A FILE TO IGNORE. Ignoring it would answer every
// Machine on an upgraded Controller with an EMPTY assignment, and an empty assignment is a valid
// desired state that stops every Worker in the Fleet — silently, because the Controller answered 200.
// An error is strictly safer: warden.go holds its last assignment across a failed fetch, so nothing
// is torn down, and the operator reads the sentence in both logs.
//
// THE FALLBACK IS THE WHOLE POINT OF THE DEFAULT FILE. A Fleet is capacity (ADR 0037) and its
// Machines are interchangeable, so "every Machine of this tenant runs this" is the common case and
// writing one file per Machine to say it would be a directory that drifts. A per-Warden file
// overrides it, which is what `spread=` and packing (slices 10 and 11) will need.
//
// NO FILE AT ALL IS AN EMPTY ASSIGNMENT, and that is a real answer rather than an error: a Machine
// that has enrolled and been given nothing to run should hold nothing, and a Warden that treated
// "no file" as a failure would keep whatever it was running last — which is exactly the drift a
// desired-state loop exists to remove.
func (c *wardenCA) readAssignment(scope wardenScope) (wardenAssignment, error) {
	root := filepath.Join(c.Dir, caAssignDir)
	if fileExists(filepath.Join(root, "default.json")) {
		return wardenAssignment{}, fmt.Errorf("%s is a Fleet-wide assignment from before namespace-per-tenant "+
			"(ADR 0036) and every Machine that read it belonged to some tenant. Move it to %s and this "+
			"Controller will serve it to that tenant's Machines only; until then nothing here answers, "+
			"and every Warden holds the assignment it already has",
			filepath.Join(root, "default.json"), filepath.Join(root, scope.Namespace, "default.json"))
	}
	dir := filepath.Join(root, scope.Namespace)
	for _, name := range []string{scope.WardenID + ".json", "default.json"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return wardenAssignment{}, err
		}
		var a wardenAssignment
		if err := json.Unmarshal(body, &a); err != nil {
			return wardenAssignment{}, fmt.Errorf("%s is not a readable assignment: %w", filepath.Join(dir, name), err)
		}
		return a, nil
	}
	return wardenAssignment{}, nil
}

// --- the listener -------------------------------------------------------------------------------

// handler is the Controller's two routes. Returned as an http.Handler rather than bound to a server
// so that the tests drive it through `httptest.NewUnstartedServer` with the real TLS configuration —
// the admission rules below are only true if the handshake is the real one.
func (c *wardenCA) handler(out io.Writer) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc(wardenEnrolPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			httpJSONError(w, http.StatusMethodNotAllowed, "enrolment is a POST")
			return
		}
		// A CLIENT CERTIFICATE ON THIS ROUTE IS IGNORED, DELIBERATELY. Enrolment is how a Machine
		// gets one; accepting one here as an alternative credential would let an enrolled Machine
		// mint identities for others without ever holding a token.
		secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if secret == "" || secret == r.Header.Get("Authorization") {
			httpJSONError(w, http.StatusUnauthorized, "enrolment needs `Authorization: Bearer <the token's secret half>`")
			return
		}
		var req enrolRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			httpJSONError(w, http.StatusBadRequest, "unreadable enrolment body")
			return
		}
		// THE TENANT COMES BACK OUT OF THE TOKEN, and it is the only input to the namespace this
		// certificate will authorise. Nothing in `req` is consulted for it — see `sign`.
		tenant, err := c.spendToken(secret)
		if err != nil {
			// ONE MESSAGE FOR EVERY REASON A TOKEN DOES NOT WORK, on the wire. The operator's copy
			// of the reason goes to the Controller's own output, where the person who can act on it
			// is; telling the caller whether a token is unknown, spent or expired is telling an
			// attacker which guesses were close.
			if out != nil {
				fmt.Fprintf(out, "enrolment refused from %s: %v\n", r.RemoteAddr, err)
			}
			httpJSONError(w, http.StatusUnauthorized, "that enrolment token is not usable")
			return
		}
		leaf, err := c.sign(req.CSR, tenant)
		if err != nil {
			httpJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		id := wardenIDFor(leaf)
		c.noteTokenSpender(secret, id)
		if out != nil {
			fmt.Fprintf(out, "enrolled %s into tenant %s (hostname %q, from %s)\n",
				id, tenant, req.Hostname, r.RemoteAddr)
		}
		// A TENANT NOBODY IS WATCHING IS A FLEET WITH NO HISTORY, so the plane worker for this
		// namespace is started here if it is not already running. A tenant minted after `ca serve`
		// started is the ordinary case — an operator onboards a customer without restarting the
		// Controller — and a Controller that only read the roster at boot would record nothing for
		// them while answering every request successfully.
		c.plane.ensure(tenant)
		writeJSON(w, http.StatusOK, enrolResponse{
			WardenID:    id,
			Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})),
			CA:          string(c.certPEM()),
			// WHERE THIS MACHINE'S LIFECYCLE WILL BE RECORDED. Handed over once, here, because this
			// is the only moment the Controller and the Machine are in contact about anything other
			// than an assignment — see wardenRecord for why a Machine must not be configured with it
			// separately.
			//
			// THE NAMESPACE HERE IS A CONVENIENCE, NOT THE AUTHORITY. It is the same value the
			// certificate carries, and `enrol` refuses the pair if they disagree: the credential is
			// the certificate, and a JSON field beside it is a copy that must be checked rather than
			// trusted.
			Temporal:    c.temporal,
			Namespace:   tenant,
			TemporalTLS: c.temporalTLS,
		})
	})

	mux.HandleFunc(wardenAssignmentPath, func(w http.ResponseWriter, r *http.Request) {
		// THE CALLER IS ITS CERTIFICATE — BOTH WHO IT IS AND WHICH TENANT IT BELONGS TO. There is no
		// id in the URL, no tenant in a header and no field anywhere a Warden could put another's
		// name in: the id is derived from the key it just proved it holds, and the namespace is a SAN
		// the Fleet CA signed. A Machine that wants another tenant's assignment has to forge a
		// certificate, which is the same problem as forging any other.
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			httpJSONError(w, http.StatusUnauthorized,
				"this route needs a client certificate issued by this Fleet's CA (`kontra warden join` obtains one)")
			return
		}
		scope, err := wardenScopeOf(r.TLS.VerifiedChains[0][0])
		if err != nil {
			// AN UNSCOPED CERTIFICATE IS REFUSED HERE TOO, not only on the Machine. A Warden enrolled
			// before this slice holds a certificate this CA issued and would otherwise fall through to
			// some default tenant's directory — which is precisely the cross-tenant read the layout
			// exists to prevent, arriving through a Machine that did nothing wrong.
			if out != nil {
				fmt.Fprintf(out, "assignment refused from %s: %v\n", r.RemoteAddr, err)
			}
			httpJSONError(w, http.StatusForbidden, "that certificate is not scoped to a tenant; re-enrol this Machine")
			return
		}
		a, err := c.readAssignment(scope)
		if err != nil {
			httpJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, a)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func httpJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// tlsConfig is the listener's. `VerifyClientCertIfGiven` is what lets ONE socket carry both routes:
// enrolment arrives without a certificate and must be allowed to, while the assignment route insists
// on one — and "if given, it must verify" means a client that offers a certificate signed by
// something else is refused at the handshake rather than inside a handler.
func (c *wardenCA) tlsConfig(sans []string) (*tls.Config, error) {
	srv, err := c.serverCertificate(sans)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{srv},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
	}, nil
}
