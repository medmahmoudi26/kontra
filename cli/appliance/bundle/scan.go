// scan.go — the gate between a staged tree and a published bundle.
//
// A bundle is world-readable by design: it is fetched anonymously over the VPC and verified by
// digest, exactly like the actor Bundles in `cli/bundle.go`, and "world-readable" is a property
// you get to choose ONCE. Anything staged into it is published, so the check has to happen before
// the tar writer, not after somebody notices.
//
// TWO KINDS OF THING ARE REFUSED, and they fail for different reasons.
//
//   - A CREDENTIAL. `/root/.npmrc` on this box carries a live npm auth token, and a `pnpm install`
//     reads it. It does not currently COPY it into node_modules — measured, on the real tree — but
//     that is a fact about pnpm's present behaviour and not a guarantee, and the whole class
//     (`.npmrc`, `.env`, an ssh key, a DigitalOcean token) is one careless `cp -r` away from being
//     inside a staging directory. The rules below are shaped like the tokens themselves rather
//     than like the words around them: `_authToken` appears as SOURCE CODE in npm-registry-fetch
//     and matching it would refuse every build for a string that is not a secret.
//
//   - A PATH FROM THE MACHINE THAT BUILT IT. A bundle that works because it still knows where it
//     was assembled is a bundle that fails on the next machine, with a message about a directory
//     nobody there has ever had.
//
// WHY THE PATH RULE IS THE BUILD'S OWN DIRECTORIES AND NOT `$HOME`. Measured: the prebuilt
// `@temporalio/core-bridge` `.node` binaries contain `/root/.cargo/registry/src/...`, baked in by
// UPSTREAM's CI, which happened to run as root. On this box `$HOME` is `/root`, so a bare
// `$HOME` rule refuses every build over a path that is not ours, that we cannot remove, and whose
// digest we pin and publish anyway. The paths that would really leak — a checkout path in a
// source map, a staging path in something generated — all live under the repo root or the staging
// root, and those ARE scanned, along with the output and cache directories and the pnpm store.
// A rule that fires on somebody else's build path teaches people to pass a `--force` flag, which
// is worse than not having the rule.
package bundle

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// secretRule is one thing a bundle may not contain.
//
// The `anchor` is a plain literal that must be present for the pattern to have any chance of
// matching, and it exists for speed: the staged tree is ~450 MB, `bytes.Index` runs at memory
// bandwidth and `regexp` does not, so the expensive half only runs on the rare chunk that could
// match at all.
type secretRule struct {
	what   string
	anchor string
	re     *regexp.Regexp
	// benign are exact matches that are documentation, not credentials. Each one is named here
	// rather than handled by loosening the pattern, because a looser pattern stops catching the
	// real thing and nobody notices.
	benign []string
}

