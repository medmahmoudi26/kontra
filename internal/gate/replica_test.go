package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tenant"
)

// The drain deletes the replica's gate_replicas row last, while the heartbeat loop still runs (it beats
// through the drain, which outlasts ReplicaSilence). No heartbeat may bring the row back afterwards: a dead
// replica's row would stay until another replica reaps it, or until the next start on a gate of one, and
// count in D72's N meanwhile.
func TestNoHeartbeatAfterDeregister(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	r := &Replica{pool: pool, id: "gate-test-0001", log: slog.Default(),
		opts: options{beatEvery: 25 * time.Millisecond, silence: ReplicaSilence}}
	if err := r.register(ctx); err != nil {
		t.Fatal(err)
	}
	rows := func() int {
		t.Helper()
		return scalar[int](t, pool, `select count(*) from kontra.gate_replicas where replica_id = $1`, r.id)
	}
	loop, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = r.replicaLoop(loop) }()
	defer func() { stop(); <-done }()

	time.Sleep(60 * time.Millisecond) // a few beats
	r.draining.Store(true)
	if err := r.markDraining(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.deregister(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // the loop keeps ticking
	if n := rows(); n != 0 {
		t.Fatalf("a heartbeat after deregister brought the replica's row back (%d rows)", n)
	}
	// Nor the drain's own beat (markDraining), nor a beat in flight.
	if err := r.beat(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 0 {
		t.Fatalf("a beat after deregister: %d rows", n)
	}
}

// The refused-patch metric of spec §13.3 in the Prometheus text format: a counter per source and refusal.
func TestPatchRefusalsMetric(t *testing.T) {
	var b strings.Builder
	writePatchRefusals(&b, "r1", nil)
	if got := b.String(); !strings.Contains(got, "# TYPE kontra_gate_operator_patches_refused_total counter") || strings.Contains(got, "{") {
		t.Errorf("no refusal yet: %q", got)
	}
	var p lifecycle.PatchRefusals
	p.Add(lifecycle.HookOnTerminal, "too_large")
	p.Add(lifecycle.UpdateAnnotate, "forbidden_key")
	p.Add(lifecycle.HookOnTerminal, "too_large")
	b.Reset()
	writePatchRefusals(&b, "r1", &p)
	want := "kontra_gate_operator_patches_refused_total{replica=\"r1\",source=\"kontra.annotate\",refusal=\"forbidden_key\"} 1\n" +
		"kontra_gate_operator_patches_refused_total{replica=\"r1\",source=\"kontra.lifecycle.OnTerminal\",refusal=\"too_large\"} 2\n"
	if got := b.String(); !strings.HasSuffix(got, want) {
		t.Errorf("metric:\n%s\nwant it to end with\n%s", got, want)
	}
}

// The reaper (spec/tunnel.md §11, D179): mcp_sessions.replica_id names the replica holding the tunnel session, so
// reaping the replica a client reached leaves its session open while the holder lives; a row whose holder is reaped
// closes tunnel_lost; a direct backend's row (replica_id NULL) is never closed by the reaper.
func TestReaperClosesOnlyTheHoldersSessions(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	team, err := tenant.EnsureTeam(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source) values
		($1, 'fs', 'mcp', '{"tunnel": {}}', 'config'), ($1, 'web', 'mcp', '{"mcp": {"url": "https://mcp.example.com/mcp"}}', 'config')`, team); err != nil {
		t.Fatal(err)
	}
	// "receiver" (the replica a client reached) and "dead-holder" are silent; "live-holder" is not.
	if _, err := pool.Exec(ctx, `insert into kontra.gate_replicas (replica_id, version, last_seen_at) values
		('receiver', 't', now() - interval '1 minute'), ('dead-holder', 't', now() - interval '1 minute'), ('live-holder', 't', now())`); err != nil {
		t.Fatal(err)
	}
	open := func(name, service string, replica *string) [32]byte {
		h := sha256.Sum256([]byte(name))
		var tun *uuid.UUID
		if replica != nil {
			u := uuid.New()
			tun = &u
		}
		if _, err := pool.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via,
			tunnel_session, replica_id, protocol_version) values ($1, $2, $3, $4, 'key:kk_t_x', 'api_key', $5, $6, '2025-11-25')`,
			h[:], team, service, uuid.New(), tun, replica); err != nil {
			t.Fatal(err)
		}
		return h
	}
	live, dead := "live-holder", "dead-holder"
	held := open("held by a live replica, reached through the dead one", "fs", &live)
	lost := open("held by the dead replica", "fs", &dead)
	direct := open("a direct backend", "web", nil)
	r := &Replica{pool: pool, id: "reaper", log: slog.Default(), opts: options{silence: ReplicaSilence}}
	gone, err := r.reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(gone)
	if strings.Join(gone, ",") != "dead-holder,receiver" {
		t.Errorf("reaped %v", gone)
	}
	state := func(h [32]byte) string {
		return scalar[string](t, pool, `select case when closed_at is null then 'open' else close_reason end
			from kontra.mcp_sessions where client_sid_hash = $1`, h[:])
	}
	if s := state(held); s != "open" {
		t.Errorf("a session whose holder lives: %s", s)
	}
	if s := state(lost); s != "tunnel_lost" {
		t.Errorf("a session whose holder was reaped: %s", s)
	}
	if s := state(direct); s != "open" {
		t.Errorf("a direct backend's session: %s", s)
	}
}

