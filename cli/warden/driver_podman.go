package warden

// driver_podman.go — `podman`: the driver that runs a Worker's two halves in containers.
//
// This is the reversal ADR 0036 decides, arriving on a **Machine** for the first time. Until now a
// fleet Machine ran the actor NATIVELY, and `infra/README.md` argued it well: "the Machine is
// already the isolation boundary (one Worker per Machine, for its unique egress address)". That
// premise held exactly while every actor was first-party. Once an actor is a stranger's code, the
// thing sharing a kernel with it is kontra's own handler, its Temporal credential and its
// object-store keys — so the Machine is a boundary between *Workers* and was never a boundary
// between *authors*.
//
// What the container buys is bounded and this file does not overclaim it. 0036: "Docker is not a
// security boundary, and this ADR does not claim it is. A shared kernel is a shared kernel." The two
// properties it DOES buy are the two this file enforces and driver_podman_test.go asserts against a
// live runtime rather than against an argv:
//
//  1. THE WORKLOAD IS NOT HOST ROOT. Measured while writing this file, on this checkout's box: a
//     container started with no user namespace wrote a file into a bind mount owned by host `0:0`;
//     the same container under a namespace wrote it owned by host `524288:524288`. That is the whole
//     difference between "contained" and "root with extra steps", it is one flag wide, and it is why
//     `start` REFUSES rather than degrades when it cannot get one.
//
//  2. THE WORKLOAD CANNOT REACH THE CONTAINER RUNTIME. Mounting the runtime socket is not a
//     weakening of the boundary, it is the deletion of it: a process that can talk to podman can
//     start a container with `-v /:/host` and own the Machine. Both sockets on this box answer a
//     dial from the host, which is what makes the negative assertion in the test worth anything.
//
// ═══ THE PAIR IS A POD ═══
//
// driver.go's header says the unit is the PAIR and points here: "Slice 02's podman driver has the
// same shape available in a pod." It is taken, and for a reason beyond tidiness — the grouping is
// the RUNTIME'S rather than one this driver invents. tmux has the session, podman has the pod, and a
// driver that reconstructed the pair by string-matching labels on every `list` would be inventing a
// third.
//
// The pod earns its keep twice more:
//
//   - ONE NETWORK NAMESPACE PER WORKER, which is the unit slice 12's egress policy needs and the
//     unit `infra/CONTEXT.md` already assumes: "on the fleet it hosts exactly one [Worker], because
//     each needs its own egress address". A pair in two unrelated containers would have two.
//   - ONE USER NAMESPACE PER WORKER. `podman pod create --userns=auto` allocates the range once and
//     both halves join it, so the actor and the handler are mapped together and neither can reach
//     the other's files as root. Verified: a container joining such a pod is uid 0 inside and
//     524288 on the host.
//
// The two halves do NOT talk to each other — both dial OUT to the Controller for Temporal, Redis, S3
// and the catalog (infra/worker-entrypoint.sh) — so the shared netns is not there to connect them.
// It is there to be the one thing egress policy can be attached to.
//
// ═══ TWO CONTAINERS, NOT ONE, AND THAT IS A CHANGE ═══
//
// `infra/worker-entrypoint.sh` runs both halves in ONE container behind a shell supervisor: "TWO
// processes, one container", with a `while kill -0` loop that exits non-zero "the moment either dies
// so the container restarts". That shape cannot express the state this seam exists to report.
// driver.go: a handle "carries the halves the runtime actually holds, so ONE half is representable —
// because it is a real state a Machine gets into, and a seam that could not report it would make the
// Warden blind to the one failure serve.go's header calls out." Behind one container id there is no
// half to be missing; `podman ps` says up or not-up and the Warden learns nothing.
//
// So the supervisor loop does not move into this driver, it moves UP — which is where ADR 0037 puts
// it anyway ("The Warden runs no actor code. It reconciles, judges health"), and which driver.go
// already refuses to let a driver own: not "restart", because "a driver that owned the policy would
// be a second place to disagree about health with the thing ADR 0037 says judges it". One container
// per half is what makes the pair's health a question `list` can answer.
//
// ═══ THE LABEL IS A LABEL, AND MUST NOT BE AN ENVIRONMENT VARIABLE ═══
//
// The same `KONTRA_WORKER=<name>/<version>/<part>` value the process driver writes into a process's
// environment is written here as `podman run --label`, and it must NOT also be set in the
// container's environment. driver.go names the hazard from the other side: a containerised Worker's
// processes "are visible in the host's process table", and "the `process` driver must not claim a
// Worker `podman` is holding". The process driver's list is a scan of `/proc/<pid>/environ`, so a
// KONTRA_WORKER in a container's environment would make every podman Worker on the Machine appear in
// the process driver's list too — two drivers reporting one Worker, and a reconcile loop that stops
// it through the wrong one.
//
// ═══ THE ENVIRONMENT IS DECLARED HERE, NOT INHERITED ═══
//
// driver.go says `ProcSpec.Env` "is the WHOLE environment and not a delta, matching what serve.go
// builds today" — `cliutil.Derive(os.Environ(), …)`. That is right for a process, which inherits whether or
// not anyone meant it to, and WRONG for a container, which inherits nothing unless told. The
// difference is the point rather than an inconvenience: the Warden's environment is where the
// Machine's own credentials live, and 0036's stated win is that "kontra's own credentials are no
// longer in the actor's process". So this driver passes `--env` for exactly what the spec carries
// and NEVER `--env-host`; a spec built for podman states what the container should see.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
)

