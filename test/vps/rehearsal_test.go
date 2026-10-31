package vps

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/test/kit"
)

// The script's steps 4 and 5 rehearsed on this host (the review's finding: before, they had never run anywhere, and the
// first run would have been on a billed droplet). scripts/team-gate-vps.sh --host runs them as written against a
// stand-in VPS: an ssh that runs the remote commands here, with /opt/kontra the stack's directory and docker compose
// there the test's project. So the archive is shipped, the remote configure step writes kontra.run.yaml, .env and the
// signing key, compose comes up with them, kontra-gate enroll mints the key and the token, a kontra-demo container on
// the project's network is the team member's node, and SendMessage and the MCP search go from this host with the key.
//
// What the stand-in changes, and why (compose.override.yaml in the stack's directory, which compose reads beside the
// shipped compose.yaml): the gate's image tag, one of the run's instead of kontra-gate:m4 (which another checkout may
// hold), which the script's docker compose up --build still builds from the shipped archive, as on the VPS, and must
// build once (D313: the VPS run failed where gate-migrate built the same tag too, and this host's overlay2 store would
// retag silently, so the record's builds are counted); Let's Encrypt resolving to 127.0.0.1 and the certificate for
// the name put in the ACME cache beforehand, signed by the run's CA (TestACMEVariantWritesItsCache shows the cache is
// written; nothing here may reach Let's Encrypt); edge, a forwarder answering for the name on port 443 of the
// network, which is what the VPS's published port is to the team member; the published ports on loopback. The
// rehearsal flags --network, --ca-file and --connect-to say how the team member and this host reach the gate; the port
// probe and the Let's Encrypt issuer are the VPS's and are not rehearsed, nor are steps 1 to 3 (the stub tests cover
// them). The whole run is under umask 077.
func TestTeamGateRehearsal(t *testing.T) {
	kit.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	img := fmt.Sprintf("kontra-gate:kitvps-%d", os.Getpid()) // built by the run's compose; removed after compose down
	t.Cleanup(func() { _, _ = kit.Run(context.Background(), "", nil, "docker", "image", "rm", img) })
	port, metrics := kit.FreePort(t), kit.FreePort(t)
	override := fmt.Sprintf(`# test/vps's stand-in VPS (TestTeamGateRehearsal): never on a real host.
services:
  gate-migrate: { image: %[1]s }
  gate:
    image: %[1]s
    extra_hosts: ["acme-v02.api.letsencrypt.org:127.0.0.1", "acme-staging-v02.api.letsencrypt.org:127.0.0.1"]
  edge:
    image: %[2]s
    init: true
    command: ["nc", "-lk", "-p", "443", "-e", "nc", "gate", "8443"]
    networks: { default: { aliases: [%[3]s] } }
    depends_on: { gate: { condition: service_healthy } }
`, img, kit.HelperImage, runName)
	var vol string // removed after compose down (cleanups run last first)
	t.Cleanup(func() {
		if vol != "" {
			_, _ = kit.Run(context.Background(), "", nil, "docker", "volume", "rm", "-f", vol)
		}
	})
	s := kit.NewStack(t, "kitvps", override)

	// The ACME cache, as the gate's image makes a new volume (65532's) with the name's certificate where autocert keeps
	// one (acme/<name>: the key, then the chain). Labelled as compose labels its own, so that compose adopts it.
	ca := kit.NewCA(t)
	cert, key := ca.Leaf(t, runName)
	vol = s.Project + "_acme"
	if out, err := kit.Run(ctx, "", nil, "docker", "volume", "create", "--label", "com.docker.compose.project="+s.Project,
		"--label", "com.docker.compose.volume=acme", vol); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	seed := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "-v", vol+":/v", kit.HelperImage, "sh", "-c",
		fmt.Sprintf(`mkdir /v/acme && cat > /v/acme/%[1]s && chown -R %[2]d:%[2]d /v && chmod 0700 /v/acme && chmod 0600 /v/acme/%[1]s`,
			runName, kit.GateUID))
	seed.Stdin = bytes.NewReader(append(key, cert...))
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding the ACME cache: %v\n%s", err, out)
	}

	// The stand-in's ssh, first on the script's PATH.
	shim := t.TempDir()
	sshLog := filepath.Join(shim, "ssh.log")
	sshStub := fmt.Sprintf(`#!/bin/bash
# ssh to test/vps's stand-in VPS, which is this host: ssh's options and the destination are dropped, /opt/kontra is the
# stack's directory, and docker compose there is the stack's project, published on loopback, building with the docker
# driver's builder as a VPS's only one (never another project's builder on this host).
printf 'ssh' >> %[1]q; printf ' %%s' "$@" >> %[1]q; echo >> %[1]q
while [ $# -gt 0 ]; do case "$1" in -i | -o) shift 2 ;; -*) shift ;; *) break ;; esac; done
shift
export COMPOSE_PROJECT_NAME=%[2]s KONTRA_LISTEN_ADDR=127.0.0.1 KONTRA_PORT=%[3]d KONTRA_METRICS_PORT=%[4]d BUILDX_BUILDER=default
cmd=$*
exec bash -c "${cmd//\/opt\/kontra/%[5]s}"
`, sshLog, s.Project, port, metrics, s.Dir)
	if err := os.WriteFile(filepath.Join(shim, "ssh"), []byte(sshStub), 0o755); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "fleet")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyFile).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, ca.PEM, 0o644); err != nil {
		t.Fatal(err)
	}
	sha := headSHA(t)
	_, imageErr := kit.Run(ctx, "", nil, "docker", "image", "inspect", "kontra-demo:"+sha)
	recordFile := filepath.Join(t.TempDir(), "record.log")

	kit.Umask(t, 0o077)
	root := kit.Root(t)
	out, err := kit.Run(ctx, root, []string{"PATH=" + shim + ":" + os.Getenv("PATH"), "KONTRA_VPS_SSH_KEY=" + keyFile},
		filepath.Join(root, script), "--host", "127.0.0.1", "--network", s.Network(), "--ca-file", caFile,
		"--connect-to", "127.0.0.1:"+strconv.Itoa(port), "--record", recordFile)
	if err != nil {
		t.Fatalf("scripts/team-gate-vps.sh --host: %v\n%s", err, out)
	}
	// The run without the record step's image list (this host's, not a VPS's).
	images := regexp.MustCompile(`(?m)^  vps \| \S+ (sha256:\S+|<none>) [0-9a-f]{12}\n`)
	t.Logf("the run:\n%s", images.ReplaceAllString(out, ""))
	b, err := os.ReadFile(recordFile)
	if err != nil {
		t.Fatal(err)
	}
	rec := string(b)
	for _, want := range []string{
		"configured: deploy/compose/kontra.run.yaml, .env (KONTRA_PORT=443), a new signing key",
		"GET https://m4-teamgate.kontra.run/healthz: 200",
		"issuer=CN = kontra kit test CA",
		"an API key kk_… and a token for hello (neither recorded)",
		"connected to https://m4-teamgate.kontra.run as ",
		`SendMessage "hi" to https://m4-teamgate.kontra.run/agents/hello with the key: `,
		"MCP tools/call search on https://m4-teamgate.kontra.run/mcp with the key: ",
		"team-gate-vps: PASS (root@127.0.0.1, https://m4-teamgate.kontra.run",
		"torn down; the gate on 127.0.0.1 stays up",
	} {
		if !strings.Contains(rec, want) {
			t.Errorf("the record lacks %q", want)
		}
	}
	// D313: the record names this host's Docker and storage driver, and compose built the gate's image once, from the
	// shipped archive with VERSION m4; gate-migrate ran that image, as the gate does.
	if !regexp.MustCompile(`(?m)^  vps \| docker Engine \S+;.*\n  vps \| storage driver \S+ `).MatchString(rec) {
		t.Errorf("the record lacks the host's docker version and storage driver")
	}
	built := regexp.MustCompile(`(?m)^  vps \| #[0-9]+ naming to (\S+)( \S+)? done$`).FindAllStringSubmatch(rec, -1)
	if len(built) != 1 || built[0][1] != "docker.io/library/"+img {
		t.Errorf("compose built %q, want %s once", built, img)
	}
	if !regexp.MustCompile(`(?m)^  vps \| kontra-gate m4 \(`).MatchString(rec) {
		t.Errorf("the record lacks kontra-gate version m4")
	}
	want, err := kit.Run(ctx, "", nil, "docker", "image", "inspect", "-f", "{{.Id}}", img)
	if err != nil {
		t.Fatalf("compose did not build %s: %v", img, err)
	}
	for _, svc := range []string{"gate-migrate", "gate"} {
		id, err := s.Container(ctx, svc)
		if err != nil {
			t.Fatal(err)
		}
		ran, err := kit.Run(ctx, "", nil, "docker", "inspect", "-f", "{{.Image}} {{.State.Status}} {{.State.ExitCode}}", id)
		if err != nil || !strings.HasPrefix(ran, strings.TrimSpace(want)+" ") {
			t.Errorf("%s ran %q (%v), want %s's image %s", svc, ran, err, img, want)
		}
		if svc == "gate-migrate" && strings.TrimSpace(ran) != strings.TrimSpace(want)+" exited 0" {
			t.Errorf("gate-migrate: %q, want exited 0", ran)
		}
	}
	// The key's first 8 random characters are its public id (the principal key:<prefix>, which enroll prints on stderr);
	// a ninth would be the secret's.
	if m := regexp.MustCompile(`kk_m4run_[A-Za-z0-9]{9,}|ket_[A-Za-z0-9]{32}`).FindString(rec); m != "" {
		t.Errorf("the record holds a secret: %q", m)
	}
	ssh, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`kk_[a-z0-9]+_[A-Za-z0-9]{32}|ket_[A-Za-z0-9]{32}`).Match(ssh) {
		t.Errorf("a secret went to ssh's command line:\n%s", ssh)
	}

	// What the remote configure step wrote, as the gate reads it.
	env, err := os.ReadFile(filepath.Join(s.Dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?s)^KONTRA_CONFIG=\./deploy/compose/kontra\.run\.yaml\nKONTRA_PORT=443\nKONTRA_DB_PASSWORD=[0-9a-f]{48}\n` +
		`TEMPORAL_DB_PASSWORD=[0-9a-f]{48}\nPOSTGRES_PASSWORD=[0-9a-f]{48}\n$`).Match(env) {
		t.Errorf(".env:\n%s", env)
	}
	cfg, err := os.ReadFile(filepath.Join(s.Dir, "deploy", "compose", "kontra.run.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "public_url: https://"+runName+"\n") || !strings.Contains(string(cfg), "domains: ["+runName+"]") ||
		strings.Contains(string(cfg), "kontra:kontra@") {
		t.Errorf("kontra.run.yaml:\n%s", cfg)
	}
	for path, want := range map[string]string{".env": "-rw-------", "deploy/compose/signing/kontra.key": "-r--------",
		"deploy/compose/kontra.run.yaml": "-rw-r--r--"} {
		fi, err := os.Stat(filepath.Join(s.Dir, path))
		if err != nil || fi.Mode().String() != want {
			t.Errorf("%s: %v %v, want %s", path, fi.Mode(), err, want)
		}
	}

	// The teardown removed the team member's machine, and the image when it built it.
	if out, err := kit.Run(ctx, "", nil, "docker", "ps", "-a", "-q", "--filter", "name=kontra-teammate-"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("a kontra-teammate container is left: %q %v", out, err)
	}
	if _, err := kit.Run(ctx, "", nil, "docker", "image", "inspect", "kontra-demo:"+sha); imageErr != nil && err == nil {
		t.Errorf("the run built kontra-demo:%s and left it", sha)
	}
}

func headSHA(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", kit.Root(t), "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		t.Fatalf("git: %v", err)
	}
	return strings.TrimSpace(string(out))
}
