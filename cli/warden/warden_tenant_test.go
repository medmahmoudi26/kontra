// warden_tenant_test.go — namespace per tenant, and the refusals that make it a boundary.
//
// ═══ WHAT IS BEING TESTED, AND WHAT DELIBERATELY IS NOT ═══
//
// A test that shows tenant A reaching ITS OWN namespace proves nothing: a Controller that ignored
// tenancy entirely passes it. So every claim below is a REFUSAL, each one has a control that proves
// the same call works when it should, and each one fails if its check is removed. The four are:
//
//	the record          a Machine that rewrote `json` to name another tenant will not load
//	the assignment      tenant A's certificate cannot reach tenant B's directory, by any path
//	the certificate     a CSR asserting a tenant gets a certificate for the tenant its TOKEN names
//	the credential      what a Machine offers Temporal is the scoped certificate, or nothing
//
// AND THE ONE THIS FILE CANNOT MAKE: that a Temporal SERVER refuses tenant A's certificate when it
// asks for namespace B. That is the server's authorizer and claim mapper, configured by whoever runs
// it, and it is not in this repo — there is no Temporal here that checks client certificates at all.
// `TestAMachineOffersItsScopedCredentialToTemporal` goes as far as this checkout can: it proves the
// certificate the Machine presents on the wire is the namespace-scoped one, so a server that DOES map
// it has one namespace to map it to. The gap is named rather than papered over.
//
// ═══ THE FIXTURE'S TENANTS ═══
//
// `acme` and `globex`, and both are named explicitly. A test that used `default` for one of them
// would pass against every fallback in the code, which is the class of bug this slice exists to
// remove.
package warden

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/config"
	"github.com/medmahmoudi26/kontra/cli/internal/corpus"
	"github.com/medmahmoudi26/kontra/cli/internal/queues"
)

const (
	tenantA = "acme"
	tenantB = "globex"
)

// --- the credential's shape -------------------------------------------------------------------------