// podmanDriver runs a Worker as a pod of two containers on this Machine.
type podmanDriver struct {
	// bin is the podman executable. A field so a test can point at a stub, and a field rather than a
	// constant for no other reason — nothing configures it.
	bin string

	// userns is how this podman gives the pair a user namespace, decided once by READING the runtime
	// (newPodmanDriver) rather than assumed. There is no third state: a driver that could hold "no
	// namespace" would be a driver that could run a stranger's code as host root.
	userns podmanUserns

	// trust is which registries this Machine pulls from and whose signatures it accepts
	// (cli/internal/trustpolicy/trustpolicy.go). IT IS A FIELD AND NOT A PACKAGE-LEVEL LOOKUP, so that every construction
	// of this driver has to state it — and THE ZERO VALUE ADMITS NOTHING, so a site that forgets
	// starts no Workers rather than starting every one. The driver ENFORCES the policy and does not
	// decide it: reading the environment here would be a second place to disagree with the Warden
	// about what this Machine trusts, which is the objection driver.go raises to a driver owning any
	// policy at all.
	trust trustpolicy.Policy
}

// podmanUserns is where the pair's user namespace comes from. Two ways to have one, and they are not
// interchangeable in how they are obtained — only in what they guarantee.
type podmanUserns struct {
	// how is for the operator: "rootless" or "auto". It is on the struct rather than derived from
	// len(args) because "no flags" and "no namespace" would otherwise be the same value, and those
	// are the two states this type exists to keep apart.
	how string

	// args are the flags that go on `podman pod create`. EMPTY IS A VALID, SAFE VALUE and only when
	// how == "rootless": podman invoked by a non-root user is already inside that user's namespace
	// and every container uid maps into its subuid range without anything being asked for.
	args []string
}

// The seam is real or it is ceremony, and this is what makes the compiler say which.
var _ workerDriver = (*podmanDriver)(nil)

func (d *podmanDriver) driverName() string { return "podman" }

