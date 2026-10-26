package e2e

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"testing"
	"time"
)

// Front.LoseWelcome, without the harness: the node's side gets everything up to the welcome and then ends,
// while the gate's side stays open, its writes accepted and nothing sent back, until the gate closes it.
func TestFrontLosesTheWelcome(t *testing.T) {
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer gl.Close()
	f, err := NewFront()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Route(&Gate{Name: "A", Addr: gl.Addr().String()})
	f.LoseWelcome(1)

	const session = "0192f0b4-0000-7000-8000-000000000001"
	node, err := net.Dial("tcp", f.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	gate, err := gl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	nr := bufio.NewReader(node)
	gr := bufio.NewReader(gate)
	line := func(r *bufio.Reader, who string) string {
		t.Helper()
		s, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("%s read: %v", who, err)
		}
		return s
	}

	// Both ways until the welcome: the challenge reaches the node, the hello and the proof the gate.
	write(t, node, "hello\n")
	if got := line(gr, "the gate"); got != "hello\n" {
		t.Fatalf("the gate read %q", got)
	}
	write(t, gate, `{"t":"challenge","v":1}`+"\n")
	if got := line(nr, "the node"); got != `{"t":"challenge","v":1}`+"\n" {
		t.Fatalf("the node read %q", got)
	}
	write(t, node, "proof\n")
	if got := line(gr, "the gate"); got != "proof\n" {
		t.Fatalf("the gate read %q", got)
	}

	// The welcome, split across two writes: the node's side ends without it.
	write(t, gate, `{"t":"wel`)
	time.Sleep(50 * time.Millisecond)
	write(t, gate, `come","v":1,"session":"`+session+`","ping_s":20}`+"\n")
	_ = node.SetReadDeadline(time.Now().Add(5 * time.Second))
	rest, err := io.ReadAll(nr)
	if err != nil {
		t.Fatalf("the node's side did not end: %v (read %q)", err, rest)
	}
	if len(rest) >= len(welcomeMarker) {
		t.Fatalf("the node read %q after the proof", rest)
	}
	eventually(t, 5*time.Second, "the lost welcome's session id recorded", func() bool { return slices.Equal(f.LostWelcomes(), []string{session}) })

	// The gate's side is held open: its writes are taken, and it reads nothing, not even an end.
	write(t, gate, "yamux frames\n")
	_ = gate.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := gr.ReadByte(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the gate's side read %v, want nothing (held open)", err)
	}

	// Only the next connection loses its welcome.
	node2, err := net.Dial("tcp", f.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer node2.Close()
	gate2, err := gl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer gate2.Close()
	welcome := `{"t":"welcome","v":1,"session":"0192f0b4-0000-7000-8000-000000000002","ping_s":20}` + "\n"
	write(t, gate2, welcome)
	_ = node2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got := line(bufio.NewReader(node2), "the second node"); got != welcome {
		t.Fatalf("the second connection's node read %q, want the welcome", got)
	}

	// The gate closes the held connection: the Front lets go of it.
	gate.Close()
	eventually(t, 5*time.Second, "the Front lets go of the held connection", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.conns) == 2 // the second connection's two ends
	})
}

func write(t *testing.T, c net.Conn, s string) {
	t.Helper()
	if _, err := io.WriteString(c, s); err != nil {
		t.Fatal(err)
	}
}
