// infra.go — `kontra infra up|down|status` over the control-plane compose file.
//
// `docker compose` is the ONE sanctioned shell-out in this CLI: the compose file IS
// the deployment contract for the control plane, so we drive the contract's own tool
// rather than reimplement it against the engine API. Actors are NEVER exec'd or
// shelled to — an actor is run by its language inside its own image (deploy builds
// those via the engine API; dispatch reaches them through Temporal).
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

func cmdInfra(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kontra infra up|down|status [--repo <dir>]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("infra", flag.ContinueOnError)
	repo := fs.String("repo", "", "repo root containing docker-compose.yml (default: walk up from CWD)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	root, err := findRepoRoot(*repo)
	if err != nil {
		return err
	}
	switch sub {
	case "up":
		if err := composeRun(root, "up", "-d"); err != nil {
			return err
		}
		return nil
	case "down":
		return composeRun(root, "down")
	case "status":
		return infraStatus(root)
	default:
		return fmt.Errorf("unknown infra subcommand %q (want up|down|status)", sub)
	}
}

// findRepoRoot walks up from CWD until it sees docker-compose.yml (the control-plane
// contract); --repo overrides. Also used by deploy to locate the base-image context.
func findRepoRoot(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(filepath.Join(override, "docker-compose.yml")); err != nil {
			return "", fmt.Errorf("--repo %s: no docker-compose.yml there", override)
		}
		return override, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no docker-compose.yml found walking up from CWD (pass --repo <dir>)")
		}
		dir = parent
	}
}

// findUp walks up from the working directory looking for a checkout that contains `marker`
// (a repo-relative path), and returns that checkout's root. The depth cap keeps a CLI run from
// a deeply nested directory out of the operator's home or /.
func findUp(marker string) (string, bool) {
	d, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", false
}

// composeRun drives the compose controller (ADR 0034 §1) — the topology this command still owns.
//
// IT NO LONGER INJECTS `KONTRA_HOST_NAME`, and that removal is the point rather than a tidy-up.
// The variable existed to carry THIS box's hostname into `orchestrator-infra`, because the panels
// health check compares a Worker's Temporal identity against a LOCAL pane's node name and
// `os.hostname()` inside a container is the container id (`242b5de62fa4`) — so every local pane
// on this controller read `poller: NONE — its kontra-handler.service is probably down` about a
// handler that was up and polling.
//
// It was a compose-shaped workaround for a compose-shaped topology: a containerised streamer
// beside host-run local Workers. That topology is the local development path `kontra up`
// replaced, and there the streamer is a host process, so `os.hostname()` is simply correct
// (ADR 0031's consequences: "A host process starting a host process needs none of it, nor
// KONTRA_HOST_NAME"). What this container still streams is FLEET panes, whose health never
// consults the host's own name.
//
// The parameter it fed — `hostIsMachine`'s third argument — is untouched and still right; it was
// only ever being handed the wrong name.
func composeRun(dir string, args ...string) error {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// dockerOut runs a docker command and captures trimmed stdout (dir "" = CWD).
func dockerOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func infraStatus(root string) error {
	type check struct {
		name   string
		ok     bool
		detail string
	}

	// compose: any running service container in the control-plane project.
	psOut, psErr := dockerOut(root, "compose", "ps", "-q", "--status", "running")
	running := 0
	if psOut != "" {
		running = len(strings.Split(psOut, "\n"))
	}
	composeDetail := fmt.Sprintf("%d containers running", running)
	if psErr != nil {
		composeDetail = "docker compose ps failed: " + psErr.Error()
	}

	// seaweed's S3 gateway may answer 403 anonymously — ANY HTTP answer means up.
	// Through the same resolver as every other endpoint: this was hardcoded to localhost, and
	// since ports bind `controller:` rather than 0.0.0.0, it reported DOWN on every install that
	// set one — while the store was serving every Dataset on the box perfectly well.
	seaweedURL := s3Endpoint()

	checks := []check{
		{"compose", psErr == nil && running > 0, composeDetail},
		{"orchestrator", httpOK(orchestratorURL() + "/api/health"), orchestratorURL() + "/api/health"},
		{"temporal", tcpUp(temporalAddress()), "tcp " + temporalAddress()},
		{"seaweed", httpAnswers(seaweedURL), seaweedURL},
	}

	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "COMPONENT\tSTATUS\tDETAIL")
	allOK := true
	for _, c := range checks {
		status := "OK"
		if !c.ok {
			status = "DOWN"
			allOK = false
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", c.name, status, c.detail)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if !allOK {
		return errors.New("one or more components are DOWN")
	}
	return nil
}

var statusHTTP = &http.Client{Timeout: 2 * time.Second}

func httpOK(url string) bool {
	resp, err := statusHTTP.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func httpAnswers(url string) bool {
	resp, err := statusHTTP.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func tcpUp(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
