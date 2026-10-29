package gate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/peer"
)

// The peer hop (spec/relay.md §11.2, D163, D179): a replica with peer.listen runs a listener of its own, never the
// public one, serving only peer.Path, and advertises its address in gate_replicas.peer_addr with every heartbeat;
// other replicas forward there the MCP exchanges of tunnel sessions this one holds.

// peerSetup reads peer: from the configuration: the shared secret from the environment variable peer.secret_env
// names (at least peer.MinSecret bytes, else the replica refuses to start), the signer and the client. Nil without
// peer.listen.
func peerSetup(cfg *config.Config) (*peer.Signer, *peer.Client, error) {
	if cfg.Peer == nil {
		return nil, nil, nil
	}
	secret := os.Getenv(cfg.Peer.SecretEnv)
	if len(secret) < peer.MinSecret {
		return nil, nil, fmt.Errorf("gate: peer.secret_env: %s holds %d bytes; the shared secret needs at least %d (spec §11.2)",
			cfg.Peer.SecretEnv, len(secret), peer.MinSecret)
	}
	signer, err := peer.NewSigner([]byte(secret))
	if err != nil {
		return nil, nil, err
	}
	client, err := peer.NewClient(signer)
	if err != nil {
		return nil, nil, err
	}
	return signer, client, nil
}

// listenPeer binds peer.listen and works out the address other replicas dial (peer.advertise; else the bound
// address when peer.listen names a host; else the address of the interface that holds the default route, with the
// bound port).
func (r *Replica) listenPeer() error {
	if r.peerSrv == nil {
		return nil
	}
	ln, err := net.Listen("tcp", r.cfg.Peer.Listen)
	if err != nil {
		return fmt.Errorf("gate: peer.listen %s: %w", r.cfg.Peer.Listen, err)
	}
	addr, err := advertise(r.cfg.Peer, ln.Addr())
	if err != nil {
		_ = ln.Close()
		return err
	}
	r.peerLn, r.peerAddr = ln, addr
	r.log.Info("peer listener bound (spec §11.2)", "listen", ln.Addr().String(), "advertise", addr)
	return nil
}

// advertise is the address other replicas reach this one's peer listener at.
func advertise(p *config.Peer, bound net.Addr) (string, error) {
	if p.Advertise != "" {
		return p.Advertise, nil
	}
	_, port, err := net.SplitHostPort(bound.String())
	if err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(p.Listen)
	if err != nil {
		return "", fmt.Errorf("gate: peer.listen: %w", err)
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return net.JoinHostPort(host, port), nil
	}
	ip, err := defaultRouteAddr()
	if err != nil {
		return "", fmt.Errorf("gate: peer.advertise: peer.listen names no host and the default route's address is unknown "+
			"(set peer.advertise): %w", err)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// defaultRouteAddr is the local address of the interface that holds the default route: the source address the
// kernel picks for a UDP "connection" to a public address. Nothing is sent.
func defaultRouteAddr() (net.IP, error) {
	for _, dst := range []string{"192.0.2.1:9", "[2001:db8::1]:9"} { // TEST-NET-1, documentation prefix
		c, err := net.Dial("udp", dst)
		if err != nil {
			continue
		}
		a, ok := c.LocalAddr().(*net.UDPAddr)
		_ = c.Close()
		if ok && a.IP != nil && !a.IP.IsUnspecified() {
			return a.IP, nil
		}
	}
	return nil, errors.New("no route")
}

// servePeer serves the peer listener until ctx ends (Run's background loop).
func (r *Replica) servePeer(ctx context.Context) error {
	if r.peerLn == nil {
		return nil
	}
	stop := context.AfterFunc(ctx, func() { _ = r.peerSrv.Close() })
	defer stop()
	if err := r.peerSrv.Serve(r.peerLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// PeerAddr is the address this replica advertises for its peer listener, "" without peer.listen.
func (r *Replica) PeerAddr() string { return r.peerAddr }

// peerAddrArg is gate_replicas.peer_addr as written: NULL without a peer listener.
func (r *Replica) peerAddrArg() *string {
	if r.peerAddr == "" {
		return nil
	}
	return &r.peerAddr
}

// warnWithoutPeer logs once, when this replica has no peer listener and its heartbeat sees another live replica,
// that a gate of more than one replica SHOULD set peer.listen (spec §11.2, D179): without it a pinned MCP session
// whose request reaches another replica than its tunnel's ends with a 404 and a new initialize.
func (r *Replica) warnWithoutPeer(ctx context.Context) {
	if r.peerAddr != "" || r.peerWarned.Load() {
		return
	}
	var others int
	if err := r.pool.QueryRow(ctx, `select count(*) from kontra.gate_replicas
		 where replica_id <> $1 and last_seen_at > now() - $2::bigint * interval '1 microsecond'`,
		r.id, r.opts.silence.Microseconds()).Scan(&others); err != nil || others == 0 {
		return
	}
	if r.peerWarned.CompareAndSwap(false, true) {
		r.log.Warn("this gate runs "+strconv.Itoa(others+1)+" replicas and this one has no peer.listen: a gate of more than one "+
			"replica SHOULD set it (spec/relay.md §11.2, D179); without it an MCP session pinned to a tunnel another replica "+
			"holds is answered 404 here and re-initialized", "replicas", others+1)
	}
}
