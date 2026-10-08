// runtimeimport.go — `kontra runtime import`: copy published runtimes into THIS install's registry.
//
// WHY AN INSTALL HAS TO MIRROR THEM AT ALL. `kontra deploy` layers an actor onto a run image
// resolved under `$KONTRA_REGISTRY/kontra-runtimes/` (ADR 0061), and that namespace is EMPTY on a
// fresh install — so the hello actor the install ships could not be built. The published set lives
// at `ghcr.io/<owner>/kontra-runtimes` and reads anonymously, which makes "just point
// KONTRA_RUNTIMES_PREFIX at ghcr" look like the answer. It is not expressible:
// `KONTRA_TRUST_REGISTRIES` would have to admit `ghcr.io`, and the runtimes there are keyless-signed
// PER RELEASE — identity `…/kontra-runtimes/.github/workflows/publish.yml@refs/tags/python/1.0.0` —
// while `trustpolicy` takes one exact `--certificate-identity` and no regexp. One string cannot
// cover three runtimes signed under three tag refs, and adding ghcr to KONTRA_TRUST_UNSIGNED would
// throw away a signature that exists. Copying them here leaves one address that is already on the
// allowlist and the same bytes for every actor built on this install.
//
// WHY NOT `kontra registry migrate`. That command drains a whole `registry:2` store into zot and
// starts by listing the source catalog; `GET /v2/_catalog` against ghcr answers **403 DENIED**, so
// it cannot see what to copy. This one is given the names.
//
// WHY NOT `docker pull && docker tag && docker push`. The daemon is not needed to move a manifest
// between two registries, and using it would mean the `cli` container had to carry a docker CLI, a
// socket and the platform the local daemon happens to run — a multi-arch runtime would arrive
// flattened to one architecture. `oras.Copy` moves the manifest and its blobs as published.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

const runtimeUsage = `usage: kontra runtime import [name:major ...] [--from <prefix>] [--to <registry>] [--dry-run] [--force]
       kontra runtime list

  import  Copies published runtimes into this install's registry under kontra-runtimes/, by
          digest, and verifies each one resolves there afterwards. Idempotent: a runtime already
          present is left alone.

          Which runtimes: the arguments, else KONTRA_RUNTIMES_IMPORT, else whatever --from's
          catalog lists (ghcr does not serve one, so there it must be named).

  --from     where to copy FROM. Default: KONTRA_RUNTIMES_SOURCE, else the published set.
  --to       the registry to copy INTO. Default: --registry / KONTRA_REGISTRY.
  --dry-run  resolve and report; copy nothing.
  --force    re-copy a runtime whose major tag has moved at the source.
`

// publishedRuntimes is where the runtimes this project publishes live. An ADDRESS, not a list of
// what is there: §3.4's invariant is that adding a runtime means pushing one, never editing kontra.
const publishedRuntimes = "ghcr.io/medmahmoudi26/kontra-runtimes"

// runtimesImportEnv names which runtimes an install mirrors. It is configuration and not a constant
// here for the same reason: a fork mirrors its own set by editing its `.env`, not this file.
const runtimesImportEnv = "KONTRA_RUNTIMES_IMPORT"

// runtimesSourceEnv is where they are copied from — re-pointed by an airgapped install at a
// registry it can actually reach.
const runtimesSourceEnv = "KONTRA_RUNTIMES_SOURCE"

// importTimeout bounds the whole copy. Three run images are a few hundred MB over a public network;
// the deadline exists so a source that accepts a connection and stalls cannot hang first boot.
const importTimeout = 20 * time.Minute

func cmdRuntime(args []string) error {
	if len(args) == 0 {
		return errors.New(runtimeUsage)
	}
	switch args[0] {
	case "import":
		return cmdRuntimeImport(args[1:])
	case "list":
		return cmdRuntimeList(args[1:])
	default:
		return errors.New(runtimeUsage)
	}
}

