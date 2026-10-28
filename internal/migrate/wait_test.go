package migrate

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// Finding robustness-5: kontra-gate migrate made a single attempt, so the compose one-shot failed when it started
// while Postgres still refused TCP (its healthcheck had passed over the socket). WaitReady tries a refused connection
// again every second within its bound, and returns any other failure at once.
func TestWaitReady(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens there now: refused
	m, err := Open(context.Background(), "postgres://kontra:x@"+addr+"/kontra?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	start := time.Now()
	err = m.WaitReady(context.Background(), 2500*time.Millisecond)
	if took := time.Since(start); err == nil || took < 1500*time.Millisecond || took > 10*time.Second {
		t.Fatalf("a refused connection: %v after %v, want an error after retrying for the bound", err, took)
	}

	// A server that answers but is no Postgres: not retried.
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	go func() {
		for {
			c, err := l2.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			_ = c.Close()
		}
	}()
	m2, err := Open(context.Background(), "postgres://kontra:x@"+l2.Addr().String()+"/kontra?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	start = time.Now()
	err = m2.WaitReady(context.Background(), 30*time.Second)
	if took := time.Since(start); err == nil || took > 5*time.Second || !strings.Contains(err.Error(), "migrate: the database") {
		t.Fatalf("a server that is no Postgres: %v after %v, want an error at once", err, took)
	}
}
