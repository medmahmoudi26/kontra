// warden_egress_test.go — the egress policy, tested by trying to leave.
//
// ═══ A SECURITY CONTROL NEEDS A TEST THAT SHOWS A REFUSAL ═══
//
// A test proving an ALLOWED destination is reachable proves the network works. The one that matters
// shows a DENIED destination refused, and it is worth nothing without the control that shows the same
// destination reachable when the rule is not there — otherwise it passes identically against a broken
// container, a dead listener, a probe that cannot dial, and a policy that works. Three of those four
// are bugs in the test, and this repo has shipped all three shapes before.
//
// So every live test here carries its controls in the same run, against the same probe binary and the
// same destination:
//
//	NO RULE INSTALLED  → the destination answers          (it is real, and the container can reach it)
//	RULE INSTALLED     → the destination is REFUSED       (the claim)
//	NAMED IN `allow`   → the destination answers again    (the refusal was the policy, not the network)
//
// Measured on this checkout's Controller while writing the file, with no policy installed at all: a
// container started with `--cap-drop ALL` REACHED 169.254.169.254 (the cloud metadata service, which
// hands out this Machine's own instance credentials), the Machine's unauthenticated Redis on its VPC
// address, and the public internet. That is the exposure, and it is what the first two lines above
// are the difference between.
//
// ═══ THE TESTS THAT NEED A KERNEL SAY SO ═══
//
// Installing an nftables ruleset needs CAP_NET_ADMIN in the host's network namespace, so half of this
// file cannot run as an ordinary user. It SKIPS LOUDLY rather than quietly, naming what would have
// been proved, because a skip that reads as a pass is the failure mode this package cares most about.
// The renderer and the loop's refusal-to-start are tested with no kernel at all and always run.
package warden

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// egressTestTable is the table the live tests install.
//
// NOT `kontra_warden`. A Machine running this suite may also be running a real Warden, and a test that
// replaced that Warden's table would be a test that turns off a live Machine's egress policy for as
// long as it runs — and then deletes it on cleanup. Everything else about the ruleset is identical, so
// nothing about the claim is weakened by the name.
const egressTestTable = "kontra_warden_test"

var egressProbeSeq atomic.Int64

// --- what the live half needs -------------------------------------------------------------------------

// requireEgressHost skips unless this box can actually be made to refuse a packet.
func requireEgressHost(t *testing.T) *podmanDriver {
	t.Helper()
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft is not installed — a Machine's egress policy is UNVERIFIED here, including whether " +
			"a denied destination is refused at all")
	}
	if os.Geteuid() != 0 {
		t.Skip("not root — installing a ruleset in the host's network namespace needs CAP_NET_ADMIN " +
			"there, so nothing about a denied destination being refused is verified here")
	}
	d := requirePodman(t)
	if out, err := exec.Command("nft", "list", "tables").CombinedOutput(); err != nil {
		t.Skipf("nft cannot read this kernel's ruleset (%v: %s), so it could not install one either",
			err, strings.TrimSpace(string(out)))
	}
	return d
}

// egressUnderTest builds a machineEgress that writes the test table and removes it afterwards.
//
// THE CLEANUP IS REGISTERED BEFORE ANYTHING IS INSTALLED, so a test that fails half way through still
// leaves this box as it found it. `nft delete table` on an absent table is an error and is ignored;
// what must not happen is the table surviving the run.
func egressUnderTest(t *testing.T, derived []string) *machineEgress {
	t.Helper()
	t.Cleanup(func() {
		_ = exec.Command("nft", "delete", "table", "inet", egressTestTable).Run()
	})
	return &machineEgress{
		nft:     "nft",
		podman:  "podman",
		table:   egressTestTable,
		derived: func() []string { return derived },
		out:     &testLog{t: t},
	}
}

