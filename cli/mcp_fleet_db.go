package main

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// MCP parity for `kontra fleet` and `kontra db`.
//
// The MCP had 14 tools and neither family, so an agent driving kontra could deploy an actor and
// dispatch it but could not create the machines to run it on, nor see the operator datasets.
// `list_datasets` looks like it covers the second, but it lists per-RUN materialized output —
// asking it for the DuckLake datasets (scope_paid, axon_attribution, …) returns run ids instead,
// which is worse than a missing tool because it answers the wrong question convincingly.
//
// Every tool here CALLS THE CLI SUBCOMMAND and captures its output, rather than reimplementing
// it. That is what keeps the two surfaces from drifting, and it is why the fleet tools needed
// no rewrite of their own when provisioning moved from OpenTofu+Ansible to the orchestrator's
// Pulumi control plane (ADR 0019): the CLI moved, and these followed.
//
// Provider credentials are never read here, for a stronger reason than before: the CLI does not
// hold one either. The cloud credential lives only in orchestrator-infra, on the Controller.

// captureCLI runs a CLI subcommand with stdout redirected into a buffer, so an MCP caller gets
// exactly what a human would read in a terminal instead of a bare exit code.
func captureCLI(fn func([]string) error, args ...string) (string, error) {
	old := cliio.Stdout
	var buf bytes.Buffer
	cliio.Stdout = &buf
	defer func() { cliio.Stdout = old }()
	err := fn(args)
	return buf.String(), err
}

func mcpFleetUp(count int, tag, fleet, actorDir string) (string, error) {
	if count < 1 {
		return "", fmt.Errorf("count must be >= 1")
	}
	args := []string{"--count", strconv.Itoa(count)}
	if tag != "" {
		if err := validTag(tag); err != nil {
			return "", err
		}
		args = append(args, "--tag", tag)
	}
	if fleet != "" {
		args = append(args, "--fleet", fleet)
	}
	if actorDir != "" {
		args = append(args, "--actor", actorDir)
	}
	return captureCLI(fleetUp, args...)
}

// mcpFleetDeploy places an Artifact on the Fleet. The old signature took a `tag` and enforced
// that it ended in a git SHA; that check now belongs to the thing that BUILDS the artifact, and
// what is placed here is a full image reference which is content-pinned by construction.
func mcpFleetDeploy(tag, fleet, actorDir, image string) (string, error) {
	if tag != "" {
		if err := validTag(tag); err != nil {
			return "", err
		}
	}
	if actorDir == "" && image == "" {
		return "", fmt.Errorf("nothing to place: pass actor_dir or image")
	}
	args := []string{}
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	if fleet != "" {
		args = append(args, "--fleet", fleet)
	}
	if actorDir != "" {
		args = append(args, "--actor", actorDir)
	}
	if image != "" {
		args = append(args, "--image", image)
	}
	return captureCLI(fleetDeployCmd, args...)
}

func mcpFleetStatus(fleet string) (string, error) {
	args := []string{}
	if fleet != "" {
		args = append(args, "--fleet", fleet)
	}
	return captureCLI(fleetStatus, args...)
}

// mcpFleetLeases is `kontra fleet leases` — who is holding this Fleet (ADR 0037).
//
// IT IS THE TOOL THAT MAKES `fleet_down` READABLE. `down` refuses a held Fleet, and an agent that
// cannot see the holders has only the refusal text to go on. There is deliberately no `force` here:
// forcing a shared Fleet down deletes another Run's Machines, which is a person's decision.
func mcpFleetLeases(fleet string) (string, error) {
	args := []string{}
	if fleet != "" {
		args = append(args, "--fleet", fleet)
	}
	return captureCLI(fleetLeases, args...)
}

func mcpFleetDown(fleet string) (string, error) {
	args := []string{}
	if fleet != "" {
		args = append(args, "--fleet", fleet)
	}
	return captureCLI(fleetDown, args...)
}

// mcpDBList reuses the CLI's dbList by capturing cliio.Stdout — the DuckLake datasets, which are a
// different thing from list_datasets' per-run output.
func mcpDBList(catalog, dataPath string) (string, error) {
	old := cliio.Stdout
	var buf bytes.Buffer
	cliio.Stdout = &buf
	defer func() { cliio.Stdout = old }()
	err := dbList(catalog, dataPath)
	return buf.String(), err
}

func mcpDBIngest(catalog, dataPath, file, name string, anew bool) (string, error) {
	old := cliio.Stdout
	var buf bytes.Buffer
	cliio.Stdout = &buf
	defer func() { cliio.Stdout = old }()
	err := dbIngest(catalog, dataPath, file, name, anew)
	return buf.String(), err
}

func mcpDBDelete(catalog, dataPath, name string) (string, error) {
	old := cliio.Stdout
	var buf bytes.Buffer
	cliio.Stdout = &buf
	defer func() { cliio.Stdout = old }()
	err := dbDelete(catalog, dataPath, name)
	return buf.String(), err
}
