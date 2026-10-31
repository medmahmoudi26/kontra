// Package vps tests the team gate's VPS kit (D281, D298; the M4 plan's gate run C): test/vps/cloud-init.yaml,
// scripts/team-gate-vps.sh against stubs of doctl, Cloudflare's API, ssh, docker and openssl (nothing is ever
// created), and, with KONTRA_KIT_TESTS=1, compose.yaml's ACME variant brought up on this host and a rehearsal of the
// script's steps 4 and 5 against it (rehearsal_test.go).
//
// Work package: M4 WP-08.
package vps

import (
	"context"
	"crypto/tls"
	"errors"
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

const (
	script = "scripts/team-gate-vps.sh"
	// The secrets of a stubbed run: the Cloudflare token, and the API key and enrollment token the ssh stub's
	// kontra-gate enroll prints (in enroll's formats). None of them may ever be on a command line.
	cfToken   = "cf-test-token-3f9a"
	stubKey   = "kk_m4run_Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4z"
	stubToken = "ket_Zy9xWv8uTs7rQp6oNm5lKj4iHg3fEd2c"
	// The run's DNS record comment and name (the script's).
	runComment = "M4 VPS gate run, delete after"
	runName    = "m4-teamgate.kontra.run"
	// The run's DigitalOcean SSH key id and a droplet KONTRA_VPS_PROTECTED names: neutral values (D306).
	doKey            = "12345678"
	protectedDroplet = "protected-droplet"
)

// The droplet's user data is ASCII only (cloud-init's trap: one non-ASCII byte and the droplet may boot without
// Docker), starts as cloud-config, and installs Docker and Compose from Ubuntu's packages and nothing else.
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

// stubs answer as doctl, curl (Cloudflare's API, DNS over HTTPS and the gate), ssh (the VPS), docker and openssl
// would, from the files in dir: droplets ("ID NAME TAGS" per line) and records (one DNS record per line: id, type, name,
// content, comment, tab-separated). Cloudflare answers only the token in CLOUDFLARE_API_TOKEN and the gate only the key
// STUB_KEY, wherever curl reads them from. Faults, each from the environment:
//   - STUB_DO_FP is the fingerprint doctl gives key 12345678 (doKey); STUB_DOCTL_AUTH=fail fails doctl account get;
//   - STUB_DOCTL_LIST=fail fails every droplet listing (a 429), =after-create those after the run's droplet exists;
//     STUB_DOCTL_DELETE=fail fails droplet deletion;
//   - STUB_CF_LIST=fail answers every record listing success:false, =after-post those after the run's POST;
//     STUB_CF_DELETE=fail every record deletion; STUB_CF_POST=lost creates the record and loses the answer (curl 28);
//   - STUB_SSH_SHIP=fail breaks the ssh that ships the archive (step 4); STUB_DOCKER=down fails docker info;
//   - STUB_COMPOSE=fail fails docker compose up on the VPS after 30 lines of output and the VPS run's error.
//
// docker cp records the modes of what it copies (stat, relative to the copied directory) in dir/cp-modes.
func stubs(dir string) map[string]string {
	return map[string]string{
		"doctl": `d=` + dir + `
case "$*" in
  "account get"*) [ "${STUB_DOCTL_AUTH:-ok}" = ok ] || { echo "Error: unauthorized" >&2; exit 1; }; echo "me@example.com active" ;;
  "compute ssh-key get ` + doKey + ` "*) echo "$STUB_DO_FP" ;;
  "compute size list "*) printf 's-1vcpu-1gb 0.00893\ns-2vcpu-4gb 0.03571\n' ;;
  "compute droplet list --tag-name kontra-m4-gaterun "*)
    if [ "${STUB_DOCTL_LIST:-ok}" = fail ] || { [ "${STUB_DOCTL_LIST:-ok}" = after-create ] && [ -e "$d/created" ]; }; then
      echo "Error: GET https://api.digitalocean.com/v2/droplets?tag_name=kontra-m4-gaterun: 429 Too many requests" >&2; exit 1
    fi
    cat "$d/droplets" ;;
  "compute droplet create "*)
    tags=""; prev=""
    for a in "$@"; do [ "$prev" = --tag-names ] && tags=$a; prev=$a; done
    echo "201 $4 $tags" >> "$d/droplets"; : > "$d/created"
    echo "201 203.0.113.7" ;;
  "compute droplet delete -f "*)
    [ "${STUB_DOCTL_DELETE:-ok}" = ok ] || { echo "Error: DELETE: 429 Too many requests" >&2; exit 1; }
    id=${!#}; grep -v "^$id " "$d/droplets" > "$d/droplets.new"; mv "$d/droplets.new" "$d/droplets" ;;
  *) echo "doctl stub: not expected: $*" >&2; exit 9 ;;
esac`,
		"curl": `d=` + dir + `
cf=https://api.cloudflare.com/client/v4
gate=https://` + runName + `
method="" url="" auth="" hdr="" data=""
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method=$2; shift ;;
    -H) case "$2" in @-) auth=$(cat) ;; @*) auth=$(cat "${2#@}") ;; [Aa]uthorization:*) auth=$2 ;; esac; shift ;;
    -D) hdr=$2; shift ;;
    -o) exec > "$2"; shift ;;
    --data) data=$2; shift ;;
    --max-time | --connect-to | --cacert) shift ;;
    http*) url=$1 ;;
  esac
  shift
done
if [ -z "$method" ]; then if [ -n "$data" ]; then method=POST; else method=GET; fi; fi
denied='{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}'
recs() {
  jq -Rn --arg f "$1" --arg v "$2" '{success: true, result: [inputs | select(length > 0) | split("\t")
    | {id: .[0], type: .[1], name: .[2], content: .[3], comment: .[4]} | select(.[$f] == $v)]}' < "$d/records"
}
listing() { [ "${STUB_CF_LIST:-ok}" = ok ] || { [ "$STUB_CF_LIST" = after-post ] && [ ! -e "$d/posted" ]; }; }
keyed() { [ "$auth" = "Authorization: Bearer $STUB_KEY" ] || { echo '{"error":"unauthenticated"}'; exit 22; }; }
case "$url" in "$cf"/*) [ "$auth" = "Authorization: Bearer $CLOUDFLARE_API_TOKEN" ] || { echo "$denied"; exit 0; } ;; esac
case "$method $url" in
  "GET $cf/zones?name=kontra.run") echo '{"success":true,"result":[{"id":"zone1"}]}' ;;
  "GET $cf/zones/zone1/dns_records?name="*)
    listing || { echo "$denied"; exit 0; }
    n=${url#*\?name=}; recs name "${n%%&*}" ;;
  "GET $cf/zones/zone1/dns_records?comment.exact=M4%20VPS%20gate%20run%2C%20delete%20after"*)
    listing || { echo "$denied"; exit 0; }
    recs comment "` + runComment + `" ;;
  "POST $cf/zones/zone1/dns_records")
    printf 'recA\t%s\n' "$(jq -r '[.type, .name, .content, .comment] | @tsv' <<< "$data")" >> "$d/records"; : > "$d/posted"
    [ "${STUB_CF_POST:-ok}" = ok ] || { echo "curl: (28) Operation timed out after 30001 milliseconds with 0 bytes received" >&2; exit 28; }
    echo '{"success":true,"result":{"id":"recA"}}' ;;
  "DELETE $cf/zones/zone1/dns_records/"*)
    [ "${STUB_CF_DELETE:-ok}" = ok ] || { echo "$denied"; exit 0; }
    id=${url##*/}; awk -F'\t' -v id="$id" '$1 != id' "$d/records" > "$d/records.new"; mv "$d/records.new" "$d/records"
    echo '{"success":true,"result":{"id":"'"$id"'"}}' ;;
  "GET https://1.1.1.1/dns-query?name=` + runName + `&type=A") echo '{"Status":0,"Answer":[{"type":1,"data":"203.0.113.7"}]}' ;;
  "GET $gate/healthz") echo ok ;;
  "POST $gate/agents/hello") keyed; echo '{"jsonrpc":"2.0","id":1,"result":{"message":{"role":"ROLE_AGENT","parts":[{"text":"Hello World"}]}}}' ;;
  "POST $gate/mcp")
    keyed
    case "$data" in
      *'"initialize"'*) printf 'HTTP/2 200\r\nmcp-session-id: sess-1\r\n\r\n' > "$hdr"; echo '{"jsonrpc":"2.0","id":1,"result":{}}' ;;
      *notifications/initialized*) : ;;
      *'"search"'*) printf 'event: message\ndata: %s\n\n' '{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"services":[{"ref":"hello","kind":"a2a","available":true}]}}}' ;;
      *) echo "curl stub: not expected: MCP $data" >&2; exit 9 ;;
    esac ;;
  "DELETE $gate/mcp") keyed ;;
  *) echo "curl stub: not expected: $method $url" >&2; exit 9 ;;
esac`,
		"ssh": `while [ $# -gt 0 ]; do case "$1" in -i | -o) shift 2 ;; -*) shift ;; *) break ;; esac; done
shift
case "$*" in
  true) ;;
  "cloud-init status --wait") echo "status: done" ;;
  "docker version "*)
    printf '%s\n' "docker Engine 29.1.3; containerd 2.2.1; runc 1.3.4; docker-init 0.19.0; client 29.1.3" \
      'storage driver overlayfs [["driver-type","io.containerd.snapshotter.v1"]]' "Docker Compose version 2.40.3" \
      "github.com/docker/buildx 0.30.1" ;;
  "test -e /opt/kontra/.env") exit 1 ;;
  "mkdir -p /opt/kontra && tar -x -C /opt/kontra")
    cat > /dev/null
    [ "${STUB_SSH_SHIP:-ok}" = ok ] || { echo "client_loop: send disconnect: Broken pipe" >&2; exit 255; } ;;
  "NAME=` + runName + ` DIR=/opt/kontra bash -s") cat > /dev/null; echo "configured (stub)" ;;
  "cd /opt/kontra && timeout 1200 docker compose up -d --build --wait")
    if [ "${STUB_COMPOSE:-ok}" = fail ]; then
      for i in $(seq 30); do echo "compose line $i"; done
      echo '#18 ERROR: image "docker.io/library/kontra-gate:m4": already exists' >&2
      exit 1
    fi
    printf '%s\n' "#17 naming to docker.io/library/kontra-gate:m4 done" " Container kontra-gate-1  Healthy" ;;
  "cd /opt/kontra && docker compose ps -a && docker compose logs --no-color --tail 40") echo "gate-migrate exited (stub ps -a)" ;;
  "cd /opt/kontra && docker compose exec -T gate kontra-gate enroll -f /etc/kontra/kontra.yaml --api-key m4run") echo "$STUB_KEY" ;;
  "cd /opt/kontra && docker compose exec -T gate kontra-gate enroll -f /etc/kontra/kontra.yaml hello") echo "$STUB_TOKEN" ;;
  "cd /opt/kontra && docker compose ps"*) echo "NAME IMAGE STATUS" ;;
  "docker image ls"*) echo "kontra-gate:m3 sha256:0 0" ;;
  *) echo "ssh stub: not expected: $*" >&2; exit 9 ;;
esac`,
		"docker": `d=` + dir + `
case "$1" in
  info) [ "${STUB_DOCKER:-up}" = up ] || { echo "Cannot connect to the Docker daemon" >&2; exit 1; } ;;
  rm | start | ps) ;;
  create) echo cid ;;
  image) [ "$2" = inspect ] || [ "$2" = rm ] || exit 9 ;;
  cp) (cd "$2" && find . -exec stat -c '%a %n' {} +) > "$d/cp-modes" ;;
  logs) echo "kontra connect: connected to https://` + runName + ` as nk_1 (tenant default, session s1)" ;;
  container) echo true ;;
  *) echo "docker stub: not expected: $*" >&2; exit 9 ;;
esac`,
		"openssl": `case "$1" in
  s_client) echo "-----BEGIN CERTIFICATE----- (stub)" ;;
  x509) cat > /dev/null; printf "issuer=C = US, O = Let's Encrypt, CN = R11\nsubject=CN = ` + runName + `\n" ;;
  *) echo "openssl stub: not expected: $*" >&2; exit 9 ;;
esac`,
	}
}

// vpsRun is one stubbed run of the script: the sandbox, an SSH key pair whose MD5 fingerprint the doctl stub returns
// unless fp says otherwise, the stubs' files and the run's record.
type vpsRun struct {
	t       *testing.T
	dir     string
	bin     string
	log     string
	keyFile string
	fp      string
	record  string
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
	tools := append(append([]string(nil), kit.BaseTools...), "doctl", "curl", "docker", "ssh", "stat", "find")
	tools = slices.DeleteFunc(tools, func(s string) bool { return slices.Contains(leave, s) })
	st := stubs(dir)
	for _, l := range leave {
		delete(st, l)
	}
	bin, log := kit.Sandbox(t, tools, st)
	r := &vpsRun{t: t, dir: dir, bin: bin, log: log, keyFile: key, fp: fp, record: filepath.Join(dir, "record.log")}
	r.env = []string{"CLOUDFLARE_API_TOKEN=" + cfToken, "KONTRA_VPS_SSH_KEY=" + key, "KONTRA_VPS_DO_KEY=" + doKey,
		"KONTRA_VPS_PROTECTED=" + protectedDroplet + ",another-droplet", "STUB_DO_FP=" + fp,
		"STUB_KEY=" + stubKey, "STUB_TOKEN=" + stubToken}
	return r
}

func (r *vpsRun) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *vpsRun) read(name string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

// run runs the script with the stubs, its record in the run's directory (never the repository's).
func (r *vpsRun) run(env []string, args ...string) (string, string, int) {
	r.t.Helper()
	return kit.Script(r.t, script, r.bin, r.log, append(append([]string(nil), r.env...), env...), append(args, "--record", r.record)...)
}

// calls is the stubs' log; it fails the test when a secret of the run was on any command line (every local user reads
// /proc/<pid>/cmdline).
func (r *vpsRun) calls() []string {
	r.t.Helper()
	calls := kit.StubCalls(r.t, r.log)
	for _, c := range calls {
		for _, secret := range []string{cfToken, stubKey, stubToken} {
			if strings.Contains(c, secret) {
				r.t.Errorf("a secret is on a command line: %q", c)
			}
		}
	}
	return calls
}

// record is one DNS record line of the stubs' records file.
func record(id, typ, name, content, comment string) string {
	return strings.Join([]string{id, typ, name, content, comment}, "\t") + "\n"
}

// index is the position of the first call with prefix (contains, for a curl call's URL), or -1.
func index(calls []string, sub string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, sub) })
}

