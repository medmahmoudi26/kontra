package warden

// driver_docker.go — `docker`: sibling of `podman` for the local Compose cluster (ADR 0047).
//
// Docker has no pods. The pair is two containers on one user-defined network, labelled the same
// way the podman driver labels, so `list` reconstructs the Worker from the runtime. The Warden
// itself holds the host Docker socket; Workers never see it.

import (
	"context"
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

func dockerImageEntrypoint(spec Spec) bool {
	return len(spec.Actor.Argv) == 0 && len(spec.Handler.Argv) == 0
}

func (d *dockerDriver) Start(ctx context.Context, spec Spec) (workerHandle, error) {
	digest, err := d.trust.Admit(ctx, spec.Image)
	if err != nil {
		return workerHandle{}, err
	}
	// Join the Compose network the Warden itself is on. A private per-actor network cannot
	// resolve `temporal` / `redis` / `seaweed-s3`. ADR 0047: workers are siblings on that net.
	net := d.net
	if net == "" {
		net = "kontra"
	}
	if dockerImageEntrypoint(spec) {
		return d.startImage(ctx, net, spec, digest)
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

func (d *dockerDriver) startImage(ctx context.Context, net string, spec Spec, digest string) (workerHandle, error) {
	args, err := d.runImageArgs(net, spec, digest)
	if err != nil {
		return workerHandle{}, err
	}
	out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput()
	if err != nil {
		d.removePair(ctx, spec.Name, spec.Version)
		return workerHandle{}, fmt.Errorf("docker run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	ref := cliutil.FirstLine(string(out))
	return workerHandle{
		Driver:  d.driverName(),
		Name:    spec.Name,
		Version: spec.Version,
		Halves: []workerHalf{
			{Part: partActor, Ref: ref},
			{Part: partHandler, Ref: ref},
		},
	}, nil
}

func (d *dockerDriver) runFlags(net, containerName, label, digest string, extraLabels []string) []string {
	flags := []string{
		"run", "--detach", "--rm",
		"--network", net,
		"--name", containerName,
		"--label", workerLabelVar + "=" + label,
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--env", "KONTRA_ACTOR_DIGEST=" + digest,
	}
	for _, l := range extraLabels {
		flags = append(flags, "--label", l)
	}
	// Share the Warden's PID namespace so Pulumi `docker rm -f` (SIGKILL, no
	// SIGTERM) of the Machine also kills sibling Workers. Docker destroy does
	// not run the Warden's shutdown hook.
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		flags = append(flags, "--pid", "container:"+host)
	}
	return flags
}

func (d *dockerDriver) withProc(flags []string, p ProcSpec) []string {
	for _, e := range p.Env {
		flags = append(flags, "--env", e)
	}
	if p.Dir != "" {
		flags = append(flags, "--workdir", p.Dir)
	}
	return flags
}

func (d *dockerDriver) runImageArgs(net string, spec Spec, digest string) ([]string, error) {
	p := spec.Actor
	if len(p.Env) == 0 {
		p = spec.Handler
	}
	flags := d.withProc(d.runFlags(
		net,
		dockerNetwork(spec.Name, spec.Version),
		workerLabel(spec.Name, spec.Version, partActor),
		digest,
		[]string{workerPairLabelVar + "=1"},
	), p)
	if err := assertNoRuntimeAccess(flags); err != nil {
		return nil, err
	}
	return append(flags, spec.Image), nil
}

func (d *dockerDriver) runArgs(net string, spec Spec, part workerPart, p ProcSpec, digest string) ([]string, error) {
	flags := d.withProc(d.runFlags(
		net,
		dockerContainer(spec.Name, spec.Version, part),
		workerLabel(spec.Name, spec.Version, part),
		digest,
		nil,
	), p)
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
	d.stopNames(ctx, secs, h)
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

func (d *dockerDriver) stopNames(ctx context.Context, secs int, h workerHandle) {
	names := map[string]struct{}{
		dockerNetwork(h.Name, h.Version):                   {},
		dockerContainer(h.Name, h.Version, partActor):      {},
		dockerContainer(h.Name, h.Version, partHandler):    {},
	}
	for _, half := range h.Halves {
		if half.Ref != "" {
			names[half.Ref] = struct{}{}
		}
	}
	for name := range names {
		_, _ = exec.CommandContext(ctx, d.bin, "stop", "--time", fmt.Sprint(secs), name).CombinedOutput()
		_, _ = exec.CommandContext(ctx, d.bin, "rm", "--force", name).CombinedOutput()
	}
}

func (d *dockerDriver) removePair(ctx context.Context, name, version string) {
	for _, n := range []string{
		dockerNetwork(name, version),
		dockerContainer(name, version, partActor),
		dockerContainer(name, version, partHandler),
	} {
		_, _ = exec.CommandContext(ctx, d.bin, "rm", "--force", n).CombinedOutput()
	}
}

func (d *dockerDriver) list(ctx context.Context) ([]workerHandle, error) {
	// Docker Desktop's `{{json .Labels}}` is a comma-separated STRING, not a JSON object, so
	// ask for the labels we set. `.Label` is the template function that returns a single value.
	format := "{{.ID}}\t{{.Label \"" + workerLabelVar + "\"}}\t{{.Label \"" + workerPairLabelVar + "\"}}"
	out, err := exec.CommandContext(ctx, d.bin, "ps",
		"--filter", "label="+workerLabelVar,
		"--format", format).Output()
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
		id, rest, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("docker ps line %q: expected id<tab>label", line)
		}
		label, pair, _ := strings.Cut(rest, "\t")
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
		h.Halves = append(h.Halves, workerHalf{Part: part, Ref: id})
		if strings.TrimSpace(pair) != "" && !hasPart(*h, counterpart(part)) {
			h.Halves = append(h.Halves, workerHalf{Part: counterpart(part), Ref: id})
		}
	}
	outH := make([]workerHandle, 0, len(order))
	for _, k := range order {
		outH = append(outH, *byID[k])
	}
	return outH, nil
}

func counterpart(p workerPart) workerPart {
	if p == partActor {
		return partHandler
	}
	return partActor
}

func hasPart(h workerHandle, p workerPart) bool {
	_, ok := h.half(p)
	return ok
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
