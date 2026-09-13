// db.go — `kontra db list|create|anew|delete`: operator-owned datasets in the DuckLake catalog.
//
// Actor OUTPUT becomes a dataset automatically (the materializer's materializeNode activity, on
// the kontra-materializer queue; read it back with `kontra explore <run>`). This command covers
// the other direction: the lists an operator brings IN — a scope export, a
// wordlist, a set of targets — so they live in the same catalog, are queryable with the same
// SQL, and can be handed to `kontra actor … dispatch` without a scratch file in /tmp.
//
// DuckDB is driven as a subprocess, exactly as `kontra runs --duckdb` already does: DuckLake
// is a DuckDB extension and its catalog/attach semantics are the contract, so we speak that
// contract rather than reimplement a parquet writer in Go.
//
// `anew` is the interesting verb, and it is `anew(1)` semantics: append only rows not already
// present, and report how many were new. Re-running a scope export through it is therefore
// idempotent and tells you what actually changed since last time — which is the whole point of
// keeping scope in a dataset instead of a file that gets clobbered.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

// lakeSchema is where operator-loaded lists live (scope, seeds) — the `standalone/` top-level
// directory. It is a SEPARATE schema from actor `output/`, because a list is an INPUT you feed
// a run and output is what a run PRODUCED: mixing them is what made "which of these is my
// scope?" a question. `db list` shows only this schema.
const lakeSchema = "standalone"

func cmdDB(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kontra db list|create|anew|delete [args]  (see --help)")
	}
	sub := args[0]
	fs := flag.NewFlagSet("db", flag.ContinueOnError)
	catalog := fs.String("catalog", "", "DuckLake catalog DSN (default: discovered from the checkout or the running stack)")
	dataPath := fs.String("data-path", "", "lake DATA_PATH (default: KONTRA_DUCKLAKE_DATA_PATH or s3://<bucket>/)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()

	switch sub {
	case "list":
		return dbList(*catalog, *dataPath)
	case "create", "anew":
		if len(rest) < 2 {
			return fmt.Errorf("usage: kontra db %s <file.csv|.jsonl|.parquet> <dataset_name>", sub)
		}
		return dbIngest(*catalog, *dataPath, rest[0], rest[1], sub == "anew")
	case "delete":
		if len(rest) < 1 {
			return errors.New("usage: kontra db delete <dataset_name>")
		}
		return dbDelete(*catalog, *dataPath, rest[0])
	default:
		return fmt.Errorf("unknown db subcommand %q (want list|create|anew|delete)", sub)
	}
}

// lakeCatalog resolves the DuckLake catalog DSN by DISCOVERING it, not by demanding it.
//
// Everything needed is already on the host: docker-compose.yml carries the DSN the orchestrator
// runs with, .env carries the password it interpolates, and the running container carries the
// two already joined. Requiring an operator to re-type that as an env var before every dataset
// command was busywork the tool can do itself — and the error printed instead ("set
// KONTRA_DUCKLAKE_CATALOG or pass --catalog") named what was missing without naming its value.
//
// Sources, in order. An explicit --catalog wins and is passed through UNTOUCHED — that is the
// escape hatch for a catalog that is not this stack's. Everything discovered is then rewritten
// for host use, because compose points the orchestrator at `host=safedeps-postgres`, a name that
// only resolves inside the compose network.
func lakeCatalog(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	for _, source := range []func() string{
		func() string { return os.Getenv("KONTRA_DUCKLAKE_CATALOG") },
		catalogFromCheckout,     // cheap: two file reads
		catalogFromRunningStack, // costs a docker exec, so it goes last
		applianceCatalog,        // the file the appliance owns — the default since ADR 0031 §1b
	} {
		if dsn := source(); dsn != "" {
			return hostReachableDSN(dsn), nil
		}
	}
	return "", errors.New("could not work out the DuckLake catalog.\n" +
		"  Four places were tried: KONTRA_DUCKLAKE_CATALOG, the checkout (docker-compose.yml + .env),\n" +
		"  the running orchestrator container, and the file the appliance owns —\n" +
		"  <data-dir>/datasets.ducklake, where the data dir is KONTRA_DATA_DIR or $KONTRA_HOME/data.\n" +
		"  The last one is the DEFAULT since the catalog stopped being a Postgres, and it does not\n" +
		"  exist yet: it is created by the first thing that writes the lake, so this usually means no\n" +
		"  Run has materialized anything on this machine. A catalog somewhere else, say where:\n" +
		"    --catalog /path/to/datasets.ducklake\n" +
		"    --catalog 'postgres:dbname=kontra_ducklake host=localhost port=5432 user=kontra password=…'")
}

