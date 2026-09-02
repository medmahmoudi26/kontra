package main

// orchestrator.go — WHICH orchestrator `kontra up` runs, and the environment it runs in
// (ADR 0031 §1, issue 14).
//
// THE TRAP THIS FILE EXISTS TO NOT REPRODUCE. `kontra infra up` reverts a container to its image
// and says nothing, which silently undoes a locally built API; the equivalent here would be a
// binary that hydrates a bundle over a developer's working orchestrator, or — the same mistake
// from the other side — one that runs a two-week-old bundle while a freshly compiled
// `control/orchestrator/dist` sits in the checkout it was started from. Both are the same bug: the
// process is healthy, the surfaces are up, and the code running is not the code you changed.
//
// SO THE CHOICE IS EXPLICIT, IT PREFERS WHAT YOU BUILT, AND IT ALWAYS SAYS WHICH. In a checkout
// with compiled output and a Node on PATH, `kontra up` runs THAT and prints the path it ran, plus
// the bundle it did not use and the flag that would force it. Anywhere else — an installed
// binary, a machine with no source — there is nothing to prefer and the bundle is the only
// answer. `--orchestrator` overrides the whole decision with a word or a path.
//
// AND THE ADDRESSES ARE FACTS, NOT PREFERENCES. Everything the child needs to reach is inside
// this process: Temporal, the object store, the state store, the data directory. An inherited
// `KONTRA_ADDRESS` pointing at a compose stack is exactly how a control plane ends up serving one
// installation's SPA over another installation's history, so those four are OVERRIDDEN and the
// override is reported when it changed something.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	applbundle "github.com/medmahmoudi26/kontra/cli/appliance/bundle"
	"github.com/medmahmoudi26/kontra/handler/hydratestore"
)

// DefaultOrchestratorPort is the API's port, and it is 8088 because that is the number in every
// bookmark, every `KONTRA_ORCHESTRATOR_URL` default and the compose service this replaces.
const DefaultOrchestratorPort = 8088

// orchestratorSource is a resolved answer to "what is `kontra up` about to exec".
type orchestratorSource struct {
	// Kind is "bundle", "local" or "none".
	Kind string

	// Chose is the sentence printed on every start: what was picked, and why it rather than the
	// other one. It is not decoration — it is the acceptance criterion.
	Chose string
	// Instead is what was available and NOT used, with the flag that would pick it. Empty when
	// there was no second option.
	Instead string

	Node  string // the interpreter, absolute
	Entry string // the script, absolute
	Dir   string // the child's working directory

	Digest    string
	SPARoot   string
	SPADigest string
	Fresh     bool
}

// orchestratorOptions is what resolveOrchestrator needs from the command.
type orchestratorOptions struct {
	// Mode is the --orchestrator flag: "", "auto", "bundle", "local", "none", or a path to
	// either a bundle archive or a checkout's orchestrator directory.
	Mode string

	DataDir  string
	Progress io.Writer

	// RepoRoot is the checkout, when there is one. Empty means the binary was started somewhere
	// without a checkout above it, which is the ordinary installed case and not an error.
	RepoRoot string

	// Store is the appliance's already-open artifact store, so hydration and the embedded
	// registry share one set of bytes on disk (ADR 0031 §2: do not build a second store).
	Store *hydratestore.Store
}

