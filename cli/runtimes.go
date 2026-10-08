// runtimes.go — resolving the **Runtime** an actor declares to a digest, and listing what is there.
//
// A Runtime is a CNB run image published from `kontra-runtimes`: the OS plus system packages an
// actor runs on. An author declares a name and a MAJOR (`python-browser:1`); the build resolves that
// to a digest and records both in the catalog, because the major is what was asked for and the
// digest is whether it is still current — which is the whole of rebase detection.
//
// WHY RESOLUTION HAPPENS BEFORE THE BUILD. `pack build --run-image` wants a reference that exists. A
// typo in a runtime name discovered by the lifecycle is a failure several containers deep with a
// message about a manifest; discovered here it is one line naming the runtimes that do exist.
//
// DISCOVERY IS A REGISTRY QUERY, NEVER A LIST IN THIS REPOSITORY. Adding a runtime is adding a
// directory to `kontra-runtimes` and pushing it — if kontra had to be edited too, a fork could not
// add one, which is the whole point of §3.4.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
)

// runtimesPrefix is where runtimes live. Default: the install's own registry under
// `kontra-runtimes/`, which is what first-boot mirroring populates. A fork points
// KONTRA_RUNTIMES_PREFIX at its own registry and changes nothing else.
func runtimesPrefix(reg string) string {
	if p := strings.TrimSpace(os.Getenv("KONTRA_RUNTIMES_PREFIX")); p != "" {
		return strings.TrimRight(p, "/")
	}
	host, _ := registryHost(reg)
	return host + "/kontra-runtimes"
}

// defaultRuntime is what an actor that declares none gets. Python actors need a Python; a Go binary
// needs only an OS.
func defaultRuntime(engine string) string {
	if engine == "go" {
		return "base:1"
	}
	return "python:1"
}

// resolvedRuntime is what the build uses and what the catalog records.
type resolvedRuntime struct {
	Name   string // as declared: `python-browser`, or the last path element of a qualified reference
	Major  uint32
	Ref    string // the reference `pack` is given, without a digest
	Digest string // what that reference resolved to, and what the catalog records
}

// Pinned returns the form `pack --run-image` should be handed: a digest, so a build is reproducible
// even if the moving major tag advances between resolution and the lifecycle's own pull.
func (r resolvedRuntime) Pinned() string { return r.Ref + "@" + r.Digest }

// parseRuntimeDeclaration splits what an author wrote.
//
// Two shapes are legal and they are distinguished by whether the first path element LOOKS LIKE A
// REGISTRY HOST — the same question `ociref` already answers for every other reference in this repo,
// rather than a second rule invented here:
//
//	python-browser:1                     a name and a major, resolved against the prefix
//	registry.example.com/team/rt/gpu:2   fully qualified; the prefix is not consulted
func parseRuntimeDeclaration(declared, reg string) (name string, major uint32, ref string, err error) {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return "", 0, "", fmt.Errorf("empty runtime")
	}
	base, tag, ok := cutLast(declared, ":")
	if !ok {
		return "", 0, "", fmt.Errorf("runtime %q has no major — write %q", declared, declared+":1")
	}
	m, convErr := strconv.ParseUint(tag, 10, 32)
	if convErr != nil {
		// A major is a NUMBER. `python:3.12` names a Python version, which is a thing an author will
		// try, and the refusal says what the field actually means.
		return "", 0, "", fmt.Errorf("runtime %q: %q is not a major version. A runtime's tag is its "+
			"MAJOR (`python:1`); the interpreter version belongs in the actor's .python-version", declared, tag)
	}
	major = uint32(m)

	first, _, hasSlash := strings.Cut(base, "/")
	switch {
	case hasSlash && ociref.LooksLikeRegistryHost(first):
		return lastPathElement(base), major, base + ":" + tag, nil
	case hasSlash:
		return "", 0, "", fmt.Errorf("runtime %q: %q is not a registry host (it needs a dot, a port, "+
			"or to be `localhost`). A bare name is resolved against KONTRA_RUNTIMES_PREFIX; anything "+
			"else must be fully qualified", declared, first)
	default:
		return base, major, runtimesPrefix(reg) + "/" + base + ":" + tag, nil
	}
}

