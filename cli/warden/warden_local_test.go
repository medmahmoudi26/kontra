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
}