// resolveOrchestrator decides what to run and gets it ready to run.
func resolveOrchestrator(ctx context.Context, opts orchestratorOptions) (*orchestratorSource, error) {
	mode := strings.TrimSpace(opts.Mode)
	if mode == "" {
		mode = envOr("KONTRA_ORCHESTRATOR", "")
	}

	switch mode {
	case "none":
		// WHAT IT COSTS, SAID HERE RATHER THAN DISCOVERED AS A HANG. The five embedded services
		// are a complete control plane for a dispatch — MEASURED: an actor Method runs, isolates,
		// commits and stores its blobs against this — and NOT for a caller that materializes.
		// `Batch.rows()` schedules `resolveBatch` on `kontra-datasets`, `dataset(name)` publishes
		// through the same role, and with no orchestrator nothing polls those queues, so the
		// caller waits until its own timeout with no error anywhere. That is the failure this
		// sentence exists to pre-empt.
		return &orchestratorSource{
			Kind: "none",
			Chose: "not started (--orchestrator=none): five embedded services only — no API, no SPA, " +
				"and no materializer, so `rows()` and named Datasets have nothing polling kontra-datasets",
		}, nil
	case "", "auto":
		return resolveAuto(ctx, opts)
	case "local":
		return resolveLocal(opts, opts.RepoRoot, "asked for with --orchestrator=local")
	case "bundle":
		path, spa, err := findBundles(opts)
		if err != nil {
			return nil, err
		}
		return hydrateSource(ctx, opts, path, spa, "asked for with --orchestrator=bundle")
	}

	// A PATH. Which of the two it is, is a property of the thing itself rather than of a second
	// flag: an archive is a bundle and a directory is a build.
	info, err := os.Stat(mode)
	if err != nil {
		return nil, fmt.Errorf("--orchestrator %s: %w (want a word — auto, local, bundle, none — a bundle .tar.gz, or a built orchestrator directory)", mode, err)
	}
	if info.IsDir() {
		return resolveLocal(opts, repoOfOrchestratorDir(mode), "asked for with --orchestrator "+mode)
	}
	_, spa, _ := findBundles(opts)
	return hydrateSource(ctx, opts, mode, spa, "asked for with --orchestrator "+mode)
}

// resolveAuto is the unattended decision, and the whole reason this file has a header.
func resolveAuto(ctx context.Context, opts orchestratorOptions) (*orchestratorSource, error) {
	local, localErr := resolveLocal(opts, opts.RepoRoot, "")
	bundle, spa, bundleErr := findBundles(opts)

	if localErr == nil {
		local.Chose = fmt.Sprintf("local build at %s — this is a checkout and it is compiled, so `kontra up` runs what you built", filepath.Dir(local.Entry))
		switch {
		case bundleErr == nil:
			local.Instead = fmt.Sprintf("the bundle %s was NOT hydrated; run it with --orchestrator=bundle", bundle)
		default:
			local.Instead = "there is no bundle here; build one with `kontra bundle orchestrator`"
		}
		return local, nil
	}
	if bundleErr == nil {
		src, err := hydrateSource(ctx, opts, bundle, spa, "")
		if err != nil {
			return nil, err
		}
		src.Instead = "no local build was usable here: " + localErr.Error()
		return src, nil
	}

	// NEITHER, and the message has to carry both halves — an operator on an installed binary and
	// a developer in a checkout have different fixes and the same symptom.
	return nil, fmt.Errorf("no orchestrator to run.\n"+
		"  bundle: %v\n"+
		"  local:  %v\n"+
		"  Build a bundle with `kontra bundle orchestrator` (and `kontra bundle spa`), point at one\n"+
		"  with --orchestrator <file.tar.gz>, or start without a control plane using --orchestrator=none",
		bundleErr, localErr)
}