// applianceCatalog is the file catalog `control/orchestrator/src/data/parquet.ts:defaultCatalogPath`
// resolves — the same directory, the same name, so the CLI and the control plane cannot disagree
// about which lake they are talking about.
//
// ONLY WHEN IT EXISTS. An absent file is not this stack's catalog, it is a machine that has never
// materialized anything, and returning the path anyway would replace `lakeCatalog`'s explanation
// with DuckDB's `Cannot open database … in read-only mode: database does not exist`.
//
// AND IT CANNOT BE READ WHILE THE CONTROL PLANE HOLDS IT. This is the cost of the catalog being a
// file rather than a server, and it is worth knowing before it is met: DuckDB takes an exclusive
// lock on a database file it opens read-write, so a second process attaching the same catalog —
// EVEN READ-ONLY — is refused with `Could not set lock on file … Conflicting lock is held in …
// (PID n)`. Measured, both directions. Every `kontra` verb that reads the lake through the
// ORCHESTRATOR (`dataset list`, `dataset query` without `--local`) is unaffected, because that
// process is the holder; the ones that attach DuckDB here need the control plane stopped.
func applianceCatalog() string {
	dir, err := applianceDataDir("")
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, "datasets.ducklake")
	if !cliutil.FileExists(path) {
		return ""
	}
	return path
}

// lakeMetaSchema is the schema DuckLake keeps its `ducklake_*` tables in, by BACKEND — the Go twin
// of `control/orchestrator/src/data/parquet.ts:metaSchemaFor`, and it must stay its twin.
//
// A Postgres catalog puts them in `public`; a file catalog puts them in `main`. There is no schema
// both answer to: measured on DuckLake 1.5.4, `__ducklake_metadata_lake.public.ducklake_table`
// against a file catalog answers `Catalog Error: … schema "public" does not exist`. Every query
// here that touches catalog metadata took `public` as a constant, which was correct for exactly as
// long as the catalog was the compose Postgres.
func lakeMetaSchema(dsn string) string {
	if strings.HasPrefix(dsn, "postgres:") || strings.HasPrefix(dsn, "postgresql:") {
		return "public"
	}
	return "main"
}

// lakeMeta qualifies one `ducklake_*` metadata table for the catalog's backend, so a query reads
// `lakeMeta(dsn, "ducklake_data_file")` rather than carrying a schema name it cannot know.
func lakeMeta(dsn, table string) string {
	return "__ducklake_metadata_lake." + lakeMetaSchema(dsn) + "." + table
}

// Compose-network hostnames the catalog DSN may carry. Both are HISTORICAL now — ADR 0031 §1b
// removed `ducklake-postgres` from docker-compose.yml and the catalog defaults to a file — and
// both stay because the DSN they appear in still does: an installation that re-split the API and
// the materializer brings a Postgres back, and one that never migrated is still pointed at
// `safedeps-postgres`, the hand-started container from another stack the control plane used to
// borrow, by its own KONTRA_DUCKLAKE_PG_HOST.
//
// A LIST, NOT A NAME, and it is a list because renaming the service broke this: the rewrite
// matched one hardcoded string, so the day the compose service was called something else, every
// `kontra dataset|db` on the host failed with `could not translate host name` — pointing at DNS,
// which is true and useless. Adding a service here is now the same edit as adding it to compose.
var composeCatalogHosts = []string{"ducklake-postgres", "safedeps-postgres"}

// hostReachableDSN rewrites compose's container-network hostname. The CLI runs on the host, where
// a compose service name does not resolve; inside the compose network it is the only name that
// does. The catalog Postgres publishes 5432 to the host precisely so this rewrite has somewhere
// to point.
func hostReachableDSN(dsn string) string {
	for _, host := range composeCatalogHosts {
		dsn = strings.ReplaceAll(dsn, "host="+host, "host=localhost")
	}
	return dsn
}

