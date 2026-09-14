package warden

// driver_docker.go — `docker`: sibling of `podman` for the local Compose cluster (ADR 0047).
//
// Docker has no pods. The pair is two containers on one user-defined network, labelled the same
// way the podman driver labels, so `list` reconstructs the Worker from the runtime. The Warden
// itself holds the host Docker socket; Workers never see it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
)

type dockerDriver struct {
	bin   string
	net   string
	trust trustpolicy.Policy
}

var _ workerDriver = (*dockerDriver)(nil)

func (d *dockerDriver) driverName() string { return "docker" }

func newDockerDriver(ctx context.Context, trust trustpolicy.Policy) (*dockerDriver, error) {
	bin := "docker"
	if err := exec.CommandContext(ctx, bin, "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		return nil, fmt.Errorf("docker is not reachable (the Warden needs the engine, usually via /var/run/docker.sock): %w", err)
	}
	net := strings.TrimSpace(os.Getenv("KONTRA_DOCKER_NETWORK"))
	if net == "" {
		net = "kontra"
	}
	return &dockerDriver{bin: bin, net: net, trust: trust}, nil
}

func dockerNetwork(name, version string) string { return "kontra-" + name + "-" + version }

func dockerContainer(name, version string, part workerPart) string {
	return dockerNetwork(name, version) + "-" + string(part)
}

func (d *dockerDriver) Start(ctx context.Context, spec Spec) (workerHandle, error) {
	digest, err := d.trust.Admit(ctx, spec.Image)
	if err != nil {
		return workerHandle{}, err
	}
	net := dockerNetwork(spec.Name, spec.Version)
	if out, err := exec.CommandContext(ctx, d.bin, "network", "create", net).CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "already exists") {
			return workerHandle{}, fmt.Errorf("docker network create %s: %v: %s", net, err, strings.TrimSpace(string(out)))
		}
	}
	h := workerHandle{Driver: d.driverName(), Name: spec.Name, Version: spec.Version}
	halves := map[workerPart]ProcSpec{partActor: spec.Actor, partHandler: spec.Handler}
	for _, part := range workerParts {
		p := halves[part]
		args, err := d.runArgs(net, spec, part, p, digest)
		if err != nil {
			d.removePair(ctx, spec.Name, spec.Version)
			return workerHandle{}, err
		}
		out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput()
		if err != nil {
			d.removePair(ctx, spec.Name, spec.Version)
			return workerHandle{}, fmt.Errorf("docker run %s: %v: %s", string(part), err, strings.TrimSpace(string(out)))
		}
		h.Halves = append(h.Halves, workerHalf{Part: part, Ref: cliutil.FirstLine(string(out))})
	}
	return h, nil
}

func (d *dockerDriver) runArgs(net string, spec Spec, part workerPart, p ProcSpec, digest string) ([]string, error) {
	flags := []string{
		"run", "--detach",
		"--network", net,
		"--name", dockerContainer(spec.Name, spec.Version, part),
		"--label", workerLabelVar + "=" + workerLabel(spec.Name, spec.Version, part),
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--env", "KONTRA_ACTOR_DIGEST=" + digest,
	}
	for _, e := range p.Env {
		flags = append(flags, "--env", e)
	}
	if p.Dir != "" {
		flags = append(flags, "--workdir", p.Dir)
	}
	if err := assertNoRuntimeAccess(flags); err != nil {
		return nil, err
	}
	args := append([]string{}, flags...)
	if len(p.Argv) > 0 {
		args = append(args, "--entrypoint", p.Argv[0])
	}
	args = append(args, spec.Image)
	if len(p.Argv) > 1 {
		args = append(args, p.Argv[1:]...)
	}
	return args, nil
}

func (d *dockerDriver) Stop(ctx context.Context, h workerHandle, drain time.Duration) error {
	secs := int(drain.Round(time.Second) / time.Second)
	if drain > 0 && secs == 0 {
		secs = 1
	}
	for _, part := range workerParts {
		name := dockerContainer(h.Name, h.Version, part)
		_, _ = exec.CommandContext(ctx, d.bin, "stop", "--time", fmt.Sprint(secs), name).CombinedOutput()
		_, _ = exec.CommandContext(ctx, d.bin, "rm", "--force", name).CombinedOutput()
	}
	_, _ = exec.CommandContext(ctx, d.bin, "network", "rm", dockerNetwork(h.Name, h.Version)).CombinedOutput()
	hs, err := d.list(ctx)
	if err != nil {
		return err
	}
	for _, got := range hs {
		if got.id() == h.id() {
			return fmt.Errorf("%s is still running after stop", h.id())
		}
	}
	return nil
}

func (d *dockerDriver) removePair(ctx context.Context, name, version string) {
	for _, part := range workerParts {
		_, _ = exec.CommandContext(ctx, d.bin, "rm", "--force", dockerContainer(name, version, part)).CombinedOutput()
	}
	_, _ = exec.CommandContext(ctx, d.bin, "network", "rm", dockerNetwork(name, version)).CombinedOutput()
}

type dockerPS struct {
	ID     string            `json:"ID"`
	Labels map[string]string `json:"Labels"`
}

func (d *dockerDriver) list(ctx context.Context) ([]workerHandle, error) {
	out, err := exec.CommandContext(ctx, d.bin, "ps",
		"--filter", "label="+workerLabelVar,
		"--format", `{"ID":"{{.ID}}","Labels":{{json .Labels}}}`).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	byID := map[string]*workerHandle{}
	order := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row dockerPS
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("docker ps json: %w", err)
		}
		label := row.Labels[workerLabelVar]
		name, version, part, ok := parseWorkerLabel(label)
		if !ok {
			continue
		}
		key := name + "@" + version
		h, exists := byID[key]
		if !exists {
			h = &workerHandle{Driver: d.driverName(), Name: name, Version: version}
			byID[key] = h
			order = append(order, key)
		}
		h.Halves = append(h.Halves, workerHalf{Part: part, Ref: row.ID})
	}
	outH := make([]workerHandle, 0, len(order))
	for _, k := range order {
		outH = append(outH, *byID[k])
	}
	return outH, nil
}

func (d *dockerDriver) logs(ctx context.Context, h workerHandle) (io.ReadCloser, error) {
	if len(h.Halves) == 0 {
		return nil, fmt.Errorf("%s has no running half to take logs from", h.id())
	}
	out, err := exec.CommandContext(ctx, d.bin, "logs", h.Halves[0].Ref).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker logs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return io.NopCloser(strings.NewReader(string(out))), nil
}
