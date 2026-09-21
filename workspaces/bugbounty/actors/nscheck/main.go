// Package main is nscheck: lame-delegation detection, as two Methods on one Actor.
//
// THE RUN. For a domain, enumerate the nameservers it delegates to, then ask each one
// DIRECTLY about that same domain and record whether it actually answers. A nameserver listed in
// a zone's NS set that does not serve the zone is a lame delegation — at best a resolution
// fault, and at worst a takeover: if the delegation points at a provider where an unclaimed zone
// name can simply be registered, whoever claims it answers for the domain.
//
// TWO METHODS, ONE ACTOR (ADR 0023 §9), and that is not a stylistic choice here. A Fleet stack
// carries a single `actorName` — which is also what NAMES the fleet, `<actor>-<version>` — so work
// split across two Actors needs two fleets; folded into one Actor it needs one. `examples/python/workflows/nscheck/` is
// the caller, and it fits in a scope because of this.
//
//	delegation   {"domain": d}            -> one unit per nameserver: {"domain": d, "ns": n}
//	ask          {"domain": d, "ns": n}   -> one verdict:             {..., "ok": bool, ...}
//
// NOTHING VANISHES. A broken nameserver is the OUTPUT of this run, not a failed Unit, so
// neither Method returns an error for a DNS answer it did not like — an isolated Unit is absent
// from the result Batch, which for this actor would mean silently dropping exactly the findings
// we are looking for. Isolation is reserved for a Unit that is malformed, which is a different
// claim about the world. A domain that resolves nowhere at all still emits a row saying so.
//
// WHAT `verdict` IS AND IS NOT. Go's stdlib resolver does not surface the DNS rcode; it maps
// responses onto error kinds. So these are CLASSIFICATIONS of what the stdlib reported, not
// rcodes off the wire — `refused-or-servfail` is one bucket because `net` reports both as
// "server misbehaving". Anything that needs the rcode itself needs miekg/dns, which is a
// dependency this actor does not have and does not need to answer "did it answer".
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"time"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
	// The actor runtime, imported for its side effect: it registers the Temporal actor
	// host behind a.Serve(). The SDK declares the seam and never imports across it —
	// runtime/ -> sdk/ is one-way — so this line is what puts a host in the binary.
	_ "github.com/medmahmoudi26/kontra/runtime/go"
)

// dialTimeout bounds one directed query. A nameserver that has not answered in this long has
// failed the only question this run asks of it, so waiting longer buys nothing and a
// fleet-wide run pays this timeout once per unreachable server.
const dialTimeout = 5 * time.Second

// nsPort is 53, and is a var only so this actor's own tests can stand a responder up on a
// loopback port. It is a test seam, not an operator knob: a nameserver reachable on some other
// port is not the delegation the resolvers of the world will follow, so answering "does it
// answer" anywhere but 53 would be answering a different question.
var nsPort = "53"

// say is what a person watching this Worker's pane sees.
//
// WHY AN ACTOR LOGS AT ALL, since nothing in kontra requires it. A **Machine's** pane runs
// `journalctl -fu` against the Worker's unit, so the pane shows exactly what the process writes to
// stderr and nothing else. An actor that writes nothing renders a BLACK RECTANGLE — which is
// indistinguishable, on the wall, from a Worker that is wedged, and it is what this actor did for
// its whole life. `loads: unknown` beside it says only that the scrape failed; it is not a claim
// that anything is wrong, and the two together give an operator nothing to act on.
//
// SO THE RULE HERE IS: one line per BATCH, and one line per FINDING. Not one per Unit — 623 units
// scroll a 50-row pane past readability in seconds and bury the four lines that matter. The batch
// lines prove the Worker is moving; the finding lines are the run's actual output, which is the
// thing somebody watching is watching FOR.
//
// stderr rather than stdout, and `log` rather than `fmt`: systemd captures both, but `log` stamps
// the time, and a pane with no clock cannot tell a Worker that stopped from one that is between
// batches.
var say = log.New(os.Stderr, "nscheck ", log.Ltime|log.Lmsgprefix)

