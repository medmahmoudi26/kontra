package main

// warden_packing_test.go — SEVERAL WORKERS ON ONE MACHINE, and the address they share (ADR 0037).
//
// ═══ WHAT THIS FILE MEASURES, AND WHY IT HAS TO BE MEASURED ═══
//
// ADR 0037 states the trade in one sentence: *"Several Workers may share a Machine, and they share
// its egress address. This is the whole reason the container Target exists (0036) and it is a real
// loss: the unique-source-address property that `subfinder` depends on stops being automatic."*
//
// The second half of that sentence is a claim about the network, and it is NOT self-evident under
// the `podman` driver. Each Worker is a pod with its own address on the Machine's bridge, so two of
// them genuinely have two different addresses — and a reader who stopped there would conclude that
// packing costs nothing, which is exactly backwards. What is shared is the address their traffic
// WEARS once it has left the bridge, after the Machine has masqueraded it, and the only place that
// answer exists is at the destination.
//
// So the measurement is: two Workers, one Machine, one destination off their bridge, and the source
// address it reports for each. MEASURED on this checkout, podman 4.3.1 — two Workers with distinct
// container addresses arriving as ONE address, `172.16.16.1`, the Machine's own on the route to that
// destination. The control is in the same test and it is what makes the measurement worth having:
// two Workers reaching a destination on THEIR OWN bridge arrive as two different addresses, so the
// instrument demonstrably reports a difference when there is one.
//
// ═══ THE CHEAP HALF IS ABOVE THE RUNTIME ═══
//
// Whether the reconcile loop will hold several Workers at once needs no container at all, and the
// tests that ask it run under the `process` driver in milliseconds. They are here rather than in
// warden_test.go because they are about one property — that a Machine is now plural — and because
// one of them pins the reason a placement can never ask for two Workers of one Artifact on one
// Machine, which is the refusal `f.place(workers=…)` inherits at the other end of the system.

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// --- above the runtime: the loop holds several Workers ------------------------------------------

// TestTheWardenHoldsSeveralWorkersAtOnce is packing at the level the Warden decides it.
//
// The loop was written for a list from its first line, so this is a property to PIN rather than to
// add: the failure it guards is a future turn that treats "the assignment" as one Worker, which
// would read as a Machine that quietly runs whichever entry sorted first.
func TestTheWardenHoldsSeveralWorkersAtOnce(t *testing.T) {
	w := testWarden(t, "pack-")
	want := []workerSpec{spec(t, "pack-nscheck", "0.1.0"), spec(t, "pack-subfinder", "0.2.0")}

	hs := turn(t, w, want, "both Workers are whole on this Machine", func(hs []workerHandle) bool {
		return len(hs) == 2 && hs[0].whole() && hs[1].whole()
	})
	if got := ids(hs); len(got) != 2 || got[0] != "pack-nscheck@0.1.0" || got[1] != "pack-subfinder@0.2.0" {
		t.Fatalf("this Machine holds %v, want both Workers", got)
	}

	// AND A SECOND TURN CHANGES NOTHING. A loop that packed on the first turn and then stopped one
	// of them on the second — because it read "anything else running is stopped" as "anything but
	// the first" — would still have passed the assertion above.
	turn(t, w, want, "both are still there after a second turn", func(hs []workerHandle) bool {
		return len(hs) == 2
	})
	for _, id := range []string{"pack-nscheck@0.1.0", "pack-subfinder@0.2.0"} {
		if n := w.startsOf(id); n != 1 {
			t.Errorf("%s was started %d times; a packed Worker that is already whole must be left alone", id, n)
		}
	}
}

// TestOneWorkerLeavingAPackedMachineLeavesTheOtherRunning is the co-tenant rule at this level.
//
// It is the same sentence `programs/fleet.ts` obeys one system away: a placement that stops being
// asked for is REMOVED, and removing it must reach exactly one Worker. Here the mechanism is a list
// that got shorter rather than a Pulumi diff, and the failure is the same shape — a Machine that
// went quiet in a way nothing reports, because both sides think they succeeded.
func TestOneWorkerLeavingAPackedMachineLeavesTheOtherRunning(t *testing.T) {
	w := testWarden(t, "shed-")
	stays := spec(t, "shed-nscheck", "0.1.0")
	goes := spec(t, "shed-subfinder", "0.2.0")

	turn(t, w, []workerSpec{stays, goes}, "both are up", func(hs []workerHandle) bool {
		return len(hs) == 2
	})
	hs := turn(t, w, []workerSpec{stays}, "only the one still asked for is up", func(hs []workerHandle) bool {
		return len(hs) == 1
	})
	if got := ids(hs); got[0] != "shed-nscheck@0.1.0" {
		t.Fatalf("the Machine kept %v; the assignment still names shed-nscheck@0.1.0", got)
	}
	if !hs[0].whole() {
		t.Error("the co-tenant survived as half a Worker, which is worse than not surviving at all")
	}
}