// catalogFromCheckout reads the DSN out of docker-compose.yml and fills in the password from
// .env. Compose stays the single source of truth for db name, user and port, so a deployment
// that changes any of them keeps working without a second copy hardcoded here.
//
// Works with the stack DOWN, which is the case `docker inspect` cannot cover: the catalog lives
// in Postgres and the data on S3, and those can be up while the orchestrator is not.
func catalogFromCheckout() string {
	root, ok := findUp("docker-compose.yml")
	if !ok {
		return ""
	}
	compose, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		return ""
	}
	m := composeCatalogRe.FindSubmatch(compose)
	if m == nil {
		return ""
	}
	// Same precedence docker compose itself applies — real environment first, then .env — so the
	// CLI and the stack cannot disagree about the password.
	//
	// `missing` is not decoration. os.Expand substitutes the EMPTY STRING for anything the mapping
	// declines, so a .env without the password yields a perfectly well-formed `password=` DSN that
	// fails much later as a Postgres authentication error — pointing at credentials rather than at
	// the missing file. Refuse here instead, and let the caller say where the value should live.
	env := dotEnv(filepath.Join(root, ".env"))
	missing := false
	dsn := os.Expand(string(m[1]), func(k string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		if v := env[k]; v != "" {
			return v
		}
		missing = true
		return ""
	})
	if missing {
		return ""
	}
	return dsn
}

var composeCatalogRe = regexp.MustCompile(`KONTRA_DUCKLAKE_CATALOG:\s*"([^"]+)"`)

// catalogFromRunningStack asks the orchestrator container what it was started with. This is what
// makes the CLI work from ANY directory — no checkout in sight — and it reports the config that
// is actually live rather than whatever the working tree currently says.
//
// Selected by compose service label, not container name: the name carries the compose project
// prefix, which is the directory the stack was brought up from.
func catalogFromRunningStack() string {
	return envFromRunningStack("KONTRA_DUCKLAKE_CATALOG")
}

// envFromRunningStack asks the orchestrator container what it was started with.
//
// This is what makes the CLI work from ANY directory — no checkout in sight — and it reports the
// config that is actually live rather than whatever the working tree currently says. Selected by
// compose service LABEL, not container name: the name carries the compose project prefix, which
// is just the directory the stack happened to be brought up from.
func envFromRunningStack(key string) string {
	id, err := exec.Command("docker", "ps", "-q", "--filter",
		"label=com.docker.compose.service=orchestrator-api").Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(id)), "\n")
	if first == "" {
		return ""
	}
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .Config.Env}}{{println .}}{{end}}", first).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// dotEnv parses KEY=VALUE lines. Deliberately minimal — no shell semantics beyond stripping one
// layer of quotes — because .env here holds plain secrets, not script.
func dotEnv(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func lakeDataPath(override string) string {
	if override != "" {
		return override
	}
	if p := os.Getenv("KONTRA_DUCKLAKE_DATA_PATH"); p != "" {
		return p
	}
	return "s3://" + s3Bucket() + "/"
}

// attachSQL is the preamble every db subcommand runs: extensions, an S3 secret pointing at the
// controller's SeaweedFS, and the lake ATTACH. The DATA_PATH must match what the catalog already
// records or DuckLake refuses to attach — that mismatch is the first thing you hit here, and the
// error it prints names the expected path, so it is worth surfacing rather than swallowing.
func attachSQL(dsn, dataPath string) string {
	return strings.Join([]string{
		"INSTALL ducklake", "LOAD ducklake",
		"INSTALL postgres", "LOAD postgres",
		"INSTALL httpfs", "LOAD httpfs",
		fmt.Sprintf("CREATE OR REPLACE SECRET kontra_s3 (TYPE s3, KEY_ID '%s', SECRET '%s', ENDPOINT '%s', USE_SSL false, URL_STYLE 'path')",
			s3AccessKey(), s3SecretKey(), s3HostPort()),
		fmt.Sprintf("ATTACH 'ducklake:%s' AS lake (DATA_PATH '%s')", dsn, dataPath),
		fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS lake.%s", lakeSchema),
	}, "; ") + "; "
}

// SeaweedFS in this deployment accepts anonymous reads, but DuckLake WRITES need a credential
// pair present; the dev default is kontra/kontra, matching the KONTRA_S3_* fallback the actors
// use.
func s3AccessKey() string { return cliutil.EnvOr("KONTRA_S3_ACCESS_KEY", "kontra") }
func s3SecretKey() string { return cliutil.EnvOr("KONTRA_S3_SECRET_KEY", "kontra") }

// readerFor maps a file extension to the DuckDB reader that parses it. Explicit rather than
// read_auto: a jsonl file of one-key objects is otherwise easy to mis-sniff as CSV.
func readerFor(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("input file %s: %w", path, err)
	}
	q := strings.ReplaceAll(abs, "'", "''")
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".csv", ".tsv":
		return fmt.Sprintf("read_csv_auto('%s')", q), nil
	case ".json", ".jsonl", ".ndjson":
		return fmt.Sprintf("read_json_auto('%s', union_by_name=true, ignore_errors=true)", q), nil
	case ".parquet", ".pq":
		return fmt.Sprintf("read_parquet('%s')", q), nil
	default:
		return "", fmt.Errorf("unsupported file type %q (want .csv .jsonl .json .parquet)", filepath.Ext(abs))
	}
}

