package main

// `kontra fleet` — the Fleet's whole lifecycle, over the orchestrator's Pulumi control plane
// (ADR 0019).
//
// What changed and why it matters: this used to shell out to `tofu` and `ansible-playbook` on
// the operator's laptop. That meant provisioning was the ONE durable operation in kontra with
// no run id, no retry, no cancellation, and no record — a killed terminal left machines nobody
// was tracking. Now the CLI does what every other kontra verb does:
//
//	kontra fleet  ->  POST /api/infra/stacks/<fqn>/<op>  ->  Temporal workflow  ->  Pulumi
//
// The workflow id IS the stack, so two concurrent `up`s on one Fleet are impossible; the
// engine runs on the Controller, which is the only host holding a cloud credential; and every
// converge is visible in the Temporal UI beside the runs it exists to serve.
//
// The layering that made the old design portable is intact and is now one file instead of a
// directory: control/orchestrator/src/infra/programs/fleet.ts is the only provider-coupled code.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
	"github.com/medmahmoudi26/kontra/cli/internal/tmux"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// fleetProject is the Pulumi project the orchestrator's dispatch table knows. A stack outside
// it is refused server-side — the route picks WHICH stack, never WHAT it contains.
const fleetProject = "kontra-fleet"

func cmdFleet(args []string) error {
	if len(args) == 0 {
		fleetUsage()
		return errors.New("fleet: expected a subcommand")
	}
	switch args[0] {
	case "up":
		return fleetUp(args[1:])
	case "deploy":
		return fleetDeployCmd(args[1:])
	case "down":
		return fleetDown(args[1:])
	case "status":
		return fleetStatus(args[1:])
	case "leases":
		return fleetLeases(args[1:])
	case "preview":
		return fleetPreview(args[1:])
	case "help", "-h", "--help":
		fleetUsage()
		return nil
	default:
		fleetUsage()
		return fmt.Errorf("unknown fleet subcommand %q", args[0])
	}
}

func fleetUsage() {
	fmt.Fprint(cliio.Stdout, `kontra fleet — provision Machines and place actors on them, through Pulumi

  kontra fleet up --count 2 --actor <dir>            # Machines only; --actor sizes and NAMES them
  kontra fleet deploy --actor <dir> [--image <ref>]  # place the Artifact
  kontra fleet preview --count 2 --actor <dir>       # what WOULD change
  kontra fleet status [--fleet nscheck-0.1.0]        # the inventory
  kontra fleet leases [--fleet nscheck-0.1.0]        # who is HOLDING it, and until when
  kontra fleet down   [--fleet nscheck-0.1.0]        # destroy the Fleet (refuses if held)

A FLEET IS NAMED AFTER WHAT IT PLACES: <actor>-<version>, read from actor.json. So the name
is the same string in the run that creates a Fleet and the command that destroys it, and
there is none to invent. --fleet names one directly, for a Machines-only Fleet.

--tag LABELS the Machines: a DigitalOcean tag, an inventory group and the kf-<tag>-NN name
prefix. Nothing dispatches on it; it defaults to the actor's name.

A FLEET IS CAPACITY, AND A RUN HOLDS A LEASE ON IT (ADR 0037). Several Runs may hold one
Fleet at once; its Machines are destroyed when the LAST Lease drops, and every Lease expires
on a clock, so a Fleet whose holders are all gone collects itself. The leases subcommand is
that Lease workflow, and down refuses to destroy a Fleet somebody is holding — --force overrides.

kontra fleet up takes NO Lease, deliberately: it is an operator's act, not a Run, so what it
creates stands until a hand ends it.

Every subcommand runs as a Temporal workflow on the Controller (workflow id = the stack),
so a converge is watchable in the Temporal UI and survives losing this terminal.

--actor reads the Machine size and the container limits from that actor's actor.json,
so hardware is declared with the actor rather than typed at the prompt.
`)
}

