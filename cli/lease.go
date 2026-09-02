// lease.go — `kontra fleet leases`, and the question `kontra fleet down` now has to ask first.
//
// A **Lease** is one **Run**'s claim on a **Fleet** (ADR 0037). **Machines** are destroyed when the
// last one drops, which means two things an operator did not have to think about before:
//
//   - A **Fleet** may be held by somebody. `kontra fleet down` used to be unambiguous because
//     exactly one **Run** owned a **Fleet**; a **Fleet** several **Runs** hold is one where a hand
//     on `down` is taking other people's **Machines**. So `down` reads the **Lease** workflow first and refuses
//     to destroy a held **Fleet** unless the operator says so.
//   - A **Fleet** may be held by NOBODY and still be standing, on a clock. `leases` is where the
//     deadline is visible, which is the difference between "this will collect itself in forty
//     minutes" and "this is leaked".
//
// THIS IS AN HTTP CLIENT, like the rest of `kontra fleet`. The **Lease** workflow is a workflow on the
// Controller and this binary never dials Temporal for it: one binary addresses either control plane
// (ADR 0034 §1), and the route is `GET /api/infra/stacks/<fqn>/leases`.
//
// THE WIRE IS PINNED BY A CORPUS. `shared/conformance/lease.json` §lease_set_wire holds the key set on both
// sides, because this is `terminal.json`'s contract in a second place and that one cost a blank
// column in `kontra panels` on every Fleet for months, with both languages green throughout. The
// asymmetry there applies here: the writer's keys are pinned exactly, this reader's must be a
// SUBSET, and what a reader may never do is declare a key nobody sends — `encoding/json` leaves it
// at the zero value on every response, and an empty `holder` is a REAL state here (an unattributed
// **Lease**), so a misspelled `holder` would make every **Lease** look adopted.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"
)

// leaseView is one entry in the **Lease** workflow. Tags are `shared/conformance/lease.json` §lease_set_wire.lease.
type leaseView struct {
	// Lease is the full id — `<holder>#<nonce>` — exactly as it was held and exactly as it must be
	// dropped. Printed verbatim: it is the only handle anything has on a single claim.
	Lease string `json:"lease"`
	// Holder is the Run's Temporal workflow id, or EMPTY for an unattributed claim. Empty is a
	// value here, not an absence: nothing can be asked about such a Lease, so its deadline is final.
	Holder string `json:"holder"`
	// ExpiresAt is milliseconds since the epoch. Milliseconds, not seconds and not an ISO string —
	// see the corpus note; a reader that converts is inventing precision it was not given.
	ExpiresAt int64 `json:"expiresAt"`
}

// leaseSet is what the route answers.
type leaseSet struct {
	Fleet  string      `json:"fleet"`
	Leases []leaseView `json:"leases"`
	// Destroyed is set only once the **Lease** workflow has torn the Fleet down, which it only reaches at zero
	// Leases. Absent while the Fleet stands.
	Destroyed bool `json:"destroyed"`
}

// leaseSeparator divides a holder from its nonce. `control/orchestrator/src/lease.ts` and `actorkit.fleet`
// declare the same character; `shared/conformance/lease.json` §names is what keeps the three equal.
const leaseSeparator = "#"

// leaseHolder is the holder half of a Lease id, for a reader that has the id and not the **Lease** workflow.
//
// SPLIT ON THE LAST SEPARATOR, NOT THE FIRST — a Temporal workflow id may legally contain a `#` and
// a nonce never does, so the last one is the only one that is structurally the separator. The
// corpus carries `weird#run#deadbeef` for exactly this, because a first-separator split names a Run
// that does not exist and there is no way to tell from the output that it did.
func leaseHolder(lease string) string {
	if at := strings.LastIndex(lease, leaseSeparator); at >= 0 {
		return lease[:at]
	}
	return lease
}

