package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// WHAT THESE TESTS PIN. The install came up with an EMPTY `kontra-runtimes/` namespace and the
// first `kontra deploy` refused with "registry is not on this Machine's allowlist: ghcr.io" — the
// published set was being resolved where it is published rather than copied where it is trusted.
// Every assertion below is about one of the three ways the fix can be silently wrong: copying a
// registry onto itself, copying nothing and reporting success, or re-copying on every boot and
// moving the base under actors that are already built.

// mirrorRegistry answers manifest HEADs from a map of `<repo>:<tag>` to digest, and records every
// write so a test can assert that nothing was pushed.
type mirrorRegistry struct {
	addr   string
	writes []string
}

func newMirrorRegistry(t *testing.T, have map[string]string) *mirrorRegistry {
	t.Helper()
	f := &mirrorRegistry{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		repo, ref, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		digest, there := have[repo+":"+ref]
		if !there {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	f.addr = strings.TrimPrefix(ts.URL, "http://")
	return f
}

const (
	digestA = "sha256:712b8d2e71e383b1c554464e7d45e5bee3a7f1acce4b69ab47194e396c03399e"
	digestB = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

func TestImportingARegistryOntoItselfIsRefusedRatherThanReportedDone(t *testing.T) {
	// THE CONFIGURATION THAT ACTUALLY HAPPENED. KONTRA_RUNTIMES_PREFIX pointed at the published set
	// to make a fresh install able to resolve a runtime; with the mirror added, source and
	// destination became the same address and the copy was a no-op that printed success.
	t.Setenv("KONTRA_RUNTIMES_PREFIX", publishedRuntimes)
	t.Setenv(runtimesSourceEnv, publishedRuntimes)
	err := cmdRuntimeImport(nil)
	if err == nil {
		t.Fatal("copying a prefix onto itself must be refused")
	}
	for _, want := range []string{"nothing would move", "KONTRA_RUNTIMES_PREFIX", runtimesSourceEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must distinguish the two variables; %q is missing from:\n%v", want, err)
		}
	}
}

func TestWhichRuntimesComesFromArgumentsThenTheEnvironment(t *testing.T) {
	t.Setenv(runtimesImportEnv, "base:1")
	got, err := importNames([]string{"python:1", "python-browser:1"}, "ghcr.io/x/kontra-runtimes")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "python-browser:1 python:1" {
		t.Errorf("arguments must win over %s, got %v", runtimesImportEnv, got)
	}
	if got, err = importNames(nil, "ghcr.io/x/kontra-runtimes"); err != nil || strings.Join(got, " ") != "base:1" {
		t.Errorf("with no arguments the environment decides, got %v (%v)", got, err)
	}
}

func TestWithNoNamesAndNoCatalogTheRefusalNamesBothWaysToSaySo(t *testing.T) {
	t.Setenv(runtimesImportEnv, "")
	// A SOURCE THAT SERVES NO CATALOG IS THE NORMAL CASE, measured: ghcr answers `GET /v2/_catalog`
	// with 403 DENIED, so discovery cannot be the only way to name the published set.
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(denied.Close)
	_, err := importNames(nil, strings.TrimPrefix(denied.URL, "http://")+"/kontra-runtimes")
	if err == nil {
		t.Fatal("a source with no catalog and no names must refuse, not import nothing")
	}
	for _, want := range []string{runtimesImportEnv, "kontra runtime import base:1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say how to name them; %q is missing from:\n%v", want, err)
		}
	}
}

func TestNamesAreAcceptedCommaSeparatedBecauseThatIsWhatAnEnvLineLooksLike(t *testing.T) {
	got := splitNames("base:1, python:1\tpython-browser:1\n")
	if strings.Join(got, "|") != "base:1|python-browser:1|python:1" {
		t.Errorf("got %v", got)
	}
	if n := splitNames("   "); len(n) != 0 {
		t.Errorf("blank must be no names, not one empty one: %q", n)
	}
}

func TestARuntimeAlreadyPresentIsNotCopiedAgain(t *testing.T) {
	// IDEMPOTENCE IS NOT A NICETY HERE: this runs on every boot of the `cli` service, and a
	// re-copy per boot would pull hundreds of MB and re-push them to the install's own registry.
	src := newMirrorRegistry(t, map[string]string{"kontra-runtimes/python:1": digestA})
	dst := newMirrorRegistry(t, map[string]string{"kontra-runtimes/python:1": digestA})
	row := importOneRuntime(context.Background(), importArgs{
		SrcPrefix: src.addr + "/kontra-runtimes",
		DstPrefix: dst.addr + "/kontra-runtimes",
		Declared:  "python:1",
	})
	if row.Err != nil {
		t.Fatalf("a runtime already present is not an error: %v", row.Err)
	}
	if row.Note != "already present" {
		t.Errorf("note = %q, want it to say the copy was skipped", row.Note)
	}
	if len(dst.writes) != 0 {
		t.Errorf("nothing may be written when the digests already agree, got %v", dst.writes)
	}
}

