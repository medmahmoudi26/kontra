// Package vps tests the team gate's VPS kit (DM4-14, DM4-31; the M4 plan's gate run C): test/vps/cloud-init.yaml,
// scripts/team-gate-vps.sh against stubs of doctl, Cloudflare's API and ssh (nothing is ever created), and, with
// KONTRA_KIT_TESTS=1, compose.yaml's ACME variant brought up on this host.
//
// Work package: M4 WP-08.
package vps

import (
	"context"
	"crypto/tls"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/test/kit"
)

const script = "scripts/team-gate-vps.sh"

// The droplet's user data is ASCII only (the Windmill fleet's cloud-init trap: one non-ASCII byte and the droplet boots
// without Docker), starts as cloud-config, and installs Docker and Compose from Ubuntu's packages and nothing else.
func TestCloudInitIsASCII(t *testing.T) {
	b, err := os.ReadFile("cloud-init.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range b {
		if c > 0x7e || (c < 0x20 && c != '\n') {
			line := strings.Count(string(b[:i]), "\n") + 1
			t.Fatalf("cloud-init.yaml line %d: byte 0x%02x is not printable ASCII", line, c)
		}
	}
	if !strings.HasPrefix(string(b), "#cloud-config\n") {
		t.Error("cloud-init.yaml does not start with #cloud-config")
	}
	var pkgs []string
	for _, l := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(l, "  - "); ok && !strings.HasPrefix(p, "[") {
			pkgs = append(pkgs, p)
		}
	}
	if want := []string{"docker.io", "docker-compose-v2", "docker-buildx"}; !slices.Equal(pkgs, want) {
		t.Errorf("packages %v, want %v", pkgs, want)
	}
}

func TestScriptLints(t *testing.T) { kit.Lint(t, script) }

// stubs answers as doctl, curl (Cloudflare's API and DNS over HTTPS), docker and ssh would, from the files in dir:
// droplets ("ID NAME TAGS" per line) and records (one DNS record id per line). STUB_DO_FP is the fingerprint doctl
// gives key 58735991, STUB_DOCTL_AUTH=fail fails doctl account get, STUB_DOCKER=down fails docker info.
func stubs(dir string) map[string]string {
	return map[string]string{
		"doctl": `d=` + dir + `
case "$*" in
  "account get"*) [ "${STUB_DOCTL_AUTH:-ok}" = ok ] || { echo "Error: unauthorized" >&2; exit 1; }; echo "me@example.com active" ;;
  "compute ssh-key get 58735991 "*) echo "$STUB_DO_FP" ;;
  "compute size list "*) printf 's-1vcpu-1gb 0.00893\ns-2vcpu-4gb 0.03571\n' ;;
  "compute droplet list --tag-name kontra-m4-gaterun "*) cat "$d/droplets" 2>/dev/null; true ;;
  "compute droplet delete -f "*) id=${!#}; grep -v "^$id " "$d/droplets" > "$d/droplets.new"; mv "$d/droplets.new" "$d/droplets" ;;
  *) echo "doctl stub: not expected: $*" >&2; exit 9 ;;
esac`,
		"curl": `d=` + dir + `
method=GET; url=""
while [ $# -gt 0 ]; do case "$1" in -X) method=$2; shift ;; http*) url=$1 ;; esac; shift; done
records() { jq -Rn '{success: true, result: [inputs | select(length > 0) | {id: .}]}' < "$d/records"; }
case "$method $url" in
  "GET https://api.cloudflare.com/client/v4/zones?name=kontra.run") echo '{"success":true,"result":[{"id":"zone1"}]}' ;;
  "GET https://api.cloudflare.com/client/v4/zones/zone1/dns_records?name=m4-teamgate.kontra.run") records ;;
  "GET https://api.cloudflare.com/client/v4/zones/zone1/dns_records?comment.exact=M4%20VPS%20gate%20run%2C%20delete%20after") records ;;
  "DELETE https://api.cloudflare.com/client/v4/zones/zone1/dns_records/"*)
    id=${url##*/}; grep -vx "$id" "$d/records" > "$d/records.new"; mv "$d/records.new" "$d/records"
    echo '{"success":true,"result":{"id":"'"$id"'"}}' ;;
  *) echo "curl stub: not expected: $method $url" >&2; exit 9 ;;
esac`,
		"docker": `[ "$1" = info ] && [ "${STUB_DOCKER:-up}" = up ] && exit 0
[ "$1" = info ] && { echo "Cannot connect to the Docker daemon" >&2; exit 1; }
echo "docker stub: not expected: $*" >&2; exit 9`,
		"ssh": `echo "ssh stub: not expected: $*" >&2; exit 9`,
	}
}