// dnsDeletes are the record ids the calls DELETE, in order.
func dnsDeletes(calls []string) []string {
	var ids []string
	for _, c := range calls {
		if strings.HasPrefix(c, "curl ") && strings.Contains(c, "-X DELETE") && strings.Contains(c, "/dns_records/") {
			ids = append(ids, c[strings.LastIndex(c, "/")+1:])
		}
	}
	return ids
}

// The script refuses to start without each prerequisite, naming it, before it calls anything but the checks; a
// KONTRA_VPS_NAME that is not m4-teamgate<…> in the zone, and flags that exclude one another, before it calls anything.
func TestTeamGateRefusesWithoutPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		leave   []string
		env     []string
		args    []string
		want    string
		noCalls bool
	}{
		{"no doctl", []string{"doctl"}, nil, nil, "doctl", false},
		{"no docker", []string{"docker"}, nil, nil, "docker", false},
		{"no jq", []string{"jq"}, nil, nil, "jq", false},
		{"no Cloudflare token", nil, []string{"CLOUDFLARE_API_TOKEN="}, nil, "CLOUDFLARE_API_TOKEN", false},
		{"doctl not authenticated", nil, []string{"STUB_DOCTL_AUTH=fail"}, nil, "doctl is not authenticated", false},
		{"no docker daemon", nil, []string{"STUB_DOCKER=down"}, nil, "docker cannot reach its daemon", false},
		{"no SSH key", nil, []string{"KONTRA_VPS_SSH_KEY=/nonexistent/key"}, nil, "/nonexistent/key", false},
		// D306: neither key has a default (no operator's own identifiers in a public script).
		{"KONTRA_VPS_SSH_KEY unset", nil, []string{"KONTRA_VPS_SSH_KEY="}, nil, "KONTRA_VPS_SSH_KEY", false},
		{"KONTRA_VPS_DO_KEY unset", nil, []string{"KONTRA_VPS_DO_KEY="}, []string{"--dry-run"}, "KONTRA_VPS_DO_KEY", false},
		// The review's case: a mistyped name on the recovery path once deleted staging's records.
		{"another gate's name", nil, []string{"KONTRA_VPS_NAME=gateway.kontra.run"}, []string{"--teardown-only"},
			"KONTRA_VPS_NAME gateway.kontra.run", true},
		{"a name outside the zone", nil, []string{"KONTRA_VPS_NAME=m4-teamgate.example.com"}, nil, "want m4-teamgate<…>.kontra.run", true},
		{"--host and --dry-run", nil, nil, []string{"--host", "192.0.2.10", "--dry-run"}, "exclude one another", true},
		{"a rehearsal flag without --host", nil, nil, []string{"--connect-to", "127.0.0.1:1"}, "need --host", true},
		{"--host without the SSH key", nil, []string{"KONTRA_VPS_SSH_KEY=/nonexistent/key"}, []string{"--host", "192.0.2.10"},
			"/nonexistent/key", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRun(t, tc.leave...)
			stdout, stderr, code := r.run(tc.env, tc.args...)
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q, want 2 naming %q (stdout %q)", code, stderr, tc.want, stdout)
			}
			calls := r.calls()
			if tc.noCalls && len(calls) > 0 {
				t.Errorf("a refused start called %q", calls)
			}
			for _, c := range calls {
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

// --dry-run runs the read-only pre-flight, prints the plan and the teardown it would run (DNS before the droplet), and
// creates nothing.
func TestTeamGateDryRun(t *testing.T) {
	r := newRun(t)
	stdout, stderr, code := r.run(nil, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"teardown, in a trap on EXIT/INT/TERM: docker rm -f kontra-teammate-",
		`DNS first: DELETE the A record of m4-teamgate.kontra.run by id, then every record commented "M4 VPS gate run, delete after"; once that succeeded,`,
		"doctl compute droplet delete -f <id of kontra-m4-teamgate-",
		"then: no droplet tagged kontra-m4-gaterun, no record for m4-teamgate.kontra.run",
		"(fra1, s-2vcpu-4gb at $0.03571/h, ubuntu-24-04-x64), https://m4-teamgate.kontra.run",
		"dry run: pre-flight passed; nothing created",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Index(stdout, "DELETE the A record") > strings.Index(stdout, "droplet delete -f <id") {
		t.Errorf("the printed teardown deletes the droplet before the DNS record:\n%s", stdout)
	}
	calls := r.calls()
	for _, want := range []string{"doctl account get", "doctl compute ssh-key get " + doKey, "doctl compute size list",
		"doctl compute droplet list --tag-name kontra-m4-gaterun", "docker info", "/dns_records?name=m4-teamgate.kontra.run",
		"/dns_records?comment.exact="} {
		if index(calls, want) < 0 {
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

// The pre-flight refuses a DigitalOcean SSH key that is not this host's, leftovers of another run, a record named the
// name or with the run's comment, and a listing that fails (a failed listing is never "nothing there"), before anything
// is created.
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
	for name, rec := range map[string]string{
		"a record named the name": record("rec9", "A", runName, "203.0.113.9", "made by hand"),
		"a record of a run":       record("rec8", "A", "m4-teamgate-2.kontra.run", "203.0.113.8", runComment),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRun(t)
			r.write("records", rec)
			_, stderr, code := r.run(nil, "--dry-run")
			if code != 2 || !strings.Contains(stderr, "already has a record") || !strings.Contains(stderr, strings.SplitN(rec, "\t", 2)[0]) {
				t.Errorf("exit %d, stderr %q", code, stderr)
			}
		})
	}
	for _, env := range []string{"STUB_DOCTL_LIST=fail", "STUB_CF_LIST=fail"} {
		t.Run(env, func(t *testing.T) {
			r := newRun(t)
			stdout, stderr, code := r.run([]string{env}, "--dry-run")
			if code == 0 || !strings.Contains(stderr, "could not be listed") || strings.Contains(stdout, "pre-flight passed") {
				t.Errorf("a listing that failed: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
			}
		})
	}
}

// --teardown-only deletes the DNS records of a run (every record with the run's comment, whatever its name) and then
// every droplet tagged kontra-m4-gaterun, whether or not its ttl passed; droplets are listed only by the tag, and a
// droplet named in KONTRA_VPS_PROTECTED is never deleted.
func TestTeamGateTeardownOnly(t *testing.T) {
	r := newRun(t)
	r.write("droplets", "101 kontra-m4-teamgate-1000 kontra-m4-gaterun,ttl-15400\n"+
		"102 kontra-m4-teamgate-9999999990 kontra-m4-gaterun,ttl-9999999999\n")
	r.write("records", record("rec1", "A", runName, "203.0.113.1", runComment)+record("rec2", "AAAA", runName, "2001:db8::1", runComment)+
		record("rec3", "A", "m4-teamgate-2.kontra.run", "203.0.113.3", runComment))
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
	if got := dnsDeletes(calls); !slices.Equal(got, []string{"rec1", "rec2", "rec3"}) {
		t.Errorf("DNS deletions %v, want rec1, rec2 and rec3: %q", got, calls)
	}
	if last, first := slices.IndexFunc(calls, func(c string) bool { return strings.HasSuffix(c, "/dns_records/rec3") }),
		index(calls, "droplet delete"); last > first {
		t.Errorf("a droplet was deleted before the DNS records: %q", calls)
	}
	if !strings.Contains(stdout, "kontra-m4-teamgate-1000 (101): its ttl passed") || !strings.Contains(stdout, "nothing of a run is left") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if r.read("droplets") != "" || r.read("records") != "" {
		t.Errorf("left: droplets %q, records %q", r.read("droplets"), r.read("records"))
	}
	assertListsOnlyByTag(t, calls)
	if strings.Contains(stdout+stderr+strings.Join(calls, "\n"), protectedDroplet) {
		t.Error("the run named the protected droplet")
	}

	// A protected droplet that somehow carries the tag is refused, and the run fails.
	r = newRun(t)
	r.write("droplets", "999 "+protectedDroplet+" kontra-m4-gaterun\n103 kontra-m4-teamgate-1 kontra-m4-gaterun,ttl-2\n")
	_, stderr, code = r.run(nil, "--teardown-only")
	calls = r.calls()
	if code == 0 || !strings.Contains(stderr, "refusing to delete "+protectedDroplet) || slices.Contains(calls, "doctl compute droplet delete -f 999") ||
		!slices.Contains(calls, "doctl compute droplet delete -f 103") {
		t.Errorf("with %s tagged: exit %d, stderr %q, calls %q", protectedDroplet, code, stderr, calls)
	}
}

// --teardown-only fails closed (the review's finding): when doctl or Cloudflare cannot list, or Cloudflare cannot
// delete, it never says "nothing of a run is left" and exits non-zero; and no droplet is deleted while a record of a run
// may still name its address.
func TestTeamGateTeardownOnlyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		env         string
		want        string
		dnsDeleted  bool
		dropletKept bool
	}{
		{"STUB_DOCTL_LIST=fail", "could not verify that no droplet", true, true},
		{"STUB_CF_LIST=fail", "could not list the DNS records", false, true},
		{"STUB_CF_DELETE=fail", "no droplet is deleted while a DNS record of a run may be left", false, true},
	} {
		t.Run(tc.env, func(t *testing.T) {
			r := newRun(t)
			r.write("droplets", "101 kontra-m4-teamgate-1000 kontra-m4-gaterun,ttl-15400\n")
			r.write("records", record("rec1", "A", runName, "203.0.113.1", runComment))
			stdout, stderr, code := r.run([]string{tc.env}, "--teardown-only")
			out := stdout + stderr
			if code == 0 || strings.Contains(out, "nothing of a run is left") || !strings.Contains(out, tc.want) {
				t.Errorf("exit %d, want non-zero naming %q and never \"nothing of a run is left\":\n%s", code, tc.want, out)
			}
			calls := r.calls()
			if tc.dropletKept && (index(calls, "droplet delete") >= 0 || r.read("droplets") == "") {
				t.Errorf("the droplet was deleted: %q", calls)
			}
			if gone := r.read("records") == ""; gone != tc.dnsDeleted {
				t.Errorf("the record deleted %v, want %v: %q", gone, tc.dnsDeleted, calls)
			}
		})
	}
}

// --teardown-only deletes only the records with the run's comment (D298): a record named the name with another
// comment is reported and left, the reap fails, and the droplets are kept. The review's case, KONTRA_VPS_NAME naming the
// staging gate, deletes nothing.
func TestTeamGateReapsOnlyTheRunsRecords(t *testing.T) {
	t.Run("another gate's name", func(t *testing.T) {
		r := newRun(t)
		staging := record("staging-A", "A", "gateway.kontra.run", "203.0.113.50", "Kontra v4 M4 staging gate") +
			record("staging-AAAA", "AAAA", "gateway.kontra.run", "2001:db8::50", "Kontra v4 M4 staging gate")
		r.write("records", staging)
		_, _, code := r.run([]string{"KONTRA_VPS_NAME=gateway.kontra.run"}, "--teardown-only")
		if code != 2 || len(dnsDeletes(r.calls())) > 0 || r.read("records") != staging {
			t.Errorf("exit %d, records %q", code, r.read("records"))
		}
	})
	t.Run("a record with another comment", func(t *testing.T) {
		r := newRun(t)
		r.write("droplets", "101 kontra-m4-teamgate-1000 kontra-m4-gaterun,ttl-15400\n")
		foreign := record("hand1", "A", runName, "198.51.100.4", "made by hand")
		r.write("records", foreign+record("rec1", "A", runName, "203.0.113.1", runComment))
		stdout, stderr, code := r.run(nil, "--teardown-only")
		calls := r.calls()
		if code == 0 || !strings.Contains(stdout, "refusing to delete A m4-teamgate.kontra.run 198.51.100.4 (hand1)") ||
			strings.Contains(stdout+stderr, "nothing of a run is left") {
			t.Errorf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
		}
		if got := dnsDeletes(calls); !slices.Equal(got, []string{"rec1"}) || r.read("records") != foreign {
			t.Errorf("DNS deletions %v (want rec1), records left %q", got, r.read("records"))
		}
		if index(calls, "droplet delete") >= 0 {
			t.Errorf("a droplet was deleted while a record named %s is left: %q", runName, calls)
		}
	})
}

// A run's own teardown (the EXIT trap), on a run that fails after its DNS record exists: DNS before the droplet; every
// record with the run's comment, also one whose id the run never learnt; the droplet kept while a record may be left;
// and a listing that fails is never "nothing left".
func TestTeamGateRunTeardown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []string
		want    string
		clean   bool // the run ends with nothing left
		deleted bool // the droplet is deleted
	}{
		{"the run fails at step 4", nil, "torn down; nothing left", true, true},
		{"the POST's answer is lost", []string{"STUB_CF_POST=lost"}, "torn down; nothing left", true, true},
		{"Cloudflare cannot delete", []string{"STUB_CF_DELETE=fail"}, "KEPT the droplet kontra-m4-teamgate-", false, false},
		{"Cloudflare cannot list", []string{"STUB_CF_LIST=after-post"}, "could not verify that no DNS record", false, false},
		{"doctl cannot list", []string{"STUB_DOCTL_LIST=after-create"}, "could not verify that no droplet", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRun(t)
			stdout, stderr, code := r.run(append([]string{"STUB_SSH_SHIP=fail"}, tc.env...))
			out := stdout + stderr
			if code == 0 || !strings.Contains(out, tc.want) || (!tc.clean && strings.Contains(out, "nothing left")) {
				t.Errorf("exit %d, want non-zero naming %q:\n%s", code, tc.want, out)
			}
			calls := r.calls()
			if index(calls, "droplet create") < 0 || index(calls, "-X POST") < 0 {
				t.Fatalf("the run never created its droplet and record: %q", calls)
			}
			del := index(calls, "doctl compute droplet delete -f 201")
			if tc.deleted != (del >= 0) {
				t.Errorf("the droplet deleted %v, want %v: %q", del >= 0, tc.deleted, calls)
			}
			if tc.deleted && (len(dnsDeletes(calls)) == 0 || index(calls, "/dns_records/recA") > del) {
				t.Errorf("the droplet was deleted before the DNS record recA: %q", calls)
			}
			if tc.clean && (r.read("records") != "" || r.read("droplets") != "") {
				t.Errorf("left: droplets %q, records %q", r.read("droplets"), r.read("records"))
			}
			if !tc.deleted && r.read("droplets") == "" {
				t.Errorf("the droplet is gone: %q", calls)
			}
		})
	}
}

