// Command kontra-gate is the gate server: serve, migrate, enroll (note 2 §4, PRD §11).
//
//	kontra-gate serve -f kontra.yaml [--listen addr] [--metrics-listen addr]
//	kontra-gate migrate [-f kontra.yaml | --postgres URL] [up|down|status]
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
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/medmahmoudi26/kontra/internal/config"
	"github.com/medmahmoudi26/kontra/internal/gate"
	"github.com/medmahmoudi26/kontra/internal/migrate"
	"github.com/medmahmoudi26/kontra/internal/obs"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
	"github.com/medmahmoudi26/kontra/internal/version"
)

const usage = `usage:
  kontra-gate serve -f kontra.yaml [--listen <addr>] [--metrics-listen <addr>]
  kontra-gate migrate [-f kontra.yaml | --postgres <url>] [up|down|status]
  kontra-gate enroll -f kontra.yaml --api-key <name> [--scope <scope>]... [--prefix <p>]
  kontra-gate enroll -f kontra.yaml <service> | --revoke <key_id>
  kontra-gate version
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintf(os.Stderr, "kontra-gate: %s\n%s", u.msg, usage)
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
		return serve(args[1:], stderr)
	case "migrate":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		return migrateCmd(ctx, args[1:], stdout)
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

// serveConfig parses serve's flags and loads the configuration with the per-replica overrides, so two
// replicas on one host can share one kontra.yaml.
func serveConfig(args []string) (*config.Config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("f", "kontra.yaml", "configuration file")
	listen := fs.String("listen", "", "public listener for this replica, overriding listen:")
	metricsListen := fs.String("metrics-listen", "", "metrics listener for this replica, overriding metrics.listen:")
	if err := fs.Parse(args); err != nil {
		return nil, usageError{"serve: " + err.Error()}
	}
	if fs.NArg() > 0 {
		return nil, usageError{"serve: unexpected argument " + fs.Arg(0)}
	}
	var opts []config.Option
	if *listen != "" {
		opts = append(opts, config.WithListen(*listen))
	}
	if *metricsListen != "" {
		opts = append(opts, config.WithMetricsListen(*metricsListen))
	}
	return config.Load(*file, opts...)
}

func serve(args []string, stderr io.Writer) error {
	cfg, err := serveConfig(args)
	if err != nil {
		return err
	}
	log, err := obs.NewLogger(stderr, obs.FormatJSON, slog.LevelInfo)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	// SIGTERM drains (spec §14.3); SIGINT stops the same way. The context is canceled with a cause
	// wrapping gate.ErrDrain, and Run does its drain writes on a context of their own. SIGHUP reload is
	// the replica's own.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		select {
		case s := <-sigs:
			log.Info("draining", "signal", s.String())
			cancel(fmt.Errorf("%w: received %s", gate.ErrDrain, s))
		case <-ctx.Done():
		}
	}()
	r, err := gate.New(ctx, cfg)
	if err != nil {
		return err
	}
	return r.Run(ctx)
}

// migrateCmd applies, rolls back or lists the embedded migrations (D28). The database comes from
// --postgres, else from the configuration file's postgres:. Flags may come before or after the action.
func migrateCmd(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("f", "kontra.yaml", "configuration file (its postgres: URL)")
	dsn := fs.String("postgres", "", "postgres:// URL, instead of the configuration file")
	if err := fs.Parse(args); err != nil {
		return usageError{"migrate: " + err.Error()}
	}
	action := "up"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return usageError{"migrate: " + err.Error()}
		}
		if fs.NArg() > 0 {
			return usageError{"migrate: unexpected argument " + fs.Arg(0)}
		}
	}
	switch action {
	case "up", "down", "status":
	default:
		return usageError{"migrate: want up, down or status, not " + action}
	}
	url := *dsn
	if url == "" {
		cfg, err := config.Load(*file)
		if err != nil {
			return err
		}
		url = cfg.Postgres
	}
	m, err := migrate.Open(ctx, url)
	if err != nil {
		return err
	}
	defer m.Close()

	switch action {
	case "up":
		res, err := m.Up(ctx)
		for _, r := range res {
			printResult(stdout, r)
		}
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		if len(res) == 0 {
			fmt.Fprintln(stdout, "no migrations to apply")
		}
	case "down":
		r, err := m.Down(ctx)
		if errors.Is(err, goose.ErrNoNextVersion) {
			fmt.Fprintln(stdout, "no migration to roll back")
			return nil
		}
		if r != nil {
			printResult(stdout, r)
		}
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
	case "status":
		st, err := m.Status(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "VERSION\tSTATE\tAPPLIED AT\tMIGRATION")
		for _, s := range st {
			at := "-"
			if !s.AppliedAt.IsZero() {
				at = s.AppliedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", s.Source.Version, s.State, at, filepath.Base(s.Source.Path))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	applied, err := m.Applied(ctx)
	if err != nil {
		return err
	}
	current := int64(0)
	if len(applied) > 0 {
		current = applied[len(applied)-1]
	}
	fmt.Fprintf(stdout, "database at version %d; this gate needs %d\n", current, m.Latest())
	return nil
}

func printResult(w io.Writer, r *goose.MigrationResult) {
	state := "OK"
	if r.Error != nil {
		state = "FAILED"
	}
	fmt.Fprintf(w, "%s %s %s (%s)\n", state, r.Direction, filepath.Base(r.Source.Path), r.Duration.Round(time.Millisecond))
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
