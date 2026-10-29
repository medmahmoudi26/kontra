// Package kit holds what the gate-run kits' tests share (test/demo, test/vps; DM4-15, DM4-31): the switch that turns
// the Docker-backed ones on, the gate image built from the working tree, a compose project of the team gate with an
// override file, and the certificates and keys a run mints.
//
// The Docker-backed tests build images and bring up containers, so they run only with KONTRA_KIT_TESTS=1 (make
// test-kits), never under -short (make check-go) and never beside another heavy run on a shared host. Every compose
// project, volume and image a test creates is named after the test process and removed in t.Cleanup.
//
// Work package: M4 WP-08.
package kit

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// GateUID is the image's nonroot user (gcr.io/distroless/static-debian12:nonroot), the owner the gate needs of what it
// reads (keys) and writes (the ACME cache).
const GateUID = 65532

// Need skips t unless the Docker-backed kit tests are on: KONTRA_KIT_TESTS=1, not -short, and a docker daemon that
// answers.
func Need(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("a Docker-backed kit test: skipped under -short")
	}
	if os.Getenv("KONTRA_KIT_TESTS") != "1" {
		t.Skip("a Docker-backed kit test: set KONTRA_KIT_TESTS=1 (make test-kits)")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker does not answer: %v: %s", err, out)
	}
}

// Root is the repository's root directory.
func Root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("kit: no caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Run runs name with args in dir, with env added to a clean copy of this process's environment, and returns its
// combined output.
func Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(CleanEnv(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, Tail(string(out), 40))
	}
	return string(out), nil
}

// CleanEnv is this process's environment without the compose and Kontra settings a developer's shell may hold.
func CleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "KONTRA_") || strings.HasPrefix(k, "COMPOSE_") || k == "POSTGRES_PASSWORD" ||
			k == "TEMPORAL_DB_PASSWORD" || k == "DOCKER_DEFAULT_PLATFORM" {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// Tail is the last n lines of s.
func Tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// GateImage is a kontra-gate image built from the working tree's Dockerfile with the docker driver's builder
// (BUILDX_BUILDER=default: never another project's builder), removed when the test ends. KONTRA_KIT_GATE_IMAGE names an
// image to use instead (make test-kits builds one for both packages).
func GateImage(t *testing.T) string {
	t.Helper()
	if img := os.Getenv("KONTRA_KIT_GATE_IMAGE"); img != "" {
		return img
	}
	img := fmt.Sprintf("kontra-gate:kit-%d", os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	t.Logf("building %s from the working tree", img)
	if _, err := Run(ctx, Root(t), []string{"BUILDX_BUILDER=default"}, "docker", "build", "-q", "--build-arg", "VERSION=kit",
		"-t", img, "."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = Run(context.Background(), "", nil, "docker", "image", "rm", img) })
	return img
}

// FreePort is a TCP port free on 127.0.0.1 now.
func FreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Stack is a compose project of compose.yaml with an override file, under a project name of this process's.
type Stack struct {
	Project string
	Dir     string // the run's directory: the override, the configuration, the keys
	files   []string
	env     []string
	root    string
	logf    func(string, ...any)
}

// NewStack writes override into a new directory and returns the project of compose.yaml and it, with env (KEY=value)
// as compose's settings. The project, its volumes and networks are removed when the test ends.
func NewStack(t *testing.T, name, override string, env ...string) *Stack {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // the gate's uid reads the mounted files under it
		t.Fatal(err)
	}
	ov := filepath.Join(dir, "compose.override.yaml")
	if err := os.WriteFile(ov, []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	root := Root(t)
	s := &Stack{Project: fmt.Sprintf("%s%d", name, os.Getpid()), Dir: dir, root: root,
		files: []string{filepath.Join(root, "compose.yaml"), ov}, env: env, logf: t.Logf}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if t.Failed() {
			if out, err := s.Compose(ctx, "logs", "--no-color", "--tail", "60"); err == nil {
				t.Logf("compose logs (last 60 lines each):\n%s", out)
			}
		}
		if out, err := s.Compose(ctx, "down", "-v", "--remove-orphans", "--timeout", "10"); err != nil {
			t.Errorf("compose down: %v\n%s", err, out)
		}
	})
	return s
}