// resolveRuntime turns a declaration into a digest, and refuses in a way that can be acted on.
func resolveRuntime(reg, declared, engine string) (resolvedRuntime, error) {
	if strings.TrimSpace(declared) == "" {
		declared = defaultRuntime(engine)
	}
	name, major, ref, err := parseRuntimeDeclaration(declared, reg)
	if err != nil {
		return resolvedRuntime{}, err
	}
	if err := ociref.Check(ref); err != nil {
		return resolvedRuntime{}, fmt.Errorf("runtime %q resolves to %q, which is not a usable "+
			"reference: %w", declared, ref, err)
	}

	host, path, _ := strings.Cut(ref, "/")
	repo, tag, _ := cutLast(path, ":")
	digest, derr := registryManifestDigest(host, repo, tag)
	if derr != nil {
		// LISTING WHAT EXISTS IS THE POINT OF THIS BRANCH. "manifest unknown" sends an author to the
		// registry; the names do not.
		avail, lerr := listRuntimes(reg)
		if lerr != nil || len(avail) == 0 {
			return resolvedRuntime{}, fmt.Errorf("runtime %q (%s) is not in the registry: %w\n"+
				"  and NO runtimes are published under %s, so no actor can be built here yet.\n"+
				"  Copy the published set in with `kontra runtime import` — first boot runs it, and an\n"+
				"  install with no route to %s needs KONTRA_RUNTIMES_SOURCE pointed at a mirror it can\n"+
				"  reach. `kontra runtime list` is what this read.",
				declared, ref, derr, runtimesPrefix(reg), publishedRuntimes)
		}
		return resolvedRuntime{}, fmt.Errorf("runtime %q (%s) is not in the registry: %w\n  available: %s",
			declared, ref, derr, strings.Join(avail, ", "))
	}
	return resolvedRuntime{Name: name, Major: major, Ref: ref, Digest: digest}, nil
}

// listRuntimes names every runtime this install can resolve, as `<name>:<major>`.
func listRuntimes(reg string) ([]string, error) { return runtimesUnder(runtimesPrefix(reg)) }

