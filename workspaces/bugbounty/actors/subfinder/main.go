// subfinder — a Go kontra actor that enumerates subdomains of a wildcard scope asset using
// ProjectDiscovery's subfinder, and STREAMS each discovered host as its own durable sub-unit.
//
//	Load        -> resolve the subfinder binary once and prove it runs (the session resource).
//	enumerate   -> the Method: range over the Batch, run subfinder for each apex, emit per host.
//	              Sequential on purpose — one subfinder per egress address is the placement rule,
//	              so the author's loop is a plain loop and there is no concurrency dial to set.
//	close       -> nothing to release (subfinder is a short-lived subprocess per unit).
//
// WHY A SUBPROCESS, NOT THE LIBRARY. subfinder's contract is its CLI, and shelling to it is the
// same call already made for `docker compose` (cli/infra.go) and axiom: drive the tool that IS
// the contract rather than reimplement it. Importing pkg/runner would pull ProjectDiscovery's
// full dependency tree into this module for no behavioural gain.
//
// SCOPE DISCIPLINE. This actor NEVER invents a domain. It enumerates exactly the apex it is
// given, and every emitted host is asserted to be a subdomain of that apex before it leaves —
// a passive source returning an unrelated host (they do) must not become a scan target.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
	// The actor runtime, imported for its side effect: it registers the Temporal actor
	// host behind a.Serve(). The SDK declares the seam and never imports across it —
	// runtime/ -> sdk/ is one-way — so this line is what puts a host in the binary.
	_ "github.com/medmahmoudi26/kontra/runtime/go"
)

// Target is one input unit: an apex domain to enumerate. `domain` is the bare apex with any
// `*.` prefix already stripped by the producer (the bbscope actor emits it that way).
type Target struct {
	Domain string `json:"domain"`
	// Provenance carried through from the scope asset so a finding can be traced back to the
	// program that published it. Opaque here — copied onto every emitted host.
	Platform   string `json:"platform,omitempty"`
	Program    string `json:"program,omitempty"`
	ProgramURL string `json:"program_url,omitempty"`
}

// Host is one discovered subdomain.
type Host struct {
	Domain     string `json:"domain"`    // the apex it was enumerated from
	Subdomain  string `json:"subdomain"` // the discovered host
	Platform   string `json:"platform,omitempty"`
	Program    string `json:"program,omitempty"`
	ProgramURL string `json:"program_url,omitempty"`
}

// Params are the run-wide dials the dispatcher/UI sees.
type Params struct {
	MaxSubdomains  int  `json:"max_subdomains"`  // hard cap per domain (default 1000)
	TimeoutSeconds int  `json:"timeout_seconds"` // per-domain wall clock (default 300)
	All            bool `json:"all"`             // subfinder -all (every source; slower)
}

const (
	defaultMaxSubdomains = 1000
	defaultTimeoutSecs   = 300
)

func main() {
	a := kontra.New()
	a.Load(loadBinary)
	a.Method("enumerate", enumerate, kontra.Takes(Target{}), kontra.Emits(Host{}))
	a.Healthcheck(binaryAlive)
	a.Close(closeNoop)
	a.Params(Params{})
	a.Serve()
}