// tagRe bounds what a tag may be. A tag is NOT an enum of known workloads — it is a DigitalOcean
// tag and an inventory group, so hardcoding `crawl|bust` meant every new actor had to either edit
// this file or mislabel its infrastructure. What matters is that the value is safe to interpolate
// into a tag, a group name and a machine name. The server enforces the same rule; this copy only
// makes the refusal immediate.
var tagRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,15}$`)

func validTag(tag string) error {
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("--tag %q invalid: lowercase letters, digits and dashes, 2-16 chars "+
			"(it becomes the DigitalOcean tag `kontra-%s`, the inventory group and the `kf-%s-NN` machine name)", tag, tag, tag)
	}
	return nil
}

// --- shared flags -------------------------------------------------------------------------

type fleetFlags struct {
	fs       *flag.FlagSet
	tag      *string
	fleet    *string
	count    *int
	sessions *int
	actor    *string
	region   *string
	size     *string
	control  *string
	api      *string
	tmux     *bool
	// force is `down`'s override of the Lease guard (ADR 0037). Registered on the shared set with
	// every other flag, and meaningful on `down` alone.
	force   *bool
	timeout *time.Duration
	/** The manifest --actor names, read once. See spec(). */
	cached *actorTargets
}

func fleetFlagSet(name string) *fleetFlags {
	fs := flag.NewFlagSet("fleet "+name, flag.ContinueOnError)
	return &fleetFlags{
		fs:       fs,
		tag:      fs.String("tag", "", "a LABEL for these Machines — a DigitalOcean tag, an inventory group and the `kf-<tag>-NN` name prefix. Defaults to the actor's name"),
		fleet:    fs.String("fleet", "", "which Fleet. Defaults to `<actor>-<version>` from --actor, which is what names a Fleet"),
		count:    fs.Int("count", 1, "how many Machines"),
		sessions: fs.Int("sessions", 0, "live Sessions per Machine (density; 0 leaves the host default of 4)"),
		actor:    fs.String("actor", "", "actor directory — reads hardware from its actor.json"),
		region:   fs.String("region", "", "override the region from actor.json"),
		size:     fs.String("size", "", "override the Machine size from actor.json"),
		control:  fs.String("controller", "", "address the Machines call home on (default: KONTRA_CONTROLLER)"),
		api:      fs.String("api", orchestratorURL(), "orchestrator base URL"),
		tmux:     fs.Bool("tmux", false, "ALSO converge an attachable tmux session on each Machine after placement (`tmux attach -t kontra-<actor>`, or the Dashboard)"),
		force:    fs.Bool("force", false, "`down` only: destroy the Fleet even though Runs still hold Leases on it"),
		timeout:  fs.Duration("timeout", 30*time.Minute, "how long to wait for the converge"),
	}
}

// spec reads --actor's manifest, once, and caches it. Both the fleet's NAME and its tag default
// from it, and re-reading the file per question is how the two could disagree.
func (f *fleetFlags) spec() (*actorTargets, error) {
	if f.cached == nil && *f.actor != "" {
		s, err := readActorTargets(*f.actor)
		if err != nil {
			return nil, err
		}
		f.cached = s
	}
	return f.cached, nil
}

// resolvedName is which Fleet this command is about: `--fleet`, or `<actor>-<version>` from the
// manifest.
//
// A FLEET IS NAMED AFTER WHAT IT PLACES. It used to be `--campaign`, defaulting to the literal
// string "default" — so every fleet anybody brought up without thinking about it shared one stack,
// and a second `fleet up` silently reconverged the first one's Machines. Deriving it from the
// Artifact means the name is the same string in the workflow that creates a fleet and the command
// that destroys it, with nothing to keep in agreement.
//
// There is no default of last resort. A Machines-only fleet (`--count 2` with no actor) has
// nothing to cliutil.Derive a name from, and inventing one is how an operator ends up owning a stack they
// cannot find again.
func (f *fleetFlags) resolvedName() (string, error) {
	if *f.fleet != "" {
		return *f.fleet, nil
	}
	spec, err := f.spec()
	if err != nil {
		return "", err
	}
	if spec != nil && spec.Name != "" && spec.Version != "" {
		return spec.Name + "-" + spec.Version, nil
	}
	return "", errors.New(
		"which Fleet? A Fleet is named after the Artifact it places, so pass --actor <dir> and it " +
			"is read from actor.json — or name it yourself with --fleet <name>")
}

// resolvedTag is the LABEL these Machines carry: `--tag`, or the actor's own name.
//
// Almost every fleet wants its Machines labelled after what runs on them, so the flag exists to be
// omitted. It used to default to the literal "crawl", which put a `role-crawl` tag on the Machines
// of every actor that was not a crawler.
func (f *fleetFlags) resolvedTag() (string, error) {
	tag := *f.tag
	if tag == "" {
		spec, err := f.spec()
		if err != nil {
			return "", err
		}
		if spec != nil {
			tag = spec.Name
		}
	}
	if tag == "" {
		return "", errors.New("--tag is required without --actor: it labels the Machines")
	}
	return tag, validTag(tag)
}

// fqn is the stack, which is also the id of the workflow that owns it. Reported as an error only
// where the name cannot be resolved; every caller has already resolved it by then.
func (f *fleetFlags) fqn() string {
	name, err := f.resolvedName()
	if err != nil {
		return fleetProject + "/?"
	}
	return fleetProject + "/" + name
}

// args assembles the DESIRED STATE of the fleet stack. Every converge sends the whole thing:
// Pulumi is declarative, so an omitted field is a request to remove what it describes, not a
// request to leave it alone. That is why `up` inherits the current placement (see fleetUp).
//
// It takes an ALREADY-BUILT Artifact rather than building one. Assembling the desired state and
// producing an Artifact are different jobs with different costs — one is pure, the other
// compiles a binary and uploads 20 MiB — and folding them together made the pure one untestable
// without a network.
func (f *fleetFlags) args(art *bundle) (map[string]any, error) {
	tag, err := f.resolvedTag()
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"tag":      tag,
		"machines": *f.count,
	}
	// Density. Omitted rather than sent as 0, because the desired state is declarative: a 0 here
	// would be a request to set the cap to nothing, and what the operator means by leaving the
	// flag alone is "whatever the host defaults to".
	if *f.sessions > 0 {
		out["maxSessions"] = *f.sessions
	}
	spec, err := f.spec()
	if err != nil {
		return nil, err
	}
	if spec != nil {
		if spec.Machine.Size != "" {
			out["size"] = spec.Machine.Size
		}
		if spec.Machine.Region != "" {
			out["region"] = spec.Machine.Region
		}
		if spec.Machine.Image != "" {
			out["image"] = spec.Machine.Image
		}
	}
	// Explicit flags beat the actor's declaration — the actor says what it needs, the
	// operator says what it gets.
	if *f.region != "" {
		out["region"] = *f.region
	}
	if *f.size != "" {
		out["size"] = *f.size
	}

	// --tmux is deliberately NOT a stack argument any more (ADR 0020). Session existence is a
	// Temporal converge on the infra queue, run AFTER placement — see convergeSessions. Two reasons
	// it moved: the systemd unit it used to install was `ConditionPathExists=/usr/bin/tmux` beside an
	// apt install that was allowed to fail, so a Machine could deploy "successfully" and never be
	// viewable; and a Machine deployed WITHOUT this flag can now be given a session on demand, with
	// no re-deploy. (It had also never worked as a stack arg: the server narrows untrusted args to
	// strings and numbers, so the boolean was dropped before the program saw it.)

	controller := controllerAddress(*f.control)
	if art != nil {
		// The MACHINES' view of the registry, which is not necessarily this process's: a Machine
		// resolving `localhost:5000` means its own loopback. `f.registryAddr()` is the same
		// resolution the push used, called with the same inputs, so the two cannot disagree.
		out["bundleUrl"] = bundleBlobURL(f.registryAddr(), art.Name, art.SHA)
		out["bundleSha"] = art.SHA
		out["actorName"] = art.Name
		out["actorVersion"] = art.Version
		out["actorEngine"] = art.Engine
		out["controller"] = controller
	}
	return out, nil
}

// inheritPlacement fills in the placement `kontra fleet up` is NOT being asked to change, from the
// Fleet's own stack outputs. Returns the line to print, or empty when the Fleet places nothing.
//
// ═══ THE WHOLE LIST FIRST, AND THIS IS NOT A PREFERENCE ═══
//
// A Fleet places SEVERAL Artifacts since ADR 0037's other half (slice 11), and it echoes all of them
// back as `placements`. The scalar branch below reads only the FIRST — `programs/fleet.ts` fills the
// scalars from `placements[0]` — so on a packed Fleet it would send a desired state naming one of
// two, which DELETES the other, runs its teardown and stops its Worker. `kontra fleet up --count 6`
// would then be a scale-up that silently un-deploys half the Fleet: the very failure this inherit
// exists to prevent, one Artifact later. So `placements` wins whenever it is there, and the scalars
// remain for a stack converged before packing existed.
//
// A FUNCTION RATHER THAN A BLOCK INSIDE `fleetUp`, because it is the only way this repo's SECOND
// writer of a packed converge can be driven by `shared/conformance/placement.json` — the corpus that exists
// precisely because nothing joins the two writers and the reader but matching string literals.
func inheritPlacement(a map[string]any, outputs map[string]any, sessions int) string {
	if placed, ok := placementsOutput(outputs); ok {
		a["placements"] = placed
		// An explicit --sessions re-densifies EVERY placement, because that is the only thing this
		// command can honestly mean on a packed Fleet: it names no actor.
		if sessions > 0 {
			for _, p := range placed {
				p["maxSessions"] = sessions
			}
		}
		// AND THE CONVERGE-LEVEL SPELLING GOES. `programs/fleet.ts` reads the scalars only when
		// `placements` is absent, so a `maxSessions` left there by `args()` would be a second,
		// half-filled desired state riding beside the true one.
		delete(a, "maxSessions")
		return "keeping " + describePlacements(placed) + " — `fleet deploy` changes what is placed"
	}
	if url, _ := outputs["bundleUrl"].(string); url == "" {
		return ""
	}
	for _, k := range []string{"bundleUrl", "bundleSha", "actorName", "actorVersion", "actorEngine", "controller"} {
		a[k], _ = outputs[k].(string)
	}
	// Separately, because it is the one placement field that is a NUMBER and the loop above would
	// leave it nil — which reads to the program as "remove the cap", so a plain scale-up would
	// quietly reset the fleet's density to the host default. An explicit --sessions still wins:
	// this only fills what the command did not say.
	if n, ok := outputs["maxSessions"].(float64); ok && n > 0 && sessions == 0 {
		a["maxSessions"] = int(n)
	}
	return fmt.Sprintf("keeping the placed actor %s@%s (bundle %s…) — `fleet deploy` changes it",
		a["actorName"], a["actorVersion"], b32(fmt.Sprint(a["bundleSha"])))
}

// placementsOutput reads a Fleet's placements back out of its own stack outputs.
//
// ═══ WHY IT IS A PARSE AND NOT A CAST ═══
//
// The outputs came back through JSON, so a `[]PlacementArgs` on the TypeScript side arrives here as
// `[]any` of `map[string]any` with every number a `float64`. Sending that straight back would put
// `maxSessions: 8.0` and `workers: 2.0` on the wire — which `coerceFleetArgs` happens to survive
// (`Number()` of a float is that float) but `machinesFor` does not, because `Number.isInteger(2.0)`
// is true in JavaScript and this is exactly the kind of "happens to work" that a second reader
// changes out from under you. So each entry is rebuilt with the types the corpus names.
//
// AN ENTRY WITH NO `bundleUrl` IS DROPPED, matching `coercePlacement` on the reader's side. A Fleet
// that has never been placed on echoes `placements: []`, and `ok` is false there — that is not a
// packed Fleet with nothing on it, it is a Fleet whose scalars the caller should read instead.
func placementsOutput(outputs map[string]any) ([]map[string]any, bool) {
	raw, _ := outputs["placements"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		entry, _ := item.(map[string]any)
		url, _ := entry["bundleUrl"].(string)
		name, _ := entry["actorName"].(string)
		if url == "" || name == "" {
			continue
		}
		p := map[string]any{"actorName": name, "bundleUrl": url}
		for _, k := range []string{"actorVersion", "actorEngine", "bundleSha", "controller"} {
			if v, ok := entry[k].(string); ok && v != "" {
				p[k] = v
			}
		}
		for _, k := range []string{"maxSessions", "workers"} {
			if n, ok := entry[k].(float64); ok && n > 0 {
				p[k] = int(n)
			}
		}
		out = append(out, p)
	}
	return out, len(out) > 0
}

// fallbackPlacementLine reads the pre-packing scalars as one placement, for a stack whose outputs
// predate `placements`. Empty when the Fleet has nothing on it, which is a Machines-only converge.
//
// It mirrors `programs/fleet.ts:placementsOf`'s fold rather than inventing a second reading of the
// same outputs — the difference between the two would be an operator's `fleet status` disagreeing
// with what the next converge actually sends.
func fallbackPlacementLine(outputs map[string]any) []map[string]any {
	name, _ := outputs["actorName"].(string)
	if name == "" {
		return nil
	}
	p := map[string]any{"actorName": name}
	for _, k := range []string{"actorVersion", "bundleSha"} {
		p[k], _ = outputs[k].(string)
	}
	if n, ok := outputs["maxSessions"].(float64); ok && n > 0 {
		p["maxSessions"] = int(n)
	}
	return []map[string]any{p}
}

// describePlacements is the line an operator reads when a scale-up keeps what is placed.
//
// IT NAMES ALL OF THEM, because the number is the point: "keeping nscheck@0.1.0" on a Fleet also
// running subfinder would be the exact sentence a reader would use to conclude the other one had
// gone, on the command whose whole risk is that it might have.
func describePlacements(placed []map[string]any) string {
	refs := make([]string, 0, len(placed))
	for _, p := range placed {
		version, _ := p["actorVersion"].(string)
		if version == "" {
			version = "?"
		}
		refs = append(refs, fmt.Sprintf("%v@%s", p["actorName"], version))
	}
	sort.Strings(refs)
	if len(refs) == 1 {
		return "the placed actor " + refs[0]
	}
	return fmt.Sprintf("the %d placed actors %s (packed onto the same Machines, sharing their egress "+
		"addresses)", len(refs), strings.Join(refs, ", "))
}

// registryAddr is the OCI registry this fleet's Bundle is pushed to AND pulled from. It is a
// method rather than a value threaded through because `args()` and the push in `fleetDeploy` are
// two call sites that must never resolve it differently — see bundleRegistry for the split that
// caused, and why it is not `deploy.go:registryAddress`.
func (f *fleetFlags) registryAddr() string { return bundleRegistry("", *f.control) }

// controllerAddress is where fleet Machines reach Temporal, the catalog, S3 and Redis. It is
// NOT localhost and there is no useful default: a Machine that cannot name the Controller
// starts, registers nothing, and looks idle.
func controllerAddress(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return cliutil.EnvOr("KONTRA_CONTROLLER", "10.124.0.2")
}

// b32 shortens a content hash for a log line; the full value is in the stack outputs.
func b32(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// --- actor.json hardware ------------------------------------------------------------------

// actorTargets is actor.json's `targets` block — the actor's own statement of what it needs to
// run, in both places it can run. Keeping it beside the code is the point: the size a crawler
// needs is a property of the crawler, not of whoever last typed a `fleet up`.
type actorTargets struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Container struct {
		Memory  string `json:"memory"`
		ShmSize string `json:"shmSize"`
		Cpus    string `json:"cpus"`
	} `json:"-"`
	Machine struct {
		Size   string `json:"size"`
		Region string `json:"region"`
		Image  string `json:"image"`
	} `json:"-"`
}

// readActorTargets parses actor.json. `cpus` is a NUMBER in the manifest and a string on the
// docker flag, so it is decoded loosely rather than forcing authors to quote it.
func readActorTargets(dir string) (*actorTargets, error) {
	b, err := os.ReadFile(filepath.Join(dir, "actor.json"))
	if err != nil {
		return nil, fmt.Errorf("--actor %s: %w", dir, err)
	}
	var raw struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Targets struct {
			Container map[string]any `json:"container"`
			Machine   map[string]any `json:"machine"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s/actor.json: %w", dir, err)
	}
	t := &actorTargets{Name: raw.Name, Version: raw.Version}
	t.Container.Memory = looseString(raw.Targets.Container["memory"])
	t.Container.ShmSize = looseString(raw.Targets.Container["shmSize"])
	t.Container.Cpus = looseString(raw.Targets.Container["cpus"])
	t.Machine.Size = looseString(raw.Targets.Machine["size"])
	t.Machine.Region = looseString(raw.Targets.Machine["region"])
	t.Machine.Image = looseString(raw.Targets.Machine["image"])
	return t, nil
}