func cmdRuntimeList(args []string) error {
	fs := flag.NewFlagSet("runtime list", flag.ContinueOnError)
	fs.SetOutput(cliio.Stdout)
	reg := fs.String("registry", "", "the registry to read (default: --registry / KONTRA_REGISTRY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prefix := runtimesPrefix(registryAddress(*reg))
	names, err := runtimesUnder(prefix)
	if err != nil {
		return fmt.Errorf("listing runtimes under %s: %w", prefix, err)
	}
	if len(names) == 0 {
		return fmt.Errorf("no runtimes are published under %s.\n"+
			"  `kontra runtime import` copies the published set into this install's registry.", prefix)
	}
	for _, n := range names {
		fmt.Fprintln(cliio.Stdout, n)
	}
	return nil
}

func cmdRuntimeImport(args []string) error {
	fs := flag.NewFlagSet("runtime import", flag.ContinueOnError)
	fs.SetOutput(cliio.Stdout)
	from := fs.String("from", "", "the prefix to copy from (default: "+runtimesSourceEnv+", else the published set)")
	to := fs.String("to", "", "the registry to copy into (default: --registry / KONTRA_REGISTRY)")
	dryRun := fs.Bool("dry-run", false, "resolve and report; copy nothing")
	force := fs.Bool("force", false, "re-copy a runtime whose major tag has moved at the source")
	if err := fs.Parse(args); err != nil {
		return err
	}

	src := runtimesSource(*from)
	dstPrefix := runtimesPrefix(registryAddress(*to))
	srcHost, srcRepos, err := splitPrefix(src)
	if err != nil {
		return fmt.Errorf("--from %q: %w", src, err)
	}
	dstHost, dstRepos, err := splitPrefix(dstPrefix)
	if err != nil {
		return fmt.Errorf("--to resolves to %q: %w", dstPrefix, err)
	}
	// THE SAME PREFIX IS A NO-OP THAT LOOKS LIKE A MIRROR — and it is the configuration that
	// actually happens: KONTRA_RUNTIMES_PREFIX pointed at the published set makes the destination
	// ghcr, so this would "copy" every runtime onto itself and first boot would report success with
	// an empty local namespace.
	if srcHost == dstHost && srcRepos == dstRepos {
		return fmt.Errorf("source and destination are both %s/%s; nothing would move.\n"+
			"  KONTRA_RUNTIMES_PREFIX is where actors RESOLVE a runtime and has to name this\n"+
			"  install's registry; %s is where they are copied from.", srcHost, srcRepos, runtimesSourceEnv)
	}

	names, err := importNames(fs.Args(), src)
	if err != nil {
		return err
	}

	cred := runtimesPushCredential()
	if err := registryAdmits(dstHost, cred); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()

	out := cliio.Stdout
	fmt.Fprintf(out, "importing %d runtime(s)\n  from %s/%s\n    to %s/%s\n",
		len(names), srcHost, srcRepos, dstHost, dstRepos)
	if *dryRun {
		fmt.Fprintln(out, "  (--dry-run: nothing will be written)")
	}

	var rows []importRow
	for _, n := range names {
		rows = append(rows, importOneRuntime(ctx, importArgs{
			SrcPrefix: src, DstPrefix: dstPrefix,
			Declared: n, Cred: cred, DryRun: *dryRun, Force: *force,
		}))
	}

	w := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "RUNTIME\tDIGEST\tRESULT")
	copied, failed, skipped := 0, 0, 0
	for _, r := range rows {
		switch {
		case r.Err != nil:
			failed++
			fmt.Fprintf(w, "%s\t%s\t%v\n", r.Name, short(r.Digest), r.Err)
		case r.Note != "":
			skipped++
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.Name, short(r.Digest), r.Note)
		default:
			copied++
			fmt.Fprintf(w, "%s\t%s\tcopied\n", r.Name, short(r.Digest))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%d copied, %d already present or skipped, %d failed\n", copied, skipped, failed)

	if failed > 0 {
		// THE MESSAGE HAS TO SAY WHAT CANNOT HAPPEN NEXT, because the operator reading it is
		// usually not importing for its own sake — they are about to deploy an actor.
		return fmt.Errorf("%d runtime(s) did not arrive at %s/%s — an actor that declares one of them "+
			"cannot be built until it does", failed, dstHost, dstRepos)
	}
	return nil
}

// runtimesSource is where runtimes are copied FROM: the flag, the environment, then the published
// set. Deliberately NOT runtimesPrefix's order — that one answers "where does an actor resolve a
// runtime", which must be this install, and conflating the two is how the mirror came to copy ghcr
// onto ghcr.
func runtimesSource(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv(runtimesSourceEnv)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return publishedRuntimes
}

// splitPrefix separates `host[:port]/path/to/namespace` into the host a request goes to and the
// repository path a name hangs off. A prefix with no path has no namespace to copy into.
func splitPrefix(prefix string) (host, repos string, err error) {
	h, _ := registryHost(prefix)
	host, repos, ok := strings.Cut(h, "/")
	if !ok || repos == "" {
		return "", "", errors.New("names no repository path (expected <registry>/<namespace>)")
	}
	return host, strings.Trim(repos, "/"), nil
}

// importNames decides which runtimes to copy, and refuses in a way that names every way to say so.
//
// DISCOVERY IS TRIED AND IS NOT RELIED ON. `GET /v2/_catalog` is how `kontra runtime list` finds
// what an install has, and ghcr answers it 403 — so against the published set the names have to
// come from the arguments or the environment, and a refusal that did not say that would read as
// "there are no runtimes".
func importNames(args []string, src string) ([]string, error) {
	if named := splitNames(strings.Join(args, " ")); len(named) > 0 {
		return named, nil
	}
	if named := splitNames(os.Getenv(runtimesImportEnv)); len(named) > 0 {
		return named, nil
	}
	found, err := runtimesUnder(src)
	if err == nil && len(found) > 0 {
		return found, nil
	}
	return nil, fmt.Errorf("which runtimes? %s does not serve a catalog this can read%s.\n"+
		"  Name them: `kontra runtime import base:1 python:1`\n"+
		"  or set %s (docker-compose.yml passes it to the `cli` service).",
		src, catalogWhy(err), runtimesImportEnv)
}

func catalogWhy(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

// splitNames accepts `a:1,b:1` and `a:1 b:1` alike, because one is what an operator types and the
// other is what a `.env` line looks like.
func splitNames(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

type importArgs struct {
	SrcPrefix, DstPrefix string
	Declared             string
	Cred                 registryCredential
	DryRun, Force        bool
}

type importRow struct {
	Name   string
	Digest string
	Note   string // "already present", "would copy", "major moved" — a result that is not an error
	Err    error
}

// importOneRuntime copies one `<name>:<major>`, and confirms the destination holds the digest the
// source reported.
//
// THE DESTINATION IS CHECKED FIRST AND THAT IS WHAT MAKES THIS IDEMPOTENT. First boot runs this
// every time the `cli` service starts; a re-copy per boot would pull hundreds of MB and, worse,
// silently advance the base image under every actor already built here the day the major tag moves
// upstream. A moved major is REPORTED and needs --force.
func importOneRuntime(ctx context.Context, a importArgs) importRow {
	// A NAME AND A MAJOR, NEVER A REFERENCE. `--from` is the only thing that says where runtimes
	// come from; a qualified `ghcr.io/x/python:1` here would be read for its last path element and
	// copied from somewhere else entirely, which is a silent wrong answer.
	if strings.Contains(a.Declared, "/") {
		return importRow{Name: a.Declared, Err: errors.New("a runtime is imported by `name:major`; " +
			"use --from to change where it is copied from")}
	}
	name, major, _, err := parseRuntimeDeclaration(a.Declared, "")
	if err != nil {
		return importRow{Name: a.Declared, Err: err}
	}
	tag := fmt.Sprintf("%d", major)
	row := importRow{Name: name + ":" + tag}

	srcHost, srcRepos, err := splitPrefix(a.SrcPrefix)
	if err != nil {
		return importRow{Name: row.Name, Err: err}
	}
	dstHost, dstRepos, err := splitPrefix(a.DstPrefix)
	if err != nil {
		return importRow{Name: row.Name, Err: err}
	}
	srcRepo, dstRepo := srcRepos+"/"+name, dstRepos+"/"+name

	want, err := registryManifestDigest(srcHost, srcRepo, tag)
	switch {
	case errors.Is(err, errNotInRegistry):
		row.Err = fmt.Errorf("not published at %s/%s:%s", srcHost, srcRepo, tag)
		return row
	case err != nil:
		row.Err = fmt.Errorf("reading the source: %w", err)
		return row
	}
	row.Digest = want

	have, herr := registryManifestDigest(dstHost, dstRepo, tag)
	switch {
	case herr == nil && have == want:
		row.Note = "already present"
		return row
	case herr == nil && !a.Force:
		row.Note = "the major has moved at the source; --force to replace " + short(have)
		return row
	case herr != nil && !errors.Is(herr, errNotInRegistry):
		row.Err = fmt.Errorf("asking the destination: %w", herr)
		return row
	}
	if a.DryRun {
		row.Note = "would copy"
		return row
	}

	src, err := remote.NewRepository(srcHost + "/" + srcRepo)
	if err != nil {
		row.Err = fmt.Errorf("source reference: %w", err)
		return row
	}
	_, src.PlainHTTP = registryHost(a.SrcPrefix)

	// THE ADDRESS THE DAEMON USES IS NOT THE ADDRESS THIS PROCESS CAN OPEN. `127.0.0.1:5000` is
	// zot as the HOST sees it, and this runs in the `cli` container; the copy speaks HTTP itself,
	// so it needs the way in from here. Only the transport changes — a manifest is stored under its
	// REPOSITORY, so the actor build that later pulls `127.0.0.1:5000/kontra-runtimes/python:1`
	// through the daemon gets exactly these bytes.
	pushHost := reachableRegistryHost(dstHost)
	dst, err := remote.NewRepository(pushHost + "/" + dstRepo)
	if err != nil {
		row.Err = fmt.Errorf("destination reference: %w", err)
		return row
	}
	_, dst.PlainHTTP = registryHost(a.DstPrefix)
	// THE DESTINATION NEEDS THE CREDENTIAL AND THE SOURCE MUST NOT GET IT. zot refuses an anonymous
	// write to `kontra-runtimes/**`; the source is somebody else's registry, and sending this
	// install's password to it would be a credential leak for no benefit — a public read needs a
	// token from the realm the registry names, which oras's own client fetches without one.
	dst.Client = &auth.Client{
		Client: retry.DefaultClient,
		Cache:  auth.NewCache(),
		// KEYED ON THE HOST THE REQUEST ACTUALLY GOES TO. oras matches a static credential by
		// address, so keying it on the daemon's spelling while connecting to another would send an
		// anonymous push and surface as a 401 with nothing about a credential in it.
		Credential: auth.StaticCredential(pushHost, auth.Credential{Username: a.Cred.User, Password: a.Cred.Password}),
	}

	if _, err := oras.Copy(ctx, src, tag, dst, tag, oras.DefaultCopyOptions); err != nil {
		row.Err = err
		return row
	}

	// COPIED BY TAG, CONFIRMED BY DIGEST. A copy that reports success and lands different bytes
	// under the same name is invisible to every listing and wrong for every actor built on it.
	got, err := registryManifestDigest(dstHost, dstRepo, tag)
	if err != nil {
		row.Err = fmt.Errorf("confirming at the destination: %w", err)
		return row
	}
	if got != want {
		row.Err = fmt.Errorf("digest mismatch: source %s, destination %s", short(want), short(got))
	}
	return row
}
