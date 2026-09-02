package main

// warden_egress.go — where a **Machine's** Workers may reach, enforced OUTSIDE the container.
//
// ADR 0036 names this as the exposure it deliberately does not close: "kontra's actors sweep DNS and
// crawl; the abuse case is renting the platform to scan a third party from kontra's addresses. That is
// bounded by policy on the **Machine**, outside the container, and is 0037's problem because it belongs
// to the **Warden**." ADR 0037 gives the Warden the duty in one clause — it "enforces egress policy".
// This file is that clause.
//
// ═══ OUTSIDE THE CONTAINER IS THE WHOLE DESIGN, AND IT MEANS ONE SPECIFIC THING ═══
//
// kontra runs code it did not write. A policy the workload can edit is not a policy, so the only
// question worth answering is WHERE the rule lives and why the actor cannot reach it. The answer is
// not "the container has no tools" and not "we dropped its capabilities" — both of those are one
// `--cap-add` away from being false. It is:
//
//	NFTABLES STATE IS PER NETWORK NAMESPACE, AND THE RULES ARE IN THE HOST'S.
//
// A netlink socket only ever addresses the nf_tables instance of its OWN network namespace; there is
// no addressing mode that names another one. A Worker is in its pod's namespace (driver_podman.go: "ONE
// NETWORK NAMESPACE PER WORKER"), so no message it can send names these rules. Getting a socket in the
// host's namespace means entering it, which means `setns` on a namespace file descriptor — and both the
// descriptor and the permission are absent: the container's `/proc` shows only its own pid namespace,
// and `setns` would need CAP_SYS_ADMIN in `init_user_ns`, which `cap_capable()` never grants to a
// process whose credentials live in a DESCENDANT user namespace. It walks from the target namespace
// toward the root and returns -EPERM the moment it reaches `init_user_ns` without having found the
// caller's own. That is true for every container the runtime can produce, including a `--privileged`
// one, because privilege is scoped to a user namespace and the one that owns the host's network
// namespace is not the container's.
//
// MEASURED ON THIS BOX, five postures, one static probe binary asking the kernel over
// NETLINK_NETFILTER for every nftables table it can see:
//
//	on the HOST, no container at all                               lists kontra's table
//	--cap-drop ALL --security-opt no-new-privileges --userns=auto  EPERM — cannot even ask
//	--privileged --userns=auto                                     answers, and the list is EMPTY
//	--privileged  (host user namespace, own network namespace)     answers, and the list is EMPTY
//	--privileged --network=host                                    SEES AND CAN EDIT THE RULES
//
// The second row is the posture `runArgs` actually starts a Worker in. The LAST row is the flag
// `assertNoRuntimeAccess` already refuses, and this file is why that refusal is load-bearing rather
// than tidy: driver_podman.go says `--network=host` "removes the per-Worker network namespace a
// Machine's egress policy attaches to", and this is the measurement behind the sentence. The two rows
// between them matter as much — they say the boundary is the NETWORK namespace and not the capability
// set, so this design does not rest on `--cap-drop ALL` staying in an argv.
//
// warden_egress_test.go runs all of these except the fourth, which podman on this box cannot start
// alongside `--network=host` and which the third already settles.
//
// ═══ WHAT THIS NEEDS THAT THE WARDEN MAY NOT HAVE ═══
//
// CAP_NET_ADMIN in the host's network namespace — in practice, root on the Machine. The **Warden**
// installed by `wardenUnit` has it: the unit carries no `User=`, `wardenJoin` writes to
// `/var/lib/kontra/warden` and `/etc/systemd/system`, and `newPodmanDriver` takes its `--userns=auto`
// branch precisely because podman there is ROOTFUL. A Warden that is NOT root cannot install this, and
// this file does not invent a capability for it: `apply` fails, `enforced()` stays false, and
// `warden.reconcile` starts no Worker at all. See `errEgressUnenforceable`.
//
// The `process` driver gets NO egress policy and is not pretended into one. There is no container, no
// pod namespace and therefore no traffic to attach a rule to that is not simply the Machine's own —
// governing it would mean governing the Warden, the SSH session and the package manager alongside the
// actor. ADR 0036 already scopes that driver to "the single box and the airgapped install", where the
// Controller and the operator are the same party, so `newServeWarden` builds a `machineEgress` only for
// `podman`.
//
// ═══ THE ADDRESS AN ACTOR DIALS IS NOT THE ADDRESS THE FILTER SEES ═══
//
// Found by running it, and it would have shipped as a silently broken allow list. Destination NAT
// happens in `prerouting` at priority -100, BEFORE any `forward` or `input` filter hook, so by the time
// a rule reads `ip daddr` the destination has already been rewritten. On this checkout's Controller,
// which publishes its services on the VPC address with Docker, a container dialling `10.124.0.2:6379`
// arrives at the filter as `172.20.0.6:6379`:
//
//	KONTRA-PRIV-DROP IN=cni-podman0 OUT=br-f2f29fdcae60 SRC=10.88.0.129 DST=172.20.0.6 DPT=6379
//
// An allow list holding `10.124.0.2` therefore never matched, and the packet fell through to the
// RFC1918 default-deny — the Machine could not reach its own Controller, and the reason was invisible.
// The DENY half happened to keep working only because both addresses were private, which is luck and
// not a design.
//
// So every address rule is asked TWICE: once against `ip daddr` (what the packet will actually be
// delivered to) and once against `ct original ip daddr` (what the actor dialled, recovered from the
// conntrack tuple, which stores the pre-NAT destination). A destination is denied if EITHER form is
// denied, and allowed if EITHER form is allowed — deny-biased on the way in, intent-following on the
// way out, which is the only pair of defaults that leaves neither an evasion nor a broken Controller.
//
// ═══ WHAT IS NOT HERE ═══
//
//   - NO PORTS. A rule names an address and covers every port on it, so allowing the Controller allows
//     a Worker to reach everything the Controller listens on — including, on this box, an
//     unauthenticated Redis. What the floor and the private default DO buy is that a Worker cannot
//     reach ANOTHER Machine, another tenant's box, or the cloud metadata service. Port granularity is
//     a set of type `ipv4_addr . inet_service` and a second decision about which side of a DNAT the
//     port is read from; it is not smuggled in here.
//   - NO PER-WORKER POLICY. The policy is the assignment's, and an assignment is one Machine's, and a
//     Machine is one tenant's (ADR 0036: a tenant is a namespace, carried in the certificate). ADR 0037
//     already says packed "Workers may share a Machine, and they share its egress address", so the
//     Machine is the unit the ADR reasons about. Per-Worker would mean a network per pod so that
//     `iifname` still names it; that is a driver change, and it is slice 12's to leave alone.
//   - NO `ct state established,related accept` SHORTCUT. Every packet is judged, so tightening an
//     assignment takes effect on the next PACKET rather than on the next connection. A long-lived
//     scan holding one socket open is exactly the thing a tightened policy has to be able to stop.
//   - NO EGRESS ANTI-SPOOF. Spoofing a SOURCE address does not evade a DESTINATION policy, so a rule
//     for it here would be a claim this file cannot back. Source validation is `rp_filter`, and it is
//     a Machine-provisioning setting rather than a Warden one.
//   - IPv6 IS GOVERNED AND MEASURED, with one half of it that is not. A deny list is written in IPv4
//     because that is the address an operator is given, and the same service answers on IPv6; on a
//     network created with `--ipv6` a container here REACHED the public v6 internet, and with the
//     policy installed the denied v6 address was refused while another kept working. What is NOT
//     measured is the opposite bridge: a container that has IPv6 while podman declares none would need
//     a router advertisement this Machine has nothing to send, so `iifname { … } meta nfproto ipv6
//     drop` is asserted against the rendered ruleset and never against a packet.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// --- what an assignment may say --------------------------------------------------------------------

