// Command kontra is the node CLI: connect, login, seal, publish (note 2 §1; there is no local console, D164).
//
//	kontra connect [--gate URL] [--serve a2a:<name>=<url> | mcp:<name>=<url> | mcp:<name>=stdio:<cmd> …]
//	               [--listen 127.0.0.1:7777] … (M2, M3; spec/tunnel.md; internal/node, internal/hiring,
//	               internal/stdiobridge)
//	kontra seal --service <service URL> [--api-key <key>] [--context <id>] [--task <id>] [file | -],
//	kontra seal --open <part.json>
//	(spec/ext-sealed.md §5.1; internal/sealing)
//	kontra version
//
// The other subcommands come with their milestones (PRD §14, note 2 §4): login with the anonymous sessions on
// the public gate (M4), publish with Cloud's listings (M5).
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
	"kontra.sh/kontra/internal/sealing"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/version"
)

const usage = `usage:
  kontra connect [--gate <url>] [--serve a2a:<name>=<url> | mcp:<name>=<url> | mcp:<name>=stdio:<cmd> …]
                 [--roots <name>=<dir> …] [--env <name> …] [--session-idle <duration>] [--max-sessions <n>]
                 [--key <path>] [--enroll <token>|@file|-] [--api-key <key>|@file|-] [--name <name>] [--rotate]
                 [--require-channel-binding] [--max-inflight <n>] [--listen <addr>|off] [--local-token <file>]
                 [--allow-origin <origin> …] [--sealed-max-age <duration>] [--sealed-required <name> …]
                 [--pin-recipients] [--accept-change <name> …] [--log json]
  kontra seal --service <service URL> [--api-key <key>|@<file>] [--context <id>] [--task <id>]
              [--media-type <type>] [file | -]
  kontra seal --open <part.json>
  kontra version

kontra connect hosts the services on this machine at a gate, and lets local clients hire the gate's services,
through one outbound connection (spec/tunnel.md):
  --gate URL                 the gate's public URL (default https://gateway.kontra.sh); https:// except on localhost
  --serve a2a:NAME=URL       an A2A agent to host, at its root URL; repeatable; none: hire only
  --serve mcp:NAME=URL       an MCP server (Streamable HTTP) to host, at its endpoint URL, e.g. http://127.0.0.1:3001/mcp
  --serve 'mcp:NAME=stdio:CMD'
                             an MCP server over stdio: CMD is one argument (quote it), split into words and run without
                             a shell, one process per MCP session
  --roots NAME=DIR           a directory the MCP service NAME may see: kontra connect answers its roots/list with these
                             and never passes on a client's roots; repeatable, one directory each (default none)
  --env NAME                 a variable of this environment the stdio servers also get (they get only PATH, HOME, LANG
                             and these); repeatable
  --session-idle DURATION    stop a stdio server whose MCP session has been idle this long (default 1h; at least the
                             gate's session_idle_s)
  --max-sessions N           stdio server processes per service at most, durable tasks included (default 8); when
                             full, a new session stops the one idle longest, if idle for 5 minutes or more
  --key PATH                 the identity key (default ~/.kontra/identity.key, or $KONTRA_KEY)
  --enroll TOKEN|@FILE|-     a one-time enrollment token from kontra-gate enroll <service>; repeatable
  --api-key KEY|@FILE|-      an API key for this gate: who this node hires as (else node:<key id>); a node that only
                             hires is admitted on it, and only while it is valid
  --name NAME                the node's name in the operator's view (default the hostname)
  --rotate                   rotate the identity key (the old one stays decrypt-only for 24 h)
  --require-channel-binding  refuse a gate that offers no TLS channel binding
  --max-inflight N           requests in flight per session (default 64)
  --listen ADDR|off          where local clients hire through the gate (default 127.0.0.1:7777): /agents/…, /mcp/…,
                             /mcp and /.well-known/… are forwarded with this node's principal; off: no hiring, for a
                             node that only hosts
  --local-token FILE         a token every local request must carry (Authorization: Bearer); required off loopback
  --allow-origin ORIGIN      a browser Origin allowed to call --listen; repeatable (default none)
  --sealed-max-age DURATION  how old a sealed request may be when it reaches a hosted service (default 25h; at least
                             the gate's longest quote expiry plus task.timeout)
  --sealed-required NAME     refuse an unsealed request to the hosted service NAME; repeatable
  --pin-recipients           refuse any new recipient or seller key of a sealed service this node hires
  --accept-change NAME       accept the recipient or seller key change a pin refused for service NAME, the one
                             pending when this run first checks it (not a later one); repeatable
  --log json                 JSON logs on stderr (default text)
Credentials are kept per gate in ~/.kontra/credentials (0600) and sent to that gate only. Sealed services
(spec/ext-sealed.md): a hosted job's state is kept under ~/.kontra/hosted, a hired job's key under ~/.kontra/jobs,
and the pins in ~/.kontra/pins; recipient key ids and seller key thumbprints (kid:<kid> for a seller signature that
names its key only, D223) are printed on stderr on a first sight and on every change.

kontra seal seals a file for a sealed service without a node (the escape hatch for scripting, ext-sealed.md §5.1):
it prints one A2A Message JSON whose single part is the file sealed to the service's recipients; --open writes the
plaintext of a reply's sealed parts and binds its job to the task the reply names, so that --task <id> seals a
follow-up of that task with its job's key. --api-key KEY|@FILE (never read from the environment) is sent to the
--service's gate only, to read the card of a service that is not listed: on the service's card and upstream card once
the card answered 404 without it, never on the gate's key set.
`