// --host runs steps 4 and 5 on a host the operator already has, under a hardened umask: it creates and deletes nothing
// on DigitalOcean or Cloudflare (it needs neither doctl nor the token), the team member's files are readable by the
// image's user whatever the umask, and neither the gate's key nor the node's token is ever on a command line or in the
// record (the review's findings on --host, the umask and argv).
func TestTeamGateHostMode(t *testing.T) {
	kit.Umask(t, 0o077)
	r := newRun(t, "doctl")
	stdout, stderr, code := r.run([]string{"CLOUDFLARE_API_TOKEN="}, "--host", "192.0.2.10", "--connect-to", "127.0.0.1:9")
	if code != 0 || !strings.Contains(stdout, "team-gate-vps: PASS (root@192.0.2.10, https://m4-teamgate.kontra.run") ||
		!strings.Contains(stdout, "torn down; the gate on 192.0.2.10 stays up") {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	calls := r.calls()
	var remote []string
	for _, c := range calls {
		if strings.Contains(c, "api.cloudflare.com") || strings.HasPrefix(c, "doctl ") {
			t.Errorf("--host called %q", c)
		}
		if strings.HasPrefix(c, "ssh ") {
			if !strings.Contains(c, " root@192.0.2.10 ") {
				t.Errorf("ssh to another host: %q", c)
			}
			remote = append(remote, c[strings.Index(c, " root@192.0.2.10 ")+len(" root@192.0.2.10 "):])
		}
	}
	for i, want := range []string{"test -e /opt/kontra/.env", "mkdir -p /opt/kontra && tar -x -C /opt/kontra",
		"NAME=m4-teamgate.kontra.run DIR=/opt/kontra bash -s", "docker version --format ",
		"cd /opt/kontra && timeout 1200 docker compose up -d --build --wait"} {
		if i >= len(remote) || !(remote[i] == want || strings.HasSuffix(want, " ") && strings.HasPrefix(remote[i], want)) {
			t.Errorf("remote command %d is not %q: %q", i+1, want, remote)
		}
	}
	modes := r.read("cp-modes")
	for _, want := range []string{"755 .\n", "644 ./run.sh\n", "644 ./token\n"} {
		if !strings.Contains(modes, want) {
			t.Errorf("under umask 077 the team member's files are copied as\n%swant %q", modes, want)
		}
	}
	rec, err := os.ReadFile(r.record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rec), stubKey) || strings.Contains(string(rec), stubToken) || !strings.Contains(string(rec), "PASS") {
		t.Errorf("the record:\n%s", rec)
	}
	// D313: the record says which Docker the stack met (the VPS run's defect showed on the containerd image store only)
	// and which images compose built.
	for _, want := range []string{
		"  vps | docker Engine 29.1.3; containerd 2.2.1;",
		`  vps | storage driver overlayfs [["driver-type","io.containerd.snapshotter.v1"]]`,
		"  vps | Docker Compose version 2.40.3",
		"; the images it built:\n  vps | #17 naming to docker.io/library/kontra-gate:m4 done\n",
	} {
		if !strings.Contains(string(rec), want) {
			t.Errorf("the record lacks %q:\n%s", want, rec)
		}
	}
}