// THE NAMESPACE SURVIVES A REAL CERTIFICATE, INCLUDING THE CASES THAT WOULD BREAK IT. A scope is
// built here, marshalled into an X.509 SAN by `crypto/x509`, parsed back out by the same package and
// read by `wardenScopeOf` — so what is pinned is the round trip through DER, not a string this file
// formatted twice.
//
// THE ADVERSARIAL INPUT IS THE CASE. Temporal namespaces are case-SENSITIVE, so `Acme` and `acme` are
// two tenants; a carrier that case-folded (a URI authority does, in several parsers) would silently
// hand one tenant the other's namespace. `wardenScope.uri` puts everything in the PATH for exactly
// this reason and this is the test that holds it there.
func TestAWardenScopeSurvivesACertificateVerbatim(t *testing.T) {
	ca, err := openWardenCA(t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	namespaces := []string{
		"acme",                  // the ordinary one
		"Acme",                  // …and its case-folded twin, which must stay a different tenant
		"acme-prod",             // a hyphen, the common real spelling
		"a.b.c",                 // dots, which a hostname parser would treat as labels
		"a_b",                   // an underscore, which is illegal in a DNS name and fine here
		"0starts-with-a-digit",  //
		strings.Repeat("n", 63), // the longest name the grammar allows
	}
	if len(namespaces) == 0 {
		t.Fatal("no namespaces to check, so this test asserts nothing")
	}
	seen := map[string]bool{}
	for _, ns := range namespaces {
		leaf := signFor(t, ca, ns)
		got, err := wardenScopeOf(leaf)
		if err != nil {
			t.Fatalf("a certificate this CA just issued for %q has no readable scope: %v", ns, err)
		}
		if got.Namespace != ns {
			t.Errorf("the namespace came back as %q, want %q — something on the path from a scope to a "+
				"certificate and back is normalising it", got.Namespace, ns)
		}
		if got.WardenID != leaf.Subject.CommonName {
			t.Errorf("the scope names %q and the subject names %q", got.WardenID, leaf.Subject.CommonName)
		}
		if len(leaf.URIs) != 1 {
			t.Errorf("the certificate for %q carries %d URIs; a credential scoped to two tenants is "+
				"scoped to neither", ns, len(leaf.URIs))
		}
		seen[got.Namespace] = true
	}
	// THE CASE-FOLD, ASSERTED AS A CONSEQUENCE RATHER THAN AS A STRING COMPARISON: `acme` and `Acme`
	// must have produced TWO entries. One entry means something lowercased on the way through.
	if !seen["acme"] || !seen["Acme"] {
		t.Errorf("`acme` and `Acme` did not survive as two namespaces: %v", sortedIDs(seen))
	}
}

// A NAME THIS FLEET CANNOT CARRY IS REFUSED AT THE COMMAND A HUMAN IS TYPING. The grammar is narrower
// than the one an ACTOR is named by on purpose — `shared/conformance/queues.json` carries `a/b`, `my actor`
// and `café` as actor names that must keep working, and every one of them is a tenant name that would
// either escape a URI or become two path components under `assignments/`.
func TestATenantNameThisFleetCannotCarryIsRefusedAtMint(t *testing.T) {
	c := newController(t)
	// CONTROL: the grammar accepts the ordinary case, so the refusals below are about the input.
	if _, err := c.ca.mintToken(time.Hour, tenantA); err != nil {
		t.Fatalf("a plain tenant name was refused, so nothing below is about the grammar: %v", err)
	}
	bad := []string{
		"",                      // no tenant at all
		"-leads-with-a-hyphen",  //
		".leads-with-a-dot",     // and therefore `.` and `..` are unspellable, which is what keeps
		"..",                    // `assignments/<tenant>` from being a traversal
		"a/b",                   // a legal ACTOR name (queues.json) and two path components here
		"my actor",              // a legal ACTOR name and not a legal URI path segment
		"café",                  // a legal ACTOR name; non-ASCII would be percent-encoded on the wire
		strings.Repeat("n", 64), // one past the limit
		"acme\nglobex",          // a newline, which would make one tenant read as two anywhere it is logged
	}
	if len(bad) == 0 {
		t.Fatal("no adversarial names, so this test asserts nothing")
	}
	for _, ns := range bad {
		if _, err := c.ca.mintToken(time.Hour, ns); err == nil {
			t.Errorf("a token was minted for tenant %q, which cannot be carried in a certificate or "+
				"used as one directory name", ns)
		}
	}
	// …and none of them reached the roster. A refusal that still recorded the tenant would leave the
	// Controller trying to start a plane worker for a namespace Temporal will not accept.
	tenants, err := c.ca.readTenants()
	if err != nil {
		t.Fatal(err)
	}
	if len(tenants) != 1 || tenants[tenantA].CreatedAt.IsZero() {
		t.Errorf("the roster is %v; only the control's tenant should be on it", sortedKeysOf(tenants))
	}
}

// --- THE NEGATIVE TEST: a Machine cannot address another tenant's namespace ---------------------------

// TENANT A'S MACHINE, TOLD TO USE TENANT B'S NAMESPACE, IS REFUSED — AND IT IS REFUSED BY ITS OWN
// CREDENTIAL, NOT BY A POLICY SOMEWHERE ELSE.
//
// This is the test the slice is for. `json` is mode 0644 on the Machine's own disk, so
// anything running as root there can put another tenant's name in it with one `sed`. If the dial took
// its namespace from that file, a compromised Machine would address another tenant's namespace
// holding a certificate the Fleet CA genuinely issued, and every check upstream would pass.
//
// THE CONTROLS, because "it failed" and "it refuses everything" are the same result:
//
//	the untampered directory loads, and its scope is A          → the loader works
//	tenant B's OWN Machine loads, with B's namespace            → `globex` is a real, loadable tenant
//	the tampered directory is refused, naming BOTH namespaces   → the claim
//	`sed`-ing it back makes it load again                       → the refusal was about the value
//
// REMOVE THE `rec.Namespace != scope.Namespace` CHECK IN `loadIdentity` AND THIS TEST GOES RED: the
// tampered directory loads, `temporalOptions` is asked for a namespace, and the sub-test below finds
// `globex` on a credential issued for `acme`.
func TestATenantsCredentialIsRefusedAnotherTenantsNamespace(t *testing.T) {
	c := newController(t)
	c.ca.temporal = "10.124.0.2:7233"

	stateA, stateB := t.TempDir(), t.TempDir()
	a, err := enrol(context.Background(), stateA, c.srv.URL, c.tokenFor(t, tenantA))
	if err != nil {
		t.Fatalf("enrolling tenant %s: %v", tenantA, err)
	}
	b, err := enrol(context.Background(), stateB, c.srv.URL, c.tokenFor(t, tenantB))
	if err != nil {
		t.Fatalf("enrolling tenant %s: %v", tenantB, err)
	}

	// CONTROL 1: A loads, and everything it will ever dial with says `acme`.
	if a.scope.Namespace != tenantA {
		t.Fatalf("tenant %s's Machine is scoped to %q", tenantA, a.scope.Namespace)
	}
	optsA, err := a.temporalOptions()
	if err != nil {
		t.Fatalf("tenant %s cannot build dial options at all, so nothing below is about tenancy: %v", tenantA, err)
	}
	if optsA.Namespace != tenantA {
		t.Fatalf("tenant %s would dial namespace %q", tenantA, optsA.Namespace)
	}
	// CONTROL 2: `globex` is a real tenant that really does load — so the refusal below is about A
	// holding B's name, not about B being unusable.
	if b.scope.Namespace != tenantB {
		t.Fatalf("tenant %s's Machine is scoped to %q", tenantB, b.scope.Namespace)
	}

	// ═══ THE CLAIM ═══ A's directory, with B's namespace written into the record it keeps for itself.
	rewriteRecordNamespace(t, stateA, tenantB)
	_, err = loadIdentity(stateA)
	if err == nil {
		t.Fatal("a Machine holding tenant acme's certificate loaded an identity claiming tenant globex — " +
			"every Temporal dial it makes would now name a namespace its credential has no claim on")
	}
	// THE MESSAGE MUST NAME BOTH, because an operator reading "invalid identity" will re-run `join`
	// and destroy the evidence that somebody edited the file.
	for _, want := range []string{tenantA, tenantB, "CERTIFICATE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not say what happened: %v", want, err)
		}
	}

	// …AND THERE IS NO OTHER PATH. `loadIdentity` is the only way `kontra warden serve` gets an
	// identity, so a refusal there is a Machine that never dials — but a future caller that built a
	// `wardenIdentity` by hand would go through `temporalOptions`, which takes its namespace from the
	// certificate and has nowhere to put another one.
	tampered := &wardenIdentity{Dir: stateA, Record: wardenRecord{Temporal: "10.124.0.2:7233", Namespace: tenantB},
		cert: a.cert, ca: a.ca, scope: a.scope}
	opts, err := tampered.temporalOptions()
	if err != nil {
		t.Fatalf("building dial options failed for a reason other than tenancy: %v", err)
	}
	if opts.Namespace != tenantA {
		t.Errorf("a record claiming %q produced a dial to namespace %q; the namespace must come from "+
			"the certificate and nowhere else", tenantB, opts.Namespace)
	}

	// CONTROL 4: put it back, and it loads. The refusal was about the value, not about the file
	// having been touched.
	rewriteRecordNamespace(t, stateA, tenantA)
	again, err := loadIdentity(stateA)
	if err != nil {
		t.Fatalf("the restored directory still will not load, so the refusal above was about "+
			"something else: %v", err)
	}
	if again.scope.Namespace != tenantA {
		t.Errorf("the restored Machine is scoped to %q", again.scope.Namespace)
	}
}