func looseString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	default:
		return fmt.Sprint(x)
	}
}

// --- the control-plane client ---------------------------------------------------------------

type stackOpAccepted struct {
	FQN        string `json:"fqn"`
	Op         string `json:"op"`
	WorkflowID string `json:"workflowId"`
	RunID      string `json:"runId"`
}

type stackOpStatus struct {
	FQN      string         `json:"fqn"`
	Status   string         `json:"status"`
	Progress map[string]any `json:"progress"`
	Result   *struct {
		FQN     string         `json:"fqn"`
		Result  string         `json:"result"`
		Changes map[string]int `json:"changes"`
		Outputs map[string]any `json:"outputs"`
	} `json:"result"`
}

// stateToken resolves the token the INFRA routes accept — KONTRA_STATE_TOKEN and only that.
//
// Deliberately not exploreToken(): that one prefers KONTRA_EXPLORE_TOKEN, and the two are
// different values here on purpose. Reading a dataset and provisioning cloud machines are not
// the same privilege, so the wider token is refused server-side — sending it produces a 401
// that looks like a missing token rather than the wrong one.
func stateToken() string {
	if t := os.Getenv("KONTRA_STATE_TOKEN"); t != "" {
		return t
	}
	if root, ok := findUp(".env"); ok {
		if t := dotEnv(filepath.Join(root, ".env"))["KONTRA_STATE_TOKEN"]; t != "" {
			return t
		}
	}
	return envFromRunningStack("KONTRA_STATE_TOKEN")
}

