// Package hostengine is ADR 0052 §1's HOST engine: the half of kontra's provisioning that runs in
// `kontra`'s own process tree, shells to the host `pulumi`, and converges exactly one project —
// `kontra-control`, the local control plane, written as Pulumi YAML on disk
// (`control/pulumi/Pulumi.yaml`).
//
// ── WHY THIS IS A SECOND ENGINE AND NOT A SECOND CALLER OF THE FIRST ────────────────────────────
//
// ADR 0019's engine lives inside `orchestrator-infra`, drives the Automation API from inline
// TypeScript, and holds a cloud credential. It cannot converge the control plane, because at the
// moment the control plane does not exist there is no container to run it in. So there are two, and
// §1's table says which is which. The load-bearing difference is the runtime: a Pulumi YAML program
// resolves providers as BINARY PLUGINS, so this side needs `pulumi` and nothing else — no Node, no
// `@pulumi/*`, no Automation API (`control/pulumi/Pulumi.yaml:20-27`). That is the whole reason a Go
// CLI can converge anything at all.
//
// ── THE TWO TABLES ARE DISJOINT, AND THAT IS A SECURITY PROPERTY ────────────────────────────────
//
// `control/orchestrator/src/infra/stacks.ts:1-8` says why a table exists at all: *"an HTTP route
// decides which stack to act on, never what the stack contains. A caller that could supply the
// program could provision anything the credential allows."* A second engine reopens that from the
// other side. If this table could name `kontra-fleet/dns`, then `kontra up` would be a path to cloud
// Machines with NO Lease, NO `checkCloudCredential`, NO Temporal record, NO mutex and NO audit —
// every control `kontra fleet up` goes through by going API → Temporal → infra worker instead.
//
// So {@link Project} is the only project {@link ParseStack} accepts, and the refusal NAMES the other
// engine rather than saying "unknown project": an operator who typed a fleet stack here has to be
// told which command owns it, not merely that this one does not. `table_test.go` sweeps the two
// project sets out of their own sources and fails if they ever intersect.
//
// ── AND THE PROVIDER LIST IS THE LAST ROW OF THAT TABLE ─────────────────────────────────────────
//
// §1: *"the host engine may declare no provider that takes a cloud credential."* That is what makes
// "no cloud credential, structurally" true of this side — not a promise about what we pass, but the
// absence of anything that could read one. {@link Providers} is the constant, and `table_test.go`
// sweeps `Pulumi.yaml` against it the way `machineSecret.test.ts` sweeps.
package hostengine

import (
	"fmt"
	"strings"
)

// Project is the only Pulumi project this engine will converge, and it comes from
// `control/pulumi/Pulumi.yaml:98` (`name: kontra-control`) rather than from anything a caller says.
//
// THE PROJECT IS NEVER TAKEN FROM THE COMMAND LINE. `control/pulumi/README.md:52-56` calls that a
// security property rather than tidiness: "A program that is a file on disk in a fixed location,
// with `name: kontra-control` as its first field, cannot be pointed at another project by a caller."
// The CLI only ever picks the STACK; this constant exists so a `--stack` that names some other
// project can be refused by comparison rather than silently honoured.
const Project = "kontra-control"

// Organization is the one organization a `file://` backend has, and it is spelled `organization`.
// See ParseStack for the measurement this exists to survive.
const Organization = "organization"

// DefaultStack is the stack name the whole repository documents — `Pulumi.yaml:7-10`,
// `control/pulumi/README.md:246` and `:301`, all three identically.
//
// ONE SEGMENT, AND THAT IS MEASURED. See ParseStack.
const DefaultStack = "local"

// Projects is this table's key set, as data, so a test can compare it against the infra engine's
// without re-deriving either by hand. It is a slice of one and is expected to stay one: a second
// project here is a second thing `kontra up` can bring into being on somebody's machine.
var Projects = []string{Project}

// Providers is every Pulumi provider the host program is allowed to declare — ADR 0052 §1's last
// row, as a constant.
//
// `docker` ALONE, and the point is what is missing: nothing here reads a cloud credential, so there
// is no value for this engine to leak, mishandle or be tricked into resolving. The infra engine's
// list (`digitalocean`, `docker`, `command`) is the other row of the same table.
//
// Measured against the program as committed: `grep -c 'type: pulumi:providers:'
// control/pulumi/Pulumi.yaml` is 1, and the 37 managed resources are all `docker:index:*`.
// `table_test.go` re-derives that from the file so a provider added later has to come past this
// list.
var Providers = []string{"docker"}

