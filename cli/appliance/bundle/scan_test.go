package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanPassesAnOrdinaryTree(t *testing.T) {
	root := stageFixture(t)
	if err := scanStaged(root, []string{"/nowhere/in/this/tree"}); err != nil {
		t.Fatalf("a clean tree was refused: %v", err)
	}
}

// `/root/.npmrc` on this box carries a live npm auth token, and a `pnpm install` reads it. It does
// not currently copy it into node_modules — measured — but the whole class is one careless `cp -r`
// from being staged, and a bundle is published world-readable.
func TestScanRefusesCredentialFilesByName(t *testing.T) {
	cases := map[string]string{
		".npmrc":               "//registry.npmjs.org/:_authToken=redacted\n",
		".env":                 "KONTRA_S3_SECRET=redacted\n",
		"server.pem":           "not actually a key\n",
		"id_rsa":               "not actually a key\n",
		"deploy.key":           "not actually a key\n",
		".env.production":      "DIGITALOCEAN_TOKEN=redacted\n",
		"service-account.json": "{}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := stageFixture(t)
			write(t, filepath.Join(root, "orchestrator", name), body, 0o600)
			err := scanStaged(root, nil)
			if err == nil {
				t.Fatalf("%s was staged into a bundle", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the message does not name the file:\n%v", err)
			}
		})
	}
}

// A `.git` directory is refused whole, not walked into: it carries remote URLs, an index and
// whatever a `git config` on the build machine set.
func TestScanRefusesADotGitDirectory(t *testing.T) {
	root := stageFixture(t)
	mkdir(t, filepath.Join(root, "orchestrator", ".git"))
	write(t, filepath.Join(root, "orchestrator", ".git", "config"), "[remote \"origin\"]\n", 0o644)
	if err := scanStaged(root, nil); err == nil || !strings.Contains(err.Error(), ".git") {
		t.Errorf(".git was staged: %v", err)
	}
}

func TestScanRefusesCredentialShapes(t *testing.T) {
	cases := map[string]string{
		"an npm access token":            "//registry.npmjs.org/:_authToken=npm_" + strings.Repeat("A1b2", 9),
		"a DigitalOcean API token":       "token = dop_v1_" + strings.Repeat("ab12cd34", 8),
		"an AWS access key id":           "const k = 'AKIA2QWERTYUIOPASDFG';",
		"a GitHub personal access token": "ghp_" + strings.Repeat("Zz9y", 9),
		"a Slack token":                  "xoxb-1234567890-abcdefghijkl",
		"an Anthropic API key":           "sk-ant-api03-" + strings.Repeat("aA1_", 8),
		"a private key": "-----BEGIN RSA PRIVATE KEY-----\n" +
			"MIIEowIBAAKCAQEAvQ8vT0k9YV8LhZ0bJ7cQ2m0N4pR1sT6uV3wX9yZ0aB2cD4eF5g==\n",
	}
	for want, body := range cases {
		t.Run(want, func(t *testing.T) {
			root := stageFixture(t)
			write(t, filepath.Join(root, "orchestrator", "dist", "leak.js"), body, 0o644)
			err := scanStaged(root, nil)
			if err == nil {
				t.Fatalf("%s was staged into a bundle", want)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message does not say what it found (%q):\n%v", want, err)
			}
		})
	}
}