// vpsRun is one stubbed run of the script: the sandbox, an SSH key pair whose MD5 fingerprint the doctl stub returns
// unless fp says otherwise, and the stubs' files.
type vpsRun struct {
	t       *testing.T
	dir     string
	bin     string
	log     string
	keyFile string
	fp      string
	env     []string
}

func newRun(t *testing.T, leave ...string) *vpsRun {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"droplets", "records"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(dir, "fleet")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen: %v %s", err, out)
	}
	out, err := exec.Command("ssh-keygen", "-E", "md5", "-lf", key+".pub").Output()
	if err != nil {
		t.Fatal(err)
	}
	fp := strings.TrimPrefix(strings.Fields(string(out))[1], "MD5:")
	tools := append(append([]string(nil), kit.BaseTools...), "doctl", "curl", "docker", "ssh")
	tools = slices.DeleteFunc(tools, func(s string) bool { return slices.Contains(leave, s) })
	st := stubs(dir)
	for _, l := range leave {
		delete(st, l)
	}
	bin, log := kit.Sandbox(t, tools, st)
	r := &vpsRun{t: t, dir: dir, bin: bin, log: log, keyFile: key, fp: fp}
	r.env = []string{"CLOUDFLARE_API_TOKEN=cf-test-token", "KONTRA_VPS_SSH_KEY=" + key, "STUB_DO_FP=" + fp}
	return r
}

func (r *vpsRun) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *vpsRun) run(env []string, args ...string) (string, string, int) {
	r.t.Helper()
	return kit.Script(r.t, script, r.bin, r.log, append(append([]string(nil), r.env...), env...), args...)
}

func (r *vpsRun) calls() []string { return kit.StubCalls(r.t, r.log) }

// The script refuses to start without each prerequisite, naming it, before it calls anything but the checks.
func TestTeamGateRefusesWithoutPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave []string
		env   []string
		want  string
	}{
		{"no doctl", []string{"doctl"}, nil, "doctl"},
		{"no docker", []string{"docker"}, nil, "docker"},
		{"no jq", []string{"jq"}, nil, "jq"},
		{"no Cloudflare token", nil, []string{"CLOUDFLARE_API_TOKEN="}, "CLOUDFLARE_API_TOKEN"},
		{"doctl not authenticated", nil, []string{"STUB_DOCTL_AUTH=fail"}, "doctl is not authenticated"},
		{"no docker daemon", nil, []string{"STUB_DOCKER=down"}, "docker cannot reach its daemon"},
		{"no SSH key", nil, []string{"KONTRA_VPS_SSH_KEY=/nonexistent/key"}, "/nonexistent/key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRun(t, tc.leave...)
			stdout, stderr, code := r.run(tc.env)
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q, want 2 naming %q (stdout %q)", code, stderr, tc.want, stdout)
			}
			for _, c := range r.calls() {
				if strings.Contains(c, "droplet create") || strings.Contains(c, "droplet delete") || strings.Contains(c, " -X POST") ||
					strings.Contains(c, " -X DELETE") || strings.HasPrefix(c, "ssh ") {
					t.Errorf("a refused start called %q", c)
				}
			}
		})
	}
	t.Run("teardown-only needs no docker", func(t *testing.T) {
		r := newRun(t, "docker", "ssh")
		if _, stderr, code := r.run(nil, "--teardown-only"); code != 0 {
			t.Errorf("--teardown-only without docker: exit %d: %s", code, stderr)
		}
	})
}

// --dry-run runs the read-only pre-flight, prints the plan and the teardown it would run, and creates nothing.
func TestTeamGateDryRun(t *testing.T) {
	r := newRun(t)
	stdout, stderr, code := r.run(nil, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"teardown, in a trap on EXIT/INT/TERM: docker rm -f kontra-teammate-",
		"doctl compute droplet delete -f <id of kontra-m4-teamgate-",
		"DELETE the A record of m4-teamgate.kontra.run by id; ssh-keygen -R <ip>;",
		"then: no droplet tagged kontra-m4-gaterun, no record for m4-teamgate.kontra.run",
		"(fra1, s-2vcpu-4gb at $0.03571/h, ubuntu-24-04-x64), https://m4-teamgate.kontra.run",
		"dry run: pre-flight passed; nothing created",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	calls := r.calls()
	for _, want := range []string{"doctl account get", "doctl compute ssh-key get 58735991", "doctl compute size list",
		"doctl compute droplet list --tag-name kontra-m4-gaterun", "docker info"} {
		if !slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, want) }) {
			t.Errorf("the pre-flight never ran %q: %q", want, calls)
		}
	}
	for _, c := range calls {
		if strings.Contains(c, "droplet create") || strings.Contains(c, "droplet delete") || strings.Contains(c, " -X POST") ||
			strings.Contains(c, " -X DELETE") || strings.HasPrefix(c, "ssh ") || (strings.HasPrefix(c, "docker ") && c != "docker info") {
			t.Errorf("--dry-run called %q", c)
		}
	}
	assertListsOnlyByTag(t, calls)
}