// loadBinary resolves subfinder ONCE and proves it executes. Failing here (rather than on the
// first unit) turns a missing binary into a clear session-load error instead of N unit failures.
func loadBinary(s *kontra.Session) error {
	path, err := exec.LookPath("subfinder")
	if err != nil {
		return fmt.Errorf("subfinder not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, path, "-version").CombinedOutput(); err != nil {
		return fmt.Errorf("subfinder -version failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	s.Set("subfinder", path)
	return nil
}

// binaryAlive is THE liveness authority. subfinder is a per-unit SUBPROCESS, not a warm
// in-process engine, so there is no long-lived resource that can die: if the binary is still
// resolvable the session is healthy and a failing unit is a bad DOMAIN, not a dead session.
// Reporting alive here is what makes the framework isolate the unit instead of reloading.
func binaryAlive(s *kontra.Session) (any, error) {
	v, ok := s.Get("subfinder")
	if !ok {
		return nil, errors.New("subfinder path not loaded")
	}
	path, _ := v.(string)
	if path == "" {
		return nil, errors.New("subfinder path empty")
	}
	return "alive", nil
}

func closeNoop(s *kontra.Session) error { return nil }

// enumerate is the Method: the author's loop over the Batch, one subfinder run per apex. A
// failure is attributed to the Unit the loop was on and the Method resumes with the remainder,
// so the naive `return err` is the correct thing to write.
func enumerate(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	for unit := range b.All() {
		if err := enumerateOne(s, unit, ds); err != nil {
			return err
		}
	}
	return b.Err()
}

// enumerateOne runs subfinder for ONE apex and pushes each in-scope host to the output Dataset.
func enumerateOne(s *kontra.Session, unit *kontra.Unit, ds *kontra.Dataset) error {
	apex := normalizeApex(str(unit, "domain"))
	if apex == "" {
		return kontra.NonRetryable("input unit has no usable `domain`")
	}
	v, ok := s.Get("subfinder")
	if !ok {
		return errors.New("subfinder path not available")
	}
	path, _ := v.(string)

	max := intParam(s, "max_subdomains", defaultMaxSubdomains)
	if max <= 0 {
		max = defaultMaxSubdomains
	}
	timeout := intParam(s, "timeout_seconds", defaultTimeoutSecs)
	if timeout <= 0 {
		timeout = defaultTimeoutSecs
	}

	args := []string{"-d", apex, "-silent", "-no-color"}
	if boolParam(s, "all") {
		args = append(args, "-all")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Always reap the process: on the cap-reached path we return before draining stdout, and an
	// unwaited child would leak until the session ends.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	plat, prog, progURL := str(unit, "platform"), str(unit, "program"), str(unit, "program_url")
	seen := make(map[string]struct{}, 256)
	n := 0
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		host := strings.ToLower(strings.TrimSpace(sc.Text()))
		if host == "" {
			continue
		}
		// SCOPE ASSERTION: a passive source can return hosts unrelated to the queried apex
		// (stale certificate-transparency entries do this routinely). Emitting one would put a
		// domain nobody put in scope into the crawl queue, so drop it rather than trust the tool.
		if !isSubdomainOf(host, apex) {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}

		ds.Push(map[string]any{
			"domain": apex, "subdomain": host,
			"platform": plat, "program": prog, "program_url": progURL,
		})
		n++
		if n >= max {
			// Hard cap. Stopping here is deliberate and LOGGED: a silent truncation would read
			// as "this domain only has N subdomains" when it has more.
			fmt.Printf("[subfinder] %s: hit the %d-subdomain cap, stopping enumeration\n", apex, max)
			break
		}
	}
	fmt.Printf("[subfinder] %s: emitted %d subdomains\n", apex, n)
	return nil // the emitted hosts ARE the output
}

// normalizeApex accepts what real scope lists contain — `*.example.com`, `.example.com`,
// `https://example.com/`, `EXAMPLE.com` — and returns the bare apex, or "" if unusable.
func normalizeApex(raw string) string {
	d := strings.ToLower(strings.TrimSpace(raw))
	if d == "" {
		return ""
	}
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	// Strip a LEADING "*." (the only glob form that means "all subdomains of this apex"), then
	// refuse if any glob survives. Order matters: the path must be checked for globs BEFORE it
	// is trimmed, or `example.com/hz/mycd/*` — where the program scoped ONE PATH — would reduce
	// to `example.com` and we would enumerate the entire domain. Caught by TestNormalizeApex.
	d = strings.TrimPrefix(d, "*.")
	d = strings.TrimPrefix(d, ".")
	if strings.Contains(d, "*") {
		return ""
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if i := strings.LastIndex(d, "@"); i >= 0 {
		d = d[i+1:]
	}
	if i := strings.Index(d, ":"); i >= 0 {
		d = d[:i]
	}
	if d == "" || !strings.Contains(d, ".") {
		return ""
	}
	return d
}

// isSubdomainOf reports whether host is apex itself or a label-aligned subdomain of it. The
// suffix check is deliberately label-aware: plain strings.HasSuffix would accept
// "evil-example.com" for apex "example.com".
func isSubdomainOf(host, apex string) bool {
	if host == apex {
		return true
	}
	return strings.HasSuffix(host, "."+apex)
}

func str(u *kontra.Unit, k string) string { return strings.TrimSpace(u.Str(k)) }

// Params is a FIELD on Session (a `map[string]any`, see sdk/go/core.Session), not
// a method — so read dials by key here rather than calling s.Params(...).
func intParam(s *kontra.Session, key string, def int) int {
	if s == nil || s.Params == nil {
		return def
	}
	switch n := s.Params[key].(type) {
	case float64: // JSON numbers arrive as float64
		return int(n)
	case int:
		return n
	}
	return def
}

func boolParam(s *kontra.Session, key string) bool {
	if s == nil || s.Params == nil {
		return false
	}
	b, _ := s.Params[key].(bool)
	return b
}
