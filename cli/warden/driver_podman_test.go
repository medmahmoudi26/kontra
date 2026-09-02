// driver_podman_test.go — the podman driver, tested against a live runtime.
//
// EVERY CLAIM THIS SLICE MAKES IS A CLAIM ABOUT WHAT PODMAN IS ACTUALLY DOING, so almost nothing here
// is a test of an argv. An argv test says the driver INTENDED not to expose the runtime socket; only
// dialling the socket from inside a container the driver started says it did not. The two places an
// argv is asserted are the two guards whose whole job is to refuse before podman is reached.
//
// ═══ THE POSITIVE CONTROL IS THE POINT ═══
//
// This repo has been bitten repeatedly by guards that passed vacuously — "a walk that found zero
// files, a 0-byte stub that exits 0, a corpus row where two implementations cannot differ". A test
// that asserts "the container could not reach /run/podman/podman.sock" is exactly that shape: it
// passes identically when the socket does not exist, when the probe is broken, when the dial silently
// no-ops, and when the driver is correct. Three of those four are bugs in the test.
//
// So the socket test carries its own controls and asserts them FIRST, in the same run and against the
// same probe binary:
//
//	the socket answers a dial from the HOST                        → the target is real
//	the socket answers a dial from INSIDE, when mounted on purpose  → the probe can detect it
//	the socket refuses a dial from inside a DRIVER-started Worker    → the claim
//
// Measured while writing this file, on this box: both `/var/run/docker.sock` and
// `/run/podman/podman.sock` answer from the host, and a container run with the socket bind-mounted
// reaches it. Without those two, the third line proves nothing.
//
// ═══ THE FIXTURE PULLS NOTHING ═══
//
// The test image is built here, `FROM scratch`, around one static Go binary. No registry, no pull, no
// Docker Hub — which matters beyond speed: this box's Docker Hub allowance is 100 GETs an hour per
// IP, shared with every other build on it, and a test suite that spends it is a test suite that
// breaks somebody else's `docker build`. A locally built image still has a real manifest digest, so
// digest-pinning is exercised for real rather than mocked.
package warden

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy/cosignstub"
)

// --- the fixture ---------------------------------------------------------------------------------

// podmanProbeSource is the whole workload: a static binary that can answer the three questions this
// file asks from INSIDE a container, plus stay alive so `list` has something to find.
//
// It is a Go program rather than a shell script because `FROM scratch` has no shell, and `FROM
// scratch` is what makes the image buildable with no network at all.
const podmanProbeSource = `package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "tcp":
		// WHERE CAN THIS CONTAINER REACH? Slice 12's whole question, asked from the only place an
		// answer means anything. One line per destination so a partial answer is still readable, and
		// the words are REACHED/REFUSED rather than an exit code because a test that asserts on which
		// destinations answered has to be able to print the ones that did.
		for _, a := range os.Args[2:] {
			c, err := net.DialTimeout("tcp", a, 3*time.Second)
			if err != nil {
				fmt.Println("REFUSED", a, err)
				continue
			}
			c.Close()
			fmt.Println("REACHED", a)
		}
	case "listen":
		// A DESTINATION THAT IS ITSELF A CONTAINER. warden_egress_test.go needs a private address a
		// container can reach with no policy installed, and on this box the Machine's own addresses are
		// not one: ufw denies container-to-host on every port it has not been told about, so a listener
		// there is refused before any kontra rule is consulted and the control would be a fiction. A
		// second container on the same bridge is reachable, is RFC1918, and belongs to nobody else.
		ln, err := net.Listen("tcp", os.Args[2])
		if err != nil {
			fmt.Println("LISTEN-ERROR", err)
			return
		}
		fmt.Println("LISTENING", ln.Addr())
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// THE SOURCE ADDRESS, AS THE DESTINATION SEES IT — which is the only place that answer
			// exists. warden_packing_test.go asks whether two Workers packed onto one Machine share
			// one (ADR 0037), and a container knows its own address but not the one its traffic
			// wears after the Machine has masqueraded it.
			fmt.Println("PEER", c.RemoteAddr().String())
			c.Close()
		}
	case "nft-dump":
		// CAN THIS CONTAINER SEE THE RULES THAT BOUND IT? It asks the kernel directly over
		// NETLINK_NETFILTER rather than shelling out to nft, because a scratch image has no nft and
		// "the tool is missing" is exactly the vacuous pass this file's header refuses. A netlink
		// socket addresses the nf_tables instance of its OWN network namespace and there is no
		// addressing mode for another one, so an empty list here is the boundary rather than an
		// accident — and warden_egress_test.go proves that by running the same binary on the host,
		// where it lists the table.
		names, err := nftTables()
		if err != nil {
			fmt.Println("NFT-ERROR", err)
			return
		}
		fmt.Println("NFT-TABLES", names)
	case "dial":
		// Can this container reach the container runtime? Printed one line per path so a partial
		// answer is still readable.
		for _, s := range os.Args[2:] {
			c, err := net.DialTimeout("unix", s, 2*time.Second)
			if err != nil {
				fmt.Println("refused", s)
				continue
			}
			c.Close()
			fmt.Println("REACHED", s)
		}
	case "uidmap":
		// WHO IS THIS CONTAINER ON THE HOST. /proc/self/uid_map is the mapping itself: "0 0
		// 4294967295" means container root IS host root, "0 524288 65534" means it is not.
		b, err := os.ReadFile("/proc/self/uid_map")
		if err != nil {
			fmt.Println("uidmap-error", err)
			return
		}
		fmt.Println("uidmap", string(b))
	case "image-entrypoint":
		// THE MODE THAT MUST NEVER RUN. It is what the fixture image's own ENTRYPOINT selects, so
		// seeing this marker means the driver failed to override it and the workload the spec asked
		// for was never started — see TestPodmanRunsTheSpecsCommandAndNotTheImagesEntrypoint.
		//
		// IT RETURNS RATHER THAN SLEEPING, and that is not a detail. When it slept, any FOREGROUND
		// podman run of this image that forgot --entrypoint blocked for thirty minutes instead of
		// failing, which is what a CombinedOutput call in this very file did: it hung the suite with
		// no output at all. A fixture whose failure mode is a hang teaches nothing, and podman keeps
		// the log of an exited container, so returning still leaves the marker to be read.
		//
		// (No backticks in this comment. It lives inside a raw string literal, and one of those ends
		// the fixture source mid-function — which is how this file first failed to compile.)
		fmt.Println("IMAGE-ENTRYPOINT-RAN", os.Args[1:])
	default:
		fmt.Println("probe up")
		time.Sleep(30 * time.Minute)
	}
}

// nftTables asks the kernel for every nftables table the caller's network namespace holds.
// NFT_MSG_GETTABLE is 1, not 0 — 0 is NEWTABLE, and sending that outside a batch returns EINVAL,
// which reads exactly like a refusal and is not one. That mistake cost a wrong conclusion once.
func nftTables() ([]string, error) {
	const (
		netlinkNetfilter = 12
		nfnlSubsysNfT    = 10
		nftMsgGetTable   = 1
		nftaTableName    = 1
	)
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, netlinkNetfilter)
	if err != nil {
		return nil, fmt.Errorf("socket: %v", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("bind: %v", err)
	}
	req := make([]byte, 20)
	binary.LittleEndian.PutUint32(req[0:], 20)
	binary.LittleEndian.PutUint16(req[4:], uint16(nfnlSubsysNfT)<<8|nftMsgGetTable)
	binary.LittleEndian.PutUint16(req[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	binary.LittleEndian.PutUint32(req[8:], 1)
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("send: %v", err)
	}
	tv := syscall.Timeval{Sec: 5}
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	names := []string{}
	buf := make([]byte, 1<<16)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return names, fmt.Errorf("recv: %v", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return names, fmt.Errorf("parse: %v", err)
		}
		for _, m := range msgs {
			if m.Header.Type == syscall.NLMSG_DONE {
				return names, nil
			}
			if m.Header.Type == syscall.NLMSG_ERROR {
				e := int32(binary.LittleEndian.Uint32(m.Data[0:4]))
				if e == 0 {
					return names, nil
				}
				return names, fmt.Errorf("kernel refused: %v", syscall.Errno(-e))
			}
			if len(m.Data) < 4 {
				continue
			}
			// nfgenmsg is four bytes, then netlink attributes.
			for a := m.Data[4:]; len(a) >= 4; {
				l := int(binary.LittleEndian.Uint16(a[0:2]))
				typ := binary.LittleEndian.Uint16(a[2:4]) & 0x3fff
				if l < 4 || l > len(a) {
					break
				}
				if typ == nftaTableName {
					v := a[4:l]
					for i, c := range v {
						if c == 0 {
							v = v[:i]
							break
						}
					}
					names = append(names, string(v))
				}
				step := (l + 3) &^ 3
				if step > len(a) {
					break
				}
				a = a[step:]
			}
		}
	}
}
`