// later is when each subcommand that is not built yet comes (PRD §14, note 2 §4). There is no console
// subcommand: the owner dropped the local console (D164).
var later = map[string]string{
	"login":   "M4", // turns an anonymous session into an account (tunnel.md §6.1; M4)
	"publish": "M5", // listings (PRD §14 M5)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process: it returns the exit code (2 for a usage error).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version.String("kontra"))
		return 0
	case "connect":
		err := connect(ctx, args[1:], stdout, stderr)
		var uerr *node.UsageError
		switch {
		case err == nil:
			return 0
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprint(stdout, usage)
			return 0
		case errors.As(err, &uerr):
			fmt.Fprintln(stderr, "kontra connect:", tunnel.Printable(err.Error()))
			fmt.Fprintln(stderr, "(kontra --help for the flags)")
			return 2
		default:
			// A refusal carries the gate's words: never an escape sequence on the operator's terminal.
			fmt.Fprintln(stderr, "kontra connect:", tunnel.Printable(err.Error()))
			return 1
		}
	case "seal":
		err := sealing.SealCommand(ctx, args[1:], os.Stdin, stdout, stderr)
		switch {
		case err == nil:
			return 0
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprint(stdout, sealing.SealUsage)
			return 0
		case errors.Is(err, sealing.ErrUsage):
			// The reason may quote an argument: escaped for the terminal; the synopsis is the CLI's own.
			fmt.Fprintln(stderr, "kontra seal:", tunnel.Printable(err.Error()))
			fmt.Fprint(stderr, sealing.SealUsage)
			return 2
		default:
			fmt.Fprintln(stderr, "kontra seal:", tunnel.Printable(err.Error()))
			return 1
		}
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	if m, ok := later[args[0]]; ok {
		fmt.Fprintf(stderr, "kontra %s: not until %s (PRD §14)\n", args[0], m)
		return 1
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// connect runs `kontra connect` until ctx ends (SIGINT, SIGTERM) or the gate revokes the key. The welcome's
// service URLs go to stdout, the logs to stderr. A command line ParseArgs refused is a *node.UsageError; a
// handshake the gate refused with a code that is not retried, a revoked key and a key file kontra connect
// refuses are errors that exit 1 (spec/tunnel.md §4).
func connect(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