// --- the Controller's side: one tenant cannot read another's assignment ---------------------------------

// TENANT A CANNOT READ TENANT B'S ASSIGNMENT, AND THERE ARE TWO WAYS IT USED TO BE ABLE TO.
//
// The first is the Fleet-wide `assignments/default.json` slice 03 shipped: ONE file, served to every
// Machine that had ever enrolled against this Controller. That is a cross-tenant read in the plainest
// form — tenant A's Machine handed the image digest, the environment and the argv tenant B wrote.
//
// The second is subtler and is the reason a directory rather than a filename prefix: a per-Warden
// file is named after a Warden ID, and a Warden ID IS NOT A SECRET. The Controller prints it on every
// enrolment and `kontra warden ca list` prints it again. So a file tenant B writes, named after
// tenant A's Machine, must not reach that Machine — which it cannot, because the directory is chosen
// from the CALLER'S CERTIFICATE before the filename is looked at.
//
// REMOVE `scope.Namespace` FROM THE PATH IN `readAssignment` AND BOTH HALVES GO RED.
func TestAWardenCannotReadAnotherTenantsAssignment(t *testing.T) {
	c := newController(t)
	a, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.tokenFor(t, tenantA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.tokenFor(t, tenantB))
	if err != nil {
		t.Fatal(err)
	}
	if a.Record.WardenID == b.Record.WardenID {
		t.Fatalf("two enrolments produced one id (%s), so nothing here is separated", a.Record.WardenID)
	}

	// Tenant B writes both kinds of file, and the per-Warden one is named after TENANT A'S MACHINE —
	// which is what an operator does by mistake and what somebody who read a log would do on purpose.
	c.assign(t, tenantB, "default", wardenAssignment{Workers: []assignedWorker{{Name: "b-fleetwide", Version: "1"}}})
	c.assign(t, tenantB, a.Record.WardenID, wardenAssignment{Workers: []assignedWorker{{Name: "b-aimed-at-a", Version: "1"}}})

	// ═══ THE CLAIM ═══ tenant A's Machine sees NEITHER, and gets the empty answer a Machine with
	// nothing assigned is supposed to get.
	wa := &warden{id: a, http: a.client()}
	got, err := wa.assignment(context.Background())
	if err != nil {
		t.Fatalf("tenant %s's Machine could not read its assignment at all: %v", tenantA, err)
	}
	if len(got) != 0 {
		t.Fatalf("tenant %s's Machine was served tenant %s's assignment: %v", tenantA, tenantB, got)
	}

	// CONTROL: tenant B's own Machine DOES get the tenant-wide file, so the empty answer above is the
	// boundary rather than a route that answers nothing.
	wb := &warden{id: b, http: b.client()}
	got, err = wb.assignment(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "b-fleetwide" {
		t.Fatalf("tenant %s's own Machine did not get its own tenant-wide assignment: %v %v", tenantB, got, err)
	}

	// CONTROL: and when tenant A is given something, it arrives — so the boundary is not a Controller
	// that has stopped serving A.
	c.assign(t, tenantA, "default", wardenAssignment{Workers: []assignedWorker{{Name: "a-fleetwide", Version: "1"}}})
	got, err = wa.assignment(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "a-fleetwide" {
		t.Fatalf("tenant %s's Machine did not get its OWN tenant-wide assignment: %v %v", tenantA, got, err)
	}
}

// A FLEET-WIDE `assignments/default.json` FROM BEFORE THIS SLICE IS AN ERROR, NOT A FILE TO IGNORE.
//
// Ignoring it is the tempting choice and it is the dangerous one: every Machine on an upgraded
// Controller would be answered with an EMPTY assignment, which is a valid desired state that stops
// every Worker in the Fleet — silently, because the Controller answered 200. An error is strictly
// safer, because go holds its last assignment across a failed fetch, so nothing is torn down
// and the sentence appears in both logs.
func TestALegacyFleetWideAssignmentIsRefusedRatherThanIgnored(t *testing.T) {
	c := newController(t)
	id, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.tokenFor(t, tenantA))
	if err != nil {
		t.Fatal(err)
	}
	w := &warden{id: id, http: id.client()}

	// CONTROL: with the per-tenant layout, this Machine reads its assignment.
	c.assign(t, tenantA, "default", wardenAssignment{Workers: []assignedWorker{{Name: "mine", Version: "1"}}})
	if got, err := w.assignment(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("the control read failed, so the refusal below proves nothing: %v %v", got, err)
	}

	// The old layout, alongside.
	legacy := filepath.Join(c.ca.Dir, caAssignDir, "default.json")
	body, _ := json.Marshal(wardenAssignment{Workers: []assignedWorker{{Name: "everybodys", Version: "1"}}})
	if err := os.WriteFile(legacy, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := w.assignment(context.Background())
	if err == nil {
		t.Fatalf("a Fleet-wide assignment from before namespace-per-tenant was served (or silently "+
			"ignored, which stops every Worker): %v", got)
	}
	if !strings.Contains(err.Error(), "default.json") || !strings.Contains(err.Error(), tenantA) {
		t.Errorf("the error does not say which file to move or where to, so nobody can act on it: %v", err)
	}
}

// --- the CA decides the tenant, and the Machine does not ------------------------------------------------

// A MACHINE CANNOT ASK FOR A TENANT, EVEN BY PUTTING ONE IN ITS CERTIFICATE REQUEST.
//
// `TestTheCAIgnoresTheNameAMachineAsksFor` already pins that the CSR's SUBJECT is discarded. This is
// the same rule applied to the field that actually authorises: `sign` builds `tmpl.URIs` from the
// tenant the TOKEN named and never reads `csr.URIs`. Copying the request's SANs through is a one-line
// change somebody could make while "preserving what the client sent", and it would hand any Machine
// any tenant it liked.
func TestTheCAIgnoresTheTenantAMachineAsksFor(t *testing.T) {
	ca, err := openWardenCA(t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	asked := &url.URL{Scheme: wardenScopeScheme, Path: "/ns/" + tenantB + "/warden/wdn-0000000000000000"}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "wdn-0000000000000000"},
		URIs:    []*url.URL{asked},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL ON THE FIXTURE: the request really does carry the tenant it is asking for, or this test
	// is about a CSR with no SANs in it.
	back, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.URIs) != 1 || back.URIs[0].String() != asked.String() {
		t.Fatalf("the fixture's CSR does not carry the tenant it claims to ask for: %v", back.URIs)
	}

	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	leaf, err := ca.sign(csr, tenantA)
	if err != nil {
		t.Fatalf("signing a well-formed request failed: %v", err)
	}
	scope, err := wardenScopeOf(leaf)
	if err != nil {
		t.Fatalf("the issued certificate has no readable scope: %v", err)
	}
	if scope.Namespace != tenantA {
		t.Errorf("the CA issued a certificate for tenant %q — the Machine asked for it and got it",
			scope.Namespace)
	}
	if len(leaf.URIs) != 1 {
		t.Errorf("the issued certificate carries %d URIs (%v); the request's own SAN survived alongside "+
			"the one the CA wrote, and a credential scoped to two tenants is scoped to neither",
			len(leaf.URIs), leaf.URIs)
	}
}

// AN UNSCOPED CERTIFICATE IS REFUSED ON BOTH SIDES, and this is the upgrade path rather than a
// hypothetical: every Warden enrolled by slice 03 holds a genuine certificate from this CA with no
// namespace in it. Treating that as "the default tenant" would put a stranger's Machine into whichever
// namespace happens to be called `default` — the collision this slice removes, arriving through a
// fallback nobody chose. So it is refused where the Machine loads it and again where the Controller
// reads it, because the two are different processes and either one alone leaves the other open.
func TestAnUnscopedCredentialIsRefusedOnBothSides(t *testing.T) {
	c := newController(t)
	state := t.TempDir()
	key, leaf := signUnscoped(t, c.ca)
	// THE RECORD SAYS `default`, WHICH IS THE FALLBACK A LENIENT `wardenScopeOf` WOULD PRODUCE. If the
	// record named some other tenant, this test would pass on the record-vs-certificate mismatch even
	// with the unscoped refusal removed — a guard proving a different guard. With `default` in it, the
	// ONLY thing that can refuse this directory is the certificate having no namespace in it.
	writeIdentity(t, state, c.ca, key, leaf, config.DefaultNamespace)

	// The Machine's side.
	if _, err := loadIdentity(state); err == nil {
		t.Error("a Machine loaded an identity whose certificate names no tenant")
	} else if !strings.Contains(err.Error(), "names no namespace") {
		t.Errorf("the refusal does not say the credential is unscoped: %v", err)
	}

	// The Controller's side, over a real handshake — the certificate IS valid and IS issued by this
	// CA, so the TLS layer admits it and the handler is what refuses.
	cli := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      certPoolOf(c.ca),
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}},
	}}}
	resp, err := cli.Get(c.srv.URL + wardenAssignmentPath)
	if err != nil {
		t.Fatalf("the unscoped certificate could not even complete a handshake, so the status below "+
			"would not be the handler's: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("the assignment route answered %s to a certificate with no tenant in it", resp.Status)
	}

	// CONTROL: a properly enrolled Machine on the same Controller and the same route gets 200, so the
	// 403 is about the missing scope and not about the route.
	ok, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.tokenFor(t, tenantA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&warden{id: ok, http: ok.client()}).assignment(context.Background()); err != nil {
		t.Fatalf("a scoped Machine was refused too, so the route is simply broken: %v", err)
	}
}