func infraAPI(base string) (*apiClient, error) {
	token := stateToken()
	if token == "" {
		return nil, errors.New("no KONTRA_STATE_TOKEN — the infra routes reach the one process holding " +
			"the cloud credential, so they fail closed. Set it to the value the orchestrator runs with " +
			"(it is in the checkout's .env)")
	}
	return newAuthAPI(base, token), nil
}

func startOp(api *apiClient, fqn, op string, args map[string]any) (*stackOpAccepted, error) {
	var out stackOpAccepted
	path := "/api/infra/stacks/" + url.PathEscape(fqn) + "/" + op
	if err := api.postJSON(path, map[string]any{"args": args}, &out); err != nil {
		var he *httpError
		if errors.As(err, &he) && he.status == 409 {
			// `--fleet`, NOT `--campaign`. This line advertised a flag that has not existed since
			// Fleet names became derived, so the one remedy offered for a 409 was a command that
			// refuses to parse — the same shape as an error telling you to re-run a deploy that
			// cannot help. An error's advice is only advice if it runs.
			return nil, fmt.Errorf("a %s converge is already running for %s — watch it with "+
				"`kontra fleet status --fleet %s`", op, fqn, strings.TrimPrefix(fqn, fleetProject+"/"))
		}
		return nil, err
	}
	return &out, nil
}

