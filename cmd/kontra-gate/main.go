// Command kontra-gate is the gate server: serve, migrate, enroll (note 2 §4, PRD §11).
//
//	kontra-gate serve -f kontra.yaml [--listen addr] [--metrics-listen addr] [--peer-listen addr] [--log-level level]
//	kontra-gate migrate [-f kontra.yaml | --postgres URL] [up|down|status]
//	kontra-gate enroll -f kontra.yaml --api-key <name> [--scope hire] [--prefix <p>]   (M1)
//	kontra-gate enroll -f kontra.yaml <service>                                       (M2)
//	kontra-gate enroll -f kontra.yaml --revoke <key_id> [--service <name>]            (M2)
//	kontra-gate health [--url https://127.0.0.1:8443/healthz]                        (M3, the compose healthcheck)
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	enrollpkg "kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/gate"
	"kontra.sh/kontra/internal/migrate"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/version"
)

const usage = `usage:
  kontra-gate serve -f kontra.yaml [--listen <addr>] [--metrics-listen <addr>] [--peer-listen <addr>]
                    [--log-level debug|info|warn|error]
  kontra-gate migrate [-f kontra.yaml | --postgres <url>] [up|down|status]
  kontra-gate enroll -f kontra.yaml --api-key <name> [--scope <scope>]... [--prefix <p>]
  kontra-gate enroll -f kontra.yaml <service>
  kontra-gate enroll -f kontra.yaml --revoke <key_id> [--service <name>]
  kontra-gate health [--url <url of /healthz>]
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
	case "health":
		return health(context.Background(), args[1:], stdout)
	case "version":
		fmt.Fprintln(stdout, version.String("kontra-gate"))
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	return usageError{"unknown command " + args[0]}
}

// serveArgs are serve's flags: the configuration file, the per-replica overrides, and the log level.
type serveArgs struct {
	file  string
	opts  []config.Option
	level slog.Level
}

// parseServe parses serve's flags. --log-level sets the lowest level the replica logs (info by default); debug
// adds the lines a test reads, such as the e2e harness's marker scans of both replicas' logs (D171).
func parseServe(args []string) (serveArgs, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("f", "kontra.yaml", "configuration file")
	listen := fs.String("listen", "", "public listener for this replica, overriding listen:")
	metricsListen := fs.String("metrics-listen", "", "metrics listener for this replica, overriding metrics.listen:")
	peerListen := fs.String("peer-listen", "", "peer listener for this replica, overriding peer.listen: (spec §11.2)")
	logLevel := fs.String("log-level", "info", "the lowest level logged: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return serveArgs{}, usageError{"serve: " + err.Error()}
	}
	if fs.NArg() > 0 {
		return serveArgs{}, usageError{"serve: unexpected argument " + fs.Arg(0)}
	}
	a := serveArgs{file: *file}
	switch *logLevel {
	case "debug":
		a.level = slog.LevelDebug
	case "info":
		a.level = slog.LevelInfo
	case "warn":
		a.level = slog.LevelWarn
	case "error":
		a.level = slog.LevelError
	default:
		return serveArgs{}, usageError{fmt.Sprintf("serve: --log-level %q: want debug, info, warn or error", *logLevel)}
	}
	if *listen != "" {
		a.opts = append(a.opts, config.WithListen(*listen))
	}
	if *metricsListen != "" {
		a.opts = append(a.opts, config.WithMetricsListen(*metricsListen))
	}
	if *peerListen != "" {
		a.opts = append(a.opts, config.WithPeerListen(*peerListen))
	}
	return a, nil
}

// serveConfig parses serve's flags and loads the configuration with the per-replica overrides, so two
// replicas on one host can share one kontra.yaml.
func serveConfig(args []string) (*config.Config, error) {
	a, err := parseServe(args)
	if err != nil {
		return nil, err
	}
	return config.Load(a.file, a.opts...)
}

func serve(args []string, stderr io.Writer) error {
	a, err := parseServe(args)
	if err != nil {
		return err
	}
	cfg, err := serveConfig(args)
	if err != nil {
		return err
	}
	log, err := obs.NewLogger(stderr, obs.FormatJSON, a.level)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	// SIGTERM drains (spec §14.3); SIGINT stops the same way, and a second either one during the drain
	// ends the process at once. The context is canceled with a cause wrapping gate.ErrDrain, and Run
	// does its drain writes on a context of their own.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	defer drainOnSignal(log, cancel)()
	// SIGHUP reloads services: from the same file with the same per-replica overrides (spec §13.5). It is
	// caught from here on, before gate.New (up to 30 s of Temporal dial), where its default action would
	// end the process; the replica applies one that arrives during the start once it runs.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	reload := func() (*config.Config, error) { return serveConfig(args) }
	r, err := gate.New(ctx, cfg, gate.WithLogger(log), gate.WithReload(reload), gate.WithSIGHUP(hup))
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

// enroll mints an API key (`--api-key <name>`, M1) or a service enrollment token (`<service>`, M2) and
// prints it alone on stdout, so KONTRA_API_KEY="$(kontra-gate enroll -f kontra.yaml --api-key demo)" and
// `kontra connect --enroll "$(kontra-gate enroll hello)"` work; what was minted is said on stderr. The key
// goes to the team tenant, created when it does not exist yet (the README order is migrate, enroll, serve),
// in the transaction that also appends its audit row. `--revoke <key_id>` revokes a node key, and with
// `--service <name>` one of its bindings (spec/tunnel.md §12); it prints nothing on stdout.
func enroll(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("f", "kontra.yaml", "configuration file")
	apiKey := fs.String("api-key", "", "mint an API key with this name and print it once")
	prefix := fs.String("prefix", "", "operator prefix of the key, 1-16 [a-z0-9], so key: globs can name a group (key:kk_<prefix>_*)")
	revoke := fs.String("revoke", "", "revoke a node key (k1_…)")
	service := fs.String("service", "", "with --revoke: revoke only the key's binding to this service")
	var scopes multiFlag
	fs.Var(&scopes, "scope", "API key scope: hire (default), enroll:<service> or enroll:* (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError{"enroll: " + err.Error()}
	}
	// A service name may come before the flags too (`kontra-gate enroll hello -f kontra.yaml`).
	var names []string
	for fs.NArg() > 0 {
		names = append(names, fs.Arg(0))
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return usageError{"enroll: " + err.Error()}
		}
	}
	switch {
	case *apiKey != "" && (*revoke != "" || *service != "" || len(names) > 0):
		return usageError{"enroll: --api-key mints a key; it takes no service and no --revoke"}
	case *apiKey != "":
		return enrollAPIKey(ctx, *file, *apiKey, *prefix, scopes, stdout, stderr)
	case *prefix != "" || len(scopes) > 0:
		return usageError{"enroll: --prefix and --scope go with --api-key <name>"}
	case *revoke != "" && len(names) > 0:
		return usageError{"enroll: --revoke takes a key id; name one service with --service"}
	case *revoke != "":
		return enrollRevoke(ctx, *file, *revoke, *service, stderr)
	case *service != "":
		return usageError{"enroll: --service goes with --revoke <key_id>; to mint a token, name the service alone"}
	case len(names) == 1:
		return enrollToken(ctx, *file, names[0], stdout, stderr)
	case len(names) > 1:
		return usageError{"enroll: one service at a time, not " + strings.Join(names, " ")}
	}
	return usageError{"enroll: want --api-key <name>, <service> or --revoke <key_id>"}
}

// openDB opens a pool on the configuration's database once its schema is current (file names the
// configuration in the error that asks for a migration).
func openDB(ctx context.Context, cfg *config.Config, file string) (*pgxpool.Pool, error) {
	m, err := migrate.Open(ctx, cfg.Postgres)
	if err != nil {
		return nil, err
	}
	err = m.Check(ctx)
	m.Close()
	if errors.Is(err, migrate.ErrSchemaNotCurrent) {
		return nil, fmt.Errorf("%w: run kontra-gate migrate -f %s first", err, file)
	}
	if err != nil {
		return nil, err
	}
	return pg.NewPool(ctx, cfg.Postgres)
}

// enrollToken mints a service enrollment token (spec/tunnel.md §5) for a tunnel service of the team tenant
// and prints it alone on stdout. The service is the row `kontra-gate serve` loaded from services: (the
// table is the truth): it must exist, be enabled and have backend tunnel. Nothing is written otherwise.
func enrollToken(ctx context.Context, file, service string, stdout, stderr io.Writer) error {
	if !config.NameRE.MatchString(service) {
		return usageError{fmt.Sprintf("enroll: %q is not a service name (%s)", service, config.NameRE)}
	}
	cfg, err := config.Load(file)
	if err != nil {
		return err
	}
	if cfg.Tenancy != config.TenancySingle {
		return errors.New("enroll <service> mints tokens for a team gate's one tenant; a multi-tenant gate's operator mints its own (spec/tunnel.md §5)")
	}
	pool, err := openDB(ctx, cfg, file)
	if err != nil {
		return err
	}
	defer pool.Close()
	tenantID, err := tenant.Lookup(ctx, pool, tenant.TeamSlug)
	if errors.Is(err, tenant.ErrNotFound) {
		return fmt.Errorf("enroll %s: this gate has no services yet: start kontra-gate serve -f %s, which loads services: into the database", service, file)
	}
	if err != nil {
		return err
	}
	store := enrollpkg.NewStore(pool, enrollpkg.Options{})
	token, minted, err := store.MintServiceTokenInfo(ctx, tenantID, service, "kontra-gate enroll")
	switch {
	case errors.Is(err, enrollpkg.ErrUnknownService):
		hint := ""
		if _, inFile := cfg.Services[service]; inFile {
			hint = fmt.Sprintf(" (it is in %s: start kontra-gate serve, or send it SIGHUP, to load it)", file)
		}
		return fmt.Errorf("enroll %s: no such service%s", service, hint)
	case errors.Is(err, enrollpkg.ErrServiceDisabled):
		return fmt.Errorf("enroll %s: the service is disabled (no longer in services:)", service)
	case errors.Is(err, enrollpkg.ErrNotTunnel):
		return fmt.Errorf("enroll %s: the service's backend is not tunnel; only nodes host tunnel services (spec/tunnel.md §5)", service)
	case err != nil:
		return err
	}
	if _, err := fmt.Fprintln(stdout, token); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "minted an enrollment token for service %s (tenant %s): single use, valid until %s; it is shown once\n",
		service, minted.Tenant, minted.ExpiresAt.UTC().Format(time.RFC3339))
	return nil
}

// enrollRevoke revokes a node key (reason operator), or with service one of its bindings (spec/tunnel.md
// §12). Each is one transaction with its audit row and NOTIFY kontra_revoke; the replica holding the key's
// session acts on it within 5 s.
//
// SPEC-CONFLICT (M2 plan, C9): §12 names `kontra-gate enroll --revoke <key_id>` for a key and no command for
// one binding; --service is the plan's proposal (operator CLI, not wire protocol).
//
// SPEC-CONFLICT (§12, §15; M2 WP-02 review): §15 says Revoke ends "the session and every future handshake"
// of a stolen identity key, but a rotation (§12) needs only proof of the old key, so a thief can rotate the
// stolen key to one of their own first; revoking the stolen key id then leaves the thief's successor
// enrolled. Revocation does not follow rotated_to: a rotation is often made because the old key leaked, and
// revoking the old key afterwards only ends its overlap early. So the command names the newest key of the
// chain when it is still enrolled, and how to revoke it; the owner decides whether Revoke should follow the
// chain (or offer a flag that does).
func enrollRevoke(ctx context.Context, file, keyID, service string, stderr io.Writer) error {
	if !config.NodeKeyIDRE.MatchString(keyID) {
		return usageError{fmt.Sprintf("enroll: --revoke %q: want a node key id, k1_ and 26 base32 characters", keyID)}
	}
	if service != "" && !config.NameRE.MatchString(service) {
		return usageError{fmt.Sprintf("enroll: --service %q is not a service name (%s)", service, config.NameRE)}
	}
	cfg, err := config.Load(file)
	if err != nil {
		return err
	}
	pool, err := openDB(ctx, cfg, file)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := enrollpkg.NewStore(pool, enrollpkg.Options{})
	before, err := store.LookupKey(ctx, keyID)
	if errors.Is(err, enrollpkg.ErrKeyNotFound) {
		return fmt.Errorf("enroll --revoke: this gate has no node key %s", keyID)
	}
	if err != nil {
		return err
	}
	// The newest key of the chain this key was rotated along, when it is still enrolled.
	var successor *enrollpkg.Key
	if before.RotatedTo != "" {
		k, err := store.Successor(ctx, keyID)
		if err != nil {
			return err
		}
		if k != nil && k.RevokedAt == nil {
			successor = k
		}
	}
	const actor = "kontra-gate enroll --revoke"
	if service == "" {
		if before.RevokedAt != nil {
			fmt.Fprintf(stderr, "node key %s (%s, tenant %s) was already revoked at %s\n", keyID, before.NodeName, before.Tenant,
				before.RevokedAt.UTC().Format(time.RFC3339))
		} else {
			if err := store.RevokeKey(ctx, keyID, enrollpkg.ReasonOperator, actor); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "revoked node key %s (%s, tenant %s): its session closes and its next handshake is refused\n",
				keyID, before.NodeName, before.Tenant)
		}
		if successor != nil {
			fmt.Fprintf(stderr, "warning: node key %s was rotated to %s (%s), which stays enrolled and keeps serving the node's services; "+
				"to cut the node off, revoke it too: kontra-gate enroll -f %s --revoke %s\n", keyID, successor.KeyID, successor.NodeName,
				file, successor.KeyID)
		}
		return nil
	}
	err = store.RevokeBinding(ctx, keyID, service, actor)
	if errors.Is(err, enrollpkg.ErrBindingNotFound) {
		return fmt.Errorf("enroll --revoke: node key %s is not enrolled for %s", keyID, service)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "revoked the binding of node key %s (%s, tenant %s) to %s: its session stops serving %s\n",
		keyID, before.NodeName, before.Tenant, service, service)
	if successor != nil {
		fmt.Fprintf(stderr, "warning: node key %s was rotated to %s (%s), which has its own copy of the binding: unless that was revoked "+
			"too, the node keeps serving %s; revoke it with: kontra-gate enroll -f %s --revoke %s --service %s\n", keyID,
			successor.KeyID, successor.NodeName, service, file, successor.KeyID, service)
	}
	return nil
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
	pool, err := openDB(ctx, cfg, file)
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