// newPodmanDriver reads the runtime to find out which namespace posture it is in.
//
// A READ, NOT A CONFIGURATION FLAG, for the same reason `list` is a read: the answer is a property of
// the Machine this is running on, and a flag would be a second place to be wrong about it. Rootless
// podman needs nothing added; rootful podman is asked for `--userns=auto`, which allocates a distinct
// range per pod out of the `containers` subuid allocation — distinct per pod being the part that
// matters, since two Workers sharing a range could reach each other's files as root.
//
// THE TRUST POLICY IS AN ARGUMENT AND NOT A READ, which is the opposite of the namespace posture
// above and deliberately so. The posture is a fact about this Machine that the runtime is the
// authority on; the policy is a decision an OPERATOR made, and a driver that went looking for it
// would be a second place to be wrong about what this Machine trusts. Passing it also makes the
// compiler ask every caller — there is no constructor that yields a driver with no policy.
func newPodmanDriver(ctx context.Context, trust trustpolicy.Policy) (*podmanDriver, error) {
	d := &podmanDriver{bin: "podman", trust: trust}
	out, err := exec.CommandContext(ctx, d.bin, "info", "--format", "{{.Host.Security.Rootless}}").Output()
	if err != nil {
		return nil, fmt.Errorf("podman info: %w", err)
	}
	if strings.TrimSpace(string(out)) == "true" {
		d.userns = podmanUserns{how: "rootless"}
		return d, nil
	}
	// ROOTFUL PODMAN IS NOT AN ERROR, it is the common case on a **Machine** the Warden owns; running
	// the WORKLOAD as host root is the error, and `--userns=auto` is what stops that. If the Machine
	// has no `containers` subuid allocation, podman refuses at `pod create` and `start` turns that
	// into the message with the fix — see startupUsernsHint.
	d.userns = podmanUserns{how: "auto", args: []string{"--userns=auto"}}
	return d, nil
}

// --- the image ------------------------------------------------------------------------------------

// ═══ TWO REFUSALS, BECAUSE THERE ARE TWO OPPOSITE TRUTHS ═══
//
// A reference this driver will not run fails for one of two reasons, and telling an operator the
// wrong one costs them an afternoon:
//
//	ociref.ErrImageUnpinned         the reference is WELL FORMED but names a tag. Pinning it fixes it.
//	ociref.ErrImageUnrepresentable  the reference is not an OCI reference AT ALL. Nothing fixes it.
//
// THE SECOND IS NOT A TYPO, IT IS A CLASS OF ACTOR. kontra names **Actors** more freely than OCI names
// repositories — `shared/conformance/queues.json` pins `a/b`, `my actor` and `café` as names that must keep
// working, and states why: "A QUEUE NAME IS NOT SANITISED… A derivation that sanitised the queue would
// route to a queue nobody polls, which is silent." Temporal accepts all three. The OCI grammar accepts
// one of them. So there is a set of Actors that serve forever under `process` — which runs from source
// and never produces an **Artifact** — and can never be placed by ANY driver that pulls an image,
// this one included.
//
// Verified against podman 4.3.1, which is the only opinion that counts here:
//
//	localhost:5000/café:0.1.0     invalid reference format        no remedy
//	kontra/café-worker:0.1.0      invalid reference format        no remedy  (the LOCAL form, also dead)
//	localhost:5000/Foo:1.0        repository name must be lowercase          no remedy
//	localhost:5000/my actor:1.0   invalid reference format        no remedy
//	localhost:5000/a/b:1.0.0      reading manifest … not found    DEPLOY FIXES IT
//	localhost:5000/nscheck:0.1.0  reading manifest … not found    DEPLOY FIXES IT
//
// `a/b` is the one that looks adversarial and is not: a slash is a legal path separator, so `a/b` is a
// perfectly good repository whose image merely has not been pushed. A check that refused it would be
// as wrong as one that accepted `café`.
//
// THE GRAMMAR AND BOTH SENTINELS NOW LIVE IN cli/internal/ociref/ociref.go, AND THAT IS THE POINT. When this driver
// was written, `cli/scale.go` gave one message for both of these — `.scratch/warden/issues/15-*` —
// and this file said "this is the site where the message had not been written yet, so it is written
// correctly rather than a third time badly". Slice 07 added the THIRD site (`kontra build --push`),
// which is what the issue said must not happen again: "Any fix should put the answer in one place all
// three consult, not add a second bespoke message." So the answer moved out whole; what stays here is
// the PINNING rule, which is this driver's alone, and the consequence sentence, which is this
// driver's alone too.

// --- what must never be handed to the workload ------------------------------------------------------

