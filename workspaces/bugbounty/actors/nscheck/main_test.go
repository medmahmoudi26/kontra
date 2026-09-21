package main

// A Method must be testable without a worker, and this one must be testable without the DNS.
//
// The interesting claim this actor makes is not "it can look up a name" — it is that `ask`
// queries THE SERVER NAMED IN THE DELEGATION rather than whatever the host's resolver would have
// answered. That is a claim about a custom Dial, and asserting it against real infrastructure
// would be asserting it against whatever the network happened to do that morning. So the tests
// stand up a responder on loopback that answers with a chosen rcode, and prove both halves at
// once: the pin lands on the right server, and the rcode becomes the right verdict.

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
)

// DNS rcodes, for readability at the call sites below.
const (
	rcodeSuccess  = 0
	rcodeServFail = 2
	rcodeNXDomain = 3
	rcodeRefused  = 5
)

// serveRcode starts a UDP responder that answers every query with `rcode` and no records, and
// points `nsPort` at it for the duration of the test.
//
// It parses nothing. A DNS response only has to echo the query's id and question for the stdlib
// to accept it as a reply, and every question this actor asks is answered the same way — so the
// whole responder is "copy the header and the question, set QR and the rcode, zero the counts".
func serveRcode(t *testing.T, rcode byte) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}

	prev := nsPort
	nsPort = port
	t.Cleanup(func() { nsPort = prev; pc.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return // the listener closed with the test
			}
			resp := reply(buf[:n], rcode)
			if resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}
	}()
}

// nsAnswer is one NS record for whatever the question asked about: a compression pointer back to
// the question's name, then `ns1.test.` as RDATA.
var nsAnswer = []byte{
	0xC0, 0x0C, // NAME: pointer to offset 12, the question's name
	0x00, 0x02, // TYPE: NS
	0x00, 0x01, // CLASS: IN
	0x00, 0x00, 0x00, 0x3C, // TTL: 60
	0x00, 0x0A, // RDLENGTH: 10
	0x03, 'n', 's', '1', 0x04, 't', 'e', 's', 't', 0x00, // RDATA: ns1.test.
}

// reply turns a query into a response with the given rcode.
//
// A NOERROR RESPONSE MUST CARRY A RECORD. Go's resolver reads NOERROR-with-nothing-in-it as "no
// such host", so a responder that only ever set the rcode would report a perfectly healthy
// nameserver as an nxdomain finding — which is this run's false positive, and is exactly
// how the first version of this test failed.
//
// Otherwise it parses nothing: the message is truncated to header + question (+ answer), because
// the section counts say there is nothing else and leaving an EDNS OPT record dangling past them
// would be malformed rather than empty.
func reply(query []byte, rcode byte) []byte {
	if len(query) < 12 {
		return nil
	}
	// Walk the question's labels to their terminating zero, then past QTYPE and QCLASS.
	i := 12
	for i < len(query) && query[i] != 0 {
		i += int(query[i]) + 1
	}
	end := i + 5
	if end > len(query) {
		return nil
	}

	out := make([]byte, end, end+len(nsAnswer))
	copy(out, query[:end])
	out[2] |= 0x80                   // QR: this is a response
	out[3] = 0x80 | rcode            // RA, plus the rcode under test
	copy(out[6:12], make([]byte, 6)) // AN/NS/AR counts, cleared then set below
	if rcode == rcodeSuccess {
		out[7] = 1 // ANCOUNT
		out = append(out, nsAnswer...)
	}
	return out
}

// loaded is a Session holding the system resolver both Methods share, exactly as Load builds it.
func loaded(t *testing.T) *kontra.Session {
	t.Helper()
	s := kontra.NewSession(nil)
	s.Set("dns", &net.Resolver{PreferGo: true})
	return s
}

// one runs `ask` over a single Unit and returns the record it pushed. Every Unit pushes exactly
// one row — including every failure — so anything else is a bug in the actor, not in the test.
func one(t *testing.T, unit map[string]any) map[string]any {
	t.Helper()
	b := kontra.TestBatch(unit)
	ds := kontra.TestDataset()
	if err := ask(loaded(t), b, ds); err != nil {
		t.Fatalf("ask: %v", err)
	}
	got := ds.Records()
	if len(got) != 1 {
		t.Fatalf("pushed %d records from one Unit, want exactly 1: %v", len(got), got)
	}
	rec, ok := got[0].(map[string]any)
	if !ok {
		t.Fatalf("pushed a %T, want a record: %v", got[0], got[0])
	}
	return rec
}

// THE RUN'S QUESTION. A nameserver that refuses the domain it is delegated for is the
// finding; `ok` must be false and the verdict must say which way it failed.
func TestAskReportsARefusingNameserverAsNotOk(t *testing.T) {
	serveRcode(t, rcodeRefused)

	rec := one(t, map[string]any{"domain": "lame.test", "ns": "127.0.0.1"})
	if rec["ok"] != false {
		t.Errorf("ok = %v, want false: the server refused the domain it is delegated for", rec["ok"])
	}
	if rec["verdict"] != "refused-or-servfail" {
		t.Errorf("verdict = %v, want refused-or-servfail", rec["verdict"])
	}
	// The pin is the mechanism: a query that had gone to the system resolver would never have
	// reached the responder, and `lame.test` would have failed some other way.
	if rec["ns_addr"] != "127.0.0.1" {
		t.Errorf("ns_addr = %v, want the delegated server's own address", rec["ns_addr"])
	}
}