// A CONTROLLER THAT NAMES ONE TENANT AND SIGNS ANOTHER IS REFUSED AT ENROLMENT.
//
// WRITTEN BECAUSE A MUTATION SURVIVED. `enrol` cross-checks the response's `namespace` against the
// certificate it was handed, and deleting that check left every test in this file green — because in
// the honest case the two agree, and a check that only ever sees agreement is a check nothing
// exercises. The disagreement has to be manufactured, so it is: a Controller that answers
// `"namespace":"globex"` while issuing a certificate scoped to `acme`.
//
// IT IS NOT A HYPOTHETICAL, it is the shape of every version-skew bug across this boundary — ADR 0037
// calls the Warden "a protocol across an organisational boundary" and names skew as a permanent cost.
// A Controller one version behind, or a proxy rewriting a field, produces exactly this: a Machine that
// enrols successfully, records a namespace it has no claim on, and fails HOURS LATER on the reconcile
// loop with no enrolment anywhere near it. The same argument, and the same remedy, as the Warden-id
// cross-check three lines above it in `enrol`.
//
// WHAT THIS TEST GUARDS IS THE PROPERTY, NOT ONE LINE, and that was measured rather than assumed:
// removing ONLY `enrol`'s cross-check leaves this green, because `enrol` finishes by calling
// `loadIdentity`, which cross-checks the same two values and refuses. Removing BOTH turns it red. So
// the property has two enforcers on two sides of the same function, which is defence in depth and is
// worth knowing about rather than discovering by deleting the wrong one.
func TestAControllerThatNamesOneTenantAndSignsAnotherIsRefused(t *testing.T) {
	honest := newController(t)

	// A Controller that is this one in every respect except the field it lies about.
	var lie bool
	liar := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		honest.ca.handler(io.Discard).ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if lie && r.URL.Path == wardenEnrolPath && rec.Code == http.StatusOK {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err == nil {
				m["namespace"] = tenantB
				body, _ = json.Marshal(m)
			}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", "")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}))
	cfg, err := honest.ca.tlsConfig([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	liar.TLS = cfg
	liar.StartTLS()
	defer liar.Close()

	// CONTROL: with the rewrite OFF, this proxy enrols a Machine perfectly — so what is refused below
	// is the disagreement and not the proxy.
	ok, err := enrol(context.Background(), t.TempDir(), liar.URL, honest.tokenFor(t, tenantA))
	if err != nil {
		t.Fatalf("the honest proxy could not enrol anyone, so nothing here tests the cross-check: %v", err)
	}
	if ok.scope.Namespace != tenantA {
		t.Fatalf("the control enrolment landed in namespace %q", ok.scope.Namespace)
	}

	// ═══ THE CLAIM ═══
	lie = true
	bad, err := enrol(context.Background(), t.TempDir(), liar.URL, honest.tokenFor(t, tenantA))
	if err == nil {
		t.Fatalf("a Machine enrolled with a record saying namespace %q against a certificate scoped to "+
			"%q — every dial it makes would name a namespace it has no claim on",
			bad.Record.Namespace, bad.scope.Namespace)
	}
	for _, want := range []string{tenantA, tenantB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so it does not say which two disagreed: %v", want, err)
		}
	}
}

// --- what the Machine offers Temporal ------------------------------------------------------------------

// THE CREDENTIAL A MACHINE PRESENTS TO ITS CONTROL PLANE IS THE NAMESPACE-SCOPED CERTIFICATE.
//
// Everything above is kontra refusing to ADDRESS another namespace. This is the other half: what
// crosses the wire, so that a Temporal configured to map a client certificate to claims has one
// namespace to map this Machine to. The listener is a real TLS server asking for a client certificate;
// the dial is the real `wardenAttach`, and it FAILS afterwards because nothing behind that socket
// speaks gRPC — which is fine, because the assertion is about what the server saw during the
// handshake, before any of that.
//
// THE CONTROL IS THE SAME MACHINE WITH `--temporal-tls` OFF: it must present NOTHING. Without it,
// "the server saw a certificate" could be a certificate the TLS stack sends on its own.
//
// ═══ THE FAKE CONTROL PLANE'S CERTIFICATE COMES FROM THE FLEET CA, AND IT HAS TO ═══
//
// Written first with a throwaway self-signed one, and it saw NO client certificate — which is not a
// bug in the code under test, it is TLS 1.3: the client verifies the server BEFORE it sends its own
// Certificate message, so a server the Machine distrusts never gets to see what the Machine would
// have offered. `temporalOptions` trusts the system pool plus the Fleet CA, so the listener is issued
// from the Fleet CA and the handshake gets far enough to prove the thing this test is about. Worth
// recording, because "the server saw nothing" and "the client sent nothing" are the same observation
// from here and only one of them is a finding.
func TestAMachineOffersItsScopedCredentialToTemporal(t *testing.T) {
	c := newController(t)
	srvCert, err := c.ca.serverCertificate([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan []*x509.Certificate, 4)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{srvCert},
		ClientAuth:   tls.RequestClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				tc, ok := conn.(*tls.Conn)
				if !ok {
					seen <- nil
					return
				}
				_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
				if err := tc.Handshake(); err != nil {
					seen <- nil
					return
				}
				seen <- tc.ConnectionState().PeerCertificates
			}()
		}
	}()

	c.ca.temporal, c.ca.temporalTLS = ln.Addr().String(), true
	state := t.TempDir()
	id, err := enrol(context.Background(), state, c.srv.URL, c.tokenFor(t, tenantA))
	if err != nil {
		t.Fatal(err)
	}
	if !id.Record.TemporalTLS {
		t.Fatal("the Controller's --temporal-tls did not reach the Machine's record, so nothing below " +
			"is about what it does")
	}

	// THE CLAIM. The dial is expected to fail; what matters is what the listener saw.
	attachAndDiscard(t, id)
	certs := waitForHandshake(t, seen)
	if len(certs) == 0 {
		t.Fatal("this Machine dialled its control plane and offered no client certificate at all, so " +
			"nothing on the wire says which tenant it is")
	}
	scope, err := wardenScopeOf(certs[0])
	if err != nil {
		t.Fatalf("what the Machine presented is not a scoped credential: %v", err)
	}
	if scope.Namespace != tenantA || scope.WardenID != id.Record.WardenID {
		t.Errorf("the Machine presented a credential for %s, and it is %s in namespace %s",
			scope, id.Record.WardenID, tenantA)
	}

	// CONTROL: the same Machine with the flag off presents nothing — so the certificate above arrived
	// because `temporalOptions` put it there.
	id.Record.TemporalTLS = false
	attachAndDiscard(t, id)
	if certs := waitForHandshake(t, seen); len(certs) != 0 {
		t.Errorf("a Machine whose enrolment did not ask for mTLS still presented %d certificate(s)",
			len(certs))
	}
}

