package warden

// driver_dev.go — `dev`: serve-dev, the fourth place a Worker runs and the only one that is a
// CHECKOUT rather than an Artifact.
//
// ═══ WHAT IT IS FOR ═══
//
// The other three drivers place code somebody PUBLISHED. `podman` and `docker` pull a digest-pinned,
// signed image through a registry allowlist; `process` runs a checkout directly and ADR 0036 scopes
// it to "the single box and the airgapped install". serve-dev is the case none of those fit: an
// operator iterating on the folder open in their editor, who wants the Worker restarted in seconds
// and does not want to publish an image between every edit.
//
// So the Worker runs in a CONTAINER — ADR 0036's rule holds, one Target and it is a container — but
// the image is THE CONTROL PLANE'S OWN and the code arrives on a bind mount. That is the whole
// trade, stated plainly:
//
//	          image                     code            trust question
//	podman    a signed Artifact         in the image    who produced these bytes
//	docker    a signed Artifact         in the image    who produced these bytes
//	dev       the control plane's own   a bind mount    none — it is the operator's own disk
//	process   n/a                       a checkout      none, and no container either
//
// ═══ WHY IT DOES NOT GO THROUGH THE TRUST POLICY, AND WHY THAT IS NOT A HOLE ═══
//
// `trustpolicy.Policy` answers "who produced this image, and may this Machine pull from there". Both
// halves are meaningless here: nothing is pulled, and the image is the one this process is already
// running from. Sending serve-dev through `Admit` would mean signing the control-plane image and
// allowlisting a registry in order to run a file the operator just saved — ceremony that protects
// nothing, because the thing being run is the mount and the mount is not signed either.
//
// The bound that DOES apply is the one this driver keeps: a served folder is the operator's own, and
// a Worker started from it gets no more authority than one started from an Artifact. Same
// `no-new-privileges`, same `--cap-drop ALL`, same {@link assertNoRuntimeAccess} refusal — a
// serve-dev Worker cannot reach the container runtime, so it cannot start `-v /:/host` and own the
// box. That check is the reason this file builds its flags through `dockerDriver.runFlags` instead
// of assembling its own.
//
// ═══ THE MOUNT IS THE SAME PATH INSIDE AND OUT ═══
//
// `Dir` is a HOST path — the daemon resolves a bind source on the host however the caller is
// containerised — and it is mounted at itself. docker-compose.yml already mounts workspaces that way
// and says why: "the control plane hands paths to the CLI, and a workspace at a different path in
// the container turns every one of those into a directory nobody can find". The same rule makes a
// stack trace out of a serve-dev Worker name a file the operator can open.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
)

// devImageVar names the image serve-dev runs. It is the control plane's own, and compose sets the
// same variable on every service built from it, so a checkout running a locally-built image and one
// running a release agree without anybody passing a flag.
const devImageVar = "KONTRA_ORCHESTRATOR_IMAGE"

const defaultDevImage = "kontra-orchestrator:latest"

// devLabelVar marks a container serve-dev owns.
//
// SEPARATE FROM `KONTRA_WORKER`, WHICH IT ALSO CARRIES. The worker label is what logship selects on
// (`control/images/logship.sh`) and what `list` reconstructs a Worker from, so a serve-dev Worker
// must have it or it becomes a Worker with no logs and no listing. This second label is what tells
// a reader — and a reaper — that the thing is a DEV worker holding a mount of somebody's editor
// directory, not a placed Artifact. A Machine's Workers and an operator's must be separable at a
// glance; without it the only difference is the image tag.
const devLabelVar = "KONTRA_DEV"

// devDriver is `dev`. It IS a dockerDriver in everything but where the image comes from and what is
// mounted into it, so it embeds one rather than restating `Stop`, `list` and `logs` — all three are
// written against the label scheme, which serve-dev shares.
type devDriver struct {
	*dockerDriver
	// image is resolved once at construction so every half of a pair runs the same bytes even if the
	// variable changes underneath a long-lived process.
	image string
}

var _ workerDriver = (*devDriver)(nil)

func (d *devDriver) driverName() string { return "dev" }