func readOp(api *apiClient, fqn string) (*stackOpStatus, error) {
	var out stackOpStatus
	if err := api.getJSON("/api/infra/ops/"+url.PathEscape(fqn), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// watchOp polls the converge to completion, printing each resource the engine touches.
//
// The progress is the ACTIVITY'S HEARTBEAT, forwarded from Pulumi's own event stream — so the
// same detail is in the Temporal UI, and closing this terminal loses the view, not the work.
func watchOp(api *apiClient, fqn string, timeout time.Duration) (*stackOpStatus, error) {
	deadline := time.Now().Add(timeout)
	var lastLine string
	for time.Now().Before(deadline) {
		st, err := readOp(api, fqn)
		if err != nil {
			return nil, err
		}
		if line := progressLine(st.Progress); line != "" && line != lastLine {
			fmt.Fprintf(cliio.Stdout, "  %s\n", line)
			lastLine = line
		}
		switch st.Status {
		case "COMPLETED":
			return st, nil
		case "FAILED", "CANCELED", "TERMINATED", "TIMED_OUT":
			return st, fmt.Errorf("converge %s: %s (see the Temporal UI for the failure — "+
				"workflow id %s)", fqn, strings.ToLower(st.Status), fqn)
		}
		time.Sleep(3 * time.Second)
	}
	return nil, fmt.Errorf("gave up watching %s after %s — the converge is STILL RUNNING on the "+
		"Controller; `kontra fleet status` rejoins it", fqn, timeout)
}

// progressLine renders one heartbeat. The engine reports a URN per resource; the tail is the
// only part an operator reads.
func progressLine(p map[string]any) string {
	if len(p) == 0 {
		return ""
	}
	if urn, ok := p["urn"].(string); ok {
		parts := strings.Split(urn, "::")
		return fmt.Sprintf("%v %s", p["op"], parts[len(parts)-1])
	}
	if ch, ok := p["changes"]; ok {
		b, _ := json.Marshal(ch)
		return "summary " + string(b)
	}
	if phase, ok := p["phase"].(string); ok {
		return phase
	}
	return ""
}

// --- tmux session converge (ADR 0020) -------------------------------------------------------
//
// `--tmux` now means "also converge a tmux session on each Machine after placement". The session
// is a Temporal workflow whose id IS the Machine (`tmux-<machine>`, conflict policy FAIL), on the
// infra queue — the only worker holding the fleet SSH key, and the only place session creation
// belongs, because reaching a Machine is Fleet authority.
//
// The CLI dials Temporal directly for this, exactly as `kontra workers list` does, rather than
// going through the orchestrator's infra routes: those routes exist to pick WHICH STACK to
// converge, and a session is not a stack.

// tmuxWindow mirrors the workflow's window shape. Deliberately unused by the CLI: an empty window
// list means "the defaults", and the defaults are decided server-side (journals, never the Worker).
// Sending commands from here would make the CLI decide what runs as root on a Machine, which is the
// authority `machine.ts` refuses to hand a caller.
type tmuxWindow struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// tmuxSessionInput is tmuxSessionWorkflow's argument, JSON-shaped for the TS worker.
type tmuxSessionInput struct {
	Machine string       `json:"machine"`
	Host    string       `json:"host"`
	Session string       `json:"session"`
	Windows []tmuxWindow `json:"windows"`
}

// tmuxSessionResult is what the workflow returns.
type tmuxSessionResult struct {
	Machine string   `json:"machine"`
	Session string   `json:"session"`
	Windows []string `json:"windows"`
	Created bool     `json:"created"`
}

// tmuxSessionTargets derives one converge per Machine from the stack outputs the converge just
// returned. Pure, so the mapping is testable with no fleet and no Temporal.
//
// The session name is {@link fleetSessionName}.
func tmuxSessionTargets(outputs map[string]any) []tmuxSessionInput {
	raw, err := json.Marshal(outputs["inventory"])
	if err != nil {
		return nil
	}
	var inv map[string]struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		PublicIP string `json:"publicIp"`
		Tag      string `json:"tag"`
		// Pre-rename checkpoints. A live fleet must not lose its label to a field name changing.
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &inv) != nil {
		return nil
	}
	// The same rule `panels/discovery.ts:sessionNameFor` applies, written independently on this
	// side like every other cross-language literal in this repo, and held to one answer by
	// fleet_tmux_test.go.
	actor, _ := outputs["actorName"].(string)
	version, _ := outputs["actorVersion"].(string)
	tag, _ := outputs["tag"].(string)
	if tag == "" {
		tag, _ = outputs["role"].(string)
	}
	// A PACKED MACHINE'S SESSION IS NAMED AFTER THE FLEET, NOT AFTER ONE OF ITS WORKERS. A tmux
	// session is per MACHINE (ADR 0020) and the scalars in these outputs describe only the FIRST
	// placement, so on a Fleet running two `nscheck-0.1.0` would be a Machine's name claiming it
	// belongs to one Actor while a second one is running on it. The tag is the honest answer and is
	// already the fallback `fleetSessionName` uses for a Machine with no placement at all.
	if placed, ok := placementsOutput(outputs); ok && len(placed) > 1 {
		actor, version = "", ""
	}
	session := fleetSessionName(actor, version, tag)
	out := make([]tmuxSessionInput, 0, len(inv))
	for _, name := range sortedKeys(inv) {
		m := inv[name]
		machine := m.Name
		if machine == "" {
			machine = name
		}
		// The private VPC address is how the Controller reaches a Machine; the public one is the
		// fallback, because that is what the placement command actually dials today.
		host := m.Host
		if host == "" {
			host = m.PublicIP
		}
		if machine == "" || host == "" {
			continue
		}
		out = append(out, tmuxSessionInput{
			Machine: machine,
			Host:    host,
			Session: session,
			// Empty: the workflow's own defaults decide what a window runs.
			Windows: []tmuxWindow{},
		})
	}
	return out
}

// fleetSessionName is the tmux session a Machine's Terminals attach to: `<actor>-<version>`.
//
// NO `kontra-` PREFIX any more. It was two things at once and did neither well: the Monitor's
// discovery mechanism for local sessions — which is a tmux user option now, and one that can also
// say what KIND a session is — and a namespace inside a tmux server, which is not worth a prefix on
// every name an operator types.
//
// THE VERSION IS THE SUBSTANTIVE HALF. `kontra-webcrawl` said which Actor was on a Machine and not
// which BUILD, so two fleets running two versions of one Actor produced two identical session
// names and `tmux attach -t kontra-webcrawl` was ambiguous the moment a deploy was in flight.
//
// Falls back to the fleet's tag for a Machine with no placement, and to `fleet` for one with
// neither. THAT FALLBACK DIFFERS FROM tmux.Session's `actor` ON PURPOSE and the difference is
// recorded in shared/conformance/queues.json §tmux_session rather than left as two literals nobody can
// tell from a typo: this names a MACHINE, which still has Terminals when no Actor is placed on
// it, and calling such a session `actor` would be a lie about what is running there.
//
// A NAME THE WHITELIST REFUSES IS REPLACED, NEVER REPAIRED. This string is interpolated into a
// command run as root on a remote Machine, and there is no safe way to rewrite an unsafe one.
// `panels/discovery.ts:sessionNameFor` has held that line since ADR 0020; this side did not, and
// the corpus is what found it — `web crawl; reboot` produced `web crawl; reboot-0_1_0` here and
// `fleet` there, for the same Machine, so the converge threw on a name the Monitor had already
// decided was `fleet`.
func fleetSessionName(actor, version, tag string) string {
	// tmux.SafeName because tmux rewrites `.` and `:` to `_` at creation — see its comment for what
	// an unsanitised `<actor>-<version>` costs.
	base := "fleet"
	switch {
	case actor != "" && version != "":
		base = tmux.SafeName(actor + "-" + version)
	case actor != "":
		base = tmux.SafeName(actor)
	case tag != "":
		base = tmux.SafeName(tag)
	}
	if !safeSessionName.MatchString(base) {
		return "fleet"
	}
	return base
}

// safeSessionName is `panels/ids.ts:SAFE.session`, written independently on this side like every
// other cross-language literal here: what tmux itself allows, minus every shell metacharacter.
var safeSessionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// tmuxConverger is the Temporal seam — an interface so tests never dial anything, the same shape
// `queueDescriber` has in workers.go.
type tmuxConverger interface {
	Converge(ctx context.Context, in tmuxSessionInput) (tmuxSessionResult, error)
	Close()
}

// newTmuxConverger dials Temporal ($KONTRA_ADDRESS / $KONTRA_NAMESPACE); a func var so tests can
// swap it out.
var newTmuxConverger = func() (tmuxConverger, error) {
	c, err := client.Dial(client.Options{HostPort: config.TemporalAddress(), Namespace: config.TemporalNamespace()})
	if err != nil {
		return nil, err
	}
	return &temporalTmuxConverger{c: c, queue: cliutil.EnvOr("KONTRA_INFRA_QUEUE", "kontra-infra")}, nil
}

type temporalTmuxConverger struct {
	c     client.Client
	queue string
}

func (t *temporalTmuxConverger) Close() { t.c.Close() }

func (t *temporalTmuxConverger) Converge(ctx context.Context, in tmuxSessionInput) (tmuxSessionResult, error) {
	var out tmuxSessionResult
	run, err := t.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// The id IS the Machine: one writer per Machine, structurally. Two concurrent converges
		// would race on `has-session` and leave duplicate windows.
		ID:                       "tmux-" + in.Machine,
		TaskQueue:                t.queue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
	}, "tmuxSessionWorkflow", in)
	if err != nil {
		return out, err
	}
	// Waited on, not fired and forgotten: a converge that failed (no tmux package, a Machine still
	// booting) has to be visible here, because the silent version of this is exactly the failure
	// ADR 0020 deleted.
	if err := run.Get(ctx, &out); err != nil {
		return out, err
	}
	return out, nil
}