// runtimeSockets are the paths that ARE the container runtime. Both of these answer a dial on this
// box, which is why the test that asserts a Worker cannot reach them is a real assertion and not a
// statement about two paths that happen not to exist.
//
// The list is used two ways and both are needed: `assertNoRuntimeAccess` refuses to BUILD an argv
// that exposes one, and the test dials them from inside a container this driver actually started.
// The first catches the mistake at the moment someone makes it; the second catches the ways of
// exposing a socket that nobody thought to enumerate.
var runtimeSockets = []string{
	"/run/podman/podman.sock",
	"/var/run/podman/podman.sock",
	"/var/run/docker.sock",
	"/run/docker.sock",
}

// assertNoRuntimeAccess is a guard on this driver's OWN argv, and it exists because the thing it
// prevents is one careless `-v` away and is invisible once made.
//
// It is not defence against an attacker — an attacker does not edit this repo's argv builder — it is
// defence against the ordinary afternoon when somebody needs the socket for a debugging session and
// leaves the flag in. A mount of the runtime socket hands the workload root on the Machine, so the
// cost of that afternoon is the whole boundary, and the check that would have caught it is cheap.
//
// `--network=host` is refused for the same reason plus a second one: it also collapses the per-Worker
// network namespace that `infra/CONTEXT.md` says each Worker needs for "its own egress address", and
// that is slice 12's entire footing.
func assertNoRuntimeAccess(args []string) error {
	for _, a := range args {
		for _, sock := range runtimeSockets {
			// A bind mount is `-v <src>:<dst>` or `--mount …source=<src>…`, so the socket path can be
			// anywhere in the argument rather than equal to it.
			if strings.Contains(a, sock) {
				return fmt.Errorf("refusing to start a Worker with %s exposed to it: a workload that can "+
					"reach the container runtime can start a container with `-v /:/host` and owns the "+
					"Machine (argument %q)", sock, a)
			}
		}
		if a == "--network=host" || a == "--net=host" {
			return fmt.Errorf("refusing to start a Worker on the host network: it removes the per-Worker "+
				"network namespace a Machine's egress policy attaches to (argument %q)", a)
		}
	}
	return nil
}

// --- naming ------------------------------------------------------------------------------------

// podmanPod is the pod holding one Worker's pair. `kontra-` prefixed so a Machine's pods are
// separable from anything else on it at a glance, and `<name>-<version>` because that is the identity
// every other surface already prints (workerHandle.id, tmux.Session, queues.Shared).
func podmanPod(name, version string) string { return "kontra-" + name + "-" + version }

// podmanContainer is one half's container. The pod name plus the part, so `podman ps` reads as the
// pair it is without anyone joining anything.
func podmanContainer(name, version string, part workerPart) string {
	return podmanPod(name, version) + "-" + string(part)
}

// --- the four verbs ------------------------------------------------------------------------------