// The pre-flight refuses a DigitalOcean SSH key that is not this host's, leftovers of another run, and an existing
// record for the name, before anything is created.
func TestTeamGatePreflightRefusals(t *testing.T) {
	t.Run("another SSH key", func(t *testing.T) {
		r := newRun(t)
		_, stderr, code := r.run([]string{"STUB_DO_FP=00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff"}, "--dry-run")
		if code != 2 || !strings.Contains(stderr, "has fingerprint '00:11:22") || !strings.Contains(stderr, r.fp) {
			t.Errorf("exit %d, stderr %q", code, stderr)
		}
	})
	t.Run("a tagged droplet left", func(t *testing.T) {
		r := newRun(t)
		r.write("droplets", "501 kontra-m4-teamgate-1 kontra-m4-gaterun,ttl-2\n")
		_, stderr, code := r.run(nil, "--dry-run")
		if code != 2 || !strings.Contains(stderr, "--teardown-only") || !strings.Contains(stderr, "501") {
			t.Errorf("exit %d, stderr %q", code, stderr)
		}
	})
	t.Run("a record left", func(t *testing.T) {
		r := newRun(t)
		r.write("records", "rec9\n")
		_, stderr, code := r.run(nil, "--dry-run")
		if code != 2 || !strings.Contains(stderr, "already has a record") {
			t.Errorf("exit %d, stderr %q", code, stderr)
		}
	})
}

// --teardown-only deletes every droplet tagged kontra-m4-gaterun, whether or not its ttl passed, and every DNS record
// with the run's name or comment; droplets are listed only by the tag, and main-droplet is never deleted.
func TestTeamGateTeardownOnly(t *testing.T) {
	r := newRun(t)
	r.write("droplets", "101 kontra-m4-teamgate-1000 kontra-m4-gaterun,ttl-15400\n"+
		"102 kontra-m4-teamgate-9999999990 kontra-m4-gaterun,ttl-9999999999\n")
	r.write("records", "rec1\nrec2\n")
	stdout, stderr, code := r.run(nil, "--teardown-only")
	if code != 0 {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	calls := r.calls()
	for _, want := range []string{"doctl compute droplet delete -f 101", "doctl compute droplet delete -f 102"} {
		if !slices.Contains(calls, want) {
			t.Errorf("never ran %q: %q", want, calls)
		}
	}
	deletes := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "curl ") && strings.Contains(c, "-X DELETE") {
			deletes++
			if !strings.HasSuffix(c, "/dns_records/rec1") && !strings.HasSuffix(c, "/dns_records/rec2") {
				t.Errorf("deleted %q", c)
			}
		}
	}
	if deletes != 2 {
		t.Errorf("%d DNS deletions, want 2: %q", deletes, calls)
	}
	if !strings.Contains(stdout, "kontra-m4-teamgate-1000 (101): its ttl passed") || !strings.Contains(stdout, "nothing of a run is left") {
		t.Errorf("stdout:\n%s", stdout)
	}
	assertListsOnlyByTag(t, calls)
	if strings.Contains(stdout+stderr+strings.Join(calls, "\n"), "main-droplet") {
		t.Error("the run named main-droplet")
	}

	// A droplet named main-droplet that somehow carries the tag is refused, and the run fails.
	r = newRun(t)
	r.write("droplets", "999 main-droplet kontra-m4-gaterun\n103 kontra-m4-teamgate-1 kontra-m4-gaterun,ttl-2\n")
	_, stderr, code = r.run(nil, "--teardown-only")
	calls = r.calls()
	if code == 0 || !strings.Contains(stderr, "refusing to delete main-droplet") || slices.Contains(calls, "doctl compute droplet delete -f 999") ||
		!slices.Contains(calls, "doctl compute droplet delete -f 103") {
		t.Errorf("with main-droplet tagged: exit %d, stderr %q, calls %q", code, stderr, calls)
	}
}

// assertListsOnlyByTag fails when doctl listed droplets other than by the run's tag.
func assertListsOnlyByTag(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		if strings.HasPrefix(c, "doctl compute droplet list") && !strings.HasPrefix(c, "doctl compute droplet list --tag-name kontra-m4-gaterun ") {
			t.Errorf("droplets listed without the tag: %q", c)
		}
	}
}