// egressPolicy is where one **Machine's** Workers may reach, as the control plane states it.
//
// TWO LISTS AND NO DEFAULT FIELD. There is no `mode: deny-all`, because the defaults are not the
// assignment's to choose: the floor and the private-network deny are properties of the Machine (see
// `egressFloor`/`egressPrivate`) and an assignment that could switch them off would be an assignment
// that could switch the control off. What an assignment CAN do is carve holes in the private default
// (`Allow`) and close specific public destinations (`Deny`).
//
// AN ENTRY IS AN ADDRESS OR A CIDR, and a malformed one refuses the WHOLE policy rather than being
// skipped. Both halves of a silent skip are bad in a way that is invisible: a dropped `Allow` entry is
// a Machine that mysteriously cannot reach its Controller, and a dropped `Deny` entry is a control
// that is not enforcing the one thing somebody wrote it for.
type egressPolicy struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// egressTable is the nftables table the **Warden** owns entirely. Nothing else may write into it and
// it is replaced whole on every turn, so there is no merge and no leftover rule from a previous
// policy — see `apply`.
const egressTable = "kontra_warden"

// egressHookPriority puts these chains BEFORE the ordinary filter tables.
//
// `iptables-nft` installs its `filter` chains at priority 0, and this box's ufw ruleset lives there;
// two base chains at the same priority are evaluated in creation order, which is not a thing to depend
// on. A `drop` is terminal wherever it happens, so an earlier priority is not needed for the denies to
// win — it is needed so that the ACCEPTS this table issues are not decided after somebody else's rule
// has already had its say about the same packet.
const egressHookPriority = -10