// SetEnv adds compose settings (KEY=value) to the stack's.
func (s *Stack) SetEnv(env ...string) { s.env = append(s.env, env...) }

// Compose runs docker compose on the stack.
func (s *Stack) Compose(ctx context.Context, args ...string) (string, error) {
	a := []string{"compose", "-p", s.Project}
	for _, f := range s.files {
		a = append(a, "-f", f)
	}
	return Run(ctx, s.root, s.env, "docker", append(a, args...)...)
}

// Container is the id of service's container.
func (s *Stack) Container(ctx context.Context, service string) (string, error) {
	out, err := s.Compose(ctx, "ps", "-a", "-q", service)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" || strings.Contains(id, "\n") {
		return "", fmt.Errorf("compose: service %s has containers %q", service, id)
	}
	return id, nil
}

// Health is service's container health status (healthy, unhealthy, starting), or its state when it has no health.
func (s *Stack) Health(ctx context.Context, service string) string {
	id, err := s.Container(ctx, service)
	if err != nil {
		return "absent"
	}
	out, err := Run(ctx, "", nil, "docker", "inspect", "-f", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", id)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(out)
}

// HealthLog is the output of service's last healthcheck runs.
func (s *Stack) HealthLog(ctx context.Context, service string) string {
	id, err := s.Container(ctx, service)
	if err != nil {
		return ""
	}
	out, _ := Run(ctx, "", nil, "docker", "inspect", "-f", "{{if .State.Health}}{{json .State.Health.Log}}{{end}}", id)
	var log []struct {
		ExitCode int
		Output   string
	}
	_ = json.Unmarshal([]byte(out), &log)
	var b strings.Builder
	for _, l := range log {
		fmt.Fprintf(&b, "exit %d: %s\n", l.ExitCode, strings.TrimSpace(l.Output))
	}
	return b.String()
}

// WaitHealthy waits until service's container is healthy.
func (s *Stack) WaitHealthy(ctx context.Context, service string, within time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	for {
		h := s.Health(ctx, service)
		if h == "healthy" {
			return nil
		}
		if h == "exited" || h == "dead" {
			return fmt.Errorf("%s is %s", service, h)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s is %s after %s; its healthchecks:\n%s", service, h, within, s.HealthLog(context.Background(), service))
		case <-time.After(2 * time.Second):
		}
	}
}

// IP is service's container address on the project's default network.
func (s *Stack) IP(ctx context.Context, service string) (string, error) {
	id, err := s.Container(ctx, service)
	if err != nil {
		return "", err
	}
	out, err := Run(ctx, "", nil, "docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", id)
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) != 1 {
		return "", fmt.Errorf("service %s has addresses %q", service, out)
	}
	return f[0], nil
}

// Network is the project's default network.
func (s *Stack) Network() string { return s.Project + "_default" }

// Entry is a file in a container, as `docker cp` archives it.
type Entry struct {
	Name     string
	UID, GID int
	Mode     os.FileMode
	Dir      bool
	Size     int64
}

// List is what path in service's container holds, owners and modes included (docker cp's archive), keyed by the
// names in it (the last element of path, then the relative path).
func (s *Stack) List(ctx context.Context, service, path string) (map[string]Entry, error) {
	id, err := s.Container(ctx, service)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "docker", "cp", id+":"+path, "-")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker cp %s:%s: %w: %s", id, path, err, errb.String())
	}
	m := map[string]Entry{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return m, nil
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(h.Name, "/")
		m[name] = Entry{Name: name, UID: h.Uid, GID: h.Gid, Mode: h.FileInfo().Mode(), Dir: h.Typeflag == tar.TypeDir, Size: h.Size}
	}
}

// Put writes a file into service's container at dir/name, owned by uid:gid with mode, through `docker cp -a` (a
// read-only root filesystem takes it into a writable volume).
func (s *Stack) Put(ctx context.Context, service, dir, name string, data []byte, uid, gid int, mode int64) error {
	id, err := s.Container(ctx, service)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Uid: uid, Gid: gid,
		ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "cp", "-a", "-", id+":"+dir)
	cmd.Stdin = &b
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp -a - %s:%s: %w: %s", id, dir, err, out)
	}
	return nil
}

