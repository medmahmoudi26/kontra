// Command kontrad is the node daemon (D364): it hosts the services on this machine at a gate and lets local clients
// call the gate's services, through one outbound connection (spec/tunnel.md; internal/node, internal/calling,
// internal/stdiobridge). It takes the node's flags directly, with no subcommand; `kontrad version` prints its version.
// It was `kontra connect` until D364; the `kontra` CLI keeps login, publish and seal.
//
//	kontrad [--gate URL] [--serve a2a:<name>=<url> | mcp:<name>=<url> | mcp:<name>=stdio:<cmd> …]
//	        [--listen 127.0.0.1:7777] …
//	kontrad version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/version"
)

const usage = `usage:
  kontrad [--gate <url>] [--serve a2a:<name>=<url> | mcp:<name>=<url> | mcp:<name>=stdio:<cmd> …]
          [--roots <name>=<dir> …] [--env <name> …] [--session-idle <duration>] [--max-sessions <n>]
          [--key <path>] [--enroll <token>|@file|-] [--api-key <key>|@file|-] [--name <name>] [--rotate]
          [--require-channel-binding] [--max-inflight <n>] [--listen <addr>|off] [--local-token <file>]
          [--allow-origin <origin> …] [--sealed-max-age <duration>] [--sealed-required <name> …]
          [--pin-recipients] [--accept-change <name> …] [--public <name> …] [--log json]
  kontrad version

kontrad hosts the services on this machine at a gate, and lets local clients call the gate's services,
through one outbound connection (spec/tunnel.md):
  --gate URL                 the gate's public URL (default https://gateway.kontra.sh); https:// except on localhost
  --serve a2a:NAME=URL       an A2A agent to host, at its root URL; repeatable; none: call only
  --serve mcp:NAME=URL       an MCP server (Streamable HTTP) to host, at its endpoint URL, e.g. http://127.0.0.1:3001/mcp
  --serve 'mcp:NAME=stdio:CMD'
                             an MCP server over stdio: CMD is one argument (quote it), split into words and run without
                             a shell, one process per MCP session
  --roots NAME=DIR           a directory the MCP service NAME may see: kontrad answers its roots/list with these
                             and never passes on a client's roots; repeatable, one directory each (default none)
  --env NAME                 a variable of this environment the stdio servers also get (they get only PATH, HOME, LANG
                             and these); repeatable
  --session-idle DURATION    stop a stdio server whose MCP session has been idle this long (default 1h; at least the
                             gate's session_idle_s)
  --max-sessions N           stdio server processes per service at most, durable tasks included (default 8); when
                             full, a new session stops the one idle longest, if idle for 5 minutes or more
  --key PATH                 the identity key (default ~/.kontra/identity.key, or $KONTRA_KEY)
  --enroll TOKEN|@FILE|-     a one-time enrollment token (ket_…) from the gate's operator: Kontra Cloud's console
                             (Network, Add a node / Add a service), or kontra-gate enroll <service> on a self-hosted
                             gate; repeatable. It binds this node's key to the services it names; to add a service,
                             pass a token naming it and keep every --serve (the key keeps the others)
  --api-key KEY|@FILE|-      an API key for this gate (kk_…): who this node calls as (else node:<key id>); a node that
                             only calls is admitted on it, and only while it is valid. Never a Cloud account key
                             (kontra_sk_…), which is for Cloud's control plane
  --name NAME                the node's name in the operator's view (default the hostname)
  --rotate                   rotate the identity key (the old one stays decrypt-only for 24 h)
  --require-channel-binding  refuse a gate that offers no TLS channel binding
  --max-inflight N           requests in flight per session (default 64)
  --listen ADDR|off          where local clients call through the gate (default 127.0.0.1:7777): /agents/…, /mcp/…,
                             /mcp and /.well-known/… are forwarded with this node's principal; off: no calling, for a
                             node that only hosts
  --local-token FILE         a token every local request must carry (Authorization: Bearer); required off loopback
  --allow-origin ORIGIN      a browser Origin allowed to call --listen; repeatable (default none)
  --sealed-max-age DURATION  how old a sealed request may be when it reaches a hosted service (default 25h; at least
                             the service's task.timeout, with margin)
  --sealed-required NAME     refuse an unsealed request to the hosted service NAME; repeatable
  --pin-recipients           refuse any new recipient or service key of a sealed service this node calls
  --accept-change NAME       accept the recipient or service key change a pin refused for service NAME, the one
                             pending when this run first checks it (not a later one); repeatable
  --public NAME              on an anonymous session, let anyone call the hosted service NAME (no credential); repeatable
  --log json                 JSON logs on stderr (default text)
Credentials are kept per gate in ~/.kontra/credentials (0600) and sent to that gate only. A node that holds none for
the gate and is given neither --enroll nor --api-key asks for an anonymous session, which a gate that opens them
(Cloud's public gate) gives under an anon-<…> tenant for a limited time (spec/tunnel.md §6): kontrad prints its
URLs and when it ends, and exits 1 saying the anonymous session expired when it does. Sealed services
(spec/ext-sealed.md): a hosted job's state is kept under ~/.kontra/hosted, the key of a job this node calls under ~/.kontra/jobs,
and the pins in ~/.kontra/pins; recipient key ids and service key thumbprints (kid:<kid> for a service signature that
names its key only, D223) are printed on stderr on a first sight and on every change.

kontra login keeps an API key for a gate in ~/.kontra/credentials, which kontrad then calls as; kontra publish sends
a listing; kontra seal seals a file without a node (kontra --help).
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process: it returns the exit code (2 for a usage error). `version` is the one positional
// argument kontrad takes; any other, `connect` included, is a usage error (D364).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, version.String(node.Binary))
		return 0
	}
	err := serve(ctx, args, stdout, stderr)
	var uerr *node.UsageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usage)
		return 0
	case errors.As(err, &uerr):
		fmt.Fprintln(stderr, node.Binary+":", tunnel.Printable(err.Error()))
		fmt.Fprintln(stderr, "(kontrad --help for the flags)")
		return 2
	default:
		// A refusal carries the gate's words: never an escape sequence on the operator's terminal.
		fmt.Fprintln(stderr, node.Binary+":", tunnel.Printable(err.Error()))
		return 1
	}
}

// serve runs the node until ctx ends (SIGINT, SIGTERM) or the gate revokes the key. The welcome's service URLs go to
// stdout, the logs to stderr. A command line ParseArgs refused is a *node.UsageError; a handshake the gate refused
// with a code that is not retried, a revoked key and a key file kontrad refuses are errors that exit 1
// (spec/tunnel.md §4).
func serve(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := node.ParseArgs(args, os.Getenv)
	if err != nil {
		return err
	}
	cfg.Version = version.Get()
	cfg.Out, cfg.Err = stdout, stderr
	format := obs.FormatText
	if cfg.LogJSON {
		format = obs.FormatJSON
	}
	logger, err := obs.NewLogger(stderr, format, slog.LevelInfo)
	if err != nil {
		return err
	}
	n, err := node.New(cfg, logger)
	if err != nil {
		return err
	}
	if err := n.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