// start creates the pod and runs both halves in it.
//
// THE POD IS CREATED FIRST AND TORN DOWN ON ANY FAILURE, so a half-created Worker does not survive an
// error to be found by the next `list` — which would be a Worker the Warden never started and cannot
// explain. Note what is NOT here: no check that the Worker is already running. driver.go, on why:
// "the check that would be worth making is `list` — the read that is true whoever started the other
// one. ADR 0037's `place()` is the idempotent verb and it is a caller of this, not this."
//
// ═══ THIS IS THE PULL, SO THIS IS WHERE TRUST POLICY GOES ═══
//
// `podman run` pulls the image if the Machine does not already hold it. There is no separate pull
// verb to hang a check on and there must not be one: a gate beside the pull is a gate an image can
// arrive around, and slice 12's egress policy has the same shape for the same reason. So `admit` runs
// FIRST — before `pod create`, before any argv is built, before anything reaches the network —
// and the digest it returns is the one this function was going to need anyway (KONTRA_ACTOR_DIGEST).
//
// The four gates and their order are argued in cli/internal/trustpolicy/trustpolicy.go. What matters here is the boundary:
// everything above this line is string comparison on a Machine that has contacted nobody, and
// everything below it can fetch, so an image from a registry this Machine does not pull from is
// refused without a DNS lookup, and an unsigned one is refused before its layers land on the disk.
func (d *podmanDriver) Start(ctx context.Context, spec Spec) (workerHandle, error) {
	digest, err := d.trust.Admit(ctx, spec.Image)
	if err != nil {
		return workerHandle{}, err
	}

	pod := podmanPod(spec.Name, spec.Version)
	podArgs := append([]string{"pod", "create", "--name", pod}, d.userns.args...)
	if err := assertNoRuntimeAccess(podArgs); err != nil {
		return workerHandle{}, err
	}
	if out, err := exec.CommandContext(ctx, d.bin, podArgs...).CombinedOutput(); err != nil {
		return workerHandle{}, fmt.Errorf("podman pod create %s: %v: %s%s",
			pod, err, strings.TrimSpace(string(out)), d.usernsHint(out))
	}

	h := workerHandle{Driver: d.driverName(), Name: spec.Name, Version: spec.Version}
	halves := map[workerPart]ProcSpec{partActor: spec.Actor, partHandler: spec.Handler}
	for _, part := range workerParts {
		p := halves[part]
		args, err := d.runArgs(pod, spec, part, p, digest)
		if err != nil {
			d.removePod(ctx, pod)
			return workerHandle{}, err
		}
		out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput()
		if err != nil {
			d.removePod(ctx, pod)
			return workerHandle{}, fmt.Errorf("podman run %s: %v: %s", string(part), err, strings.TrimSpace(string(out)))
		}
		// `podman run -d` prints the container id, and that id is the half's Ref — the same role the
		// pid plays for the process driver (driver.go: "podman's will be a container id").
		h.Halves = append(h.Halves, workerHalf{Part: part, Ref: cliutil.FirstLine(string(out))})
	}
	return h, nil
}

// runArgs is one half's `podman run`. Every flag here is either identity, isolation, or the spec.
//
// THE GUARD RUNS ON THE FLAGS ONLY, AND THE BOUNDARY IS `podman run [FLAGS] IMAGE [COMMAND…]`. Past
// the image reference, podman is no longer reading arguments — it is handing them to the workload, so
// nothing there can mount anything or change a namespace. Scanning them anyway was the first version
// of this and its own test caught it: a Worker asked to DIAL `/run/podman/podman.sock` was refused for
// naming the path, which would equally refuse any actor whose arguments happen to mention one. A
// guard that fires on a string in a workload's argv is a guard that gets loosened by whoever hits it
// next, so it is aimed at the only place the hazard can actually live.
func (d *podmanDriver) runArgs(pod string, spec Spec, part workerPart, p ProcSpec, digest string) ([]string, error) {
	flags := []string{
		"run", "--detach",
		"--pod", pod,
		"--name", podmanContainer(spec.Name, spec.Version, part),

		// THE LABEL, and only as a label — see this file's header for why it must not also reach the
		// container's environment.
		"--label", workerLabelVar + "=" + workerLabel(spec.Name, spec.Version, part),

		// NO NEW PRIVILEGES and NO CAPABILITIES. Neither half needs any: the actor polls a queue and
		// the handler polls a queue, and both reach the Controller over TCP. A capability set that is
		// empty by default is the one that stays empty, because the day something appears to need
		// CAP_NET_ADMIN is a day someone has to justify it in a diff.
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
	}

	// KONTRA_ACTOR_DIGEST IS THE IDENTITY FIELD ADR 0032 BUILT AND 0036 SPENDS. The registrar reads it
	// (`runtime/go/registrar/registrar.go`, `runtime/python/internals/catalog.py`) and publishes it to
	// the catalog, so setting it here is what makes a running Worker able to say which image it is —
	// and its conformance test already pins the failure of NOT setting it: "worker booting with no
	// KONTRA_ACTOR_DIGEST unpinned the image the design tool had pinned".
	//
	// It is derived from the reference rather than passed alongside it, so the two cannot disagree.
	flags = append(flags, "--env", "KONTRA_ACTOR_DIGEST="+digest)
	for _, e := range p.Env {
		flags = append(flags, "--env", e)
	}
	if p.Dir != "" {
		flags = append(flags, "--workdir", p.Dir)
	}

	if err := assertNoRuntimeAccess(flags); err != nil {
		return nil, err
	}

	// ═══ --entrypoint, OR THE SPEC IS SILENTLY IGNORED ═══
	//
	// `podman run IMAGE cmd…` replaces the image's CMD and leaves its ENTRYPOINT in charge, so an
	// image that declares one runs THAT and receives the spec's argv as arguments it is free to drop.
	// Every kontra worker image declares one: `ENTRYPOINT ["/kontra/entrypoint.sh"]` (deploy.go's
	// workerDockerfile), and that script takes no arguments at all.
	//
	// MEASURED, because the first version of this file got it wrong and the fixture hid it. Driving a
	// real `layer-scan@0.3.0` worker image with an actor half of `python3 /actor/layer-scan/actor.py`
	// produced, in BOTH containers:
	//
	//	[worker] started: redis=shared placement=10 daprd=18 host=19 handler=29
	//
	// — the image's own supervisor, starting BOTH halves, in each of the two containers meant to hold
	// one half each. Four processes, and two rival pollers on every queue: precisely the "two rival
	// pollers on `<name>-<version>`" that driver.go describes as a worker which "can take its
	// continue_as_new and run different code". The pair-as-unit seam was intact and the thing it was
	// pointing at was wrong.
	//
	// ONLY Argv[0] GOES IN THE FLAG, and the rest stay after the image. The JSON-array form of
	// `--entrypoint` would put the whole command in the flag section, back inside the guard's reach —
	// which is how a workload argument that merely names a socket would start being refused again.
	// This keeps the boundary the guard relies on exactly where it was.
	//
	// An empty Argv means the image's entrypoint IS the spec, so nothing is overridden.
	args := make([]string, 0, len(flags)+3+len(p.Argv))
	args = append(args, flags...)
	if len(p.Argv) > 0 {
		args = append(args, "--entrypoint", p.Argv[0])
	}
	args = append(args, spec.Image)
	if len(p.Argv) > 1 {
		args = append(args, p.Argv[1:]...)
	}
	return args, nil
}

