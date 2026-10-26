//go:build unix

// Package helloworld_test checks run.sh, the script that starts the a2a-samples helloworld seller unmodified.
// The seller is Python and has no Go code; these tests are the only Go here.
package helloworld_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// pin is the a2a-samples commit run.sh runs.
const pin = "6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3"

const sample = "samples/python/agents/helloworld"

// samplesEnv names a checkout of a2a-samples at pin, for the tests that need the real seller (the e2e
// harness reads the same variable).
const samplesEnv = "KONTRA_A2A_SAMPLES"

// tools is what run.sh needs besides uv; toolPath links them alone into a directory used as PATH.
var tools = []string{"bash", "env", "dirname", "git", "mkdir", "mv", "rm"}

func toolPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range tools {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not found: %v", name, err)
		}
		if err := os.Symlink(p, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeUV writes an executable uv that records that it ran and exits 0; the file it writes is returned.
func fakeUV(t *testing.T) (uv, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "ran")
	uv = filepath.Join(dir, "uv")
	if err := os.WriteFile(uv, []byte("#!/bin/sh\necho \"$@\" > '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return uv, marker
}

// script runs run.sh with only env (and a scratch HOME) and returns its exit code and stderr.
func script(t *testing.T, env ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, "./run.sh")
	cmd.Env = append([]string{"HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, ".cache")}, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String()
	}
	t.Fatalf("run.sh: %v (stderr %q)", err, stderr.String())
	return 0, ""
}

// holdPort9999 listens on 127.0.0.1:9999 for the test, or skips it when something else already does.
func holdPort9999(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Skipf("127.0.0.1:9999 is in use by something else: %v", err)
	}
	t.Cleanup(func() { l.Close() })
}

// needPort9999 skips the test unless 127.0.0.1:9999 is free.
func needPort9999(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Skipf("127.0.0.1:9999 is in use: %v", err)
	}
	l.Close()
}

func TestRunShWithoutUV(t *testing.T) {
	path := toolPath(t)
	code, stderr := script(t, "PATH="+path)
	if code != 1 || !strings.Contains(stderr, "uv not found: install uv") {
		t.Fatalf("without uv: exit %d, stderr %q", code, stderr)
	}
	code, stderr = script(t, "PATH="+path, "UV=/nonexistent/uv")
	if code != 1 || !strings.Contains(stderr, "/nonexistent/uv not found") {
		t.Fatalf("UV=/nonexistent/uv: exit %d, stderr %q", code, stderr)
	}
}

