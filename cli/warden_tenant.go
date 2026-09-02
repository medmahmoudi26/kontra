package main

// warden_tenant.go — a tenant is a Temporal namespace, and a **Warden**'s certificate says which one.
//
// ADR 0036: "A tenant is a Temporal namespace. Queue names stay exactly as `shared/conformance/queues.json`
// pins them, because they were always namespace-relative … A **Warden**'s enrolment mints credentials
// scoped to one namespace, so a compromised **Machine** cannot address another tenant's queues at
// all."
//
// ═══ THE ONE FACT THIS FILE EXISTS FOR ═══
//
// IN TEMPORAL, THE NAMESPACE IS THE ONLY AUTHORISATION BOUNDARY THERE IS. Not the task queue, not
// the workflow id, not a prefix convention. A credential that can poll one queue in a namespace can
// poll every queue in it, read every workflow's history in it, and signal every workflow in it by id.
// 0036 §7 states it and this slice is where the code stops relying on anything weaker.
//
// Two places in slice 03/04 were relying on something weaker, and both are named here rather than
// left to be found:
//
//   - `wardenMachineQueue(wardenID)` is `warden-wdn-<16 hex>`, derived from the Machine's own key. In
//     ONE namespace that is unguessability, not authorisation: anything holding a client for that
//     namespace may poll that queue and take a watch meant for another tenant's Machine.
//   - `wardenWorkflowID(wardenID)` is the same shape, and `wardenRetire` SIGNALS it by id. A signal
//     needs no queue and no poll — so in one shared namespace, knowing a Warden id (which the
//     Controller prints on enrolment and `kontra warden ca list` prints again) is enough to end
//     another tenant's watcher.
//
// Neither is fixed by renaming anything. Both are fixed by the two Machines being in two namespaces,
// which is what this file makes true.
//
// ═══ A TENANT IS A NAMESPACE, AND THERE IS NO DERIVATION BETWEEN THEM ═══
//
// The obvious shape is a tenant NAME plus a rule that turns it into a namespace — `kontra-<tenant>`,
// say. That rule would be a second thing to get right in every surface that has to reach a tenant's
// work, and this repo's own record (`shared/conformance/README.md`) is that a derivation with more than one
// writer drifts silently. So the tenant IS the namespace: one string, no mapping, nothing to keep in
// step. `--tenant` is the flag because that is the operator's word for it; the value is a namespace
// because that is the only boundary Temporal has.
//
// The cost is stated: a Controller cannot rename a tenant without renaming a Temporal namespace, and
// Temporal has no rename. That is the correct cost for the boundary being the real thing rather than
// a label in front of it.
//
// ═══ WHERE THE NAMESPACE LIVES IN THE CREDENTIAL ═══
//
// In the certificate, as its single URI SAN:
//
//	kontra:///ns/<namespace>/warden/<warden-id>
//
// IN THE CERTIFICATE AND NOT ONLY IN `warden.json`, because `warden.json` is a note a Machine writes
// to itself and a Machine can edit its own notes. The certificate is signed by the Fleet CA, so the
// namespace in it is the CA's assertion and changing it invalidates the chain — which is what makes
// it a credential rather than a preference. `loadIdentity` reads the namespace out of the CERTIFICATE
// and refuses a record that disagrees with it.
//
// A URI SAN rather than a subject field because it is unambiguous and single-valued: there is exactly
// one, `wardenScopeOf` refuses a certificate with more, and both halves are named in the string so a
// human reading `openssl x509 -text` can see what it says without a key.
//
// THE SECOND READER OF THIS SHAPE IS NOT IN THIS REPO, and that is why the spelling is pinned by a
// round-trip test with adversarial namespaces rather than left implicit. A Temporal deployment that
// enforces this boundary does so with its own claim mapper reading this URI off the client
// certificate; that configuration lives with whoever runs the server. What kontra guarantees on its
// own is everything below: the namespace is chosen by the Controller, carried in a signed credential,
// and never taken from anything the Machine said.

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// --- the namespace's grammar ----------------------------------------------------------------------

