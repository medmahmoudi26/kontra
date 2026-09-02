// lease_conformance_test.go — THE GO ARM of shared/conformance/lease.json.
//
// This side is the **Lease** workflow wire's READER. `control/orchestrator/src/lease.ts` is the writer, and the two are
// joined by nothing but a matching set of string literals — no code generation, no shared schema, no
// import in either direction. `encoding/json` does not complain about a key it was not told about
// and does not complain about a field it was not given, so the whole failure mode is silent by
// construction: the struct decodes, `kontra fleet leases` prints, and one column is wrong.
//
// AND ONE COLUMN BEING WRONG IS NOT COSMETIC HERE. An empty `holder` is a REAL state — an
// unattributed **Lease**, one nothing can be asked about, whose clock is the whole of its life — so
// a `holder` key that drifted would render every **Lease** as adopted, on the one screen an operator
// reads when a **Fleet** will not die. `terminal.json` records this exact shape costing a blank
// column in `kontra panels` on every Fleet for months, with both languages green throughout.
package main

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

type leaseCorpus struct {
	Names struct {
		Cases []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
			Why   string `json:"why"`
		} `json:"cases"`
	} `json:"names"`
	LeaseID struct {
		Cases []struct {
			Why    string `json:"why"`
			Holder string `json:"holder"`
			Nonce  string `json:"nonce"`
			Lease  string `json:"lease"`
		} `json:"cases"`
	} `json:"lease_id"`
	LeaseSetWire struct {
		Keys struct {
			Envelope struct {
				Required []string `json:"required"`
				Optional []string `json:"optional"`
			} `json:"envelope"`
			Lease struct {
				Required []string `json:"required"`
				Optional []string `json:"optional"`
			} `json:"lease"`
		} `json:"keys"`
		Golden struct {
			Body json.RawMessage `json:"body"`
		} `json:"golden"`
	} `json:"lease_set_wire"`
}

func loadLeaseCorpus(t *testing.T) leaseCorpus {
	t.Helper()
	raw, err := os.ReadFile("../shared/conformance/lease.json")
	if err != nil {
		t.Fatalf("the corpus is the contract and it is unreadable: %v", err)
	}
	var c leaseCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("shared/conformance/lease.json does not parse: %v", err)
	}
	// THE GUARDS THAT KEEP THIS FILE FROM PASSING VACUOUSLY. Every assertion below is a loop, and a
	// loop over an empty slice is a test that reports success for having found nothing — which is
	// the shape `shared/conformance/README.md` names as the failure mode of half the guards it replaced.
	if len(c.LeaseSetWire.Keys.Envelope.Required) == 0 {
		t.Fatal("the corpus names no required Lease workflow keys — every containment check below is vacuous")
	}
	if len(c.LeaseSetWire.Keys.Lease.Required) == 0 {
		t.Fatal("the corpus names no required lease keys — every containment check below is vacuous")
	}
	if len(c.LeaseID.Cases) == 0 {
		t.Fatal("the corpus has no lease_id cases — the holder split below would be tested on nothing")
	}
	if len(c.LeaseSetWire.Golden.Body) == 0 {
		t.Fatal("the corpus carries no golden Lease workflow — the round-trip below would prove nothing")
	}
	return c
}

