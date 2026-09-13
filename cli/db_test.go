package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The catalog must be DISCOVERED, not demanded. This pins the whole chain: an operator who has
// only a checkout (no exports, stack possibly down) gets a working DSN, with the password filled
// in from .env and compose's container-network hostname rewritten for host use.
//
// The regression this guards is a UX one — `dataset list` used to abort with "set
// KONTRA_DUCKLAKE_CATALOG or pass --catalog" when both values were already sitting on disk.
func TestCatalogDiscoveredFromCheckout(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "docker-compose.yml"),
		"services:\n  orchestrator-api:\n    environment:\n"+
			"      KONTRA_DUCKLAKE_CATALOG: \"postgres:dbname=kontra_ducklake host=safedeps-postgres port=5432 user=kontra password=${KONTRA_DUCKLAKE_PG_PASSWORD}\"\n")
	write(t, filepath.Join(root, ".env"), "# comment\nKONTRA_DUCKLAKE_PG_PASSWORD=s3cr3t\nOTHER=x\n")
	chdir(t, root)
	t.Setenv("KONTRA_DUCKLAKE_CATALOG", "")

	got, err := lakeCatalog("")
	if err != nil {
		t.Fatalf("lakeCatalog: %v", err)
	}
	want := "postgres:dbname=kontra_ducklake host=localhost port=5432 user=kontra password=s3cr3t"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The real environment beats .env, matching docker compose's own precedence — otherwise the CLI
// and the stack could disagree about the password after an operator overrides one of them.
func TestCheckoutPasswordFromEnvironmentWins(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "docker-compose.yml"),
		"      KONTRA_DUCKLAKE_CATALOG: \"postgres:user=kontra password=${KONTRA_DUCKLAKE_PG_PASSWORD}\"\n")
	write(t, filepath.Join(root, ".env"), "KONTRA_DUCKLAKE_PG_PASSWORD=from-dotenv\n")
	chdir(t, root)
	t.Setenv("KONTRA_DUCKLAKE_PG_PASSWORD", "from-environ")

	if got := catalogFromCheckout(); got != "postgres:user=kontra password=from-environ" {
		t.Errorf("got %q", got)
	}
}

// A .env with no password leaves ${…} unexpanded. Returning that would surface as a Postgres
// authentication failure deep inside DuckDB; the discovery must decline so the caller can print
// something actionable instead.
func TestUnexpandedPlaceholderIsNotADSN(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "docker-compose.yml"),
		"      KONTRA_DUCKLAKE_CATALOG: \"postgres:password=${KONTRA_DUCKLAKE_PG_PASSWORD}\"\n")
	chdir(t, root)
	t.Setenv("KONTRA_DUCKLAKE_PG_PASSWORD", "")

	if got := catalogFromCheckout(); got != "" {
		t.Errorf("expected no DSN, got %q", got)
	}
}

