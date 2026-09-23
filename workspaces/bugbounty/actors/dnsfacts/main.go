// Package main is dnsfacts: units of {"host": ...} in — a subfinder run's subdomains, a crawl's
// hosts — DNS facts out. Three Methods share the one resolver Load opens; no credential needed.
package main

import (
	"context"
	"net"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
	// The actor runtime, imported for its side effect: it registers the Temporal actor
	// host behind a.Serve(). The SDK declares the seam and never imports across it —
	// runtime/ -> sdk/ is one-way — so this line is what puts a host in the binary.
	_ "github.com/medmahmoudi26/kontra/runtime/go"
)

func main() {
	a := kontra.New()
	a.Load(func(s *kontra.Session) error { s.Set("dns", &net.Resolver{PreferGo: true}); return nil })
	a.Method("addrs", ask(func(r *net.Resolver, c context.Context, h string) (any, error) { return r.LookupHost(c, h) }),
		kontra.Does("Resolve each host to its A/AAAA addresses"))
	a.Method("cname", ask(func(r *net.Resolver, c context.Context, h string) (any, error) { return r.LookupCNAME(c, h) }),
		kontra.Does("Follow each host's CNAME chain to its canonical name"))
	a.Method("ns", ask(func(r *net.Resolver, c context.Context, h string) (any, error) { return r.LookupNS(c, h) }),
		kontra.Does("Read each host's delegated nameservers"))
	a.Serve()
}

// ask makes a Method of one DNS question: every Unit answered off the session's shared resolver,
// one record per host that answers — NXDOMAIN is an ordinary outcome for a passive-source host.
func ask(q func(*net.Resolver, context.Context, string) (any, error)) func(*kontra.Session, *kontra.Batch, *kontra.Dataset) error {
	return func(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
		r, _ := s.Get("dns")
		for unit := range b.All() {
			if answers, err := q(r.(*net.Resolver), context.Background(), unit.Str("host")); err == nil {
				ds.Push(map[string]any{"host": unit.Str("host"), "answers": answers})
			}
		}
		return b.Err()
	}
}
