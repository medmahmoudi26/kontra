package warden

import (
	"os"
	"strings"
	"testing"
)

// devArgs builds the run arguments without touching a daemon.
//
// THE STRUCT IS BUILT DIRECTLY, not through `NewDevDriver`, which pings docker first. Every test in
// this package does the same for the same reason: what is under test is the ARGUMENT this driver
// constructs, and a suite that needed an engine to assert a string would not run anywhere.
func devArgs(t *testing.T, spec Spec, part workerPart) []string {
	t.Helper()
	d := &devDriver{
		// `standalonePID` IS SET HERE BECAUSE `NewDevDriver` SETS IT, and this literal stands in for
		// that constructor. The field's zero value is the WARDEN's behaviour (share the Warden's PID
		// namespace) so that a plain `&dockerDriver{}` cannot silently lose the containment guarantee
		// — which means the driver that must NOT share has to say so, here as well as in the real
		// constructor. A helper that skipped it would assert against a dev driver that no production
		// path ever builds.
		dockerDriver: &dockerDriver{bin: "docker", net: "kontra", standalonePID: true},
		image:        "kontra-orchestrator:latest",
	}
	halves := map[workerPart]ProcSpec{partActor: spec.Actor, partHandler: spec.Handler}
	args, err := d.devRunArgs(spec, part, halves[part])
	if err != nil {
		t.Fatalf("devRunArgs: %v", err)
	}
	return args
}

func has(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// A serve-dev Worker is NOT in the Warden's PID namespace, and this is the regression.
//
// MEASURED, on the first real serve-dev run: `docker: Error response from daemon: No such
// container: main-droplet`. `dockerDriver.runFlags` adds `--pid container:<hostname>` so that a
// Pulumi `docker rm -f` of a MACHINE — SIGKILL, no SIGTERM, no shutdown hook — also kills the
// Workers on it. That resolves for a Warden, which is a container named after its own hostname.
// serve-dev is typed by an operator, usually from a shell on the host, where the hostname is the
// BOX and there is no container by that name; docker then refuses to start anything at all.
//
// There is no better name to look up, either: a serve-dev Worker has no Machine to be destroyed
// with, so it wants its own PID namespace. The flag is the Warden's and stays the Warden's.
func TestServeDevDoesNotJoinAnotherContainersPidNamespace(t *testing.T) {
	spec := Spec{Name: "probe", Version: "0.1.0", Actor: ProcSpec{Dir: "/w/probe", Argv: []string{"python3", "w.py"}}}
	for _, a := range devArgs(t, spec, partActor) {
		if strings.HasPrefix(a, "container:") {
			t.Fatalf("serve-dev asked to join %q — that is the Warden's flag and it cannot resolve here", a)
		}
	}
}

// The posture a serve-dev Worker runs under is the one every other container Worker runs under.
//
// SKIPPING THE TRUST POLICY IS NOT SKIPPING THE SANDBOX. serve-dev does not pull, so there is no
// registry to allow and no signature to check — but the code in the mount is still somebody's code,
// and it gets no more authority than a signed Artifact would.
func TestServeDevKeepsTheContainerPosture(t *testing.T) {
	spec := Spec{Name: "probe", Version: "0.1.0", Actor: ProcSpec{Dir: "/w/probe", Argv: []string{"python3", "w.py"}}}
	args := devArgs(t, spec, partActor)
	if !has(args, "--security-opt", "no-new-privileges") {
		t.Error("no-new-privileges is missing: a Worker could regain privileges through a setuid binary")
	}
	if !has(args, "--cap-drop", "ALL") {
		t.Error("--cap-drop ALL is missing")
	}
	if !has(args, "--network", "kontra") {
		t.Error("the Worker is not on the control plane's network, so `temporal:7233` will not resolve")
	}
}

// The folder is mounted AT ITSELF, and the whole point is that one path means one thing.
//
// docker-compose.yml mounts workspaces the same way and says why: "the control plane hands paths to
// the CLI, and a workspace at a different path in the container turns every one of those into a
// directory nobody can find". It is also what makes a stack trace out of a serve-dev Worker name a
// file the operator can open.
func TestServeDevMountsTheFolderAtItself(t *testing.T) {
	spec := Spec{Name: "probe", Version: "0.1.0", Actor: ProcSpec{Dir: "/w/probe", Argv: []string{"python3", "w.py"}}}
	if !has(devArgs(t, spec, partActor), "--volume", "/w/probe:/w/probe") {
		t.Error("the served folder is not bind-mounted at its own path")
	}
}

// logship selects by LABEL, so a Worker without one is a Worker with no logs anywhere.
//
// `KONTRA_WORKER` is what `control/images/logship.sh` filters on and what `list` reconstructs a
// Worker from; `KONTRA_DEV` is what separates an operator's dev Worker — holding a mount of their
// editor directory — from a placed Artifact.
func TestServeDevIsLabelledForLogshipAndForAReaper(t *testing.T) {
	spec := Spec{Name: "probe", Version: "0.1.0", Actor: ProcSpec{Dir: "/w/probe", Argv: []string{"python3", "w.py"}}}
	args := devArgs(t, spec, partActor)
	for _, want := range []string{workerLabelVar + "=probe/0.1.0/actor", devLabelVar + "=1", "kontra.logs=true"} {
		if !has(args, "--label", want) {
			t.Errorf("label %q is missing — %v", want, args)
		}
	}
}

// A workflow Worker is ONE process, and the driver must not invent a second container for the half
// that does not exist: an empty one would exit immediately and read as a crash-looping handler.
func TestServeDevStartsOnlyTheHalvesThatHaveSomethingToRun(t *testing.T) {
	spec := Spec{Name: "hunt", Actor: ProcSpec{Dir: "/w/hunt", Argv: []string{"kontra", "workflow", "serve", "workflow.py"}}}
	if len(spec.Handler.Argv) != 0 {
		t.Fatal("this case is about a spec with no handler")
	}
	args := devArgs(t, spec, partActor)
	if args[len(args)-1] != "workflow.py" {
		t.Errorf("the argv did not survive into the container command: %v", args)
	}
}

// THE ZERO VALUE OF THE DRIVER SHARES THE WARDEN'S PID NAMESPACE.
//
// This is a regression test for a field, not for a feature. The flag started life as `sharePID`,
// which meant a bare `&dockerDriver{…}` — the shape `warden_local_test.go` uses — defaulted to NOT
// sharing, quietly dropping the containment property that makes destroying a Machine kill the
// Workers on it. The field is now `standalonePID` so the zero value is the Warden's behaviour and
// only serve-dev opts out. If anyone flips the sense back, this fails before the safety does.
func TestDockerZeroValueSharesTheWardenPIDNamespace(t *testing.T) {
	d := &dockerDriver{bin: "docker", net: "kontra"}
	spec := Spec{
		Name:    "probe",
		Version: "0.1.0",
		Image:   "registry:5000/probe@sha256:" + strings.Repeat("b", 64),
		Actor:   ProcSpec{Env: []string{"KONTRA_ADDRESS=temporal:7233"}},
		Handler: ProcSpec{Env: []string{"KONTRA_ADDRESS=temporal:7233"}},
	}
	args, err := d.runImageArgs("kontra", spec, spec.Image)
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname on this box")
	}
	if !containsValue(args, "--pid", "container:"+host) {
		t.Fatalf("a zero-value dockerDriver must share the Warden PID namespace, got %s", strings.Join(args, " "))
	}
}
