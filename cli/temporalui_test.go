package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra-local/cli/appliance"
)

// THE FLAG IS OFF AND OFF MEANS NOTHING HAPPENS: no artifact hydrated, no port bound, no process.
//
// This is the acceptance criterion "with the flag off, no UI port is bound and no UI artifact is
// hydrated", asked of the function `kontra up` actually calls. Asserting it about the ORDER of the
// lines in cmdUp would not be a test — it would be a reading of the source, and any later edit
// could quietly turn the default on for everybody who never asked.
func TestTemporalUIOffHydratesNothingAndBindsNothing(t *testing.T) {
	dir := t.TempDir()

	// A port that is free now and must still be free afterwards.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	ui, err := startTemporalUI(context.Background(), false, appliance.TemporalUIOptions{
		DataDir:         dir,
		TemporalAddress: "127.0.0.1:1",
		Port:            port,
	})
	if err != nil {
		t.Fatalf("the off case must not fail: %v", err)
	}
	if ui != nil {
		t.Fatal("`kontra up` without --temporal-ui started a UI")
	}

	// Nothing was fetched, expanded or placed.
	if _, err := os.Stat(filepath.Join(dir, "temporal-ui")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data directory has a temporal-ui entry after a run with the flag off (%v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the off case wrote %v into the data directory", names)
	}

	// And the port is still ours to take.
	again, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Errorf("port %d is bound after a run with the flag off: %v", port, err)
	} else {
		again.Close()
	}
}

// The flag surface. A missing or unreadable value must fail at PARSE — before a listener is bound
// and before nine megabytes are fetched — so a typo costs nothing.
func TestUpTakesTheTemporalUIFlags(t *testing.T) {
	if err := cmdUp([]string{"--temporal-ui-port", "not-a-number"}); err == nil ||
		!strings.Contains(err.Error(), "temporal-ui-port") {
		t.Errorf("`kontra up --temporal-ui-port` is not a flag: %v", err)
	}
	// A boolean flag still refuses a value that is not one, which is what stops
	// `--temporal-ui=no` from reading as "on".
	if err := cmdUp([]string{"--temporal-ui=maybe"}); err == nil ||
		!strings.Contains(err.Error(), "temporal-ui") {
		t.Errorf("`kontra up --temporal-ui=maybe` must be refused by name: %v", err)
	}
}

// THE ESCAPE HATCH HAS TO BE FINDABLE BY SOMEBODY WHO NEEDS IT, and the moment they need it is
// the moment a run finished `completed` with empty output. A flag nobody can find in `kontra help`
// is a flag that does not exist.
func TestHelpExplainsTheTemporalUI(t *testing.T) {
	for _, want := range []string{"--temporal-ui", "--temporal-ui-port 8233", "OFF by default"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("`kontra help` does not mention %q, so the UI cannot be found without reading the source", want)
		}
	}
}

// `kontra up --temporal-ui` PRINTS ITS ADDRESS, and prints the codec endpoint beside it.
//
// The second line is the one that earns its place: an operator looking at a page full of
// undecodable `$ref` payloads can compare it with the `Payload codec:` line above and see that
// they are the same string. In compose those were two independently-configured values whose
// disagreement was invisible.
func TestPrintTemporalUINamesTheAddressAndTheCodec(t *testing.T) {
	var out bytes.Buffer
	printTemporalUI(&out, fakeTemporalUI{
		url:   "http://localhost:8233",
		pid:   4242,
		codec: "http://127.0.0.1:18234",
		art: &appliance.HydratedTemporalUI{
			Version: "2.53.3",
			Digest:  "ec160cab0235f300444e66a68ca90599e21419c4294c6f11553fad4c60f634ad",
		},
	})
	got := out.String()
	for _, want := range []string{"http://localhost:8233", "4242", "2.53.3", "ec160cab0235", "http://127.0.0.1:18234"} {
		if !strings.Contains(got, want) {
			t.Errorf("the banner does not name %q:\n%s", want, got)
		}
	}
}

// A UI STARTED WITHOUT A CODEC SAYS SO, because the symptom otherwise is silent: every offloaded
// payload renders as a `$ref` the page cannot open and nothing explains why.
func TestPrintTemporalUINamesAMissingCodec(t *testing.T) {
	var out bytes.Buffer
	printTemporalUI(&out, fakeTemporalUI{
		url: "http://localhost:8233",
		art: &appliance.HydratedTemporalUI{Version: "2.53.3", Digest: "abc"},
	})
	if !strings.Contains(out.String(), "no codec") {
		t.Errorf("a UI with no codec must say so:\n%s", out.String())
	}
}

// THE FAKE IS ONLY HONEST IF THE REAL TYPE STILL FITS. Without this line the interface could
// drift away from *appliance.TemporalUI and the banner tests would keep passing against a shape
// nothing produces.
var _ temporalUIBanner = (*appliance.TemporalUI)(nil)

type fakeTemporalUI struct {
	url   string
	pid   int
	codec string
	art   *appliance.HydratedTemporalUI
}

func (f fakeTemporalUI) URL() string                             { return f.url }
func (f fakeTemporalUI) PID() int                                { return f.pid }
func (f fakeTemporalUI) CodecEndpoint() string                   { return f.codec }
func (f fakeTemporalUI) Artifact() *appliance.HydratedTemporalUI { return f.art }