// egressFloor is what no assignment may allow.
//
// 169.254.0.0/16 is IPv4 link-local, and on every cloud this Fleet runs on it is also the metadata
// service — 169.254.169.254 hands out the Machine's own instance credentials to anything that asks,
// with no authentication, over plain HTTP. Measured on this Controller before any rule was installed:
// a container started with `--cap-drop ALL` REACHED it. A tenant who could add that address to an
// allow list would be a tenant who could ask for the Machine's cloud credentials, so the floor is
// checked BEFORE the allow list and the ordering is the whole point of having two sets.
//
// fe80::/10 is the same range in IPv6 and is included for the same reason plus one more: IPv6
// link-local is configured automatically on every interface, so it is reachable without anybody
// routing it.
var egressFloor = []string{"169.254.0.0/16", "fe80::/10"}

// egressPrivate is denied unless the assignment names something inside it.
//
// THIS IS THE LATERAL-MOVEMENT CONTROL. The Machines of a Fleet share a VPC with each other and with
// the Controller; an actor that can dial 10.0.0.0/8 can reach every other tenant's Machine on it. The
// three RFC1918 ranges are the issue's own words. The other two are here because they are private in
// practice and a reader should not have to wonder:
//
//	100.64.0.0/10  carrier-grade NAT, and what Tailscale numbers a tailnet out of — this box has a
//	               tailscale0 interface, so it is a real path to somebody else's machines
//	127.0.0.0/8    a container's loopback is its own namespace's, so this never fires on the way out;
//	               it is here so that a policy which somehow saw one would not accept it
//
// fc00::/7 is the IPv6 unique-local range and ::1 its loopback.
var egressPrivate = []string{
	"10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"::1/128", "fc00::/7",
}

// errEgressUnenforceable is what a Warden that cannot install the policy reports, and what stops it
// starting Workers.
var errEgressUnenforceable = errors.New("this Machine's egress policy is not installed")

// --- the container networks the policy attaches to ---------------------------------------------------

// containerNet is one network a Worker's pod can be on, as the runtime describes it.
//
// THE INTERFACE NAME IS THE SELECTOR AND THAT IS DELIBERATE. A bridge lives in the HOST's network
// namespace, so `iifname` names something a container has no way to rename, remove or move itself off
// — unlike a source address, which a Worker holding CAP_NET_ADMIN in its own namespace could change.
type containerNet struct {
	Name    string
	Iface   string
	Subnets []netip.Prefix
}