// convergeSessions runs the session converge for every Machine in the inventory. Best-effort per
// Machine and never fatal to the deploy: the actor is already placed and running by the time this
// runs, and a Machine that cannot be given a view is not a Machine that cannot work.
func convergeSessions(st *stackOpStatus, timeout time.Duration) error {
	if st.Result == nil {
		return nil
	}
	targets := tmuxSessionTargets(st.Result.Outputs)
	if len(targets) == 0 {
		fmt.Fprintln(cliio.Stdout, "--tmux: no machines in the inventory, nothing to converge")
		return nil
	}
	d, err := newTmuxConverger()
	if err != nil {
		return fmt.Errorf("--tmux: cannot reach temporal at %s: %w", config.TemporalAddress(), err)
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	failed := 0
	for _, in := range targets {
		res, err := d.Converge(ctx, in)
		if err != nil {
			failed++
			fmt.Fprintf(cliio.Stdout, "  %s: session converge FAILED: %v\n", in.Machine, err)
			continue
		}
		verb := "already had"
		if res.Created {
			verb = "created"
		}
		fmt.Fprintf(cliio.Stdout, "  %s: %s session %s [%s]\n", in.Machine, verb, res.Session,
			strings.Join(res.Windows, " "))
	}
	fmt.Fprintf(cliio.Stdout, "--tmux: %d/%d machines have an attachable session — `tmux attach -t %s` on the "+
		"Machine, or the Dashboard in the orchestrator UI\n", len(targets)-failed, len(targets), targets[0].Session)
	return nil
}

// --- subcommands ----------------------------------------------------------------------------

func fleetUp(args []string) error {
	f := fleetFlagSet("up")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	api, err := infraAPI(*f.api)
	if err != nil {
		return err
	}
	// `up` brings MACHINES into existence; `deploy` places an Artifact on them. One verb never
	// does both (infra/CONTEXT.md) — so --actor here only supplies the hardware the actor asked
	// for, and only an explicit --image places anything.
	//
	// Placement is nonetheless a property of the stack, so a scale-up that said nothing about
	// an actor would REMOVE the one already running. Inherit it unless this command is
	// deliberately changing it.
	a, err := f.args(nil)
	if err != nil {
		return err
	}
	{
		if prev, err := readOp(api, f.fqn()); err == nil && prev.Result != nil {
			if line := inheritPlacement(a, prev.Result.Outputs, *f.sessions); line != "" {
				fmt.Fprintln(cliio.Stdout, line)
			}
		}
	}
	tag, err := f.resolvedTag()
	if err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "converging %s → %d %s machine(s)\n", f.fqn(), *f.count, tag)
	if _, err := startOp(api, f.fqn(), "up", a); err != nil {
		return err
	}
	st, err := watchOp(api, f.fqn(), *f.timeout)
	if err != nil {
		return err
	}
	if err := printFleet(st); err != nil {
		return err
	}
	if *f.tmux {
		return convergeSessions(st, *f.timeout)
	}
	return nil
}