// readLeases asks who is holding one Fleet.
//
// A FLEET WITH NO LEDGER ANSWERS AN EMPTY LIST, not a 404 — the server decides that, and this
// records why the distinction is load-bearing here: `fleetDown` branches on it, and "nobody holds
// it" and "I could not find out" must not be the same value to something about to delete Machines.
func readLeases(api *apiClient, fqn string) (*leaseSet, error) {
	var out leaseSet
	if err := api.getJSON("/api/infra/stacks/"+url.PathEscape(fqn)+"/leases", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// fleetLeases prints the **Lease** workflow for one Fleet.
func fleetLeases(args []string) error {
	f := fleetFlagSet("leases")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	if _, err := f.resolvedName(); err != nil {
		return err
	}
	api, err := infraAPI(*f.api)
	if err != nil {
		return err
	}
	led, err := readLeases(api, f.fqn())
	if err != nil {
		return err
	}
	return printLeases(led, time.Now())
}

// printLeases renders the **Lease** workflow. `now` is a parameter so the remaining-time column is testable
// without a clock — the same reason every other rendering in this CLI takes one.
func printLeases(led *leaseSet, now time.Time) error {
	if led.Destroyed {
		// A **Lease** workflow only reaches this at zero Leases, so it is also the answer to "was this Fleet
		// destroyed by the last one out, or by a hand?".
		fmt.Fprintf(stdout, "%s: destroyed — the last Lease dropped and its Machines are gone\n", led.Fleet)
		return nil
	}
	if len(led.Leases) == 0 {
		// NOT AN ERROR AND NOT A BLANK. A Fleet nobody holds is either one an operator brought up
		// by hand — `kontra fleet up` takes no Lease, deliberately, because it is not a Run — or one
		// that has already been collected. Both are worth saying in words rather than as an empty
		// table somebody has to interpret.
		fmt.Fprintf(stdout, "%s: no Leases — nothing is holding this Fleet\n", led.Fleet)
		fmt.Fprintf(stdout, "  (a Fleet with no Lease is never collected on its own: `kontra fleet down` ends it)\n")
		return nil
	}
	fmt.Fprintf(stdout, "%s: %d Lease(s)\n", led.Fleet, len(led.Leases))
	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "LEASE\tHOLDER\tEXPIRES IN")
	for _, l := range led.Leases {
		holder := l.Holder
		if holder == "" {
			// The empty holder is a state, so it gets a word rather than a dash: this Lease is
			// renewed by nothing and its clock is the whole of its life.
			holder = "(nobody — expires on its clock)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", l.Lease, holder, until(l.ExpiresAt, now))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nMachines are destroyed when the LAST Lease drops. A Lease whose holder is\n"+
		"still running is renewed at every deadline, so `EXPIRES IN` is how long this Fleet\n"+
		"survives a holder nobody can account for — not a limit on how long a Run may take.\n")
	return nil
}

// until renders a deadline as a duration, rounded to the second.
//
// A DEADLINE IN THE PAST IS PRINTED AS SUCH rather than clamped to zero: a Lease whose expiry has
// passed and which is still listed means the **Lease** workflow has not yet acted on it — a control plane that
// is down, or a check in flight — and that is precisely the state an operator is looking for when a
// Fleet will not die.
func until(expiresAtMs int64, now time.Time) string {
	if expiresAtMs <= 0 {
		return "-"
	}
	d := time.UnixMilli(expiresAtMs).Sub(now).Round(time.Second)
	if d < 0 {
		return fmt.Sprintf("overdue by %s", (-d).String())
	}
	return d.String()
}

// heldBy is `fleet down`'s question, and it fails CLOSED.
//
// A LEDGER THAT CANNOT BE READ IS NOT AN EMPTY LEDGER. The whole point of asking is that destroying
// a held Fleet takes another Run's Machines, and an error here means the answer is unknown — which
// must stop the destroy and say so, not fall through to "nobody holds it". That is the same rule
// `queuePollers` states from the other side: an answer nobody could get is not a measurement.
func heldBy(api *apiClient, fqn string) ([]leaseView, error) {
	led, err := readLeases(api, fqn)
	if err != nil {
		return nil, fmt.Errorf("could not read the Leases on %s, so it is not safe to destroy it: %w", fqn, err)
	}
	return led.Leases, nil
}

// refuseIfHeld is the guard `fleetDown` runs before it starts anything.
func refuseIfHeld(api *apiClient, fqn string, force bool) error {
	held, err := heldBy(api, fqn)
	if err != nil {
		if force {
			// --force means "destroy it anyway", and that has to include "even though I could not
			// find out who holds it" — otherwise an operator whose control plane is half up cannot
			// clean up at all, which is the situation that produces leaked Machines by hand.
			fmt.Fprintf(stdout, "warning: %v\n", err)
			return nil
		}
		return err
	}
	if len(held) == 0 {
		return nil
	}
	if force {
		fmt.Fprintf(stdout, "warning: %d Lease(s) still hold %s — destroying anyway (--force)\n", len(held), fqn)
		for _, l := range held {
			fmt.Fprintf(stdout, "  %s\n", l.Lease)
		}
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d Lease(s) still hold %s, and destroying it would take their Machines:\n", len(held), fqn)
	for _, l := range held {
		who := leaseHolder(l.Lease)
		if l.Holder != "" {
			who = l.Holder
		}
		if who == "" {
			who = "nobody (expires on its clock)"
		}
		fmt.Fprintf(&b, "  %s  held by %s\n", l.Lease, who)
	}
	b.WriteString("\nA Fleet is capacity now, not one Run's provisioning: its Machines are destroyed when\n" +
		"the LAST Lease drops, and every Lease expires on a clock, so an abandoned one collects\n" +
		"itself. Wait for them, or `kontra fleet down --force` to destroy it out from under them.")
	return errors.New(b.String())
}