// privateDest is an RFC1918 destination a container CAN reach with no policy installed — the control
// every live test below needs, FOUND rather than assumed, and returned only once it has answered.
//
// ═══ WHY THE DESTINATION IS ANOTHER CONTAINER AND NOT THIS MACHINE ═══
//
// The obvious destination is a listener on one of the Machine's own private addresses, and it was the
// first version of this helper. It does not work here and the reason is worth writing down: ufw is
// active on this box and denies container-to-host on every port it has not been told about, so a
// listener on an ephemeral port is refused BEFORE any kontra rule is consulted. Measured — all eleven
// of this Controller's private addresses timed out from a container with no policy installed at all,
// while the Machine's Redis on 10.124.0.2:6379 answered, because THAT one is a Docker-published port
// whose DNAT and FORWARD rules run ahead of ufw's INPUT chain. A control that had used one of the
// eleven would have been a test passing on a listener nothing could ever have reached.
//
// So the destination is a second container: RFC1918, reachable with no policy, owned by this test, and
// on the `forward` hook — which is the hook that matters, since scanning a third party and reaching
// another Machine in the VPC are both forwarded. What that leaves UNTESTED here is the `input` chain,
// the Machine itself, because this box's ufw refuses the control it would need. The chain body is the
// same `jump egress` and the renderer test asserts both hooks are emitted; that is weaker than a
// measurement and is stated rather than glossed.
//
// IT DEPENDS ON `net.bridge.bridge-nf-call-iptables=1`, which is this kernel's default and is what
// makes traffic between two containers on ONE bridge traverse the ip forward hook at all. With it off,
// containers on a Machine reach each other whatever the policy says — same tenant, so not a
// cross-tenant hole, but not nothing either. The skip below is what fires if it is off.
func privateDest(t *testing.T, d *podmanDriver, image string) string {
	t.Helper()
	name := fmt.Sprintf("kontra-egress-dest-%d-%d", os.Getpid(), egressProbeSeq.Add(1))
	t.Cleanup(func() { removeContainer(d, name) })
	const port = "19112"
	args := append([]string{"run", "--detach", "--name", name}, d.userns.args...)
	args = append(args, "--entrypoint", "/probe", image, "listen", "0.0.0.0:"+port)
	if out, err := exec.Command(d.bin, args...).CombinedOutput(); err != nil {
		t.Skipf("could not start a destination container (%v: %s), so nothing here can be measured",
			err, strings.TrimSpace(string(out)))
	}
	var dest string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command(d.bin, "inspect", name, "--format", "{{.NetworkSettings.IPAddress}}").Output()
		ip := strings.TrimSpace(string(out))
		if err == nil && ip != "" {
			if a, perr := netip.ParseAddr(ip); perr == nil && a.IsPrivate() {
				dest = net.JoinHostPort(ip, port)
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if dest == "" {
		t.Skip("the destination container never got a private address, so there is nothing to prove a " +
			"container cannot reach")
	}
	// CONTROL — it answers with no policy installed. Retried because a container that has an address
	// has not necessarily bound its socket yet, and a control that raced would skip a working box.
	for time.Now().Before(deadline) {
		if reached(probeTCP(t, d, image, dest), dest) {
			return dest
		}
		time.Sleep(500 * time.Millisecond)
	}
	logs, _ := exec.Command(d.bin, "logs", name).CombinedOutput()
	t.Skipf("with NO egress policy installed a container could not reach %s, so what is UNVERIFIED "+
		"here is that the policy is what refuses it. The destination said: %s", dest, strings.TrimSpace(string(logs)))
	return ""
}

// probeTCP runs the fixture in the driver's OWN posture and returns what it said.
//
// THE POSTURE IS TAKEN FROM THE DRIVER, not written out here: `d.userns.args` is whatever
// `newPodmanDriver` decided by reading the runtime, and `--cap-drop ALL` plus `no-new-privileges` are
// what `runArgs` puts on every half. A test that hard-coded a weaker or stronger posture would be
// testing a container the Warden never starts.
func probeTCP(t *testing.T, d *podmanDriver, image string, dests ...string) string {
	t.Helper()
	return probeRun(t, d, image, append([]string{"tcp"}, dests...)...)
}

func probeRun(t *testing.T, d *podmanDriver, image string, args ...string) string {
	t.Helper()
	return probeRunAs(t, d, image, egressHardening(d), args...)
}

// egressHardening is the posture `runArgs` puts on every half of every Worker, taken from the driver
// rather than written out again. A test that hard-coded a weaker or stronger one would be testing a
// container the Warden never starts.
func egressHardening(d *podmanDriver) []string {
	return append([]string{"--cap-drop", "ALL", "--security-opt", "no-new-privileges"}, d.userns.args...)
}

// probeRunAs runs the fixture with EXACTLY the flags given and none of the driver's own, because the
// postures that prove the boundary are deliberately weaker than any Worker the driver would start.
func probeRunAs(t *testing.T, d *podmanDriver, image string, extraFlags []string, args ...string) string {
	t.Helper()
	name := fmt.Sprintf("kontra-egress-%d-%d", os.Getpid(), egressProbeSeq.Add(1))
	flags := []string{"run", "--rm", "--name", name}
	flags = append(flags, extraFlags...)
	flags = append(flags, "--entrypoint", "/probe", image)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, d.bin, append(flags, args...)...).CombinedOutput()
	if err != nil {
		// A container that could not START is not a container that was refused a destination, and
		// reporting the two the same way is how a broken fixture reads as a working control.
		t.Fatalf("podman run %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
	removeContainer(d, name)
	return string(out)
}

// removeContainer RETRIES, for the same reason `cleanupPod` does and the podman driver's own `stop`
// does. Measured here: one `podman rm -f` leaves the container STOPPED but still an object podman
// holds — ten of them accumulated over one afternoon of running this file — because this box's kernel
// refuses to let podman signal a container's init. A test suite that only passes on a machine no
// previous run has touched is not passing, and a leftover name is how that starts.
func removeContainer(d *podmanDriver, name string) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, _ = exec.Command(d.bin, "rm", "-f", name).CombinedOutput()
		out, err := exec.Command(d.bin, "ps", "-a", "--filter", "name="+name, "--format", "{{.Names}}").Output()
		if err != nil || !strings.Contains(string(out), name) || !time.Now().Before(deadline) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func reached(out, dest string) bool { return strings.Contains(out, "REACHED "+dest) }
func refused(out, dest string) bool { return strings.Contains(out, "REFUSED "+dest) }

// --- the claim ------------------------------------------------------------------------------------

// AN ACTOR CANNOT REACH A DESTINATION ITS ASSIGNMENT DOES NOT ALLOW, PROVEN FROM INSIDE.
//
// The destination is an RFC1918 address the container demonstrably reached a moment earlier — see
// `privateDest`, which will not return one until it has answered. That is the same class of address as
// every other Machine in the VPC, the Controller, and the unauthenticated Redis a container on this
// checkout's box reaches today.
//
// DELETE `private` FROM `renderEgress`'S PRECEDENCE LOOP AND THE CLAIM GOES RED. Delete the `apply`
// call and `privateDest` skips instead, which is the point of having both: the run never reports a
// pass on a destination nothing could have reached.
func TestAnActorCannotReachADestinationItsAssignmentDoesNotAllow(t *testing.T) {
	d := requireEgressHost(t)
	image := probeImage(t)
	// CONTROL — `privateDest` only returns a destination a container REACHED with no policy
	// installed, so the refusal below cannot be satisfied by a listener that never came up or an
	// address this box does not route to its containers.
	dest := privateDest(t, d, image)
	host, _, _ := net.SplitHostPort(dest)
	e := egressUnderTest(t, nil)

	// ═══ THE CLAIM ═══
	if err := e.apply(context.Background(), egressPolicy{}); err != nil {
		t.Fatalf("could not install the policy: %v", err)
	}
	if out := probeTCP(t, d, image, dest); !refused(out, dest) {
		t.Fatalf("a container reached %s, which is an RFC1918 address no assignment allowed — the "+
			"default-deny is not enforcing:\n%s", dest, out)
	}

	// CONTROL — named in the assignment's allow list, the SAME destination answers again. This is what
	// separates "the policy refused it" from "the container's network is broken".
	if err := e.apply(context.Background(), egressPolicy{Allow: []string{host}}); err != nil {
		t.Fatalf("could not install the policy with %s allowed: %v", host, err)
	}
	if out := probeTCP(t, d, image, dest); !reached(out, dest) {
		t.Fatalf("%s was in the assignment's allow list and the container still could not reach it, so "+
			"the deny above may have been a broken network rather than the policy:\n%s", dest, out)
	}
}

// THE CLOUD METADATA SERVICE IS NOT THE ASSIGNMENT'S TO ALLOW.
//
// 169.254.169.254 hands out this Machine's own instance credentials to anything that asks, over plain
// HTTP with no authentication. An `Allow` entry naming it must not work, or a tenant who can write an
// assignment can ask for the Fleet's cloud token. This is the only rule in the file whose whole
// meaning is its ORDER, so it is the only one with a test that can only fail on order.
//
// MOVE `floor` AFTER `allow` IN `renderEgress`'s PRECEDENCE LOOP AND THIS GOES RED.
func TestNoAssignmentCanAllowTheMetadataService(t *testing.T) {
	d := requireEgressHost(t)
	image := probeImage(t)
	const metadata = "169.254.169.254:80"
	e := egressUnderTest(t, nil)

	// CONTROL — it answers with no policy. On a box where it does not, this test can prove nothing and
	// says so rather than passing.
	if out := probeTCP(t, d, image, metadata); !reached(out, metadata) {
		t.Skipf("this Machine has no metadata service answering on %s, so the floor cannot be tested "+
			"here — what is UNVERIFIED is that an assignment cannot allow one:\n%s", metadata, out)
	}

	// ═══ THE CLAIM ═══ the assignment asks for it explicitly, twice over, and is refused.
	if err := e.apply(context.Background(), egressPolicy{
		Allow: []string{"169.254.169.254", "169.254.0.0/16"},
	}); err != nil {
		t.Fatalf("could not install the policy: %v", err)
	}
	if out := probeTCP(t, d, image, metadata); !refused(out, metadata) {
		t.Fatalf("an assignment allowed %s and it worked — a tenant who can write an assignment can "+
			"read this Machine's cloud credentials:\n%s", metadata, out)
	}
}

// A PUBLIC DESTINATION THE ASSIGNMENT DENIES IS REFUSED, AND THIS IS THE `forward` HOOK.
//
// The test above exercises `input` — traffic aimed at the Machine itself. This one leaves the Machine
// entirely, which is the abuse ADR 0036 names: "renting the platform to scan a third party from
// kontra's addresses". Both hooks jump to the same chain and neither is assumed from the other.
func TestAPublicDestinationTheAssignmentDeniesIsRefused(t *testing.T) {
	d := requireEgressHost(t)
	image := probeImage(t)
	const denied, allowed = "1.1.1.1:443", "9.9.9.9:443"
	e := egressUnderTest(t, nil)

	// CONTROL — both answer with no policy. If this box has no route off itself, nothing below is a
	// measurement and the skip says so.
	out := probeTCP(t, d, image, denied, allowed)
	if !reached(out, denied) || !reached(out, allowed) {
		t.Skipf("this Machine cannot reach the public internet, so what is UNVERIFIED here is that a "+
			"denied third party is refused on the forward hook:\n%s", out)
	}

	// ═══ THE CLAIM ═══ one is denied by name; the other is not, and must keep working — a policy that
	// broke all public egress would satisfy the first assertion and be useless.
	if err := e.apply(context.Background(), egressPolicy{Deny: []string{"1.1.1.1"}}); err != nil {
		t.Fatalf("could not install the policy: %v", err)
	}
	out = probeTCP(t, d, image, denied, allowed)
	if !refused(out, denied) {
		t.Fatalf("the assignment denied 1.1.1.1 and a container reached it anyway:\n%s", out)
	}
	if !reached(out, allowed) {
		t.Fatalf("the assignment denied 1.1.1.1 and %s stopped working too, so the policy is not a "+
			"deny list, it is an outage:\n%s", allowed, out)
	}
}

// THE POLICY SURVIVES A CONTAINER RESTART AND A WARDEN RESTART.
//
// Both fall out of WHERE the rule is rather than from anything that re-installs it, and that is the
// property worth pinning: the rule is attached to the Machine's bridge in the host's network namespace,
// so it never knew which container was behind it, and it is in the KERNEL rather than in the Warden's
// memory, so a Warden that exits leaves it standing. Nothing in warden_egress.go removes a rule.
func TestTheEgressPolicySurvivesAContainerAndAWardenRestart(t *testing.T) {
	d := requireEgressHost(t)
	image := probeImage(t)
	dest := privateDest(t, d, image)

	// A Warden installs the policy...
	first := egressUnderTest(t, nil)
	if err := first.apply(context.Background(), egressPolicy{}); err != nil {
		t.Fatalf("could not install the policy: %v", err)
	}
	if out := probeTCP(t, d, image, dest); !refused(out, dest) {
		t.Fatalf("the policy did not refuse %s at all, so nothing below is about survival:\n%s", dest, out)
	}

	// ...a container restart is a NEW container, on a new veth, with a new address. `probeTCP` has
	// started and removed one already; this is another.
	if out := probeTCP(t, d, image, dest); !refused(out, dest) {
		t.Fatalf("a second, freshly started container reached %s, so the rule was attached to the "+
			"container rather than to the Machine:\n%s", dest, out)
	}

	// ...and the Warden restarts. `first` is dropped on the floor without being asked to tear anything
	// down, which is exactly what a `systemctl restart` does to it.
	second := newMachineEgress(wardenRecord{}, &testLog{t: t})
	second.table = egressTestTable
	if second.enforced() {
		t.Fatal("a Warden that has just started claims to have installed a policy it has not")
	}
	if out := probeTCP(t, d, image, dest); !refused(out, dest) {
		t.Fatalf("the policy stopped enforcing when the Warden that installed it went away, so it "+
			"lives in a process rather than in the kernel:\n%s", out)
	}
	// And the new Warden converges onto the same table rather than colliding with it.
	if err := second.apply(context.Background(), egressPolicy{}); err != nil {
		t.Fatalf("the restarted Warden could not re-install the policy over the existing table: %v", err)
	}
	if out := probeTCP(t, d, image, dest); !refused(out, dest) {
		t.Fatalf("re-installing the policy over itself stopped it working:\n%s", out)
	}
}

// THE RULES ARE NOT REACHABLE FROM INSIDE THE CONTAINER, AND THE REASON IS THE NETWORK NAMESPACE.
//
// Four postures, one probe binary, asking the kernel over NETLINK_NETFILTER for every nftables table it
// can see. The rows are chosen so that the conclusion cannot be read out of any one of them:
//
//	the HOST                                        lists the table   ← the target is real, the probe works
//	the driver's posture (caps dropped, own userns)  EPERM             ← cannot even ask
//	--privileged with its own user namespace         lists NOTHING     ← privilege is not the boundary
//	--privileged --network=host                      lists the table   ← the namespace IS the boundary
//
// The last row is what `assertNoRuntimeAccess` refuses, and it is the positive control that makes the
// third row mean something: without it, an empty list is equally explained by a probe that cannot see
// ANY ruleset anywhere.
//
// READING IS THE RIGHT QUESTION EVEN THOUGH THE CLAIM IS ABOUT EDITING, and not by inference. Every
// nftables message — GETTABLE and DELTABLE alike — enters the kernel through `nfnetlink_rcv_msg`,
// which checks `netlink_net_capable(skb, CAP_NET_ADMIN)` against the socket's OWN network namespace
// before it looks at what was asked. A container that cannot list a table has failed the identical
// check a write would fail, and a container that lists an EMPTY set of tables is looking at a
// different namespace's nf_tables, so there is nothing there for a write to land on. The probe asks
// the cheap half of one question rather than two halves of two.
func TestTheEgressRulesAreNotReachableFromInsideAContainer(t *testing.T) {
	d := requireEgressHost(t)
	image := probeImage(t)
	e := egressUnderTest(t, nil)
	if err := e.apply(context.Background(), egressPolicy{}); err != nil {
		t.Fatalf("could not install the policy: %v", err)
	}

	// CONTROL — on the host, the table is there to be seen. Asked through `nft` rather than the probe
	// so that a broken probe cannot satisfy it.
	if out, err := exec.Command("nft", "list", "table", "inet", egressTestTable).CombinedOutput(); err != nil {
		t.Fatalf("the policy is not in the host's ruleset at all (%v: %s), so nothing below is a "+
			"statement about reaching it", err, strings.TrimSpace(string(out)))
	}

	// ═══ THE CLAIM ═══ the posture the driver actually starts a Worker in cannot even ask.
	out := probeRun(t, d, image, "nft-dump")
	if !strings.Contains(out, "NFT-ERROR") {
		t.Errorf("a Worker in the driver's own posture was allowed to enumerate nftables: %s", out)
	}
	if strings.Contains(out, egressTestTable) {
		t.Fatalf("a Worker in the driver's own posture can SEE this Machine's egress policy: %s", out)
	}

	// ...and neither can one handed every capability the runtime has. If this row were missing, the
	// claim would rest on `--cap-drop ALL` staying in an argv.
	out = probeRunAs(t, d, image, append([]string{"--privileged"}, d.userns.args...), "nft-dump")
	if strings.Contains(out, egressTestTable) {
		t.Fatalf("a --privileged Worker can see this Machine's egress policy, so the boundary is the "+
			"capability set and not the network namespace: %s", out)
	}

	// CONTROL — the same privileged container ON THE HOST NETWORK sees it, which is the flag
	// `assertNoRuntimeAccess` refuses and the reason that refusal is load-bearing here. Without this
	// row, "the list was empty" is also what a probe that can never see anything would print.
	//
	// NO `--userns=auto` ON THIS ROW, and the reason is itself a measurement: podman cannot start a
	// `--privileged` container that is in a private user namespace AND the host's network namespace —
	// "error mounting sysfs to rootfs at /sys: operation not permitted", because mounting sysfs needs
	// the caller's user namespace to own the network namespace it is describing. So this row is the
	// most privileged container this runtime can actually produce, which is what a control has to be.
	out = probeRunAs(t, d, image, []string{"--privileged", "--network=host"}, "nft-dump")
	if !strings.Contains(out, egressTestTable) {
		t.Fatalf("even on the HOST network a privileged container could not see the policy, so the two "+
			"assertions above are satisfied by a probe that sees nothing anywhere: %s", out)
	}
	// And the driver refuses to build that argv — the one row above that must never be a Worker.
	if err := assertNoRuntimeAccess([]string{"run", "--network=host"}); err == nil {
		t.Error("the driver would build a Worker on the host network, which is the one posture measured " +
			"above as able to edit this Machine's egress policy")
	}
}

// --- the renderer, with no kernel ---------------------------------------------------------------------

// testNets is one governed network, so a render can be read without podman.
var testNets = []containerNet{{Name: "podman", Iface: "cni-podman0",
	Subnets: []netip.Prefix{netip.MustParsePrefix("10.88.0.0/16")}}}

func renderForTest(t *testing.T, p egressPolicy, derived []string) string {
	t.Helper()
	floor, err := resolveEgress("floor", egressFloor)
	if err != nil {
		t.Fatal(err)
	}
	private, err := resolveEgress("private", egressPrivate)
	if err != nil {
		t.Fatal(err)
	}
	allow, err := resolveEgress("allow", append(append([]string{}, p.Allow...), derived...))
	if err != nil {
		t.Fatal(err)
	}
	deny, err := resolveEgress("deny", p.Deny)
	if err != nil {
		t.Fatal(err)
	}
	return renderEgress(egressTable, testNets, floor, private, allow, deny)
}

// THE PRECEDENCE IS THE POLICY, so it is asserted as an ORDER and not as a set of substrings.
//
// A test that only checked "the floor rule is present" would pass with the floor written after the
// allow list, which is the one arrangement that makes the floor meaningless. The live test above proves
// the same thing against a kernel; this one says which line to move when it goes red.
func TestTheEgressPrecedenceIsFloorThenDenyThenAllowThenPrivate(t *testing.T) {
	got := renderForTest(t, egressPolicy{Allow: []string{"10.124.0.2"}, Deny: []string{"1.1.1.1"}}, nil)
	order := []string{
		"ip daddr @floor4 counter drop",
		"ip daddr @deny4 counter drop",
		"ip daddr @allow4 counter accept",
		"ip daddr @private4 counter drop",
	}
	at := -1
	for _, rule := range order {
		i := strings.Index(got, rule)
		if i < 0 {
			t.Fatalf("the ruleset has no %q in it at all:\n%s", rule, got)
		}
		if i < at {
			t.Fatalf("%q comes before a rule that must precede it — the precedence is floor, deny, "+
				"allow, private, and every swap of it opens something:\n%s", rule, got)
		}
		at = i
	}
}

// EVERY ADDRESS RULE IS ASKED TWICE, AND THE SECOND FORM IS THE ONE THAT WAS MISSING.
//
// Destination NAT rewrites `ip daddr` in `prerouting`, before any filter hook. Measured on this
// Controller: a container dialling the Controller's published Redis at 10.124.0.2:6379 arrived at the
// forward hook as 172.20.0.6:6379, so an allow list holding 10.124.0.2 never matched and the Machine
// could not reach its own control plane. `ct original ip daddr` is the address the actor dialled.
//
// DELETE THE `ct original` LINE FROM `renderEgress` AND THIS GOES RED — and so does any Fleet whose
// Controller publishes its ports.
// ═══ WRITTEN TWICE, BECAUSE THE FIRST VERSION COULD NOT FAIL ═══
//
// It asked `strings.Contains(got, "ip6 daddr @deny6")` — and that string is a SUBSTRING of
// "ct original ip6 daddr @deny6". Deleting the plain form left the test green, which a mutation caught
// and nothing else would have: the assertion for each rule was being satisfied by the OTHER rule. It
// is the same shape slice 09 found matching a word in a command's footer.
//
// So every form is matched as a WHOLE LINE, anchored on the newline and the indent the renderer emits.
// A prefix of another rule cannot satisfy a line.
func TestEveryEgressRuleIsAskedBeforeAndAfterDestinationNAT(t *testing.T) {
	got := renderForTest(t, egressPolicy{Allow: []string{"10.124.0.2"}, Deny: []string{"1.1.1.1"}}, nil)
	verbs := map[string]string{"floor": "drop", "deny": "drop", "allow": "accept", "private": "drop"}
	for _, set := range []string{"floor", "deny", "allow", "private"} {
		for _, form := range []string{"ip daddr @%s4", "ct original ip daddr @%s4",
			"ip6 daddr @%s6", "ct original ip6 daddr @%s6"} {
			line := "\n\t\t" + fmt.Sprintf(form, set) + " counter " + verbs[set] + "\n"
			if !strings.Contains(got, line) {
				t.Errorf("the ruleset has no rule %q, so a destination reached through a published port "+
					"is judged on the wrong address:\n%s", strings.TrimSpace(line), got)
			}
		}
	}
	// AND THE VACUITY GUARD ITSELF: the plain and the `ct original` forms must be DIFFERENT lines, four
	// of each. Counting is what makes "the substring was there" and "the rule was there" different
	// questions.
	if n := strings.Count(got, "\n\t\tip daddr @"); n != 4 {
		t.Errorf("the ruleset has %d post-DNAT IPv4 rules, want one per precedence step:\n%s", n, got)
	}
	if n := strings.Count(got, "\n\t\tct original ip daddr @"); n != 4 {
		t.Errorf("the ruleset has %d pre-DNAT IPv4 rules, want one per precedence step:\n%s", n, got)
	}
	if n := strings.Count(got, "\n\t\tip6 daddr @"); n != 4 {
		t.Errorf("the ruleset has %d post-DNAT IPv6 rules, want one per precedence step:\n%s", n, got)
	}
	if n := strings.Count(got, "\n\t\tct original ip6 daddr @"); n != 4 {
		t.Errorf("the ruleset has %d pre-DNAT IPv6 rules, want one per precedence step:\n%s", n, got)
	}
}

// BOTH HOOKS ARE EMITTED, AND BOTH JUMP TO THE SAME CHAIN.
//
// `forward` is traffic the Machine routes on a container's behalf — the third party it was rented to
// scan, and every other Machine in the VPC. `input` is the Machine ITSELF, where the Warden's state,
// the Controller's services and every other container's published port live. Only `forward` is proved
// against a kernel in this file (see `privateDest` for why this box's ufw refuses the control `input`
// would need), so this is the assertion that stops the second chain being dropped unnoticed — weaker
// than a measurement, and said so rather than left implied.
func TestBothEgressHooksAreInstalled(t *testing.T) {
	got := renderForTest(t, egressPolicy{}, nil)
	for _, hook := range []string{"forward", "input"} {
		want := fmt.Sprintf("chain %s {\n\t\ttype filter hook %s priority %d; policy accept;\n\t\tjump egress\n",
			hook, hook, egressHookPriority)
		if !strings.Contains(got, want) {
			t.Errorf("the ruleset has no %s chain jumping to the policy, so that whole direction is "+
				"ungoverned:\n%s", hook, got)
		}
	}
	// AND THE PRIORITY IS BEFORE THE ORDINARY FILTER TABLES. iptables-nft puts its `filter` chains at
	// 0 and this box's ufw ruleset lives there; two base chains at one priority are evaluated in
	// creation order, which is not a thing to depend on.
	if egressHookPriority >= 0 {
		t.Errorf("the policy is hooked at priority %d, at or after the ordinary filter tables",
			egressHookPriority)
	}
}

// THE FAMILY IS REFUSED PER BRIDGE, ON EXACTLY THE ONES THE RUNTIME GAVE NO IPv6.
//
// The bypass is not exotic: an operator denies a host by its IPv4 address, because that is the address
// they were given, and the same service answers on IPv6. Without a v6 rule the public fall-through at
// the bottom of the chain lets the v6 address out with the deny list intact and irrelevant.
//
// Both states coexist on one Machine, which is why the rule names bridges rather than being a
// Machine-wide switch — a dual-stack network must be JUDGED by the v6 rules, and a v4-only one must
// have the family refused. TestTheEgressPolicyGovernsIPv6OnADualStackNetwork measures both against a
// real kernel; this pins which bridge each rule names, which a live test cannot see.
func TestIPv6IsRefusedOnExactlyTheBridgesTheRuntimeGaveNone(t *testing.T) {
	got := renderForTest(t, egressPolicy{}, nil)
	if !strings.Contains(got, `iifname { "cni-podman0" } meta nfproto ipv6 counter drop`) {
		t.Errorf("the v4-only bridge does not have IPv6 refused, so a v6 address reaches past a deny "+
			"list written in IPv4:\n%s", got)
	}

	mixed := []containerNet{
		{Name: "podman", Iface: "cni-podman0", Subnets: []netip.Prefix{netip.MustParsePrefix("10.88.0.0/16")}},
		{Name: "dual", Iface: "cni-dual", Subnets: []netip.Prefix{
			netip.MustParsePrefix("10.99.0.0/24"), netip.MustParsePrefix("fd00:dead::/64")}},
	}
	floor, _ := resolveEgress("floor", egressFloor)
	private, _ := resolveEgress("private", egressPrivate)
	allow, _ := resolveEgress("allow", nil)
	deny, _ := resolveEgress("deny", nil)
	got = renderEgress(egressTable, mixed, floor, private, allow, deny)
	// The v4-only bridge is still named...
	if !strings.Contains(got, `iifname { "cni-podman0" } meta nfproto ipv6 counter drop`) {
		t.Errorf("a Machine with one dual-stack network stopped refusing IPv6 on its v4-only bridge, so "+
			"one network's configuration decided another network's policy:\n%s", got)
	}
	// ...and the dual-stack one is NOT, which is what stops this being an IPv6 ban.
	if strings.Contains(got, `"cni-dual"`) && strings.Contains(got, `iifname { "cni-dual"`) {
		t.Errorf("the dual-stack bridge had its IPv6 dropped wholesale, so a network deliberately given "+
			"IPv6 cannot use it:\n%s", got)
	}
	if !strings.Contains(got, "ip6 daddr @private6 counter drop") {
		t.Errorf("nothing judges IPv6 by the policy at all:\n%s", got)
	}
}

// IPv6 IS GOVERNED, MEASURED AGAINST A REAL DUAL-STACK NETWORK.
//
// This was a stated gap an hour ago — "the v6 rules mirror the v4 ones and are asserted only against
// the rendered ruleset, because nothing in this repo has a dual-stack container network to run them
// against". It turned out to be one `podman network create --ipv6` away, and the measurement changed
// what shipped: the family refusal became per-bridge rather than per-Machine.
//
// Measured while writing it, in this order. A container on the DEFAULT network cannot send IPv6 at all
// ("connect: network is unreachable"), so the exposure is absent there. A container on a network
// created with `--ipv6` REACHED 2606:4700:4700::1111 and 2620:fe::fe on the public internet, with no
// policy installed — so the exposure is real exactly where podman hands out a v6 address.
//
// DELETE THE `ip6 daddr @deny6` RULE FROM `renderEgress` AND THE CLAIM GOES RED.
func TestTheEgressPolicyGovernsIPv6OnADualStackNetwork(t *testing.T) {
	d := requireEgressHost(t)
	image := probeImage(t)
	const denied, allowed = "[2606:4700:4700::1111]:443", "[2620:fe::fe]:443"

	net6 := fmt.Sprintf("kontra-s12-v6-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command(d.bin, "network", "rm", "-f", net6).Run() })
	if out, err := exec.Command(d.bin, "network", "create", "--ipv6",
		"--subnet", "10.99.0.0/24", "--subnet", "fd00:c0de:12::/64", net6).CombinedOutput(); err != nil {
		t.Skipf("cannot create a dual-stack podman network (%v: %s), so what is UNVERIFIED here is that "+
			"the IPv6 rules govern anything", err, strings.TrimSpace(string(out)))
	}

	// CONTROL — with no policy, both public v6 destinations answer from inside that network. Without
	// this, every assertion below is satisfied by a Machine with no IPv6 at all.
	on6 := []string{"--network", net6}
	out := probeRunAs(t, d, image, append(on6, egressHardening(d)...), "tcp", denied, allowed)
	if !reached(out, denied) || !reached(out, allowed) {
		t.Skipf("a container on a dual-stack network cannot reach the public IPv6 internet from this "+
			"Machine, so what is UNVERIFIED here is that the IPv6 rules refuse anything:\n%s", out)
	}

	// ═══ THE CLAIM ═══ one v6 destination is denied by name and refused; the other is untouched, so
	// this is a deny list rather than an outage.
	e := egressUnderTest(t, nil)
	if err := e.apply(context.Background(), egressPolicy{Deny: []string{"2606:4700:4700::1111"}}); err != nil {
		t.Fatalf("could not install the policy: %v", err)
	}
	out = probeRunAs(t, d, image, append(on6, egressHardening(d)...), "tcp", denied, allowed)
	if !refused(out, denied) {
		t.Fatalf("the assignment denied an IPv6 destination and a container reached it anyway:\n%s", out)
	}
	if !reached(out, allowed) {
		t.Fatalf("denying one IPv6 address stopped IPv6 working entirely, which is an outage and not a "+
			"policy:\n%s", out)
	}

	// ═══ WHAT IS DELIBERATELY NOT ASSERTED HERE ═══
	//
	// The other half of the per-bridge rule — that a container on the V4-ONLY bridge has the family
	// refused — was written here and then removed, because it could not fail. A container on the
	// default network has no IPv6 ROUTE ("connect: network is unreachable"), so it is refused with the
	// rule, without the rule, and with the whole table deleted. Making the family refusal Machine-wide
	// left that assertion green; only `TestIPv6IsRefusedOnExactlyTheBridgesTheRuntimeGaveNone`, which
	// reads which bridge each rule names, went red.
	//
	// Producing a container that HAS v6 while podman declares none needs a router advertisement on the
	// bridge, which this Machine has nothing to send and which is not something to arrange inside a
	// test suite. So that half is asserted against the rendered ruleset and is UNMEASURED, and saying
	// so is worth more than a line that passes either way.
}

// AN EMPTY SET IS STILL WRITTEN. Every rule names one, so a ruleset that omitted the sets an assignment
// happened not to fill would be a ruleset `nft` refuses to load — which turns "this assignment has no
// deny list" into "this Machine has no policy at all".
func TestAnEmptyEgressListStillRendersItsSet(t *testing.T) {
	got := renderForTest(t, egressPolicy{}, nil)
	for _, set := range []string{"set allow4 {", "set allow6 {", "set deny4 {", "set deny6 {"} {
		if !strings.Contains(got, set) {
			t.Errorf("an assignment with no lists rendered no %q, so nft would refuse the whole "+
				"ruleset:\n%s", set, got)
		}
	}
}

// AN ALLOW LIST NAMING A RANGE AND A HOST INSIDE IT MUST STILL LOAD.
//
// An nftables interval set refuses overlapping elements — "conflicting intervals specified" — and the
// refusal is not of the element, it is of the WHOLE ruleset. So an assignment allowing a VPC range and
// then one host inside it, which is what an operator writes when they add a second Controller, would
// leave `apply` failing, `enforced()` false, and `reconcile` starting NO Worker on the Machine at all.
// Found by the metadata test above, which allows both forms on purpose.
//
// REMOVE `auto-merge` FROM `renderEgress` AND THIS GOES RED — and so does every live test that names
// an address twice.
func TestAnAllowListMayNameARangeAndAHostInsideIt(t *testing.T) {
	got := renderForTest(t, egressPolicy{Allow: []string{"10.0.0.0/8", "10.1.2.3"}}, nil)
	if strings.Count(got, "auto-merge") != 8 {
		t.Fatalf("not every interval set takes the union of its elements, so an overlapping list "+
			"refuses the whole ruleset and the Machine starts nothing:\n%s", got)
	}
	if !strings.Contains(got, "10.0.0.0/8") || !strings.Contains(got, "10.1.2.3/32") {
		t.Errorf("both forms should reach the set and let the kernel merge them:\n%s", got)
	}
}

// A MALFORMED ENTRY REFUSES THE WHOLE POLICY RATHER THAN BEING SKIPPED. Both halves of a silent skip
// are invisible: a dropped allow is a Machine that cannot reach its Controller for no stated reason,
// and a dropped deny is a control that is not enforcing the one thing somebody wrote it for.
func TestAMalformedEgressEntryRefusesTheWholePolicy(t *testing.T) {
	if _, err := resolveEgress("allow", []string{"10.0.0.1", "not an address at all"}); err == nil {
		t.Fatal("a policy naming `not an address at all` was accepted, so the rest of it is now enforcing " +
			"something nobody wrote")
	}
	// CONTROL — the good entries on their own are fine, so the refusal above is the bad one.
	got, err := resolveEgress("allow", []string{"10.0.0.1", "192.168.0.0/16", "2001:db8::1"})
	if err != nil {
		t.Fatalf("a policy of perfectly good entries was refused: %v", err)
	}
	if len(got.v4) != 2 || len(got.v6) != 1 {
		t.Errorf("entries did not split by family: v4=%v v6=%v", got.v4, got.v6)
	}
}

// WHAT THE MACHINE NEEDS IS DERIVED, NOT WRITTEN IN THE ASSIGNMENT — the same rule
// `assignedWorker.derivedEnv` follows one layer up. The Controller and Temporal are on a private
// address in every deployment this Fleet has, so an operator who forgot a line would have Machines
// whose Workers cannot poll and no message saying why.
func TestTheDerivedAllowCarriesTheControllerAndTemporal(t *testing.T) {
	got := egressDerived(wardenRecord{
		Controller: "https://10.124.0.2:8443",
		Temporal:   "10.124.0.2:7233",
	})
	if len(got) < 2 || got[0] != "10.124.0.2" || got[1] != "10.124.0.2" {
		t.Fatalf("the Controller and Temporal are not in the derived allow list: %v", got)
	}
	// A Machine enrolled against a Controller with no control plane records an empty Temporal, and that
	// is the airgapped case rather than a failure.
	got = egressDerived(wardenRecord{Controller: "https://ctl.example:8443"})
	for _, e := range got {
		if e == "" {
			t.Fatalf("an empty endpoint became an empty allow entry, which resolves to nothing: %v", got)
		}
	}
}

// A LOOPBACK RESOLVER IS NOT ADDED. `/etc/resolv.conf` on a systemd-resolved box is the 127.0.0.53
// stub; a container reaching that reaches its OWN namespace's loopback and never leaves, so allowing it
// would be a rule that matches nothing while looking like DNS was handled. The real servers are in
// `/run/systemd/resolve/resolv.conf`, which is where podman takes them from — measured: a container on
// this box was handed 67.207.67.2, not 127.0.0.53.
func TestTheDerivedAllowTakesResolversButNotLoopbackOnes(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.conf")
	real := filepath.Join(dir, "real.conf")
	if err := os.WriteFile(stub, []byte("# comment\nnameserver 127.0.0.53\noptions edns0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("nameserver 67.207.67.2\nnameserver 67.207.67.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolvConfServers(stub, real, filepath.Join(dir, "absent.conf"))
	if len(got) != 2 || got[0] != "67.207.67.2" || got[1] != "67.207.67.3" {
		t.Fatalf("expected the two upstream servers and neither the stub nor an error: %v", got)
	}
}

// PODMAN'S NETWORKS ARE READ FROM PODMAN, and a network with no bridge is skipped rather than
// governed. `host` and `none` have no interface of their own; the first is what
// `assertNoRuntimeAccess` refuses a Worker onto, and neither has traffic arriving on an interface a
// rule could name.
func TestPodmanNetworksWithoutABridgeAreNotGoverned(t *testing.T) {
	nets, err := parsePodmanNets("podman\tcni-podman0\t10.88.0.0/16 \nhost\t<no value>\t\n" +
		"dual\tcni-dual\t10.89.0.0/16 fd00:dead::/64 \n")
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 2 || nets[0].Iface != "cni-podman0" || nets[1].Iface != "cni-dual" {
		t.Fatalf("expected the two bridged networks and not `host`: %#v", nets)
	}
	if len(nets[1].Subnets) != 2 {
		t.Errorf("a dual-stack network lost a subnet: %v", nets[1].Subnets)
	}
	// EVERY NETWORK IS GOVERNED, not only the one the driver uses today — `podmanDriver.start` passes
	// no `--network`, and a policy that hard-coded the default would stop covering Workers the day
	// somebody adds the flag, silently.
	if _, err := parsePodmanNets("host\t<no value>\t\n"); err == nil {
		t.Error("a runtime with no bridged network at all was accepted, so the policy would attach to " +
			"nothing and still report success")
	}
}

// --- the loop's refusal --------------------------------------------------------------------------------

// egressStub writes a `podman` and an `nft` that answer without a kernel, so the loop's gate can be
// tested by an ordinary user. `nftOK` decides whether the install succeeds.
func egressStub(t *testing.T, nftOK bool) *machineEgress {
	t.Helper()
	dir := t.TempDir()
	podman := filepath.Join(dir, "podman")
	body := "#!/bin/sh\ncase \"$2\" in\n  ls) echo podman ;;\n  inspect) printf 'podman\\tstub0\\t10.88.0.0/16 \\n' ;;\nesac\n"
	if err := os.WriteFile(podman, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	nft := filepath.Join(dir, "nft")
	script := "#!/bin/sh\ncat >/dev/null\nexit 0\n"
	if !nftOK {
		script = "#!/bin/sh\ncat >/dev/null\necho 'Operation not permitted' >&2\nexit 1\n"
	}
	if err := os.WriteFile(nft, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &machineEgress{nft: nft, podman: podman, table: egressTestTable,
		derived: func() []string { return nil }, out: &testLog{t: t}}
}

// NO WORKER STARTS UNTIL THE POLICY IS IN THE KERNEL.
//
// This is the one place the reconcile loop refuses to do its job, and it is the direction that fails
// safe: a Worker started before the policy is installed is a stranger's code with the whole VPC, every
// other Machine on it and the cloud metadata service reachable. The control is the same Warden with a
// working `nft`, which starts the same Worker — so the refusal is the gate and not a broken fixture.
//
// DELETE THE `enforced()` CHECK FROM `reconcile` AND THE FIRST HALF GOES RED.
func TestNoWorkerStartsUntilTheEgressPolicyIsInstalled(t *testing.T) {
	w := testWarden(t, "s12-gate-")
	w.egress = egressStub(t, false)
	s := spec(t, "s12-gate-a", "0.1.0")

	w.reconcile(context.Background(), []Spec{s})
	if n := w.startsOf("s12-gate-a@0.1.0"); n != 0 {
		t.Fatalf("the Warden started a Worker %d time(s) while its egress policy was not installed", n)
	}
	if w.egress.enforced() {
		t.Fatal("a Warden whose `nft` refused every call believes its policy is installed")
	}

	// CONTROL — the same loop, the same Worker, with an `nft` that accepts. If this does not start, the
	// assertion above is about a broken fixture rather than about the gate.
	w.egress = egressStub(t, true)
	w.reconcile(context.Background(), []Spec{s})
	if n := w.startsOf("s12-gate-a@0.1.0"); n != 1 {
		t.Fatalf("with the policy installed the Warden started the Worker %d time(s), want 1", n)
	}
	eventually(t, w, "the Worker is running once the policy is installed", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "s12-gate-a")
		return ok && h.whole()
	})
}

// namedDriver is a driver that answers to a name it was given. The gate above is about the FIELD; this
// is what lets a test ask which drivers `serve` gives the field to.
type namedDriver struct {
	workerDriver
	name string
}

func (n namedDriver) driverName() string { return n.name }

// `kontra warden serve` BUILDS THE POLICY FOR `podman` AND NOT FOR `process`.
//
// The same seam `TestTheReconcileLoopReportsItsTurnsToTheWatcher` holds for the watchpoint, and for the
// same reason it was written: every other test in this package sets the field itself, so a `serve` that
// FORGOT it would leave a Fleet with no egress policy at all and leave the whole suite green. The two
// halves are asserted together because each one alone is satisfiable by a mistake — always-nil passes
// the second, always-set passes the first.
//
// DELETE THE `if drv.driverName() == "podman"` BLOCK FROM `newServeWarden` AND THE FIRST HALF GOES RED.
func TestServeGivesThePodmanDriverAnEgressPolicyAndTheProcessDriverNone(t *testing.T) {
	id := &wardenIdentity{Record: wardenRecord{WardenID: "wdn-egress-seam"}}
	base := &ProcessDriver{out: io.Discard, err: io.Discard}

	served := newServeWarden(id, namedDriver{workerDriver: base, name: "podman"}, time.Second)
	if served.egress == nil {
		t.Error("`kontra warden serve --driver podman` builds a Warden that enforces no egress policy, " +
			"so every Machine in the Fleet would run a stranger's code with the whole VPC reachable")
	}

	served = newServeWarden(id, namedDriver{workerDriver: base, name: "process"}, time.Second)
	if served.egress != nil {
		t.Error("`kontra warden serve --driver process` acquired an egress policy, but that driver has " +
			"no container to attach one to — the rule would govern the Warden and the operator's shell " +
			"alongside the actor (ADR 0036: `process` is the single-box and airgapped driver)")
	}
}

// A WARDEN WITH NO EGRESS POLICY AT ALL IS NOT GATED, because the `process` driver has no container to
// attach one to and ADR 0036 scopes it to the single box and the airgapped install. Without this, the
// gate above would stop every existing test in this package and `kontra serve` with it.
func TestAWardenWithNoEgressPolicyStartsWorkersAsBefore(t *testing.T) {
	w := testWarden(t, "s12-nogate-")
	if w.egress != nil {
		t.Fatal("a Warden built for tests acquired an egress policy, which would make every reconcile " +
			"test in this package depend on nftables")
	}
	w.reconcile(context.Background(), []Spec{spec(t, "s12-nogate-a", "0.1.0")})
	if n := w.startsOf("s12-nogate-a@0.1.0"); n != 1 {
		t.Fatalf("a Warden with no egress policy started the Worker %d time(s), want 1", n)
	}
}
