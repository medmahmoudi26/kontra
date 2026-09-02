// doctor_test.go — the pure bits of `kontra doctor`: host extraction and table
// rendering. Live probing is covered by running it against the control plane.
package main

import (
	"strings"
	"testing"
)

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8088":     "localhost",
		"https://orch.example:8443": "orch.example",
		"localhost:7233":            "localhost",
		"10.0.0.5:7233":             "10.0.0.5",
		"http://box/":               "box",
		"":                          "localhost",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderDoctor(t *testing.T) {
	services := []svcRow{
		{"control plane", "docker compose containers", "-", true, "4 running"},
		{"orchestrator", "graph API + interpreter", "http://localhost:8088", true, ""},
		{"temporal", "workflow engine (gRPC)", "localhost:7233", false, ""},
	}
	consoles := []uiRow{
		{"Temporal", "http://localhost:8233", "workflow executions & history"},
	}
	out := withStdout(t, func() { renderDoctor(services, consoles) })
	for _, want := range []string{
		"Services", "COMPONENT", "control plane", "4 running",
		"orchestrator", "http://localhost:8088", "up",
		"temporal", "DOWN",
		"Web consoles", "Temporal", "http://localhost:8233",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderDoctor output missing %q in:\n%s", want, out)
		}
	}
}

func TestUIHelper(t *testing.T) {
	if got := ui("localhost", "8233"); got != "http://localhost:8233" {
		t.Errorf("ui() = %q", got)
	}
}