// podmanNets asks podman which networks exist and what each one's bridge is called.
//
// A READ OF THE RUNTIME, for the same reason `list` is (driver.go: "the runtime is the truth"). The
// alternative is a configured interface name, which is a second place to be wrong about a fact podman
// already knows — and being wrong here is silent, because a policy attached to an interface no packet
// arrives on enforces nothing while looking perfectly installed.
//
// EVERY NETWORK, NOT JUST THE ONE THE DRIVER USES. `podmanDriver.start` passes no `--network`, so its
// pods are on the default one today; a policy that hard-coded that would stop covering the Workers the
// day somebody adds the flag, and the failure would be an ungoverned actor rather than an error.
func podmanNets(ctx context.Context, bin string) ([]containerNet, error) {
	out, err := exec.CommandContext(ctx, bin, "network", "ls", "--format", "{{.Name}}").Output()
	if err != nil {
		return nil, fmt.Errorf("podman network ls: %w", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("podman reports no networks at all, so there is no interface a Machine's " +
			"egress policy could attach to")
	}

	// ONE FIELD AT A TIME THROUGH --format, rather than parsing podman's JSON. The two shapes podman
	// 4.x prints for a network differ between `ls` and `inspect` — `ls --format json` omitted `subnets`
	// for a network `inspect` reported them for, on this box — and a struct that silently decoded the
	// thinner one would produce a network with no subnets and no error.
	args := append([]string{"network", "inspect", "--format",
		"{{.Name}}\t{{.NetworkInterface}}\t{{range .Subnets}}{{.Subnet}} {{end}}"}, names...)
	out, err = exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("podman network inspect: %w", err)
	}
	return parsePodmanNets(string(out))
}

// parsePodmanNets reads the three tab-separated fields back. Split out so the shapes podman prints can
// be pinned by a test without a runtime.
func parsePodmanNets(out string) ([]containerNet, error) {
	var nets []containerNet
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			return nil, fmt.Errorf("podman described a network as %q, which has no interface in it", line)
		}
		n := containerNet{Name: strings.TrimSpace(f[0]), Iface: strings.TrimSpace(f[1])}
		// A NETWORK WITH NO BRIDGE IS NOT AN ERROR AND IS NOT GOVERNABLE. podman's `none` and `host`
		// networks have no interface of their own; the second is what `assertNoRuntimeAccess` refuses a
		// Worker onto, and neither has traffic that arrives on an interface this policy could name.
		if n.Iface == "" || n.Iface == "<no value>" {
			continue
		}
		if len(f) > 2 {
			for _, s := range strings.Fields(f[2]) {
				p, err := netip.ParsePrefix(s)
				if err != nil {
					return nil, fmt.Errorf("podman describes network %s with subnet %q: %w", n.Name, s, err)
				}
				n.Subnets = append(n.Subnets, p)
			}
		}
		nets = append(nets, n)
	}
	if len(nets) == 0 {
		return nil, errors.New("podman describes no network with a bridge interface, so there is nothing " +
			"a Machine's egress policy could attach to")
	}
	return nets, nil
}

// --- what the Machine itself needs, which is not the assignment's to remember -------------------------

// egressDerived is what a **Machine's** Workers must be able to reach for the Machine to work at all,
// derived from facts the assignment did not write.
//
// SAME RULE AS `assignedWorker.derivedEnv`, one layer down. That function adds KONTRA_NAMESPACE from
// the certificate because "an assignment is a file an operator writes, and the one thing a file must
// not be able to say is which tenant the Workers on this Machine belong to". The same holds here for a
// different reason: the Controller and Temporal are on a PRIVATE address in every deployment this Fleet
// has, so the private default-deny would cut every Worker off from the control plane the moment the
// policy was installed, and an operator who forgot one line in a JSON file would have a Fleet of
// Machines running Workers that cannot poll. Deriving it means that line does not exist to be forgotten.
//
// IT IS AN ALLOW AND THEREFORE IT IS UNDER THE FLOOR, not over it: these entries go into the same
// `allow` sets the assignment writes and are checked after `egressFloor`. A Controller URL that somehow
// resolved to 169.254.169.254 would still be refused.
//
// THE RESOLVERS ARE HERE BECAUSE A POLICY THAT KILLS DNS IS A POLICY THAT GETS TURNED OFF. On this box
// podman hands a container the upstream servers from `/run/systemd/resolve/resolv.conf` (67.207.67.2,
// public, so the default-deny never touched them) and NOT the `127.0.0.53` stub that `/etc/resolv.conf`
// carries — measured. On a Machine whose resolver is a VPC address, the private deny would silently
// break every actor that crawls by name, and the symptom would be an actor bug report rather than a
// firewall one. Both files are read for exactly that reason, and loopback entries are dropped because a
// container reaching 127.0.0.53 reaches its own namespace and never leaves.
func egressDerived(rec wardenRecord) []string {
	var out []string
	for _, s := range []string{rec.Controller, rec.Temporal} {
		if h := egressEndpointHost(s); h != "" {
			out = append(out, h)
		}
	}
	out = append(out, resolvConfServers("/etc/resolv.conf", "/run/systemd/resolve/resolv.conf")...)
	return out
}