// resolveLocal points at a checkout's compiled output.
//
// IT DOES NOT COMPILE ANYTHING. `pnpm exec tsc` is the developer's verb and it belongs in their
// hands: a `kontra up` that silently rebuilt would be slow, would sometimes fail for reasons that
// have nothing to do with starting a control plane, and would make "what is running" depend on
// when you last started it rather than on what you last built.
func resolveLocal(opts orchestratorOptions, repo, chose string) (*orchestratorSource, error) {
	if repo == "" {
		return nil, errors.New("no checkout here")
	}
	dir := filepath.Join(repo, "control", "orchestrator")
	entry := filepath.Join(dir, "dist", "src", "main.js")
	if _, err := os.Stat(entry); err != nil {
		return nil, fmt.Errorf("%s is not compiled (no dist/src/main.js; run `pnpm --dir %s exec tsc`)", dir, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		return nil, fmt.Errorf("%s has no node_modules (run `pnpm --dir %s install`)", dir, dir)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("no `node` on PATH to run %s with", entry)
	}
	src := &orchestratorSource{Kind: "local", Node: node, Entry: entry, Dir: dir, Chose: chose}
	// The SPA a local build serves is a built console on disk — `server.ts:defaultWebRoot` looks in
	// the same places this does, and the two must agree: a probe that answers "no SPA" about a
	// checkout that has one is a console that boots, serves its API and renders nothing.
	//
	// THE SPA IS IN ANOTHER REPOSITORY NOW (ADR 0038). It was `frontend/dist` here, and before that
	// `orchestrator/web` — each move broke this line, so the candidates live in one function
	// (`applbundle.ConsoleDist`) rather than being spelled out at each site.
	if spa, _ := applbundle.ConsoleDist(repo); spa != "" {
		src.SPARoot = spa
	}
	return src, nil
}

// hydrateSource hydrates a bundle and describes what came out.
//
// THE DESCRIPTION IS BUILT HERE, not by the caller, because the caller knows why it chose this
// artifact and only this function knows WHICH artifact that turned out to be. A line that says
// "asked for with --orchestrator=bundle" and not which bundle, at which digest, is the same
// omission as a deploy that reports success without a version.
func hydrateSource(ctx context.Context, opts orchestratorOptions, bundle, spa, why string) (*orchestratorSource, error) {
	h, err := applbundle.HydrateOrchestrator(ctx, applbundle.HydrateOptions{
		DataDir:  opts.DataDir,
		Bundle:   bundle,
		SPA:      spa,
		Store:    opts.Store,
		Progress: opts.Progress,
	})
	if err != nil {
		return nil, err
	}
	chose := fmt.Sprintf("bundle %s (%s) at %s", filepath.Base(bundle), shortDigest(h.Digest), h.Root)
	if why != "" {
		chose += " — " + why
	}
	if h.Fresh {
		chose += " — hydrated just now"
	}
	return &orchestratorSource{
		Kind:      "bundle",
		Chose:     chose,
		Node:      h.Node,
		Entry:     h.Entry,
		Dir:       h.Root,
		Digest:    h.Digest,
		SPARoot:   h.SPARoot,
		SPADigest: h.SPADigest,
		Fresh:     h.Fresh,
	}, nil
}

// findBundles locates the two archives.
//
// THREE PLACES, IN THIS ORDER, and each one answers a different question. The environment is an
// operator pinning an exact file. The checkout's `build/bundles` is where `kontra bundle` writes,
// so a developer who just built one needs to name nothing. `$KONTRA_HOME/bundles` is where an
// INSTALLED binary looks, because there is no checkout on that machine at all.
func findBundles(opts orchestratorOptions) (bundle, spa string, err error) {
	name := fmt.Sprintf("kontra-orchestrator-%s-%s-%s.tar.gz", applbundle.NodeVersion, runtime.GOOS, runtime.GOARCH)
	var tried []string

	for _, dir := range bundleSearchPath(opts.RepoRoot) {
		tried = append(tried, filepath.Join(dir, name))
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			bundle = filepath.Join(dir, name)
			break
		}
	}
	if v := strings.TrimSpace(os.Getenv("KONTRA_ORCHESTRATOR_BUNDLE")); v != "" {
		bundle = v
	}
	if bundle == "" {
		return "", "", fmt.Errorf("no %s in %s", name, strings.Join(tried, " or "))
	}

	// The SPA is optional and looked for beside whatever bundle was found first, then along the
	// same search path. A control plane with no SPA serves its API and says the surfaces are
	// missing; that is a degraded start, not a failed one.
	for _, dir := range append([]string{filepath.Dir(bundle)}, bundleSearchPath(opts.RepoRoot)...) {
		cand := filepath.Join(dir, applbundle.SPAName+".tar.gz")
		if _, err := os.Stat(cand); err == nil {
			spa = cand
			break
		}
	}
	if v := strings.TrimSpace(os.Getenv("KONTRA_SPA_BUNDLE")); v != "" {
		spa = v
	}
	return bundle, spa, nil
}