var (
	probeOnce sync.Once
	probeRef  string
	probeErr  error
)

// probeImage builds the fixture image once per package run and returns it as a DIGEST-PINNED
// reference, because a tag is the one thing the driver refuses.
func probeImage(t *testing.T) string {
	t.Helper()
	probeOnce.Do(func() { probeRef, probeErr = buildProbeImage() })
	if probeErr != nil {
		t.Skipf("cannot build the probe image, so nothing here can be tested against a real runtime: %v", probeErr)
	}
	return probeRef
}

func buildProbeImage() (string, error) {
	dir, err := os.MkdirTemp("", "kontra-slice02-probe")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(podmanProbeSource), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module kontraslice02probe\n\ngo 1.21\n"), 0o644); err != nil {
		return "", err
	}
	// CGO_ENABLED=0 is what makes the binary static and therefore runnable in a scratch image;
	// GOWORK=off keeps it out of the repo's workspace, which does not contain it.
	build := exec.Command("go", "build", "-o", "probe", ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %v: %s", err, out)
	}

	// THE FIXTURE DECLARES AN ENTRYPOINT THAT MUST BE OVERRIDDEN, and that is the whole reason it
	// takes an argument. An image whose ENTRYPOINT is bare `/probe` would receive the spec's argv as
	// arguments and behave correctly by accident — which is what the first version of this file did,
	// and it is why a real worker image running its own supervisor in both containers went unnoticed
	// until the driver was pointed at one. Every kontra worker image declares an ENTRYPOINT
	// (`/kontra/entrypoint.sh`), so the fixture declares one too, and picks a mode nothing else asks
	// for so its presence in a log is unambiguous.
	const containerfile = "FROM scratch\nCOPY probe /probe\nENTRYPOINT [\"/probe\", \"image-entrypoint\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(containerfile), 0o644); err != nil {
		return "", err
	}
	const tag = "kontra-slice02-probe:test"
	bld := exec.Command("podman", "build", "-q", "-t", tag, "-f", "Containerfile", ".")
	bld.Dir = dir
	if out, err := bld.CombinedOutput(); err != nil {
		return "", fmt.Errorf("podman build: %v: %s", err, out)
	}

	out, err := exec.Command("podman", "image", "inspect", tag, "--format", "{{.Digest}}").Output()
	if err != nil {
		return "", fmt.Errorf("podman image inspect: %w", err)
	}
	digest := strings.TrimSpace(string(out))
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("locally built image has no manifest digest (%q), so digest pinning cannot be tested", digest)
	}
	return "localhost/kontra-slice02-probe@" + digest, nil
}

// requirePodman skips LOUDLY. A silent skip that reads as a pass is the failure mode this repo cares
// most about, so the message says what was missing and what it would have proved.
func requirePodman(t *testing.T) *podmanDriver {
	return requirePodmanTrusting(t, fixtureTrust(t))
}

// requirePodmanTrusting is requirePodman with the trust policy stated (cli/internal/trustpolicy/trustpolicy.go, slice 14).
//
// EVERY TEST IN THIS FILE NOW HAS TO SAY WHAT IT TRUSTS, and that is the property rather than a cost.
// The zero policy admits nothing, so a driver built without one starts no Workers; if the default had
// been "allow", every assertion below would have kept passing with the gate deleted, which is the
// shape of a security control this repo has already shipped once (`infra/watchdog.sh`).
func requirePodmanTrusting(t *testing.T, trust trustpolicy.Policy) *podmanDriver {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is not installed — the container Target of ADR 0036 is UNVERIFIED on this machine")
	}
	d, err := newPodmanDriver(context.Background(), trust)
	if err != nil {
		t.Skipf("podman is installed but not answering, so nothing here is verified: %v", err)
	}
	return d
}

// fixtureTrust is the policy the fixture image needs, and it is built through `trustpolicy.Load` rather
// than by filling the struct in, so these tests exercise the same parse an operator's flags do.
//
// `localhost` IS THE WHOLE ALLOWLIST HERE. `buildProbeImage` produces `localhost/kontra-slice02-probe@
// sha256:…` — podman's name for an image built on this box — and nothing in this file may reach any
// other registry. Marked unsigned because the fixture is built locally and signed by nobody; the
// signature gate has its own tests, which is where that is asserted rather than assumed.
func fixtureTrust(t *testing.T) trustpolicy.Policy {
	t.Helper()
	p, err := trustpolicy.Load(trustpolicy.Options{Registries: "localhost", Unsigned: "localhost"})
	if err != nil {
		t.Fatalf("the fixture's own trust policy does not load: %v", err)
	}
	return p
}