// SERVFAIL and REFUSED share a bucket because the stdlib reports both as "server misbehaving".
// Pinned as a test so the day that stops being true is a failure here rather than a silent
// reclassification of every finding.
func TestServfailSharesTheRefusedBucket(t *testing.T) {
	serveRcode(t, rcodeServFail)

	if v := one(t, map[string]any{"domain": "lame.test", "ns": "127.0.0.1"})["verdict"]; v != "refused-or-servfail" {
		t.Errorf("verdict = %v, want refused-or-servfail", v)
	}
}

// The sharpest signal: this server says the domain does not exist while its siblings serve it.
func TestNxdomainFromADelegatedServerIsItsOwnVerdict(t *testing.T) {
	serveRcode(t, rcodeNXDomain)

	rec := one(t, map[string]any{"domain": "lame.test", "ns": "127.0.0.1"})
	if rec["ok"] != false || rec["verdict"] != "nxdomain" {
		t.Errorf("ok=%v verdict=%v, want false/nxdomain", rec["ok"], rec["verdict"])
	}
}

// A healthy delegation. Worth its own test because "everything is a finding" is the easiest way
// for a run like this to be useless.
func TestAskReportsAnAnsweringNameserverAsOk(t *testing.T) {
	serveRcode(t, rcodeSuccess)

	rec := one(t, map[string]any{"domain": "good.test", "ns": "127.0.0.1"})
	if rec["ok"] != true || rec["verdict"] != "ok" {
		t.Errorf("ok=%v verdict=%v, want true/ok", rec["ok"], rec["verdict"])
	}
}

// A delegation naming a host that does not resolve — the takeover shape. It is a FINDING, so it
// emits a row; the temptation to treat it as a failed lookup and drop the Unit is exactly what
// would hide it.
func TestAnUnresolvableNameserverIsAFindingNotADrop(t *testing.T) {
	rec := one(t, map[string]any{
		"domain": "acme.test",
		// .invalid is reserved by RFC 2606 and resolves nowhere, so this needs no network.
		"ns": "ns1.nothing-here.invalid.",
	})
	if rec["ok"] != false || rec["verdict"] != "ns-unresolvable" {
		t.Errorf("ok=%v verdict=%v, want false/ns-unresolvable", rec["ok"], rec["verdict"])
	}
	if rec["detail"] == "" || rec["detail"] == nil {
		t.Error("detail is empty: a finding a human has to act on must say what happened")
	}
}

// A domain that delegates to nothing still appears in the output. THE POINT: a run whose
// unanswerable inputs quietly vanish reports "no findings" for a scan that never ran, which is
// the failure this whole system is shaped around.
func TestADomainWithNoNameserversStillEmitsARow(t *testing.T) {
	rec := one(t, map[string]any{"domain": "orphan.test", "ns": "", "note": "the domain has no NS records"})
	if rec["ok"] != false || rec["verdict"] != "no-nameservers" {
		t.Errorf("ok=%v verdict=%v, want false/no-nameservers", rec["ok"], rec["verdict"])
	}
	if rec["detail"] != "the domain has no NS records" {
		t.Errorf("detail = %v, want the note delegation carried through", rec["detail"])
	}
}

// The ONE thing that is a Unit-level failure rather than a finding. A malformed Unit is a claim
// about the input, not about the world, so it isolates (ADR 0023 §13) and the rest of the Batch
// is untouched.
func TestAUnitWithNoDomainIsolatesRatherThanEmitting(t *testing.T) {
	for name, method := range map[string]func(*kontra.Session, *kontra.Batch, *kontra.Dataset) error{
		"delegation": delegation,
		"ask":        ask,
	} {
		t.Run(name, func(t *testing.T) {
			b := kontra.TestBatch(map[string]any{"not-a-domain": "x"})
			ds := kontra.TestDataset()
			err := method(loaded(t), b, ds)
			if err == nil {
				t.Fatal("no error: a Unit with no `domain` must isolate, not push a verdict")
			}
			if !strings.Contains(err.Error(), "domain") {
				t.Errorf("error %q does not name the missing field", err)
			}
			if got := ds.Records(); len(got) != 0 {
				t.Errorf("pushed %v; an isolated Unit contributes nothing to the output", got)
			}
		})
	}
}

// classify is the run's vocabulary, and every verdict string here is a value an operator
// will filter on in SQL. Pinned so renaming one is a deliberate act.
func TestClassifyBucketsTheStdlibsErrorKinds(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&net.DNSError{Err: "i/o timeout", IsTimeout: true}, "timeout"},
		{&net.DNSError{Err: "no such host", IsNotFound: true}, "nxdomain"},
		{&net.DNSError{Err: "server misbehaving"}, "refused-or-servfail"},
		{&net.DNSError{Err: "something else"}, "error"},
		{errors.New("not a DNS error at all"), "error"},
	}
	for _, c := range cases {
		if got, _ := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A server that never answers is a failed delegation from a resolver's point of view, and must
// not hang the run: the bound is dialTimeout, per query.
func TestASilentNameserverTimesOutRatherThanHanging(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_, port, _ := net.SplitHostPort(pc.LocalAddr().String())
	prev := nsPort
	nsPort = port
	defer func() { nsPort = prev }()

	start := time.Now()
	verdict, _ := querySOA("127.0.0.1", "silent.test")
	if verdict != "timeout" {
		t.Errorf("verdict = %q, want timeout", verdict)
	}
	// Generous, because the stdlib retries within its own deadline; the assertion is that a
	// bound exists at all, not what it is to the millisecond.
	if elapsed := time.Since(start); elapsed > 3*dialTimeout {
		t.Errorf("took %v, want the query bounded near %v", elapsed, dialTimeout)
	}
}
