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
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/pressly/goose/v3"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/gate"
	"kontra.sh/kontra/internal/migrate"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/version"
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
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		return enroll(ctx, args[1:], stdout, stderr)
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
	// SIGTERM drains (spec §14.3); SIGINT stops the same way, and a second either one during the drain
	// ends the process at once. The context is canceled with a cause wrapping gate.ErrDrain, and Run
	// does its drain writes on a context of their own. SIGHUP reload is the replica's own.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	defer drainOnSignal(log, cancel)()
	r, err := gate.New(ctx, cfg)
	if err != nil {
		return err
	}
	return r.Run(ctx)
}

// drainOnSignal cancels with a cause wrapping gate.ErrDrain at the first SIGTERM or SIGINT, and then stops
// catching them, so a second one during the drain (which lasts up to drain_timeout, D76) ends the
// process at once with the signal's default action instead of being swallowed. The returned func stops
// catching them earlier, when serve returns first.
func drainOnSignal(log *slog.Logger, cancel context.CancelCauseFunc) (stop func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	done := make(chan struct{})
	var once sync.Once
	stop = func() {
		once.Do(func() {
			signal.Stop(sigs)
			close(done)
		})
	}
	go func() {
		select {
		case s := <-sigs:
			stop()
			log.Info("draining; a second SIGTERM or interrupt stops the gate at once", "signal", s.String())
			cancel(fmt.Errorf("%w: received %s", gate.ErrDrain, s))
		case <-done:
		}
	}()
	return stop
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

// enroll mints an API key (`--api-key <name>`, M1) and prints it alone on stdout, so
// KONTRA_API_KEY="$(kontra-gate enroll -f kontra.yaml --api-key demo)" works; what was minted is said on
// stderr. The key goes to the team tenant, created when it does not exist yet (the README order is
// migrate, enroll, serve), in the transaction that also appends its audit row. Service enrollment tokens
// and node-key revocation are the tunnel's (M2).
func enroll(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("f", "kontra.yaml", "configuration file")
	apiKey := fs.String("api-key", "", "mint an API key with this name and print it once")
	prefix := fs.String("prefix", "", "operator prefix of the key, 1-16 [a-z0-9], so key: globs can name a group (key:kk_<prefix>_*)")
	revoke := fs.String("revoke", "", "revoke a node key (M2)")
	var scopes multiFlag
	fs.Var(&scopes, "scope", "API key scope: hire (default), enroll:<service> or enroll:* (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError{"enroll: " + err.Error()}
	}
	switch {
	case *apiKey != "" && (*revoke != "" || fs.NArg() > 0):
		return usageError{"enroll: --api-key mints a key; it takes no service and no --revoke"}
	case *apiKey != "":
		return enrollAPIKey(ctx, *file, *apiKey, *prefix, scopes, stdout, stderr)
	case *prefix != "" || len(scopes) > 0:
		return usageError{"enroll: --prefix and --scope go with --api-key <name>"}
	case *revoke != "" || fs.NArg() > 0:
		// Service enrollment tokens and node-key revocation belong to the tunnel (spec/tunnel.md §5, §12).
		fmt.Fprintln(stdout, "kontra-gate enroll <service> and --revoke: not until M2 (the tunnel, spec/tunnel.md §5)")
		return nil
	}
	return usageError{"enroll: want --api-key <name>, <service> or --revoke <key_id>"}
}

func enrollAPIKey(ctx context.Context, file, name, prefix string, scopeArgs []string, stdout, stderr io.Writer) error {
	if err := auth.CheckKeyName(name); err != nil {
		return usageError{"enroll: --api-key: " + err.Error()}
	}
	if prefix != "" {
		if err := auth.CheckOperatorPrefix(prefix); err != nil {
			return usageError{"enroll: --prefix: " + err.Error()}
		}
	}
	var scopes []auth.Scope
	for _, s := range scopeArgs {
		sc, err := auth.ParseScope(s)
		if err != nil {
			return usageError{"enroll: --scope: " + err.Error()}
		}
		scopes = append(scopes, sc)
	}
	cfg, err := config.Load(file)
	if err != nil {
		return err
	}
	if cfg.Tenancy != config.TenancySingle {
		return errors.New("enroll --api-key mints keys for a team gate's one tenant; a multi-tenant gate's operator writes its own keys (D37)")
	}
	if !cfg.Auth.APIKeys {
		fmt.Fprintln(stderr, "warning: auth.api_keys is not true in "+file+": this gate refuses API keys until it is")
	}
	m, err := migrate.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	err = m.Check(ctx)
	m.Close()
	if errors.Is(err, migrate.ErrSchemaNotCurrent) {
		return fmt.Errorf("%w: run kontra-gate migrate -f %s first", err, file)
	}
	if err != nil {
		return err
	}
	pool, err := pg.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	tenantID, err := tenant.EnsureTeam(ctx, pool)
	if err != nil {
		return err
	}
	minted, err := auth.NewAPIKeys(pool, nil).Mint(ctx, auth.MintRequest{
		TenantID: tenantID,
		Name:     name,
		Scopes:   scopes,
		Prefix:   prefix,
		Actor:    "kontra-gate enroll --api-key",
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, minted.Secret); err != nil {
		return err
	}
	shown := make([]string, len(minted.Scopes))
	for i, s := range minted.Scopes {
		shown[i] = string(s)
	}
	fmt.Fprintf(stderr, "minted API key %q: principal %s, tenant %s, buyer %s, scopes %s; the key is shown once\n",
		name, minted.Principal(), tenant.TeamSlug, minted.BuyerID, strings.Join(shown, ","))
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint(*m) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