// cleanupPod removes a Worker's pod after a test, RETRYING for the same reason the driver's own
// `stop` retries: `podman pod rm --force` on this box exits 0 having removed the containers and left
// the pod, and a leftover pod fails the NEXT run of this file with "pod already exists". A test suite
// that only passes on a machine no previous run has touched is not passing.
func cleanupPod(t *testing.T, name, version string) {
	t.Helper()
	pod := podmanPod(name, version)
	t.Cleanup(func() {
		// Time-bounded rather than attempt-bounded: one `pod rm --force` costs ~20s on a runtime that
		// cannot signal init, so a fixed attempt count is either far too short or unbounded in wall
		// clock. Two minutes covers the three-call convergence measured here with room to spare.
		deadline := time.Now().Add(2 * time.Minute)
		for {
			_ = exec.Command("podman", "pod", "rm", "--force", pod).Run()
			out, err := exec.Command("podman", "pod", "ps", "--filter", "name="+pod, "--format", "{{.Name}}").Output()
			if err != nil || !strings.Contains(string(out), pod) {
				return
			}
			if !time.Now().Before(deadline) {
				t.Errorf("could not remove pod %s, so the next run of this file will fail on it", pod)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
}

// startPodmanWorker starts a pair and registers its teardown.
//
// A USER-NAMESPACE REFUSAL BECOMES A LOUD SKIP AND NOT A FAILURE, because it is a statement about
// this Machine's provisioning rather than about the driver — and the driver refusing is the correct
// behaviour being observed, not a bug. The message carries podman's own text, which contains the fix.
func startPodmanWorker(t *testing.T, d *podmanDriver, spec Spec) workerHandle {
	t.Helper()
	cleanupPod(t, spec.Name, spec.Version)
	h, err := d.Start(context.Background(), spec)
	if err != nil {
		if strings.Contains(err.Error(), "subuid") || strings.Contains(err.Error(), "available IDs") {
			t.Skipf("this Machine has no subuid allocation, so podman cannot map the workload off host "+
				"root and the driver refused — which is the designed behaviour, but it leaves every "+
				"container assertion in this file unverified here:\n%v", err)
		}
		t.Fatalf("start: %v", err)
	}
	return h
}

// waitForPodmanWorker polls `list` for one Worker. Polling because a container appearing in and
// leaving `podman ps` is not synchronous with the command that caused it; the assertions are on WHAT
// is found, never on how long it took.
//
// IT MATCHES ON THE PAIR, NEVER ON THE NAME ALONE, and the reason is a bug this file briefly had.
// `list()` reports every kontra Worker on the BOX, not this process's — that is rule 3, and it is
// the whole point of the driver reading the runtime instead of a file. Making the fixture VERSION
// process-unique stopped two concurrent `cli.test` runs from colliding on a pod name, but these
// matchers still compared `h.Name == name`, and the NAMES are still shared. So the loud failure
// (`pod already exists`) became a silent one: process A polling for `podmanstop` would happily
// return process B's Worker and assert against it. Quieter is not fixed.
func waitForPodmanWorker(t *testing.T, d *podmanDriver, name, version string, want func(workerHandle) bool, what string) workerHandle {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last []workerHandle
	for time.Now().Before(deadline) {
		hs, err := d.list(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		last = hs
		for _, h := range hs {
			if h.Name == name && h.Version == version && want(h) {
				return h
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("list never reported %s: %s\n  it reported: %v", name, what, last)
	return workerHandle{}
}

// podmanLogsContaining waits for a half's container to have printed something and returns the log.
// The workload in these tests exits as soon as it has answered, and podman keeps the log afterwards —
// which is the asymmetry driver_process.go names and this driver relies on.
func podmanLogsContaining(t *testing.T, d *podmanDriver, h workerHandle, want string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		rc, err := d.logs(context.Background(), h)
		if err == nil {
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			body = string(b)
			if strings.Contains(body, want) {
				return body
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the Worker's log never contained %q:\n%s", want, body)
	return ""
}

// probeSpec is a Worker whose two halves both run the probe in one mode.
//
// Argv[0] IS THE EXECUTABLE, which is what `ProcSpec` means everywhere else — serve.go builds
// `{filepath.Join(absActor, m.Name)}` and `{"go", "run", "."}`. It is spelled out here because the
// driver now passes Argv[0] as `--entrypoint`, so a spec that listed only arguments would be asking
// podman to exec a mode name.
func probeSpec(name, version, image string, args ...string) Spec {
	p := ProcSpec{Argv: append([]string{"/probe"}, args...)}
	return Spec{Name: name, Version: version, Image: image, Actor: p, Handler: p}
}

// --- list reads podman ---------------------------------------------------------------------------

// A WORKER THIS DRIVER DID NOT START IS STILL A WORKER ON THIS MACHINE. driver.go calls a driver's
// memory of what it started a CACHE and the runtime the truth, and this is the half of that rule a
// memory-backed driver fails silently: reconcile sees nothing, starts a SECOND Worker, and the queue
// has two rival pollers.
//
// The container below is started by `podman run` DIRECTLY — the driver is never told about it, and a
// fresh driver value is used so there is no map that could have remembered it even in principle.
func TestPodmanListReportsAWorkerThisDriverDidNotStart(t *testing.T) {
	d := requirePodman(t)
	image := probeImage(t)

	name, version := "podmanghost", fixtureVersion("9.9.9")
	pod := podmanPod(name, version)
	cleanupPod(t, name, version)

	// Created the way an operator or a previous Warden would have: podman's own CLI, this driver
	// absent from the story entirely.
	if out, err := exec.Command("podman", "pod", "create", "--name", pod).CombinedOutput(); err != nil {
		t.Skipf("cannot create a pod here, so the runtime cannot be read: %v: %s", err, out)
	}
	create := exec.Command("podman", "run", "--detach", "--pod", pod,
		"--name", podmanContainer(name, version, partActor),
		"--label", workerLabelVar+"="+workerLabel(name, version, partActor),
		"--entrypoint", "/probe", image)
	out, err := create.CombinedOutput()
	if err != nil {
		t.Fatalf("starting the out-of-band container: %v: %s", err, out)
	}
	outOfBand := cliutil.FirstLine(string(out))

	h := waitForPodmanWorker(t, d, name, version,
		func(h workerHandle) bool { _, ok := h.half(partActor); return ok },
		"a Worker started outside the driver, with its actor half")

	half, _ := h.half(partActor)
	if !strings.HasPrefix(outOfBand, half.Ref) && !strings.HasPrefix(half.Ref, outOfBand) {
		t.Errorf("the handle names container %s; the container the driver never started is %s", half.Ref, outOfBand)
	}
	if h.Driver != "podman" {
		t.Errorf("handle carries driver %q, want podman", h.Driver)
	}
	// ONE HALF IS A REAL STATE and the handle has to be able to say so. This is also the assertion
	// that a one-container-per-PAIR driver could not make at all — see driver_podman.go's header on
	// why worker-entrypoint.sh's shape was not carried over.
	if h.whole() {
		t.Errorf("a Worker with only an actor reported as whole: %v", h)
	}
}

// AND ONE THAT DIED IS GONE, WITH NOTHING TO INVALIDATE. The driver started this pair itself, so a
// driver answering from memory would keep reporting it forever — reconcile satisfied while the queue
// has had no poller for an hour.
func TestPodmanListForgetsAWorkerThatDied(t *testing.T) {
	d := requirePodman(t)
	image := probeImage(t)

	name, version := "podmandies", fixtureVersion("0.0.1")
	h := startPodmanWorker(t, d, probeSpec(name, version, image))
	waitForPodmanWorker(t, d, name, version, workerHandle.whole, "the pair it just started")

	// KILLED BEHIND THE DRIVER'S BACK, which is what an OOM kill, a segfault and an operator's
	// `podman rm -f` all look like from here.
	//
	// The exit status is deliberately ignored: measured on this box, `podman rm -f` prints "unable to
	// signal init: permission denied" and a libpod timeout while removing the container correctly.
	// What the container runtime DID is read from `list` below, not from this command's opinion of
	// itself — the same rule the driver's own `stop` follows, and for the same reason.
	_ = exec.Command("podman", "rm", "--force", podmanContainer(name, version, partActor)).Run()

	got := waitForPodmanWorker(t, d, name, version,
		func(h workerHandle) bool { _, ok := h.half(partActor); return !ok },
		"the pair with its actor gone")
	if _, ok := got.half(partHandler); !ok {
		t.Errorf("the surviving handler half was dropped too: %v", got)
	}

	_ = exec.Command("podman", "rm", "--force", podmanContainer(name, version, partHandler)).Run()
	deadline := time.Now().Add(30 * time.Second)
	for {
		hs, err := d.list(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		still := false
		for _, x := range hs {
			if x.Name == name && x.Version == version {
				still = true
			}
		}
		if !still {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("list still reports %s after both halves are gone: %v", name, hs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = h
}

// STOP ENDS BOTH HALVES, and `list` is what says so — asking the driver whether it thinks it stopped
// something is the question that always answers yes.
func TestPodmanStopEndsBothHalvesOfThePair(t *testing.T) {
	d := requirePodman(t)
	image := probeImage(t)

	name, version := "podmanstop", fixtureVersion("0.0.3")
	h := startPodmanWorker(t, d, probeSpec(name, version, image))
	waitForPodmanWorker(t, d, name, version, workerHandle.whole, "the pair it just started")

	if err := d.Stop(context.Background(), h, 2*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	hs, err := d.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, x := range hs {
		if x.Name == name && x.Version == version {
			t.Errorf("%s survived stop: %v", name, x)
		}
	}
}

// STOP-THEN-START IS THE MAIN LINE, NOT A CORNER. It is how a Worker is moved to a new digest, so a
// reconcile loop does it constantly — and this is the test that found the bug that made it impossible.
//
// `podman pod rm --force` exits 0 having removed both containers and LEFT THE POD in `Created` with
// zero containers. Every container-level check agreed the Worker was gone — `list` reads containers —
// and the next `start` died on `adding pod to state: name "kontra-…" is in use: pod already exists`.
// A Warden could have stopped each Worker exactly once.
//
// THE FIRST START IS ASSERTED TOO, so that a driver which simply cannot start anything does not pass
// this by failing early and identically at both ends.
func TestPodmanAWorkerCanBeStartedAgainAfterItIsStopped(t *testing.T) {
	d := requirePodman(t)
	image := probeImage(t)

	name, version := "podmanrestart", fixtureVersion("0.0.6")
	spec := probeSpec(name, version, image)

	h := startPodmanWorker(t, d, spec)
	waitForPodmanWorker(t, d, name, version, workerHandle.whole, "the pair on its FIRST start")
	if err := d.Stop(context.Background(), h, time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The whole claim: the runtime is left in a state a start can happen in again.
	if _, err := d.Start(context.Background(), spec); err != nil {
		t.Fatalf("a stopped Worker could not be started again — stop left the runtime holding "+
			"something: %v", err)
	}
	waitForPodmanWorker(t, d, name, version, workerHandle.whole, "the pair on its SECOND start")
}

// THE SPEC'S COMMAND RUNS, NOT THE IMAGE'S ENTRYPOINT — and the whole pair-as-unit design rests on it.
//
// `podman run IMAGE cmd…` replaces CMD and leaves ENTRYPOINT in charge. Every kontra worker image
// declares one (`ENTRYPOINT ["/kontra/entrypoint.sh"]`, deploy.go), and that script is the supervisor
// that starts BOTH halves — so without `--entrypoint` the driver produces two containers each running
// the whole Worker. Four processes, two rival pollers on every queue, and a `list` that reports a
// tidy, correct-looking pair the entire time.
//
// FOUND BY POINTING THE DRIVER AT A REAL IMAGE, not by this test — which is the lesson. Driving
// `layer-scan@0.3.0` with an actor half of `python3 /actor/layer-scan/actor.py` logged
// `[worker] started: redis=shared placement=10 daprd=18 host=19 handler=29` in BOTH containers. The
// fixture at the time had a bare `/probe` entrypoint, so it swallowed the spec's argv as arguments and
// behaved correctly by accident: a test that supplied the one input the bug could not appear on. The
// fixture now declares an entrypoint the driver has to override, so this cannot recur silently.
func TestPodmanRunsTheSpecsCommandAndNotTheImagesEntrypoint(t *testing.T) {
	d := requirePodman(t)
	image := probeImage(t)

	// THE FIXTURE MUST ACTUALLY DECLARE ONE, or this test proves nothing at all.
	ep, err := exec.Command("podman", "image", "inspect", image, "--format", "{{.Config.Entrypoint}}").Output()
	if err != nil {
		t.Fatalf("podman image inspect: %v", err)
	}
	if !strings.Contains(string(ep), "image-entrypoint") {
		t.Fatalf("the fixture image declares entrypoint %q, which the driver would not have to override "+
			"— nothing here is tested", strings.TrimSpace(string(ep)))
	}

	name, version := "podmanentry", fixtureVersion("0.0.9")
	h := startPodmanWorker(t, d, probeSpec(name, version, image, "dial", "/run/podman/podman.sock"))
	body := podmanLogsContaining(t, d, h, "/run/podman/podman.sock")

	if strings.Contains(body, "IMAGE-ENTRYPOINT-RAN") {
		t.Errorf("the image's own ENTRYPOINT ran instead of the spec's command. For a kontra worker "+
			"image that entrypoint is the supervisor that starts BOTH halves, so each of the two "+
			"containers would be running a whole Worker — two rival pollers per queue:\n%s", body)
	}
	if !strings.Contains(body, "refused /run/podman/podman.sock") {
		t.Errorf("the command the spec asked for did not run:\n%s", body)
	}
}

// --- the runtime socket --------------------------------------------------------------------------

// THE ASSERTION THIS SLICE EXISTS FOR, and the one the issue says to make rather than assume: an
// actor cannot reach the container runtime. Mounting the socket is not a smaller boundary, it is
// none — a workload that can talk to podman can start a container with `-v /:/host`.
//
// Read this file's header for why the two controls come first. Briefly: without them, this test
// passes against a broken probe, an absent socket, and a correct driver alike.
func TestPodmanWorkerCannotReachTheContainerRuntimeSocket(t *testing.T) {
	d := requirePodman(t)
	image := probeImage(t)

	// CONTROL 1 — the sockets are real and answering, here, now. Whichever ones do are the ones this
	// test is entitled to make a claim about; a path that answers nothing from the host proves
	// nothing when it answers nothing from a container.
	var live []string
	for _, s := range runtimeSockets {
		c, err := net.DialTimeout("unix", s, 2*time.Second)
		if err != nil {
			continue
		}
		_ = c.Close()
		live = append(live, s)
	}
	if len(live) == 0 {
		t.Skip("no container runtime socket answers a dial from this host, so a container failing to " +
			"reach one would prove nothing about the driver")
	}
	t.Logf("control 1: these sockets answer from the host: %v", live)

	// CONTROL 2 — the probe can DETECT a reachable socket from inside a container. Run deliberately
	// with the socket bind-mounted, which is precisely the mistake the driver must never make. This is
	// what turns "refused" below into evidence instead of a tautology.
	//
	// `--entrypoint` here for the same reason the driver passes one: the fixture image declares an
	// ENTRYPOINT on purpose, so without it this would run the marker mode instead of dialling — and
	// the control would report the probe as broken when it is fine.
	mount := live[0] + ":" + live[0]
	out, err := exec.Command("podman", "run", "--rm", "-v", mount, "--entrypoint", "/probe",
		image, "dial", live[0]).CombinedOutput()
	if err != nil {
		t.Fatalf("the positive control could not run: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "REACHED "+live[0]) {
		t.Fatalf("the probe did not reach %s even with the socket MOUNTED, so it cannot detect a "+
			"reachable socket and the assertion below would be vacuous:\n%s", live[0], out)
	}
	t.Logf("control 2: with the socket mounted on purpose, the probe reaches it — the probe works")

	// THE CLAIM — the same probe, the same sockets, in a Worker this driver started.
	name, version := "podmansock", fixtureVersion("0.0.2")
	argv := append([]string{"dial"}, live...)
	h := startPodmanWorker(t, d, probeSpec(name, version, image, argv...))

	body := podmanLogsContaining(t, d, h, live[0])
	for _, s := range live {
		if strings.Contains(body, "REACHED "+s) {
			t.Errorf("a Worker this driver started REACHED the container runtime at %s — that is root "+
				"on the Machine, handed to code kontra did not write:\n%s", s, body)
		}
		if !strings.Contains(body, "refused "+s) {
			t.Errorf("the Worker never reported on %s at all, so nothing was tested for it:\n%s", s, body)
		}
	}
}

// AND THE ARGV GUARD, which is the other half: the test above catches a socket that IS exposed, and
// this catches the moment somebody writes the flag that would expose one. Both are wanted — the guard
// fires in a unit test on every run, where the live assertion needs a working podman.
func TestPodmanRefusesToBuildAnArgvThatExposesTheRuntime(t *testing.T) {
	for _, bad := range [][]string{
		{"run", "-v", "/run/podman/podman.sock:/run/podman/podman.sock"},
		{"run", "-v", "/var/run/docker.sock:/var/run/docker.sock:ro"},
		{"run", "--mount", "type=bind,source=/var/run/docker.sock,destination=/sock"},
		{"run", "--network=host"},
		{"run", "--net=host"},
	} {
		if err := assertNoRuntimeAccess(bad); err == nil {
			t.Errorf("assertNoRuntimeAccess(%v) allowed an argv that hands the workload the Machine", bad)
		}
	}
	// AND IT MUST NOT REFUSE EVERYTHING, or it would be a guard that cannot distinguish and the tests
	// above would pass against a driver that can start nothing at all.
	ok := []string{"run", "--detach", "--pod", "kontra-nscheck-0.1.0", "--cap-drop", "ALL",
		"--env", "KONTRA_ADDRESS=controller:7233", "localhost/nscheck@sha256:" + strings.Repeat("a", 64)}
	if err := assertNoRuntimeAccess(ok); err != nil {
		t.Errorf("assertNoRuntimeAccess refused an ordinary Worker argv: %v", err)
	}
}

// A WORKLOAD MAY NAME THE SOCKET; IT MAY NOT BE GIVEN ONE. The distinction is the whole reason the
// guard is aimed at the flags and stops at the image reference, and it was found the hard way: the
// first version scanned the entire argv, so the socket test — whose workload is literally
// `dial /run/podman/podman.sock` — was refused by the driver it was testing. A guard that fires on a
// string in a workload's arguments is one that gets loosened by whoever hits it next.
//
// Past `IMAGE`, podman is not parsing flags any more, so a path there cannot become a mount however
// it is spelled.
func TestPodmanPassesAWorkloadArgvThatMerelyNamesTheRuntimeSocket(t *testing.T) {
	d := &podmanDriver{bin: "podman", userns: podmanUserns{how: "auto", args: []string{"--userns=auto"}}}
	digest := "sha256:" + strings.Repeat("a", 64)
	image := "localhost/probe@" + digest

	spec := Spec{Name: "sockargv", Version: "0.0.8", Image: image}
	args, err := d.runArgs(podmanPod("sockargv", "0.0.8"), spec, partActor,
		ProcSpec{Argv: []string{"/probe", "dial", "/run/podman/podman.sock"}}, digest)
	if err != nil {
		t.Fatalf("the driver refused a workload whose ARGUMENT names a socket: %v", err)
	}

	at := -1
	for i, a := range args {
		if a == image {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("the image reference is missing from the argv: %v", args)
	}

	// ARGV[0] IS THE ENTRYPOINT and everything after it lands past the image, which is what keeps the
	// workload's arguments out of the section podman parses as flags.
	if args[at-2] != "--entrypoint" || args[at-1] != "/probe" {
		t.Errorf("the executable is not passed as --entrypoint, so the image's own would run: %v", args[at-2:at])
	}
	for _, a := range args[:at] {
		if strings.Contains(a, ".sock") {
			t.Errorf("a socket path appears in the FLAGS, where podman would act on it: %q", a)
		}
	}
	if got := strings.Join(args[at+1:], " "); got != "dial /run/podman/podman.sock" {
		t.Errorf("the workload's arguments after the image = %q, want them passed through untouched", got)
	}
}

// --- not host root -------------------------------------------------------------------------------

// THE WORKLOAD MUST NOT BE HOST ROOT, and /proc/self/uid_map is the mapping itself rather than a
// proxy for it. Measured on this box, the two possible answers:
//
//	0          0 4294967295   container root IS host root — a bind mount is written as uid 0
//	0     524288      65534   container root is host 524288 — an unprivileged id that owns nothing
//
// The filesystem consequence was measured directly while writing the driver: the same container
// wrote a file into a bind mount owned by `0:0` without a namespace and `524288:524288` with one.
// This asserts the mapping rather than the file because the seam has no way to pass a mount, and the
// mapping is the cause of which the ownership was the effect.
func TestPodmanWorkloadIsNotHostRoot(t *testing.T) {
	d := requirePodman(t)
	image := probeImage(t)

	name, version := "podmanuid", fixtureVersion("0.0.4")
	h := startPodmanWorker(t, d, probeSpec(name, version, image, "uidmap"))
	body := podmanLogsContaining(t, d, h, "uidmap")

	var checked int
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		// `uidmap <container-base> <host-base> <count>`
		if len(f) != 4 || f[0] != "uidmap" {
			continue
		}
		checked++
		if f[1] != "0" {
			t.Errorf("container uid 0 is not the mapped id (%q) — this test is reading the wrong line: %q", f[1], line)
			continue
		}
		if f[2] == "0" {
			t.Errorf("the Worker's uid 0 maps to HOST uid 0: a container escape, or any bind mount, is "+
				"root on the Machine. driver_podman.go promises a user namespace and this Worker has "+
				"none:\n%s", body)
		}
	}
	if checked == 0 {
		t.Fatalf("no uid_map line was found in the Worker's log, so nothing was asserted:\n%s", body)
	}
}

// --- the image is pinned -------------------------------------------------------------------------

// A TAG IS REFUSED, NOT RESOLVED. Two Machines in one **Fleet** that pull `nscheck:0.1.0` an hour
// apart can be running different code, and every fact recorded about the Run becomes a claim about
// when somebody looked.
func TestPodmanRefusesAnImageThatIsNotDigestPinned(t *testing.T) {
	good := "localhost:5000/nscheck@sha256:" + strings.Repeat("a", 64)
	got, err := ociref.ImageDigest(good)
	if err != nil {
		t.Fatalf("a digest-pinned reference was refused: %v", err)
	}
	if got != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("digest = %q, want the sha256 out of the reference", got)
	}

	for _, bad := range []string{
		"nscheck",                      // a bare name
		"nscheck:0.1.0",                // A TAG — the case this exists for
		"localhost:5000/nscheck:0.1.0", // a tag with a registry, which looks the most legitimate
		"localhost:5000/nscheck",       // implicitly :latest, the worst of them
		"localhost/nscheck@sha256:" + strings.Repeat("a", 63), // one hex short
		"localhost/nscheck@sha256:" + strings.Repeat("A", 64), // uppercase is not a digest
		"localhost/nscheck@md5:" + strings.Repeat("a", 32),    // a different algorithm
	} {
		if _, err := ociref.ImageDigest(bad); !errors.Is(err, ociref.ErrImageUnpinned) {
			t.Errorf("ociref.ImageDigest(%q) = %v, want ociref.ErrImageUnpinned", bad, err)
		}
	}
	if _, err := ociref.ImageDigest(""); err == nil {
		t.Error("ociref.ImageDigest(\"\") accepted a spec with no Image at all")
	}
}

// TWO REFUSALS FOR TWO OPPOSITE TRUTHS, AND THEY MUST NOT BE THE SAME MESSAGE.
//
// kontra names **Actors** more freely than OCI names repositories. `shared/conformance/queues.json` pins
// `a/b`, `my actor` and `café` as names that must keep working — "A QUEUE NAME IS NOT SANITISED" — and
// Temporal accepts all three, so they serve happily under `process`, which runs from source and never
// produces an **Artifact**. The OCI grammar accepts one of the three. The other two name a class of
// Worker that NO image driver can ever place, and an operator told "not digest-pinned" would go and
// pin it, and it would fail again identically.
//
// Every expectation below was checked against podman 4.3.1 rather than reasoned about, because the
// runtime is the thing that decides:
//
//	localhost:5000/café:0.1.0     invalid reference format             no remedy
//	kontra/café-worker:0.1.0      invalid reference format             no remedy
//	localhost:5000/Foo:1.0        repository name must be lowercase    no remedy
//	localhost:5000/my actor:1.0   invalid reference format             no remedy
//	localhost:5000/a/b:1.0.0      reading manifest … not found         DEPLOY FIXES IT
//
// `cli/scale.go:242` gives one message for both today (`.scratch/warden/issues/15-*`, not fixed here).
func TestPodmanTellsAnUnpinnedImageApartFromAnUnnameableOne(t *testing.T) {
	pin := "@sha256:" + strings.Repeat("a", 64)

	// NOTHING FIXES THESE. Each is given BOTH tagged and digest-pinned, because the trap in the obvious
	// implementation is a check that looks only for a digest: `localhost:5000/café@sha256:…` is
	// perfectly pinned and still cannot be pulled, so a driver that accepted it would have moved the
	// failure one step later — into podman's own wording, at start time, on a Machine.
	for _, ref := range []string{
		"localhost:5000/café:0.1.0", // the REMOTE form
		"localhost:5000/café" + pin, // …pinned, and still dead
		"kontra/café-worker:0.1.0",  // the LOCAL form: different prefix AND suffix, same verdict
		"kontra/café-worker" + pin,
		"localhost:5000/Foo:1.0", // uppercase: podman says "repository name must be lowercase"
		"localhost:5000/Foo" + pin,
		"localhost:5000/my actor:1.0",
		"localhost:5000/my actor" + pin,
	} {
		_, err := ociref.ImageDigest(ref)
		if !errors.Is(err, ociref.ErrImageUnrepresentable) {
			t.Errorf("ociref.ImageDigest(%q) = %v, want ociref.ErrImageUnrepresentable — this Actor can never be "+
				"placed from an image and the message must say so", ref, err)
			continue
		}
		if errors.Is(err, ociref.ErrImageUnpinned) {
			t.Errorf("ociref.ImageDigest(%q) reported BOTH refusals, so a caller cannot tell them apart", ref)
		}
		// The remedy that does not exist must not be offered.
		if strings.Contains(err.Error(), "kontra build --push") {
			t.Errorf("ociref.ImageDigest(%q) offered pinning as the fix for a reference no pin can save:\n%v", ref, err)
		}
	}

	// AND THESE ARE MERELY UNPINNED — a slash is a legal path separator, so `a/b` is a good repository
	// whose image has not been pushed yet. A check that refused it would be exactly as wrong as one
	// that accepted `café`, which is why it is here rather than in the list above.
	for _, ref := range []string{
		"a/b:1.0.0",
		"localhost:5000/a/b:1.0.0",
		"localhost:5000/nscheck:0.1.0",
	} {
		_, err := ociref.ImageDigest(ref)
		if !errors.Is(err, ociref.ErrImageUnpinned) {
			t.Errorf("ociref.ImageDigest(%q) = %v, want ociref.ErrImageUnpinned — this reference is well formed and "+
				"pinning it is a real remedy", ref, err)
		}
		if errors.Is(err, ociref.ErrImageUnrepresentable) {
			t.Errorf("ociref.ImageDigest(%q) told the operator their Actor can never run, when all it needs "+
				"is a digest", ref)
		}
	}

	// …and the same names, pinned, are accepted — otherwise every assertion above would hold against a
	// function that refuses everything.
	for _, ref := range []string{"a/b" + pin, "localhost:5000/a/b" + pin, "localhost:5000/nscheck" + pin} {
		if _, err := ociref.ImageDigest(ref); err != nil {
			t.Errorf("ociref.ImageDigest(%q) refused a well-formed pinned reference: %v", ref, err)
		}
	}
}

// A SPEC WITH NO IMAGE IS REFUSED BEFORE ANYTHING IS CREATED, so a Warden misconfiguration does not
// leave a pod behind for the next `list` to report as a Worker nobody started.
func TestPodmanStartRefusesAnUnpinnedSpecWithoutCreatingAnything(t *testing.T) {
	d := requirePodman(t)

	name, version := "podmanunpinned", fixtureVersion("0.0.7")
	cleanupPod(t, name, version)

	if _, err := d.Start(context.Background(), probeSpec(name, version, "kontra-slice02-probe:test")); err == nil {
		t.Fatal("start accepted a tag")
	}
	out, _ := exec.Command("podman", "pod", "ps", "--format", "{{.Name}}").Output()
	if strings.Contains(string(out), podmanPod(name, version)) {
		t.Errorf("the refused start left pod %s behind:\n%s", podmanPod(name, version), out)
	}
}

// --- slice 14: trust policy, at the place the image is actually pulled -------------------------------

// A NON-ALLOWLISTED REGISTRY IS REFUSED WITHOUT PODMAN BEING RUN AT ALL.
//
// THE ASSERTION IS "THE RUNTIME WAS NEVER INVOKED", not "an error was returned", and the difference is
// the whole slice. `podman run` pulls, so a gate that fired after podman was reached would already
// have made a DNS lookup and a TLS ClientHello to a host nobody allowlisted — telling whoever owns
// that name this Machine's address, its clock, and that something told it to go there. An error value
// cannot distinguish the two orders; an empty invocation log can.
//
// THE STUB IS THE INSTRUMENT AND NOT A CONVENIENCE. Pointing `bin` at a recording script is what makes
// "nothing was pulled" observable, and it keeps this test hermetic in the one direction that matters:
// a REGRESSION here cannot reach ghcr.io, because the only thing that would run is a shell script in a
// temp directory.
func TestPodmanNeverReachesTheRuntimeForARegistryThatIsNotAllowlisted(t *testing.T) {
	log, d := stubbedPodman(t, fixtureTrust(t))

	image := "ghcr.io/acme/bundles/nscheck@sha256:" + strings.Repeat("a", 64)
	_, err := d.Start(context.Background(), probeSpec("notallowed", "0.0.14", image))
	if err == nil {
		t.Fatal("a Worker was started from a registry this Machine does not pull from")
	}
	if !errors.Is(err, trustpolicy.ErrRegistryNotAllowed) {
		t.Errorf("start = %v, want trustpolicy.ErrRegistryNotAllowed", err)
	}
	if got := log(t); got != "" {
		t.Errorf("podman was run before the registry was judged, so this Machine contacted a host nobody "+
			"allowlisted:\n%s", got)
	}
	// The refusal has to name the address it is protecting, or an operator reads it as a pull failure.
	for _, want := range []string{"ghcr.io", "localhost", "trust-registries"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}

	// THE CONTROL: the same driver, the same policy, an image from the allowlisted registry — and now
	// podman IS run. Without it, "the log is empty" passes against a stub that never records, a driver
	// that refuses everything, and a `start` that returns early for an unrelated reason.
	ok := "localhost/kontra-slice02-probe@sha256:" + strings.Repeat("b", 64)
	if _, err := d.Start(context.Background(), probeSpec("allowed", "0.0.14", ok)); err != nil {
		t.Fatalf("an image from the allowlisted registry was refused: %v", err)
	}
	if got := log(t); !strings.Contains(got, "pod create") {
		t.Errorf("podman was not run for an allowed image, so the emptiness asserted above proves "+
			"nothing:\n%s", got)
	}
}

// AN UNSIGNED IMAGE IS REFUSED BEFORE ITS LAYERS LAND ON THE MACHINE.
//
// This refusal necessarily costs a request — the signature lives in the registry, so learning it is
// absent means asking. What it must NOT cost is the pull: cosign reads a signature manifest and a
// ~1 KiB payload blob, and the image behind it can be hundreds of megabytes (ADR 0036 measures a
// ~900 MB actor image). So the ordering assertion here is the same shape as the one above with one
// gate moved: cosign ran, podman did not.
func TestPodmanNeverPullsAnImageThatIsNotSigned(t *testing.T) {
	cosign := cosignstub.Stub(t, 1, "Error: no matching signatures")
	trust, err := trustpolicy.Load(trustpolicy.Options{Registries: "localhost", Key: "/keys/release.pub"})
	if err != nil {
		t.Fatal(err)
	}
	log, d := stubbedPodman(t, trust)

	image := "localhost/kontra-slice02-probe@sha256:" + strings.Repeat("a", 64)
	_, err = d.Start(context.Background(), probeSpec("unsigned", "0.0.14", image))
	if err == nil {
		t.Fatal("an unsigned Worker image was started")
	}
	if !errors.Is(err, trustpolicy.ErrUnsigned) {
		t.Errorf("start = %v, want trustpolicy.ErrUnsigned", err)
	}
	if got := log(t); got != "" {
		t.Errorf("podman was run for an image no signature vouches for, so the bytes were pulled before "+
			"the question was asked:\n%s", got)
	}
	// The gate really was reached — otherwise "podman was not run" is satisfied by the allowlist
	// refusing first, which is a different test.
	if !strings.Contains(cosign(t), "verify") {
		t.Error("cosign was never asked, so this test is the allowlist test with extra steps")
	}

	// THE CONTROL: a cosign that verifies, and the same image starts.
	cosignstub.Stub(t, 0, "")
	if _, err := d.Start(context.Background(), probeSpec("signed", "0.0.14", image)); err != nil {
		t.Fatalf("a signed image was refused: %v", err)
	}
	if got := log(t); !strings.Contains(got, "pod create") {
		t.Errorf("podman was not run for a verified image:\n%s", got)
	}
}

// AND AGAINST THE REAL RUNTIME: a refused start leaves nothing behind.
//
// The two tests above prove the ORDER with a stub. This one proves the CONSEQUENCE with podman itself,
// which is the property `TestPodmanStartRefusesAnUnpinnedSpecWithoutCreatingAnything` already holds
// for the pin: a refusal must not leave a pod for the next `list` to report as a Worker nobody
// started, and must not leave one for the next `start` to die on with "pod already exists".
func TestPodmanARefusedPlacementLeavesNoPodBehind(t *testing.T) {
	d := requirePodmanTrusting(t, fixtureTrust(t))

	for _, k := range []struct {
		name, image string
		want        error
	}{
		{"trustregistry", "ghcr.io/acme/bundles/nscheck@sha256:" + strings.Repeat("a", 64), trustpolicy.ErrRegistryNotAllowed},
		{"trustunpinned", "localhost/kontra-slice02-probe:test", ociref.ErrImageUnpinned},
	} {
		version := fixtureVersion("0.0.14")
		cleanupPod(t, k.name, version)
		if _, err := d.Start(context.Background(), probeSpec(k.name, version, k.image)); !errors.Is(err, k.want) {
			t.Errorf("start(%s) = %v, want %v", k.image, err, k.want)
		}
		out, _ := exec.Command("podman", "pod", "ps", "--format", "{{.Name}}").Output()
		if strings.Contains(string(out), podmanPod(k.name, version)) {
			t.Errorf("the refused start left pod %s behind:\n%s", podmanPod(k.name, version), out)
		}
		// AND NOTHING WAS FETCHED. A refused reference must not appear in this Machine's image store —
		// which is what would be there if the gate had run after the pull rather than before it.
		if k.want == trustpolicy.ErrRegistryNotAllowed {
			out, _ := exec.Command("podman", "images", "--format", "{{.Repository}}").Output()
			if strings.Contains(string(out), "ghcr.io/acme/bundles/nscheck") {
				t.Errorf("a refused image is in this Machine's image store, so it was pulled first:\n%s", out)
			}
		}
	}
}

// stubbedPodman is a driver whose runtime is a recording shell script, plus a reader for the log.
//
// It returns the reader FIRST because every caller reads it more than it builds one, and reading it
// TRUNCATES: a control assertion after a refusal needs "what happened since", not "everything so far".
func stubbedPodman(t *testing.T, trust trustpolicy.Policy) (func(*testing.T) string, *podmanDriver) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "podman")
	// `run` prints a container id because `start` reads one off stdout; everything else exits 0. The
	// script is deliberately dumb — what it is instrumenting is whether it was called at all.
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\n[ \"$1\" = run ] && echo %s\nexit 0\n",
		log, strings.Repeat("c", 64))
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &podmanDriver{bin: bin, userns: podmanUserns{how: "auto", args: []string{"--userns=auto"}}, trust: trust}
	return func(t *testing.T) string {
		t.Helper()
		b, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		_ = os.Remove(log)
		return string(b)
	}, d
}

// --- the two drivers do not claim each other's Workers ---------------------------------------------

// NEITHER MAY SEE THE OTHER (driver.go: "`podman` arrives beside it in slice 02 and neither may see
// the other"). The hazard is one-directional and specific: a rootful podman's container processes are
// visible in the HOST's process table, so if this driver put KONTRA_WORKER in the container's
// ENVIRONMENT as well as on its label, the process driver's /proc scan would claim every containerised
// Worker on the Machine — two drivers reporting one Worker, and a reconcile loop stopping it through
// the wrong one.
func TestPodmanWorkerIsNotClaimedByTheProcessDriver(t *testing.T) {
	requireProcTable(t)
	d := requirePodman(t)
	image := probeImage(t)

	name, version := "podmanonly", fixtureVersion("0.0.5")
	startPodmanWorker(t, d, probeSpec(name, version, image))
	waitForPodmanWorker(t, d, name, version, workerHandle.whole, "the containerised pair")

	// THE CONTROL: the process driver's scan is working right now, in this test, and would have found
	// a labelled process if there were one. Without this the assertion below passes on any machine
	// where the scan is broken, returns nothing, or was never reached.
	startLabelled(t, "procvisible", fixtureVersion("0.0.5"), partActor, "sleep", "600")
	proc := &ProcessDriver{out: os.Stdout, err: os.Stderr}
	waitForWorker(t, proc, "procvisible", fixtureVersion("0.0.5"),
		func(h workerHandle) bool { _, ok := h.half(partActor); return ok },
		"a labelled bare process, proving the /proc scan works in this run")

	hs, err := proc.list(context.Background())
	if err != nil {
		t.Fatalf("process list: %v", err)
	}
	for _, h := range hs {
		if h.Name == name && h.Version == version {
			t.Errorf("the `process` driver claimed a Worker podman is holding (%v) — the label must be a "+
				"podman label and must NOT be in the container's environment", h)
		}
	}
}