// ident rejects anything that is not a plain identifier. Dataset names reach SQL as table names
// and cannot be parameterized, so this is the injection boundary — refuse rather than quote.
func datasetIdent(name string) (string, error) {
	if name == "" {
		return "", errors.New("dataset name is empty")
	}
	// HYPHENS ARE ALLOWED, and that is the point: actor names are hyphenated (layer-scan,
	// registry-watch), and a standalone dataset must be able to carry the SAME name as the actor
	// that produces it. Forbidding '-' here forced every hand-made dataset to invent an
	// underscore alias -- `layer-scan` output sat beside a `docker_leak_findings` standalone
	// holding the same rows under a name nothing could join to the actor. The identifier is
	// QUOTED at every use below, so '-' is safe; unquoted it would parse as subtraction.
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return "", fmt.Errorf("dataset name %q: use letters, digits, underscore and hyphen only", name)
		}
	}
	// A double quote would escape the quoting below and let a name inject SQL; the loop above
	// already excludes it, this is the belt to that braces.
	if strings.Contains(name, `"`) {
		return "", fmt.Errorf("dataset name %q: must not contain a double quote", name)
	}
	if name[0] >= '0' && name[0] <= '9' {
		return "", fmt.Errorf("dataset name %q: must not start with a digit", name)
	}
	return name, nil
}

func runDuck(sql string) (string, error) {
	bin, err := duckdbBin()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, "-c", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("duckdb: %w\n%s", err, duckFailure(string(out)))
	}
	return string(out), nil
}

// duckFailure turns duckdb's combined output into the part of it that is about the failure.
//
// ── WHY THIS IS NOT JUST `TrimSpace` ────────────────────────────────────────────────────────────
//
// It was, and the result read as a contradiction (issue #3). kontra sends several statements in one
// `-c`, so when the third one fails the combined output still carries the first two's RESULTS — and
// duckdb renders a successful DDL statement as a box:
//
//	error: duckdb: exit status 1
//	  Could not set lock on file ".../datasets.ducklake": Conflicting lock held in .../node (PID …)
//	┌─────────┐
//	│ Success │
//	│  true   │
//	└─────────┘
//
// The reporter read that as kontra printing `Success: true` after its own error, which is exactly
// what it looks like. Nothing succeeded that they asked for; a statement they never typed did. So
// the boxes come out and the error lines stay.
//
// ── AND THE LOCK IS NAMED, BECAUSE IT IS THE COMMON ONE AND IT IS ACTIONABLE ────────────────────
//
// DuckLake takes an exclusive lock on the catalog, and `kontra up` holds it for as long as the
// control plane runs. Every `dataset create` on a running installation hits this, and duckdb's own
// message names a PID and a path — true, and no help at all unless you already know that the PID is
// your own control plane. One sentence turns a dead end into a next step.
func duckFailure(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// A RESULT BOX, in any of the three rows duckdb draws it with. Matched on the box-drawing
		// runes rather than on the word `Success`, because the noise is every result table — a
		// `count(*)` from a statement that ran before the failure is just as misleading.
		if strings.ContainsAny(trimmed, "┌┐└┘├┤┬┴┼─│") {
			continue
		}
		kept = append(kept, trimmed)
	}
	msg := strings.Join(kept, "\n")
	if strings.Contains(out, "Conflicting lock") && strings.Contains(out, ".ducklake") {
		msg += "\n\nthe lock is almost certainly your own control plane: DuckLake takes an exclusive" +
			"\nlock on the catalog and `kontra up` holds it while it runs. Stop it, run this, start it" +
			"\nagain — or use the API/console, which goes through the process that already holds it."
	}
	return msg
}