// --catalog is the escape hatch for a catalog that is not this stack's, so it is passed through
// byte-for-byte — including a hostname the rewrite would otherwise claim.
func TestExplicitCatalogIsNotRewritten(t *testing.T) {
	const dsn = "postgres:dbname=other host=safedeps-postgres port=5432"
	got, err := lakeCatalog(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if got != dsn {
		t.Errorf("override was rewritten: %q", got)
	}
}

// Outside a checkout with no stack reachable, the error must say where the value normally comes
// from and show the flag — the previous message named an env var and stopped there.
func TestNoSourceGivesAnActionableError(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("KONTRA_DUCKLAKE_CATALOG", "")
	t.Setenv("PATH", "") // no docker binary to ask
	// AND NO APPLIANCE CATALOG EITHER, which is now the last source `lakeCatalog` tries. Without
	// this the test reads the DEVELOPER'S OWN ~/.kontra/data/datasets.ducklake and passes or fails
	// depending on whether that machine has ever materialized a Dataset.
	t.Setenv("KONTRA_HOME", t.TempDir())

	_, err := lakeCatalog("")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"docker-compose.yml", "--catalog"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// chdir moves into dir for the test. t.Chdir would do this, but the walk in findUp resolves
// symlinks differently from the temp dir on macOS/Linux, so the cwd is normalised here.
func chdir(t *testing.T, dir string) {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(real)
}

// A standalone dataset must be able to carry the SAME name as the actor that produces it.
// Actor names are hyphenated (layer-scan, registry-watch, docker-registry-monitor), and while
// datasetIdent rejected '-' every hand-made dataset had to invent an underscore alias — which
// is how `layer-scan` output ended up sitting beside a `docker_leak_findings` standalone
// holding the same rows under a name nothing could join back to the actor.
func TestDatasetIdentAcceptsActorNames(t *testing.T) {
	for _, name := range []string{"layer-scan", "registry-watch", "docker-registry-monitor", "crawl4ai", "scope_paid", "a1"} {
		got, err := datasetIdent(name)
		if err != nil {
			t.Fatalf("datasetIdent(%q) rejected an actor-shaped name: %v", name, err)
		}
		if got != name {
			t.Fatalf("datasetIdent(%q) = %q, want it unchanged", name, got)
		}
	}
}

// The hyphen is only safe because every interpolation quotes the identifier. A double quote
// would escape that quoting, so it stays rejected — the loop excludes it and an explicit check
// backs the loop up.
func TestDatasetIdentStillRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{
		"",          // empty
		"1leading",  // leading digit
		`has"quote`, // would escape the quoting at every use site
		"has space",
		"semi;colon",
		"dot.separated", // '.' is a schema separator, not part of a name
		"slash/name",
	} {
		if _, err := datasetIdent(name); err == nil {
			t.Fatalf("datasetIdent(%q) was accepted; it must be rejected", name)
		}
	}
}

// ── issue #3: a failure must not end with a box saying Success ─────────────────────────────────
//
// kontra sends several statements in one `-c`, so a failure on the third still carries the first
// two's RESULTS — and duckdb renders a successful DDL as a box containing the word `Success`. The
// reporter read `error: … / Success / true` as kontra contradicting itself, which is exactly what it
// looks like. Nothing they asked for succeeded; a statement they never typed did.
func TestDuckFailureDropsResultBoxesAndKeepsTheError(t *testing.T) {
	out := "Could not set lock on file \"/x/datasets.ducklake\": Conflicting lock held in /x/node (PID 42)\n" +
		"┌─────────┐\n│ Success │\n│  true   │\n└─────────┘\n"
	got := duckFailure(out)
	if !strings.Contains(got, "Conflicting lock") {
		t.Errorf("the error itself was dropped: %q", got)
	}
	if strings.Contains(got, "Success") || strings.Contains(got, "┌") {
		t.Errorf("a result box survived into a failure message: %q", got)
	}
}

func TestDuckFailureDropsEveryResultTableNotJustTheSuccessOne(t *testing.T) {
	// Matched on the box-drawing runes, not on the word: a `count(*)` from a statement that ran
	// before the failure is just as misleading as `Success`.
	out := "┌──────┐\n│ rows │\n│  912 │\n└──────┘\nIO Error: could not open file\n"
	got := duckFailure(out)
	if strings.Contains(got, "912") {
		t.Errorf("a count from a statement that ran before the failure survived: %q", got)
	}
	if !strings.Contains(got, "IO Error") {
		t.Errorf("the error itself was dropped: %q", got)
	}
}

func TestDuckFailureNamesTheControlPlaneWhenTheCatalogIsLocked(t *testing.T) {
	// duckdb names a PID and a path — true, and no help unless you already know the PID is your own
	// control plane.
	got := duckFailure("Could not set lock on file \"/x/datasets.ducklake\": Conflicting lock held in /x/node (PID 42)\n")
	if !strings.Contains(got, "kontra up") {
		t.Errorf("the lock message does not name what holds it: %q", got)
	}
}

func TestDuckFailureLeavesAnUnrelatedErrorAlone(t *testing.T) {
	// Non-vacuous partner: a helper that appended the lock advice to everything would pass the case
	// above and be wrong about every other failure.
	got := duckFailure("Parser Error: syntax error at or near \"SLECT\"\n")
	if strings.Contains(got, "kontra up") {
		t.Errorf("lock advice was added to an unrelated failure: %q", got)
	}
	if !strings.Contains(got, "Parser Error") {
		t.Errorf("the error itself was dropped: %q", got)
	}
}