// --- the Controller watches every tenant, not just one ---------------------------------------------------

// EVERY TENANT GETS ITS OWN PLANE WORKER, AND A TENANT ONBOARDED AFTER `ca serve` STARTED GETS ONE TOO.
//
// `wardenPlaneQueue` is `kontra-wardens` IN EVERY NAMESPACE, and a Temporal client is bound to one
// namespace at dial time — so slice 04's single worker reaches exactly one tenant. A Controller that
// kept it would answer every enrolment successfully, execute one tenant's watchers, and leave every
// other tenant's Machines arming watches nobody takes. In the Transcript that reads as a Fleet where
// nothing has happened, which is the failure shape this repo has now found in five surfaces and the
// reason this is a test rather than a comment.
//
// THE SEAM IS `wardenPlaneStart`, so what is asserted is WHICH namespaces acquired a worker — a list
// of strings — rather than a Temporal connection nothing here can make.
func TestEveryTenantGetsItsOwnPlaneWorker(t *testing.T) {
	c := newController(t)

	started := []string{}
	restore := wardenPlaneStart
	t.Cleanup(func() { wardenPlaneStart = restore })
	wardenPlaneStart = func(_ context.Context, _, namespace string) (func(), error) {
		started = append(started, namespace)
		return func() {}, nil
	}

	// `acme` exists before the Controller comes up: a token was minted for it earlier.
	if _, err := c.ca.mintToken(time.Hour, tenantA); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := servePlane(ctx, c.ca, "10.124.0.2:7233", false); err != nil {
		t.Fatalf("starting the plane: %v", err)
	}
	c.ca.temporal = "10.124.0.2:7233"
	if got := c.ca.plane.namespaces(); len(got) != 1 || got[0] != tenantA {
		t.Fatalf("a Controller that came up with %s on its roster is watching %v", tenantA, got)
	}

	// `globex` is onboarded WHILE the Controller is running — the ordinary case, and the one a
	// roster read at boot alone would miss.
	if _, err := enrol(ctx, t.TempDir(), c.srv.URL, c.tokenFor(t, tenantB)); err != nil {
		t.Fatal(err)
	}
	if got := c.ca.plane.namespaces(); len(got) != 2 || got[0] != tenantA || got[1] != tenantB {
		t.Errorf("after a %s Machine enrolled, this Controller is watching %v — a tenant nobody is "+
			"watching is indistinguishable from a tenant to whom nothing has happened", tenantB, got)
	}

	// A SECOND MACHINE OF ONE TENANT ADDS NOTHING. `ensure` is idempotent by namespace, or a hundred
	// Machines would be a hundred Temporal connections polling one queue.
	if _, err := enrol(ctx, t.TempDir(), c.srv.URL, c.tokenFor(t, tenantB)); err != nil {
		t.Fatal(err)
	}
	if got := c.ca.plane.namespaces(); len(got) != 2 {
		t.Errorf("a second Machine of tenant %s produced %v", tenantB, got)
	}
	if len(started) != 2 {
		t.Errorf("wardenPlaneStart was called %d times (%v); once per TENANT is the claim", len(started), started)
	}

	// --no-plane and an empty --temporal are choices, not failures: neither leaves a plane behind.
	for _, tc := range []struct {
		why      string
		addr     string
		disabled bool
	}{{"--no-plane", "10.124.0.2:7233", true}, {"no --temporal", "", false}} {
		bare := newController(t)
		if _, err := bare.ca.mintToken(time.Hour, tenantA); err != nil {
			t.Fatal(err)
		}
		if err := servePlane(ctx, bare.ca, tc.addr, tc.disabled); err != nil {
			t.Errorf("%s reported a failure: %v", tc.why, err)
		}
		if got := bare.ca.plane.namespaces(); len(got) != 0 {
			t.Errorf("%s still started workers for %v", tc.why, got)
		}
	}
}

