// Package kontra_test renders the chart with helm (when it is installed) and checks the values M3 WP-04 adds
// (D183): gate.replicas, refused above 1 with persistence or without the peer hop, and gate.peer, which renders the
// peer port, the shared secret's variable in the gate container only, and NetworkPolicy ingress and egress within
// the release.
package kontra_test

import (
	"os/exec"
	"strings"
	"testing"
)

func helm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	out, err := exec.Command(bin, append([]string{"template", "t", ".", "--set", "config.kontraYaml=x"}, args...)...).CombinedOutput()
	return string(out), err
}

func TestReplicasNeedThePeerHop(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--set", "gate.replicas=2", "--set", "persistence.enabled=true", "--set", "gate.peer.enabled=true",
			"--set", "gate.peer.secret.existingSecret=peer"}, "persistence.enabled"},
		{[]string{"--set", "gate.replicas=2", "--set", "persistence.enabled=false"}, "needs gate.peer.enabled"},
		{[]string{"--set", "gate.peer.enabled=true"}, "gate.peer.secret.existingSecret: required"},
		{[]string{"--set", "gate.replicas=0"}, "at least 1"},
	} {
		out, err := helm(t, tc.args...)
		if err == nil || !strings.Contains(out, tc.want) {
			t.Errorf("%v: %v\n%s", tc.args, err, out)
		}
	}
}

func TestPeerRenders(t *testing.T) {
	out, err := helm(t, "--set", "gate.replicas=2", "--set", "persistence.enabled=false", "--set", "gate.peer.enabled=true",
		"--set", "gate.peer.secret.existingSecret=kontra-peer", "--set", "gate.peer.port=8555")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"replicas: 2", "type: RollingUpdate",
		"- name: peer\n              containerPort: 8555",
		"- name: KONTRA_PEER_SECRET\n              valueFrom:\n                secretKeyRef:\n                  name: kontra-peer\n                  key: secret",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in the render", want)
		}
	}
	if strings.Count(out, "KONTRA_PEER_SECRET") != 1 {
		t.Errorf("the secret's variable is set %d times: the gate container only", strings.Count(out, "KONTRA_PEER_SECRET"))
	}
	// The gate's NetworkPolicy: ingress to the peer port from the release's gate pods, and egress to them.
	np := out[strings.Index(out, "kind: NetworkPolicy"):]
	np = np[:strings.Index(np, "---")]
	ingress := np[strings.Index(np, "ingress:"):strings.Index(np, "egress:")]
	egress := np[strings.Index(np, "egress:"):]
	gatePods := "app.kubernetes.io/component: gate"
	for name, part := range map[string]string{"ingress": ingress, "egress": egress} {
		if !strings.Contains(part, "port: 8555") || !strings.Contains(part, gatePods) {
			t.Errorf("%s has no peer rule:\n%s", name, part)
		}
	}
	// Off by default: no peer port, the render of one replica unchanged.
	out, err = helm(t)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "8444") || strings.Contains(out, "KONTRA_PEER_SECRET") || !strings.Contains(out, "replicas: 1") ||
		!strings.Contains(out, "type: Recreate") {
		t.Error("the default render has a peer port, a peer secret, or not one replica with Recreate")
	}
}