// egressEndpointHost takes the host out of a URL or a `host:port`.
//
// NOT `doctor.go`'s `hostOf`, and the difference is the empty case: that one answers "localhost" so a
// UI link always points somewhere, which is right for a link and wrong here. `wardenRecord.Temporal`
// is EMPTY on a Machine enrolled against a Controller with no control plane — the airgapped case, not
// a failure — and turning that into `localhost` would put a resolved loopback address into a Machine's
// allow list for no reason anybody wrote. It also reads a bracketed IPv6 host, which a link builder
// never sees and an endpoint does.
func egressEndpointHost(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		if h := u.Hostname(); h != "" {
			return h
		}
	}
	if h, _, err := net.SplitHostPort(s); err == nil && h != "" {
		return h
	}
	return s
}

// resolvConfServers reads `nameserver` lines out of the files given, skipping loopback.
func resolvConfServers(paths ...string) []string {
	var out []string
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = strings.TrimSpace(line[:i])
			}
			rest, ok := strings.CutPrefix(line, "nameserver")
			if !ok {
				continue
			}
			a, err := netip.ParseAddr(strings.TrimSpace(rest))
			if err != nil || a.IsLoopback() {
				continue
			}
			out = append(out, a.String())
		}
	}
	return out
}

// --- rendering the ruleset ---------------------------------------------------------------------------

// egressSet is one named nftables set, already split by family.
type egressSet struct {
	name string
	v4   []string
	v6   []string
}

// resolveEgress turns a list of addresses, CIDRs and host names into nftables set elements, split by
// family.
//
// A NAME IS RESOLVED HERE AND NOT LEFT TO nft. nftables would resolve a host name once, at load time,
// and bake the answer in; this file re-renders on every reconcile turn, so a Controller that moved is
// followed within one interval. A name that does not resolve is an ERROR rather than an empty set, for
// the reason this whole file is built on: a rule that silently matches nothing is the failure mode.
func resolveEgress(what string, entries []string) (egressSet, error) {
	s := egressSet{name: what}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			s.add(p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil {
			// UNMAPPED, because `::ffff:10.0.0.1` and `10.0.0.1` are the same destination and only one
			// of them belongs in an ipv4 set. Without this, the mapped spelling silently lands in the
			// v6 set, where no packet from a v4 container will ever match it.
			a = a.Unmap()
			s.add(netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		ips, err := net.LookupIP(e)
		if err != nil || len(ips) == 0 {
			return egressSet{}, fmt.Errorf("the %s list names %q, which is neither an address, nor a CIDR, "+
				"nor a name this Machine can resolve (%v) — a rule that matched nothing would leave this "+
				"policy looking installed while enforcing something else", what, e, err)
		}
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip); ok {
				a = a.Unmap()
				s.add(netip.PrefixFrom(a, a.BitLen()))
			}
		}
	}
	return s, nil
}

func (s *egressSet) add(p netip.Prefix) {
	if p.Addr().Is4() {
		s.v4 = append(s.v4, p.String())
		return
	}
	s.v6 = append(s.v6, p.String())
}

