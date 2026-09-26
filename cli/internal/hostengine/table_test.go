package hostengine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// --- the one-segment stack name ------------------------------------------------------------------

// The measured failure this exists for: on pulumi 3.244.0 against a `file://` backend,
// `stack select kontra-control/local` dies with `error: organization name must be 'organization'`,
// which names neither the project nor the fix. `kontra-control/local` is the spelling a reader of ADR
// 0019's fqns reaches for first, so it is accepted from a human and NORMALISED to the bare stack —
// what this asserts is that the string handed to `pulumi` never has a slash in it.
func TestParseStackAlwaysHandsPulumiOneSegment(t *testing.T) {
	for _, in := range []string{"", "local", "kontra-control/local", "organization/kontra-control/local"} {
		ref, err := ParseStack(in)
		if err != nil {
			t.Fatalf("ParseStack(%q): %v", in, err)
		}
		if strings.Contains(ref.Stack, "/") {
			t.Errorf("ParseStack(%q) gave pulumi %q — a name with a slash is parsed as <org>/<stack> and "+
				"a file:// backend has exactly one organization", in, ref.Stack)
		}
		if ref.Project != Project {
			t.Errorf("ParseStack(%q) resolved project %q, want %q", in, ref.Project, Project)
		}
	}
	if ref, _ := ParseStack(""); ref.Stack != DefaultStack {
		t.Errorf("an unset --stack must mean %q, got %q", DefaultStack, ref.Stack)
	}
}

func TestParseStackRefusesAnotherOrganization(t *testing.T) {
	_, err := ParseStack("acme/kontra-control/local")
	if err == nil {
		t.Fatal("a three-segment name with somebody else's org must be refused")
	}
	if !strings.Contains(err.Error(), Organization) {
		t.Errorf("the refusal must name the one organization a file:// backend has: %v", err)
	}
}

// --- the dispatch table -------------------------------------------------------------------------

// ADR 0052 §1: the host table contains `kontra-control` and refuses every other project. This is the
// refusal itself, and what it asserts is not that it FAILS but WHAT IT SAYS: if the host table could
// name `kontra-fleet/dns`, `kontra up` would be a path to cloud Machines with no Lease, no
// `checkCloudCredential`, no Temporal record, no mutex and no audit. An operator who typed that has
// to be told which command does own it, so "unknown project" is the one message this must not be.
func TestHostTableRefusesTheInfraEnginesProjects(t *testing.T) {
	for _, in := range []string{"kontra-fleet/dns", "kontra-docker-fleet/local", "organization/kontra-fleet/dns"} {
		_, err := ParseStack(in)
		if err == nil {
			t.Fatalf("ParseStack(%q) was accepted — the host engine must converge %s and nothing else", in, Project)
		}
		for _, want := range []string{"kontra fleet", "Lease", "ADR 0052 §1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseStack(%q): the refusal must name the other engine and what converging here "+
					"would skip; %q is missing from:\n%v", in, want, err)
			}
		}
	}
}

// --- the sweeps ---------------------------------------------------------------------------------

// repoRoot walks up for the checkout. A test binary's working directory is its own package
// directory, so this is deterministic; it FATALS rather than skips, because a sweep that skips is a
// sweep that passes.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("no checkout above %s — these sweeps read the infra engine's own source and the Pulumi "+
		"program, and cannot be run without them", dir)
	return ""
}

// infraProjectRe pulls the infra engine's project names out of its own source.
//
// READ FROM `stacks.ts` RATHER THAN COPIED INTO GO, because a copy is what makes the intersection
// test vacuous: the failure this guards against is somebody ADDING a project to one table, and a
// hand-maintained list here would be updated by the same person in the same commit or not at all.
var infraProjectRe = regexp.MustCompile(`export const [A-Z_]*PROJECT[A-Z_]*\s*=\s*'([^']+)'`)

// ADR 0052 §1: "Two `default:` arms that throw, and a test asserting the intersection is empty."
// This is that test. It fails if a project is ever added to both tables — which is the moment
// `kontra up` becomes able to converge something whose whole safety story is the route it takes.
func TestTheTwoDispatchTablesDoNotIntersect(t *testing.T) {
	root := repoRoot(t)
	src := filepath.Join(root, "control", "orchestrator", "src", "infra", "stacks.ts")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading the infra engine's dispatch table: %v", err)
	}
	var infra []string
	for _, m := range infraProjectRe.FindAllSubmatch(b, -1) {
		infra = append(infra, string(m[1]))
	}
	// NON-VACUITY, TWICE. A regex that stopped matching would make "the intersection is empty"
	// trivially true, and so would an empty host table.
	if len(infra) < 2 {
		t.Fatalf("found %d projects in %s (%v) — the infra engine has at least kontra-fleet and "+
			"kontra-docker-fleet, so this extraction is broken and the assertion below would pass "+
			"by seeing nothing", len(infra), src, infra)
	}
	if len(Projects) == 0 {
		t.Fatal("the host table is empty, so the intersection is empty for the wrong reason")
	}
	if both := intersect(Projects, infra); len(both) > 0 {
		t.Errorf("%v is in BOTH dispatch tables. ADR 0052 §1: the host engine holds no cloud credential "+
			"and takes no Lease, so a project it shares with the infra engine is a way to provision that "+
			"skips the route (API -> Temporal -> infra worker) its safety comes from", both)
	}
	// And the reciprocal direction, as a statement rather than an inference: the host project must be
	// one the infra table would refuse. That is `planFor`'s default arm
	// (`control/orchestrator/src/infra/stacks.ts:78-82`), which no Go test can call — so this asserts
	// the input to it instead, and the throw belongs to that file's own suite.
	for _, h := range Projects {
		if strings.HasPrefix(h, "kontra-fleet") || strings.HasPrefix(h, "kontra-docker-fleet") {
			t.Errorf("the host project %q is spelled like a fleet project", h)
		}
	}
}