func fleetDeployCmd(args []string) error {
	f := fleetFlagSet("deploy")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	api, err := infraAPI(*f.api)
	if err != nil {
		return err
	}
	// A deploy must not silently resize the Fleet. Take the machine count from what is
	// actually there unless the operator said otherwise.
	if !flagPassed(f.fs, "count") {
		if prev, err := readOp(api, f.fqn()); err == nil && prev.Result != nil {
			if n, ok := prev.Result.Outputs["machines"].(float64); ok && int(n) > 0 {
				*f.count = int(n)
			}
		}
	}
	// Build and publish the Artifact HERE, at deploy time, so the sha the Machines verify is
	// the sha of the code in this checkout. Placing a Bundle somebody else built reopens the
	// "what is actually running" question this split exists to close.
	if *f.actor == "" {
		return errors.New("nothing to place: pass --actor <dir>")
	}
	art, err := buildBundle(*f.actor, cliio.Stdout)
	if err != nil {
		return err
	}
	if _, err := pushBundle(context.Background(), f.registryAddr(), art, cliio.Stdout); err != nil {
		return err
	}
	a, err := f.args(art)
	if err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "placing %v@%v (bundle %s…) on %d machine(s) in %s\n",
		a["actorName"], a["actorVersion"], b32(a["bundleSha"].(string)), *f.count, f.fqn())
	if _, err := startOp(api, f.fqn(), "up", a); err != nil {
		return err
	}
	st, err := watchOp(api, f.fqn(), *f.timeout)
	if err != nil {
		return err
	}
	if err := printFleet(st); err != nil {
		return err
	}
	// AFTER placement, and only on request. The session follows the Worker's journals, so
	// converging it before the units exist would create windows onto nothing.
	if *f.tmux {
		return convergeSessions(st, *f.timeout)
	}
	return nil
}

func fleetPreview(args []string) error {
	f := fleetFlagSet("preview")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	api, err := infraAPI(*f.api)
	if err != nil {
		return err
	}
	a, err := f.args(nil)
	if err != nil {
		return err
	}
	if _, err := startOp(api, f.fqn(), "preview", a); err != nil {
		return err
	}
	st, err := watchOp(api, f.fqn(), *f.timeout)
	if err != nil {
		return err
	}
	if st.Result != nil {
		b, _ := json.MarshalIndent(st.Result.Changes, "", "  ")
		fmt.Fprintf(cliio.Stdout, "would change: %s\n", b)
	}
	return nil
}

