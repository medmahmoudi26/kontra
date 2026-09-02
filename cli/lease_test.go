// lease_test.go — what `kontra fleet leases` prints, and what `kontra fleet down` refuses.
//
// The guard is the part worth testing hardest. Before ADR 0037 a **Fleet** had exactly one owner, so
// `down` was unambiguous; now several **Runs** may hold one, and a hand on `down` takes their
// **Machines** with no error anywhere — the **Run** that lost its capacity sees a queue nobody polls
// and reports as slow. The guard has to refuse when it knows the **Fleet** is held AND when it does
// not know, and those are two different code paths with opposite temptations.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// leaseServer stands in for the orchestrator's infra surface. It answers the **Lease** workflow route and
// records what the destroy path did, so "did it start a converge?" is an observable rather than an
// inference.
type leaseServer struct {
	leases  *leaseSet
	status  int
	started []string
}

func (s *leaseServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/leases"):
			if s.status >= 400 {
				w.WriteHeader(s.status)
				_, _ = w.Write([]byte(`{"error":"temporal is unwell"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.leases)
		default:
			s.started = append(s.started, r.Method+" "+r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"fqn":"x","op":"destroy","workflowId":"x","runId":"y"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func leaseAPI(t *testing.T, srv *httptest.Server) *apiClient {
	t.Helper()
	return newAuthAPI(srv.URL, "test-token")
}

const testFqn = "kontra-fleet/nscheck-0.1.0"

func TestDownRefusesAFleetSomebodyIsHolding(t *testing.T) {
	s := &leaseServer{leases: &leaseSet{
		Fleet: testFqn,
		Leases: []leaseView{
			{Lease: "recon-7#a1b2c3d4", Holder: "recon-7", ExpiresAt: time.Now().Add(20 * time.Minute).UnixMilli()},
		},
	}}
	srv := s.start(t)

	err := refuseIfHeld(leaseAPI(t, srv), testFqn, false)
	if err == nil {
		t.Fatal("destroying a Fleet another Run is holding was allowed — that deletes its Machines")
	}
	// The refusal has to NAME the holder, or an operator's only option is --force.
	for _, want := range []string{"recon-7#a1b2c3d4", "recon-7", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
	if len(s.started) != 0 {
		t.Fatalf("a converge was started despite the refusal: %v", s.started)
	}
}

func TestDownProceedsWhenNobodyHoldsIt(t *testing.T) {
	// THE CONTROL. Without this, a guard that refused unconditionally would pass the test above and
	// break every teardown in the repo — including `kontra fleet up`'s own pair, which takes no
	// Lease and must stay destroyable.
	s := &leaseServer{leases: &leaseSet{Fleet: testFqn, Leases: nil}}
	srv := s.start(t)
	if err := refuseIfHeld(leaseAPI(t, srv), testFqn, false); err != nil {
		t.Fatalf("a Fleet nobody holds must still be destroyable: %v", err)
	}
}

func TestDownRefusesWhenItCannotFindOutWhoHoldsIt(t *testing.T) {
	// FAIL CLOSED. "Nobody holds it" and "I could not find out" must not be the same value to
	// something about to delete Machines — the same rule `queuePollers` states from the other side:
	// an answer nobody could get is not a measurement.
	s := &leaseServer{status: 502}
	srv := s.start(t)
	err := refuseIfHeld(leaseAPI(t, srv), testFqn, false)
	if err == nil {
		t.Fatal("an unreadable Lease workflow was treated as an empty one")
	}
	if !strings.Contains(err.Error(), "not safe to destroy") {
		t.Errorf("the refusal does not say why it stopped:\n%s", err)
	}
}

func TestForceDestroysAnyway(t *testing.T) {
	// --force has to cover BOTH refusals. An operator whose control plane is half up and who cannot
	// read the **Lease** workflow is exactly the person who needs to clean up by hand, and a --force that only
	// covered the known-held case would leave them with no way to do it — which is how Machines get
	// leaked by a person rather than by a program.
	held := &leaseServer{leases: &leaseSet{
		Fleet:  testFqn,
		Leases: []leaseView{{Lease: "recon-7#a1", Holder: "recon-7", ExpiresAt: 1}},
	}}
	if err := refuseIfHeld(leaseAPI(t, held.start(t)), testFqn, true); err != nil {
		t.Fatalf("--force did not override a held Fleet: %v", err)
	}
	unreadable := &leaseServer{status: 502}
	if err := refuseIfHeld(leaseAPI(t, unreadable.start(t)), testFqn, true); err != nil {
		t.Fatalf("--force did not override an unreadable Lease workflow: %v", err)
	}
}

func TestLeasesPrintsBothKindsOfHolder(t *testing.T) {
	out := captureLeaseOutput(t, &leaseSet{
		Fleet: testFqn,
		Leases: []leaseView{
			{Lease: "#0f0f0f0f", Holder: "", ExpiresAt: time.Date(2026, 8, 30, 12, 30, 0, 0, time.UTC).UnixMilli()},
			{Lease: "recon-7#a1b2c3d4", Holder: "recon-7", ExpiresAt: time.Date(2026, 8, 30, 12, 10, 0, 0, time.UTC).UnixMilli()},
		},
	}, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))

	for _, want := range []string{"recon-7#a1b2c3d4", "recon-7", "30m0s", "10m0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Lease workflow does not show %q:\n%s", want, out)
		}
	}
	// THE UNATTRIBUTED CASE GETS WORDS, NOT A DASH. A Lease held by nobody is renewed by nothing and
	// its clock is the whole of its life; a blank cell reads identically to a rendering bug, which
	// is the failure `terminal.json` was written after.
	//
	// ASSERTED ON THAT LEASE'S OWN ROW, and that is not fussiness — it is the correction of a guard
	// that passed vacuously. `strings.Contains(out, "nobody")` over the WHOLE output was green with
	// the holder rendered as `-`, because the explanatory footer below the table contains the
	// sentence "survives a holder nobody can account for". Proved by breaking it: the mutation that
	// blanks the holder column left this test passing until the search was narrowed to the row.
	row := lineWith(t, out, "#0f0f0f0f")
	if !strings.Contains(row, "nobody") {
		t.Errorf("an unattributed Lease renders as a blank holder rather than as a state:\n  %s", row)
	}
}

// lineWith returns the single output line containing `needle`, and fails if there is not exactly
// one — a search that matched the footer, or nothing, is the failure this helper exists to expose.
func lineWith(t *testing.T, out, needle string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one line containing %q, found %d:\n%s", needle, len(found), out)
	}
	return found[0]
}

func TestAnOverdueLeaseSaysSoRatherThanReadingAsZero(t *testing.T) {
	// A Lease past its deadline that is STILL LISTED means the **Lease** workflow has not acted on it yet — a
	// control plane that is down, or a liveness check in flight. That is precisely the state an
	// operator is hunting when a Fleet will not die, so clamping it to `0s` would hide the one thing
	// worth seeing.
	out := captureLeaseOutput(t, &leaseSet{
		Fleet:  testFqn,
		Leases: []leaseView{{Lease: "recon-7#a1", Holder: "recon-7", ExpiresAt: time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC).UnixMilli()}},
	}, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(out, "overdue by 1h0m0s") {
		t.Errorf("an expired Lease does not read as overdue:\n%s", out)
	}
}

func TestAFleetNobodyHoldsSaysSoInWords(t *testing.T) {
	out := captureLeaseOutput(t, &leaseSet{Fleet: testFqn}, time.Now())
	if !strings.Contains(out, "no Leases") {
		t.Errorf("an empty Lease workflow prints an empty table rather than an answer:\n%s", out)
	}
	// And it says what that MEANS, because "no Leases" on its own reads as "safe" when it is the
	// opposite: nothing will ever collect a Fleet nobody holds.
	if !strings.Contains(out, "never collected") {
		t.Errorf("an empty Lease workflow does not say that nothing will collect this Fleet:\n%s", out)
	}
}

func TestADestroyedLedgerSaysTheMachinesAreGone(t *testing.T) {
	out := captureLeaseOutput(t, &leaseSet{Fleet: testFqn, Destroyed: true}, time.Now())
	if !strings.Contains(out, "destroyed") {
		t.Errorf("a Lease workflow that tore its Fleet down reads as one that was merely never held:\n%s", out)
	}
	// The two zero-Lease states must not render the same. One means "nothing will ever collect
	// this", the other means "it has already been collected", and an operator acts oppositely.
	if strings.Contains(out, "no Leases") {
		t.Errorf("a destroyed Lease workflow renders as an empty one:\n%s", out)
	}
}

func captureLeaseOutput(t *testing.T, led *leaseSet, now time.Time) string {
	t.Helper()
	var buf strings.Builder
	saved := cliio.Stdout
	cliio.Stdout = &buf
	t.Cleanup(func() { cliio.Stdout = saved })
	if err := printLeases(led, now); err != nil {
		t.Fatalf("printLeases: %v", err)
	}
	return buf.String()
}

func TestFleetDownRegistersForce(t *testing.T) {
	// The guard is reachable only if the flag exists on the shared set. A `--force` that parsed
	// nowhere would make every refusal above unanswerable from a terminal.
	f := fleetFlagSet("down")
	if f.fs.Lookup("force") == nil {
		t.Fatal("`kontra fleet down --force` is documented and the flag is not registered")
	}
}