// wardenNamespaceRe is what a tenant may be called.
//
// REFUSED AT MINT, NOT AT ENROLMENT. A namespace that cannot be represented in a certificate is a
// Fleet whose Machines enrol and then fail to load their own identity — hours later, on a Machine
// nobody logs into. The one moment a human is present is `kontra warden ca token`, so that is where
// the refusal is.
//
// The set is the intersection of three things: what Temporal accepts as a namespace, what survives a
// URI path segment with no escaping, and what is safe as ONE path component of the assignment
// directory. The leading character must be alphanumeric, which is what makes `..` and `.` unspellable
// and therefore makes `assignments/<namespace>/` incapable of traversal.
var wardenNamespaceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// validWardenNamespace refuses a name this Fleet cannot carry, and says which rule it broke.
func validWardenNamespace(ns string) error {
	if ns == "" {
		return errors.New("a tenant needs a name, and that name is its Temporal namespace")
	}
	if !wardenNamespaceRe.MatchString(ns) {
		return fmt.Errorf("%q is not usable as a tenant: a tenant IS a Temporal namespace, so it must "+
			"start with a letter or a digit and may then contain letters, digits, `.`, `-` and `_`, up "+
			"to 63 characters — it is carried inside this Machine's certificate and used as one "+
			"directory name on the Controller, and neither survives anything else", ns)
	}
	return nil
}

// --- the scope, as a credential carries it ---------------------------------------------------------

// wardenScopeScheme is the URI scheme of the SAN a **Warden**'s certificate carries. Its own scheme
// rather than `spiffe:` because this is not SPIFFE and a borrowed scheme would make a reader expect a
// trust domain, a workload API and a rotation story that do not exist here.
const wardenScopeScheme = "kontra"

// wardenScope is what one **Warden**'s credential authorises: one namespace, one Machine.
//
// THE WARDEN ID IS IN IT AS WELL AS THE NAMESPACE, and not because anything authorises on it — the
// id is already derived from the key (`wardenIDFor`) and is checked against the subject. It is here
// so that the URI is self-describing when a human or a claim mapper reads it, and so that a
// certificate whose SAN was copied from another Machine's is a certificate that contradicts itself.
type wardenScope struct {
	Namespace string
	WardenID  string
}

// uri renders the scope as the certificate carries it: `kontra:///ns/<namespace>/warden/<id>`.
//
// AN EMPTY AUTHORITY AND EVERYTHING IN THE PATH. A host component would be normalised by some URI
// parsers and not by others, and the one thing that must not happen to a namespace on its way through
// a credential is a case fold — Temporal namespaces are case-sensitive, so `Acme` and `acme` are two
// different tenants and a parser that lowercased one into the other would hand a tenant another
// tenant's namespace. A path segment is never case-folded by anything.
func (s wardenScope) uri() (*url.URL, error) {
	if err := validWardenNamespace(s.Namespace); err != nil {
		return nil, err
	}
	if s.WardenID == "" {
		return nil, errors.New("a scope names a Warden as well as a namespace")
	}
	return &url.URL{Scheme: wardenScopeScheme, Path: "/ns/" + s.Namespace + "/warden/" + s.WardenID}, nil
}

func (s wardenScope) String() string {
	u, err := s.uri()
	if err != nil {
		return fmt.Sprintf("<unrepresentable scope %q/%q>", s.Namespace, s.WardenID)
	}
	return u.String()
}

// wardenScopeOf reads the namespace a certificate authorises, STRICTLY.
//
// Every refusal here is the difference between a scoped credential and an unscoped one, and an
// unscoped credential is exactly what this slice exists to make impossible. In particular:
//
//   - NO SAN AT ALL is refused rather than treated as "the default namespace". A certificate issued
//     before this slice has no scope, and defaulting it would silently put a pre-tenancy Machine into
//     whichever namespace happened to be first — which is the collision ADR 0036 §6 describes,
//     arriving through a fallback nobody chose.
//   - MORE THAN ONE is refused. A caller that took the first would ignore whatever came after it, and
//     "the certificate says two namespaces" must not resolve to "it says the one I read".
func wardenScopeOf(cert *x509.Certificate) (wardenScope, error) {
	if cert == nil {
		return wardenScope{}, errors.New("no certificate to read a namespace out of")
	}
	scoped := make([]*url.URL, 0, 1)
	for _, u := range cert.URIs {
		if u != nil && u.Scheme == wardenScopeScheme {
			scoped = append(scoped, u)
		}
	}
	if len(scoped) == 0 {
		return wardenScope{}, fmt.Errorf("the certificate for %q names no namespace, so it is not a "+
			"scoped credential — every certificate this Fleet issues carries exactly one %s:// URI "+
			"saying which tenant it is for (ADR 0036: a tenant is a Temporal namespace). Re-enrol this "+
			"Machine with `kontra warden join` against a Controller running this version",
			cert.Subject.CommonName, wardenScopeScheme)
	}
	if len(scoped) > 1 {
		return wardenScope{}, fmt.Errorf("the certificate for %q names %d namespaces (%s); a credential "+
			"scoped to two tenants is scoped to neither",
			cert.Subject.CommonName, len(scoped), joinURIs(scoped))
	}
	s, err := parseWardenScope(scoped[0])
	if err != nil {
		return wardenScope{}, err
	}
	// THE SAN AND THE SUBJECT MUST NAME THE SAME MACHINE. Both are written by the CA from the same
	// key, so they can only disagree if one of them was copied from somewhere else.
	if s.WardenID != cert.Subject.CommonName {
		return wardenScope{}, fmt.Errorf("the certificate's subject is %q and its scope names %q — one "+
			"of the two was taken from another Machine's certificate",
			cert.Subject.CommonName, s.WardenID)
	}
	return s, nil
}