func fleetStatus(args []string) error {
	f := fleetFlagSet("status")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	api, err := infraAPI(*f.api)
	if err != nil {
		return err
	}
	st, err := readOp(api, f.fqn())
	if err != nil {
		var he *httpError
		if errors.As(err, &he) && he.status == 404 {
			fmt.Fprintf(cliio.Stdout, "no fleet %s — nothing has ever been converged under this name\n", f.fqn())
			return nil
		}
		return err
	}
	fmt.Fprintf(cliio.Stdout, "%s  %s\n", st.FQN, st.Status)
	if st.Status == "RUNNING" {
		// Rejoining a converge somebody else started is the normal case now, so say so
		// rather than printing a stale inventory.
		if line := progressLine(st.Progress); line != "" {
			fmt.Fprintf(cliio.Stdout, "  in flight: %s\n", line)
		}
		return watchAndPrint(api, f.fqn(), *f.timeout)
	}
	return printFleet(st)
}

func watchAndPrint(api *apiClient, fqn string, timeout time.Duration) error {
	st, err := watchOp(api, fqn, timeout)
	if err != nil {
		return err
	}
	return printFleet(st)
}

func fleetDown(args []string) error {
	f := fleetFlagSet("down")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	api, err := infraAPI(*f.api)
	if err != nil {
		return err
	}
	tag, err := f.resolvedTag()
	if err != nil {
		return err
	}
	// ASK WHO IS HOLDING IT FIRST (ADR 0037). This was unambiguous while exactly one Run owned a
	// Fleet; a Fleet several Runs hold is one where a hand on `down` takes other people's Machines,
	// and the Run whose capacity vanished sees a queue nobody polls rather than an error. The guard
	// fails CLOSED — an unreadable **Lease** workflow stops the destroy — because "nobody holds it" and "I could
	// not find out" must not be the same value to something about to delete Machines.
	if err := refuseIfHeld(api, f.fqn(), *f.force); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "destroying %s\n", f.fqn())
	// destroy needs no args: it tears down whatever the state says exists. Sending a desired
	// state here would be the one way to destroy the WRONG thing.
	if _, err := startOp(api, f.fqn(), "destroy", map[string]any{"tag": tag, "machines": 0}); err != nil {
		return err
	}
	st, err := watchOp(api, f.fqn(), *f.timeout)
	if err != nil {
		return err
	}
	if st.Result != nil {
		b, _ := json.Marshal(st.Result.Changes)
		fmt.Fprintf(cliio.Stdout, "destroyed: %s\n", b)
	}
	return nil
}

// printFleet renders the stack outputs — the inventory is the one-way handoff Fleet gives
// Execution, so this is the whole interface between the two.
func printFleet(st *stackOpStatus) error {
	if st.Result == nil {
		fmt.Fprintf(cliio.Stdout, "%s: %s (no outputs yet)\n", st.FQN, st.Status)
		return nil
	}
	// EVERY PLACEMENT, ONE LINE EACH, because a Fleet holds several since ADR 0037's other half.
	// This printed `outputs["actorName"]` — the FIRST placement — and on a packed Fleet that is the
	// surface where a co-tenant becomes invisible: an operator reading one `actor:` line on a Fleet
	// running two would conclude the other had gone, on exactly the command they ran to check.
	//
	// THE SCALARS ARE THE FALLBACK AND ARE NOT OPTIONAL. A Fleet converged before packing echoes no
	// `placements`, and a reader that only understood the array would print NOTHING about the actor
	// on every stack this repo has ever created — a surface that goes BLANK rather than wrong, which
	// reads as "nothing is placed" on a Fleet that is working.
	placed, ok := placementsOutput(st.Result.Outputs)
	if !ok {
		placed = fallbackPlacementLine(st.Result.Outputs)
	}
	if len(placed) > 0 {
		for _, p := range placed {
			// Density is printed, not assumed: it is written into worker-<actor>.env by the
			// placement and there is no other surface that says what a Worker's cap actually is.
			density := "4 (host default)"
			if n, ok := p["maxSessions"].(int); ok && n > 0 {
				density = fmt.Sprint(n)
			}
			// So is the machine count, and for the sharper version of the same reason: a placement
			// on two of six Machines looks exactly like one on all six from every other surface.
			where := "every machine"
			if n, ok := p["workers"].(int); ok && n > 0 {
				where = fmt.Sprintf("%d machine(s)", n)
			}
			fmt.Fprintf(cliio.Stdout, "actor: %v@%v  bundle %v  sessions/worker %s  on %s\n",
				p["actorName"], p["actorVersion"], p["bundleSha"], density, where)
		}
		if len(placed) > 1 {
			fmt.Fprintf(cliio.Stdout, "  %d actors are PACKED onto these machines and share their egress "+
				"addresses (ADR 0037)\n", len(placed))
		}
	}
	raw, _ := json.Marshal(st.Result.Outputs["inventory"])
	var inv map[string]struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		PublicIP string `json:"publicIp"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(raw, &inv); err != nil || len(inv) == 0 {
		fmt.Fprintln(cliio.Stdout, "no machines")
		return nil
	}
	w := tabwriter.NewWriter(cliio.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "MACHINE\tROLE\tPRIVATE\tPUBLIC")
	for _, name := range sortedKeys(inv) {
		m := inv[name]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Name, m.Role, m.Host, m.PublicIP)
	}
	return w.Flush()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// flagPassed reports whether the operator actually typed a flag, as opposed to it holding its
// default. The difference decides whether `fleet deploy` may resize the Fleet.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