// cloudProviders is the denylist half of the sweep, and it is separate from Providers on purpose.
//
// AN ALLOWLIST ALONE WOULD PASS VACUOUSLY if the sweep's extraction ever stopped finding anything —
// a renamed YAML key, a reformatted file — because "every provider found is allowed" is trivially
// true of no providers at all. The denylist makes the same test fail loudly for the shapes that
// actually matter, and `table_test.go` additionally asserts the extraction found the provider it
// knows is there.
var cloudProviders = []string{
	"digitalocean", "aws", "azure", "azuread", "azure-native", "gcp", "google-native",
	"hcloud", "linode", "vultr", "scaleway", "equinix", "cloudflare", "oci", "alicloud",
	"openstack", "civo", "exoscale", "upcloud",
}

// StackRef is one stack of one project, after ParseStack has had its say.
//
// `Stack` is what reaches `pulumi`, and it is ALWAYS one segment. `Project` is carried so a caller
// can say which project it believes it is converging; it is compared, never passed.
type StackRef struct {
	Project string
	Stack   string
}

// ParseStack turns what an operator typed after `--stack` into the one-segment name `pulumi` will
// accept, and refuses every project that is not this engine's.
//
// ── THE ONE-SEGMENT RULE IS MEASURED, AND THE ERROR IT AVOIDS NAMES NOTHING ─────────────────────
//
// `Pulumi.yaml:12-18`, re-stated in `control/pulumi/README.md:246-249` and `:301-305`: a two-segment
// name is parsed as `<org>/<stack>`, and a `file://` backend has exactly one organization, spelled
// `organization`. On 3.244.0, `stack select kontra-control/local` against a `file://` backend dies
// with
//
//	error: organization name must be 'organization'
//
// which names neither the project nor the fix. `kontra-control/local` is the spelling a reader of
// ADR 0019's fqns would reach for first — the infra engine's stacks really are
// `<project>/<stack>` — so this function ACCEPTS that spelling from a human and hands `pulumi` the
// bare stack, rather than letting the tool explain itself badly. `organization/kontra-control/local`
// is the fully-qualified spelling of the same stack and resolves to it.
//
// ── AND THE PROJECT SEGMENT IS THE DISPATCH TABLE ───────────────────────────────────────────────
//
// A project that is not ours is refused NAMING THE ENGINE THAT OWNS IT. `kontra up --stack
// kontra-fleet/dns` is not a typo to correct, it is a request to provision cloud Machines through
// the one path that has no Lease, no credential check, no Temporal record and no audit (see the
// package comment), and the message has to say where that request does belong.
func ParseStack(s string) (StackRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return StackRef{Project: Project, Stack: DefaultStack}, nil
	}
	parts := strings.Split(s, "/")
	ref := StackRef{Project: Project}
	switch len(parts) {
	case 1:
		ref.Stack = parts[0]
	case 2:
		ref.Project, ref.Stack = parts[0], parts[1]
	case 3:
		// `organization/<project>/<stack>`. Accepted because it is the fully-qualified spelling of
		// the same stack, and refused for any other org because a `file://` backend has no other.
		if parts[0] != Organization {
			return StackRef{}, fmt.Errorf("--stack %q: a file:// backend has exactly one organization "+
				"and it is spelled %q (measured on pulumi 3.244.0: any other name dies with "+
				"`organization name must be 'organization'`, which names neither the project nor the fix)",
				s, Organization)
		}
		ref.Project, ref.Stack = parts[1], parts[2]
	default:
		return StackRef{}, fmt.Errorf("--stack %q: a stack is <stack>, %s/<stack> or %s/%s/<stack>",
			s, Project, Organization, Project)
	}
	if ref.Project != Project {
		return StackRef{}, fmt.Errorf("--stack %q: the host engine converges %s and nothing else.\n"+
			"  %s is the INFRA engine's (ADR 0019): it runs inside orchestrator-infra, holds the cloud\n"+
			"  credential, and is reached through `kontra fleet up|deploy|preview|down` — which goes\n"+
			"  API -> Temporal -> infra worker, so a converge leaves a Lease, a run record and an audit\n"+
			"  trail. Converging it from here would leave none of them (ADR 0052 §1).",
			s, Project, ref.Project)
	}
	if ref.Stack == "" {
		return StackRef{}, fmt.Errorf("--stack %q names a project and no stack (try %q)", s, DefaultStack)
	}
	if strings.ContainsAny(ref.Stack, " \t") {
		return StackRef{}, fmt.Errorf("--stack %q: a stack name has no spaces in it", s)
	}
	return ref, nil
}

// IsCloudProvider reports whether a Pulumi provider name is one that reads a cloud credential.
// Exported for the sweep in table_test.go, which is the only caller: the denylist's job is to make
// that test non-vacuous, not to gate anything at runtime — the runtime gate is the program being a
// file in a fixed location that declares its own providers.
func IsCloudProvider(name string) bool {
	for _, c := range cloudProviders {
		if name == c {
			return true
		}
	}
	return false
}

// IsDeclaredProvider reports whether `name` is in Providers.
func IsDeclaredProvider(name string) bool {
	for _, p := range Providers {
		if name == p {
			return true
		}
	}
	return false
}
