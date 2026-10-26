package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// The checks the tunnel tests make, run without the harness (M2 review of WP-08: two of them could not
// fail).

// A card the gate serves names public_url, the Front, on an ephemeral port (32768–60999 on Linux), which
// may hold 9999 (39999, 49999, 59999): that is not the seller's address, which is host:9999 only.
func TestNamesSellerIgnoresTheFrontsPort(t *testing.T) {
	card := func(urls ...string) []byte {
		var c struct {
			Name                string               `json:"name"`
			SupportedInterfaces []a2a.AgentInterface `json:"supportedInterfaces"`
		}
		c.Name = "Hello World Agent"
		for _, u := range urls {
			c.SupportedInterfaces = append(c.SupportedInterfaces, a2a.AgentInterface{URL: u, ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2a.Version})
		}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, port := range []string{"39999", "49999", "59999", "41234"} {
		front := "http://127.0.0.1:" + port + "/agents/hello"
		if namesSeller(card(front, front)) {
			t.Errorf("a card served at public_url %s is taken for one naming the seller", front)
		}
	}
	for _, seller := range []string{"http://" + helloAddr, "http://127.0.0.1:9999/", "http://localhost:9999/", "http://[::1]:9999"} {
		if !namesSeller(card("http://127.0.0.1:41234/agents/hello", seller)) {
			t.Errorf("a card naming %s is not taken for one naming the seller", seller)
		}
	}
}

// After a restart, the node's report that it left out a used --enroll token is read from the restarted
// launch's lines only. The first launch's welcome logs "the gate confirmed the enrollment: its tokens are
// not sent again", which must not pass for the restart's own report.
func TestSkippedUsedTokenReadsTheRestartedLaunch(t *testing.T) {
	n := &Node{LogPath: filepath.Join(t.TempDir(), "node-n.log"), OutPath: filepath.Join(t.TempDir(), "node-n.out")}
	appendLine := func(msg string) {
		t.Helper()
		f, err := os.OpenFile(n.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b, _ := json.Marshal(map[string]string{"level": "INFO", "msg": msg})
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.markLaunch(); err != nil { // the first launch: no log yet
		t.Fatal(err)
	}
	appendLine("kontra connect starting")
	appendLine("the gate confirmed the enrollment: its tokens are not sent again")
	if err := n.markLaunch(); err != nil { // the restart
		t.Fatal(err)
	}
	appendLine("kontra connect starting")
	if skippedUsedToken(n) {
		t.Fatal("the first launch's confirmation passed for the restarted launch's report")
	}
	if got := n.LaunchLog("kontra connect starting"); len(got) != 1 {
		t.Fatalf("LaunchLog read %d lines of the restarted launch's one", len(got))
	}
	if got := n.Log("kontra connect starting"); len(got) != 2 {
		t.Fatalf("Log read %d lines of both launches' two", len(got))
	}
	appendLine("an --enroll token this node already enrolled with at this gate is not sent again")
	if !skippedUsedToken(n) {
		t.Fatal("the restarted launch's report was not read")
	}
}
