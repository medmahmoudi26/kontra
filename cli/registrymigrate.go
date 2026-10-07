// registrymigrate.go — `kontra registry migrate`: move every repository and tag from the
// `registry:2` store into zot, by digest, and prove it arrived.
//
// WHY A COMMAND AND NOT A SWAP. zot's on-disk layout is not `registry:2`'s, so pointing the new
// service at the old volume presents an EMPTY registry — and nothing in this system distinguishes an
// empty registry from a fresh one (`GET /api/images` answers 200 with `[]`, the console draws `?`,
// `kontra doctor` has no registry check, and `versionDeployed()` stops guarding version
// immutability). ADR 0052 records what is at stake: this store "holds the digests Placements are
// pinned to … rebuilding an image yields a NEW digest, so a Fleet recorded against the old one can
// never be re-run as recorded."
//
// THE DESTINATION'S DIGEST IS CHECKED, NOT ASSUMED. A copy that reports success and lands different
// bytes is the failure this is guarding against, so every tag is re-read from the destination and
// compared with the source's `Docker-Content-Digest` — the same authority `confirmPushed` uses.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

const registryUsage = `usage: kontra registry migrate --from <host:port> [--to <host:port>] [--dry-run]

  Copies every repository and tag from a registry:2 store into zot, by digest, then verifies that
  each one resolves at the destination and that every digest the catalog knows about is present.

  --from     the old registry (required). A scheme may be given; without one, plain HTTP.
  --to       the new registry. Defaults to --registry / KONTRA_REGISTRY.
  --dry-run  list what would be copied and copy nothing.
`

// migrateTimeout bounds the whole copy. 7.9 GiB over loopback is seconds, but a destination that
// accepts a connection and then stalls would otherwise hang an operator's terminal with no output.
const migrateTimeout = 30 * time.Minute

func cmdRegistry(args []string) error {
	if len(args) == 0 {
		return errors.New(registryUsage)
	}
	switch args[0] {
	case "migrate":
		return cmdRegistryMigrate(args[1:])
	default:
		return errors.New(registryUsage)
	}
}

type migrateRow struct {
	Repo, Tag, Digest string
	Err               error
	Skipped           bool
}

