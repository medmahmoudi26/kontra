package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra-local/cli/appliance/registry"
)

// TestRegistryAddressPrefersTheRunningAppliance pins the resolution order deploy and scale share.
// The whole point of having one function is that both halves of a round trip get the same string,
// and the third rung — the address `kontra up` actually bound — is what makes a moved port not
// silently orphan every deploy.
func TestRegistryAddressPrefersTheRunningAppliance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KONTRA_HOME", home)
	t.Setenv("KONTRA_REGISTRY", "")

	// Nothing running, nothing configured.
	if got := registryAddress(""); got != defaultRegistry {
		t.Errorf("with nothing running: got %q, want %q", got, defaultRegistry)
	}

	// An appliance publishes the address it bound.
	regDir := filepath.Join(home, "data", "registry")
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "address"), []byte("127.0.0.1:51234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := registryAddress(""); got != "127.0.0.1:51234" {
		t.Errorf("with an appliance running: got %q, want the bound address", got)
	}

	// The environment overrides it, and the flag overrides that.
	t.Setenv("KONTRA_REGISTRY", "controller.internal:5000")
	if got := registryAddress(""); got != "controller.internal:5000" {
		t.Errorf("KONTRA_REGISTRY should win over the appliance: got %q", got)
	}
	if got := registryAddress("box:5001"); got != "box:5001" {
		t.Errorf("--registry should win over everything: got %q", got)
	}
}

// fakeRegistry answers HEAD /v2/<name>/manifests/<ref> with whatever the test wants.
func fakeRegistry(t *testing.T, status int, digest string) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if digest != "" {
			w.Header().Set("Docker-Content-Digest", digest)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "http://")
}

// TestConfirmPushedNamesTheAddress is the acceptance criterion "a mismatch fails naming the
// address". Every branch here is a different way for push and pull to disagree, and each one has
// to say WHERE rather than leaving `no such image` to be discovered at scale time.
func TestConfirmPushedNamesTheAddress(t *testing.T) {
	const pushed = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const other = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	t.Run("agreement returns the digest", func(t *testing.T) {
		addr := fakeRegistry(t, http.StatusOK, pushed)
		got, err := confirmPushed(addr, "beacon", "0.2.0", addr+"/beacon:0.2.0", pushed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != pushed {
			t.Errorf("digest = %q, want %q", got, pushed)
		}
	})

	t.Run("the registry does not hold it", func(t *testing.T) {
		addr := fakeRegistry(t, http.StatusNotFound, "")
		_, err := confirmPushed(addr, "beacon", "0.2.0", addr+"/beacon:0.2.0", pushed)
		if err == nil {
			t.Fatal("a push the registry cannot resolve was accepted")
		}
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("the error does not name the address %s: %v", addr, err)
		}
		// It must also say what the operator would otherwise be chasing.
		if !strings.Contains(err.Error(), "no such image") {
			t.Errorf("the error does not connect itself to the symptom it prevents: %v", err)
		}
	})

	t.Run("a different registry answers", func(t *testing.T) {
		addr := fakeRegistry(t, http.StatusOK, other)
		_, err := confirmPushed(addr, "beacon", "0.2.0", addr+"/beacon:0.2.0", pushed)
		if err == nil {
			t.Fatal("a digest disagreement was accepted")
		}
		for _, want := range []string{addr, pushed, other} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %s: %v", want, err)
			}
		}
	})

	t.Run("nothing answers", func(t *testing.T) {
		_, err := confirmPushed("127.0.0.1:1", "beacon", "0.2.0", "127.0.0.1:1/beacon:0.2.0", pushed)
		if err == nil {
			t.Fatal("an unreachable registry was accepted after a push")
		}
		if !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Errorf("the error does not name the address: %v", err)
		}
	})

	t.Run("a registry with no digest header still confirms the address", func(t *testing.T) {
		addr := fakeRegistry(t, http.StatusOK, "")
		got, err := confirmPushed(addr, "beacon", "0.2.0", addr+"/beacon:0.2.0", pushed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != pushed {
			t.Errorf("digest = %q, want the daemon's %q", got, pushed)
		}
	})
}

// TestPullFailureNamesTheAddress is the same guarantee on the pull side. Docker reports all three
// of these as some flavour of `no such image`; each one has to come out naming the registry, and
// the third has to say the thing only the registry can tell us — that it HAS the image and the
// daemon still could not fetch it, which means the two are resolving one string differently.
func TestPullFailureNamesTheAddress(t *testing.T) {
	docker := errors.New("Error response from daemon: manifest unknown")

	t.Run("the registry does not hold it", func(t *testing.T) {
		addr := fakeRegistry(t, http.StatusNotFound, "")
		err := pullFailure(addr, "beacon", "0.2.0", addr+"/beacon:0.2.0", docker)
		for _, want := range []string{addr, "beacon", "0.2.0", "kontra deploy", docker.Error()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
	})

	t.Run("nothing answers there", func(t *testing.T) {
		err := pullFailure("127.0.0.1:1", "beacon", "0.2.0", "127.0.0.1:1/beacon:0.2.0", docker)
		if !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Errorf("the error does not name the address: %v", err)
		}
		if !strings.Contains(err.Error(), "container create") {
			t.Errorf("the error does not say why a missing image is fatal here: %v", err)
		}
	})

	t.Run("the registry has it and the daemon still could not", func(t *testing.T) {
		addr := fakeRegistry(t, http.StatusOK, "sha256:abc")
		err := pullFailure(addr, "beacon", "0.2.0", addr+"/beacon:0.2.0", docker)
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("the error does not name the address: %v", err)
		}
		// The one diagnosis a caller could not have made without asking the registry.
		if !strings.Contains(err.Error(), "resolves") {
			t.Errorf("the error does not say the two sides disagree about the address: %v", err)
		}
	})
}

