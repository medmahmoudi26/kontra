package warden

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapLocalIdentityScopesTheNamespace(t *testing.T) {
	dir := t.TempDir()
	id, err := bootstrapLocalIdentity(dir, "acme", "http://orchestrator-api:8088")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id.Record.WardenID, wardenIDPrefix) {
		t.Fatalf("warden id %q missing prefix %s", id.Record.WardenID, wardenIDPrefix)
	}
	pemBytes, err := os.ReadFile(filepath.Join(wardenIdentityDir(dir), "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("cert.pem is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.URIs) == 0 {
		t.Fatal("local identity cert has no URI SAN")
	}
	got := cert.URIs[0].String()
	want := "kontra:///ns/acme/warden/" + id.Record.WardenID
	if got != want {
		t.Fatalf("URI SAN = %q, want %q", got, want)
	}
	again, err := bootstrapLocalIdentity(dir, "other", "http://x")
	if err != nil {
		t.Fatal(err)
	}
	if again.Record.WardenID != id.Record.WardenID {
		t.Fatal("second --local must reuse the identity already on disk")
	}
}

func TestDockerRunArgsNeverMountTheRuntimeSocket(t *testing.T) {
	d := &dockerDriver{bin: "docker", net: "kontra"}
	spec := Spec{
		Name:    "hello",
		Version: "0.1.0",
		Image:   "registry:5000/hello@sha256:" + strings.Repeat("a", 64),
		Actor:   ProcSpec{Argv: []string{"python3", "/actor/hello/actor.py"}, Env: []string{"KONTRA_ADDRESS=temporal:7233"}},
		Handler: ProcSpec{Argv: []string{"/handler"}, Env: []string{"KONTRA_ADDRESS=temporal:7233"}},
	}
	args, err := d.runArgs("kontra-hello-0.1.0", spec, partActor, spec.Actor, spec.Image)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, sock := range []string{"/var/run/docker.sock", "/run/podman/podman.sock", "/var/run/docker.sock"} {
		if strings.Contains(joined, sock) {
			t.Fatalf("worker argv exposes the runtime socket: %s", joined)
		}
	}
	if strings.Contains(joined, "--network=host") || strings.Contains(joined, "--net=host") {
		t.Fatalf("worker argv uses host network: %s", joined)
	}
	onCluster, err := d.runArgs(d.net, spec, partActor, spec.Actor, spec.Image)
	if err != nil {
		t.Fatal(err)
	}
	if !containsNetwork(onCluster, "kontra") {
		t.Fatalf("worker must join the Compose network, got %s", strings.Join(onCluster, " "))
	}
}

func TestDockerEmptyArgvRunsTheImageOnce(t *testing.T) {
	d := &dockerDriver{bin: "docker", net: "kontra"}
	spec := Spec{
		Name:    "hello",
		Version: "0.1.0",
		Image:   "registry:5000/hello@sha256:" + strings.Repeat("a", 64),
		Actor:   ProcSpec{Env: []string{"KONTRA_ADDRESS=temporal:7233"}},
		Handler: ProcSpec{Env: []string{"KONTRA_ADDRESS=temporal:7233"}},
	}
	if !dockerImageEntrypoint(spec) {
		t.Fatal("empty argv on both halves is the image entrypoint")
	}
	args, err := d.runImageArgs("kontra", spec, spec.Image)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, dockerContainer(spec.Name, spec.Version, partActor)) ||
		strings.Contains(joined, dockerContainer(spec.Name, spec.Version, partHandler)) {
		t.Fatalf("image entrypoint must not start per-half containers, got %s", joined)
	}
	if !containsValue(args, "--name", dockerNetwork(spec.Name, spec.Version)) {
		t.Fatalf("supervised worker name, got %s", joined)
	}
	if !containsValue(args, "--label", workerPairLabelVar+"=1") {
		t.Fatalf("missing pair label, got %s", joined)
	}
	if strings.Contains(joined, "--entrypoint") {
		t.Fatalf("empty argv must keep the image entrypoint, got %s", joined)
	}
	if !containsValue(args, "--pid", "container:"+mustHostname(t)) {
		t.Fatalf("worker must share the Warden PID namespace, got %s", joined)
	}
}

func mustHostname(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil || h == "" {
		t.Fatalf("hostname: %v", err)
	}
	return h
}

func containsValue(args []string, flag, want string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == want {
			return true
		}
	}
	return false
}

func containsNetwork(args []string, name string) bool {
	for i, a := range args {
		if a == "--network" && i+1 < len(args) && args[i+1] == name {
			return true
		}
	}
	return false
}

// A WORKER THAT DIES MUST LEAVE ITS LAST WORDS, and `--rm` was deleting them.
//
// Measured on the compose install: a Fleet Machine logged `starting hello@0.1.0 (restart 1)` through
// `(restart 7)` and `hello@0.1.0 is missing` in between, and the container holding the reason was
// gone each time before anything could read it — including the install job's own failure dump, which
// walks `docker ps -a`. The Worker had printed the cause on its first line.
func TestADeadWorkerKeepsItsLogsUntilItsReplacementStarts(t *testing.T) {
	d := &dockerDriver{bin: "docker", net: "kontra"}
	spec := Spec{
		Name:    "hello",
		Version: "0.1.0",
		Image:   "registry:5000/hello@sha256:" + strings.Repeat("a", 64),
		Actor:   ProcSpec{Env: []string{"KONTRA_ADDRESS=temporal:7233"}},
		Handler: ProcSpec{Env: []string{"KONTRA_ADDRESS=temporal:7233"}},
	}

	for _, args := range [][]string{
		mustRunArgs(t, d, spec, partActor),
		mustRunImageArgs(t, d, spec),
	} {
		for _, a := range args {
			if a == "--rm" {
				t.Errorf("--rm deletes the container the moment it exits, which is the log an "+
					"operator needs: %s", strings.Join(args, " "))
			}
		}
		// Still detached and still named — the name is what the next attempt has to clear.
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--detach") || !strings.Contains(joined, "--name") {
			t.Errorf("a worker must still be detached and named: %s", joined)
		}
	}
}

func mustRunArgs(t *testing.T, d *dockerDriver, spec Spec, part workerPart) []string {
	t.Helper()
	args, err := d.runArgs(d.net, spec, part, spec.Actor, spec.Image)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func mustRunImageArgs(t *testing.T, d *dockerDriver, spec Spec) []string {
	t.Helper()
	args, err := d.runImageArgs(d.net, spec, spec.Image)
	if err != nil {
		t.Fatal(err)
	}
	return args
}