// WriteOwned writes data to path for the gate's uid to read: owned by GateUID with mode when this process may chown
// (root, as on this host), else world-readable.
func WriteOwned(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, GateUID, GateUID); err != nil {
		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
}

// CA is a throwaway certificate authority.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	PEM  []byte
}

// NewCA mints a CA valid for a day.
func NewCA(t *testing.T) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kontra kit test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{cert: cert, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Leaf mints a server certificate for names, signed by ca when ca is not nil (else self-signed), valid for 90 days:
// its chain and its key in PEM. KeyPEM is SEC 1 ("EC PRIVATE KEY"), which both tls.X509KeyPair and autocert's cache read.
func (ca *CA) Leaf(t *testing.T, names ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	parent, signer := tmpl, key
	if ca != nil {
		parent, signer = ca.cert, ca.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

// SigningKey is a card-signing key, ECDSA P-256 in PKCS #8 PEM, as the compose files' comments make one (D166).
func SigningKey(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// Env is KEY=value pairs from alternating arguments.
func Env(kv ...any) []string {
	var env []string
	for i := 0; i+1 < len(kv); i += 2 {
		v := kv[i+1]
		var s string
		switch v := v.(type) {
		case int:
			s = strconv.Itoa(v)
		default:
			s = fmt.Sprint(v)
		}
		env = append(env, fmt.Sprintf("%s=%s", kv[i], s))
	}
	return env
}

// BaseTools are the commands a script's sandbox links from this host's PATH: what the kit scripts use besides the
// tools a test leaves out or replaces with a stub.
var BaseTools = []string{"bash", "env", "sed", "awk", "grep", "cat", "date", "mktemp", "rm", "mkdir", "head", "tail", "tr",
	"cut", "sort", "uniq", "wc", "sleep", "seq", "git", "jq", "dirname", "basename", "id", "uname", "tee", "ls", "chmod",
	"tar", "timeout", "cp", "mv", "touch", "ssh-keygen", "openssl"}

// Sandbox is a directory to use as a script's whole PATH: links to the host's tools (those of them that exist) and the
// stubs (name to a bash script body, run with KIT_STUB_LOG naming a file each stub appends "name args" to). Tools the
// test wants missing are simply not listed.
func Sandbox(t *testing.T, tools []string, stubs map[string]string) (bin, stubLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if _, ok := stubs[tool]; ok {
			continue
		}
		p, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	stubLog = filepath.Join(dir, "stub.log")
	for name, body := range stubs {
		script := "#!/bin/bash\nprintf '%s' \"" + name + "\" >> \"$KIT_STUB_LOG\"; printf ' %s' \"$@\" >> \"$KIT_STUB_LOG\"; echo >> \"$KIT_STUB_LOG\"\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, stubLog
}

// Script runs the repository's script with args, PATH set to bin and env added (KEY=value), from the repository's
// root, and returns its stdout and stderr and exit code.
func Script(t *testing.T, script, bin, stubLog string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	root := Root(t)
	cmd := exec.CommandContext(ctx, bash, append([]string{filepath.Join(root, script)}, args...)...)
	cmd.Dir = root
	cmd.Env = append([]string{"PATH=" + bin, "HOME=" + t.TempDir(), "KIT_STUB_LOG=" + stubLog, "TMPDIR=" + t.TempDir(), "LANG=C.UTF-8"}, env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	code = 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return o.String(), e.String(), code
}

// StubCalls is the stub log's lines.
func StubCalls(t *testing.T, stubLog string) []string {
	t.Helper()
	b, err := os.ReadFile(stubLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// Lint runs bash -n on each script and, when shellcheck is installed, shellcheck.
func Lint(t *testing.T, scripts ...string) {
	t.Helper()
	root := Root(t)
	for _, s := range scripts {
		p := filepath.Join(root, s)
		if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", s, err, out)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", s)
		}
	}
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Log("shellcheck is not installed: bash -n only")
		return
	}
	args := []string{"-x"}
	for _, s := range scripts {
		args = append(args, filepath.Join(root, s))
	}
	if out, err := exec.Command(sc, args...).CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}