// --- the collision the ADR describes, and what separates the two tenants now -----------------------------

// TWO TENANTS RUNNING THE SAME ACTOR DO NOT TOUCH EACH OTHER — and the queue name is IDENTICAL, which
// is the whole point.
//
// ADR 0036 §6: "Two customers who both ship an actor called `nscheck` at `0.1.0` do not merely see
// each other: they land on ONE task queue, and one tenant's Batches are executed by the other's
// Workers." This test asserts that collision is still there, deliberately — the queue derivation is
// driven from `shared/conformance/queues.json`, which slice 08 did not change and must not, because queue
// names were always namespace-relative — and then asserts that the two Machines are nonetheless
// separated, by the only thing that can separate them.
//
// If a later change "fixes" the collision by putting a tenant into a queue name, this test goes red
// on the FIRST assertion, which is where that conversation should happen.
func TestTwoTenantsRunTheSameActorWithoutTouchingEachOther(t *testing.T) {
	const (
		actor   = "nscheck"
		version = "0.1.0"
	)
	// The corpus, not a literal: `corpus.LoadQueues` refuses one that shrank and one that lost its
	// adversarial inputs.
	corpus := corpus.LoadQueues(t)
	if len(corpus.Shared.Cases) == 0 {
		t.Fatal("the shared-queue corpus is empty, so the collision below is not being derived from anything")
	}

	c := newController(t)
	c.ca.temporal = "10.124.0.2:7233"
	a, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.tokenFor(t, tenantA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.tokenFor(t, tenantB))
	if err != nil {
		t.Fatal(err)
	}

	// ONE. The queue name is the same for both, and it is the corpus's answer.
	qa, qb := queues.Shared(actor, version), queues.Shared(actor, version)
	if qa != qb {
		t.Fatalf("the queue derivation is not a function of (name, version) any more: %q vs %q", qa, qb)
	}
	if qa != actor+"-"+version {
		t.Fatalf("queues.Shared(%q,%q) = %q; shared/conformance/queues.json says %q-%q", actor, version, qa, actor, version)
	}
	// …and the corpus still carries a row for exactly this shape, so the literal above is checked
	// against the contract rather than against itself.
	found := false
	for _, cs := range corpus.Shared.Cases {
		if cs.Expect == queues.Shared(cs.Name, cs.Version) && strings.Contains(cs.Expect, "-") {
			found = true
		}
	}
	if !found {
		t.Fatal("the corpus no longer contains a `<name>-<version>` row, so this test is comparing a " +
			"literal with itself")
	}

	// TWO. The two Machines are in two namespaces, which is the only boundary Temporal has.
	if a.scope.Namespace == b.scope.Namespace {
		t.Fatalf("both tenants' Machines are in namespace %q — they share every queue in it, including %q",
			a.scope.Namespace, qa)
	}
	oa, err := a.temporalOptions()
	if err != nil {
		t.Fatal(err)
	}
	ob, err := b.temporalOptions()
	if err != nil {
		t.Fatal(err)
	}
	if oa.Namespace != tenantA || ob.Namespace != tenantB {
		t.Fatalf("the two Machines dial namespaces %q and %q", oa.Namespace, ob.Namespace)
	}

	// THREE. Each one is told to run the same Actor, by its own tenant, and reads only its own.
	same := wardenAssignment{Workers: []assignedWorker{{
		Name: actor, Version: version, Image: "reg.example/" + actor + "@sha256:" + strings.Repeat("a", 64),
		Env: map[string]string{"WHOSE": tenantA},
	}}}
	c.assign(t, tenantA, "default", same)
	same.Workers[0].Env = map[string]string{"WHOSE": tenantB}
	c.assign(t, tenantB, "default", same)

	for _, tc := range []struct {
		id     *wardenIdentity
		tenant string
	}{{a, tenantA}, {b, tenantB}} {
		w := &warden{id: tc.id, http: tc.id.client()}
		got, err := w.assignment(context.Background())
		if err != nil {
			t.Fatalf("tenant %s could not read its assignment: %v", tc.tenant, err)
		}
		if len(got) != 1 || got[0].Name != actor {
			t.Fatalf("tenant %s was assigned %v", tc.tenant, got)
		}
		// The two Workers have the same NAME, the same VERSION and therefore the same QUEUE — and the
		// environment says which tenant's file it came from.
		want := "WHOSE=" + tc.tenant
		if !containsEnv(got[0].Actor.Env, want) {
			t.Errorf("tenant %s's Worker was built from the other tenant's assignment: %v",
				tc.tenant, got[0].Actor.Env)
		}
	}
}