func TestAMajorThatMovedUpstreamIsReportedAndNotRolledUnderBuiltActors(t *testing.T) {
	// A MOVED MAJOR IS AN UPDATE, NOT A REPAIR. Replacing it silently would change the base image
	// under every actor already built here, which ADR 0061 makes `kontra rebase`'s decision.
	src := newMirrorRegistry(t, map[string]string{"kontra-runtimes/python:1": digestA})
	dst := newMirrorRegistry(t, map[string]string{"kontra-runtimes/python:1": digestB})
	row := importOneRuntime(context.Background(), importArgs{
		SrcPrefix: src.addr + "/kontra-runtimes",
		DstPrefix: dst.addr + "/kontra-runtimes",
		Declared:  "python:1",
	})
	if row.Err != nil {
		t.Fatalf("a moved major is reported, not failed: %v", row.Err)
	}
	if !strings.Contains(row.Note, "--force") {
		t.Errorf("note = %q, want it to name the flag that would replace it", row.Note)
	}
	if len(dst.writes) != 0 {
		t.Errorf("a moved major must not be written without --force, got %v", dst.writes)
	}
}

func TestADryRunResolvesAndWritesNothing(t *testing.T) {
	src := newMirrorRegistry(t, map[string]string{"kontra-runtimes/base:1": digestA})
	dst := newMirrorRegistry(t, nil)
	row := importOneRuntime(context.Background(), importArgs{
		SrcPrefix: src.addr + "/kontra-runtimes",
		DstPrefix: dst.addr + "/kontra-runtimes",
		Declared:  "base:1", DryRun: true,
	})
	if row.Err != nil || row.Note != "would copy" {
		t.Errorf("row = %+v, want a resolved dry run", row)
	}
	if row.Digest != digestA {
		t.Errorf("a dry run must still report the digest it would copy, got %q", row.Digest)
	}
	if len(dst.writes) != 0 {
		t.Errorf("--dry-run wrote %v", dst.writes)
	}
}

func TestARuntimeTheSourceDoesNotPublishSaysWhereItLooked(t *testing.T) {
	src := newMirrorRegistry(t, nil)
	dst := newMirrorRegistry(t, nil)
	row := importOneRuntime(context.Background(), importArgs{
		SrcPrefix: src.addr + "/kontra-runtimes",
		DstPrefix: dst.addr + "/kontra-runtimes",
		Declared:  "nosuch:1",
	})
	if row.Err == nil {
		t.Fatal("a runtime that is not published must be an error")
	}
	if !strings.Contains(row.Err.Error(), src.addr+"/kontra-runtimes/nosuch:1") {
		t.Errorf("the error must name the address it asked: %v", row.Err)
	}
}

func TestAQualifiedReferenceIsRefusedRatherThanCopiedFromSomewhereElse(t *testing.T) {
	row := importOneRuntime(context.Background(), importArgs{
		SrcPrefix: "127.0.0.1:5000/kontra-runtimes",
		DstPrefix: "127.0.0.1:5001/kontra-runtimes",
		Declared:  "ghcr.io/other/python:1",
	})
	if row.Err == nil || !strings.Contains(row.Err.Error(), "--from") {
		// `parseRuntimeDeclaration` reads a qualified reference for its LAST path element, so this
		// would have imported `python:1` from --from and reported the qualified name as copied.
		t.Errorf("a qualified reference must be refused and point at --from, got %+v", row)
	}
}

func TestTheSourceIsTheFlagThenTheEnvironmentThenThePublishedSet(t *testing.T) {
	t.Setenv(runtimesSourceEnv, "mirror.internal/kontra-runtimes")
	if got := runtimesSource("ghcr.io/fork/kontra-runtimes"); got != "ghcr.io/fork/kontra-runtimes" {
		t.Errorf("--from must win, got %q", got)
	}
	if got := runtimesSource(""); got != "mirror.internal/kontra-runtimes" {
		t.Errorf("%s must be consulted, got %q", runtimesSourceEnv, got)
	}
	t.Setenv(runtimesSourceEnv, "")
	if got := runtimesSource(""); got != publishedRuntimes {
		t.Errorf("with nothing set the published set is the source, got %q", got)
	}
}

func TestAPrefixWithNoNamespaceIsRefused(t *testing.T) {
	if _, _, err := splitPrefix("127.0.0.1:5000"); err == nil {
		t.Error("a registry with no repository path has no namespace to copy into")
	}
	host, repos, err := splitPrefix("https://ghcr.io/owner/kontra-runtimes/")
	if err != nil || host != "ghcr.io" || repos != "owner/kontra-runtimes" {
		t.Errorf("got (%q, %q, %v)", host, repos, err)
	}
}

func TestTheRuntimesRefusalNamesTheRuntimesVariableAndNotTheActorsOne(t *testing.T) {
	// THE TWO ACCOUNTS ARE NOT INTERCHANGEABLE, and a 401 that named the actors password would send
	// an operator to set a secret this push never reads.
	reg, _ := probe(t, http.StatusUnauthorized)
	t.Setenv(runtimesUserEnv, "")
	t.Setenv(runtimesPasswordEnv, "")
	err := registryAdmits(reg, runtimesPushCredential())
	if err == nil {
		t.Fatal("a 401 against an anonymous runtimes push must be refused")
	}
	if !strings.Contains(err.Error(), runtimesPasswordEnv) {
		t.Errorf("the refusal must name %s:\n%v", runtimesPasswordEnv, err)
	}
	if strings.Contains(err.Error(), pushPasswordEnv) {
		t.Errorf("the refusal must NOT name the actors password:\n%v", err)
	}
	if c := runtimesPushCredential(); c.User != defaultRuntimesUser {
		t.Errorf("user = %q, want the account docker-compose.yml writes (%q)", c.User, defaultRuntimesUser)
	}
}
