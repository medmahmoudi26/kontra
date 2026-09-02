package appliance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEndpointsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Endpoints{
		Bind:     "172.17.0.1",
		Temporal: "172.17.0.1:39101",
		S3:       "http://172.17.0.1:39102",
		KV:       "172.17.0.1:39103",
		Codec:    "http://172.17.0.1:39104",
		Registry: "172.17.0.1:39105",
		PID:      4242,
	}
	if err := WriteEndpoints(dir, want); err != nil {
		t.Fatalf("WriteEndpoints: %v", err)
	}
	got, ok := ReadEndpoints(dir)
	if !ok {
		t.Fatal("ReadEndpoints did not find the record it was just handed")
	}
	if got != want {
		t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", got, want)
	}

	// THE SECOND WRITE SUPERSEDES, whole. `kontra up` writes the five bound services and then
	// writes again once the orchestrator child answers, and a reader must never see a record with
	// six fields half-filled — which is what a mutate-in-place would allow.
	want.API = "http://172.17.0.1:39106"
	if err := WriteEndpoints(dir, want); err != nil {
		t.Fatalf("second WriteEndpoints: %v", err)
	}
	got, _ = ReadEndpoints(dir)
	if got.API != want.API || got.Temporal != want.Temporal {
		t.Fatalf("the second write lost a field: %+v", got)
	}

	if err := RemoveEndpoints(dir); err != nil {
		t.Fatalf("RemoveEndpoints: %v", err)
	}
	if _, ok := ReadEndpoints(dir); ok {
		t.Fatal("the record survived RemoveEndpoints; the next deploy would resolve a dead address")
	}
	// Twice is fine: a stop after a crash that already lost the file is not an error.
	if err := RemoveEndpoints(dir); err != nil {
		t.Fatalf("second RemoveEndpoints must be a no-op, got %v", err)
	}
}

// A file that is not an endpoints record must read as ABSENT rather than as an appliance with
// empty addresses. The difference matters: `false` sends a caller to its own defaults, while a
// record full of empty strings would have it dial "".
func TestReadEndpointsRejectsWhatIsNotOne(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", "this is not json at all"},
		{"json without a temporal address", `{"s3":"http://x:1"}`},
		{"empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, endpointsFileName), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if e, ok := ReadEndpoints(dir); ok {
				t.Fatalf("read %q as an appliance: %+v", tc.body, e)
			}
		})
	}
	if _, ok := ReadEndpoints(t.TempDir()); ok {
		t.Fatal("an empty directory read as a running appliance")
	}
	if _, ok := ReadEndpoints(""); ok {
		t.Fatal("an empty data directory read as a running appliance")
	}
}

// THE REACHABILITY RULE, which exists because getting it wrong is silent. A worker container
// handed a loopback address starts, retries forever, and shows in `docker ps` as a live replica.
func TestReachableFromContainer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		e       Endpoints
		refused bool
	}{
		{"the bridge gateway is reachable", Endpoints{Bind: "172.17.0.1", Temporal: "172.17.0.1:7233"}, false},
		{"a VPC address is reachable", Endpoints{Bind: "10.124.0.2", Temporal: "10.124.0.2:7233"}, false},
		{"0.0.0.0 is reachable", Endpoints{Bind: "0.0.0.0", Temporal: "0.0.0.0:7233"}, false},
		{"a name is not ours to second-guess", Endpoints{Bind: "host.docker.internal", Temporal: "host.docker.internal:7233"}, false},
		{"127.0.0.1 is refused", Endpoints{Bind: "127.0.0.1", Temporal: "127.0.0.1:7233"}, true},
		{"any 127/8 address is refused", Endpoints{Bind: "127.0.0.53", Temporal: "127.0.0.53:7233"}, true},
		{"::1 is refused", Endpoints{Bind: "::1", Temporal: "[::1]:7233"}, true},
		// A record written before Bind existed still has to answer, from the address it does have.
		{"no bind field falls back to the temporal host", Endpoints{Temporal: "127.0.0.1:7233"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.e.ReachableFromContainer()
			if tc.refused && err == nil {
				t.Fatalf("%+v must be refused: a container cannot reach it", tc.e)
			}
			if !tc.refused && err != nil {
				t.Fatalf("%+v must be accepted, got %v", tc.e, err)
			}
			if !tc.refused {
				return
			}
			// THE REFUSAL HAS TO SAY WHAT TO DO. An error that names the problem and not the fix
			// is how an operator concludes the appliance is broken.
			msg := strings.ToLower(err.Error())
			for _, want := range []string{"kontra up --bind", DefaultBridgeBind, "container"} {
				if !strings.Contains(msg, strings.ToLower(want)) {
					t.Fatalf("the refusal does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}
