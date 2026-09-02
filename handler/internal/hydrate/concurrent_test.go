package hydrate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-local/handler/internal/cas"
)

// The env the parent hands the children. A child is this same test binary, run with
// -test.run pointed at the helper below — the standard Go way to get a REAL second process,
// which is what "two `kontra up` invocations racing the same first-run hydration" means. A
// goroutine race (TestConcurrentHydrationInOneProcess) exercises the same code but shares a
// heap; only separate processes prove that the safety is in the filesystem operations and
// not in some in-process coordination that happens to exist.
const (
	childEnv    = "KONTRA_HYDRATE_CHILD"
	childURL    = "KONTRA_HYDRATE_CHILD_URL"
	childDigest = "KONTRA_HYDRATE_CHILD_DIGEST"
	childRoot   = "KONTRA_HYDRATE_CHILD_ROOT"
	childDest   = "KONTRA_HYDRATE_CHILD_DEST"
)

func TestConcurrentHydrationAcrossProcesses(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		t.Skip("running as a child")
	}

	gz := tarGz(t, []tarEntry{
		{name: "top/bin/node", body: strings.Repeat("payload ", 1<<15), mode: 0o755},
		{name: "top/README", body: "readme", mode: 0o644},
	})
	digest := cas.Sha256Hex(gz)

	// Stall in the MIDDLE of the body, so every child is holding a half-written temporary
	// at the same instant. Without the stall the first child usually wins outright and the
	// race the test is named after never happens.
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Length", fmt.Sprint(len(gz)))
		half := len(gz) / 2
		_, _ = w.Write(gz[:half])
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write(gz[half:])
	}))
	defer srv.Close()

	root := t.TempDir()
	work := t.TempDir()
	const n = 6

	type child struct {
		cmd *exec.Cmd
		out *strings.Builder
	}
	children := make([]child, n)
	for i := range children {
		out := &strings.Builder{}
		cmd := exec.Command(os.Args[0], "-test.run=^TestHydrationChildProcess$", "-test.v")
		cmd.Env = append(os.Environ(),
			childEnv+"=1",
			childURL+"="+srv.URL+"/pkg.tar.gz",
			childDigest+"="+digest,
			childRoot+"="+root,
			childDest+"="+filepath.Join(work, fmt.Sprintf("w%d", i)),
		)
		cmd.Stdout, cmd.Stderr = out, out
		children[i] = child{cmd: cmd, out: out}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
	}
	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Errorf("child %d failed: %v\n%s", i, err, c.out.String())
		}
	}
	for _, c := range children {
		for _, line := range strings.Split(c.out.String(), "\n") {
			if strings.HasPrefix(line, "hydrated") {
				t.Log(line)
			}
		}
	}
	t.Logf("%d racing processes, %d download(s) — no lock, so a cold-start race may pay twice", n, atomic.LoadInt64(&hits))

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{Name: "pkg", URL: srv.URL + "/pkg.tar.gz", Digest: digest, Kind: KindTarGz, StripComponents: 1}
	assertOneCorrectStore(t, s, a, work, n)
}

// TestHydrationChildProcess is not a test. It is one `kontra up`.
func TestHydrationChildProcess(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("helper process for TestConcurrentHydrationAcrossProcesses")
	}
	s, err := Open(os.Getenv(childRoot))
	if err != nil {
		t.Fatal(err)
	}
	a := Artifact{
		Name: "pkg", URL: os.Getenv(childURL), Digest: os.Getenv(childDigest),
		Kind: KindTarGz, StripComponents: 1,
	}
	res, err := s.Hydrate(context.Background(), a, os.Getenv(childDest), cas.Shared)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("hydrated pid=%d fetched=%v method=%s files=%d bytes=%d\n",
		os.Getpid(), res.Fetched, res.Method, res.Files, res.Bytes)
}
