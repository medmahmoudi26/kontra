// api.go — env defaults, the orchestrator HTTP client, and the catalog wire types.
//
// The graph wire types (nodeSpec/graphSpec) and runStart went with the dispatch verb: they
// described the body of `POST /api/runs {graph}` and the ids it answered with, and that route
// now takes only a caller's workflow (ADR 0023 §12).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/config"
)

// Env knobs — the single source of connection defaults; every command reads these.
const (
	defaultAPI = "http://localhost:8088"
	defaultS3  = "http://localhost:8333"
)

// THE TEMPORAL ADDRESS AND NAMESPACE DEFAULTS ARE IN `internal/config`, because both halves of the
// CLI answer with them: every control-plane command dials them, and the Warden's `ca` subcommands
// take them as flag defaults. Two copies would be two answers to "what does an unconfigured
// installation talk to".

/**
 * The control plane's address, when this installation has said where that is.
 *
 * `localhost` IS NOT A SAFE DEFAULT ANY MORE, and that is a consequence of closing the ports.
 * Every service used to publish on 0.0.0.0, so `localhost:7233` happened to reach Temporal on the
 * control machine. Now they bind an address — `${KONTRA_BIND}`, derived from `controller:` — and on
 * a machine where that is the VPC address, NOTHING listens on loopback.
 *
 * MEASURED on a freshly installed controller: `kontra workflow start` died with
 *   failed reaching server: dial tcp [::1]:7233: connect: connection refused
 * — `localhost` resolving to the IPv6 loopback, against a Temporal bound to 10.116.0.2. Every
 * command that talks to Temporal was broken on a correctly-secured machine, and the error names an
 * address the operator never chose, which is the worst kind: it reads as "Temporal is down".
 *
 * So the fallback chain is: the explicit variable, then `controller:` from config.yaml, then
 * localhost. The middle step is the one that makes a secured install work out of the box, and it
 * is the same single fact `KONTRA_BIND` and `KONTRA_REDIS_BIND` are derived from — where this
 * installation's control plane lives.
 *
 * Config read failures fall through to localhost rather than erroring: a CI box or a container
 * with no `.kontra/` is environment-only by design (see `config.LoadConfig`), and a connection default is
 * not the place to start refusing to run.
 */

func orchestratorURL() string {
	if v := os.Getenv("KONTRA_ORCHESTRATOR_URL"); v != "" {
		return v
	}
	if host := config.Controller(); host != "" {
		return "http://" + host + ":8088"
	}
	return defaultAPI
}

// s3Endpoint is the object store, resolved the same way and for the same reason. `kontra build
// used to push the Bundle here, and it died with
//
//	creating bucket kontra-bundles: dial tcp [::1]:8333: connect: connection refused
//
// on a controller whose SeaweedFS binds the VPC address — the third command in a row to fail on a
// hard-coded `localhost`, which is what turned this into one resolver instead of three patches.
// That caller is gone (a Bundle is an OCI artifact now, ADR 0036) and the measurement is kept,
// because `bundle.go:bundleRegistry` is the same rung ladder for the registry that replaced it.
func s3Endpoint() string {
	if v := os.Getenv("KONTRA_S3_ENDPOINT"); v != "" {
		return v
	}
	if host := config.Controller(); host != "" {
		return "http://" + host + ":8333"
	}
	return defaultS3
}

// stdout/stderr/stdin MOVED TO `internal/cliio`, and every use is now `stdout` and friends.
//
// They were `var stdout io.Writer = os.Stdout` here, which works while there is one package. The
// Warden is its own package now and writes to the same streams; two copies would mean a test that
// captures output sees one of them and silently misses the other.

// queues.Shared moved to identity.go, beside workflowQueue: an Actor's queue and a Workflow's are
// the two answers to one question, and the `wf-` prefix that keeps them from colliding is only
// legible when both derivations are in view.

// --- wire types (backend/types.ts names, verbatim) ---

// actorRecord is one row of GET /api/actors (the catalog).
type actorRecord struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Operations []struct {
		Name string `json:"name"`
	} `json:"operations"`
	Digest string `json:"digest"`
	// Runtime is what this version's image was BUILT on — read, not written, by this side. `kontra
	// rebase` compares the digest recorded here against the current digest of `<name>:<major>`, and a
	// difference is the whole of rebase detection.
	Runtime *struct {
		Name   string `json:"name"`
		Major  uint32 `json:"major"`
		Digest string `json:"digest"`
	} `json:"runtime"`
	// History is the digests this version has already had, newest first. Absent until a rebase.
	History []string `json:"history"`
}

// --- HTTP client ---

// httpError keeps the status separable from transport errors — callers branch on 404.
type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.body) }

type apiClient struct {
	base string
	http *http.Client
	// bearer is sent as `Authorization: Bearer …` when set. Only the token-gated read
	// surfaces need it (explore presigns URLs, raw state exposes actor state); the rest of
	// this API predates admission control and is unauthenticated — see control/orchestrator/src/auth.ts.
	bearer string
}

func newAPI(base string) *apiClient {
	return &apiClient{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 60 * time.Second}}
}

// newAuthAPI is newAPI for the token-gated surfaces. Those FAIL CLOSED server-side, so the
// token is part of reaching them at all, not an optional hardening.
func newAuthAPI(base, token string) *apiClient {
	a := newAPI(base)
	a.bearer = token
	return a
}

func (a *apiClient) getJSON(path string, out any) error { return a.do(http.MethodGet, path, nil, out) }

func (a *apiClient) postJSON(path string, body, out any) error {
	return a.do(http.MethodPost, path, body, out)
}

func (a *apiClient) putJSON(path string, body, out any) error {
	return a.do(http.MethodPut, path, body, out)
}

func (a *apiClient) deleteJSON(path string, out any) error {
	return a.do(http.MethodDelete, path, nil, out)
}

func (a *apiClient) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+a.bearer)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the orchestrator at %s — is it up? (`kontra infra status`): %v", a.base, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out != nil {
		// A *string out means the caller wants the body VERBATIM. One route answers markdown — the
		// Scratch spec, written to be read by a person or an agent rather than parsed — and
		// wrapping it in JSON only to unwrap it would be a round trip through a format neither end
		// wanted.
		if sp, ok := out.(*string); ok {
			*sp = string(b)
			return nil
		}
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("%s %s: bad JSON response: %w", method, path, err)
		}
	}
	return nil
}