var secretRules = []secretRule{
	{
		what:   "an npm access token",
		anchor: "npm_",
		re:     regexp.MustCompile(`npm_[A-Za-z0-9]{36}`),
	},
	{
		what:   "a DigitalOcean API token",
		anchor: "dop_v1_",
		re:     regexp.MustCompile(`dop_v1_[0-9a-f]{64}`),
	},
	{
		what:   "an AWS access key id",
		anchor: "AKIA",
		re:     regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		// AWS's own documented placeholder, quoted verbatim in the SDK's generated `.d.ts` for
		// AssumeRole and AssumeRoleWithWebIdentity. It is in the bundle because those files are,
		// and it is not a key.
		benign: []string{"AKIAIOSFODNN7EXAMPLE"},
	},
	{
		what:   "a GitHub personal access token",
		anchor: "ghp_",
		re:     regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),
	},
	{
		what:   "a Slack token",
		anchor: "xox",
		re:     regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{12,}`),
	},
	{
		what:   "an Anthropic API key",
		anchor: "sk-ant-",
		re:     regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	},
	{
		what:   "a private key",
		anchor: "-----BEGIN ",
		// The MATERIAL, not the label. `-----BEGIN RSA PRIVATE KEY-----` appears as a bare string
		// in libduckdb.so's PEM parser and in npm-registry-fetch's README; a key is that header
		// followed by base64, so that is what the pattern says.
		re: regexp.MustCompile(`-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----[\r\n]+[A-Za-z0-9+/=]{40}`),
	},
}

// forbiddenNames are files and directories that never belong in a bundle whatever they contain.
// A name check costs nothing and catches the case where a credential file was copied in whole,
// which is the only way most of these ever travel.
var forbiddenNames = []string{
	".npmrc", ".yarnrc", ".netrc", ".env", ".git", ".ssh", ".aws", ".pulumi", ".docker",
	"id_rsa", "id_ed25519", "id_ecdsa", "credentials.json", "service-account.json",
}

// forbiddenSuffixes catch the same class by extension.
var forbiddenSuffixes = []string{".pem", ".p12", ".pfx", ".key", ".keystore"}

// scanStaged refuses a staged tree that must not be published.
//
// machinePaths are absolute directories belonging to THIS build — see the file header for why the
// list is these and not `$HOME`. It reports up to a handful of findings rather than the first,
// because one bad staging step usually produces several and finding them one build at a time is
// the slow way.
func scanStaged(root string, machinePaths []string) error {
	// Longest first, so a finding is reported against the most specific directory that explains
	// it: `/root/kontra-local/build/stage` rather than `/root/kontra-local`.
	paths := append([]string(nil), machinePaths...)
	paths = filterAbsolute(paths)
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })

	var findings []string
	const maxFindings = 10

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		if bad := forbiddenName(d.Name()); bad != "" {
			findings = append(findings, fmt.Sprintf("  %s — %s", filepath.ToSlash(rel), bad))
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // directories carry no bytes; symlink targets are checked by the tar writer
		}
		found, scanErr := scanFile(path, paths)
		if scanErr != nil {
			return scanErr
		}
		if found != "" {
			findings = append(findings, fmt.Sprintf("  %s — %s", filepath.ToSlash(rel), found))
		}
		if len(findings) >= maxFindings {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan staged tree: %w", err)
	}
	if len(findings) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to build a bundle: a bundle is published world-readable, and this tree carries\n%s",
		strings.Join(findings, "\n"))
}

func forbiddenName(name string) string {
	for _, f := range forbiddenNames {
		if name == f {
			return "a credential file (" + f + ")"
		}
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".env.") {
		return "a credential file (.env.*)"
	}
	for _, s := range forbiddenSuffixes {
		if strings.HasSuffix(lower, s) {
			return "a key or certificate file (*" + s + ")"
		}
	}
	return ""
}

// scanChunk and scanOverlap bound the memory this uses. libduckdb.so is 68 MB and the Rust bridge
// is 31 MB; reading either whole is affordable once and wasteful forty thousand times, and this
// runs over every file in the tree. The overlap is what stops a token that straddles a chunk
// boundary from being missed — it must exceed the longest pattern any rule can match.
const (
	scanChunk   = 1 << 20
	scanOverlap = 512
)

// scanFile returns a description of the first problem in one file, or "".
func scanFile(path string, machinePaths []string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, scanChunk+scanOverlap)
	var carry int
	for {
		n, err := io.ReadFull(f, buf[carry:])
		total := carry + n
		if total > 0 {
			if hit := scanBytes(buf[:total], machinePaths); hit != "" {
				return hit, nil
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		// Carry the tail forward so a match spanning the boundary is still contiguous.
		carry = scanOverlap
		copy(buf[:carry], buf[total-carry:total])
	}
}

// scanBytes is the whole rule set applied to one window. Exported to the package's tests, which
// is where each pattern's real-world false positives are pinned.
func scanBytes(b []byte, machinePaths []string) string {
	for _, p := range machinePaths {
		if bytes.Contains(b, []byte(p)) {
			return fmt.Sprintf("a path from the machine that built it (%s)", p)
		}
	}
	for _, rule := range secretRules {
		if !bytes.Contains(b, []byte(rule.anchor)) {
			continue
		}
		for _, m := range rule.re.FindAll(b, -1) {
			if slicesContainsString(rule.benign, string(m)) {
				continue
			}
			return "what looks like " + rule.what
		}
	}
	return ""
}

func slicesContainsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// filterAbsolute drops the empty and relative entries a caller may pass when an environment
// variable it consulted was unset — scanning for "" would match every file in the tree.
func filterAbsolute(in []string) []string {
	out := in[:0]
	seen := map[string]bool{}
	for _, p := range in {
		p = strings.TrimRight(p, "/")
		if p == "" || !filepath.IsAbs(p) || p == "/" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