// TestTwoWorkersOfOneArtifactOnOneMachineAreNotExpressible is WHY `workers=` cannot exceed the
// Machine count, measured at the seam that makes it true.
//
// `sdk/python/actorkit/fleet.py:place` refuses `workers > machines` and names this as the reason;
// `programs/fleet.ts:machinesFor` refuses it again server-side. Both refusals rest on a fact about
// the label: a Worker is identified by `<name>@<version>`, so two of one Artifact on one Machine are
// one entry to every reader — `list()` cannot tell them apart and the loop below says so out loud
// rather than starting the second. Without this test the two refusals upstream would be assertions
// about a limit nobody had checked.
func TestTwoWorkersOfOneArtifactOnOneMachineAreNotExpressible(t *testing.T) {
	w := testWarden(t, "dup-")
	// THE LOOP'S OWN DECISIONS, NOT JUST THE RUNTIME'S STATE. Counting driver `start` calls is not
	// enough and a mutation proved it: with the dedup's `continue` deleted, the id lands in the turn's
	// order TWICE, the second pass reaches `start` — and the BACKOFF refuses it, one second after the
	// first. The Machine ends up holding one Worker either way, `startsOf` is 1 either way, and the
	// difference is a restart counter quietly incremented by an assignment bug. So this reads what the
	// loop decided rather than what survived it.
	var log bytes.Buffer
	w.out = &log
	one := spec(t, "dup-nscheck", "0.1.0")
	two := spec(t, "dup-nscheck", "0.1.0")

	hs := turn(t, w, []workerSpec{one, two}, "one Worker, not two", func(hs []workerHandle) bool {
		return len(hs) == 1 && hs[0].whole()
	})
	if got := ids(hs); len(got) != 1 {
		t.Fatalf("this Machine holds %v; two of one actor@version are indistinguishable to `list`", got)
	}
	if n := w.startsOf("dup-nscheck@0.1.0"); n != 1 {
		t.Errorf("started %d times for one duplicated entry; the second is refused, not queued", n)
	}
	// SAID OUT LOUD — silently taking the last one would make a Fleet run something nobody can find
	// in the file.
	if !strings.Contains(log.String(), "names dup-nscheck@0.1.0 twice") {
		t.Errorf("the duplicate was not reported:\n%s", log.String())
	}
	// AND CONSIDERED ONCE. A second pass over the same id is a decision the loop should never take,
	// and the backoff refusing it is not the same thing as never reaching it.
	if n := strings.Count(log.String(), "starting dup-nscheck@0.1.0"); n != 1 {
		t.Errorf("the loop decided to start it %d times in one turn:\n%s", n, log.String())
	}
	if strings.Contains(log.String(), "not restarting") {
		t.Errorf("the duplicate reached the backoff, so it was considered twice:\n%s", log.String())
	}
}

// --- the measurement: one Machine, one source address --------------------------------------------

// TestTwoPackedWorkersShareTheMachinesSourceAddress is ADR 0037's stated cost, measured.
//
// See this file's header for the shape. The two things that make it a measurement rather than a
// restatement are that the destination is OFF the Workers' bridge — so the Machine's masquerade is
// in the path — and that the control in the same test reaches a destination ON it, where the two
// Workers arrive as two addresses. Without the control, a listener that reported one address for
// everything would pass.
//
// IT SKIPS LOUDLY. A silent skip that reads as a pass is the failure this package cares most about,
// so every exit says what was missing and what it would have proved.
func TestTwoPackedWorkersShareTheMachinesSourceAddress(t *testing.T) {
	d := requirePodman(t)
	image := probeImage(t)

	// A NETWORK OF ITS OWN FOR THE DESTINATION, which is the whole apparatus. A listener on the
	// Workers' own bridge is reached without masquerade and would report their container addresses;
	// a listener on this Machine is not reachable at all, because ufw denies container-to-host on
	// every port it has not been told about (warden_egress_test.go measured all eleven of this
	// Controller's private addresses timing out). A second bridge is off the first one, owned by
	// this test, and needs nothing of the host's.
	far := fmt.Sprintf("kontra-pack-far-%d", os.Getpid())
	if out, err := exec.Command(d.bin, "network", "create", far).CombinedOutput(); err != nil {
		t.Skipf("cannot create a second podman network (%v: %s), so what is UNVERIFIED here is that "+
			"two Workers packed onto one Machine share its source address", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = exec.Command(d.bin, "network", "rm", "-f", far).Run() })

	dest, listener := packingListener(t, d, image, far)
	near, nearListener := packingListener(t, d, image, "")

	// THE WORKERS. Started through the driver rather than with a hand-written `podman run`, so the
	// posture is the one a Warden actually gives a Worker — `--cap-drop ALL`, no-new-privileges and
	// whatever user namespace `newPodmanDriver` decided by reading this runtime.
	sources := map[string][]string{}
	for _, target := range []struct {
		what string
		addr string
		on   string
	}{{"off this Machine's bridge", dest, listener}, {"on it", near, nearListener}} {
		for _, name := range []string{"packa", "packb"} {
			runPackedWorker(t, d, image, name, target.addr)
		}
		sources[target.what] = peersOf(t, d, target.on, 2)
	}

	off := sources["off this Machine's bridge"]
	on := sources["on it"]

	// ═══ THE MEASUREMENT ═══
	if len(off) != 2 || off[0] != off[1] {
		t.Fatalf("two Workers packed onto one Machine were seen as %v from off its bridge; ADR 0037 "+
			"says they share its egress address, and a Fleet that packs `subfinder` is relying on the "+
			"OPPOSITE being asked for with spread=", off)
	}

	// ═══ THE CONTROL, WITHOUT WHICH THE MEASUREMENT IS A LISTENER THAT ONLY EVER SAYS ONE THING ═══
	if len(on) != 2 || on[0] == on[1] {
		t.Fatalf("the same two Workers were seen as %v from a destination on their own bridge; this "+
			"instrument has to be able to report two different sources, or the assertion above is "+
			"about the listener rather than about the Machine", on)
	}
	t.Logf("packed Workers arrive as %s from off this Machine's bridge, and as %v from on it",
		off[0], on)
}