// TestStreamPushOutputReadsTheDigest pins where identity comes from: the engine's push stream, in
// the trailing `aux` message. A build stream has no such field, and an image that was built but
// never pushed therefore has no digest — which is the point (ADR 0032).
func TestStreamPushOutputReadsTheDigest(t *testing.T) {
	const stream = `{"status":"The push refers to repository [localhost:5000/beacon]"}
{"status":"Preparing","progressDetail":{},"id":"abc123"}
{"status":"Pushed","progressDetail":{},"id":"abc123"}
{"status":"0.2.0: digest: sha256:deadbeef size: 1234"}
{"progressDetail":{},"aux":{"Tag":"0.2.0","Digest":"sha256:deadbeef","Size":1234}}
`
	var out strings.Builder
	got, err := streamPushOutput(&out, strings.NewReader(stream))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sha256:deadbeef" {
		t.Errorf("digest = %q, want sha256:deadbeef", got)
	}
	if !strings.Contains(out.String(), "Pushed") {
		t.Errorf("the progress was swallowed: %q", out.String())
	}

	// A stream carrying no aux is not an error — confirmPushed asks the registry instead.
	got, err = streamPushOutput(&out, strings.NewReader(`{"status":"Pushed"}`+"\n"))
	if err != nil || got != "" {
		t.Errorf("no-aux stream: digest=%q err=%v, want empty and nil", got, err)
	}

	// An in-stream error still wins over anything already parsed.
	if _, err := streamPushOutput(&out, strings.NewReader(`{"error":"denied"}`+"\n")); err == nil {
		t.Error("an in-stream error was swallowed")
	}
}

// TestAgainstTheEmbeddedRegistry ties the two halves together against the real thing: the
// appliance publishes an address, `registryAddress` finds it with nothing configured, and the
// digest the registry reports for a tag is what `confirmPushed` records. Every other test here
// uses a stand-in; this one uses the registry that will actually be serving.
func TestAgainstTheEmbeddedRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KONTRA_HOME", home)
	t.Setenv("KONTRA_REGISTRY", "")

	// A FREE port, not `Port: 0` — that means the default 5000, and a suite run on a controller
	// that is already serving one would talk to the wrong registry and report it as a pass.
	srv, err := registry.Start(registry.Options{
		DataDir: filepath.Join(home, "data"), Port: freeTestPort(t), Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("registry.Start: %v", err)
	}
	defer srv.Stop()

	// THE ADDRESS IS DISCOVERED, not assumed: Port 0 means it is not 5000, so a resolution that
	// fell back to defaultRegistry would be caught here rather than at scale time.
	if got := registryAddress(""); got != srv.Address() {
		t.Fatalf("registryAddress() = %q, want the running appliance's %q", got, srv.Address())
	}
	reg := registryAddress("")

	if err := registryReachable(reg); err != nil {
		t.Fatalf("the embedded registry does not answer /v2/: %v", err)
	}

	// Push a minimal image the way the daemon does, and keep the digest the registry reported.
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	cd := sha256Digest(config)
	post := func(path string, body []byte, hdr map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://"+reg+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if resp := post("/v2/beacon/blobs/uploads/?digest="+cd, config, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("config upload: %s", resp.Status)
	}
	man := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + cd +
		`","size":` + fmt.Sprint(len(config)) + `},"layers":[]}`)
	req, err := http.NewRequest(http.MethodPut, "http://"+reg+"/v2/beacon/manifests/0.2.0", bytes.NewReader(man))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("manifest push: %s", resp.Status)
	}
	pushed := resp.Header.Get("Docker-Content-Digest")

	// `deploy`'s reader agrees with the registry's writer.
	got, err := confirmPushed(reg, "beacon", "0.2.0", reg+"/beacon:0.2.0", pushed)
	if err != nil {
		t.Fatalf("confirmPushed against the embedded registry: %v", err)
	}
	if got != pushed {
		t.Errorf("recorded digest %q, registry reported %q", got, pushed)
	}
	if got != sha256Digest(man) {
		t.Errorf("the recorded digest is not the hash of the manifest bytes: %s vs %s", got, sha256Digest(man))
	}

	// `scale`'s diagnosis reads the same registry: a version nobody pushed comes back as a
	// sentence about the registry rather than as Docker's "no such image".
	failed := pullFailure(reg, "beacon", "9.9.9", reg+"/beacon:9.9.9", errors.New("manifest unknown"))
	for _, want := range []string{reg, "9.9.9", "kontra deploy"} {
		if !strings.Contains(failed.Error(), want) {
			t.Errorf("the pull diagnosis does not name %q: %v", want, failed)
		}
	}

	// The version guard reads the same registry.
	if !versionDeployed(reg, "beacon", "0.2.0") {
		t.Error("versionDeployed did not see the tag it just pushed")
	}

	// And when the appliance stops, the published address goes with it, so the next deploy falls
	// back to the conventional one rather than to a dead port.
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := registryAddress(""); got != defaultRegistry {
		t.Errorf("after the appliance stopped: registryAddress() = %q, want %q", got, defaultRegistry)
	}
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// freeTestPort picks a port nothing is on. The appliance package has its own; this is the one line
// of it that matters on the far side of the package boundary.
func freeTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("no free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