// NewDevDriver builds the serve-dev driver.
//
// THE EMBEDDED DRIVER GETS THE ZERO POLICY, WHICH ADMITS NOTHING. serve-dev never pulls, so there
// is no registry to allow and no signature to check, and {@link devDriver.Start} does not call
// `Admit` at all. The zero value is passed rather than a permissive one precisely because of that:
// if a future change ever routes serve-dev through `dockerDriver.Start`, it refuses instead of
// pulling an unsigned image from anywhere. A gate whose unconfigured state is "allow" is not a gate
// (trustpolicy.go's own words); this one's unconfigured state is "nothing".
func NewDevDriver(ctx context.Context) (*devDriver, error) {
	base, err := newDockerDriver(ctx, trustpolicy.Policy{})
	if err != nil {
		return nil, err
	}
	image := strings.TrimSpace(os.Getenv(devImageVar))
	if image == "" {
		image = defaultDevImage
	}
	// NOT IN THE WARDEN'S PID NAMESPACE — see `dockerDriver.runFlags`. A Warden shares it so that
	// destroying the Machine kills the Workers on it; serve-dev has no Machine, and from a shell on
	// the host there is no container to join.
	base.standalonePID = true
	return &devDriver{dockerDriver: base, image: image}, nil
}

// Start runs the pair as two containers on the control-plane network, each holding a bind mount of
// the folder its half runs from.
//
// TWO CONTAINERS AND NOT ONE, matching `dockerDriver`. The halves have different working directories
// and different environments — the actor gets PYTHONPATH, the handler gets GOWORK=off — and folding
// them into one container would mean one of them running with the other's. `whole()` then means what
// it means everywhere else: a pair with one half gone comes back with one entry.
func (d *devDriver) Start(ctx context.Context, spec Spec) (workerHandle, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return workerHandle{}, fmt.Errorf("serve-dev needs a worker name")
	}
	// REPLACE, AND THIS IS THE ONE PLACE serve-dev DIVERGES FROM THE SEAM'S CONTRACT. driver.go says
	// `start` "is NOT idempotent and does not check whether the Worker is already running, because
	// the check that would be worth making is `list`". That is right for a Machine, where two callers
	// may be racing to place the same Artifact and the reconcile loop is the arbiter. It is wrong
	// here: there is one caller, it is a person who just saved a file, and the only thing a running
	// pair from the previous edit can do is hold the name and serve stale code. So re-running the
	// command IS the reload, and that is the sentence the CLI prints.
	//
	// DESTRUCTIVE AND DELIBERATELY SO — anything in flight on the old pair dies with it. A serve-dev
	// Worker is an editor loop, not a placement; `--mode docker` is the one with the other semantics.
	d.removePair(ctx, spec.Name, spec.Version)

	h := workerHandle{Driver: d.driverName(), Name: spec.Name, Version: spec.Version}
	halves := map[workerPart]ProcSpec{partActor: spec.Actor, partHandler: spec.Handler}
	for _, part := range workerParts {
		p := halves[part]
		// A HALF WITH NO ARGV IS NOT STARTED, which is how a workflow Worker — one process, not a
		// pair — reaches this driver without inventing an empty handler container that would exit
		// immediately and read as a crash-looping half.
		if len(p.Argv) == 0 {
			continue
		}
		args, err := d.devRunArgs(spec, part, p)
		if err != nil {
			d.removePair(ctx, spec.Name, spec.Version)
			return workerHandle{}, err
		}
		out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput()
		if err != nil {
			d.removePair(ctx, spec.Name, spec.Version)
			return workerHandle{}, fmt.Errorf("serve-dev %s: %v: %s", string(part), err, strings.TrimSpace(string(out)))
		}
		h.Halves = append(h.Halves, workerHalf{Part: part, Ref: cliutil.FirstLine(string(out))})
	}
	if len(h.Halves) == 0 {
		return workerHandle{}, fmt.Errorf("serve-dev %s: neither half has anything to run", spec.Name+"@"+spec.Version)
	}
	return h, nil
}

