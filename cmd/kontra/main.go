// Command kontra is the node CLI: connect, console, login, seal, publish (note 2 §1).
//
//	kontra connect [--gate URL] --serve a2a:<name>=<url> … (M2, spec/tunnel.md; internal/node)
//	kontra version
//
// The other subcommands come with their milestones (PRD §14, note 2 §4): seal with sealing (M3), console
// and login with the anonymous sessions on the public gate (M4), publish with Cloud's listings (M5).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/version"
)

const usage = `usage:
  kontra connect [--gate <url>] --serve a2a:<name>=<url> [--serve …] [--key <path>] [--enroll <token>|@file|-]
                 [--api-key <key>|@file|-] [--name <name>] [--rotate] [--require-channel-binding]
                 [--max-inflight <n>] [--log json]
  kontra version
`

// later is when each subcommand that is not built yet comes (PRD §14, note 2 §4).
var later = map[string]string{
	"seal":    "M3", // sealing: `kontra seal` (PRD §14 M3)
	"console": "M4", // the console on :7778 (note 2 §1; the README's M4 target)
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
		if err := connect(ctx, args[1:], stderr); err != nil {
			fmt.Fprintln(stderr, "kontra connect:", err)
			return 1
		}
		return 0
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

// connect runs `kontra connect` until ctx ends (SIGINT, SIGTERM) or the gate revokes the key.
func connect(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := node.ParseArgs(args, os.Getenv)
	if err != nil {
		return err
	}
	cfg.Version = version.Get()
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