// --- the Workers a Warden starts are scoped too -----------------------------------------------------

// THE WORKERS ARE WHAT POLL, SO THE NAMESPACE HAS TO REACH THEM.
//
// A **Warden** whose own watcher is correctly in tenant A's namespace, starting **Workers** that poll
// in `default`, has separated the ledger and not the work. `runtime/handler/main.go:65`,
// `runtime/go/temporalhost/host.go:230` and `runtime/python/internals/temporal/host.py:340` each read
// `KONTRA_NAMESPACE` and each default it to `"default"` — so an assignment that does not set it puts
// every tenant's Workers on one queue in one namespace, which is ADR 0036 §6's collision surviving
// the whole slice.
//
// AND THE ASSIGNMENT MAY NOT OVERRIDE IT. An assignment is a file an operator writes; the one thing
// it must not be able to say is which tenant the Workers on this Machine belong to. The test writes
// the other tenant's name into every layer of the environment it can reach and asserts the
// certificate wins.
//
// TWO VARIABLES HERE ARE A PRE-EXISTING BUG, FOUND BY WRITING THIS. `envList`'s header has said since
// slice 03 that `KONTRA_ACTOR_NAME` and `KONTRA_ACTOR_VERSION` "ARE ADDED HERE and not left to the
// assignment … Derived from the Worker's own identity, they cannot do either" — and the function
// merged two maps and added nothing. Both failures it claimed to prevent were reachable from a JSON
// file: an assignment that OMITS them starts a pair that polls nothing, and one that CONTRADICTS them
// starts a Worker whose label and whose queue disagree, which the reconcile loop cannot see and
// restarts every five seconds forever. They are derived now, so the comment is true.
func TestTheWorkersAWardenStartsCarryItsOwnNamespace(t *testing.T) {
	c := newController(t)
	id, err := enrol(context.Background(), t.TempDir(), c.srv.URL, c.tokenFor(t, tenantA))
	if err != nil {
		t.Fatal(err)
	}

	// An assignment that tries, in all three places it could, to be somebody else.
	c.assign(t, tenantA, "default", wardenAssignment{Workers: []assignedWorker{{
		Name: "nscheck", Version: "0.1.0",
		Env: map[string]string{
			"KONTRA_NAMESPACE":     tenantB,
			"KONTRA_ACTOR_NAME":    "not-nscheck",
			"KONTRA_ACTOR_VERSION": "9.9.9",
			"HARMLESS":             "kept",
		},
		Actor:   assignedHalf{Env: map[string]string{"KONTRA_NAMESPACE": tenantB}},
		Handler: assignedHalf{Env: map[string]string{"KONTRA_NAMESPACE": tenantB}},
	}}})

	got, err := (&warden{id: id, http: id.client()}).assignment(context.Background())
	if err != nil {
		t.Fatalf("reading the assignment: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the assignment came back as %v", got)
	}
	// BOTH HALVES, because they are two processes and only one of them is the handler that picks the
	// queue — a fix that reached one of them would be half a boundary.
	for _, half := range []struct {
		what string
		env  []string
	}{{"actor", got[0].Actor.Env}, {"handler", got[0].Handler.Env}} {
		if len(half.env) == 0 {
			t.Fatalf("the %s half has no environment at all, so the assertions below are vacuous", half.what)
		}
		for _, want := range []string{
			"KONTRA_NAMESPACE=" + tenantA,
			"KONTRA_ACTOR_NAME=nscheck",
			"KONTRA_ACTOR_VERSION=0.1.0",
		} {
			if !containsEnv(half.env, want) {
				t.Errorf("the %s half is missing %q — it got %v", half.what, want, half.env)
			}
		}
		if containsEnv(half.env, "KONTRA_NAMESPACE="+tenantB) {
			t.Errorf("the assignment put tenant %s's namespace on the %s half: %v", tenantB, half.what, half.env)
		}
		// CONTROL: an ordinary variable the assignment DOES own still arrives, so what happened above
		// is an override of three names rather than the environment being discarded.
		if !containsEnv(half.env, "HARMLESS=kept") {
			t.Errorf("the %s half lost the assignment's own variables: %v", half.what, half.env)
		}
	}

	// AN ASSIGNMENT THAT SAYS NOTHING GETS THEM ANYWAY — the omission case, which is the other half of
	// the comment that was not true.
	bare := assignedWorker{Name: "subfinder", Version: "0.2.0"}.spec(tenantA)
	for _, want := range []string{"KONTRA_NAMESPACE=" + tenantA, "KONTRA_ACTOR_NAME=subfinder", "KONTRA_ACTOR_VERSION=0.2.0"} {
		if !containsEnv(bare.Actor.Env, want) {
			t.Errorf("an assignment naming no environment produced %v, missing %q", bare.Actor.Env, want)
		}
	}
}