func bundleSearchPath(repo string) []string {
	var dirs []string
	if repo != "" {
		dirs = append(dirs, filepath.Join(repo, "build", "bundles"))
	}
	if root, err := kontraRoot(); err == nil {
		dirs = append(dirs, filepath.Join(root, "bundles"))
	}
	return dirs
}

// repoOfOrchestratorDir turns `--orchestrator <path>/orchestrator` back into the checkout the
// rest of the resolution wants. Accepts the checkout itself too, because typing either is the
// same intent and refusing one of them would be a rule with no purpose.
// statOK is "this path exists", for the several places that ask without caring why not.
func statOK(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func repoOfOrchestratorDir(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	// `control/orchestrator`, so the repo root is TWO segments up rather than one. It was
	// `backend` (one), and before that `orchestrator` at the root (zero) — which is why this
	// climbs by matching the tail rather than by a fixed count.
	if filepath.Base(abs) == "orchestrator" && filepath.Base(filepath.Dir(abs)) == "control" {
		return filepath.Dir(filepath.Dir(abs))
	}
	return abs
}

// --- the child's environment -------------------------------------------------------------------

// orchestratorEnvOptions are the appliance's own facts, which the child must be told.
type orchestratorEnvOptions struct {
	TemporalAddress string
	S3Endpoint      string
	KVAddress       string
	DataDir         string
	Port            int
	KontraHome      string
	Roles           string

	// BindIP is the address `kontra up` was told to bind, handed on so the child's HTTP listener
	// is not the one surface that ignores it. See the KONTRA_ORCHESTRATOR_BIND default below.
	BindIP string

	// S3Public is `--s3-public-endpoint`, and it is empty unless the operator typed it. Presigned
	// URLs are signed FOR a host — `kontra explore` hands one to a workstation — and a signature
	// computed for the wrong host is invalid there, so this is a value with three sources in
	// precedence order: the flag, then whatever the environment already said, then the address
	// the store actually bound. Only the first is a fact about this process.
	S3Public string
	// S3Bound is that third answer.
	S3Bound string
}

// orchestratorEnv builds the child's environment from the parent's.
//
// INHERIT, THEN OVERRIDE, AND THE SPLIT IS THE POINT. Everything the operator set is passed
// through — tokens, OTEL variables, KONTRA_CONTROLLER, a proxy — because those are their
// deployment's choices and this process has no better answer. The addresses are not choices: they
// are where THIS process bound its listeners, and an inherited value naming a compose stack is
// how a control plane ends up reading one installation's history while serving another's SPA.
//
// It returns the overrides it actually changed, so `kontra up` can say so. A silent override is
// the same class of bug as a silent revert.
func orchestratorEnv(base []string, opts orchestratorEnvOptions) (env []string, replaced []string) {
	inherited := map[string]string{}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			inherited[k] = v
		}
	}

	// KONTRA_ORCHESTRATOR_DB IS A FACT FOR A REASON WORTH STATING: the value an operator is most
	// likely to have inherited is `/data/orchestrator.db`, the CONTAINER path this file's
	// predecessor used. On the host that directory does not exist, and a control plane that
	// cannot open its run-record database fails at boot for a reason that reads as a permissions
	// problem.
	facts := map[string]string{
		"KONTRA_ADDRESS":         opts.TemporalAddress,
		"KONTRA_S3_ENDPOINT":     opts.S3Endpoint,
		"KONTRA_REDIS_HOST":      opts.KVAddress,
		"KONTRA_DATA_DIR":        opts.DataDir,
		"KONTRA_ORCHESTRATOR_DB": filepath.Join(opts.DataDir, "orchestrator.db"),
	}
	if opts.S3Public != "" {
		facts["KONTRA_S3_PUBLIC_ENDPOINT"] = opts.S3Public
	}
	// DEFAULTS, NOT FACTS. These have a right answer for the appliance and a legitimate reason to
	// be set differently: a deployment may serve the API on another port, and one that keeps a
	// compose `orchestrator-infra` beside this process MUST leave the infra role out of it (ADR
	// 0034 §1) — which is exactly what setting KONTRA_ORCHESTRATOR_ROLES does.
	defaults := map[string]string{
		"KONTRA_S3_PUBLIC_ENDPOINT": opts.S3Bound,
		"KONTRA_ORCHESTRATOR_PORT":  fmt.Sprint(opts.Port),
		"KONTRA_ORCHESTRATOR_ROLES": opts.Roles,
		"KONTRA_HOME":               opts.KontraHome,
		// THE API'S BIND ADDRESS, and until issue 18 it was the one surface `--bind` did not
		// reach. The five embedded services honour it; the child listened on `0.0.0.0` because
		// that is what it did as a container, where a published port was the control. MEASURED on
		// a droplet with a public interface: `kontra up --bind 172.17.0.1` left an
		// unauthenticated orchestrator API answering on every address the box has — which is
		// exactly the exposure ADR 0031 §3 claims loopback removes, on the one service the claim
		// did not cover.
		//
		// A DEFAULT AND NOT A FACT, because there is one deployment where `0.0.0.0` is still
		// right and it is not hypothetical: docker-compose.yml describes relocating the API role
		// to a worker host as a container of `kontra-orchestrator:latest`, and a process inside a
		// container that binds its host's loopback is unreachable through its own published port.
		// An operator who sets this keeps it; `server.ts` still falls back to `0.0.0.0` when
		// nobody says, so nothing that runs the image today changes.
		"KONTRA_ORCHESTRATOR_BIND": opts.BindIP,
		// What a worker started by the Workflows page's Serve button must be told, because it
		// runs on this host and cannot inherit an address that only means something in here. The
		// compose file computes the same string from its own published ports.
		"KONTRA_SERVE_ENV": strings.Join([]string{
			"KONTRA_ADDRESS=" + opts.TemporalAddress,
			"KONTRA_S3_ENDPOINT=" + opts.S3Endpoint,
			fmt.Sprintf("KONTRA_ORCHESTRATOR_URL=http://127.0.0.1:%d", opts.Port),
			"KONTRA_REDIS_HOST=" + opts.KVAddress,
		}, " "),
	}

	out := map[string]string{}
	for k, v := range inherited {
		out[k] = v
	}
	for k, v := range facts {
		if v == "" {
			continue
		}
		if old, ok := inherited[k]; ok && old != v {
			replaced = append(replaced, fmt.Sprintf("%s=%s (was %s)", k, v, old))
		}
		out[k] = v
	}
	for k, v := range defaults {
		if v == "" {
			continue
		}
		if _, ok := inherited[k]; !ok {
			out[k] = v
		}
	}

	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+out[k])
	}
	sort.Strings(replaced)
	return env, replaced
}

// printOrchestratorChoice is the line that makes the decision auditable.
func printOrchestratorChoice(w io.Writer, src *orchestratorSource) {
	fmt.Fprintf(w, "%-22s %s\n", "Orchestrator:", src.Chose)
	if src.Instead != "" {
		fmt.Fprintf(w, "%-22s %s\n", "", src.Instead)
	}
	switch {
	case src.SPARoot == "" && src.Kind != "none":
		fmt.Fprintf(w, "%-22s none — the API will serve no pages. Build one with `kontra bundle spa`\n", "SPA:")
	case src.SPADigest != "":
		fmt.Fprintf(w, "%-22s %s (%s)\n", "SPA:", src.SPARoot, shortDigest(src.SPADigest))
	case src.SPARoot != "":
		fmt.Fprintf(w, "%-22s %s\n", "SPA:", src.SPARoot)
	}
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