// The ACME variant on this host (the M4 plan's audit, gaps 17; DM4-14): compose.yaml with kontra.vps.yaml as an
// operator edits it, the gate image built from the working tree, a fresh named volume. The gate, running as 65532 on a
// read-only root filesystem, writes its ACME cache into the volume (the account key autocert writes before it asks the
// CA, whose name resolves to 127.0.0.1 here, so nothing reaches Let's Encrypt); once the cache holds a certificate for
// the name (put there as autocert stores one), compose's own healthcheck, which sends public_url's host as the server
// name, makes the gate healthy, and the published port serves that certificate; without a server name, health fails.
func TestACMEVariantWritesItsCache(t *testing.T) {
	kit.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	const domain = "gate.kontra.test"
	img := kit.GateImage(t)
	port, metrics := kit.FreePort(t), kit.FreePort(t)
	override := `services:
  gate-migrate: { image: ` + img + ` }
  gate:
    image: ` + img + `
    extra_hosts: ["acme-v02.api.letsencrypt.org:127.0.0.1", "acme-staging-v02.api.letsencrypt.org:127.0.0.1"]
`
	s := kit.NewStack(t, "kitacme", override)
	cfg, err := os.ReadFile(filepath.Join(kit.Root(t), "deploy", "compose", "kontra.vps.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	kit.WriteOwned(t, filepath.Join(s.Dir, "kontra.yaml"), []byte(strings.ReplaceAll(string(cfg), "gate.example.com", domain)), 0o644)
	kit.WriteOwned(t, filepath.Join(s.Dir, "signing", "kontra.key"), kit.SigningKey(t), 0o400)
	if err := os.MkdirAll(filepath.Join(s.Dir, "tls"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.SetEnv(kit.Env("KONTRA_CONFIG", filepath.Join(s.Dir, "kontra.yaml"), "KONTRA_SIGNING_DIR", filepath.Join(s.Dir, "signing"),
		"KONTRA_TLS_DIR", filepath.Join(s.Dir, "tls"), "KONTRA_LISTEN_ADDR", "127.0.0.1", "KONTRA_PORT", port, "KONTRA_METRICS_PORT", metrics)...)
	if out, err := s.Compose(ctx, "up", "-d", "--no-build"); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}

	// The gate writes its cache: the volume's root, acme/ and the account key are 65532's.
	var cache map[string]kit.Entry
	deadline := time.Now().Add(8 * time.Minute)
	for {
		cache, err = s.List(ctx, "gate", "/var/lib/kontra-gate")
		if err == nil {
			if _, ok := cache["kontra-gate/acme/acme_account+key"]; ok {
				break
			}
		}
		if s.Health(ctx, "gate") == "exited" {
			logs, _ := s.Compose(ctx, "logs", "--no-color", "gate")
			t.Fatalf("the gate exited before it wrote its ACME cache:\n%s", kit.Tail(logs, 30))
		}
		if time.Now().After(deadline) {
			logs, _ := s.Compose(ctx, "logs", "--no-color", "gate")
			t.Fatalf("no acme/acme_account+key in the volume after 8 minutes (%v, %v):\n%s", cache, err, kit.Tail(logs, 30))
		}
		time.Sleep(3 * time.Second)
	}
	for _, name := range []string{"kontra-gate", "kontra-gate/acme", "kontra-gate/acme/acme_account+key"} {
		e := cache[name]
		if e.UID != kit.GateUID || e.GID != kit.GateUID {
			t.Errorf("%s is %d:%d, want %d:%d", name, e.UID, e.GID, kit.GateUID, kit.GateUID)
		}
	}
	if m := cache["kontra-gate/acme"].Mode.Perm(); m != 0o700 {
		t.Errorf("acme/ has mode %v, want 0700 (the account key and private keys)", m)
	}
	t.Logf("the gate wrote its cache: %v", cache)

	// Without a certificate for the name the gate is not healthy (autocert could not order one).
	if h := s.Health(ctx, "gate"); h == "healthy" {
		t.Fatalf("the gate is healthy with no certificate in its cache")
	}
	certPEM, keyPEM := (*kit.CA)(nil).Leaf(t, domain)
	if err := s.Put(ctx, "gate", "/var/lib/kontra-gate/acme", domain, append(keyPEM, certPEM...), kit.GateUID, kit.GateUID, 0o600); err != nil {
		t.Fatal(err)
	}
	// autocert keeps a failed order a minute before it reads the cache again.
	if err := s.WaitHealthy(ctx, "gate", 4*time.Minute); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Compose(ctx, "exec", "-T", "gate", "/usr/local/bin/kontra-gate", "health", "--url", "https://127.0.0.1:8443/healthz"); err == nil ||
		!regexp.MustCompile(`tls|remote error`).MatchString(out) {
		t.Errorf("health without a server name against built-in ACME: %v\n%s", err, out)
	}
	conn, err := tls.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port), &tls.Config{ServerName: domain, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("TLS to the published port with SNI %s: %v", domain, err)
	}
	defer conn.Close()
	if names := conn.ConnectionState().PeerCertificates[0].DNSNames; !slices.Equal(names, []string{domain}) {
		t.Errorf("the published port serves a certificate for %v, want the cache's for %s", names, domain)
	}
}