// packingListener starts a destination container and returns its address and its container name.
// An empty `network` puts it on the Workers' own bridge, which is the control.
func packingListener(t *testing.T, d *podmanDriver, image, network string) (string, string) {
	t.Helper()
	name := fmt.Sprintf("kontra-pack-dest-%d-%d", os.Getpid(), egressProbeSeq.Add(1))
	t.Cleanup(func() { _ = exec.Command(d.bin, "rm", "-f", "--time", "0", name).Run() })
	const port = "19197"
	args := append([]string{"run", "--detach", "--name", name}, d.userns.args...)
	if network != "" {
		args = append(args, "--network", network)
	}
	args = append(args, "--entrypoint", "/probe", image, "listen", "0.0.0.0:"+port)
	if out, err := exec.Command(d.bin, args...).CombinedOutput(); err != nil {
		t.Skipf("could not start a destination container (%v: %s), so the source address two packed "+
			"Workers wear cannot be measured here", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command(d.bin, "inspect", name,
			"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}").Output()
		ip := strings.TrimSpace(string(out))
		if err == nil && ip != "" {
			if a, perr := netip.ParseAddr(ip); perr == nil && a.IsPrivate() {
				return ip + ":" + port, name
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Skip("the destination container never got a private address, so there is nothing for a Worker " +
		"to be seen arriving from")
	return "", ""
}

// runPackedWorker dials `dest` from a container in the driver's own posture, and insists it arrived.
//
// A REFUSED DIAL IS A SKIP AND NOT A FAILURE, because it says nothing about whether two Workers
// share an address — it says this box could not carry the traffic at all.
func runPackedWorker(t *testing.T, d *podmanDriver, image, label, dest string) {
	t.Helper()
	name := fmt.Sprintf("kontra-pack-%s-%d-%d", label, os.Getpid(), egressProbeSeq.Add(1))
	flags := []string{"run", "--rm", "--name", name}
	flags = append(flags, egressHardening(d)...)
	flags = append(flags, "--entrypoint", "/probe", image, "tcp", dest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, d.bin, flags...).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "REACHED "+dest) {
		t.Skipf("a Worker could not reach %s (%v: %s), so what is UNVERIFIED here is the address it "+
			"would have arrived from", dest, err, strings.TrimSpace(string(out)))
	}
}

// peersOf reads the source addresses a destination reported, waiting for `want` of them.
//
// THE PORT IS DROPPED. Two connections from one address have two ephemeral ports by definition, so
// comparing `addr:port` would report every pair as different and this whole file would pass while
// measuring nothing.
func peersOf(t *testing.T, d *podmanDriver, container string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var seen []string
	for time.Now().Before(deadline) {
		out, _ := exec.Command(d.bin, "logs", container).CombinedOutput()
		seen = nil
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "PEER ") {
				continue
			}
			addr := strings.TrimSpace(strings.TrimPrefix(line, "PEER "))
			if ap, err := netip.ParseAddrPort(addr); err == nil {
				seen = append(seen, ap.Addr().String())
			}
		}
		if len(seen) >= want {
			return seen[len(seen)-want:]
		}
		time.Sleep(250 * time.Millisecond)
	}
	// FATAL AND NOT A SKIP, WHICH IS THE OPPOSITE OF EVERY OTHER EXIT IN THIS FILE. The others are
	// environmental — no podman, no second network, a dial that could not be carried — and skipping
	// is the honest answer to "this box cannot host the experiment". This one is different in kind:
	// `runPackedWorker` has ALREADY asserted that both Workers REACHED the destination, so a
	// destination that reports no source addresses is a broken INSTRUMENT, and skipping on a broken
	// instrument is exactly the silent pass this package refuses. Found by mutation: deleting the
	// probe's `PEER` line left this test green.
	t.Fatalf("the destination reported %d source address(es) after two Workers reached it and this "+
		"needs %d — the instrument is broken, not the box", len(seen), want)
	return nil
}