// A replica without peer.listen that sees another live replica logs relay §11.2's SHOULD once (D179); one with a
// peer listener, or alone, logs nothing. The heartbeat writes the advertised peer address.
func TestMultiReplicaWarningAndPeerAddr(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil))
	r := &Replica{pool: pool, id: "gate-a", log: log, opts: options{silence: ReplicaSilence}}
	if err := r.register(ctx); err != nil {
		t.Fatal(err)
	}
	r.warnWithoutPeer(ctx)
	if strings.Contains(buf.String(), "SHOULD") {
		t.Fatal("warned with one replica")
	}
	if _, err := pool.Exec(ctx, `insert into kontra.gate_replicas (replica_id, version) values ('gate-b', 't')`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.warnWithoutPeer(ctx)
	}
	mu.Lock()
	n := strings.Count(buf.String(), "SHOULD set it")
	mu.Unlock()
	if n != 1 {
		t.Errorf("warned %d times:\n%s", n, buf.String())
	}
	if scalar[*string](t, pool, `select peer_addr from kontra.gate_replicas where replica_id = 'gate-a'`) != nil {
		t.Error("a replica without peer.listen advertises an address")
	}
	// With a peer listener: no warning, and the heartbeat writes its address.
	buf.Reset()
	p := &Replica{pool: pool, id: "gate-c", log: log, opts: options{silence: ReplicaSilence}, peerAddr: "10.0.0.7:8444"}
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	p.warnWithoutPeer(ctx)
	if strings.Contains(buf.String(), "SHOULD") {
		t.Error("a replica with a peer listener warned")
	}
	if _, err := pool.Exec(ctx, `update kontra.gate_replicas set peer_addr = null where replica_id = 'gate-c'`); err != nil {
		t.Fatal(err)
	}
	if err := p.beat(ctx); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `select peer_addr from kontra.gate_replicas where replica_id = 'gate-c'`); got != "10.0.0.7:8444" {
		t.Errorf("peer_addr %q after a beat", got)
	}
}

// peer.advertise: explicit; else the bound address when peer.listen names a host; else the default route's address
// with the bound port.
func TestPeerAdvertise(t *testing.T) {
	bound := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5555}
	for _, tc := range []struct {
		p    config.Peer
		want string
	}{
		{config.Peer{Listen: "127.0.0.1:0", Advertise: "gate-0.gate:8444"}, "gate-0.gate:8444"},
		{config.Peer{Listen: "127.0.0.1:0"}, "127.0.0.1:5555"},
		{config.Peer{Listen: "gate.internal:8444"}, "gate.internal:5555"},
	} {
		if got, err := advertise(&tc.p, bound); err != nil || got != tc.want {
			t.Errorf("%+v: %q %v", tc.p, got, err)
		}
	}
	for _, listen := range []string{":0", "0.0.0.0:0", "[::]:0"} {
		got, err := advertise(&config.Peer{Listen: listen}, bound)
		if err != nil {
			t.Logf("%s: no default route here: %v", listen, err)
			continue
		}
		host, port, _ := net.SplitHostPort(got)
		if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() || port != "5555" {
			t.Errorf("%s: %q", listen, got)
		}
	}
}

// peer: needs a secret of at least 32 bytes from the variable it names.
func TestPeerSetup(t *testing.T) {
	cfg := &config.Config{Peer: &config.Peer{Listen: "127.0.0.1:0", SecretEnv: "KONTRA_TEST_PEER_SECRET_X"}}
	t.Setenv("KONTRA_TEST_PEER_SECRET_X", strings.Repeat("s", 31))
	if _, _, err := peerSetup(cfg); err == nil {
		t.Error("a 31-byte secret was accepted")
	}
	t.Setenv("KONTRA_TEST_PEER_SECRET_X", strings.Repeat("s", 32))
	if s, c, err := peerSetup(cfg); err != nil || s == nil || c == nil {
		t.Errorf("%v", err)
	}
	if s, c, err := peerSetup(&config.Config{}); err != nil || s != nil || c != nil {
		t.Errorf("without peer: %v", err)
	}
}
