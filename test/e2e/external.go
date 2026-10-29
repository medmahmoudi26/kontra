package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// External is a gate the harness attaches to instead of starting its own (Attach): M3 WP-10's compose team gate,
// whose containers the pentest runs, while the nodes, sellers, stock servers and doubles the harness starts run on
// the host, as a team's machines would (D168).
type External struct {
	URL  string        // public_url, which every node dials and every buyer calls
	Pool *pgxpool.Pool // the gate's database, for the probes' reads
	TC   client.Client // the gate's Temporal, for histories; nil when the run has none
	// ConfigPath is the configuration file as kontra-gate reads it where Gate runs it (enroll -f).
	ConfigPath string
	// Gate runs a short kontra-gate command where the gate runs (docker compose exec) and returns its stdout; its
	// stderr is in the error.
	Gate func(ctx context.Context, args ...string) ([]byte, error)
	// Env is the environment every process the harness starts begins from, before the run's marker (D185: a
	// minimal one); nil is this process's.
	Env []string
	// APIKeyNames name the two API keys Attach mints with `kontra-gate enroll --api-key` (buyer A's and B's).
	APIKeyNames [2]string
}

// Attach returns a harness whose one gate is x's: Gates holds it twice (there is one replica), Front is nil, and
// LaunchNode needs NodeOptions.Gate. Close stops what the harness started (nodes, sellers, doubles) and never the
// gate. Buyer A's and B's keys are minted on the gate.
func Attach(ctx context.Context, x External) (h *Harness, err error) {
	if x.URL == "" || x.Gate == nil || x.ConfigPath == "" {
		return nil, errors.New("e2e: Attach needs the gate's URL, its configuration path and a way to run kontra-gate")
	}
	h = &Harness{run: randomHex(6), ext: &x, Upstreams: map[string]*slowagent.Agent{}, Pool: x.Pool, TC: x.TC, configPath: x.ConfigPath}
	defer func() {
		if err != nil {
			_ = h.Close()
			h = nil
		}
	}()
	root, err := TempRoot()
	if err != nil {
		return h, err
	}
	if h.Dir, err = os.MkdirTemp(root, "run-"+h.run+"-"); err != nil {
		return h, err
	}
	g := &Gate{Name: "compose", URL: x.URL}
	h.Gates = [2]*Gate{g, g}
	names := x.APIKeyNames
	if names[0] == "" {
		names = [2]string{"e2e-a-" + h.run, "e2e-b-" + h.run}
	}
	if h.APIKey, err = h.enroll(ctx, names[0]); err != nil {
		return h, fmt.Errorf("e2e: attach: %w", err)
	}
	if h.APIKeyB, err = h.enroll(ctx, names[1]); err != nil {
		return h, fmt.Errorf("e2e: attach: %w", err)
	}
	return h, nil
}

// KontraBinary is the kontra binary the harness builds once per test binary (the nodes' and kontra seal's).
func KontraBinary() (string, error) { return nodeBinary() }
