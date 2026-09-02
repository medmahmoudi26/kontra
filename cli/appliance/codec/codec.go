// codec.go — Temporal's remote codec, served by the appliance on a listener of its own, in place
// of the `codec-server` container (ADR 0031 §1: "it is an HTTP handler in a container; it becomes
// an HTTP handler").
//
// THIS PACKAGE KEEPS EXACTLY ONE THING, AND THIS IS IT: Endpoint() reads the address the UI is
// told to call OFF THE LISTENER THAT IS BOUND, instead of it being configured beside the bind.
// That is the whole reason there is a package here rather than eight lines in `kontra up`. The
// algorithm is not ours — it lives in `runtime/handler/codecserver` over `runtime/handler/internal/codec`, which
// is where the Go Worker's own codec comes from, so there is one implementation and no second
// threshold to disagree about.
//
// WHAT IT COST TO NOT HAVE THAT. There was `KONTRA_CODEC_ADDR: ":8234"`, a published host port
// (18234), and `--ui-codec-endpoint http://localhost:18234` on the Temporal service, which had to
// be kept equal to the published one BY HAND: the UI calls the codec FROM THE BROWSER, and a
// browser cannot resolve a compose service name. Three spellings of one port, and the failure
// when they drifted was silent — the UI showed undecodable payloads and said nothing. In one
// process there is one port. The Temporal UI slice is HANDED Endpoint(); it does not configure it,
// and `--ui-codec-endpoint` does not exist any more.
//
// THE CODEC IS A CLAIM-CHECK, NOT ENCRYPTION. It OFFLOADS: a payload over 128 KiB is written to
// the object store and replaced in history by a small ref; a payload under it rides INLINE IN
// WORKFLOW HISTORY IN THE CLEAR, for the namespace's whole retention, readable by anyone who can
// read history — and an offloaded one is just as readable to anyone who can reach the store. No
// size makes a payload secret. That is why a secret travels through history as a NAME, why a
// fleet credential is resolved at the last hop, and why a HITL `ask` redacts its context (ADR
// 0007 §4, ADR 0034 §6). Serving this endpoint changes none of it.
//
// IT READS THE STORE DIRECTLY, not over the S3 API, and that is this package's one import of a
// sibling role — the exception named in appliance.go's import rule. A ref carries a digest and
// not a location, so the codec has to read the store the WRITERS wrote to; in-process the two are
// the same program. The access it needs is written down as `objstore.Backing` rather than taken
// from an unexported field, which is what this split changed: four whole-object operations
// against one bucket. The KEY LAYOUT is what makes it safe to say — cas/<sha[:2]>/<sha> is pinned
// across all three SDKs by shared/conformance/codec/fixtures.json and by objectstore.Store.CasKey.
package codec

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/medmahmoudi26/kontra/runtime/handler/codecserver"
)

// DefaultPort is the port the codec listens on.
//
// 18234 AND NOT 8234, which was the container's own listen port: the compose file published it on
// the host as 18234 precisely because `logdy` binds 8234 on the controller, and the appliance runs
// on that same controller. Publishing there failed the container's start with a bind error while
// the Temporal UI silently fell back to showing undecodable payloads — a loud failure and a quiet
// one for the same collision. It is also the address a browser already points at today, so a
// bookmark and a `temporal --codec-endpoint` line both survive the move.
const DefaultPort = 18234

// Options configures the served codec. Store is the only required field.
type Options struct {
	// Store is where claim-checks live: the appliance's object store as a key -> bytes surface,
	// from objstore.Server.Backing. Required — a codec without it is a passthrough that answers
	// every decode with the `$ref` the UI could already not read.
	//
	// AN INTERFACE AND NOT THE SERVER, so the bucket is resolved by whoever knows which bucket the
	// writers use (KONTRA_S3_BUCKET) rather than defaulted a second time in here.
	Store codecserver.Backing

	// BindIP is the address to listen on; empty means 127.0.0.1.
	//
	// The browser has to reach this. Loopback is right for a workstation, where the UI and the
	// browser are on the same machine; a controller an operator browses to needs the address that
	// operator's browser can resolve, which is the same `--bind` decision every other embedded
	// service here takes.
	BindIP string

	// Port is the codec port; 0 means DefaultPort.
	Port int

	// Prefix is the key prefix the writers use (KONTRA_S3_PREFIX); empty is the common case.
	Prefix string

	// Threshold in bytes; 0 means the codec's own default (KONTRA_S3_THRESHOLD, else 128 KiB) —
	// exactly what the container read, so a threshold an operator already set keeps applying.
	Threshold int

	// UIOrigin is the browser origin CORS admits; empty means codecserver.DefaultUIOrigin. The
	// slice that starts Temporal's Web UI sets this from the UI it started.
	UIOrigin string

	// Logf receives failures the listener could not serve. nil means stderr.
	Logf func(format string, args ...any)
}

// Server is a started codec endpoint. It is returned already listening.
type Server struct {
	srv     *http.Server
	ln      net.Listener
	address string
	origin  string
}

// Address is host:port.
func (c *Server) Address() string { return c.address }

// Endpoint is the full base URL — THE value that used to be `--ui-codec-endpoint`, derived from
// the listener rather than configured beside it. A caller appends nothing: the remote-codec
// contract is this URL plus /encode and /decode, and Temporal's UI and CLI both append their own.
func (c *Server) Endpoint() string { return "http://" + c.address }

// UIOrigin is the browser origin CORS admits, for a caller that wants to report it.
func (c *Server) UIOrigin() string { return c.origin }

// Start boots the codec endpoint and returns once it is serving.
func Start(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("appliance: the codec needs the object store (a codec with no store decodes nothing)")
	}
	if opts.BindIP == "" {
		opts.BindIP = "127.0.0.1"
	}
	if net.ParseIP(opts.BindIP) == nil {
		return nil, fmt.Errorf("appliance: bind address %q is not an IP (use 127.0.0.1, not a hostname)", opts.BindIP)
	}
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "kontra-codec: "+format+"\n", args...) }
	}

	addr := hostPort(opts.BindIP, opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Named, like the other three: an operator whose older compose stack is still up owns 18234
		// through the `codec-server` service that used to publish it, and "the appliance is broken"
		// and "you are running two codecs" are one sentence apart.
		return nil, fmt.Errorf("appliance: cannot listen on %s for the codec — "+
			"another process (an older compose stack's `codec-server`?) has it: %w", addr, err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		addr = hostPort(opts.BindIP, tcp.Port)
	}

	handler := codecserver.New(codecserver.Options{
		Store:     opts.Store,
		Prefix:    opts.Prefix,
		Threshold: opts.Threshold,
		Origin:    opts.UIOrigin,
	})
	c := &Server{ln: ln, address: addr, origin: opts.UIOrigin}
	if c.origin == "" {
		c.origin = codecserver.DefaultUIOrigin
	}
	c.srv = &http.Server{
		Handler: handler,
		// A decode reads one claim-check off local disk, so these are bounded in a way the object
		// store's own listener is not — nothing here streams a multipart upload over a VPC link.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := c.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("listener stopped: %v", err)
		}
	}()
	return c, nil
}

// Stop shuts the listener down, letting in-flight requests finish.
func (c *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.srv.Shutdown(ctx); err != nil {
		return c.srv.Close()
	}
	return nil
}

// hostPort is net.JoinHostPort with an int, as every listening role in this tree spells it. One
// line, copied rather than shared: the package that would hold it for all of them is the parent,
// and the parent imports the roles, not the other way round.
func hostPort(ip string, port int) string { return net.JoinHostPort(ip, strconv.Itoa(port)) }