// D313: when docker compose up fails, the record keeps its whole output (the VPS run's kept the last 20 lines, where the
// error happened to be), then the stack as compose left it, and the run fails without going on to step 5.
func TestTeamGateRecordsComposeFailure(t *testing.T) {
	r := newRun(t, "doctl")
	stdout, stderr, code := r.run([]string{"CLOUDFLARE_API_TOKEN=", "STUB_COMPOSE=fail"}, "--host", "192.0.2.10", "--connect-to", "127.0.0.1:9")
	if code == 0 || strings.Contains(stdout, "PASS") {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	b, err := os.ReadFile(r.record)
	if err != nil {
		t.Fatal(err)
	}
	rec := string(b)
	for _, want := range []string{
		"  vps | storage driver overlayfs",
		"team-gate-vps: docker compose up -d --build --wait failed (exit 1) after ",
		"its output, all 31 lines:\n  vps | compose line 1\n  vps | compose line 2\n",
		"  vps | compose line 30\n",
		`  vps | #18 ERROR: image "docker.io/library/kontra-gate:m4": already exists`,
		"the stack as compose left it:\n  vps | gate-migrate exited (stub ps -a)\n",
		"torn down; the gate on 192.0.2.10 stays up",
	} {
		if !strings.Contains(rec, want) {
			t.Errorf("the record lacks %q:\n%s", want, rec)
		}
	}
	if i := index(r.calls(), "/healthz"); i >= 0 {
		t.Errorf("the run went on to step 5 after compose failed: %q", r.calls()[i])
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

// The ACME variant on this host (the M4 plan's audit, gaps 17; D281): compose.yaml with kontra.vps.yaml as an
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
	if err := s.PutVolume(ctx, "acme", "acme/"+domain, append(keyPEM, certPEM...), kit.GateUID, kit.GateUID, 0o600); err != nil {
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

// D306: what the README's VPS steps write in the working tree (.env with the database passwords, the operator's copy of
// the configuration, the card-signing key, a provided certificate) is ignored by git, so `git add -A` never commits it,
// and kept out of the image's build context; the shipped configurations are not ignored.
func TestOperatorSecretsAreIgnored(t *testing.T) {
	root := kit.Root(t)
	ignored := func(path string) bool {
		cmd := exec.Command("git", "check-ignore", "-q", "--no-index", path)
		cmd.Dir = root
		err := cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return true
		case errors.As(err, &ee) && ee.ExitCode() == 1:
			return false
		}
		t.Fatalf("git check-ignore %s: %v", path, err)
		return false
	}
	for _, p := range []string{".env", "deploy/compose/kontra.team.yaml", "deploy/compose/kontra.local.yaml",
		"deploy/compose/signing/kontra.key", "deploy/compose/tls/cert.pem", "deploy/compose/tls/key.pem"} {
		if !ignored(p) {
			t.Errorf("%s is not ignored by git", p)
		}
	}
	for _, p := range []string{"deploy/compose/kontra.yaml", "deploy/compose/kontra.vps.yaml", "deploy/compose/kontra.lifecycle.yaml",
		"deploy/compose/postgres-init.sh", "compose.yaml"} {
		if ignored(p) {
			t.Errorf("%s, which ships, is ignored by git", p)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	for _, want := range []string{".env", "deploy"} {
		if !slices.Contains(lines, want) {
			t.Errorf(".dockerignore has no line %q: the build context would carry the operator's secrets", want)
		}
	}
}