// usernsHint turns podman's own refusal into the fix, and only when that is what happened.
//
// PODMAN'S MESSAGE IS ACCURATE AND UNACTIONABLE: "Cannot find mappings for user \"containers\": no
// subuid ranges found for user \"containers\" in /etc/subuid", then "could not find enough available
// IDs". An operator reading that on a fresh Machine has no way to know it is one line in one file, so
// this appends the line. Matching on podman's text rather than pre-flighting /etc/subuid is
// deliberate: parsing subuid here would be a second implementation of podman's allocation rules, and
// the runtime is the thing that decides.
func (d *podmanDriver) usernsHint(out []byte) string {
	if d.userns.how != "auto" || !bytes.Contains(out, []byte("subuid")) && !bytes.Contains(out, []byte("available IDs")) {
		return ""
	}
	return "\n  this Machine has no subuid allocation for user namespaces, so podman cannot map the" +
		"\n  workload off host root — and running a stranger's code as host root is what this refuses." +
		"\n  fix:  echo 'containers:524288:16777216' >> /etc/subuid" +
		"\n        echo 'containers:524288:16777216' >> /etc/subgid"
}

// stop ends the pair by removing the pod, then VERIFIES against `list`.
//
// THE EXIT STATUS IS NOT THE ANSWER, AND THIS BOX PROVES IT. Measured here: `podman stop` printed
// "unable to signal init: permission denied" and then "timed out waiting for file
// /run/libpod/exits/…: internal libpod error" and exited non-zero — while the container went to
// `Exited` and left `podman ps` exactly as asked. A driver that returned that error would report a
// successful stop as a failure, and a reconcile loop above it would retry forever against a Worker
// that is already gone. So the stop is issued and then the RUNTIME is asked whether it took, which is
// the same rule `list` is built on and the same reason.
func (d *podmanDriver) Stop(ctx context.Context, h workerHandle, drain time.Duration) error {
	pod := podmanPod(h.Name, h.Version)

	// drain is podman's `--time`: seconds to wait after SIGTERM before SIGKILL. 0 means kill now,
	// which is what a caller that has already decided the Worker is finished passes (driver.go).
	secs := int(drain.Round(time.Second) / time.Second)
	if drain > 0 && secs == 0 {
		// A sub-second drain is still a request to be graceful, and rounding it to 0 would silently
		// turn it into a kill.
		secs = 1
	}
	_, _ = exec.CommandContext(ctx, d.bin, "pod", "stop", "--time", fmt.Sprint(secs), pod).CombinedOutput()

	// THE POD IS PART OF "STOPPED", AND FORGETTING THAT BREAKS RESTART. Found by running this
	// driver's own tests twice: `podman pod rm --force` exits 0 having removed both containers and
	// LEFT THE POD, which then sits in `Created` with zero containers. Every container-level check
	// passes — `list` reads containers and reports the Worker gone — while the next `start` for the
	// same Worker dies on `adding pod to state: name "kontra-…" is in use: pod already exists`.
	//
	// For a reconcile loop that is not a corner case, it is the main line: stop-then-start is how a
	// Worker is moved to a new digest, and a Warden that could stop a Worker exactly once would wedge
	// the first time it tried. So the removal is RETRIED until podman itself stops listing the pod,
	// and `start` is left alone — it must not delete a pod it finds, because it cannot tell a leftover
	// from a Worker somebody else is running (driver.go: start "does not check whether the Worker is
	// already running", and `place()` is the idempotent verb above it).
	//
	// THE BACKSTOP IS A MINUTE BECAUSE A SLOW RUNTIME IS A REAL ONE. On a healthy Machine the first
	// removal succeeds and this loop turns once. On the box this was written on, where the kernel
	// refuses to let podman signal a container's init at all ("unable to signal init: permission
	// denied"), `podman pod rm --force` fails PARTIALLY and converges: measured at 21s, then 11s, then
	// success on the third call — each one removing more of the pod than the last. A deadline that
	// gave up before that would report a failure while the removal it asked for was still working,
	// and a reconcile loop above would thrash against a Worker that was on its way out anyway.
	// Blocking is the lesser evil than returning early and leaving the next `start` to discover the
	// pod; a caller that cannot wait cancels ctx, which is checked below.
	deadline := time.Now().Add(drain + time.Minute)
	for {
		d.removePod(ctx, pod)

		hs, err := d.list(ctx)
		if err != nil {
			return err
		}
		running := false
		for _, got := range hs {
			if got.id() == h.id() {
				running = true
			}
		}
		if !running && !d.podExists(ctx, pod) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopping %s: %w", h.id(), err)
		}
		if !time.Now().Before(deadline) {
			if running {
				return fmt.Errorf("%s is still running after stop", h.id())
			}
			return fmt.Errorf("%s is stopped but pod %s survived removal, so it cannot be started again", h.id(), pod)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// podExists asks podman whether the pod is still an object it holds. `pod ps --filter name=` rather
// than `pod exists`, because the filter is a prefix-free exact name here and one read serves both the
// "gone" and "still there" answers without branching on an exit code.
func (d *podmanDriver) podExists(ctx context.Context, pod string) bool {
	out, err := exec.CommandContext(ctx, d.bin, "pod", "ps", "--filter", "name="+pod, "--format", "{{.Name}}").Output()
	if err != nil {
		// Unreadable is not "gone": saying gone here would let `stop` report success and leave the
		// next `start` to discover otherwise.
		return true
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == pod {
			return true
		}
	}
	return false
}

// removePod is the teardown that must not fail the caller: `pod rm --force` removes the pair's
// containers with it, and its error is never the interesting one — `stop` verifies by listing and
// `start` is already returning the error that brought it here.
func (d *podmanDriver) removePod(ctx context.Context, pod string) {
	_, _ = exec.CommandContext(ctx, d.bin, "pod", "rm", "--force", pod).CombinedOutput()
}

// podmanPS is the shape of `podman ps --format json` this driver reads. Two fields of many, named to
// match podman's own JSON.
type podmanPS struct {
	ID     string            `json:"Id"`
	Labels map[string]string `json:"Labels"`
}

// list is every Worker podman is holding — READ FROM PODMAN, never from a map this driver kept.
//
// driver.go calls this "the single most common bug in this class of software", and the two properties
// it owes are the two driver_podman_test.go asserts by going AROUND the driver: a Worker started by
// `podman run` directly is reported, and a Worker whose container was killed behind the driver's back
// is forgotten.
//
// `podman ps` WITHOUT `-a` IS THE WHOLE TRICK. A container that exited is still an object podman
// holds — it keeps its logs, which is what makes `logs` useful after a crash — and `podman ps -a`
// would report it forever, the containerised form of reading from memory. Running is the question;
// existing is not.
func (d *podmanDriver) list(ctx context.Context) ([]workerHandle, error) {
	out, err := exec.CommandContext(ctx, d.bin, "ps",
		"--filter", "label="+workerLabelVar, "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	var rows []podmanPS
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("podman ps --format json: %w", err)
	}

	type key struct{ name, version string }
	found := map[key]map[workerPart]string{}
	for _, r := range rows {
		// THE SAME STRICT PARSE THE PROCESS DRIVER USES. A label is where an arbitrary container on
		// the Machine gets to claim it is one of ours, and driver.go's parse refuses everything that
		// is not exactly `<name>/<version>/<part>` for a part it knows.
		name, version, part, ok := parseWorkerLabel(r.Labels[workerLabelVar])
		if !ok {
			continue
		}
		k := key{name, version}
		if found[k] == nil {
			found[k] = map[workerPart]string{}
		}
		// Two containers for one half means two rival pollers on one queue. The lower id wins so the
		// answer is stable across calls, which is the process driver's rule (lowest pid) carried over;
		// resolving the rivalry is not this seam's job, and a caller that cares can see it in
		// `podman ps` the same way this did.
		if cur, seen := found[k][part]; !seen || r.ID < cur {
			found[k][part] = r.ID
		}
	}

	res := make([]workerHandle, 0, len(found))
	for k, halves := range found {
		h := workerHandle{Driver: d.driverName(), Name: k.name, Version: k.version}
		for _, part := range workerParts {
			if id, ok := halves[part]; ok {
				h.Halves = append(h.Halves, workerHalf{Part: part, Ref: id})
			}
		}
		res = append(res, h)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].id() < res[j].id() })
	return res, nil
}