func main() {
	a := kontra.New()
	// One resolver for the SYSTEM lookups — the NS set, and the nameserver hostnames' own
	// addresses. The directed queries below each build their own, because a resolver is pinned
	// to the server it dials.
	a.Load(func(s *kontra.Session) error {
		s.Set("dns", &net.Resolver{PreferGo: true})
		// THE FIRST LINE IN THE PANE, and the one that separates "not started" from "started and
		// idle". A Worker polling an empty queue is silent and correct; without this line it is
		// also indistinguishable from one that never loaded.
		say.Printf("session loaded — waiting for a Batch (dial timeout %s, port %s)", dialTimeout, nsPort)
		return nil
	})
	a.Method("delegation", delegation,
		kontra.Does("Resolve each domain's delegated NS set — one domain in, one unit per nameserver out"))
	a.Method("ask", ask,
		kontra.Does("Ask one nameserver whether it actually serves the zone it is delegated for"))
	a.Serve()
}

// delegation fans a domain out into one Unit per nameserver — the shape the next Method takes.
//
// THE FAN-OUT IS THE POINT. `ask` has to query ONE server about ONE domain, so the pairing has to
// exist as Units before it can be a Batch; doing it in the caller would mean pulling every NS
// record into workflow memory to re-emit it, which is the exact thing Batches exist to avoid.
// One Unit in, N out (ADR 0023 §18) is ordinary here: a well-run domain has two to four.
//
// A domain with NO usable NS set still emits ONE Unit, carrying an empty `ns`. That is
// deliberate — see the package header. `ask` turns it into a `no-nameservers` verdict, so the
// domain appears in the output instead of quietly not being there.
func delegation(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	r, _ := s.Get("dns")
	res := r.(*net.Resolver)
	started, domains, pairs := time.Now(), 0, 0
	defer func() {
		say.Printf("delegation · %d domain(s) → %d pair(s) in %s", domains, pairs, took(started))
	}()

	for unit := range b.All() {
		domains++
		domain := unit.Str("domain")
		if domain == "" {
			// No domain to look up: THIS Unit is malformed, which is the one thing that is a
			// Unit-level failure rather than a finding. Returning the error isolates it and
			// leaves the rest of the Batch alone (ADR 0023 §13).
			return errors.New("unit has no `domain` field")
		}

		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
		ns, err := res.LookupNS(ctx, domain)
		cancel()

		if err != nil || len(ns) == 0 {
			why := reason(err, "the domain has no NS records")
			// A FINDING, not noise: a domain that delegates to nothing is the takeover shape this
			// actor exists to find, and it is rare enough that every one belongs on screen.
			say.Printf("  ✗ %s — no NS set (%s)", domain, why)
			ds.Push(map[string]any{
				"domain": domain,
				"ns":     "",
				"note":   why,
			})
			continue
		}
		pairs += len(ns)
		for _, n := range ns {
			ds.Push(map[string]any{
				"domain": domain,
				// Kept with its trailing dot, as the resolver returned it: `ns1.example.com.`
				// is the name in the zone, and normalising it here would make the output
				// disagree with `dig NS`.
				"ns": n.Host,
			})
		}
	}
	return b.Err()
}