// THE CONTROL CASE FOR THE TEST ABOVE. An intersection test's failure mode is silence — a comparison
// that never matches passes forever — so the comparison itself is exercised with two sets that DO
// intersect, which is the shape `pulumiState.test.ts` uses for the same reason (a non-vacuous control
// beside the real assertion).
func TestTheIntersectionCheckWouldCatchASharedProject(t *testing.T) {
	if both := intersect([]string{Project, "kontra-fleet"}, []string{"kontra-fleet", "kontra-docker-fleet"}); len(both) != 1 || both[0] != "kontra-fleet" {
		t.Fatalf("intersect is broken: got %v, so the real assertion above proves nothing", both)
	}
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if in[x] {
			out = append(out, x)
		}
	}
	return out
}

// providerRe is every `pulumi:providers:<x>` the program declares; typeRe is the namespace of every
// resource it creates. Both, because a provider can also arrive implicitly — a resource of a type
// nobody declared a provider for gets the ambient default provider for that package, which is
// exactly how a cloud provider would sneak in without a `pulumi:providers:` line.
var (
	providerRe = regexp.MustCompile(`type:\s*pulumi:providers:([a-z0-9-]+)`)
	typeRe     = regexp.MustCompile(`(?m)^\s*type:\s*([a-z0-9-]+):[a-zA-Z0-9-]+[:/]`)
)

// ADR 0052 §1's last row: "the host engine may declare no provider that takes a cloud credential."
//
// That is what makes "no cloud credential, structurally" true of this side — not a promise about what
// this CLI passes, but the absence of anything that could read one. Swept out of the program itself,
// the way `machineSecret.test.ts` sweeps, so a `digitalocean:` resource added to Pulumi.yaml fails
// here rather than at the first converge that has a token in its environment.
func TestHostProgramDeclaresNoCloudProvider(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "control", "pulumi", ProgramFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the host engine's program: %v", err)
	}
	declared, used := sweepProviders(b)
	// NON-VACUITY: the file declares exactly one provider today and creates `docker:index:*`
	// resources. If either extraction finds nothing, every assertion below passes by seeing nothing.
	if !declared["docker"] {
		t.Fatalf("no `type: pulumi:providers:docker` found in %s — this sweep's extraction is broken, "+
			"so its refusals below would pass vacuously (found: %v)", path, keys(declared))
	}
	if !used["docker"] {
		t.Fatalf("no docker:… resource types found in %s — extraction broken (found: %v)", path, keys(used))
	}
	for ns := range declared {
		if IsCloudProvider(ns) {
			t.Errorf("%s declares provider %q, which reads a cloud credential. ADR 0052 §1: the host "+
				"engine may declare none — that absence is what makes `kontra up` structurally unable to "+
				"provision in somebody's cloud account", path, ns)
		}
		if !IsDeclaredProvider(ns) {
			t.Errorf("%s declares provider %q, which is not in hostengine.Providers %v. A new provider on "+
				"this side is a decision, not a detail: it is a new thing `kontra up` can bring into being "+
				"on an operator's machine", path, ns, Providers)
		}
	}
	for ns := range used {
		if IsCloudProvider(ns) {
			t.Errorf("%s creates %s:… resources, which needs a cloud credential even with no explicit "+
				"provider — an undeclared type gets the ambient default provider for its package", path, ns)
		}
		if !IsDeclaredProvider(ns) {
			t.Errorf("%s creates %s:… resources and hostengine.Providers is %v; a resource whose package "+
				"nobody declared runs under an implicit default provider", path, ns, Providers)
		}
	}
}

// sweepProviders is the extraction, separated so it can be run against bytes this test controls.
func sweepProviders(b []byte) (declared, used map[string]bool) {
	declared, used = map[string]bool{}, map[string]bool{}
	for _, m := range providerRe.FindAllSubmatch(b, -1) {
		declared[string(m[1])] = true
	}
	for _, m := range typeRe.FindAllSubmatch(b, -1) {
		ns := string(m[1])
		if ns == "pulumi" { // pulumi:providers:… — counted by providerRe
			continue
		}
		used[ns] = true
	}
	return declared, used
}

// THE CONTROL CASE FOR THE SWEEP ABOVE, and it is the whole reason that sweep is worth having: an
// allowlist over an extraction that finds nothing passes on every file, including one with a Droplet
// in it. Both ways a cloud provider can arrive are exercised — an explicit `pulumi:providers:` line,
// and a resource type whose package nobody declared, which silently gets that package's ambient
// default provider.
func TestTheProviderSweepCatchesACloudProvider(t *testing.T) {
	declared, used := sweepProviders([]byte(`
resources:
  doProvider:
    type: pulumi:providers:digitalocean
  box:
    type: digitalocean:index:Droplet
    properties:
      region: ams3
  net:
    type: docker:index:Network
`))
	if !declared["digitalocean"] {
		t.Error("an explicit `type: pulumi:providers:digitalocean` was not seen — the real sweep would " +
			"miss the one line that matters")
	}
	if !used["digitalocean"] {
		t.Error("a `digitalocean:index:Droplet` resource was not seen; an undeclared type still gets " +
			"the ambient default provider for its package, so this is the sneakier of the two shapes")
	}
	if !used["docker"] {
		t.Error("the sweep lost the docker resource beside them, so it is not reading types at all")
	}
	if !IsCloudProvider("digitalocean") || IsCloudProvider("docker") {
		t.Error("IsCloudProvider disagrees with the two cases the sweep is built on")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