// logs is what podman retained for both halves.
//
// THIS IS THE ASYMMETRY driver_process.go NAMES: "`podman logs` has a real answer here and this does
// not." A foreground process driver Worker writes to the terminal that started it and the kernel keeps
// nothing, so it refuses with errNoRetainedLogs; a container's stdout is captured by the runtime and
// SURVIVES THE CONTAINER'S DEATH, which is exactly when it is wanted. Verified: a killed container's
// `podman logs` still returns what it printed.
//
// So this asks for the half by CONTAINER NAME rather than by the ref on the handle. The name is
// derived from the Worker's identity and outlives the container; the handle's refs are the halves
// podman is currently holding, and the half that is missing from the handle is usually the half whose
// output you came for.
func (d *podmanDriver) logs(ctx context.Context, h workerHandle) (io.ReadCloser, error) {
	var buf bytes.Buffer
	var found bool
	for _, part := range workerParts {
		name := podmanContainer(h.Name, h.Version, part)
		out, err := exec.CommandContext(ctx, d.bin, "logs", name).CombinedOutput()
		if err != nil {
			// A half that was never started, or whose container has been removed, has no log — and the
			// OTHER half may still have one worth reading, so this is not fatal to the call.
			continue
		}
		found = true
		fmt.Fprintf(&buf, "== %s ==\n", part)
		buf.Write(out)
	}
	if !found {
		return nil, fmt.Errorf("%s: %w — podman is holding no container for either half, so there is "+
			"nothing left that could have kept one", h.id(), errNoRetainedLogs)
	}
	return io.NopCloser(&buf), nil
}