// jsonTagsOf reports the wire names a struct actually declares, read from the struct rather than
// from a list written beside it. A list would drift with the struct and agree with itself while
// agreeing with nothing else — which is the whole reason a corpus exists.
func jsonTagsOf(v any) []string {
	rt := reflect.TypeOf(v)
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		for j, r := range tag {
			if r == ',' {
				name = tag[:j]
				break
			}
		}
		if name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// TestTheReaderDeclaresNoKeyNobodySends is the containment rule, and it runs in one direction on
// purpose (`shared/conformance/lease.json` §lease_set_wire.keys.containment).
//
// A reader IGNORING a key is correct and common. A reader DECLARING a key nobody sends is the silent
// bug: `encoding/json` leaves it at the zero value on every response, and for `holder` the zero
// value is a state this **Lease** workflow genuinely has.
func TestTheReaderDeclaresNoKeyNobodySends(t *testing.T) {
	c := loadLeaseCorpus(t)

	sent := func(req, opt []string) map[string]bool {
		m := map[string]bool{}
		for _, k := range append(append([]string{}, req...), opt...) {
			m[k] = true
		}
		return m
	}

	leaseSetSent := sent(c.LeaseSetWire.Keys.Envelope.Required, c.LeaseSetWire.Keys.Envelope.Optional)
	got := jsonTagsOf(leaseSet{})
	if len(got) == 0 {
		t.Fatal("leaseSet declares no json tags at all — this test would pass on an empty struct")
	}
	for _, k := range got {
		if !leaseSetSent[k] {
			t.Errorf("leaseSet declares %q and nothing sends it: it will decode to the zero value on\n"+
				"every response, which is indistinguishable from a real absence. The keys the writer\n"+
				"sends are %v", k, c.LeaseSetWire.Keys.Envelope)
		}
	}

	leaseSent := sent(c.LeaseSetWire.Keys.Lease.Required, c.LeaseSetWire.Keys.Lease.Optional)
	gotLease := jsonTagsOf(leaseView{})
	if len(gotLease) == 0 {
		t.Fatal("leaseView declares no json tags at all — this test would pass on an empty struct")
	}
	for _, k := range gotLease {
		if !leaseSent[k] {
			t.Errorf("leaseView declares %q and nothing sends it (see the `holder` note in the corpus:\n"+
				"an empty holder is a REAL state here, so a drifted key makes every Lease read as adopted)", k)
		}
	}
}

// TestTheReaderReadsEveryRequiredKey is the other half. The containment above stops this reader
// inventing keys; this stops it QUIETLY DROPPING one. A required key nobody here decodes is a column
// that cannot be printed, and the corpus's whole purpose is that neither side can move alone.
func TestTheReaderReadsEveryRequiredKey(t *testing.T) {
	c := loadLeaseCorpus(t)
	have := map[string]bool{}
	for _, k := range jsonTagsOf(leaseSet{}) {
		have[k] = true
	}
	for _, k := range c.LeaseSetWire.Keys.Envelope.Required {
		if !have[k] {
			t.Errorf("the Lease workflow wire carries required key %q and leaseSet does not decode it", k)
		}
	}
	haveLease := map[string]bool{}
	for _, k := range jsonTagsOf(leaseView{}) {
		haveLease[k] = true
	}
	for _, k := range c.LeaseSetWire.Keys.Lease.Required {
		if !haveLease[k] {
			t.Errorf("a Lease carries required key %q and leaseView does not decode it", k)
		}
	}
}

// TestTheGoldenLeaseSetRoundTrips proves the SHAPE decodes and not only that the spellings match — a
// key set can agree while a type does not, and `expiresAt` is the one that would: milliseconds as a
// number, which an `int64` reads and a `time.Time` would refuse.
func TestTheGoldenLeaseSetRoundTrips(t *testing.T) {
	c := loadLeaseCorpus(t)
	var got leaseSet
	if err := json.Unmarshal(c.LeaseSetWire.Golden.Body, &got); err != nil {
		t.Fatalf("the golden Lease workflow does not decode into this reader's struct: %v", err)
	}
	if got.Fleet == "" {
		t.Fatal("the golden decoded with an empty fleet — the `fleet` key drifted")
	}
	if len(got.Leases) < 2 {
		t.Fatalf("the golden carries %d Lease(s); it is supposed to carry the SHARED case, which is\n"+
			"the one this whole slice exists for", len(got.Leases))
	}
	var unattributed, attributed int
	for _, l := range got.Leases {
		if l.Lease == "" {
			t.Errorf("a golden Lease decoded with an empty id — the `lease` key drifted")
		}
		if l.ExpiresAt == 0 {
			t.Errorf("Lease %q decoded with expiresAt 0 — the key drifted, or the type is not a\n"+
				"millisecond number", l.Lease)
		}
		if l.Holder == "" {
			unattributed++
		} else {
			attributed++
		}
	}
	// BOTH STATES, AND THAT IS THE POINT. A golden of only attributed Leases would pass with a
	// drifted `holder` key on the day somebody adopted a Fleet; a golden of only unattributed ones
	// would pass with a `holder` key that decodes nothing at all.
	if unattributed != 1 || attributed != 1 {
		t.Errorf("the golden must carry exactly one attributed and one unattributed Lease so a drifted\n"+
			"`holder` key cannot pass; got %d attributed, %d unattributed", attributed, unattributed)
	}
}

// TestTheHolderSplitMatchesTheCorpus drives the id grammar from this side.
//
// `leaseHolder` is the only place in Go that takes a Lease apart, and the input that breaks a naive
// implementation is in the corpus: a holder that itself contains the separator. Splitting on the
// FIRST one names a Run that does not exist, and nothing in the output says so.
func TestTheHolderSplitMatchesTheCorpus(t *testing.T) {
	c := loadLeaseCorpus(t)
	seen := 0
	sawEmbeddedSeparator := false
	for _, tc := range c.LeaseID.Cases {
		got := leaseHolder(tc.Lease)
		if got != tc.Holder {
			t.Errorf("leaseHolder(%q) = %q, corpus says %q\n  why this case exists: %s",
				tc.Lease, got, tc.Holder, tc.Why)
		}
		seen++
		if len(tc.Holder) > 0 && tc.Holder != tc.Lease {
			for _, r := range tc.Holder {
				if string(r) == leaseSeparator {
					sawEmbeddedSeparator = true
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no lease_id cases ran")
	}
	// THE CORPUS STILL CONTAINS ITS INTERESTING INPUT. A corpus that quietly lost the embedded-
	// separator row would leave this test passing on the easy cases only, which is exactly the
	// failure `shared/conformance/README.md` step 3 exists to prevent.
	if !sawEmbeddedSeparator {
		t.Fatal("the corpus no longer carries a holder containing the separator — without that row\n" +
			"a first-separator split passes every remaining case")
	}
}

// TestTheSeparatorIsTheCorpusSeparator pins the one character this file splits on against the
// corpus, so that a rename on the TypeScript or Python side fails HERE rather than producing a
// holder column full of whole lease ids.
func TestTheSeparatorIsTheCorpusSeparator(t *testing.T) {
	c := loadLeaseCorpus(t)
	found := false
	for _, n := range c.Names.Cases {
		if n.Name != "separator" {
			continue
		}
		found = true
		if leaseSeparator != n.Value {
			t.Errorf("leaseSeparator is %q and the corpus says %q — %s", leaseSeparator, n.Value, n.Why)
		}
	}
	if !found {
		t.Fatal("the corpus no longer names the separator; this test asserted nothing")
	}
}
