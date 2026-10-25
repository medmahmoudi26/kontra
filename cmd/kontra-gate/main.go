// Command kontra-gate is the gate server: serve, migrate, enroll (note 2 §4, PRD §11).
//
//	kontra-gate serve -f kontra.yaml
//	kontra-gate migrate -f kontra.yaml [up|down|status]
//	kontra-gate enroll -f kontra.yaml --api-key <name> [--scope hire] [--prefix <p>]   (M1)
//	kontra-gate enroll -f kontra.yaml <service> | --revoke <key_id>                    (M2)
//	kontra-gate version
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

	"github.com/medmahmoudi26/kontra/internal/config"
	"github.com/medmahmoudi26/kontra/internal/gate"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
	"github.com/medmahmoudi26/kontra/internal/version"
)

const usage = `usage:
  kontra-gate serve -f kontra.yaml
  kontra-gate migrate -f kontra.yaml [up|down|status]
  kontra-gate enroll -f kontra.yaml --api-key <name> [--scope <scope>]... [--prefix <p>]
  kontra-gate enroll -f kontra.yaml <service> | --revoke <key_id>
  kontra-gate version
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "kontra-gate:", err)
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{"no command"}
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "migrate":
		return migrateCmd(args[1:])
	case "enroll":
		return enroll(args[1:], stdout)
	case "version":
		fmt.Fprintln(stdout, version.String("kontra-gate"))
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	return usageError{"unknown command " + args[0]}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	file := fs.String("f", "kontra.yaml", "configuration file")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	cfg, err := config.Load(*file)
	if err != nil {
		return err
	}
	// SIGTERM drains (spec §14.3); SIGINT stops the same way. SIGHUP reload is the replica's own.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	r, err := gate.New(ctx, cfg)
	if err != nil {
		return err
	}
	return r.Run(ctx)
}

func migrateCmd(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	file := fs.String("f", "kontra.yaml", "configuration file (its postgres: URL)")
	dsn := fs.String("postgres", "", "postgres:// URL, instead of the configuration file")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	dir := "up"
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	switch dir {
	case "up", "down", "status":
	default:
		return usageError{"migrate: want up, down or status"}
	}
	_, _ = file, dsn
	return unimpl.Error("kontra-gate migrate "+dir, "WP-01")
}

func enroll(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	file := fs.String("f", "kontra.yaml", "configuration file")
	apiKey := fs.String("api-key", "", "mint an API key with this name and print it once")
	prefix := fs.String("prefix", "", "operator-chosen part of the key prefix, for key: globs")
	revoke := fs.String("revoke", "", "revoke a node key (M2)")
	var scopes multiFlag
	fs.Var(&scopes, "scope", "API key scope (repeatable; default hire)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	_, _ = file, prefix
	switch {
	case *apiKey != "":
		return unimpl.Error("kontra-gate enroll --api-key", "WP-03")
	case *revoke != "" || fs.NArg() > 0:
		// Service enrollment tokens and node-key revocation belong to the tunnel (spec/tunnel.md §5, §12).
		fmt.Fprintln(stdout, "kontra-gate enroll <service> and --revoke: not until M2 (the tunnel, spec/tunnel.md §5)")
		return nil
	}
	return usageError{"enroll: want --api-key <name>, <service> or --revoke <key_id>"}
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint(*m) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