func cmdRegistryMigrate(args []string) error {
	fs := flag.NewFlagSet("registry migrate", flag.ContinueOnError)
	fs.SetOutput(cliio.Stdout)
	from := fs.String("from", "", "the registry:2 address to read from")
	to := fs.String("to", "", "the zot address to write to (default: --registry / KONTRA_REGISTRY)")
	dryRun := fs.Bool("dry-run", false, "list what would be copied and copy nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New(registryUsage)
	}
	dst := registryAddress(*to)

	srcHost, srcPlain := registryHost(*from)
	dstHost, dstPlain := registryHost(dst)
	// SAME ADDRESS IS A NO-OP THAT LOOKS LIKE A MIGRATION. It would copy every tag onto itself,
	// report success, and leave the operator believing the old store had been drained.
	if srcHost == dstHost {
		return fmt.Errorf("--from and --to are the same registry (%s); nothing would move", srcHost)
	}

	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()

	repos, err := registryCatalog(ctx, *from)
	if err != nil {
		return fmt.Errorf("listing repositories at %s: %w", srcHost, err)
	}
	if len(repos) == 0 {
		return fmt.Errorf("%s has no repositories; nothing to migrate (is that the old registry?)", srcHost)
	}

	out := cliio.Stdout
	fmt.Fprintf(out, "migrating %d repositories\n  from %s\n    to %s\n", len(repos), srcHost, dstHost)
	if *dryRun {
		fmt.Fprintln(out, "  (--dry-run: nothing will be written)")
	}

	var rows []migrateRow
	for _, repo := range repos {
		tags, err := registryTags(ctx, *from, repo)
		if err != nil {
			rows = append(rows, migrateRow{Repo: repo, Err: fmt.Errorf("listing tags: %w", err)})
			continue
		}
		for _, tag := range tags {
			rows = append(rows, migrateOneTag(ctx, migrateTagArgs{
				SrcAddr: *from, SrcHost: srcHost, SrcPlain: srcPlain,
				DstHost: dstHost, DstPlain: dstPlain,
				Repo: repo, Tag: tag, DryRun: *dryRun,
			}))
		}
	}

	w := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "REPOSITORY\tTAG\tDIGEST\tRESULT")
	copied, failed, skipped := 0, 0, 0
	for _, r := range rows {
		switch {
		case r.Err != nil:
			failed++
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\n", r.Repo, r.Tag, short(r.Digest), r.Err)
		case r.Skipped:
			skipped++
			fmt.Fprintf(w, "%s\t%s\t%s\twould copy\n", r.Repo, r.Tag, short(r.Digest))
		default:
			copied++
			fmt.Fprintf(w, "%s\t%s\t%s\tok\n", r.Repo, r.Tag, short(r.Digest))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if *dryRun {
		fmt.Fprintf(out, "\n%d tags would be copied, %d could not be read\n", skipped, failed)
		return nil
	}
	fmt.Fprintf(out, "\n%d tags copied, %d failed\n", copied, failed)

	// A TAG-COMPLETE COPY IS NOT A COMPLETE COPY, and this is measured rather than theoretical: on the
	// install this was written against, 27 of 27 tags copied with matching digests and TWO catalog
	// digests still did not resolve — `desync@177f80c8` and `webcrawl@e09d6df4`, both present in the
	// old store, both reachable by NO tag. A later push had moved the tag and the catalog kept the
	// older digest. ADR 0052 names exactly this: "rebuilding an image yields a NEW digest, so a Fleet
	// recorded against the old one can never be re-run as recorded."
	//
	// So untagged manifests the catalog still references are copied BY DIGEST, by graph, because
	// there is no name to copy them under.
	if n, err := copyCatalogDigests(ctx, srcHost, srcPlain, dstHost, dstPlain, dst, out); err != nil {
		return err
	} else if n > 0 {
		fmt.Fprintf(out, "%d untagged manifest(s) the catalog still references copied by digest\n", n)
	}

	// THE CATALOG IS THE SECOND OPINION, and it is the one that matters to a Run.
	missing, checked, cerr := catalogDigestsMissing(dst)
	switch {
	case cerr != nil:
		fmt.Fprintf(out, "could not cross-check the catalog (%v)\n"+
			"  the copy above is unverified against what Placements are pinned to; run\n"+
			"  `kontra registry migrate --from %s --dry-run` again once the orchestrator is reachable\n",
			cerr, srcHost)
	case len(missing) > 0:
		for _, m := range missing {
			fmt.Fprintf(out, "MISSING at %s: %s\n", dstHost, m)
		}
		return fmt.Errorf("%d of %d catalog digests do not resolve at %s — do NOT remove the old volume",
			len(missing), checked, dstHost)
	default:
		fmt.Fprintf(out, "all %d catalog digests resolve at %s\n", checked, dstHost)
	}

	if failed > 0 {
		return fmt.Errorf("%d tags failed to copy — the old volume still holds the only copy", failed)
	}
	// The volume name carries the compose project, which defaults to `kontra`
	// (`docker-compose.yml:51`, `${COMPOSE_PROJECT_NAME:-kontra}`). Printed rather than run: dropping
	// 7.9 GiB of digests Placements are pinned to is the operator's call, not this command's.
	fmt.Fprintf(out, "\nthe old volume is untouched. Remove it when you are satisfied:\n"+
		"  docker volume rm ${COMPOSE_PROJECT_NAME:-kontra}_registry-data\n")
	return nil
}

type migrateTagArgs struct {
	SrcAddr, SrcHost, DstHost string
	SrcPlain, DstPlain        bool
	Repo, Tag                 string
	DryRun                    bool
}

func migrateOneTag(ctx context.Context, a migrateTagArgs) migrateRow {
	row := migrateRow{Repo: a.Repo, Tag: a.Tag, Skipped: a.DryRun}

	want, err := registryManifestDigest(a.SrcAddr, a.Repo, a.Tag)
	if err != nil {
		row.Err = fmt.Errorf("reading source digest: %w", err)
		return row
	}
	row.Digest = want
	if a.DryRun {
		return row
	}

	src, err := remote.NewRepository(a.SrcHost + "/" + a.Repo)
	if err != nil {
		row.Err = fmt.Errorf("source reference: %w", err)
		return row
	}
	src.PlainHTTP = a.SrcPlain
	dst, err := remote.NewRepository(a.DstHost + "/" + a.Repo)
	if err != nil {
		row.Err = fmt.Errorf("destination reference: %w", err)
		return row
	}
	dst.PlainHTTP = a.DstPlain

	// Copied by TAG in both directions so the destination carries the same name the catalog and a
	// Placement resolve, and verified by DIGEST below so the name is not the only thing that agreed.
	if _, err := oras.Copy(ctx, src, a.Tag, dst, a.Tag, oras.DefaultCopyOptions); err != nil {
		row.Err = err
		return row
	}

	got, err := registryManifestDigest(a.DstHost, a.Repo, a.Tag)
	if err != nil {
		row.Err = fmt.Errorf("confirming at destination: %w", err)
		return row
	}
	if got != want {
		// A copy that lands different bytes under the same name is the one failure that would
		// otherwise be invisible: every listing agrees and every pull gets the wrong image.
		row.Err = fmt.Errorf("digest mismatch: source %s, destination %s", short(want), short(got))
	}
	return row
}

// copyCatalogDigests copies every digest the catalog records that the destination cannot already
// serve. These are manifests with no tag pointing at them, so there is no name to copy under and
// `oras.CopyGraph` is the call — it walks the manifest and its blobs and writes them untagged.
//
// An unreachable orchestrator is NOT an error here: the tag pass has already run, and the cross-check
// below reports the same condition with the right wording. Returning an error would make a copy that
// succeeded look like one that failed.
func copyCatalogDigests(ctx context.Context, srcHost string, srcPlain bool, dstHost string, dstPlain bool, dstAddr string, out io.Writer) (int, error) {
	var actors []actorRecord
	if err := newAPI(orchestratorURL()).getJSON("/api/actors", &actors); err != nil {
		return 0, nil
	}
	seen := map[string]bool{}
	copied := 0
	for _, a := range actors {
		if a.Digest == "" || seen[a.Name+"@"+a.Digest] {
			continue
		}
		seen[a.Name+"@"+a.Digest] = true
		if _, err := registryManifestDigest(dstAddr, a.Name, a.Digest); err == nil {
			continue // already there, under some tag
		}
		src, err := remote.NewRepository(srcHost + "/" + a.Name)
		if err != nil {
			return copied, fmt.Errorf("source reference for %s: %w", a.Name, err)
		}
		src.PlainHTTP = srcPlain
		dst, err := remote.NewRepository(dstHost + "/" + a.Name)
		if err != nil {
			return copied, fmt.Errorf("destination reference for %s: %w", a.Name, err)
		}
		dst.PlainHTTP = dstPlain

		desc, err := src.Resolve(ctx, a.Digest)
		if err != nil {
			// The catalog references something the OLD store cannot serve either. That is
			// pre-existing loss, not a migration failure, and saying so is the honest report.
			fmt.Fprintf(out, "  %s@%s (%s) is in the catalog and in neither registry — pre-existing loss\n",
				a.Name, short(a.Digest), a.Version)
			continue
		}
		if err := oras.CopyGraph(ctx, src, dst, desc, oras.DefaultCopyGraphOptions); err != nil {
			return copied, fmt.Errorf("copying %s@%s by digest: %w", a.Name, short(a.Digest), err)
		}
		fmt.Fprintf(out, "  copied untagged %s@%s (catalog says version %s)\n", a.Name, short(a.Digest), a.Version)
		copied++
	}
	return copied, nil
}

// registryCatalog lists every repository, following the distribution spec's `Link` paging. A
// registry that pages and a client that does not reads as a successful partial migration.
func registryCatalog(ctx context.Context, reg string) ([]string, error) {
	var all []string
	path := "/v2/_catalog?n=200"
	for path != "" {
		var page struct {
			Repositories []string `json:"repositories"`
		}
		next, err := registryGetJSON(ctx, reg, path, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Repositories...)
		path = next
	}
	sort.Strings(all)
	return all, nil
}

func registryTags(ctx context.Context, reg, repo string) ([]string, error) {
	var all []string
	path := "/v2/" + repo + "/tags/list?n=200"
	for path != "" {
		var page struct {
			Tags []string `json:"tags"`
		}
		next, err := registryGetJSON(ctx, reg, path, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Tags...)
		path = next
	}
	sort.Strings(all)
	return all, nil
}

// registryGetJSON decodes one page and returns the next path, or "" when there is none. It tries
// every base `registryProbeBases` knows, so the same loopback-versus-in-network split `confirmPushed`
// already handles does not need solving twice.
func registryGetJSON(ctx context.Context, reg, path string, into any) (string, error) {
	var lastErr error
	for _, base := range registryProbeBases(reg) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("GET %s%s: %s", base, path, resp.Status)
			continue
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if err := json.Unmarshal(body, into); err != nil {
			return "", fmt.Errorf("decoding %s%s: %w", base, path, err)
		}
		return nextLinkPath(resp.Header.Get("Link")), nil
	}
	if lastErr == nil {
		lastErr = errors.New("no reachable base")
	}
	return "", lastErr
}

// nextLinkPath pulls the path out of `</v2/_catalog?n=200&last=x>; rel="next"`, and returns "" for
// anything that is not a next link.
func nextLinkPath(header string) string {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		open := strings.Index(part, "<")
		close := strings.Index(part, ">")
		if open < 0 || close < open {
			continue
		}
		return part[open+1 : close]
	}
	return ""
}

// catalogDigestsMissing asks the orchestrator what digests it has recorded and reports the ones the
// destination cannot serve. An unreachable orchestrator is reported, never treated as "none missing".
func catalogDigestsMissing(dst string) (missing []string, checked int, err error) {
	var actors []actorRecord
	if err := newAPI(orchestratorURL()).getJSON("/api/actors", &actors); err != nil {
		return nil, 0, err
	}
	for _, a := range actors {
		if a.Digest == "" {
			continue
		}
		checked++
		// Asked BY DIGEST, not by version: a tag can have moved since the catalog recorded it, and
		// what a Placement pins is the digest.
		if _, err := registryManifestDigest(dst, a.Name, a.Digest); err != nil {
			missing = append(missing, fmt.Sprintf("%s@%s (%s)", a.Name, short(a.Digest), a.Version))
		}
	}
	return missing, checked, nil
}

func short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