// THE FALSE POSITIVES ARE THE HARD PART, and every one of these is in the real production tree.
// A rule that refuses a legitimate upstream file is a rule somebody will disable, and a disabled
// rule catches nothing at all.
func TestScanAllowsWhatTheRealTreeActuallyContains(t *testing.T) {
	cases := map[string]string{
		// npm-registry-fetch/lib/auth.js — the WORD, as source code, in a package the orchestrator
		// depends on transitively. Matching it would refuse every build.
		"the word _authToken in source": "const { _authToken } = opts;\nif (_authToken) headers.authorization = `Bearer ${_authToken}`\n",
		// @aws-sdk/nested-clients' generated .d.ts for AssumeRole quotes AWS's own placeholder.
		"AWS's documented example key": "* AccessKeyId: \"AKIAIOSFODNN7EXAMPLE\",\n",
		// libduckdb.so and npm-registry-fetch's README both carry the LABEL with no key material.
		"a PEM label with no material": "static const char *pem = \"-----BEGIN RSA PRIVATE KEY-----\";\n",
		// The Rust bridge's prebuilt .node embeds upstream's own CI path. It is not ours, we
		// cannot remove it, and we publish its digest anyway.
		"upstream's build path":  "/root/.cargo/registry/src/index.crates.io-1949/tokio-1.0/src/lib.rs",
		"a bare npm_ identifier": "function npm_package_version() { return 1 }\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := stageFixture(t)
			write(t, filepath.Join(root, "orchestrator", "dist", "upstream.js"), body, 0o644)
			// The machine path list is what a real build passes: the repo root and the staging
			// root, NOT bare $HOME. See bundlescan.go's header.
			if err := scanStaged(root, []string{"/home/somebody/checkout", root + "/nonexistent"}); err != nil {
				t.Errorf("a legitimate upstream file was refused:\n%v", err)
			}
		})
	}
}

func TestScanRefusesTheBuildMachinesPaths(t *testing.T) {
	root := stageFixture(t)
	buildDir := "/home/ci/agent/work/kontra-local"
	write(t, filepath.Join(root, "orchestrator", "dist", "src", "app.js.map"),
		`{"sources":["`+buildDir+`/backend/src/server.ts"]}`, 0o644)

	err := scanStaged(root, []string{buildDir})
	if err == nil {
		t.Fatal("a source map naming the build machine's checkout was staged")
	}
	if !strings.Contains(err.Error(), buildDir) {
		t.Errorf("the message does not name the path it objects to:\n%v", err)
	}
}

// A secret that straddles a read boundary is a secret that is not found, and the boundary is an
// implementation detail nobody would think to test against. The overlap in `scanFile` is what
// stops it, so this puts a token exactly across one.
func TestScanFindsATokenAcrossAChunkBoundary(t *testing.T) {
	token := "npm_" + strings.Repeat("A1b2", 9)
	for _, offset := range []int{-4, -1, 0, 1} {
		pad := scanChunk - len(token)/2 + offset
		root := stageFixture(t)
		write(t, filepath.Join(root, "orchestrator", "dist", "big.js"), strings.Repeat("x", pad)+token, 0o644)
		if err := scanStaged(root, nil); err == nil {
			t.Errorf("a token spanning the read boundary at +%d was missed", offset)
		}
	}
}

func TestScanSkipsEmptyAndRelativeMachinePaths(t *testing.T) {
	// A caller that consulted an unset environment variable passes "". Scanning for the empty
	// string would match every file in the tree and refuse every build, with a message naming
	// nothing.
	root := stageFixture(t)
	if err := scanStaged(root, []string{"", "  ", "relative/path", "/"}); err != nil {
		t.Fatalf("empty and relative machine paths were treated as real: %v", err)
	}
}

func TestScanReportsMoreThanOneFinding(t *testing.T) {
	root := stageFixture(t)
	write(t, filepath.Join(root, "orchestrator", ".npmrc"), "x\n", 0o600)
	write(t, filepath.Join(root, "orchestrator", "dist", "a.js"), "npm_"+strings.Repeat("A1b2", 9), 0o644)
	err := scanStaged(root, nil)
	if err == nil {
		t.Fatal("nothing was refused")
	}
	if lines := strings.Count(err.Error(), "\n"); lines < 2 {
		t.Errorf("only one finding was reported; one bad staging step usually produces several:\n%v", err)
	}
}

func TestScanIgnoresBrokenSymlinks(t *testing.T) {
	// pnpm's `.bin` shims outlive a prune, so a staged tree contains dangling links. Following one
	// would fail the scan with an ENOENT that says nothing about credentials.
	root := stageFixture(t)
	if err := os.Symlink("../gone/nowhere", filepath.Join(root, "orchestrator", "node_modules", "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := scanStaged(root, nil); err != nil {
		t.Fatalf("a dangling symlink broke the scan: %v", err)
	}
}