// --- helpers ---------------------------------------------------------------------------------------

// signFor issues a certificate for one namespace through the real signer, from a fresh key.
func signFor(t *testing.T, ca *wardenCA, ns string) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.sign(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), ns)
	if err != nil {
		t.Fatalf("signing for namespace %q: %v", ns, err)
	}
	return leaf
}

// signUnscoped is slice 03's signer: the same CA, the same subject, and NO namespace. Written out
// rather than reached for through a flag, because the thing being reproduced is a certificate this
// code can no longer issue.
func signUnscoped(t *testing.T, ca *wardenCA) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, err := randomSerial()
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	id := wardenIDPrefix + hex.EncodeToString(sum[:])[:16]
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: id, Organization: []string{"kontra Warden"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(wardenCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.URIs) != 0 {
		t.Fatalf("the fixture's `unscoped` certificate carries %v, so it is not unscoped", leaf.URIs)
	}
	return key, leaf
}

// writeIdentity lays out a state directory by hand — what `enrol` would have written.
func writeIdentity(t *testing.T, state string, ca *wardenCA, key *ecdsa.PrivateKey, leaf *x509.Certificate, ns string) {
	t.Helper()
	dir := wardenIdentityDir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), body, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(wardenKeyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	write(wardenCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0o644)
	write(wardenCAFile, ca.certPEM(), 0o644)
	rec, _ := json.MarshalIndent(wardenRecord{
		WardenID: leaf.Subject.CommonName, Controller: "https://127.0.0.1:8443",
		EnrolledAt: time.Now().UTC(), NotAfter: leaf.NotAfter, Namespace: ns,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(state, wardenRecordFile), rec, 0o644); err != nil {
		t.Fatal(err)
	}
}

// rewriteRecordNamespace is the `sed` a root process on a Machine would run.
func rewriteRecordNamespace(t *testing.T, state, ns string) {
	t.Helper()
	path := filepath.Join(state, wardenRecordFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec wardenRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Namespace = ns
	out, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	// CONTROL ON THE HELPER: the file really does say what this test thinks it says. A rewrite that
	// silently did nothing would make the refusal it is supposed to provoke unobservable.
	again, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(again), `"namespace": "`+ns+`"`) {
		t.Fatalf("the record was not rewritten to namespace %q: %s", ns, again)
	}
}

func certPoolOf(ca *wardenCA) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// attachAndDiscard runs the REAL `wardenAttach` against the fake listener and throws away the error
// it is certain to produce. Bounded, because nothing behind that socket speaks gRPC and the SDK would
// otherwise spend its own default timeout finding out.
func attachAndDiscard(t *testing.T, id *wardenIdentity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plane, err := wardenAttach(ctx, id, "process", newWardenWatchpoint(id.Record.WardenID, nil), nil)
	if err == nil {
		plane.close()
		t.Fatal("a Temporal client handshake succeeded against a socket that speaks no gRPC")
	}
}

func waitForHandshake(t *testing.T, seen chan []*x509.Certificate) []*x509.Certificate {
	t.Helper()
	select {
	case certs := <-seen:
		return certs
	case <-time.After(10 * time.Second):
		t.Fatal("the fake control plane was never dialled at all, so nothing here observed a credential")
		return nil
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