func TestRunShPortTaken(t *testing.T) {
	holdPort9999(t)
	uv, ran := fakeUV(t)
	code, stderr := script(t, "PATH="+toolPath(t), "UV="+uv, "A2A_SAMPLES="+t.TempDir())
	if code != 1 || !strings.Contains(stderr, "127.0.0.1:9999 is taken") {
		t.Fatalf("port taken: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("uv ran although the port was taken")
	}
}

// git runs git in dir with a fixed identity.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A checkout at another commit, or one that is not a checkout, is refused before uv runs.
func TestRunShRefusesAnotherCommit(t *testing.T) {
	needPort9999(t)
	path := toolPath(t)
	uv, ran := fakeUV(t)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, sample), 0o755); err != nil {
		t.Fatal(err)
	}
	code, stderr := script(t, "PATH="+path, "UV="+uv, "A2A_SAMPLES="+dir)
	if code != 1 || !strings.Contains(stderr, "is not a git checkout") {
		t.Fatalf("not a checkout: exit %d, stderr %q", code, stderr)
	}

	git(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, sample, "__main__.py"), []byte("print('not the sample')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "x")
	code, stderr = script(t, "PATH="+path, "UV="+uv, "A2A_SAMPLES="+dir)
	if code != 1 || !strings.Contains(stderr, "not at the pinned "+pin) {
		t.Fatalf("another commit: exit %d, stderr %q", code, stderr)
	}

	empty := t.TempDir()
	code, stderr = script(t, "PATH="+path, "UV="+uv, "A2A_SAMPLES="+empty)
	if code != 1 || !strings.Contains(stderr, "has no "+sample) {
		t.Fatalf("no sample: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("uv ran for a checkout run.sh refuses")
	}
}

// pinnedCheckout returns the checkout named by KONTRA_A2A_SAMPLES, or skips.
func pinnedCheckout(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(samplesEnv)
	if dir == "" {
		t.Skipf("%s is not set (an a2a-samples checkout at %s)", samplesEnv, pin)
	}
	if head := git(t, dir, "rev-parse", "HEAD"); head != pin {
		t.Fatalf("%s=%s is at %s, want %s", samplesEnv, dir, head, pin)
	}
	return dir
}

// A modified sample, or one with an extra file (even an ignored one, such as __pycache__), is refused: the
// example runs the sample unmodified.
func TestRunShRefusesModifiedSample(t *testing.T) {
	needPort9999(t)
	src := pinnedCheckout(t)
	path := toolPath(t)
	uv, ran := fakeUV(t)

	dir := filepath.Join(t.TempDir(), "a2a-samples")
	git(t, filepath.Dir(dir), "clone", "-q", "--no-checkout", src, dir)
	git(t, dir, "-c", "advice.detachedHead=false", "checkout", "-q", pin)

	code, stderr := script(t, "PATH="+path, "UV="+uv, "A2A_SAMPLES="+dir)
	if code != 0 {
		t.Fatalf("the clean pinned checkout: exit %d, stderr %q", code, stderr)
	}
	args, err := os.ReadFile(ran)
	if err != nil {
		t.Fatalf("uv did not run for the clean pinned checkout: %v", err)
	}
	if got, want := strings.TrimSpace(string(args)), "run --no-project --python 3.12 --exclude-newer 2026-08-05T00:00:00Z --with-requirements requirements.txt python __main__.py"; got != want {
		t.Fatalf("uv ran with %q, want %q", got, want)
	}
	os.Remove(ran)

	for name, change := range map[string]func(){
		"an edited file": func() {
			f, err := os.OpenFile(filepath.Join(dir, sample, "agent_executor.py"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString("# edited\n")
			f.Close()
		},
		"an ignored file": func() {
			if err := os.MkdirAll(filepath.Join(dir, sample, "__pycache__"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, sample, "__pycache__", "x.pyc"), []byte{0}, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		change()
		code, stderr := script(t, "PATH="+path, "UV="+uv, "A2A_SAMPLES="+dir)
		if code != 1 || !strings.Contains(stderr, "is not the unmodified sample") {
			t.Fatalf("%s: exit %d, stderr %q", name, code, stderr)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Fatalf("%s: uv ran", name)
		}
		git(t, dir, "checkout", "-q", "--", sample)
		git(t, dir, "clean", "-q", "-fdx", "--", sample)
	}
}

// The real seller: run.sh starts it from the pinned checkout with uv, it answers "hi" as the PRD's helloworld
// does, it stops on SIGTERM, and the checkout is unmodified afterwards (no file changed or added, ignored
// ones included). Needs KONTRA_A2A_SAMPLES, uv and a free 127.0.0.1:9999; uv may download the requirements.
func TestRunShStartsTheUnmodifiedSample(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the Python seller (integration)")
	}
	dir := pinnedCheckout(t)
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not found")
	}
	needPort9999(t)

	cmd := exec.Command("./run.sh")
	cmd.Env = append(os.Environ(), "A2A_SAMPLES="+dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	})

	var card a2a.AgentCard
	deadline := time.Now().Add(3 * time.Minute)
	for {
		resp, err := http.Get("http://127.0.0.1:9999/.well-known/agent-card.json")
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&card)
			resp.Body.Close()
			if err == nil {
				break
			}
		}
		select {
		case err := <-exited:
			t.Fatalf("run.sh exited before the seller answered: %v\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the seller did not answer within 3 minutes\n%s", out.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if card.Name != "Hello World Agent" || len(card.SupportedInterfaces) != 1 ||
		card.SupportedInterfaces[0].URL != "http://127.0.0.1:9999" ||
		card.SupportedInterfaces[0].ProtocolBinding != a2a.TransportProtocolJSONRPC ||
		card.SupportedInterfaces[0].ProtocolVersion != "1.0" {
		t.Fatalf("card %+v", card)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := a2aclient.NewFromCard(ctx, &card)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted || len(task.Artifacts) != 1 || len(task.Artifacts[0].Parts) != 1 {
		t.Fatalf("SendMessage returned %+v", res)
	}
	if got := task.Artifacts[0].Parts[0].Text(); got != "Hello, World! I have received your request (hi)" {
		t.Fatalf("answer %q", got)
	}

	// SIGTERM to the script's process (uv, which run.sh became) stops the seller too.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatalf("run.sh did not stop on SIGTERM\n%s", out.String())
	}
	if err := syscall.Kill(-cmd.Process.Pid, 0); err == nil {
		t.Fatal("a process of the seller outlived run.sh")
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:9999", time.Second); err == nil {
		conn.Close()
		t.Fatal("127.0.0.1:9999 still answers after run.sh stopped")
	}
	if st := git(t, dir, "status", "--porcelain", "--ignored"); st != "" {
		t.Fatalf("the checkout changed:\n%s", st)
	}
}