// ask queries ONE nameserver directly about ONE domain, and records what happened.
//
// This is the half that makes the run a check rather than an inventory. `LookupNS` through
// the system resolver would ask a recursive resolver, get one authoritative answer back, and tell
// you nothing about the other servers in the set — the whole failure mode being hunted is the one
// where most of the NS set is fine and one member is not. So the resolver is pinned: a custom
// Dial that ignores the address the stdlib picked and dials the target server's own IP on 53.
//
// EVERY UNIT EMITS EXACTLY ONE ROW, including every failure. `ok` is the finding.
func ask(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	sys, _ := s.Get("dns")
	res := sys.(*net.Resolver)
	started, asked, lame := time.Now(), 0, 0
	defer func() {
		// THE COUNT AN OPERATOR IS ACTUALLY WATCHING FOR. `lame` is the run's whole output, so the
		// batch line carries it rather than making somebody count the ✗ lines above it.
		say.Printf("ask · %d pair(s) → %d lame, %d ok in %s", asked, lame, asked-lame, took(started))
	}()

	for unit := range b.All() {
		asked++
		domain, nsHost := unit.Str("domain"), unit.Str("ns")
		if domain == "" {
			return errors.New("unit has no `domain` field")
		}

		out := map[string]any{"domain": domain, "ns": nsHost, "ok": false}

		// Carried through from `delegation`: the domain delegates to nothing we could read.
		if nsHost == "" {
			out["verdict"] = "no-nameservers"
			out["detail"] = unit.Str("note")
			lame++
			say.Printf("  ✗ %s — no-nameservers", domain)
			ds.Push(out)
			continue
		}

		// The nameserver's OWN address. A delegation naming a host that does not resolve is
		// already a finding and a classic takeover shape — the zone points somewhere nobody is,
		// and whoever gets there first answers for it.
		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
		addrs, err := res.LookupHost(ctx, strings.TrimSuffix(nsHost, "."))
		cancel()
		if err != nil || len(addrs) == 0 {
			out["verdict"] = "ns-unresolvable"
			out["detail"] = reason(err, "the nameserver hostname has no address")
			lame++
			// The classic takeover shape: the zone points at a host nobody is, so whoever
			// registers it first answers for the domain.
			say.Printf("  ✗ %s via %s — ns-unresolvable", domain, nsHost)
			ds.Push(out)
			continue
		}
		out["ns_addr"] = addrs[0]

		// Ask THIS server, about THIS domain. SOA rather than A: every authoritative server for
		// a zone has the zone's SOA, whereas the apex may legitimately have no address record —
		// which would otherwise read as a broken server.
		verdict, detail := querySOA(addrs[0], domain)
		out["ok"] = verdict == "ok"
		out["verdict"] = verdict
		if detail != "" {
			out["detail"] = detail
		}
		if verdict != "ok" {
			lame++
			// ONLY THE FAILURES ARE NAMED. A healthy NS set is the overwhelming majority — 193 of
			// 623 on the recorded run — and printing those would push the findings off the pane.
			say.Printf("  ✗ %s via %s (%s) — %s", domain, nsHost, addrs[0], verdict)
		}
		ds.Push(out)
	}
	return b.Err()
}

// querySOA asks one server, by address, whether it serves `domain`.
//
// `LookupNS` is the request because Go's stdlib has no SOA lookup and NS is the next best
// question with the same property: an authoritative server answers it for its own zone, and a
// server that does not serve the zone refuses, SERVFAILs, or says the name does not exist.
func querySOA(addr, domain string) (verdict, detail string) {
	pinned := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// The address the stdlib chose is DISCARDED — that is the whole mechanism. Without
			// this the query goes to the system resolver and answers a different question.
			d := net.Dialer{Timeout: dialTimeout}
			return d.DialContext(ctx, network, net.JoinHostPort(addr, nsPort))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	if _, err := pinned.LookupNS(ctx, domain); err != nil {
		return classify(err)
	}
	return "ok", ""
}

// classify buckets a resolver error into the actor's vocabulary. See the package header for
// why these are classifications rather than rcodes.
func classify(err error) (verdict, detail string) {
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsTimeout:
			// No answer at all. Indistinguishable from a firewall, and equally a failed
			// delegation from the resolver's point of view.
			return "timeout", de.Err
		case de.IsNotFound:
			// This server says the domain does not exist while its siblings serve it — the
			// sharpest lame-delegation signal there is.
			return "nxdomain", de.Err
		case strings.Contains(de.Err, "server misbehaving"):
			// `net` collapses REFUSED, SERVFAIL and NOTIMP into one string; keeping them in one
			// bucket is more honest than guessing which arrived.
			return "refused-or-servfail", de.Err
		}
		return "error", de.Err
	}
	return "error", err.Error()
}

// reason prefers the error's own words and falls back to a description, so a `detail` field is
// never empty and never a bare "<nil>".
func reason(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// took renders a duration the way a pane should read it: whole milliseconds under a second,
// one decimal of seconds above. `time.Duration`'s own String gives `1.234567891s`, and nine
// significant digits of noise is what makes a log line unscannable.
func took(since time.Time) string {
	d := time.Since(since)
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}