// runtimesUnder names every runtime published under one prefix, as `<name>:<major>`.
//
// It reads the registry's catalog rather than any manifest annotation, because a NAME is all this
// needs and reading annotations would mean a manifest fetch per repository for a message.
//
// NOT EVERY REGISTRY WILL BE ENUMERATED. `GET /v2/_catalog` is in the distribution spec and ghcr
// answers it 403 DENIED — measured — so discovery works against this install's zot and against a
// mirror, and the published set has to be named rather than found. `kontra runtime import` is where
// that costs something, and it says so.
func runtimesUnder(prefix string) ([]string, error) {
	_, prefixPath, ok := strings.Cut(prefix, "/")
	if !ok {
		return nil, fmt.Errorf("runtimes prefix %q names no repository path", prefix)
	}
	host, _ := registryHost(prefix)
	repos, err := registryRepositories(host)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range repos {
		if !strings.HasPrefix(r, prefixPath+"/") {
			continue
		}
		name := strings.TrimPrefix(r, prefixPath+"/")
		tags, err := registryTagList(host, r)
		if err != nil {
			continue
		}
		for _, t := range tags {
			// Only the MAJOR tags are offerable: a patch tag is immutable and naming one in a
			// suggestion would pin an actor to a runtime that can never be patched.
			if _, err := strconv.ParseUint(t, 10, 32); err == nil {
				out = append(out, name+":"+t)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// registryRepositories lists every repository, following the spec's `Link` paging — a registry that
// pages against a client that does not would silently hide the runtimes on page two.
func registryRepositories(host string) ([]string, error) {
	var all []string
	path := "/v2/_catalog?n=200"
	for path != "" {
		var page struct {
			Repositories []string `json:"repositories"`
		}
		next, err := registryJSON(host, path, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Repositories...)
		path = next
	}
	return all, nil
}

func registryTagList(host, repo string) ([]string, error) {
	var page struct {
		Tags []string `json:"tags"`
	}
	if _, err := registryJSON(host, "/v2/"+repo+"/tags/list?n=200", &page); err != nil {
		return nil, err
	}
	return page.Tags, nil
}

// registryJSON decodes one page and returns the next path, trying every base
// `registryProbeBases` knows so the loopback-versus-in-network split is not solved twice.
func registryJSON(host, path string, into any) (string, error) {
	var lastErr error
	for _, base := range registryProbeBases(host) {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
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
		return nextRegistryLink(resp.Header.Get("Link")), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no reachable registry base for %s", host)
	}
	return "", lastErr
}

func nextRegistryLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		open, close := strings.Index(part, "<"), strings.Index(part, ">")
		if open < 0 || close < open {
			continue
		}
		return part[open+1 : close]
	}
	return ""
}

// cutLast splits on the LAST separator, which is what a reference needs: `host:5000/a/b:1.0.0` has
// two colons and only the second is the tag.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func lastPathElement(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// --- the runtime goes through the same trust gate an actor image does ---

// admitRuntime puts the resolved runtime through this install's trust policy before a build is given
// it as `--run-image`.
//
// WHY A RUNTIME EARNS THE SAME JUDGEMENT AS AN ACTOR IMAGE, AND ARGUABLY MORE. A Machine refuses to
// run an actor image whose registry is not allowed or whose signature it cannot accept
// (`trustpolicy.Policy.Admit`, called by the podman driver before it pulls). A runtime is the BASE of
// every actor built on it — layered under the deps and the app, present on every Machine that runs any
// of them — and nothing was asking the same question about it. `pack` pulls it by digest and the
// Warden never sees it as a reference, so the existing gate could not reach it.
//
// THE UNCONFIGURED CASE IS A NOTE AND NOT A PASS, which is this package's own distinction
// (`ErrUnsigned` vs `ErrUnverifiable`: "one is a decision somebody made and the other is a question
// nobody could ask"). The zero policy admits NOTHING, so gating unconditionally would refuse every
// build on an install that has not configured trust — and silently skipping would let an operator
// believe a runtime had been checked. So it says, once, that it did not ask.
func admitRuntime(ctx context.Context, r resolvedRuntime, progress io.Writer) error {
	o := trustpolicy.Options{
		Registries: os.Getenv(trustpolicy.RegistriesEnv),
		Unsigned:   os.Getenv(trustpolicy.UnsignedEnv),
		Key:        os.Getenv(trustpolicy.KeyEnv),
		Identity:   os.Getenv(trustpolicy.IdentityEnv),
		Issuer:     os.Getenv(trustpolicy.IssuerEnv),
	}
	if o == (trustpolicy.Options{}) {
		if progress != nil {
			fmt.Fprintf(progress, "note: no trust policy is configured (%s), so %s was NOT checked — "+
				"the runtime every actor built here is layered on is being taken on trust\n",
				trustpolicy.RegistriesEnv, r.Pinned())
		}
		return nil
	}
	pol, err := trustpolicy.Load(o)
	if err != nil {
		return fmt.Errorf("the trust policy this install is configured with cannot be loaded, so the "+
			"runtime %s cannot be judged: %w", r.Pinned(), err)
	}
	if _, err := pol.Admit(ctx, r.Pinned()); err != nil {
		return fmt.Errorf("runtime %s is not admissible under this install's trust policy, and it would "+
			"be the base of every actor built on it: %w", r.Pinned(), err)
	}
	return nil
}
