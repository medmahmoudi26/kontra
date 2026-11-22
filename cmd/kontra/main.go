// Command kontra is the person's CLI: login, seal, publish (note 2 §1; there is no local console, D164). The node is
// its own binary, kontrad (D364); `kontra connect` only says so.
//
//	kontra seal --service <service URL> [--api-key <key>] [--context <id>] [--task <id>] [file | -],
//	kontra seal --open <part.json>
//	(spec/ext-sealed.md §5.1; internal/sealing)
//	kontra login [--gate URL] [--no-browser] (spec/tunnel.md §6.3; internal/login; M4 WP-06)
//	kontra publish [--gate URL] [--control-plane URL] [--version N | --replace] [--dry-run] [--no-browser] <listing.yaml>
//	(spec/tunnel.md §6.4, spec/schema/listing.json; internal/publish; M5-G3, D323)
//	kontra version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"kontra.sh/kontra/internal/login"
	"kontra.sh/kontra/internal/publish"
	"kontra.sh/kontra/internal/sealing"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/version"
)

// retiredConnect is what `kontra connect` prints, whatever its arguments (D364).
const retiredConnect = "kontra connect was renamed: run kontrad with the same flags (D364)"

const usage = `usage:
  kontra seal --service <service URL> [--api-key <key>|@<file>] [--context <id>] [--task <id>]
              [--media-type <type>] [file | -]
  kontra seal --open <part.json>
  kontra login [--gate <url>] [--no-browser]
  kontra publish [--gate <url>] [--control-plane <url>] [--version <n> | --replace] [--dry-run] [--no-browser] <listing.yaml>
  kontra version

The node, which hosts the services on this machine at a gate and calls through it, is its own binary: kontrad
(kontrad --help). It was kontra connect until D364.

kontra seal seals a file for a sealed service without a node (the escape hatch for scripting, ext-sealed.md §5.1):
it prints one A2A Message JSON whose single part is the file sealed to the service's recipients; --open writes the
plaintext of a reply's sealed parts and binds its job to the task the reply names, so that --task <id> seals a
follow-up of that task with its job's key. --api-key KEY|@FILE (never read from the environment) is sent to the
--service's gate only, to read the card of a service that is not listed: on the service's card and upstream card once
the card answered 404 without it, never on the gate's key set.

kontra login gets an API key for a gate (default https://gateway.kontra.sh) from the gate's sign-in and keeps it in
~/.kontra/credentials for that gate only; kontrad then calls as it, and an anonymous session's next handshake
with it becomes the account's (spec/tunnel.md §6.3). --no-browser prints the sign-in URL instead of opening it.

kontra publish sends a listing file for an A2A agent (spec/schema/listing.json: visibility, sealing, task timeout; no
price, and MCP servers are not listed) to the gate operator's control plane, after a sign-in in the browser for that
publish only: a person publishes, never an API key (spec/tunnel.md §6.4). It never replaces a listing unseen: name
the version you read (--version N) or --replace. --dry-run checks the file and prints the request; kontra publish -h
has the flags.
`

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
		// D364: the node is kontrad; the old command starts nothing, reads nothing and names the new one, with no alias.
		fmt.Fprintln(stderr, retiredConnect)
		return 2
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
	case "login":
		// tunnel.md §6.3 (D278): an API key from the gate's sign-in, kept for that gate only.
		if err := login.Command(ctx, args[1:], stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "kontra login:", tunnel.Printable(err.Error()))
			if errors.Is(err, login.ErrUsage) {
				return 2
			}
			return 1
		}
		return 0
	case "publish":
		// tunnel.md §6.4 (D323): a listing sent to the operator's control plane, signed in for that publish only.
		if err := publish.Command(ctx, args[1:], stdout, stderr); err != nil {
			var problems publish.Problems
			if errors.As(err, &problems) {
				// One line per problem, each escaped for the terminal (a file's text is quoted in them).
				fmt.Fprintln(stderr, "kontra publish: the listing is not valid (spec/schema/listing.json):")
				for _, p := range problems {
					fmt.Fprintln(stderr, "  "+tunnel.Printable(p))
				}
				return 1
			}
			fmt.Fprintln(stderr, "kontra publish:", tunnel.Printable(err.Error()))
			if errors.Is(err, publish.ErrUsage) {
				return 2
			}
			return 1
		}
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	// There is no console subcommand: the owner dropped the local console (D164).
	fmt.Fprint(stderr, usage)
	return 2
}