// parseWardenScope reads the URI back. The whole grammar, positionally, because a lenient parse here
// would let `kontra:///ns/acme/../globex/warden/x` mean something.
func parseWardenScope(u *url.URL) (wardenScope, error) {
	bad := func(why string) (wardenScope, error) {
		return wardenScope{}, fmt.Errorf("%q is not a Warden scope (%s); the shape is %s:///ns/<namespace>/warden/<warden-id>",
			u.String(), why, wardenScopeScheme)
	}
	if u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return bad("it carries an authority, a query or a fragment, and a scope is a path alone")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "ns" || parts[2] != "warden" {
		return bad(fmt.Sprintf("its path is %q", u.Path))
	}
	if err := validWardenNamespace(parts[1]); err != nil {
		return bad(err.Error())
	}
	// THE WARDEN ID IS CHECKED AGAINST ITS FULL SHAPE, NOT ITS PREFIX, because both halves of a scope
	// become ONE PATH COMPONENT on the Controller — `assignments/<namespace>/<warden-id>.json`. Only
	// the Fleet CA can write this SAN, and it writes `wdn-` plus sixteen lowercase hex (`sign`), so
	// nothing else should ever parse; making that a refusal rather than an assumption means the path
	// this value is joined into cannot be reasoned about wrongly by the next reader.
	if !wardenIDRe.MatchString(parts[3]) {
		return bad(fmt.Sprintf("%q is not a Warden id (%s plus sixteen hex characters)", parts[3], wardenIDPrefix))
	}
	return wardenScope{Namespace: parts[1], WardenID: parts[3]}, nil
}

// wardenIDRe is the shape `wardenIDFor` and `sign` both produce: the prefix plus 64 bits of a
// sha256, lowercase.
var wardenIDRe = regexp.MustCompile(`^` + wardenIDPrefix + `[0-9a-f]{16}$`)

func joinURIs(us []*url.URL) string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.String())
	}
	return strings.Join(out, ", ")
}

// --- the roster the Controller keeps ---------------------------------------------------------------

// caTenantsFile is the Controller's list of tenants it serves.
//
// IT EXISTS BECAUSE A TOKEN RECORD IS SWEPT AND A TENANT IS NOT. `tokens.json` remembers which tenant
// a token was minted for and is deleted thirty days after the token is spent (`mintToken`), so a
// Controller that kept the tenant only there would forget, a month in, which namespaces its enrolled
// Machines are watched in — and stop executing their watchers on the next restart, silently, which is
// the failure shape this repo has now found in five surfaces.
const caTenantsFile = "tenants.json"

// wardenDefaultTenant is the tenant `kontra warden ca token` mints for when nobody says otherwise,
// and it is `temporalNamespace()` — the same `KONTRA_NAMESPACE`-or-`default` every other command in
// this CLI resolves. A single-tenant Fleet therefore behaves exactly as it did before this slice: one
// tenant, named after the namespace it was already using.
func wardenDefaultTenant() string { return temporalNamespace() }

// caTenant is one tenant as the Controller records it. Deliberately almost empty: a tenant IS a
// namespace, so there is nothing to map and nothing to configure. What is worth keeping is when it
// first appeared, because a namespace that showed up unexpectedly is the one question this file can
// answer.
type caTenant struct {
	CreatedAt time.Time `json:"createdAt"`
}

func (c *wardenCA) tenantsPath() string { return filepath.Join(c.Dir, caTenantsFile) }

func (c *wardenCA) readTenants() (map[string]caTenant, error) {
	body, err := os.ReadFile(c.tenantsPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]caTenant{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]caTenant{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", c.tenantsPath(), err)
	}
	return m, nil
}

// noteTenantLocked records a tenant the first time a token is minted for it. THE CALLER HOLDS `c.mu`
// — the only caller is `mintToken`, which is already holding it to spend-check the token file, and
// two locks around one directory would be two chances to take them in different orders.
func (c *wardenCA) noteTenantLocked(name string) error {
	m, err := c.readTenants()
	if err != nil {
		return err
	}
	if _, ok := m[name]; ok {
		return nil
	}
	m[name] = caTenant{CreatedAt: time.Now().UTC()}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.tenantsPath(), append(body, '\n'), 0o600)
}
