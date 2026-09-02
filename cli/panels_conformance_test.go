// panels_conformance_test.go — THE GO ARM of conformance/terminal.json.
//
// This side is the wire's READER. `backend/src/panels/types.ts` is the writer, and the two are
// joined by nothing but a matching set of string literals: no code generation, no shared schema, no
// import in either direction. `encoding/json` does not complain about a key it was not told about
// and does not complain about a field it was not given, so the whole failure mode is silent by
// construction — the struct decodes, the command prints, and one column is blank.
//
// TWO LIVE DRIFTS WERE FOUND BY WRITING THIS, both of them years-shaped rather than days-shaped:
// `campaign` had been renamed to `fleet` on the TypeScript side and never here, and `role` was
// declared here and never sent by anyone. Both passed every test in both languages, because each
// side's tests were built from that side's own declaration. That is the closed loop ADR 0035 rule
// two exists to break: the corpus is a THIRD file that neither side owns, so a rename in either one
// fails a test in the other language.
package main

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

type terminalCorpus struct {
	Keys struct {
		Required []string `json:"required"`
		// Sent when there is something to send. `lastSnapshotAt` is absent until a Machine has been
		// probed once and `telemetry` until a Warden has reported — so a reader that declares them
		// is correct, and one that requires them would be wrong. They count as SENT for the
		// containment below; the distinction they carry is about presence, not about spelling.
		Optional []string `json:"optional"`
	} `json:"keys"`
	Goldens struct {
		Terminal map[string]any `json:"terminal"`
	} `json:"goldens"`
}

func loadTerminalCorpus(t *testing.T) terminalCorpus {
	t.Helper()
	raw, err := os.ReadFile("../conformance/terminal.json")
	if err != nil {
		t.Fatalf("the corpus is the contract and it is unreadable: %v", err)
	}
	var c terminalCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("conformance/terminal.json does not parse: %v", err)
	}
	// THE GUARD THAT KEEPS THIS FILE FROM PASSING VACUOUSLY. A corpus that parsed to an empty
	// `required` would make every assertion below a loop over nothing, and this file would go green
	// on a Terminal with no keys at all.
	if len(c.Keys.Required) == 0 {
		t.Fatal("the corpus names no required keys — every assertion in this file would be vacuous")
	}
	if len(c.Keys.Optional) == 0 {
		t.Fatal("the corpus names no optional keys — `optional` was dropped or renamed, and every\n" +
			"key it held would now read as one nobody sends")
	}
	if len(c.Goldens.Terminal) == 0 {
		t.Fatal("the corpus carries no golden Terminal — the round-trip below would prove nothing")
	}
	return c
}

// jsonTags reports the wire names this package's `terminal` struct actually declares, read from the
// struct rather than from a list written beside it — a list would drift with the struct and agree
// with itself while agreeing with nothing else.
func jsonTags(t *testing.T) []string {
	t.Helper()
	rt := reflect.TypeOf(terminal{})
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		if comma := len(name); comma > 0 {
			for j, r := range name {
				if r == ',' {
					name = name[:j]
					break
				}
			}
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// EVERY KEY THIS READER DECLARES IS A KEY THE WRITER SENDS. The containment runs one way and only
// one way, and getting that direction right is the whole test.
//
// A reader is ALLOWED to ignore keys. The streamer sends `command`, `poller`, `session`,
// `telemetry` and more that this command has no column for, and adding a Go field for each so the
// two sets matched would be ceremony that catches nothing — an ignored key is ignored correctly.
//
// A reader is NOT allowed to declare a key nobody sends, and that is not symmetry-for-its-own-sake:
// such a field decodes to the zero value on every response forever, and `dashIfEmpty` draws it the
// same way it draws a Machine that genuinely has no Fleet. Both drifts this corpus was written
// after were of exactly that kind — `campaign` after the writer renamed it to `fleet`, and `role`,
// which no writer has ever sent.
func TestThisReaderDeclaresNoKeyTheStreamerDoesNotSend(t *testing.T) {
	c := loadTerminalCorpus(t)
	sent := map[string]bool{}
	for _, k := range append(append([]string(nil), c.Keys.Required...), c.Keys.Optional...) {
		sent[k] = true
	}
	got := jsonTags(t)

	var phantom []string
	for _, k := range got {
		if !sent[k] {
			phantom = append(phantom, k)
		}
	}
	if len(phantom) > 0 {
		t.Errorf("this struct decodes %v, which conformance/terminal.json says nobody sends.\n"+
			"  A key no writer emits is not a harmless extra field: it decodes to the zero value on\n"+
			"  every response, and a blank cell reads as data. `campaign` sat here in exactly this\n"+
			"  state after backend/src/panels/types.ts renamed it to `fleet`, and every Terminal on\n"+
			"  every Fleet printed `-` in that column with no test red on either side.\n"+
			"  If the streamer really did gain this key, add it to the corpus and to\n"+
			"  backend/src/panels/types.ts in this same commit.", phantom)
	}
	// AND THE ONE KEY THIS COMMAND ACTUALLY PRINTS IS PRESENT. The containment above is satisfied
	// vacuously by a struct that declares nothing at all, which would also print an empty table.
	declared := map[string]bool{}
	for _, k := range got {
		declared[k] = true
	}
	for _, must := range []string{"fleet", "machine", "actor", "version", "window"} {
		if !declared[must] {
			t.Errorf("this struct does not decode %q, which `kontra panels` prints", must)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Fatal("jsonTags did not sort; the diff above cannot be trusted")
	}
}

// A KEY SET CAN AGREE WHILE A TYPE DOES NOT, so the golden is decoded rather than only compared by
// name. `lastSnapshotAt` is the field this catches: epoch milliseconds into an int64, where a
// seconds/millis confusion is right about every spelling and wrong about every age on the wall.
func TestTheGoldenTerminalDecodes(t *testing.T) {
	c := loadTerminalCorpus(t)
	raw, err := json.Marshal(c.Goldens.Terminal)
	if err != nil {
		t.Fatalf("re-marshalling the golden: %v", err)
	}
	var got terminal
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the golden Terminal does not decode into this package's struct: %v", err)
	}
	for _, tc := range []struct{ what, got, want string }{
		{"id", got.ID, "fleet:kf-dns-01:nscheck-0.1.0:actor"},
		{"machine", got.Machine, "kf-dns-01"},
		{"fleet", got.Fleet, "nscheck-0.1.0"},
		{"actor", got.Actor, "nscheck"},
		{"version", got.Version, "0.1.0"},
		{"window", got.Window, "actor"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s decoded to %q, want %q", tc.what, tc.got, tc.want)
		}
	}
	if got.LastSnapshotAt != 1756555200000 {
		t.Errorf("lastSnapshotAt decoded to %d, want 1756555200000 — epoch MILLISECONDS. A value "+
			"1000x off is a snapshot age of half a century on a tile that is two seconds old",
			got.LastSnapshotAt)
	}
	// THE COLUMN THAT WAS BLANK. `fleet` is what `kontra panels` prints in its second cell, and for
	// as long as this struct asked for `campaign` it printed `-` for every Terminal on every Fleet.
	if got.Fleet == "" {
		t.Error("fleet decoded empty from a golden that carries it — this is the exact bug the " +
			"corpus was written for, back again")
	}
}