// devRunArgs is `dockerDriver.runArgs` with the image replaced and the source folder mounted.
//
// THE DIGEST FIELD IS THE IMAGE REFERENCE, not a digest, and it is passed through to
// `KONTRA_ACTOR_DIGEST` exactly as the other drivers pass a real one. A serve-dev Worker reporting
// `kontra-orchestrator:latest` where a placed one reports `sha256:…` is the truth: the bytes are
// whatever that tag pointed at when the container started, and nothing here can say more.
func (d *devDriver) devRunArgs(spec Spec, part workerPart, p ProcSpec) ([]string, error) {
	net := d.net
	if net == "" {
		net = "kontra"
	}
	flags := d.withProc(d.runFlags(
		net,
		dockerContainer(spec.Name, spec.Version, part),
		workerLabel(spec.Name, spec.Version, part),
		d.image,
		[]string{devLabelVar + "=1", "kontra.logs=true"},
	), p)
	if p.Dir != "" {
		// AT ITSELF. See this file's header: a path that means two things is a path nobody can open.
		flags = append(flags, "--volume", p.Dir+":"+p.Dir)
	}
	if err := assertNoRuntimeAccess(flags); err != nil {
		return nil, err
	}
	args := append([]string{}, flags...)
	args = append(args, "--entrypoint", p.Argv[0])
	args = append(args, d.image)
	if len(p.Argv) > 1 {
		args = append(args, p.Argv[1:]...)
	}
	return args, nil
}

// Remove stops and deletes a serve-dev pair by NAME, whether or not this process started it.
//
// BY NAME AND NOT BY HANDLE, which is what makes it usable as a stop verb. A handle comes from a
// `Start` this process performed; an operator typing `--replicas 0` in a new shell has none, and the
// runtime is the truth (driver.go's own rule). The container names are derived from the same
// `dockerContainer` the start used, so the two cannot disagree.
//
// SILENT ON A NAME THAT IS NOT RUNNING. Stopping something already stopped is the state the caller
// asked for, not a failure to report.
func (d *devDriver) Remove(ctx context.Context, name, version string) {
	d.removePair(ctx, name, version)
}

// Pause and Unpause freeze and thaw a serve-dev pair.
//
// THE SAME SEMANTIC THE PROCESS PATH HAD, THROUGH THE RUNTIME INSTEAD OF A PID. `workflow pause`
// used to be SIGSTOP on a recorded pid; `docker pause` is SIGSTOP on every process in the container,
// applied through the freezer cgroup so nothing can escape it by forking. What the verb MEANS is
// unchanged and so is the caveat the CLI prints: the Worker stops polling, the run makes no
// progress, and activities already dispatched keep running with their timers ticking.
//
// AND IT STILL CANNOT DRIFT, which was the reason for stopping rather than respawning. The tmux
// implementation killed the pane and re-ran the file, which re-derived the task queue from the
// folder's content digest — so an edit between pause and resume moved the queue and the "resumed"
// worker polled one the paused run was not waiting on. A frozen container is the same container
// holding the code it imported at boot, on the queue it has polled since.
//
// EVERY NAME, AND FAILURES ARE COLLECTED RATHER THAN RETURNED AT THE FIRST ONE: a pair with one half
// already paused must still pause the other, or the verb leaves the Worker half-frozen.
func (d *devDriver) Pause(ctx context.Context, name, version string) error {
	return d.freeze(ctx, "pause", name, version)
}

func (d *devDriver) Unpause(ctx context.Context, name, version string) error {
	return d.freeze(ctx, "unpause", name, version)
}

func (d *devDriver) freeze(ctx context.Context, verb, name, version string) error {
	var failed []string
	ran := 0
	for _, part := range workerParts {
		c := dockerContainer(name, version, part)
		out, err := exec.CommandContext(ctx, d.bin, verb, c).CombinedOutput()
		if err != nil {
			// NOT RUNNING IS NOT A FAILURE TO REPORT HERE. A workflow Worker is one half, so the
			// other name never existed; saying so would make every `pause` of one look broken.
			if strings.Contains(string(out), "No such container") {
				continue
			}
			failed = append(failed, c+": "+strings.TrimSpace(string(out)))
			continue
		}
		ran++
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s %s: %s", verb, name, strings.Join(failed, "; "))
	}
	if ran == 0 {
		return fmt.Errorf("nothing to %s: no serve-dev container for %s — `kontra workers list` shows what is running", verb, name)
	}
	return nil
}