// elements renders one family's elements, SORTED AND DEDUPLICATED. Sorted because the ruleset is
// compared against the last one applied and against a fixture, and Go's iteration order would otherwise
// make two identical policies render differently — the same reason `envList` sorts.
func elements(in []string) string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, e := range in {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// renderEgress is the whole ruleset, as `nft -f` reads it.
//
// THE FIRST TWO LINES ARE A CREATE-THEN-DELETE AND THEY ARE NOT A MISTAKE. `table inet kontra_warden`
// on its own creates the table if it is absent and does nothing if it is present, so the `delete` that
// follows can never fail on a Machine seeing this for the first time. `nft -f` sends the whole file as
// ONE netlink transaction, so the delete and the rebuild are atomic: there is no instant at which the
// Machine is running Workers with no policy, which a flush-then-load would have.
//
// THE TABLE NAME IS AN ARGUMENT so a test can install a real ruleset on a real kernel without fighting
// a Warden that may be serving on the same box. Everything else about it is identical.
func renderEgress(table string, nets []containerNet, floor, private, allow, deny egressSet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\n\ntable inet %s {\n", table, table, table)

	ifaces := make([]string, 0, len(nets))
	for _, n := range nets {
		ifaces = append(ifaces, `"`+n.Iface+`"`)
	}
	sort.Strings(ifaces)
	fmt.Fprintf(&b, "\tset bridges {\n\t\ttype ifname\n\t\telements = { %s }\n\t}\n", strings.Join(ifaces, ", "))

	for _, s := range []egressSet{floor, private, allow, deny} {
		for _, fam := range []struct {
			suffix, typ string
			el          []string
		}{{"4", "ipv4_addr", s.v4}, {"6", "ipv6_addr", s.v6}} {
			// ═══ auto-merge, OR AN ORDINARY ALLOW LIST TAKES THE MACHINE DOWN ═══
			//
			// An interval set refuses OVERLAPPING elements outright: `nft` answers "conflicting
			// intervals specified" and the WHOLE ruleset fails to load. Found by a test that allowed
			// both `169.254.169.254` and `169.254.0.0/16`, but the reachable case is far more ordinary
			// — an assignment allowing a VPC range and then one host inside it, which is what an
			// operator writes when they add a second Controller. The consequence is not a bad rule, it
			// is NO rule: `apply` fails, `enforced()` stays false, and `reconcile` then starts no Worker
			// at all. A policy whose failure mode is an empty Machine is a policy people switch off.
			//
			// `auto-merge` makes the kernel take the union, which is what a list of allowed
			// destinations means anyway.
			fmt.Fprintf(&b, "\tset %s%s {\n\t\ttype %s\n\t\tflags interval\n\t\tauto-merge\n", s.name, fam.suffix, fam.typ)
			// AN EMPTY SET IS WRITTEN AS AN EMPTY SET, never omitted. Every rule below names one, so a
			// missing set is a ruleset `nft` refuses to load — which would turn "this assignment has no
			// deny list" into "this Machine has no policy at all".
			if el := elements(fam.el); el != "" {
				fmt.Fprintf(&b, "\t\telements = { %s }\n", el)
			}
			b.WriteString("\t}\n")
		}
	}

	b.WriteString("\n\tchain egress {\n")
	// THE SELECTOR. Everything below this line is about a packet that entered the Machine from a
	// container bridge; a reply arriving on the UPLINK returns here untouched, which is why these chains
	// can carry a `drop` with no conntrack exemption. A reply from a container on a governed bridge is
	// the one exception and it is handled below — see the `ct original` note.
	b.WriteString("\t\tiifname != @bridges return\n")
	// ═══ IPv6 THE RUNTIME NEVER HANDED OUT IS NOT TRAFFIC, IT IS A BYPASS ═══
	//
	// THE BYPASS, STATED PLAINLY: an assignment's `deny` list names a host, an operator writes it in
	// IPv4 because that is the address they were given, and the SAME service answers on IPv6. The
	// public fall-through at the bottom of this chain lets the v6 address straight out, with the deny
	// list intact and irrelevant. Nothing about that requires an attacker — it is what happens when
	// somebody denies `1.1.1.1` and the actor resolves AAAA.
	//
	// Measured on this box, both halves. On the default podman network, which declares no IPv6 subnet,
	// a container has no v6 route at all — "connect: network is unreachable" — so there is nothing to
	// leak. On a network created with `--ipv6`, the same container REACHED 2606:4700:4700::1111 and
	// 2620:fe::fe on the public internet. So the exposure is real exactly where podman hands out a v6
	// address, and absent exactly where it does not.
	//
	// PER BRIDGE, NOT PER MACHINE, because those two states can coexist on one Machine and do on this
	// one. A Machine with one dual-stack network and one v4-only network must judge the first by the v6
	// rules and refuse the family outright on the second — a single Machine-wide switch would either
	// leave the v4-only bridge trusting rules nobody wrote for it, or ban IPv6 on the network that was
	// deliberately given some.
	if novs := noIPv6Bridges(nets); len(novs) > 0 {
		fmt.Fprintf(&b, "\t\tiifname { %s } meta nfproto ipv6 counter drop\n", strings.Join(novs, ", "))
	}
	// PRECEDENCE, TOP TO BOTTOM, AND EACH STEP IS A DECISION SOMEBODY CAN BE HELD TO:
	//   floor    the assignment may not allow it            (metadata, link-local)
	//   deny     the assignment said no                     (beats its own allow, because a deny an
	//                                                        allow could cancel is not a deny)
	//   allow    the assignment or the Machine said yes
	//   private  the default-deny this control exists for
	//   ...then anything left is public, and is allowed.
	for _, step := range []struct {
		set  string
		verb string
	}{{"floor", "drop"}, {"deny", "drop"}, {"allow", "accept"}, {"private", "drop"}} {
		fmt.Fprintf(&b, "\t\tip daddr @%s4 counter %s\n", step.set, step.verb)
		// THE PRE-DNAT FORM, and see this file's header for the afternoon it cost. `ct original ip
		// daddr` is the destination as the actor dialled it; `ip daddr` is where the packet is actually
		// going. Both are asked because a Machine that publishes ports rewrites one into the other.
		//
		// IT EARNS ITS KEEP A SECOND TIME, FOUND BY DELETING IT. When BOTH ends of a flow are behind
		// governed bridges — one container talking to another on the same Machine — the REPLY also
		// enters on a governed interface, so it is judged by this chain too. Its `ip daddr` is the
		// CLIENT's address, which no allow list names, so it falls through to the private default-deny
		// and the connection dies half-open. Its `ct original ip daddr` is still the destination that
		// was allowed. Measured: with this line removed, an explicitly allowed container-to-container
		// destination became unreachable while the deny half went on working perfectly.

		fmt.Fprintf(&b, "\t\tct original ip daddr @%s4 counter %s\n", step.set, step.verb)
		fmt.Fprintf(&b, "\t\tip6 daddr @%s6 counter %s\n", step.set, step.verb)
		fmt.Fprintf(&b, "\t\tct original ip6 daddr @%s6 counter %s\n", step.set, step.verb)
	}
	b.WriteString("\t}\n")

	// TWO HOOKS, BECAUSE A CONTAINER HAS TWO WAYS OUT. `forward` is traffic the Machine routes on its
	// behalf — the third party it was rented to scan. `input` is the Machine ITSELF, which is where the
	// Warden's own state, the Controller's services and every other container's published port live, and
	// leaving it ungoverned would make the most valuable destination on the network the one exempt one.
	for _, hook := range []string{"forward", "input"} {
		fmt.Fprintf(&b, "\n\tchain %s {\n\t\ttype filter hook %s priority %d; policy accept;\n\t\tjump egress\n\t}\n",
			hook, hook, egressHookPriority)
	}
	b.WriteString("}\n")
	return b.String()
}

// --- installing it ------------------------------------------------------------------------------------

// machineEgress installs and re-installs the **Machine's** policy. One per Warden, and nil on a Warden
// that has no host to enforce on.
type machineEgress struct {
	nft    string
	podman string
	table  string
	// derived is what the Machine needs regardless of the assignment — see `egressDerived`. A function
	// rather than a slice so a test can supply it without a certificate.
	derived func() []string
	out     io.Writer

	// installed is whether a ruleset has EVER been applied successfully, and it is what gates starting
	// Workers. It is not reset by a later failure, and that is deliberate: this file never removes its
	// rules, so a Warden that applied a policy and then lost the ability to re-apply one is still
	// enforcing the last one. What it must not do is start a Worker the policy was never installed for.
	installed bool
	// last is the ruleset currently in the kernel as far as this process knows, kept only so the log
	// says something on a CHANGE rather than every five seconds.
	last string
}

func newMachineEgress(rec wardenRecord, out io.Writer) *machineEgress {
	return &machineEgress{
		nft:     "nft",
		podman:  "podman",
		table:   egressTable,
		derived: func() []string { return egressDerived(rec) },
		out:     out,
	}
}

func (e *machineEgress) enforced() bool { return e != nil && e.installed }

// apply puts the policy in the host's nftables. Idempotent by construction — the ruleset is rebuilt
// from scratch and replaces the table whole, so calling it every turn converges rather than accumulates,
// and an operator who deleted the table by hand gets it back within one interval.
func (e *machineEgress) apply(ctx context.Context, p egressPolicy) error {
	if e == nil {
		return nil
	}
	nets, err := podmanNets(ctx, e.podman)
	if err != nil {
		return err
	}
	floor, err := resolveEgress("floor", egressFloor)
	if err != nil {
		return err
	}
	private, err := resolveEgress("private", egressPrivate)
	if err != nil {
		return err
	}
	allow, err := resolveEgress("allow", append(append([]string{}, p.Allow...), e.derived()...))
	if err != nil {
		return err
	}
	deny, err := resolveEgress("deny", p.Deny)
	if err != nil {
		return err
	}
	table := e.table
	if table == "" {
		table = egressTable
	}
	ruleset := renderEgress(table, nets, floor, private, allow, deny)

	cmd := exec.CommandContext(ctx, e.nft, "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: `%s -f -` refused it (%v: %s).\n"+
			"  Installing it needs CAP_NET_ADMIN in this Machine's OWN network namespace — in practice, "+
			"root. The unit `kontra warden join` writes runs as root; a Warden started by hand as a user "+
			"cannot enforce anything, and this one will start no Workers rather than run a stranger's "+
			"code with no egress policy at all",
			errEgressUnenforceable, e.nft, err, strings.TrimSpace(string(out)))
	}
	if ruleset != e.last {
		e.logf("egress policy installed on %s: %d allowed, %d denied, default-deny to %s",
			strings.Join(ifaceNames(nets), ", "), len(allow.v4)+len(allow.v6), len(deny.v4)+len(deny.v6),
			strings.Join(egressPrivate, " "))
		e.last = ruleset
	}
	e.installed = true
	return nil
}

// noIPv6Bridges names the governed bridges the runtime has given NO IPv6 subnet, quoted for nft.
//
// IT READS WHAT PODMAN SAID, not what the Machine's interfaces hold. The host here has a global IPv6
// address and a default v6 route, and a container on the v4-only network still cannot send a v6 packet
// — because the question is what the RUNTIME handed the container, not what the host could route if
// asked. Reading the host would refuse the family on a Machine where it works, and permit it on one
// where podman had quietly enabled it.
func noIPv6Bridges(nets []containerNet) []string {
	var out []string
	for _, n := range nets {
		v6 := false
		for _, s := range n.Subnets {
			if s.Addr().Is6() && !s.Addr().Is4In6() {
				v6 = true
			}
		}
		if !v6 {
			out = append(out, `"`+n.Iface+`"`)
		}
	}
	sort.Strings(out)
	return out
}

func ifaceNames(nets []containerNet) []string {
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.Iface)
	}
	sort.Strings(out)
	return out
}

func (e *machineEgress) logf(format string, args ...any) {
	if e == nil || e.out == nil {
		return
	}
	fmt.Fprintf(e.out, format+"\n", args...)
}