func dbList(catalog, dataPath string) error {
	dsn, err := lakeCatalog(catalog)
	if err != nil {
		return err
	}
	sql := attachSQL(dsn, lakeDataPath(dataPath)) +
		fmt.Sprintf("SELECT table_name AS dataset, estimated_size AS rows, column_count AS cols "+
			"FROM duckdb_tables() WHERE database_name='lake' AND schema_name='%s' ORDER BY table_name;", lakeSchema)
	out, err := runDuck(sql)
	if err != nil {
		return err
	}
	fmt.Fprint(cliio.Stdout, out)
	return nil
}

// dbIngest implements both `create` (replace wholesale) and `anew` (append only rows not already
// present, reporting how many were new).
func dbIngest(catalog, dataPath, file, name string, anew bool) error {
	dsn, err := lakeCatalog(catalog)
	if err != nil {
		return err
	}
	tbl, err := datasetIdent(name)
	if err != nil {
		return err
	}
	reader, err := readerFor(file)
	if err != nil {
		return err
	}
	q := fmt.Sprintf("lake.%s.%q", lakeSchema, tbl) // %q: a hyphenated name is a valid identifier only when quoted

	if !anew {
		sql := attachSQL(dsn, lakeDataPath(dataPath)) +
			fmt.Sprintf("CREATE OR REPLACE TABLE %s AS SELECT * FROM %s; ", q, reader) +
			fmt.Sprintf("SELECT count(*) AS rows FROM %s;", q)
		out, err := runDuck(sql)
		if err != nil {
			return err
		}
		fmt.Fprintf(cliio.Stdout, "created dataset %s from %s\n%s", tbl, file, out)
		return nil
	}

	// anew: create the table on first use, then insert only rows absent from it. EXCEPT does the
	// anti-join across ALL columns and also de-duplicates within the incoming file, which is the
	// behaviour `anew(1)` has and what makes re-running a scope export idempotent.
	sql := attachSQL(dsn, lakeDataPath(dataPath)) +
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s AS SELECT * FROM %s LIMIT 0; ", q, reader) +
		fmt.Sprintf("CREATE OR REPLACE TEMP TABLE _incoming AS SELECT * FROM %s; ", reader) +
		fmt.Sprintf("CREATE OR REPLACE TEMP TABLE _new AS SELECT * FROM _incoming EXCEPT SELECT * FROM %s; ", q) +
		fmt.Sprintf("INSERT INTO %s SELECT * FROM _new; ", q) +
		fmt.Sprintf("SELECT (SELECT count(*) FROM _new) AS added, count(*) AS total FROM %s;", q)
	out, err := runDuck(sql)
	if err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "anew %s <- %s\n%s", tbl, file, out)
	return nil
}

func dbDelete(catalog, dataPath, name string) error {
	dsn, err := lakeCatalog(catalog)
	if err != nil {
		return err
	}
	tbl, err := datasetIdent(name)
	if err != nil {
		return err
	}
	sql := attachSQL(dsn, lakeDataPath(dataPath)) +
		fmt.Sprintf("DROP TABLE IF EXISTS lake.%s.%q;", lakeSchema, tbl)
	if _, err := runDuck(sql); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "deleted dataset %s\n", tbl)
	return nil
}
